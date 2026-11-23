package users

import (
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/skobkin/magnetico-web/internal/pgtest"
)

// ---- password compatibility categories ----

func TestVettedFamiliesEqualFixtures(t *testing.T) {
	got := map[string]bool{}
	for _, c := range fixtureCases(t) {
		fam, ok := HashFamily(c.Hash)
		if !ok {
			t.Fatalf("fixture %s has no recognisable family", c.ID)
		}
		got[fam] = true
		if CategorizeHash(c.Hash) != HashVerified {
			t.Errorf("fixture %s (%s) is not categorised as verified", c.ID, fam)
		}
	}
	var fix, vet []string
	for f := range got {
		fix = append(fix, f)
	}
	vet = append(vet, VettedFamilies...)
	sort.Strings(fix)
	sort.Strings(vet)
	if strings.Join(fix, "|") != strings.Join(vet, "|") {
		t.Fatalf("VettedFamilies %v differ from the PHP fixture families %v", vet, fix)
	}
}

func TestHashCategories(t *testing.T) {
	sodium := hashFor(t, famSodium)
	cases := []struct {
		name string
		hash string
		want HashCategory
	}{
		{"empty", "", HashEmpty},
		{"bcrypt cost 12 parses but has no vectors", "$2y$12$" + strings.Repeat("a", 53), HashUnvetted},
		{"argon2id other memory parses but has no vectors", strings.Replace(sodium, "m=65536,t=4,p=1", "m=32768,t=4,p=1", 1), HashUnvetted},
		{"argon2id at the current parameters has no PHP vectors either", strings.Replace(sodium, "m=65536,t=4,p=1", "m=65536,t=3,p=4", 1), HashUnvetted},
		{"bcrypt 2a", "$2a$10$" + strings.Repeat("a", 53), HashUnsupported},
		{"bcrypt 2b", "$2b$10$" + strings.Repeat("a", 53), HashUnsupported},
		{"argon2id above the memory bound", strings.Replace(sodium, "m=65536,t=4,p=1", "m=131072,t=4,p=1", 1), HashUnsupported},
		{"argon2id version 16", strings.Replace(sodium, "v=19", "v=16", 1), HashUnsupported},
		{"garbage", "not a hash", HashUnsupported},
		{"ssha", "{SSHA}abcdefghijklmnop", HashUnsupported},
	}
	for _, c := range cases {
		if got := CategorizeHash(c.hash); got != c.want {
			t.Errorf("%s: category %d, want %d", c.name, got, c.want)
		}
	}
}

// ---- analysis (unit level) ----

func codes(f findingSet) string {
	var out []string
	for c := range f {
		out = append(out, c)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func TestAnalyzeBlockingFindings(t *testing.T) {
	good := hashFor(t, famSodium)
	s := &Snapshot{
		Users: []SrcUser{
			{ID: 1, Username: "a", Password: good, Email: "a@example.org", Roles: `["ROLE_USER"]`},
			{ID: 1, Username: "a", Password: "", Email: "xy", Roles: `["ROLE_USER"]`},
			{ID: 2, Username: "", Password: good, Email: "b@example.org", Roles: `["ROLE_USER"]`},
		},
		Invites: []SrcInvite{
			{ID: 1, UserID: 99, Code: "short"},
			{ID: 1, UserID: 1, UsedByID: i64(98), Code: "short"},
		},
		Tokens: []SrcToken{{Key: "", UserID: 97}, {Key: "k", UserID: 1}, {Key: "k", UserID: 1}},
	}
	a := Analyze(s)
	want := "email-length,invite-code-duplicate,invite-code-format,invite-id-duplicate,invite-orphan-owner,invite-orphan-used-by," +
		"password-empty,token-key-duplicate,token-key-empty,token-orphan-owner,user-id-duplicate,username-duplicate,username-length"
	if got := codes(a.Blocking); got != want {
		t.Fatalf("blocking findings\n got %s\nwant %s", got, want)
	}
	if got := Analyze(&Snapshot{}); got.Blocking["source-empty"] == nil {
		t.Fatal("an empty source is not reported")
	}
}

func TestAnalyzeWarningsDoNotBlock(t *testing.T) {
	good := hashFor(t, famSodium)
	s := &Snapshot{Users: []SrcUser{
		{ID: 1, Username: "x", Password: good, Email: "Same@Example.org", Roles: `["ROLE_USER","ROLE_ADMIN"]`},
		{ID: 2, Username: " padded ", Password: good, Email: "same@example.org", Roles: `not json`},
		{ID: 3, Username: "ok-name", Password: good, Email: "no-at-sign", Roles: `["ROLE_USER"]`},
	}}
	s.Invites = []SrcInvite{
		{ID: 1, UserID: 1, UsedByID: i64(1), Code: "0123456789abcdef0123456789abcdef"},
		{ID: 2, UserID: 2, UsedByID: i64(3), Code: "1123456789abcdef0123456789abcdef"},
		{ID: 3, UserID: 2, UsedByID: i64(3), Code: "2123456789abcdef0123456789abcdef"},
	}
	for i := 0; i < 51; i++ {
		s.Tokens = append(s.Tokens, SrcToken{Key: "UPPER" + strings.Repeat("0", 27) + string(rune('a'+i%26)) + string(rune('a'+i/26)), UserID: 3})
	}
	a := Analyze(s)
	if len(a.Blocking) != 0 {
		t.Fatalf("unexpected blocking findings %s", codes(a.Blocking))
	}
	want := "email-case-duplicate,email-rules,invite-self-used,roles-not-plain-user,token-key-not-32-hex,tokens-over-cap,user-used-multiple-invites,username-rules"
	if got := codes(a.Warnings); got != want {
		t.Fatalf("warnings\n got %s\nwant %s", got, want)
	}
}

// ---- source fingerprint and snapshot (disposable PostgreSQL) ----

func TestSourceFingerprintAcceptsLegacySchema(t *testing.T) {
	src, d := seeded(t)
	s, err := openSource(tctx(t), src)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	a, e := s.snapshot(tctx(t))
	if e != nil {
		t.Fatal(e)
	}
	b, e := s.snapshot(tctx(t))
	if e != nil {
		t.Fatal(e)
	}
	if a.Digest != b.Digest || a.Digest == "" {
		t.Fatalf("snapshot digest is not deterministic: %q vs %q", a.Digest, b.Digest)
	}
	if len(a.Users) != len(d.Users) || len(a.Invites) != len(d.Invites) || len(a.Tokens) != len(d.Tokens) {
		t.Fatalf("snapshot counts %d/%d/%d", len(a.Users), len(a.Invites), len(a.Tokens))
	}
	if !strings.Contains(strings.Join(a.Notes, "\n"), "source-role-has-write-privileges") {
		t.Errorf("a superuser source role must be reported as holding write privileges: %v", a.Notes)
	}
}

func TestSourceFingerprintRefusals(t *testing.T) {
	cases := []struct {
		name string
		sql  string
	}{
		{"extra table", `CREATE TABLE users.extra (id int)`},
		{"extra column", `ALTER TABLE users.users ADD COLUMN extra int`},
		{"changed type", `ALTER TABLE users.users ALTER COLUMN username TYPE varchar(30)`},
		{"missing migration version", `DELETE FROM migration_versions WHERE version LIKE '%161732'`},
		{"unknown newer version", `INSERT INTO migration_versions (version) VALUES ('DoctrineMigrations\Version20260101000000')`},
		{"no doctrine table", `DROP TABLE migration_versions`},
		{"missing reset table", `DROP TABLE users.password_reset_tokens`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src, _ := seeded(t)
			execSQL(t, src, c.sql)
			cfg := cfgFor(src, "postgres://127.0.0.1:1/none")
			cfg.DryRun = true
			_, err := Run(tctx(t), cfg)
			wantClass(t, err, ClassSourceSchema)
		})
	}
	t.Run("no users schema", func(t *testing.T) {
		src := pgtest.IntegrationCluster(t).TestDatabase(t)
		cfg := cfgFor(src, "postgres://127.0.0.1:1/none")
		cfg.DryRun = true
		_, err := Run(tctx(t), cfg)
		wantClass(t, err, ClassSourceSchema)
	})
}

func TestSnapshotOrderIsIndependentOfInsertOrder(t *testing.T) {
	a, d := seeded(t)
	b := legacyDB(t)
	rev := d
	rev.Users = append([]sUser(nil), d.Users...)
	rev.Invites = append([]sInvite(nil), d.Invites...)
	rev.Tokens = append([]sToken(nil), d.Tokens...)
	// Invitations may be loaded in any order (the used_by users exist); tokens
	// in reverse.
	for i, j := 0, len(rev.Tokens)-1; i < j; i, j = i+1, j-1 {
		rev.Tokens[i], rev.Tokens[j] = rev.Tokens[j], rev.Tokens[i]
	}
	rev.load(t, b)
	snap := func(dsn string) *Snapshot {
		s, err := openSource(tctx(t), dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer s.close()
		x, e := s.snapshot(tctx(t))
		if e != nil {
			t.Fatal(e)
		}
		return x
	}
	if snap(a).Digest != snap(b).Digest {
		t.Fatal("digest depends on physical insertion order")
	}
}

// TestEveryTargetConstraintHasADryRunReplica fails when the version-1 schema
// gains a constraint or unique index on a table the import writes that
// ConstraintCoverage does not account for, and checks that every finding code
// it names is one Analyze can actually produce.
func TestEveryTargetConstraintHasADryRunReplica(t *testing.T) {
	tgt := targetDB(t)
	rows, err := admin(t, tgt).Query(tctx(t), `
        SELECT conname FROM pg_constraint
         WHERE conrelid IN ('magnetico_web.users'::regclass, 'magnetico_web.invites'::regclass, 'magnetico_web.rss_tokens'::regclass)
        UNION
        SELECT indexrelid::regclass::text FROM pg_index
         WHERE indrelid IN ('magnetico_web.users'::regclass, 'magnetico_web.invites'::regclass, 'magnetico_web.rss_tokens'::regclass)
           AND indisunique AND NOT indisprimary`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		name = strings.TrimPrefix(name, "magnetico_web.")
		seen[name] = true
		if _, ok := ConstraintCoverage[name]; !ok {
			t.Errorf("target constraint %s has no dry-run replica or documented reason", name)
		}
	}
	for name := range ConstraintCoverage {
		if !seen[name] {
			t.Errorf("ConstraintCoverage lists %s, which the schema does not have", name)
		}
	}
	// Every named finding code is reachable (appears in the analysis source).
	src, err := os.ReadFile("validate.go")
	if err != nil {
		t.Fatal(err)
	}
	for name, how := range ConstraintCoverage {
		if strings.HasPrefix(how, "constant:") || strings.HasPrefix(how, "ids are") {
			continue
		}
		if !strings.Contains(string(src), `"`+how+`"`) {
			t.Errorf("%s names finding %q, which Analyze never produces", name, how)
		}
	}
}
