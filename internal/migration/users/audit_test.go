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
