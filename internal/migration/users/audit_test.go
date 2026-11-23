package users

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// runAudit executes docs/wip/07H-audit.sql (the owner's read-only audit) and
// returns the rows of its two result sets as text cells.
func runAudit(t *testing.T, dsn string) (formats, shape [][]string, header []string) {
	t.Helper()
	sql, err := os.ReadFile("../../../docs/wip/07H-audit.sql")
	if err != nil {
		t.Fatal(err)
	}
	results, err := admin(t, dsn).PgConn().Exec(tctx(t), string(sql)).ReadAll()
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	var sets [][][]string
	var heads [][]string
	for _, r := range results {
		if len(r.FieldDescriptions) == 0 {
			continue
		}
		var h []string
		for _, f := range r.FieldDescriptions {
			h = append(h, f.Name)
		}
		var rows [][]string
		for _, row := range r.Rows {
			var cells []string
			for _, c := range row {
				cells = append(cells, string(c))
			}
			rows = append(rows, cells)
		}
		sets, heads = append(sets, rows), append(heads, h)
	}
	if len(sets) != 2 {
		t.Fatalf("audit produced %d result sets, want 2", len(sets))
	}
	return sets[0], sets[1], heads[0]
}

// TestAuditAgreesWithTheGoClassifierAndLeaksNothing proves the owner's audit
// SQL classifies exactly like CategorizeHash (the authoritative classifier)
// over the PHP fixtures and the unvetted/unsupported variants, that its
// output carries only aggregates, and that it writes nothing.
func TestAuditAgreesWithTheGoClassifierAndLeaksNothing(t *testing.T) {
	src := legacyDB(t)
	sodium := hashFor(t, famSodium)
	hashes := []string{
		"$2y$12$" + strings.Repeat("a", 53),
		"$2y$12$" + strings.Repeat("b", 53),
		strings.Replace(sodium, "m=65536,t=4,p=1", "m=32768,t=4,p=1", 1),
		strings.Replace(sodium, "m=65536,t=4,p=1", "m=65536,t=3,p=4", 1),
		"$2a$10$" + strings.Repeat("a", 53),
		"$2b$10$" + strings.Repeat("a", 53),
		strings.Replace(sodium, "m=65536,t=4,p=1", "m=131072,t=4,p=1", 1),
		strings.Replace(sodium, "v=19", "v=16", 1),
		strings.Replace(sodium, "$argon2id$", "$argon2d$", 1),
		"not a hash", "{SSHA}abcdefghijklmnop", "$1$legacy$md5cryptstyle",
	}
	for _, c := range fixtureCases(t) {
		hashes = append(hashes, c.Hash)
	}
	want := map[string]int{}
	for _, h := range hashes {
		switch CategorizeHash(h) {
		case HashVerified:
			want["verified"]++
		case HashUnvetted:
			want["unvetted"]++
		case HashUnsupported:
			want["unsupported"]++
		}
	}
	for i, h := range hashes {
		execSQL(t, src, `INSERT INTO users.users (id, username, password, email, roles, created_at)
            VALUES (`+strconv.Itoa(i+1)+`, 'u`+strconv.Itoa(i)+`', '`+h+`', 'u`+strconv.Itoa(i)+`@example.org', '["ROLE_USER"]', now())`)
	}
	execSQL(t, src, `INSERT INTO users.users (id, username, password, email, roles, created_at) VALUES (900, 'empty', '', 'e@example.org', '["ROLE_USER","ROLE_ADMIN"]', now())`)
	want["empty"]++
	before := sourceState(t, src)

	formats, shape, header := runAudit(t, src)
	col := map[string]int{}
	for i, h := range header {
		col[h] = i
	}
	got := map[string]int{}
	var total int
	for _, row := range formats {
		n, err := strconv.Atoi(row[col["accounts"]])
		if err != nil {
			t.Fatal(err)
		}
		got[row[col["category"]]] += n
		total += n
	}
	for _, cat := range []string{"verified", "unvetted", "unsupported", "empty"} {
		if got[cat] != want[cat] {
			t.Errorf("category %s: audit %d, Go classifier %d", cat, got[cat], want[cat])
		}
	}
	if total != len(hashes)+1 {
		t.Errorf("audit counted %d accounts, want %d", total, len(hashes)+1)
	}

	// Only aggregates: no hash, salt, digest, username or email in any cell.
	var all strings.Builder
	for _, row := range append(append([][]string{}, formats...), shape...) {
		all.WriteString(strings.Join(row, "|") + "\n")
	}
	out := all.String()
	for _, h := range hashes {
		for _, part := range strings.Split(h, "$") {
			if len(part) >= 8 && strings.Contains(out, part) && !strings.HasPrefix(part, "v=") && !strings.HasPrefix(part, "m=") && !strings.HasPrefix(part, "argon2") {
				t.Errorf("audit output contains hash material %.6s…", part)
			}
		}
	}
	for _, s := range []string{"example.org", "u1", "ROLE_USER"} {
		if strings.Contains(out, s) {
			t.Errorf("audit output contains %q", s)
		}
	}
	if len(shape) != 1 || shape[0][0] != strconv.Itoa(len(hashes)+1) {
		t.Errorf("shape row: %v", shape)
	}
	if sourceState(t, src) != before {
		t.Error("the audit wrote to the source")
	}
}

func TestAuditShapeCounts(t *testing.T) {
	src, _ := seeded(t)
	execSQL(t, src,
		`INSERT INTO users.api_tokens (key, user_id, created_at)
            SELECT 'f' || lpad(g::text, 31, '0'), 6, now() FROM generate_series(1, 55) g`)
	_, shape, _ := runAudit(t, src)
	// users, invites, used_invites, api_tokens, max_tokens_per_user, users_over_50_tokens, case_duplicate_email_groups
	want := []string{"6", "5", "2", "59", "56", "1", "1"}
	for i, w := range want {
		if shape[0][i] != w {
			t.Errorf("shape column %d = %s, want %s (row %v)", i, shape[0][i], w, shape[0])
		}
	}
}

// malformedHashes returns hashes that carry an exact vetted parameter family
// (or are one edit away from one) but that the Go verifier rejects as a whole.
func malformedHashes(t *testing.T) []string {
	t.Helper()
	var out []string
	argon := func(base string, mutate func(p []string)) {
		p := strings.Split(base, "$") // "", alg, v, params, salt, key
		mutate(p)
		out = append(out, strings.Join(p, "$"))
	}
	for _, fam := range []string{famSodium, famArgon2i, famArgonT2, famArgonT3P2} {
		base := hashFor(t, fam)
		argon(base, func(p []string) { p[4] = p[4][:10] })                              // salt below 8 bytes
		argon(base, func(p []string) { p[4] = p[4][:len(p[4])-1] })                     // impossible Base64 length (1 mod 4)
		argon(base, func(p []string) { p[4] = p[4][:len(p[4])-1] + "B" })               // non-zero trailing bits in the salt
		argon(base, func(p []string) { p[4] = strings.Repeat("A", 100) })               // salt above 64 bytes
		argon(base, func(p []string) { p[5] = p[5][:len(p[5])-2] })                     // truncated key (length 1 mod 4)
		argon(base, func(p []string) { p[5] = p[5][:20] })                              // key below 16 bytes
		argon(base, func(p []string) { p[5] = p[5][:len(p[5])-1] + "B" })               // non-zero trailing bits in the key
		argon(base, func(p []string) { p[5] += "=" })                                   // padded Base64
		argon(base, func(p []string) { p[5] = "!" + p[5][1:] })                         // invalid Base64 character
		argon(base, func(p []string) { p[3] = strings.Replace(p[3], "m=", "m=0", 1) })  // leading zero in m
		argon(base, func(p []string) { p[3] = strings.Replace(p[3], "p=", "p=0", 1) })  // leading zero in p
		argon(base, func(p []string) { p[3] = strings.Replace(p[3], "t=", "t=00", 1) }) // leading zeros in t
		argon(base, func(p []string) { p[3] = strings.Replace(p[3], "m=", "m=+", 1) })  // signed m
		argon(base, func(p []string) { p[3] += ",x=1" })                                // extra parameter
	}
	for _, fam := range []string{famBcrypt10, famBcrypt13} {
		b := hashFor(t, fam)
		out = append(out,
			b[:len(b)-1],      // truncated hash
			b+"a",             // too long
			b[:10]+"+"+b[11:], // character outside ./A-Za-z0-9
			"$2y$03$"+b[7:],   // cost below 4
			"$2y$17$"+b[7:],   // cost above 16
		)
	}
	return out
}

// TestAuditNeverCallsAMalformedHashVerified is the regression for exact
// parameter families whose complete hash the verifier would reject.
func TestAuditNeverCallsAMalformedHashVerified(t *testing.T) {
	src := legacyDB(t)
	bad := malformedHashes(t)
	for i, h := range bad {
		if got := CategorizeHash(h); got != HashUnsupported {
			t.Fatalf("malformed case %d: the Go classifier says %d, want unsupported (fix the case)", i, got)
		}
	}
	fixtures := fixtureCases(t)
	var values []string
	add := func(h string) {
		id := strconv.Itoa(len(values) + 1)
		values = append(values, `(`+id+`, 'u`+id+`', '`+h+`', 'u`+id+`@example.org', '["ROLE_USER"]', now())`)
	}
	for _, c := range fixtures {
		add(c.Hash)
	}
	for _, h := range bad {
		add(h)
	}
	execSQL(t, src, `INSERT INTO users.users (id, username, password, email, roles, created_at) VALUES `+strings.Join(values, ","))
	formats, _, header := runAudit(t, src)
	col := map[string]int{}
	for i, h := range header {
		col[h] = i
	}
	got := map[string]int{}
	for _, row := range formats {
		n, err := strconv.Atoi(row[col["accounts"]])
		if err != nil {
			t.Fatal(err)
		}
		got[row[col["category"]]] += n
	}
	if got["verified"] != len(fixtures) || got["unsupported"] != len(bad) || got["unvetted"] != 0 {
		t.Fatalf("audit categories %v; want verified=%d unsupported=%d unvetted=0", got, len(fixtures), len(bad))
	}
}
