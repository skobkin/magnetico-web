package forensics

// Registry-state digests (owner review finding 2): a deterministic SHA-256
// per registry content table plus a combined digest, streamed from the same
// keyset reads the export uses (they accept state.Queryer, so they also run
// unchanged INSIDE the cleanup drop transaction for the stale-evidence
// recheck). Every column of every row feeds a canonical length-prefixed
// serialization — so ANY mutation (a `running -> completed_pass` transition,
// a retry terminal state, an updated_at-only touch) changes the digest even
// when data_revision and row counts do not.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"strconv"
	"time"

	"github.com/skobkin/magnetico-web/internal/migration/state"
)

// RegistryDigest carries one SHA-256 (lowercase hex) per table and the
// combined digest. The four 04B tables render as "unavailable" under v1.
type RegistryDigest struct {
	Progress           string `json:"progress"`
	Errors             string `json:"errors"`
	VerificationRuns   string `json:"verification_runs"`
	VerificationIssues string `json:"verification_issues"`
	RetryRuns          string `json:"retry_runs"`
	RetryAttempts      string `json:"retry_attempts"`
	Combined           string `json:"combined"`
}

const digestUnavailable = "unavailable"

// canonical is the length-prefixed field serializer: a 4-byte big-endian
// length, the bytes, then ';' per field — unambiguous boundaries, no
// separator injection. Timestamps enter as RFC 3339 Nano UTC strings (the
// timeString form), bytea as lowercase hex, NULLs as the "\N" sentinel.
type canonical struct{ h hash.Hash }

func newCanonical() *canonical { return &canonical{h: sha256.New()} }

func (c *canonical) field(s string) {
	l := len(s)
	c.h.Write([]byte{byte(l >> 24), byte(l >> 16), byte(l >> 8), byte(l)})
	c.h.Write([]byte(s))
	c.h.Write([]byte{';'})
}

func (c *canonical) intF(v int64) { c.field(formatInt64(v)) }
func (c *canonical) boolF(b bool) { c.field(boolStr(b)) }
func (c *canonical) nilF()        { c.field("\\N") }
func (c *canonical) optInt(v *int64) {
	if v == nil {
		c.nilF()
	} else {
		c.intF(*v)
	}
}
func (c *canonical) rawF(raw jsonRawBytes) {
	if raw == nil {
		c.nilF()
	} else {
		c.field(string(raw))
	}
}
func (c *canonical) bytesF(b []byte) {
	if b == nil {
		c.nilF()
	} else {
		c.field(hex.EncodeToString(b))
	}
}
func (c *canonical) optTimeString(t *time.Time) {
	if t == nil {
		c.nilF()
	} else {
		c.field(timeString(*t))
	}
}
func (c *canonical) hex() string { return hex.EncodeToString(c.h.Sum(nil)) }

type jsonRawBytes = []byte

func formatInt64(v int64) string { return strconv.FormatInt(v, 10) }

func boolStr(b bool) string {
	if b {
		return "t"
	}
	return "f"
}

// ComputeRegistryDigest streams the full registry of runID through the
// canonical serializer. class must be SchemaV1 or SchemaV2. q may be a pool,
// a connection or (for the in-drop-transaction stale-evidence recheck) the
// drop transaction itself.
func ComputeRegistryDigest(ctx context.Context, q state.Queryer, runID string, class state.SchemaClass) (*RegistryDigest, error) {
	d := &RegistryDigest{}

	prog, err := state.ReadProgressRaw(ctx, q, class)
	if err != nil {
		return nil, err
	}
	d.Progress = digestProgress(prog)

	if d.Errors, err = digestErrors(ctx, q, runID); err != nil {
		return nil, err
	}

	if class == state.SchemaV1 {
		d.VerificationRuns = digestUnavailable
		d.VerificationIssues = digestUnavailable
		d.RetryRuns = digestUnavailable
		d.RetryAttempts = digestUnavailable
		d.Combined = combineDigest(d)
		return d, nil
	}

	if d.VerificationRuns, err = digestVerificationRuns(ctx, q, runID); err != nil {
		return nil, err
	}
	if d.VerificationIssues, err = digestVerificationIssues(ctx, q); err != nil {
		return nil, err
	}
	if d.RetryRuns, err = digestRetryRuns(ctx, q, runID); err != nil {
		return nil, err
	}
	if d.RetryAttempts, err = digestRetryAttempts(ctx, q); err != nil {
		return nil, err
	}
	d.Combined = combineDigest(d)
	return d, nil
}

// combineDigest folds the per-table digests in a fixed order.
func combineDigest(d *RegistryDigest) string {
	c := newCanonical()
	c.field("progress=" + d.Progress)
	c.field("errors=" + d.Errors)
	c.field("verification_runs=" + d.VerificationRuns)
	c.field("verification_issues=" + d.VerificationIssues)
	c.field("retry_runs=" + d.RetryRuns)
	c.field("retry_attempts=" + d.RetryAttempts)
	return c.hex()
}

func digestProgress(p *state.Progress) string {
	c := newCanonical()
	c.field("progress")
	c.field(p.RunID)
	c.field(string(p.Phase))
	c.rawF(p.FingerprintRaw)
	c.field(p.SourceAdapter)
	c.intF(int64(p.ContractVersion))
	c.field(p.CodecProfile)
	c.intF(p.ExpectedTotal)
	c.intF(p.SourceMinID)
	c.intF(p.SourceMaxID)
	c.intF(p.SourceFileRows)
	c.intF(p.LastProcessedID)
	c.intF(p.ScannedCount)
	c.intF(p.SuccessCount)
	c.intF(p.ErrorCount)
	c.intF(p.RunErrorCount)
	c.intF(p.BlobCount)
	c.intF(p.DataRevision)
	c.boolF(p.DeferIndexes)
	c.boolF(p.IndexesDeferred)
	c.rawF(p.OptionsRaw)
	c.field(timeString(p.StartedAt))
	c.field(timeString(p.UpdatedAt))
	return c.hex()
}

func digestErrors(ctx context.Context, q state.Queryer, runID string) (string, error) {
	c := newCanonical()
	after := int64(0)
	for {
		rows, err := state.ListErrorRows(ctx, q, runID, after, digestPage)
		if err != nil {
			return "", err
		}
		for _, r := range rows {
			c.field("error")
			c.intF(r.ID)
			c.field(r.Scope)
			c.optInt(r.SourceID)
			c.bytesF(r.SourceInfoHash)
			c.field(r.Stage)
			c.field(r.Category)
			c.field(r.Code)
			c.rawF(r.Details)
			c.intF(int64(r.Attempts))
			c.boolF(r.Resolved)
			c.optTimeString(r.ResolvedAt)
			c.field(timeString(r.CreatedAt))
			after = r.ID
		}
		if len(rows) < digestPage {
			return c.hex(), nil
		}
	}
}

func digestVerificationRuns(ctx context.Context, q state.Queryer, runID string) (string, error) {
	c := newCanonical()
	after := int64(0)
	for {
		runs, err := state.ListVerificationRuns(ctx, q, runID, after, digestPage)
		if err != nil {
			return "", err
		}
		for _, r := range runs {
			c.field("verification_run")
			c.intF(r.Seq)
			c.field(r.VerificationID)
			c.field(r.Kind)
			c.field(r.State)
			c.field(r.Phase)
			c.intF(r.StartDataRevision)
			c.rawF(r.SourceIdentityRaw)
			c.rawF(r.SourceFingerprintRaw)
			c.optInt(r.SourceCheckpoint)
			c.optInt(r.TargetCheckpoint)
			c.optInt(r.ExpectedSourceIDs)
			c.optInt(r.ObservedSourceIDs)
			c.optInt(r.ExpectedTargetIDs)
			c.optInt(r.ObservedTargetIDs)
			c.intF(r.VerifiedCount)
			c.intF(r.MismatchCount)
			c.intF(r.UnverifiableCount)
			c.intF(r.AssumedSingleCount)
			c.intF(r.IssueCount)
			c.rawF(r.SettingsRaw)
			c.field(timeString(r.StartedAt))
			c.field(timeString(r.UpdatedAt))
			c.optTimeString(r.CompletedAt)
			after = r.Seq
		}
		if len(runs) < digestPage {
			return c.hex(), nil
		}
	}
}

func digestVerificationIssues(ctx context.Context, q state.Queryer) (string, error) {
	c := newCanonical()
	after := int64(0)
	for {
		rows, err := state.ListVerificationIssuesAfter(ctx, q, after, digestPage)
		if err != nil {
			return "", err
		}
		for _, r := range rows {
			c.field("verification_issue")
			c.intF(r.ID)
			c.field(r.VerificationID)
			c.optInt(r.SourceID)
			c.bytesF(r.InfoHash)
			c.field(r.Code)
			c.field(r.Stage)
			c.field(r.Severity)
			c.rawF(r.Details)
			c.field(timeString(r.CreatedAt))
			after = r.ID
		}
		if len(rows) < digestPage {
			return c.hex(), nil
		}
	}
}

func digestRetryRuns(ctx context.Context, q state.Queryer, runID string) (string, error) {
	c := newCanonical()
	// retry_runs orders by (started_at, retry_id); the first page starts at
	// the smallest possible cursor (zero time + empty id → the reader
	// substitutes the zero UUID internally).
	var afterStartedAt time.Time
	afterRetryID := ""
	for {
		runs, err := state.ListRetryRuns(ctx, q, runID, afterStartedAt, afterRetryID, digestPage)
		if err != nil {
			return "", err
		}
		for _, r := range runs {
			c.field("retry_run")
			c.field(r.RetryID)
			c.field(r.State)
			c.intF(r.StartDataRevision)
			c.intF(r.CurrentDataRevision)
			c.intF(r.UnresolvedAtStart)
			c.optInt(r.LastSourceID)
			c.intF(r.AttemptedCount)
			c.intF(r.FixedCount)
			c.intF(r.StillInvalidCount)
			c.intF(r.MissingCount)
			c.rawF(r.SourceFingerprintRaw)
			c.boolF(r.AcceptedSourceRepair)
			c.rawF(r.PreviousFingerprintRaw)
			c.rawF(r.SettingsRaw)
			c.field(timeString(r.StartedAt))
			c.field(timeString(r.UpdatedAt))
			c.optTimeString(r.CompletedAt)
			afterStartedAt = r.StartedAt
			afterRetryID = r.RetryID
		}
		if len(runs) < digestPage {
			return c.hex(), nil
		}
	}
}

func digestRetryAttempts(ctx context.Context, q state.Queryer) (string, error) {
	c := newCanonical()
	after := int64(0)
	for {
		rows, err := state.ListRetryAttemptsAfter(ctx, q, after, digestPage)
		if err != nil {
			return "", err
		}
		for _, r := range rows {
			c.field("retry_attempt")
			c.intF(r.ID)
			c.field(r.RetryID)
			c.intF(r.SourceID)
			c.bytesF(r.SourceInfoHash)
			c.field(r.Outcome)
			c.field(r.Stage)
			c.field(r.Code)
			c.rawF(r.Details)
			c.field(timeString(r.AttemptedAt))
			after = r.ID
		}
		if len(rows) < digestPage {
			return c.hex(), nil
		}
	}
}

const digestPage = 5000
