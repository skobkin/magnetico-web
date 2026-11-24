package users

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/skobkin/magnetico-web/internal/migration/pgidentity"
	"github.com/skobkin/magnetico-web/internal/web/webschema"
)

// advisoryKey serializes concurrent `migrate users` runs; a neighbour of the
// webschema initialization keys (…187, …188).
const advisoryKey int64 = 7881644658832750189

// lockTimeout bounds how long a final run waits for a user table (a variable
// only so a test need not wait five seconds).
var lockTimeout = "5s"

// testHook is set only by tests: it is called at named stages inside the
// target transaction of a final run, and a non-nil result aborts the run like
// any other database error. It is nil in production.
var testHook func(stage string, tx pgx.Tx) error

func hook(stage string, tx pgx.Tx) *Error {
	if testHook == nil {
		return nil
	}
	if err := testHook(stage, tx); err != nil {
		return storageErr(stage, err)
	}
	return nil
}

// Config is the validated input of Run and Verify. The DSNs come from the
// config file or environment only (no flag), and are never printed.
type Config struct {
	SourceDSN, TargetDSN string
	SourceTimezone       string

	DryRun              bool
	AcceptNeedsReset    int
	ReplaceUsers        bool
	ConfirmPlan         string
	DeleteUnmatchedIDs  []int64
	SourceWritesStopped bool
}

// ParseIDList parses a comma-separated list of positive user ids.
func ParseIDList(s string) ([]int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	seen := map[int64]bool{}
	var out []int64
	for _, part := range strings.Split(s, ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil || id <= 0 {
			return nil, errors.New("not a comma-separated list of positive integers")
		}
		if seen[id] {
			return nil, errors.New("duplicate id in the list")
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

func (c Config) location() (*time.Location, *Error) {
	if c.SourceTimezone == "" {
		return nil, newErr(ClassConfig, "--source-timezone is required (an IANA zone name; there is no default)")
	}
	if c.SourceTimezone == "Local" {
		return nil, newErr(ClassConfig, "--source-timezone must name an explicit IANA zone, not Local")
	}
	loc, err := time.LoadLocation(c.SourceTimezone)
	if err != nil {
		return nil, newErr(ClassConfig, "--source-timezone is not a known IANA zone name")
	}
	return loc, nil
}

func (c Config) validateCommon() (*time.Location, *Error) {
	if c.SourceDSN == "" {
		return nil, newErr(ClassConfig, "no source database URL (set MAGNETICO_MIGRATION_USERS_SOURCE_DSN or the migrateUsersSourceDatabaseURL file key; there is no fallback to the target)")
	}
	if c.TargetDSN == "" {
		return nil, newErr(ClassConfig, "no target database URL")
	}
	return c.location()
}

func (c Config) validateRun() (*time.Location, *Error) {
	loc, err := c.validateCommon()
	if err != nil {
		return nil, err
	}
	if c.AcceptNeedsReset < 0 {
		return nil, newErr(ClassConfig, "--accept-needs-reset must not be negative")
	}
	if len(c.DeleteUnmatchedIDs) > 0 && !c.ReplaceUsers {
		return nil, newErr(ClassConfig, "--delete-unmatched-user-ids requires --replace-users")
	}
	if !c.DryRun {
		if c.ReplaceUsers && c.ConfirmPlan == "" {
			return nil, newErr(ClassConfig, "a real --replace-users run requires --confirm-plan <digest> from a previous --dry-run with the same options")
		}
		if !c.SourceWritesStopped {
			return nil, newErr(ClassSourceWritesNotAssert,
				"a final run is an offline operation: stop the legacy application, prevent legacy writes, then pass --source-writes-stopped (see the cutover checklist)")
		}
	}
	return loc, nil
}

type runCtx struct {
	cfg  Config
	loc  *time.Location
	rep  *Report
	snap *Snapshot
	an   *Analysis
	exp  *Expected
}

func (r *runCtx) fillSource() {
	r.rep.Source = Counts{Users: len(r.snap.Users), Invites: len(r.snap.Invites), UsedInvites: r.an.UsedInvites, Tokens: len(r.snap.Tokens)}
	r.rep.SourceDigest = r.snap.Digest
	r.rep.NeedsReset, r.rep.Unvetted, r.rep.Verified = r.an.NeedsReset, r.an.Unvetted, r.an.Verified
	r.rep.Blocking, r.rep.Warnings = r.an.Blocking.list(), r.an.Warnings.list()
	r.rep.Notes = append(r.rep.Notes, r.snap.Notes...)
}

// readSource opens the source, records the isolation evidence, reads the
// snapshot and analyses it. A *Error is final for the run.
func (r *runCtx) readSource(ctx context.Context, src *source) *Error {
	ev, err := src.evidence(ctx)
	if err != nil {
		return err
	}
	r.rep.SourceOtherClients, r.rep.SourceDBReadOnly = ev.OtherClientBackends, ev.DatabaseReadOnly
	snap, err := src.snapshot(ctx)
	if err != nil {
		return err
	}
	r.snap, r.an = snap, Analyze(snap)
	r.fillSource()
	if r.an.Blocking["source-empty"] != nil {
		return newErr(ClassSourceEmpty, "the source contains no users (most likely the wrong database)")
	}
	if len(r.an.Blocking) > 0 {
		var d []string
		for _, f := range r.rep.Blocking {
			d = append(d, fmt.Sprintf("%s x%d", f.Code, f.Count))
		}
		return newErr(ClassDataBlocked, "the source holds records that cannot be stored in the target unchanged; fix the legacy data (nothing is transformed or dropped)", d...)
	}
	r.exp = BuildExpected(r.snap, r.loc)
	return nil
}

func sameDatabase(ctx context.Context, srcID pgidentity.Identity, tgt *pgx.Conn) (bool, *Error) {
	tid, err := pgidentity.Read(ctx, tgt)
	if err != nil {
		return false, storageErr("target identity", err)
	}
	same, ok := pgidentity.SameDatabase(srcID, tid)
	return same || !ok, nil // unknown identity fails closed toward "same"
}

func targetSchemaErr(err error) *Error {
	var se *webschema.StateError
	switch {
	case errors.Is(err, webschema.ErrWebSchemaMissing):
		return newErr(ClassTargetSchema, "the magnetico_web schema is missing (run migrate web-schema-init)")
	case errors.As(err, &se):
		return newErr(ClassTargetSchema, "the magnetico_web schema is not exactly the supported version; it is never repaired", se.Details...)
	}
	return storageErr("verify target schema", err)
}

type privMode int

const (
	privRead privMode = iota
	privWrite
	privReplace
)

// checkPrivileges verifies the role can do what this run will do. Missing
// privileges are reported by schema-qualified name only.
func checkPrivileges(ctx context.Context, q queryer, mode privMode) *Error {
	var ok bool
	if err := q.QueryRow(ctx, `SELECT has_schema_privilege(current_user, 'magnetico_web', 'USAGE')`).Scan(&ok); err != nil {
		return storageErr("check privileges", err)
	}
	if !ok {
		return newErr(ClassPrivileges, "the database role lacks privileges required by the migration", "magnetico_web: USAGE")
	}
	var missing []string
	has := func(tbl, priv string) *Error {
		var g bool
		if err := q.QueryRow(ctx, `SELECT has_table_privilege(current_user, $1, $2)`, "magnetico_web."+tbl, priv).Scan(&g); err != nil {
			return storageErr("check privileges", err)
		}
		if !g {
			missing = append(missing, "magnetico_web."+tbl+": "+priv)
		}
		return nil
	}
	for _, t := range userTables {
		if e := has(t, "SELECT"); e != nil {
			return e
		}
	}
	for _, t := range []string{"users", "invites", "rss_tokens"} {
		var g bool
		if err := q.QueryRow(ctx, `SELECT has_sequence_privilege(current_user, pg_get_serial_sequence($1, 'id'), 'SELECT')`, "magnetico_web."+t).Scan(&g); err != nil {
			return storageErr("check privileges", err)
		}
		if !g {
			missing = append(missing, "identity sequence of magnetico_web."+t+": SELECT")
		}
	}
	if mode >= privWrite {
		for _, t := range []string{"users", "invites", "rss_tokens"} {
			if e := has(t, "INSERT"); e != nil {
				return e
			}
			var owner bool
			if err := q.QueryRow(ctx, `SELECT pg_has_role(current_user, c.relowner, 'USAGE') FROM pg_class c WHERE c.oid = $1::regclass`, "magnetico_web."+t).Scan(&owner); err != nil {
				return storageErr("check privileges", err)
			}
			if !owner {
				missing = append(missing, "magnetico_web."+t+": ownership (needed for the transactional identity RESTART)")
			}
		}
	}
	if mode >= privReplace {
		for _, t := range userTables {
			if e := has(t, "DELETE"); e != nil {
				return e
			}
		}
	}
	if len(missing) > 0 {
		return newErr(ClassPrivileges, "the database role lacks privileges required by the migration", missing...)
	}
	return nil
}

// ---- plan, scope, refusals ----

type scope struct {
	class     Class
	unmatched []int64 // target accounts with no source account of the same id and username
	digest    string
}

func idsString(ids []int64) string {
	s := make([]string, len(ids))
	for i, v := range ids {
		s[i] = strconv.FormatInt(v, 10)
	}
	return strings.Join(s, ",")
}

func equalIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// evaluateScope classifies the target, finds the unmatched accounts and
// computes the plan digest (plan §4.3).
func (r *runCtx) evaluateScope(ts *TargetState) scope {
	sc := scope{class: classify(ts, r.exp, r.cfg.ReplaceUsers)}
	if sc.class == ClassReplace {
		src := map[int64]string{}
		for _, u := range r.exp.Users {
			src[u.ID] = u.Username
		}
		for _, u := range ts.Users {
			if name, ok := src[u.ID]; !ok || name != u.Username {
				sc.unmatched = append(sc.unmatched, u.ID)
			}
		}
	}
	authorised := append([]int64(nil), r.cfg.DeleteUnmatchedIDs...)
	sort.Slice(authorised, func(i, j int) bool { return authorised[i] < authorised[j] })

	h := newHasher("magnetico-users-plan-v1")
	h.str(r.snap.Digest)
	h.str(r.cfg.SourceTimezone)
	h.str(string(sc.class))
	if r.cfg.ReplaceUsers {
		h.str("replace")
	} else {
		h.str("no-replace")
	}
	h.i64(int64(r.cfg.AcceptNeedsReset))
	h.str(idsString(authorised))
	// The identities' current next values decide the ids a run assigns and
	// where each identity ends, so they are part of the plan.
	for _, t := range []string{"users", "invites", "rss_tokens"} {
		h.str(t)
		h.i64(ts.Next[t])
	}
	if r.cfg.ReplaceUsers {
		matched := []int64{}
		un := map[int64]bool{}
		for _, id := range sc.unmatched {
			un[id] = true
		}
		for _, u := range ts.Users {
			if !un[u.ID] {
				matched = append(matched, u.ID)
			}
		}
		h.str(idsString(matched))
		c := ts.Counts()
		for _, n := range []int{c.Users, c.Invites, c.Tokens, c.APITokens, c.Sessions, c.OneTime} {
			h.i64(int64(n))
		}
		h.str(ts.RowDigest)
	}
	sc.digest = h.sum()
	return sc
}

// refusals lists, in priority order, every reason a final run would refuse
// before writing. A dry-run reports them all as would_refuse.
func (r *runCtx) refusals(ts *TargetState, sc scope) []*Error {
	var out []*Error
	if r.an.Unvetted > 0 {
		out = append(out, newErr(ClassUnvettedHash,
			"accounts hold password hash parameters that the verifier parses but that have no independent PHP test vectors; add vectors for those exact parameters first",
			fmt.Sprintf("%d account(s)", r.an.Unvetted), "user ids: "+idsString(r.an.UnvettedIDs)))
	}
	if r.an.NeedsReset != r.cfg.AcceptNeedsReset {
		out = append(out, newErr(ClassNeedsResetUnack,
			"accounts with unsupported password hashes cannot log in until an administrator resets them; acknowledge the exact number with --accept-needs-reset",
			fmt.Sprintf("found %d, acknowledged %d", r.an.NeedsReset, r.cfg.AcceptNeedsReset), "user ids: "+idsString(r.an.NeedsResetIDs)))
	}
	switch sc.class {
	case ClassDiverged:
		c := ts.Counts()
		out = append(out, newErr(ClassTargetNotEmpty,
			"the target already holds user data that is not exactly the expected import; nothing is replaced without --replace-users",
			fmt.Sprintf("users=%d invites=%d rss_tokens=%d api_tokens=%d sessions=%d one_time_tokens=%d",
				c.Users, c.Invites, c.Tokens, c.APITokens, c.Sessions, c.OneTime)))
	case ClassReplace:
		auth := append([]int64(nil), r.cfg.DeleteUnmatchedIDs...)
		sort.Slice(auth, func(i, j int) bool { return auth[i] < auth[j] })
		if !equalIDs(sc.unmatched, auth) {
			out = append(out, newErr(ClassUnmatchedTarget,
				"target accounts that are not in the source would be deleted; --delete-unmatched-user-ids must equal that set exactly",
				"unmatched target user ids: ["+idsString(sc.unmatched)+"]", "authorised: ["+idsString(auth)+"]"))
		}
	case ClassEmpty, ClassIdentical:
		if len(r.cfg.DeleteUnmatchedIDs) > 0 {
			out = append(out, newErr(ClassUnmatchedTarget,
				"--delete-unmatched-user-ids lists accounts but there are no unmatched target accounts",
				"authorised: ["+idsString(r.cfg.DeleteUnmatchedIDs)+"]"))
		}
	}
	if r.cfg.ConfirmPlan != "" && r.cfg.ConfirmPlan != sc.digest {
		out = append(out, newErr(ClassPlanMismatch, "the confirmed plan digest does not match the plan computed now (source, timezone, target or options changed)"))
	}
	return out
}

func (r *runCtx) discarded(ts *TargetState) *Discarded {
	d := &Discarded{Users: len(ts.Users), Invites: len(ts.Invites), Sessions: ts.Session, APITokens: ts.API,
		RSSTokens: len(ts.RSS), OneTimeLinks: ts.OneTime}
	exp := map[int64]ExpUser{}
	for _, u := range r.exp.Users {
		exp[u.ID] = u
	}
	for _, u := range ts.Users {
		if e, ok := exp[u.ID]; !ok || userDiffers(u, e) != "" {
			d.DiffersFromSource++
		}
		if u.Role == "admin" {
			d.AdminRoles++
		}
		if !u.Active {
			d.DisabledAccounts++
		}
		if u.Verified != nil {
			d.VerifiedEmails++
		}
	}
	for _, t := range ts.RSS {
		if !t.Legacy {
			d.NonLegacyRSSTokens++
		}
	}
	srcInv := map[int64]string{}
	for _, v := range r.exp.Invites {
		srcInv[v.ID] = v.Code
	}
	for _, v := range ts.Invites {
		if code, ok := srcInv[v.ID]; !ok || code != v.Code {
			d.InvitesNotInSource++
		}
	}
	return d
}

// ---- Run ----

func fail(rep *Report, e *Error) (*Report, error) {
	rep.Class = e.Class
	switch e.Class {
	case ClassStorage, ClassLockTimeout, ClassVerifyFailed, ClassSourceChanged:
		rep.Result = "failed"
	default:
		rep.Result = "refused"
	}
	return rep, e
}

// Run executes `migrate users` (a dry-run or the final import). The returned
// report is always non-nil; the error, when set, is an *Error.
func Run(ctx context.Context, cfg Config) (*Report, error) {
	mode := "final"
	if cfg.DryRun {
		mode = "dry-run"
	}
	rep := &Report{Mode: mode, Timezone: cfg.SourceTimezone}
	loc, e := cfg.validateRun()
	if e != nil {
		return fail(rep, e)
	}
	rc := &runCtx{cfg: cfg, loc: loc, rep: rep}

	src, e := openSource(ctx, cfg.SourceDSN)
	if e != nil {
		return fail(rep, e)
	}
	defer src.close()
	if e := rc.readSource(ctx, src); e != nil {
		return fail(rep, e)
	}

	tcfg, err := pgx.ParseConfig(cfg.TargetDSN)
	if err != nil {
		return fail(rep, newErr(ClassConfig, "the target database URL cannot be parsed"))
	}
	if tcfg.RuntimeParams == nil {
		tcfg.RuntimeParams = map[string]string{}
	}
	tcfg.RuntimeParams["application_name"] = "magnetico_migrate_users"
	if cfg.DryRun {
		tcfg.RuntimeParams["default_transaction_read_only"] = "on"
	}
	tgt, err := pgx.ConnectConfig(ctx, tcfg)
	if err != nil {
		return fail(rep, storageErr("connect target", err))
	}
	defer func() { _ = tgt.Close(context.Background()) }()
	ev, e := src.evidence(ctx)
	if e != nil {
		return fail(rep, e)
	}
	same, e := sameDatabase(ctx, ev.Identity, tgt)
	if e != nil {
		return fail(rep, e)
	}
	rep.SameDatabase = same

	if cfg.DryRun {
		return rc.dryRun(ctx, tgt)
	}
	return rc.final(ctx, tgt, src)
}

func (r *runCtx) dryRun(ctx context.Context, tgt *pgx.Conn) (*Report, error) {
	tx, err := tgt.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return fail(r.rep, storageErr("begin target", err))
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, "SET LOCAL search_path = pg_catalog"); err != nil {
		return fail(r.rep, storageErr("target search_path", err))
	}
	if err := webschema.VerifyIn(ctx, tx); err != nil {
		return fail(r.rep, targetSchemaErr(err))
	}
	mode := privRead
	if e := checkPrivileges(ctx, tx, mode); e != nil {
		return fail(r.rep, e)
	}
	ts, e := readTarget(ctx, tx)
	if e != nil {
		return fail(r.rep, e)
	}
	r.rep.Target = ts.Counts()
	r.rep.NextIDs = ts.Next
	sc := r.evaluateScope(ts)
	r.rep.TargetState, r.rep.PlanDigest = string(sc.class), sc.digest
	r.rep.UnmatchedTargetUserIDs = sc.unmatched
	if sc.class == ClassReplace {
		r.rep.Discarded = r.discarded(ts)
	}
	refusals := r.refusals(ts, sc)
	if len(refusals) > 0 {
		for _, x := range refusals {
			r.rep.WouldRefuse = append(r.rep.WouldRefuse, x.Class)
		}
		r.rep.Notes = append(r.rep.Notes, "dry-run: nothing was written to either database")
		return fail(r.rep, refusals[0])
	}
	r.rep.Result = "ok"
	if sc.class == ClassIdentical {
		r.rep.Result = "already-migrated"
	}
	r.rep.Notes = append(r.rep.Notes, "dry-run: nothing was written to either database")
	return r.rep, nil
}

func (r *runCtx) final(ctx context.Context, tgt *pgx.Conn, src *source) (*Report, error) {
	tx, err := tgt.Begin(ctx)
	if err != nil {
		return fail(r.rep, storageErr("begin target", err))
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	for _, stmt := range []string{
		"SET LOCAL lock_timeout = '" + lockTimeout + "'",
		"SET LOCAL search_path = pg_catalog",
	} {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return fail(r.rep, storageErr("target session setup", err))
		}
	}
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", advisoryKey); err != nil {
		return fail(r.rep, storageErr("target advisory lock", err))
	}
	if err := webschema.VerifyIn(ctx, tx); err != nil {
		return fail(r.rep, targetSchemaErr(err))
	}
	mode := privWrite
	if r.cfg.ReplaceUsers {
		mode = privReplace
	}
	if e := checkPrivileges(ctx, tx, mode); e != nil {
		return fail(r.rep, e)
	}
	// ACCESS EXCLUSIVE serializes against conflicting database operations for
	// the transaction. It does not prove that the web service is stopped.
	locked := append([]string(nil), userTables...)
	sort.Strings(locked)
	for i := range locked {
		locked[i] = "magnetico_web." + locked[i]
	}
	if _, err := tx.Exec(ctx, "LOCK TABLE "+strings.Join(locked, ", ")+" IN ACCESS EXCLUSIVE MODE"); err != nil {
		return fail(r.rep, storageErr("lock user tables", err))
	}
	ts, e := readTarget(ctx, tx)
	if e != nil {
		return fail(r.rep, e)
	}
	r.rep.Target = ts.Counts()
	sc := r.evaluateScope(ts)
	r.rep.TargetState, r.rep.PlanDigest = string(sc.class), sc.digest
	r.rep.UnmatchedTargetUserIDs = sc.unmatched
	if refusals := r.refusals(ts, sc); len(refusals) > 0 {
		for _, x := range refusals {
			r.rep.WouldRefuse = append(r.rep.WouldRefuse, x.Class)
		}
		return fail(r.rep, refusals[0])
	}
	if sc.class == ClassIdentical {
		r.rep.Result = "already-migrated"
		r.rep.NextIDs = ts.Next
		r.rep.Notes = append(r.rep.Notes, "the target already holds exactly this import; nothing was written")
		return r.rep, nil
	}
	if sc.class == ClassReplace {
		r.rep.Discarded = r.discarded(ts)
		for _, t := range userTables {
			if _, err := tx.Exec(ctx, "DELETE FROM magnetico_web."+t); err != nil {
				return fail(r.rep, storageErr("delete "+t, err))
			}
		}
		if e := hook("after-delete", tx); e != nil {
			return fail(r.rep, e)
		}
	}
	if e := r.importRows(ctx, tx, ts.Next); e != nil {
		return fail(r.rep, e)
	}
	after, e := readTarget(ctx, tx)
	if e != nil {
		return fail(r.rep, e)
	}
	mm := reconcile(after, r.exp)
	for _, x := range []*mismatches{referential(after), sequencesAhead(after)} {
		mm.total += x.total
		mm.list = append(mm.list, x.list...)
	}
	if mm.total > 0 {
		r.rep.Mismatches, r.rep.MismatchCount = mm.list, mm.total
		return fail(r.rep, newErr(ClassVerifyFailed, "the import does not reconcile with the source; the transaction was rolled back"))
	}
	r.rep.NextIDs = after.Next

	// Defence in depth only: a fresh read of the source. It detects a legacy
	// write that happened before this point; it does not make the two
	// databases one atomic snapshot (write isolation is an operator step).
	if e := hook("before-drift-check", tx); e != nil {
		return fail(r.rep, e)
	}
	again, serr := src.snapshot(ctx)
	if serr != nil {
		return fail(r.rep, serr)
	}
	if again.Digest != r.snap.Digest {
		return fail(r.rep, newErr(ClassSourceChanged, "the source changed while the migration was running; the transaction was rolled back"))
	}
	if err := tx.Commit(ctx); err != nil {
		return fail(r.rep, storageErr("commit", err))
	}
	r.rep.ImportedUsers, r.rep.ImportedInvites, r.rep.ImportedTokens = len(r.exp.Users), len(r.exp.Invites), len(r.exp.Tokens)
	r.rep.Result = "ok"
	return r.rep, nil
}

// importRows inserts every row with an explicit id (no nextval) and then
// advances the three identities with the transactional RESTART. next holds
// the identity values read under the lock before anything was changed.
func (r *runCtx) importRows(ctx context.Context, tx pgx.Tx, next map[string]int64) *Error {
	for _, u := range r.exp.Users {
		if _, err := tx.Exec(ctx, `INSERT INTO magnetico_web.users
            (id, username, email, email_verified_at, password_hash, active, role, created_at, password_changed_at, last_login_at)
            OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, NULL, $4, true, 'user', $5, NULL, NULL)`,
			u.ID, u.Username, u.Email, u.Hash, u.CreatedAt); err != nil {
			return storageErr("insert user", err)
		}
	}
	if e := hook("after-users", tx); e != nil {
		return e
	}
	for _, v := range r.exp.Invites {
		if _, err := tx.Exec(ctx, `INSERT INTO magnetico_web.invites (id, owner_id, code, used_by, used_at, created_at, expires_at, revoked_at)
            OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, NULL, $5, NULL, NULL)`,
			v.ID, v.OwnerID, v.Code, v.UsedBy, v.CreatedAt); err != nil {
			return storageErr("insert invite", err)
		}
	}
	if e := hook("after-invites", tx); e != nil {
		return e
	}
	id := next["rss_tokens"]
	for _, t := range r.exp.Tokens {
		if _, err := tx.Exec(ctx, `INSERT INTO magnetico_web.rss_tokens (id, token_hash, user_id, label, created_at, legacy)
            OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, NULL, $4, true)`,
			id, t.Digest, t.UserID, t.CreatedAt); err != nil {
			return storageErr("insert rss token", err)
		}
		id++
	}
	if e := hook("after-tokens", tx); e != nil {
		return e
	}
	for _, tbl := range []string{"users", "invites", "rss_tokens"} {
		var maxID int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(max(id), 0) FROM magnetico_web.`+tbl).Scan(&maxID); err != nil {
			return storageErr("read max id of "+tbl, err)
		}
		n := max(maxID+1, next[tbl]) // never backward
		if _, err := tx.Exec(ctx, fmt.Sprintf("ALTER TABLE magnetico_web.%s ALTER COLUMN id RESTART WITH %d", tbl, n)); err != nil {
			return storageErr("advance identity of "+tbl, err)
		}
	}
	return hook("after-restart", tx)
}

// Verify is `migrate users verify`: read-only on both sides (separate
// snapshots), exit status 0 only on an exact match.
func Verify(ctx context.Context, cfg Config) (*Report, error) {
	rep := &Report{Mode: "verify", Timezone: cfg.SourceTimezone}
	loc, e := cfg.validateCommon()
	if e != nil {
		return fail(rep, e)
	}
	rc := &runCtx{cfg: cfg, loc: loc, rep: rep}
	src, e := openSource(ctx, cfg.SourceDSN)
	if e != nil {
		return fail(rep, e)
	}
	defer src.close()
	if e := rc.readSource(ctx, src); e != nil {
		return fail(rep, e)
	}
	tcfg, err := pgx.ParseConfig(cfg.TargetDSN)
	if err != nil {
		return fail(rep, newErr(ClassConfig, "the target database URL cannot be parsed"))
	}
	if tcfg.RuntimeParams == nil {
		tcfg.RuntimeParams = map[string]string{}
	}
	tcfg.RuntimeParams["default_transaction_read_only"] = "on"
	tcfg.RuntimeParams["application_name"] = "magnetico_migrate_users_verify"
	tgt, err := pgx.ConnectConfig(ctx, tcfg)
	if err != nil {
		return fail(rep, storageErr("connect target", err))
	}
	defer func() { _ = tgt.Close(context.Background()) }()
	tx, err := tgt.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return fail(rep, storageErr("begin target", err))
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, "SET LOCAL search_path = pg_catalog"); err != nil {
		return fail(rep, storageErr("target search_path", err))
	}
	if err := webschema.VerifyIn(ctx, tx); err != nil {
		return fail(rep, targetSchemaErr(err))
	}
	if e := checkPrivileges(ctx, tx, privRead); e != nil {
		return fail(rep, e)
	}
	ts, e := readTarget(ctx, tx)
	if e != nil {
		return fail(rep, e)
	}
	rep.Target, rep.NextIDs = ts.Counts(), ts.Next
	mm := reconcile(ts, rc.exp)
	for _, x := range []*mismatches{referential(ts), sequencesAhead(ts)} {
		mm.total += x.total
		mm.list = append(mm.list, x.list...)
	}
	if mm.total > 0 {
		rep.Mismatches, rep.MismatchCount = mm.list, mm.total
		return fail(rep, newErr(ClassVerifyFailed, "the target does not match the source",
			fmt.Sprintf("%d mismatch(es); see the report", mm.total)))
	}
	rep.Result = "verified"
	return rep, nil
}
