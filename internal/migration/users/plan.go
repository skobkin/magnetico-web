package users

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/skobkin/magnetico-web/internal/web/accounts"
)

// Expected is what a correct import leaves in the target, derived from the
// snapshot and the stated source time zone.
type Expected struct {
	Users   []ExpUser
	Invites []ExpInvite
	Tokens  []ExpToken // deterministic order: created_at, owner, digest
}

type ExpUser struct {
	ID                    int64
	Username, Email, Hash string
	CreatedAt             time.Time
}

type ExpInvite struct {
	ID, OwnerID int64
	Code        string
	UsedBy      *int64
	CreatedAt   time.Time // the owner's converted created_at (web-v1 §5.3)
}

type ExpToken struct {
	Digest    []byte
	UserID    int64
	CreatedAt time.Time
}

// zoned applies the operator-stated zone to a legacy wall-clock value.
func zoned(t time.Time, loc *time.Location) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), loc)
}

// BuildExpected converts a validated snapshot. It must only be called when
// Analyze produced no blocking finding.
func BuildExpected(s *Snapshot, loc *time.Location) *Expected {
	e := &Expected{}
	created := map[int64]time.Time{}
	for _, u := range s.Users {
		c := zoned(u.CreatedAt, loc)
		created[u.ID] = c
		e.Users = append(e.Users, ExpUser{ID: u.ID, Username: u.Username, Email: u.Email, Hash: u.Password, CreatedAt: c})
	}
	for _, v := range s.Invites {
		e.Invites = append(e.Invites, ExpInvite{ID: v.ID, OwnerID: v.UserID, Code: v.Code, UsedBy: v.UsedByID, CreatedAt: created[v.UserID]})
	}
	for _, t := range s.Tokens {
		e.Tokens = append(e.Tokens, ExpToken{Digest: accounts.TokenDigest(t.Key), UserID: t.UserID, CreatedAt: zoned(t.CreatedAt, loc)})
	}
	sort.SliceStable(e.Tokens, func(i, j int) bool {
		a, b := e.Tokens[i], e.Tokens[j]
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		if a.UserID != b.UserID {
			return a.UserID < b.UserID
		}
		return bytes.Compare(a.Digest, b.Digest) < 0
	})
	return e
}

// ---- target state ----

type tgtUser struct {
	ID                             int64
	Username                       string
	Email, Hash                    *string
	Verified, PwChanged, LastLogin *time.Time
	Active                         bool
	Role                           string
	CreatedAt                      time.Time
}

type tgtInvite struct {
	ID, OwnerID              int64
	Code                     string
	UsedBy                   *int64
	UsedAt, Expires, Revoked *time.Time
	CreatedAt                time.Time
}

type tgtRSS struct {
	ID                         int64
	Digest                     []byte
	UserID                     int64
	Label                      *string
	CreatedAt                  time.Time
	LastUsed, Expires, Revoked *time.Time
	Legacy                     bool
}

// TargetState is a read of every user-owned table plus the identity state.
type TargetState struct {
	Users   []tgtUser
	Invites []tgtInvite
	RSS     []tgtRSS
	API     int
	Session int
	OneTime int
	// Next is the next value each identity would hand out, read from the
	// sequence relation (never via nextval()).
	Next map[string]int64
	// RowDigest covers every row of the six tables (full row text, ordered);
	// it is the "row digest of everything that would be deleted".
	RowDigest string
}

// userTables lists the six user-owned tables in deletion (FK) order.
var userTables = []string{"sessions", "one_time_tokens", "api_tokens", "rss_tokens", "invites", "users"}

type queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (t *TargetState) Counts() Counts {
	c := Counts{Users: len(t.Users), Invites: len(t.Invites), Tokens: len(t.RSS), APITokens: t.API, Sessions: t.Session, OneTime: t.OneTime}
	for _, v := range t.Invites {
		if v.UsedBy != nil {
			c.UsedInvites++
		}
	}
	for _, r := range t.RSS {
		if r.Legacy {
			c.LegacyTokens++
		}
	}
	return c
}

func (t *TargetState) empty() bool {
	return len(t.Users)+len(t.Invites)+len(t.RSS)+t.API+t.Session+t.OneTime == 0
}

func readTarget(ctx context.Context, q queryer) (*TargetState, *Error) {
	ts := &TargetState{Next: map[string]int64{}}
	fail := func(op string, err error) (*TargetState, *Error) { return nil, storageErr(op, err) }

	rows, err := q.Query(ctx, `SELECT id, username, email, email_verified_at, password_hash, active, role, created_at,
        password_changed_at, last_login_at FROM magnetico_web.users ORDER BY id`)
	if err != nil {
		return fail("read target users", err)
	}
	for rows.Next() {
		var u tgtUser
		if err := rows.Scan(&u.ID, &u.Username, &u.Email, &u.Verified, &u.Hash, &u.Active, &u.Role, &u.CreatedAt, &u.PwChanged, &u.LastLogin); err != nil {
			rows.Close()
			return fail("read target users", err)
		}
		ts.Users = append(ts.Users, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fail("read target users", err)
	}

	rows, err = q.Query(ctx, `SELECT id, owner_id, code, used_by, used_at, created_at, expires_at, revoked_at FROM magnetico_web.invites ORDER BY id`)
	if err != nil {
		return fail("read target invites", err)
	}
	for rows.Next() {
		var v tgtInvite
		if err := rows.Scan(&v.ID, &v.OwnerID, &v.Code, &v.UsedBy, &v.UsedAt, &v.CreatedAt, &v.Expires, &v.Revoked); err != nil {
			rows.Close()
			return fail("read target invites", err)
		}
		ts.Invites = append(ts.Invites, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fail("read target invites", err)
	}

	rows, err = q.Query(ctx, `SELECT id, token_hash, user_id, label, created_at, last_used_at, expires_at, revoked_at, legacy
        FROM magnetico_web.rss_tokens ORDER BY id`)
	if err != nil {
		return fail("read target rss_tokens", err)
	}
	for rows.Next() {
		var r tgtRSS
		if err := rows.Scan(&r.ID, &r.Digest, &r.UserID, &r.Label, &r.CreatedAt, &r.LastUsed, &r.Expires, &r.Revoked, &r.Legacy); err != nil {
			rows.Close()
			return fail("read target rss_tokens", err)
		}
		ts.RSS = append(ts.RSS, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fail("read target rss_tokens", err)
	}

	for tbl, dst := range map[string]*int{"api_tokens": &ts.API, "sessions": &ts.Session, "one_time_tokens": &ts.OneTime} {
		if err := q.QueryRow(ctx, `SELECT count(*) FROM magnetico_web.`+tbl).Scan(dst); err != nil {
			return fail("count target "+tbl, err)
		}
	}

	for _, tbl := range []string{"users", "invites", "rss_tokens"} {
		var seq string
		if err := q.QueryRow(ctx, `SELECT pg_get_serial_sequence($1, 'id')`, "magnetico_web."+tbl).Scan(&seq); err != nil || seq == "" {
			if err == nil {
				err = fmt.Errorf("no identity sequence")
			}
			return fail("read identity of "+tbl, err)
		}
		var last int64
		var called bool
		// seq is a catalog-produced, quoted, qualified name.
		if err := q.QueryRow(ctx, `SELECT last_value, is_called FROM `+seq).Scan(&last, &called); err != nil {
			return fail("read identity of "+tbl, err)
		}
		if called {
			last++
		}
		ts.Next[tbl] = last
	}

	h := newHasher("magnetico-users-target-rows-v1")
	for _, spec := range []struct{ tbl, order string }{
		{"users", "id"}, {"invites", "id"}, {"rss_tokens", "id"}, {"api_tokens", "id"},
		{"sessions", "token_hash"}, {"one_time_tokens", "token_hash"},
	} {
		rows, err := q.Query(ctx, `SELECT t::text FROM magnetico_web.`+spec.tbl+` t ORDER BY `+spec.order)
		if err != nil {
			return fail("digest target "+spec.tbl, err)
		}
		h.str(spec.tbl)
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				rows.Close()
				return fail("digest target "+spec.tbl, err)
			}
			h.str(s)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fail("digest target "+spec.tbl, err)
		}
	}
	ts.RowDigest = h.sum()
	return ts, nil
}

// ---- reconciliation ----

const maxMismatches = 200

type mismatches struct {
	list  []string
	total int
}

func (m *mismatches) add(code string, id int64) {
	m.total++
	if len(m.list) < maxMismatches {
		if id != 0 {
			m.list = append(m.list, fmt.Sprintf("%s:%d", code, id))
		} else {
			m.list = append(m.list, code)
		}
	}
}

func eqStrPtr(p *string, s string) bool { return p != nil && *p == s }

// userDiffers names the first field in which a target account differs from
// the expected import ("" when equal).
func userDiffers(u tgtUser, e ExpUser) string {
	switch {
	case u.Username != e.Username:
		return "user-username"
	case !eqStrPtr(u.Email, e.Email):
		return "user-email"
	case !eqStrPtr(u.Hash, e.Hash):
		return "user-password-hash"
	case !u.CreatedAt.Equal(e.CreatedAt):
		return "user-created-at"
	case u.Verified != nil:
		return "user-email-verified"
	case !u.Active:
		return "user-inactive"
	case u.Role != "user":
		return "user-role"
	case u.PwChanged != nil || u.LastLogin != nil:
		return "user-activity-set"
	}
	return ""
}

// reconcile compares the target with what a correct import leaves. Records
// are identified by id and finding code only.
func reconcile(ts *TargetState, exp *Expected) *mismatches {
	m := &mismatches{}
	if len(ts.Users) != len(exp.Users) {
		m.add("count-users", 0)
	}
	if len(ts.Invites) != len(exp.Invites) {
		m.add("count-invites", 0)
	}
	if len(ts.RSS) != len(exp.Tokens) {
		m.add("count-rss-tokens", 0)
	}
	if ts.API != 0 {
		m.add("api-tokens-present", 0)
	}
	if ts.Session != 0 {
		m.add("sessions-present", 0)
	}
	if ts.OneTime != 0 {
		m.add("one-time-tokens-present", 0)
	}

	tu := map[int64]tgtUser{}
	for _, u := range ts.Users {
		tu[u.ID] = u
	}
	seen := map[int64]bool{}
	for _, e := range exp.Users {
		u, ok := tu[e.ID]
		if !ok {
			m.add("user-missing", e.ID)
			continue
		}
		seen[e.ID] = true
		if code := userDiffers(u, e); code != "" {
			m.add(code, e.ID)
		}
	}
	for _, u := range ts.Users {
		if !seen[u.ID] {
			m.add("user-unexpected", u.ID)
		}
	}

	ti := map[int64]tgtInvite{}
	for _, v := range ts.Invites {
		ti[v.ID] = v
	}
	seen = map[int64]bool{}
	for _, e := range exp.Invites {
		v, ok := ti[e.ID]
		if !ok {
			m.add("invite-missing", e.ID)
			continue
		}
		seen[e.ID] = true
		usedOK := (v.UsedBy == nil) == (e.UsedBy == nil) && (v.UsedBy == nil || *v.UsedBy == *e.UsedBy)
		switch {
		case v.OwnerID != e.OwnerID:
			m.add("invite-owner", e.ID)
		case v.Code != e.Code:
			m.add("invite-code", e.ID)
		case !usedOK:
			m.add("invite-used-by", e.ID)
		case !v.CreatedAt.Equal(e.CreatedAt):
			m.add("invite-created-at", e.ID)
		case v.UsedAt != nil || v.Expires != nil || v.Revoked != nil:
			m.add("invite-state-set", e.ID)
		}
	}
	for _, v := range ts.Invites {
		if !seen[v.ID] {
			m.add("invite-unexpected", v.ID)
		}
	}

	byDigest := map[string]tgtRSS{}
	for _, r := range ts.RSS {
		byDigest[string(r.Digest)] = r
	}
	seenTok := map[string]bool{}
	for _, e := range exp.Tokens {
		r, ok := byDigest[string(e.Digest)]
		if !ok {
			m.add("rss-token-missing-for-user", e.UserID)
			continue
		}
		seenTok[string(e.Digest)] = true
		switch {
		case r.UserID != e.UserID:
			m.add("rss-token-owner", e.UserID)
		case !r.CreatedAt.Equal(e.CreatedAt):
			m.add("rss-token-created-at", e.UserID)
		case !r.Legacy:
			m.add("rss-token-not-legacy", r.ID)
		case r.Label != nil || r.LastUsed != nil || r.Expires != nil || r.Revoked != nil:
			m.add("rss-token-state-set", r.ID)
		}
	}
	for _, r := range ts.RSS {
		if !seenTok[string(r.Digest)] {
			m.add("rss-token-unexpected", r.ID)
		}
	}
	return m
}

// referential checks that the foreign keys of the imported rows close; the
// database enforces them too, this is the explicit reconciliation anti-join.
func referential(ts *TargetState) *mismatches {
	m := &mismatches{}
	ids := map[int64]bool{}
	for _, u := range ts.Users {
		ids[u.ID] = true
	}
	for _, v := range ts.Invites {
		if !ids[v.OwnerID] {
			m.add("fk-invite-owner", v.ID)
		}
		if v.UsedBy != nil && !ids[*v.UsedBy] {
			m.add("fk-invite-used-by", v.ID)
		}
	}
	for _, r := range ts.RSS {
		if !ids[r.UserID] {
			m.add("fk-rss-token-owner", r.ID)
		}
	}
	return m
}

// sequencesAhead verifies that every identity's next value exceeds the
// largest id in its table.
func sequencesAhead(ts *TargetState) *mismatches {
	m := &mismatches{}
	var maxU, maxI, maxR int64
	for _, u := range ts.Users {
		maxU = max(maxU, u.ID)
	}
	for _, v := range ts.Invites {
		maxI = max(maxI, v.ID)
	}
	for _, r := range ts.RSS {
		maxR = max(maxR, r.ID)
	}
	if ts.Next["users"] <= maxU {
		m.add("sequence-users", 0)
	}
	if ts.Next["invites"] <= maxI {
		m.add("sequence-invites", 0)
	}
	if ts.Next["rss_tokens"] <= maxR {
		m.add("sequence-rss-tokens", 0)
	}
	return m
}

// Class is the classification of a target state against the expected import.
type Class string

const (
	ClassEmpty     Class = "empty"
	ClassIdentical Class = "identical"
	ClassReplace   Class = "replace"  // diverged, --replace-users requested
	ClassDiverged  Class = "diverged" // diverged, refused without --replace-users
)

func classify(ts *TargetState, exp *Expected, replace bool) Class {
	switch {
	case ts.empty():
		return ClassEmpty
	case reconcile(ts, exp).total == 0:
		return ClassIdentical
	case replace:
		return ClassReplace
	}
	return ClassDiverged
}
