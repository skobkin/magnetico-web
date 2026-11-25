// Package data is the read-only data access of the administrator listings:
// quarantine, users, invitations and trackers. Every function takes the
// access.Principal the guard produced, selects an explicit column list (never
// SELECT *, never a credential column: no password hash, no invitation code,
// no token digest, no quarantine payload or info hash), is keyset-paginated and
// bounded by PageSize, and takes no input that is not parsed strictly.
package data

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/skobkin/magnetico-web/internal/opstats"
	"github.com/skobkin/magnetico-web/internal/web/admin/access"
)

// PageSize is the number of rows of a listing page; a page query fetches one
// more to learn whether there is a next page.
const PageSize = 50

// MaxSearch is the longest accepted search text, in characters.
const MaxSearch = 64

// ErrBadInput is returned for a search text that is too long or not valid UTF-8.
var ErrBadInput = errors.New("data: invalid input")

// Store reads through the web role's pool.
type Store struct{ db opstats.DB }

// New returns a Store over db.
func New(db opstats.DB) *Store { return &Store{db: db} }

// ---- quarantine ----------------------------------------------------------------

// QuarantineRow is a quarantined record's metadata. It cannot hold the payload
// or the info hash: those columns are never selected (and the web role has no
// grant on them).
type QuarantineRow struct {
	ID            int64
	Reason        string
	Detail        string // may be empty; untrusted text, escaped by the template
	Attempts      int32
	PayloadBytes  int32
	ReceivedAt    time.Time
	QuarantinedAt time.Time
}

// QuarantinePage is one page, newest first.
type QuarantinePage struct {
	Items []QuarantineRow
	Next  int64 // the id to continue after; 0 when there is no next page
}

// Quarantine lists quarantined records with id < after (after <= 0: the newest).
func (s *Store) Quarantine(ctx context.Context, p access.Principal, after int64) (QuarantinePage, error) {
	if err := p.Check(); err != nil {
		return QuarantinePage{}, err
	}
	if after <= 0 {
		after = 1<<62 - 1
	}
	rows, err := s.db.Query(ctx, `SELECT id, reason_code, COALESCE(detail, ''), attempts, payload_bytes, received_at, quarantined_at
FROM magnetico_ingest.quarantine WHERE id < $1 ORDER BY id DESC LIMIT $2`, after, PageSize+1)
	if err != nil {
		return QuarantinePage{}, err
	}
	defer rows.Close()
	var pg QuarantinePage
	for rows.Next() {
		var r QuarantineRow
		if err := rows.Scan(&r.ID, &r.Reason, &r.Detail, &r.Attempts, &r.PayloadBytes, &r.ReceivedAt, &r.QuarantinedAt); err != nil {
			return QuarantinePage{}, err
		}
		r.Detail = clip(r.Detail, 200)
		pg.Items = append(pg.Items, r)
	}
	if err := rows.Err(); err != nil {
		return QuarantinePage{}, err
	}
	if len(pg.Items) > PageSize {
		pg.Items = pg.Items[:PageSize]
		pg.Next = pg.Items[PageSize-1].ID
	}
	return pg, nil
}

// clip shortens an untrusted string to n runes.
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

// ---- users -----------------------------------------------------------------------

// UserRow is an account without any credential: no password hash, no session,
// no token.
type UserRow struct {
	ID              int64
	Username        string
	Email           string
	EmailVerifiedAt *time.Time
	Active          bool
	Role            string
	CreatedAt       time.Time
	LastLoginAt     *time.Time
	InvitedBy       string // the owner of the invitation this account used; empty if none
}

// UsersPage is one page of accounts, ordered by id.
type UsersPage struct {
	Items []UserRow
	Next  int64
}

// escapeLike makes s a literal inside a LIKE pattern.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// ValidSearch reports whether q is an acceptable search text.
func ValidSearch(q string) error {
	if utf8.RuneCountInString(q) > MaxSearch || !utf8.ValidString(q) || strings.ContainsRune(q, 0) {
		return ErrBadInput
	}
	return nil
}

// Users lists accounts with id > after whose username or email contains q
// (case-insensitively, wildcards in q are literal); q == "" lists all.
func (s *Store) Users(ctx context.Context, p access.Principal, q string, after int64) (UsersPage, error) {
	if err := p.Check(); err != nil {
		return UsersPage{}, err
	}
	if err := ValidSearch(q); err != nil {
		return UsersPage{}, err
	}
	pattern := "%" + escapeLike(q) + "%"
	rows, err := s.db.Query(ctx, `SELECT u.id, u.username, COALESCE(u.email, ''), u.email_verified_at, u.active, u.role, u.created_at, u.last_login_at,
       COALESCE((SELECT o.username FROM magnetico_web.invites i JOIN magnetico_web.users o ON o.id = i.owner_id WHERE i.used_by = u.id ORDER BY i.id LIMIT 1), '')
FROM magnetico_web.users u
WHERE u.id > $1 AND ($2 = '%%' OR u.username ILIKE $2 ESCAPE '\' OR COALESCE(u.email, '') ILIKE $2 ESCAPE '\')
ORDER BY u.id LIMIT $3`, max(after, 0), pattern, PageSize+1)
	if err != nil {
		return UsersPage{}, err
	}
	defer rows.Close()
	var pg UsersPage
	for rows.Next() {
		var u UserRow
		if err := rows.Scan(&u.ID, &u.Username, &u.Email, &u.EmailVerifiedAt, &u.Active, &u.Role, &u.CreatedAt, &u.LastLoginAt, &u.InvitedBy); err != nil {
			return UsersPage{}, err
		}
		pg.Items = append(pg.Items, u)
	}
	if err := rows.Err(); err != nil {
		return UsersPage{}, err
	}
	if len(pg.Items) > PageSize {
		pg.Items = pg.Items[:PageSize]
		pg.Next = pg.Items[PageSize-1].ID
	}
	return pg, nil
}

// ---- invitations --------------------------------------------------------------------

// InvitationRow is an invitation WITHOUT its code: a code is a credential that
// lets anyone register, so no administrator page ever shows one.
type InvitationRow struct {
	ID        int64
	Owner     string
	UsedBy    string // empty while unused
	UsedAt    *time.Time
	CreatedAt time.Time
	ExpiresAt *time.Time
	RevokedAt *time.Time
	Status    string // used, revoked, expired or unused
}

// InvitationsPage is one page, newest first.
type InvitationsPage struct {
	Items []InvitationRow
	Next  int64
}

// Invitations lists invitations with id < after (after <= 0: the newest).
func (s *Store) Invitations(ctx context.Context, p access.Principal, after int64) (InvitationsPage, error) {
	if err := p.Check(); err != nil {
		return InvitationsPage{}, err
	}
	if after <= 0 {
		after = 1<<62 - 1
	}
	rows, err := s.db.Query(ctx, `SELECT i.id, o.username, COALESCE(u.username, ''), i.used_at, i.created_at, i.expires_at, i.revoked_at,
       CASE WHEN i.used_by IS NOT NULL THEN 'used' WHEN i.revoked_at IS NOT NULL THEN 'revoked'
            WHEN i.expires_at IS NOT NULL AND i.expires_at <= now() THEN 'expired' ELSE 'unused' END
FROM magnetico_web.invites i JOIN magnetico_web.users o ON o.id = i.owner_id LEFT JOIN magnetico_web.users u ON u.id = i.used_by
WHERE i.id < $1 ORDER BY i.id DESC LIMIT $2`, after, PageSize+1)
	if err != nil {
		return InvitationsPage{}, err
	}
	defer rows.Close()
	var pg InvitationsPage
	for rows.Next() {
		var r InvitationRow
		if err := rows.Scan(&r.ID, &r.Owner, &r.UsedBy, &r.UsedAt, &r.CreatedAt, &r.ExpiresAt, &r.RevokedAt, &r.Status); err != nil {
			return InvitationsPage{}, err
		}
		pg.Items = append(pg.Items, r)
	}
	if err := rows.Err(); err != nil {
		return InvitationsPage{}, err
	}
	if len(pg.Items) > PageSize {
		pg.Items = pg.Items[:PageSize]
		pg.Next = pg.Items[PageSize-1].ID
	}
	return pg, nil
}

// ---- trackers -----------------------------------------------------------------------

// TrackerRow is one entry of the public tracker list.
type TrackerRow struct {
	Position int32
	URL      string
}

// MaxTrackers bounds the tracker listing (the public list is a few dozen entries).
const MaxTrackers = 500

// Trackers lists the public tracker list in order.
func (s *Store) Trackers(ctx context.Context, p access.Principal) ([]TrackerRow, error) {
	if err := p.Check(); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `SELECT position, url FROM magnetico_web.trackers ORDER BY position, id LIMIT $1`, MaxTrackers)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TrackerRow
	for rows.Next() {
		var t TrackerRow
		if err := rows.Scan(&t.Position, &t.URL); err != nil {
			return nil, err
		}
		t.URL = clip(t.URL, 300)
		out = append(out, t)
	}
	return out, rows.Err()
}
