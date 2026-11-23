package password

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

type fixture struct {
	ID               string `json:"id"`
	Family           string `json:"family"`
	PasswordLen      int    `json:"password_len"`
	PasswordB64      string `json:"password_b64"`
	Hash             string `json:"hash"`
	Verifies         bool   `json:"verifies"`
	WrongB64         string `json:"wrong_password_b64"`
	WrongVerifies    bool   `json:"wrong_verifies"`
	TailB64          string `json:"tail_variant_b64"`
	TailVerifies     bool   `json:"tail_variant_verifies"`
	Prefix72B64      string `json:"prefix72_b64"`
	Prefix72Verifies *bool  `json:"prefix72_verifies"`
}

func loadFixtures(t *testing.T) (meta map[string]any, cases []fixture) {
	t.Helper()
	raw, err := os.ReadFile("testdata/php/fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Meta  map[string]any `json:"meta"`
		Cases []fixture      `json:"cases"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Cases) == 0 {
		t.Fatal("no fixtures")
	}
	return doc.Meta, doc.Cases
}

func b64(t *testing.T, s string) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Every vector was produced and judged by PHP itself; Go must agree with each
// of PHP's verdicts, including bcrypt's 72-byte truncation.
func TestPHPVectors(t *testing.T) {
	meta, cases := loadFixtures(t)
	t.Logf("fixtures: PHP %v, argon2 %v, libsodium %v, %d cases", meta["php_version"], meta["password_argon2_provider"], meta["sodium_library_version"], len(cases))
	svc := NewService(NewLimiter(2, time.Minute), CurrentParams)
	ctx := context.Background()
	for _, c := range cases {
		t.Run(c.ID, func(t *testing.T) {
			if testing.Short() && (strings.Contains(c.ID, "cost13") ||
				c.Family != "bcrypt" && !strings.HasSuffix(c.ID, "/ascii") && !strings.HasSuffix(c.ID, "/len73")) {
				t.Skip("reduced subset under -short")
			}
			if got := svc.params.Classify(c.Hash); got != ClassLegacy {
				t.Fatalf("Classify = %v, want legacy (all PHP hashes need a rehash)", got)
			}
			pw := b64(t, c.PasswordB64)
			if len(pw) != c.PasswordLen {
				t.Fatalf("fixture password length %d != %d", len(pw), c.PasswordLen)
			}
			check := func(what, in string, want bool) {
				t.Helper()
				got, err := svc.Verify(ctx, in, c.Hash)
				if err != nil {
					t.Fatalf("%s: %v", what, err)
				}
				if got != want {
					t.Errorf("%s: Verify = %v, PHP says %v", what, got, want)
				}
			}
			check("password", pw, c.Verifies)
			check("wrong password", b64(t, c.WrongB64), c.WrongVerifies)
			if tail := b64(t, c.TailB64); len(tail) > MaxLoginLength {
				// Beyond the login limit: refused before any hashing.
				if _, err := svc.Verify(ctx, tail, c.Hash); !errors.Is(err, ErrTooLong) {
					t.Errorf("oversize tail variant: err = %v, want ErrTooLong", err)
				}
			} else {
				check("tail variant", tail, c.TailVerifies)
			}
			if c.Prefix72Verifies != nil {
				check("72-byte prefix", b64(t, c.Prefix72B64), *c.Prefix72Verifies)
			}
			// A single flipped hash byte must fail.
			flipped := flipHashByte(c.Hash)
			if got, err := svc.Verify(ctx, pw, flipped); err == nil && got {
				t.Error("a hash with a flipped character still verifies")
			}
		})
	}
}

// flipHashByte changes one character of the hash part (the last field).
func flipHashByte(h string) string {
	b := []byte(h)
	i := len(b) - 5
	if b[i] == 'A' {
		b[i] = 'B'
	} else {
		b[i] = 'A'
	}
	return string(b)
}

func TestFixturesHaveExpectedCoverage(t *testing.T) {
	_, cases := loadFixtures(t)
	seen := map[string]bool{}
	for _, c := range cases {
		seen[strings.SplitN(c.ID, "/", 2)[0]] = true
		if c.PasswordLen > 72 && c.Family == "bcrypt" && (c.Prefix72Verifies == nil || !*c.Prefix72Verifies) {
			t.Errorf("%s: PHP should confirm bcrypt ignores bytes beyond 72", c.ID)
		}
		if c.PasswordLen > 72 && c.Family != "bcrypt" && c.Prefix72Verifies != nil && *c.Prefix72Verifies {
			t.Errorf("%s: argon2 must not ignore bytes beyond 72", c.ID)
		}
	}
	for _, want := range []string{"argon2i_m16384_t2_p4", "argon2id_sodium_m65536_t4_p1", "argon2id_php_default", "bcrypt_2y_cost10", "bcrypt_2y_cost13"} {
		if !seen[want] {
			t.Errorf("fixture family %s missing", want)
		}
	}
}

// Every PHP-era hash that verifies must, once rehashed, verify (and only) its
// own password against the new argon2id hash.
func TestRehashOfPHPFixtures(t *testing.T) {
	_, cases := loadFixtures(t)
	svc := NewService(NewLimiter(2, time.Minute), fastParams)
	ctx := context.Background()
	n := 0
	for _, c := range cases {
		if !c.Verifies || testing.Short() && (strings.Contains(c.ID, "cost13") ||
			c.Family != "bcrypt" && !strings.HasSuffix(c.ID, "/ascii") && !strings.HasSuffix(c.ID, "/len73")) {
			continue
		}
		pw := b64(t, c.PasswordB64)
		if len(pw) == 0 || len(pw) > MaxLoginLength {
			continue
		}
		ok, err := svc.Verify(ctx, pw, c.Hash)
		if err != nil || !ok {
			t.Fatalf("%s: legacy verify: %v %v", c.ID, ok, err)
		}
		nh, err := svc.Rehash(ctx, pw)
		if err != nil {
			t.Fatalf("%s: rehash: %v", c.ID, err)
		}
		if svc.params.Classify(nh) != ClassCurrent {
			t.Errorf("%s: rehash not current", c.ID)
		}
		if ok, err := svc.Verify(ctx, pw, nh); err != nil || !ok {
			t.Errorf("%s: rehashed hash rejects the password: %v %v", c.ID, ok, err)
		}
		if ok, _ := svc.Verify(ctx, b64(t, c.WrongB64), nh); ok && b64(t, c.WrongB64) != pw {
			t.Errorf("%s: rehashed hash accepts the wrong password", c.ID)
		}
		n++
	}
	if n == 0 {
		t.Fatal("no fixtures exercised")
	}
}
