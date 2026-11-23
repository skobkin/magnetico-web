package users

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// Error classes (docs/wip/07H-user-migration-plan.md §7). The class and the
// fixed message are the only error text callers may show.
const (
	ClassConfig                = "config"
	ClassSourceSchema          = "source-schema"
	ClassTargetSchema          = "target-schema"
	ClassPrivileges            = "privileges"
	ClassSourceEmpty           = "source-empty"
	ClassTargetNotEmpty        = "target-not-empty"
	ClassUnmatchedTarget       = "unmatched-target-accounts"
	ClassPlanMismatch          = "plan-mismatch"
	ClassNeedsResetUnack       = "needs-reset-unacknowledged"
	ClassUnvettedHash          = "unvetted-hash-format"
	ClassSourceWritesNotAssert = "source-writes-not-asserted"
	ClassDataBlocked           = "data-blocked"
	ClassLockTimeout           = "lock-timeout"
	ClassSourceChanged         = "source-changed"
	ClassVerifyFailed          = "verify-failed"
	ClassStorage               = "storage"
)

// Error is a classified, secret-safe failure.
type Error struct {
	Class   string
	Message string
	Details []string // ids, counts, object names; never data values
}

func (e *Error) Error() string {
	if len(e.Details) == 0 {
		return e.Class + ": " + e.Message
	}
	return e.Class + ": " + e.Message + " (" + strings.Join(e.Details, "; ") + ")"
}

func newErr(class, msg string, details ...string) *Error {
	return &Error{Class: class, Message: msg, Details: details}
}

// storageErr reduces any database or I/O error to text that cannot carry a
// value: the SQLSTATE and constraint name of a PostgreSQL error (never its
// Message, Detail or Where, which echo keys such as Key (code)=(…)), or the
// Go type of anything else. A lock timeout is its own class.
func storageErr(op string, err error) *Error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		if pe.Code == "55P03" {
			return newErr(ClassLockTimeout,
				"a user table is locked by another session (the web service or an operator command is probably running)")
		}
		d := []string{op, "SQLSTATE " + pe.Code}
		if pe.ConstraintName != "" {
			d = append(d, "constraint "+pe.ConstraintName)
		}
		return newErr(ClassStorage, "database error", d...)
	}
	if isContextDone(err) {
		return newErr(ClassStorage, "interrupted; the target transaction was rolled back", op)
	}
	return newErr(ClassStorage, "database or connection error", op, fmt.Sprintf("%T", err))
}

// Finding is one aggregated data finding; IDs are numeric record ids of the
// kind the code names (user, invite or token owner).
type Finding struct {
	Code  string  `json:"code"`
	Count int     `json:"count"`
	IDs   []int64 `json:"ids,omitempty"`
}

// Counts are row counts, per table.
type Counts struct {
	Users        int `json:"users"`
	Invites      int `json:"invites"`
	UsedInvites  int `json:"used_invites"`
	Tokens       int `json:"tokens"`
	APITokens    int `json:"api_tokens"`
	Sessions     int `json:"sessions"`
	OneTime      int `json:"one_time_tokens"`
	LegacyTokens int `json:"legacy_rss_tokens"`
}

// Report is the machine-readable result of a run. It never contains DSNs,
// hashes, token keys or digests of them, invitation codes, emails or
// usernames; SourceDigest and PlanDigest are digests over the whole data set.
type Report struct {
	Mode   string `json:"mode"`   // dry-run | final | verify
	Result string `json:"result"` // ok | refused | failed | already-migrated | verified
	Class  string `json:"error_class,omitempty"`

	SourceDigest string `json:"source_digest,omitempty"`
	PlanDigest   string `json:"plan_digest,omitempty"`
	Timezone     string `json:"source_timezone,omitempty"`
	TargetState  string `json:"target_state,omitempty"` // empty | identical | replace | diverged

	SameDatabase       bool `json:"same_database"`
	SourceOtherClients int  `json:"source_other_client_backends"`
	SourceDBReadOnly   bool `json:"source_database_default_read_only"`

	Source Counts `json:"source"`
	Target Counts `json:"target_before"`

	NeedsReset int `json:"needs_reset"`
	Unvetted   int `json:"unvetted"`
	Verified   int `json:"verified"`

	Blocking []Finding `json:"blocking,omitempty"`
	Warnings []Finding `json:"warnings,omitempty"`

	WouldRefuse []string `json:"would_refuse,omitempty"`

	UnmatchedTargetUserIDs []int64    `json:"unmatched_target_user_ids,omitempty"`
	Discarded              *Discarded `json:"discarded,omitempty"`

	Mismatches      []string `json:"mismatches,omitempty"`
	MismatchCount   int      `json:"mismatch_count,omitempty"`
	ImportedUsers   int      `json:"imported_users,omitempty"`
	ImportedInvites int      `json:"imported_invites,omitempty"`
	ImportedTokens  int      `json:"imported_rss_tokens,omitempty"`

	NextIDs map[string]int64 `json:"next_ids,omitempty"`
	Notes   []string         `json:"notes,omitempty"`
}

// Discarded counts what --replace-users throws away (the warning block).
type Discarded struct {
	Users              int `json:"users"`
	Invites            int `json:"invites"`
	Sessions           int `json:"sessions"`
	APITokens          int `json:"api_tokens"`
	RSSTokens          int `json:"rss_tokens"`
	NonLegacyRSSTokens int `json:"non_legacy_rss_tokens"`
	OneTimeLinks       int `json:"one_time_links"`
	DiffersFromSource  int `json:"accounts_whose_data_differs_from_source"`
	AdminRoles         int `json:"admin_roles"`
	DisabledAccounts   int `json:"disabled_accounts"`
	VerifiedEmails     int `json:"verified_emails"`
	InvitesNotInSource int `json:"invites_not_in_source"`
}

type findingSet map[string][]int64

func (f findingSet) add(code string, id int64) { f[code] = append(f[code], id) }

const maxFindingIDs = 1000

func (f findingSet) list() []Finding {
	codes := make([]string, 0, len(f))
	for c := range f {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	out := make([]Finding, 0, len(codes))
	for _, c := range codes {
		ids := append([]int64(nil), f[c]...)
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		n := len(ids)
		if len(ids) > maxFindingIDs {
			ids = ids[:maxFindingIDs]
		}
		out = append(out, Finding{Code: c, Count: n, IDs: ids})
	}
	return out
}

// WriteReport writes the report as indented JSON with file mode 0600,
// creating the file exclusively (an existing file is never overwritten).
func WriteReport(path string, r *Report) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("report file: %w", err)
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		_ = f.Close()
		return fmt.Errorf("report file: %w", err)
	}
	return f.Close()
}

// Summary renders the operator-facing lines of a report: counts, ids, classes
// and digests only (the same secret-free content as the JSON report).
func (r *Report) Summary() []string {
	var out []string
	add := func(format string, a ...any) { out = append(out, fmt.Sprintf(format, a...)) }
	add("mode=%s result=%s source_timezone=%s same_database=%v", r.Mode, r.Result, r.Timezone, r.SameDatabase)
	add("source: users=%d invites=%d (used %d) legacy_api_tokens=%d | needs_reset=%d unvetted=%d verified=%d",
		r.Source.Users, r.Source.Invites, r.Source.UsedInvites, r.Source.Tokens, r.NeedsReset, r.Unvetted, r.Verified)
	if r.Mode != "verify" || r.Result != "" {
		add("source isolation evidence (informational only): other client backends=%d, database default read-only=%v",
			r.SourceOtherClients, r.SourceDBReadOnly)
	}
	if r.TargetState != "" {
		add("target before: state=%s users=%d invites=%d rss_tokens=%d api_tokens=%d sessions=%d one_time_tokens=%d",
			r.TargetState, r.Target.Users, r.Target.Invites, r.Target.Tokens, r.Target.APITokens, r.Target.Sessions, r.Target.OneTime)
	}
	for _, f := range r.Blocking {
		add("BLOCKING %s x%d ids=%v", f.Code, f.Count, f.IDs)
	}
	for _, f := range r.Warnings {
		add("warning %s x%d ids=%v", f.Code, f.Count, f.IDs)
	}
	for _, n := range r.Notes {
		add("note: %s", n)
	}
	if len(r.UnmatchedTargetUserIDs) > 0 {
		add("unmatched target user ids: %v", r.UnmatchedTargetUserIDs)
	}
	if d := r.Discarded; d != nil {
		add("WARNING --replace-users discards: users=%d invites=%d sessions=%d api_tokens=%d rss_tokens=%d (non-legacy %d) one_time_links=%d; "+
			"accounts differing from the source=%d admin_roles=%d disabled=%d verified_emails=%d invites_not_in_source=%d",
			d.Users, d.Invites, d.Sessions, d.APITokens, d.RSSTokens, d.NonLegacyRSSTokens, d.OneTimeLinks,
			d.DiffersFromSource, d.AdminRoles, d.DisabledAccounts, d.VerifiedEmails, d.InvitesNotInSource)
	}
	if len(r.WouldRefuse) > 0 {
		add("a final run would refuse: %s", strings.Join(r.WouldRefuse, ", "))
	}
	if r.MismatchCount > 0 {
		add("reconciliation mismatches: %d (first: %v)", r.MismatchCount, r.Mismatches)
	}
	if r.ImportedUsers+r.ImportedInvites+r.ImportedTokens > 0 {
		add("imported: users=%d invites=%d rss_tokens=%d", r.ImportedUsers, r.ImportedInvites, r.ImportedTokens)
	}
	if r.SourceDigest != "" {
		add("source digest: %s", r.SourceDigest)
	}
	if r.PlanDigest != "" {
		add("plan digest: %s", r.PlanDigest)
	}
	return out
}
