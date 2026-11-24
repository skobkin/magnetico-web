package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/skobkin/magnetico-web/codec"
	webaccount "github.com/skobkin/magnetico-web/internal/web/account"
	"github.com/skobkin/magnetico-web/internal/web/accounts"
	webapidocs "github.com/skobkin/magnetico-web/internal/web/apidocs"
	webapiv2 "github.com/skobkin/magnetico-web/internal/web/apiv2"
	webauth "github.com/skobkin/magnetico-web/internal/web/auth"
	webfeed "github.com/skobkin/magnetico-web/internal/web/feed"
	"github.com/skobkin/magnetico-web/internal/web/filecache"
	"github.com/skobkin/magnetico-web/internal/web/fileview"
	webmail "github.com/skobkin/magnetico-web/internal/web/mail"
	"github.com/skobkin/magnetico-web/internal/web/password"
	websearch "github.com/skobkin/magnetico-web/internal/web/search"
	webserver "github.com/skobkin/magnetico-web/internal/web/server"
	webtorrents "github.com/skobkin/magnetico-web/internal/web/torrents"
	webtrackers "github.com/skobkin/magnetico-web/internal/web/trackers"
	webdb "github.com/skobkin/magnetico-web/internal/web/webdb"
	"github.com/skobkin/magnetico-web/internal/web/webschema"
	"github.com/skobkin/magnetico-web/opflags"
	"github.com/skobkin/magnetico-web/persistence"
)

// assembleWeb builds the complete web service: every route the production
// binary serves, over backend and the resolved options. It is the single place
// the pieces are wired together, so tests can assemble exactly the router
// that runs in production. On error the caller closes backend; assembleWeb
// itself never exits the process.
func assembleWeb(o *opflags.WebOptions, backend *webdb.Backend, logger *slog.Logger) (*webserver.Server, error) {
	srv, err := webserver.New(webserver.Config{
		Listen:          o.Listen,
		PublicURL:       o.PublicBaseURL,
		TrustedProxies:  o.TrustedProxyPrefixes,
		RequestTimeout:  o.RequestTimeout,
		SearchTimeout:   o.SearchTimeout,
		ShutdownTimeout: o.ShutdownTimeout,
		SiteTitle:       o.SiteTitle,
		Logger:          logger,
	}, backend)
	if err != nil {
		return nil, err
	}
	// Authentication (W3): sessions, CSRF, login/logout, rate limits and the
	// purger. password.Default is prepared by a startup hook before the
	// listener opens.
	accountStore := accounts.New(backend.Pool())
	authSvc, err := webauth.Install(srv, webauth.Config{
		Store:    accountStore,
		Verifier: password.Default,
		Lifetimes: accounts.SessionLifetimes{
			Idle: o.SessionIdleTimeout, Absolute: o.SessionAbsoluteLifetime, Remember: o.SessionRememberLifetime,
		},
		DevMode:      o.DevMode,
		RecoveryLink: o.SMTPConfigured(),
	})
	if err != nil {
		return nil, err
	}

	// Optional email (W6): a strict-TLS sender behind a bounded in-process queue
	// whose workers are server background tasks. Without SMTP the queue is nil
	// and every email-dependent control is unavailable.
	var mailQueue *webmail.Queue
	if o.SMTPConfigured() {
		sender, err := webmail.NewSender(webmail.Config{
			Host: o.SMTPHost, Port: o.SMTPPort, Security: o.SMTPSecurity,
			Username: o.SMTPUsername, Password: o.SMTPPassword, From: o.SMTPFrom,
		})
		if err != nil {
			return nil, err
		}
		mailQueue = webmail.NewQueue(sender, logger, webmail.Options{})
		srv.AddBackground(mailQueue.Run)
		srv.OnDrained(func(ctx context.Context) error {
			if n := mailQueue.Drain(ctx); n != 0 {
				return fmt.Errorf("mail queue abandoned %d jobs", n)
			}
			st := mailQueue.Stats()
			logger.Info("mail queue stopped", "sent", st.Sent, "failed", st.Failed, "expired", st.Expired, "rejected", st.Rejected)
			return nil
		})
	}
	if _, err := webaccount.Install(srv, webaccount.Config{
		Store: accountStore, Passwords: password.Default, Auth: authSvc, Mail: mailQueue,
		PublicBase: o.PublicBaseURL, InviteQuota: o.InviteQuota,
	}); err != nil {
		return nil, err
	}

	// Search, details and the file browser (W4). The file-index cache owns the
	// only request-detached work; it is stopped after requests drain and
	// before the database closes.
	reader := persistence.NewReader(backend.Pool(), codec.DefaultLimits())
	searchSvc, err := websearch.New(reader, websearch.Config{Concurrency: o.SearchConcurrency})
	if err != nil {
		return nil, err
	}
	fileCache, err := filecache.New(reader, filecache.Config{Logger: logger})
	if err != nil {
		return nil, err
	}
	srv.OnDrained(func(ctx context.Context) error {
		err := fileCache.Close(ctx)
		st := fileCache.Stats()
		logger.Info("file index cache stopped", "hits", st.Hits, "misses", st.Misses, "coalesced", st.Coalesced,
			"builds", st.Builds, "build_errors", st.BuildErrors, "abandoned", st.Abandoned, "evictions", st.Evictions, "expired", st.Expired,
			"replaced", st.Replaced, "oversize", st.Oversize, "retries", st.Retries, "entries", st.Entries, "bytes", st.Bytes)
		return err
	})
	if diagHook != nil {
		diagHook(diagSources{FileCache: fileCache, Backend: backend})
	}
	viewSvc, err := fileview.New(fileCache, reader, fileview.Config{Logger: logger})
	if err != nil {
		return nil, err
	}
	// Public tracker list (W7): loaded before the listener opens (startup fails
	// when it cannot be read), refreshed in the background, shared by the
	// pages, the API, the feeds and the public /magnet short link.
	trackerCache := webtrackers.New(webtrackers.NewReader(backend.Pool()), o.TrackerCacheTTL, logger)
	srv.OnStartup(trackerCache.Load)
	srv.AddBackground(trackerCache.Run)
	webtrackers.Install(srv, trackerCache)

	if _, err := webtorrents.Install(srv, webtorrents.Deps{
		Search: searchSvc, Details: reader, View: viewSvc, Trackers: trackerCache, PublicBase: o.PublicBaseURL,
	}); err != nil {
		return nil, err
	}

	// RSS feeds (W7): the deprecated v1 compatibility feed and the v2 feed, each
	// with its own fixed credential policy, over RSS tokens only.
	if _, err := webfeed.Install(srv, webfeed.Config{
		Auth: accountStore, Rows: reader, Trackers: trackerCache, PublicBase: o.PublicBaseURL,
	}); err != nil {
		return nil, err
	}

	// Public REST API v2 (W7): bearer API tokens only, over the same search,
	// details and file-view services as the pages.
	if _, err := webapiv2.Install(srv, webapiv2.Config{
		Auth: accountStore, Search: searchSvc, Details: reader, View: viewSvc, Trackers: trackerCache,
	}); err != nil {
		return nil, err
	}

	// Public API documentation (W7): landing page, the vendored interactive
	// renderer and the embedded OpenAPI documents.
	webapidocs.Install(srv)
	return srv, nil
}

func runWeb(o *opflags.WebOptions) {
	logger, err := webserver.NewLogger(os.Stderr, o.LogLevel, o.LogFormat)
	if err != nil {
		log.Fatalln(err.Error()) // unreachable: validated by opflags
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	backend, err := webdb.Open(ctx, o.DatabaseURL, o.PoolMaxConns)
	if err != nil {
		log.Fatalln(fmt.Sprintf("The web service could not start: database %s. %s",
			redactDatabaseURL(o.DatabaseURL), controlledErrorDetail(err)))
	}
	srv, err := assembleWeb(o, backend, logger)
	if err != nil {
		backend.Close()
		log.Fatalln("The web service could not start: " + err.Error())
	}

	err = srv.Run(ctx)
	if errors.Is(err, webserver.ErrShutdownIncomplete) {
		// Same convention as `crawl` and `ingest`.
		log.Println(webserver.ErrShutdownIncomplete)
		os.Exit(3)
	}
	if err != nil {
		log.Fatalln(appendSchemaStateDetails(fmt.Sprintf(
			"The web service stopped: database %s. %s", redactDatabaseURL(o.DatabaseURL), webErrorDetail(err)), err))
	}
}

// webErrorDetail is controlledErrorDetail plus the web-schema sentinels
// (fixed messages only; the catalog-object details are appended separately by
// appendSchemaStateDetails).
func webErrorDetail(err error) string {
	switch {
	case errors.Is(err, webschema.ErrWebSchemaMissing):
		return webschema.ErrWebSchemaMissing.Error()
	case errors.Is(err, webschema.ErrWebSchemaUnsupported):
		return webschema.ErrWebSchemaUnsupported.Error()
	case errors.Is(err, webschema.ErrWebPrivileges):
		return webschema.ErrWebPrivileges.Error()
	case errors.Is(err, webauth.ErrPrepare):
		return webauth.ErrPrepare.Error()
	}
	return controlledErrorDetail(err)
}

// runMigrateWebSchemaInit implements `magnetico migrate web-schema-init`: it
// creates the magnetico_web schema next to a verified 0.8.0 schema and seeds
// the approved tracker list once. It is non-destructive and idempotent; an
// existing schema that is not exactly the supported version is refused and
// left untouched.
func runMigrateWebSchemaInit(opFlags *opflags.SchemaInitOptions) {
	report, err := webschema.Init(context.Background(), opFlags.DatabaseURL)
	if err != nil {
		log.Fatalln(appendSchemaStateDetails(fmt.Sprintf(
			"Could not initialize the web schema on %s. %s",
			redactDatabaseURL(opFlags.DatabaseURL), webErrorDetail(err)), err))
	}
	state := "already up to date"
	if report.Created {
		state = "created now"
	}
	seed := string(report.Seed)
	if report.Seed == webschema.SeedApplied {
		seed = fmt.Sprintf("seeded %d", report.SeededCount)
	}
	log.Printf("the magnetico_web schema on %s is ready (%s); trackers: %s",
		redactDatabaseURL(opFlags.DatabaseURL), state, seed)
}
