// Package server is the authenticated HTTP surface of the ingest service
// (docs/specs/crawler-ingest-delivery-v1.md §5): the existence lookup and
// the all-or-nothing record admission.
//
// Nothing in a response or a log line carries a token, a record's content or
// driver error text; failures are reported by fixed codes only.
package server

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/clientip"
	"github.com/skobkin/magnetico-web/internal/ingest/crawlerauth"
	"github.com/skobkin/magnetico-web/internal/ingest/existence"
	"github.com/skobkin/magnetico-web/internal/ingest/pgqueue"
	"github.com/skobkin/magnetico-web/internal/ingest/record"
	"github.com/skobkin/magnetico-web/internal/ingest/stats"
	"github.com/skobkin/magnetico-web/internal/opstats"
	"github.com/skobkin/magnetico-web/persistence"
)

// Protocol limits (spec §5).
const (
	APIPrefix           = "/api/ingest/v1"
	MaxExistsHashes     = 1024
	MaxExistsBodyBytes  = 64 << 10
	MaxSubmitRecords    = 256
	MaxSubmitBodyBytes  = record.MaxRecordBytes + 1<<20 // 17 MiB
	DefaultSubmitSlots  = 8
	DefaultExistsSlots  = 64
	maxBatchDiagnostics = 16
)

// Authenticator resolves the Bearer credential of a request to a crawler
// (*crawlerauth.Authenticator). The credential is the only source of identity.
type Authenticator interface {
	Authenticate(ctx context.Context, bearer string) (crawlerauth.Identity, error)
}

// Admitter is the queue seam (*pgqueue.Queue).
type Admitter interface {
	Admit(ctx context.Context, items []pgqueue.Item) (pgqueue.AdmitResult, error)
}

// Checker is the existence seam (*existence.Service).
type Checker interface {
	Check(ctx context.Context, hashes [][20]byte) existence.Result
}

// Config configures New.
type Config struct {
	// Auth authenticates requests. Required.
	Auth  Authenticator
	Queue Admitter
	Exist Checker
	// Limits are the record validation limits (default codec.DefaultLimits).
	Limits codec.Limits
	// SubmitSlots / ExistsSlots bound concurrent handlers (defaults 8 / 64);
	// extra requests receive 503 busy.
	SubmitSlots int
	ExistsSlots int
	// Stats records operational statistics (nil: disabled). It never changes a
	// decision, a status code or the order of anything the handlers do.
	Stats *stats.Recorder
	// TrustedProxies are the reverse proxies whose X-Forwarded-For is believed
	// when resolving a crawler's last address (none: the TCP peer is used).
	TrustedProxies []netip.Prefix
	// OnAdmitted is called after a successful admission (wakes the consumer).
	OnAdmitted func()
	// Logger receives controlled messages only (default log.Default()).
	Logger *log.Logger
}

// Counters are the service counters.
type Counters struct {
	SubmitRequests  uint64
	SubmitAccepted  uint64 // records acknowledged with 202
	SubmitRejected  uint64 // 4xx other than auth
	QueueFull       uint64
	StorageErrors   uint64
	Busy            uint64
	AuthFailures    uint64
	AuthUnavailable uint64 // 503 raised by the authenticator (storage failure or saturation)
	ExistsRequests  uint64
	ExistsUnknown   uint64
	InternalErrors  uint64
}

// Server is the HTTP handler set.
type Server struct {
	cfg    Config
	submit chan struct{}
	exists chan struct{}
	log    *log.Logger

	submitReq, submitAcc, submitRej, queueFull, storageErr, busy, authFail, authUnavail,
	existsReq, existsUnk, internalErr atomic.Uint64
}

// New validates the configuration and builds the server.
func New(cfg Config) (*Server, error) {
	if cfg.Auth == nil {
		return nil, errors.New("server: an authenticator is required")
	}
	if cfg.Queue == nil || cfg.Exist == nil {
		return nil, errors.New("server: queue and existence service are required")
	}
	if cfg.Limits == (codec.Limits{}) {
		cfg.Limits = codec.DefaultLimits()
	}
	if cfg.SubmitSlots <= 0 {
		cfg.SubmitSlots = DefaultSubmitSlots
	}
	if cfg.ExistsSlots <= 0 {
		cfg.ExistsSlots = DefaultExistsSlots
	}
	l := cfg.Logger
	if l == nil {
		l = log.Default()
	}
	return &Server{
		cfg:    cfg,
		submit: make(chan struct{}, cfg.SubmitSlots),
		exists: make(chan struct{}, cfg.ExistsSlots),
		log:    l,
	}, nil
}

// Counters returns a snapshot.
func (s *Server) Counters() Counters {
	return Counters{
		SubmitRequests: s.submitReq.Load(), SubmitAccepted: s.submitAcc.Load(), SubmitRejected: s.submitRej.Load(),
		QueueFull: s.queueFull.Load(), StorageErrors: s.storageErr.Load(), Busy: s.busy.Load(),
		AuthFailures: s.authFail.Load(), AuthUnavailable: s.authUnavail.Load(), ExistsRequests: s.existsReq.Load(), ExistsUnknown: s.existsUnk.Load(),
		InternalErrors: s.internalErr.Load(),
	}
}

// Handler returns the routed handler (panics are recovered into a bare 500).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(APIPrefix+"/torrents/exists", s.handleExists)
	mux.HandleFunc(APIPrefix+"/torrents", s.handleSubmit)
	return s.recoverer(mux)
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				s.internalErr.Add(1)
				s.cfg.Stats.RequestRejected(opstats.RejectInternal, "")
				s.log.Printf("ingest: handler panic recovered (details withheld)")
				writeError(w, http.StatusInternalServerError, "internal_error", false)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code      string      `json:"code"`
	Retryable bool        `json:"retryable"`
	Items     []itemIssue `json:"items,omitempty"`
}

type itemIssue struct {
	Index int    `json:"index"`
	Code  string `json:"code"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code string, retryable bool) {
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Retryable: retryable}})
}

func writeRetryable(w http.ResponseWriter, code string, seconds int) {
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	writeError(w, http.StatusServiceUnavailable, code, true)
}

// preflight applies method, authentication and content-type checks in that
// order, before anything is read from the body. On success it returns the
// request carrying the authenticated crawler in its context.
func (s *Server) preflight(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		s.cfg.Stats.RequestRejected(opstats.RejectMalformed, "")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", false)
		return nil, false
	}
	id, err := s.cfg.Auth.Authenticate(r.Context(), bearerToken(r))
	switch {
	case err == nil:
		r = r.WithContext(crawlerauth.WithIdentity(r.Context(), id))
	case errors.Is(err, crawlerauth.ErrUnauthorized):
		s.authFail.Add(1)
		// A disabled crawler's refused request is also counted for that crawler;
		// an unknown or malformed credential has no crawler and counts globally.
		s.cfg.Stats.RequestRejected(opstats.RejectAuth, id.CrawlerID)
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, http.StatusUnauthorized, "unauthorized", false)
		return nil, false
	case r.Context().Err() != nil:
		return nil, false // client went away while a lookup was pending
	default:
		// Authentication could not be decided: retryable, and never a 401.
		s.authUnavail.Add(1)
		s.cfg.Stats.RequestRejected(opstats.RejectUnavailableAuth, "")
		var un crawlerauth.ErrUnavailable
		if errors.As(err, &un) && un.Busy {
			writeRetryable(w, "busy", 1)
		} else {
			writeRetryable(w, "storage_unavailable", 5)
		}
		return nil, false
	}
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		s.cfg.Stats.RequestRejected(opstats.RejectMalformed, id.CrawlerID)
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", false)
		return nil, false
	}
	return r, true
}

// countingReader counts the request-body bytes actually read.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (s *Server) clientAddr(r *http.Request) netip.Addr {
	return clientip.Resolve(r.RemoteAddr, r.Header.Values("X-Forwarded-For"), s.cfg.TrustedProxies)
}

// bearerToken extracts the Bearer credential ("" when absent or another scheme).
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

func acquire(slots chan struct{}) bool {
	select {
	case slots <- struct{}{}:
		return true
	default:
		return false
	}
}

// decodeEnvelope strictly decodes {"<field>": [...]} from the limited body,
// rejecting unknown members, a missing/null array and trailing data. The
// returned error is errBodyTooLarge for a limit breach.
var errBodyTooLarge = errors.New("body too large")

func decodeEnvelope(body io.Reader, field string, v any) error {
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return errBodyTooLarge
		}
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return errBodyTooLarge
		}
		return errors.New("trailing data")
	}
	return nil
}

// --- exists ------------------------------------------------------------

type existsRequest struct {
	InfoHashes *[]string `json:"info_hashes"`
}

type existsResponse struct {
	Exists  []string `json:"exists"`
	Missing []string `json:"missing"`
	Unknown []string `json:"unknown"`
}

func (s *Server) handleExists(w http.ResponseWriter, r *http.Request) {
	r, ok := s.preflight(w, r)
	if !ok {
		return
	}
	crawler, _ := crawlerauth.FromContext(r.Context())
	cid := crawler.CrawlerID
	if !acquire(s.exists) {
		s.busy.Add(1)
		s.cfg.Stats.RequestRejected(opstats.RejectBusy, cid)
		writeRetryable(w, "busy", 1)
		return
	}
	defer func() { <-s.exists }()
	s.existsReq.Add(1)
	s.cfg.Stats.ExistsRequest(cid)

	var req existsRequest
	body := &countingReader{r: http.MaxBytesReader(w, r.Body, MaxExistsBodyBytes)}
	defer func() { s.cfg.Stats.RequestBytes(cid, body.n) }()
	if err := decodeEnvelope(body, "info_hashes", &req); err != nil {
		if errors.Is(err, errBodyTooLarge) {
			s.cfg.Stats.RequestRejected(opstats.RejectTooLarge, cid)
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", false)
			return
		}
		s.cfg.Stats.RequestRejected(opstats.RejectMalformed, cid)
		writeError(w, http.StatusBadRequest, "invalid_request", false)
		return
	}
	if req.InfoHashes == nil || len(*req.InfoHashes) > MaxExistsHashes {
		s.cfg.Stats.RequestRejected(opstats.RejectMalformed, cid)
		writeError(w, http.StatusBadRequest, "invalid_request", false)
		return
	}
	hashes := make([][20]byte, 0, len(*req.InfoHashes))
	for _, hx := range *req.InfoHashes {
		var h [20]byte
		if len(hx) != 40 {
			s.cfg.Stats.RequestRejected(opstats.RejectMalformed, cid)
			writeError(w, http.StatusBadRequest, "invalid_request", false)
			return
		}
		if _, err := hex.Decode(h[:], []byte(hx)); err != nil {
			s.cfg.Stats.RequestRejected(opstats.RejectMalformed, cid)
			writeError(w, http.StatusBadRequest, "invalid_request", false)
			return
		}
		hashes = append(hashes, h)
	}

	res := s.cfg.Exist.Check(r.Context(), hashes)
	if r.Context().Err() != nil {
		return // client went away
	}
	s.existsUnk.Add(uint64(len(res.Unknown)))
	if len(res.Unknown) > 0 && len(res.Exists) == 0 && len(res.Missing) == 0 {
		// Nothing could be answered: a whole-request storage failure.
		s.storageErr.Add(1)
		s.cfg.Stats.RequestRejected(opstats.RejectStorageUnavailable, cid)
		writeRetryable(w, "storage_unavailable", 5)
		return
	}
	s.cfg.Stats.ExistsDone(cid, s.clientAddr(r), stats.ExistsOutcome{
		Hashes: len(res.Exists) + len(res.Missing) + len(res.Unknown),
		Found:  len(res.Exists), Missing: len(res.Missing), Unknown: len(res.Unknown),
		CacheHitPositive: res.CacheHitsPositive, CacheHitNegative: res.CacheHitsNegative, CacheMiss: res.CacheMisses,
	})
	out := existsResponse{
		Exists:  hexList(res.Exists),
		Missing: hexList(res.Missing),
		Unknown: hexList(res.Unknown),
	}
	writeJSON(w, http.StatusOK, out)
}

func hexList(hs [][20]byte) []string {
	out := make([]string, 0, len(hs))
	for _, h := range hs {
		out = append(out, hex.EncodeToString(h[:]))
	}
	return out
}

// --- submit ------------------------------------------------------------

type submitRequest struct {
	Records *[]json.RawMessage `json:"records"`
}

func (s *Server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	r, ok := s.preflight(w, r)
	if !ok {
		return
	}
	crawler, _ := crawlerauth.FromContext(r.Context())
	cid := crawler.CrawlerID
	s.cfg.Stats.SubmitAuthenticated(cid)
	if !acquire(s.submit) {
		s.busy.Add(1)
		s.cfg.Stats.RequestRejected(opstats.RejectBusy, cid)
		writeRetryable(w, "busy", 1)
		return
	}
	defer func() { <-s.submit }()
	s.submitReq.Add(1)

	var req submitRequest
	body := &countingReader{r: http.MaxBytesReader(w, r.Body, MaxSubmitBodyBytes)}
	defer func() { s.cfg.Stats.RequestBytes(cid, body.n) }()
	if err := decodeEnvelope(body, "records", &req); err != nil {
		s.submitRej.Add(1)
		if errors.Is(err, errBodyTooLarge) {
			s.cfg.Stats.RequestRejected(opstats.RejectTooLarge, cid)
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", false)
			return
		}
		s.cfg.Stats.RequestRejected(opstats.RejectMalformed, cid)
		writeError(w, http.StatusBadRequest, "invalid_request", false)
		return
	}
	if req.Records == nil || len(*req.Records) == 0 || len(*req.Records) > MaxSubmitRecords {
		s.submitRej.Add(1)
		s.cfg.Stats.RequestRejected(opstats.RejectMalformed, cid)
		writeError(w, http.StatusBadRequest, "invalid_request", false)
		return
	}
	raws := *req.Records

	items, status, code, issues := s.validateAll(raws)
	if status != 0 {
		s.submitRej.Add(1)
		if status == http.StatusRequestEntityTooLarge {
			s.cfg.Stats.RequestRejected(opstats.RejectTooLarge, cid)
		} else {
			s.cfg.Stats.RequestRejected(opstats.RejectInvalidRecord, cid)
		}
		if len(issues) > 0 {
			writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Items: issues}})
			return
		}
		writeError(w, status, code, false)
		return
	}

	// The records are received (validated); the recorder is not held across the
	// admission I/O: the outcome is recorded when it is known.
	s.cfg.Stats.SubmitValidated(cid, s.clientAddr(r), len(items))
	// Every validated record gets an outcome, even if admission panics.
	resolved := false
	defer func() {
		if !resolved {
			s.cfg.Stats.SubmitRefused(cid, len(items), stats.NoReject)
		}
	}()
	res, err := s.cfg.Queue.Admit(r.Context(), items)
	resolved = true
	switch {
	case err == nil:
	case errors.Is(err, pgqueue.ErrQueueFull):
		s.queueFull.Add(1)
		s.cfg.Stats.SubmitRefused(cid, len(items), opstats.RejectQueueFull)
		writeRetryable(w, "queue_full", 30)
		return
	case r.Context().Err() != nil:
		s.cfg.Stats.SubmitRefused(cid, len(items), stats.NoReject)
		return // client went away; the admission rolled back or committed idempotently
	case errors.Is(err, persistence.ErrStorageIO):
		s.storageErr.Add(1)
		s.cfg.Stats.SubmitRefused(cid, len(items), opstats.RejectStorageUnavailable)
		writeRetryable(w, "storage_unavailable", 5)
		return
	default:
		s.internalErr.Add(1)
		s.cfg.Stats.SubmitRefused(cid, len(items), opstats.RejectInternal)
		s.log.Printf("ingest: admission failed with an unclassified error (details withheld)")
		writeError(w, http.StatusInternalServerError, "internal_error", true)
		return
	}
	s.cfg.Stats.SubmitAcked(cid, res.Inserted, res.Duplicates, res.InsertedBytes)
	s.submitAcc.Add(uint64(len(items)))
	if s.cfg.OnAdmitted != nil {
		s.cfg.OnAdmitted()
	}
	writeJSON(w, http.StatusAccepted, map[string]int{"accepted": len(items)})
}

// validateAll applies the spec §5.3 table. A non-zero status means the whole
// request is rejected and nothing may be written.
func (s *Server) validateAll(raws []json.RawMessage) (items []pgqueue.Item, status int, code string, issues []itemIssue) {
	items = make([]pgqueue.Item, 0, len(raws))
	var failed []itemIssue
	anyTooLarge := false
	for i, raw := range raws {
		it, c := s.classify(raw)
		if c == "" {
			items = append(items, it)
			continue
		}
		if c == "record_too_large" {
			anyTooLarge = true
		}
		failed = append(failed, itemIssue{Index: i, Code: c})
	}
	if len(failed) == 0 {
		return items, 0, "", nil
	}
	if len(raws) == 1 {
		switch failed[0].Code {
		case "record_too_large":
			return nil, http.StatusRequestEntityTooLarge, "record_too_large", nil
		default: // unsupported_version, invalid_record
			return nil, http.StatusBadRequest, failed[0].Code, nil
		}
	}
	if anyTooLarge {
		return nil, http.StatusRequestEntityTooLarge, "payload_too_large", nil
	}
	if len(failed) > maxBatchDiagnostics {
		failed = failed[:maxBatchDiagnostics]
	}
	return nil, http.StatusBadRequest, "invalid_batch", failed
}

// classify validates one record. Precedence: unsupported_version >
// record_too_large > invalid_record.
func (s *Server) classify(raw []byte) (pgqueue.Item, string) {
	if len(raw) > record.MaxRecordBytes {
		if errors.Is(record.CheckVersion(raw), record.ErrUnsupportedVersion) {
			return pgqueue.Item{}, "unsupported_version"
		}
		return pgqueue.Item{}, "record_too_large"
	}
	rec, err := record.Decode(raw, s.cfg.Limits)
	switch {
	case err == nil:
		return pgqueue.Item{InfoHash: rec.InfoHash, Payload: raw}, ""
	case errors.Is(err, record.ErrUnsupportedVersion):
		return pgqueue.Item{}, "unsupported_version"
	case errors.Is(err, record.ErrRecordTooLarge):
		return pgqueue.Item{}, "record_too_large"
	default:
		return pgqueue.Item{}, "invalid_record"
	}
}
