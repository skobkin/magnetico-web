package users

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/skobkin/magnetico-web/internal/web/accounts"
	"github.com/skobkin/magnetico-web/internal/web/password"
)

// VettedFamilies are the exact legacy hash parameter sets that have
// independently generated PHP vectors (internal/web/password/testdata/php).
// A test asserts that this list equals the families in fixtures.json.
// Anything the verifier parses but that is not listed is "unvetted": login
// would work, but production compatibility is not approved (plan D10).
var VettedFamilies = []string{
	"argon2i/v=19/m=16384,t=2,p=4",
	"argon2id/v=19/m=65536,t=4,p=1",
	"argon2id/v=19/m=65536,t=2,p=1",
	"argon2id/v=19/m=65536,t=3,p=2",
	"2y/cost=10",
	"2y/cost=13",
}

// HashFamily reduces a stored hash to its format and parameters (never the
// salt or digest): "argon2id/v=19/m=…,t=…,p=…" or "2y/cost=NN".
func HashFamily(hash string) (string, bool) {
	switch {
	case strings.HasPrefix(hash, "$argon2"):
		parts := strings.Split(hash, "$")
		if len(parts) != 6 {
			return "", false
		}
		return parts[1] + "/" + parts[2] + "/" + parts[3], true
	case strings.HasPrefix(hash, "$2y$") && len(hash) >= 7:
		return "2y/cost=" + hash[4:6], true
	}
	return "", false
}

// HashCategory is the three-way password compatibility classification of
// plan §0.4.
type HashCategory int

const (
	HashVerified    HashCategory = iota // (a) exact PHP vectors exist
	HashUnvetted                        // (b) parsed by the verifier, no exact vectors
	HashUnsupported                     // (c) needs-reset
	HashEmpty                           // not a hash at all: blocks the migration
)

// CategorizeHash classifies a stored legacy hash. It uses password.Classify,
// the same classifier the login path uses.
func CategorizeHash(hash string) HashCategory {
	switch password.Classify(hash) {
	case password.ClassNone:
		return HashEmpty
	case password.ClassUnsupported:
		return HashUnsupported
	}
	fam, ok := HashFamily(hash)
	if ok {
		for _, v := range VettedFamilies {
			if v == fam {
				return HashVerified
			}
		}
	}
	return HashUnvetted
}

var (
	inviteCodeRE = regexp.MustCompile(`^[A-Za-z0-9_-]{22,64}$`)
	legacyKeyRE  = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// Analysis is the outcome of validating a snapshot against the target
// contract: blocking findings stop the run, warnings are reported only.
type Analysis struct {
	Blocking, Warnings findingSet
	Verified           int
	Unvetted           int
	NeedsReset         int
	UnvettedIDs        []int64
	NeedsResetIDs      []int64
	UsedInvites        int
}

// Analyze validates every source row against the version-1 target
// constraints (replicated here so a dry-run needs no write access) and
// collects the data findings of plan §4.2. Records are identified by numeric
// id only.
func Analyze(s *Snapshot) *Analysis {
	a := &Analysis{Blocking: findingSet{}, Warnings: findingSet{}}
	if len(s.Users) == 0 {
		a.Blocking.add("source-empty", 0)
	}
	userIDs := map[int64]bool{}
	usernames := map[string]bool{}
	emailGroups := map[string][]int64{}
	for _, u := range s.Users {
		if userIDs[u.ID] {
			a.Blocking.add("user-id-duplicate", u.ID)
		}
		userIDs[u.ID] = true
		n := utf8.RuneCountInString(u.Username)
		if n < 1 || n > 25 {
			a.Blocking.add("username-length", u.ID)
		}
		if usernames[u.Username] {
			a.Blocking.add("username-duplicate", u.ID)
		}
		usernames[u.Username] = true
		if e := utf8.RuneCountInString(u.Email); e < 3 || e > 254 {
			a.Blocking.add("email-length", u.ID)
		}
		switch CategorizeHash(u.Password) {
		case HashEmpty:
			a.Blocking.add("password-empty", u.ID)
		case HashUnsupported:
			a.NeedsReset++
			a.NeedsResetIDs = append(a.NeedsResetIDs, u.ID)
		case HashUnvetted:
			a.Unvetted++
			a.UnvettedIDs = append(a.UnvettedIDs, u.ID)
		default:
			a.Verified++
		}
		if accounts.ValidateUsername(u.Username) != nil {
			a.Warnings.add("username-rules", u.ID)
		}
		if accounts.ValidateEmail(u.Email) != nil {
			a.Warnings.add("email-rules", u.ID)
		}
		k := strings.ToLower(u.Email)
		emailGroups[k] = append(emailGroups[k], u.ID)
		var roles []string
		if json.Unmarshal([]byte(u.Roles), &roles) != nil || len(roles) != 1 || roles[0] != "ROLE_USER" {
			a.Warnings.add("roles-not-plain-user", u.ID)
		}
	}
	for _, ids := range emailGroups {
		if len(ids) > 1 {
			for _, id := range ids {
				a.Warnings.add("email-case-duplicate", id)
			}
		}
	}

	inviteIDs := map[int64]bool{}
	codes := map[string]bool{}
	usedBy := map[int64]int{}
	for _, v := range s.Invites {
		if inviteIDs[v.ID] {
			a.Blocking.add("invite-id-duplicate", v.ID)
		}
		inviteIDs[v.ID] = true
		if !userIDs[v.UserID] {
			a.Blocking.add("invite-orphan-owner", v.ID)
		}
		if v.UsedByID != nil {
			a.UsedInvites++
			if !userIDs[*v.UsedByID] {
				a.Blocking.add("invite-orphan-used-by", v.ID)
			}
			usedBy[*v.UsedByID]++
			if *v.UsedByID == v.UserID {
				a.Warnings.add("invite-self-used", v.ID)
			}
		}
		if !inviteCodeRE.MatchString(v.Code) {
			a.Blocking.add("invite-code-format", v.ID)
		}
		if codes[v.Code] {
			a.Blocking.add("invite-code-duplicate", v.ID)
		}
		codes[v.Code] = true
	}
	for id, n := range usedBy {
		if n > 1 {
			a.Warnings.add("user-used-multiple-invites", id)
		}
	}

	keys := map[string]bool{}
	perUser := map[int64]int{}
	for _, t := range s.Tokens {
		if t.Key == "" {
			a.Blocking.add("token-key-empty", t.UserID)
		}
		if !userIDs[t.UserID] {
			a.Blocking.add("token-orphan-owner", t.UserID)
		}
		if keys[t.Key] {
			a.Blocking.add("token-key-duplicate", t.UserID)
		}
		keys[t.Key] = true
		if !legacyKeyRE.MatchString(t.Key) {
			a.Warnings.add("token-key-not-32-hex", t.UserID)
		}
		perUser[t.UserID]++
	}
	for id, n := range perUser {
		if n > accounts.MaxActiveTokens {
			a.Warnings.add("tokens-over-cap", id)
		}
	}
	return a
}

// ConstraintCoverage records, for every constraint and unique index of the
// three tables the import writes, how a dry-run (which writes nothing and so
// cannot meet the database constraint) knows the import cannot violate it:
// the Analyze finding code that replicates it, or why it cannot be reached.
// A test fails when the version-1 schema gains a constraint that is not
// listed here, so the replicas cannot silently fall behind the DDL.
var ConstraintCoverage = map[string]string{
	"users_pkey":                    "user-id-duplicate",
	"users_username_key":            "username-duplicate",
	"users_username_length":         "username-length",
	"users_role_check":              "constant: role is always 'user'",
	"users_email_length":            "email-length",
	"users_verified_needs_email":    "constant: email_verified_at is always NULL",
	"users_password_hash_nonempty":  "password-empty",
	"users_verified_email_key":      "constant: nothing is verified, so the partial unique index has no rows",
	"invites_pkey":                  "invite-id-duplicate",
	"invites_code_key":              "invite-code-duplicate",
	"invites_code_format":           "invite-code-format",
	"invites_used_at_needs_used_by": "constant: used_at is always NULL",
	"invites_owner_fkey":            "invite-orphan-owner",
	"invites_used_by_fkey":          "invite-orphan-used-by",
	"rss_tokens_pkey":               "ids are explicit and consecutive from the identity's next value",
	"rss_tokens_token_hash_key":     "token-key-duplicate",
	"rss_tokens_token_hash_len":     "constant: SHA-256 is 32 bytes",
	"rss_tokens_label_length":       "constant: label is always NULL",
	"rss_tokens_user_fkey":          "token-orphan-owner",
}
