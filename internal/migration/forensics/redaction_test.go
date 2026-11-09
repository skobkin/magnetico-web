package forensics

// Redaction unit tests (owner review finding 1): the deny-by-default
// whitelist must drop unknown keys entirely, refuse unsafe values under
// known keys, and preserve exactly the known-safe scalar diagnostics —
// including the owner's exact adversarial cases.

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustRedact(t *testing.T, kind, raw string) (map[string]any, redactStats) {
	t.Helper()
	out, stats := RedactJSON(kind, json.RawMessage(raw))
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("redacted output not an object: %v (%s)", err, out)
	}
	return m, stats
}

func TestRedactJSONOwnerAdversarialCases(t *testing.T) {
	t.Parallel()

	t.Run("password under unknown key is dropped entirely", func(t *testing.T) {
		t.Parallel()
		m, stats := mustRedact(t, "error_details", `{"password":"secret","count":3}`)
		if _, present := m["password"]; present {
			t.Fatalf("unknown key echoed: %v", m)
		}
		if m["count"] != float64(3) {
			t.Fatalf("safe numeric lost: %v", m)
		}
		if stats.UnknownKeys != 1 {
			t.Fatalf("unknown_keys = %d, want 1", stats.UnknownKeys)
		}
		if _, has := m["redacted"]; !has {
			t.Fatalf("redacted stats missing from output: %v", m)
		}
	})

	t.Run("free-text keys are not whitelisted at all (review finding 6)", func(t *testing.T) {
		t.Parallel()
		// The old "safetext" rule let any short control-free value through
		// known free-text keys — an unmarked secret like "hunter2" passed
		// unchanged. Arbitrary stored strings cannot be proven safe, so
		// reason/error/stage are now unknown keys: dropped entirely, only a
		// count survives.
		m, stats := mustRedact(t, "error_details",
			`{"reason":"hunter2","error":"short","stage":"decode"}`)
		for _, k := range []string{"reason", "error", "stage"} {
			if _, present := m[k]; present {
				t.Fatalf("free-text key %s echoed: %v", k, m)
			}
		}
		if stats.UnknownKeys != 3 {
			t.Fatalf("unknown_keys = %d, want 3", stats.UnknownKeys)
		}
		if _, has := m["redacted"]; !has {
			t.Fatalf("redaction stats missing from output: %v", m)
		}
	})

	t.Run("credential grammar under a former safetext key is dropped", func(t *testing.T) {
		t.Parallel()
		m, stats := mustRedact(t, "error_details",
			`{"reason":"postgres://user:pass@host/db","error":"host /var/run has passwd entries"}`)
		for _, k := range []string{"reason", "error"} {
			if _, present := m[k]; present {
				t.Fatalf("key %s echoed: %v", k, m)
			}
		}
		if stats.UnknownKeys != 2 {
			t.Fatalf("unknown_keys = %d, want 2", stats.UnknownKeys)
		}
	})

	t.Run("unknown keys with hostile names are never echoed", func(t *testing.T) {
		t.Parallel()
		raw := `{"debug_note":"API token: abc123","password_hint":"the password is hunter2","secret":"s"}`
		out, stats := RedactJSON("error_details", json.RawMessage(raw))
		s := string(out)
		for _, forbidden := range []string{"debug_note", "password_hint", "hunter2", "abc123", "token", "secret"} {
			if strings.Contains(s, forbidden) {
				t.Fatalf("output leaks %q: %s", forbidden, s)
			}
		}
		if stats.UnknownKeys != 3 {
			t.Fatalf("unknown_keys = %d, want 3", stats.UnknownKeys)
		}
	})

	t.Run("nested objects under scalar constraints are refused", func(t *testing.T) {
		t.Parallel()
		m, stats := mustRedact(t, "error_details", `{"count":{"actual":3},"stage":{"nested":"obj"}}`)
		if v, ok := m["count"].(map[string]any); !ok || v["redacted"] != "constraint" {
			t.Fatalf("key count not refused: %v", m["count"])
		}
		if _, present := m["stage"]; present {
			t.Fatalf("free-text key stage echoed: %v", m)
		}
		if stats.ConstraintFailures != 1 || stats.UnknownKeys != 1 {
			t.Fatalf("stats = %+v, want 1 constraint failure + 1 unknown key", stats)
		}
	})
}

// TestRedactJSONExactInt64Preservation (review round 2, finding 2): the
// redactor decodes with UseNumber — numbers keep their exact digits — and
// conInt validates via exact signed int64 parsing. Integers past the
// float64 mantissa (2^53) used to be silently MANGLED by the float64
// decode, and ±MaxInt64 used to be refused by the float range check. The
// assertions compare the OUTPUT BYTES: re-unmarshaling into float64 would
// reproduce exactly the bug under test.
func TestRedactJSONExactInt64Preservation(t *testing.T) {
	t.Parallel()

	t.Run("integers beyond the float64 mantissa survive byte-exact", func(t *testing.T) {
		t.Parallel()
		raw := `{"count":9007199254740993,"actual":9223372036854775807,"observed":-9223372036854775808}`
		out, stats := RedactJSON("error_details", json.RawMessage(raw))
		if stats.ConstraintFailures != 0 || stats.UnknownKeys != 0 {
			t.Fatalf("exact int64 values refused: %+v", stats)
		}
		s := string(out)
		for _, want := range []string{
			`"count":9007199254740993`,
			`"actual":9223372036854775807`,
			`"observed":-9223372036854775808`,
		} {
			if !strings.Contains(s, want) {
				t.Fatalf("output %s lost the exact integer %s", s, want)
			}
		}
		// The float64-mangled forms of count (2^53+1 → 2^53) and MaxInt64
		// (→ 2^63) must never appear. (The MinInt64 mangle shares digits
		// with its correct form, so it is only checked under its own key.)
		for _, mangled := range []string{`"count":9007199254740992`, `"actual":9223372036854775808`} {
			if strings.Contains(s, mangled) {
				t.Fatalf("output %s shows float64-mangled digits (%s)", s, mangled)
			}
		}
	})

	t.Run("out-of-range, noninteger and exponent forms fail the int constraint", func(t *testing.T) {
		t.Parallel()
		m, stats := mustRedact(t, "error_details",
			`{"count":99999999999999999999,"actual":3.14,"observed":4.0,"length":1e6}`)
		for _, k := range []string{"count", "actual", "observed", "length"} {
			if v, ok := m[k].(map[string]any); !ok || v["redacted"] != "constraint" {
				t.Fatalf("key %s not refused: %v", k, m[k])
			}
		}
		if stats.ConstraintFailures != 4 {
			t.Fatalf("constraint_failures = %d, want 4", stats.ConstraintFailures)
		}
	})

	t.Run("null under an int constraint stays null", func(t *testing.T) {
		t.Parallel()
		m, stats := mustRedact(t, "error_details", `{"orphan_id_min":null}`)
		if v, ok := m["orphan_id_min"]; !ok || v != nil {
			t.Fatalf("explicit null = %v, want null", v)
		}
		if stats.ConstraintFailures != 0 {
			t.Fatalf("null refused: %+v", stats)
		}
	})
}

func TestRedactJSONPreservesSafeDiagnostics(t *testing.T) {
	t.Parallel()

	raw := `{
      "limit":"stored_bytes","side":"source","sqlstate":"22001",
      "actual":33000000,"allowed":33554432,"count":12,"expected":12,"observed":11,
      "at_start":100,"at_end":200,"length":42,"file_count":7,"decoded_files":6,
      "fields":3,"source_files":9,"target_files":8,"from_id":1,"to_id":64,
      "checkpoint":64,"min_id":1,"max_id":999,"uncovered":1,"assumed_single":true,
      "orphan_files":0,"orphan_source_files":0,"null_owner_files":0,
      "orphan_id_min":null,"orphan_id_max":null,"registry_integrity":0,
      "stored_max_id":77,"fresh_max_id":77,"stored_torrents":1000,"fresh_torrents":1000
    }`
	m, stats := mustRedact(t, "issue_details", raw)
	if stats.UnknownKeys != 0 || stats.ConstraintFailures != 0 {
		t.Fatalf("safe diagnostics refused: %+v", stats)
	}
	if _, has := m["redacted"]; has {
		t.Fatalf("no redaction should be recorded: %v", m)
	}
	checks := map[string]any{
		"limit": "stored_bytes", "side": "source", "sqlstate": "22001",
		"actual": float64(33000000), "allowed": float64(33554432),
		"assumed_single": true, "file_count": float64(7), "checkpoint": float64(64),
	}
	for k, want := range checks {
		if m[k] != want {
			t.Fatalf("key %s = %v, want %v", k, m[k], want)
		}
	}
	// NULL numbers under an int constraint stay null (not refused).
	if v, ok := m["orphan_id_min"]; !ok || v != nil {
		t.Fatalf("explicit JSON null under int constraint = %v, want null", m["orphan_id_min"])
	}
}

func TestRedactJSONOtherKinds(t *testing.T) {
	t.Parallel()

	t.Run("fingerprint kinds", func(t *testing.T) {
		t.Parallel()
		raw := `{"server_address":"127.0.0.1","server_port":"5432","postmaster_start_time":"2026-11-09T10:00:00Z",
          "database":"magneticodb","torrents":100,"min_id":1,"max_id":100,
          "anchor_min_info_hash":"` + strings.Repeat("ab", 20) + `","anchor_max_info_hash":"` + strings.Repeat("cd", 20) + `",
          "file_rows":50,"null_owner_files":0,"orphan_files":0,"orphan_id_min":null,"orphan_id_max":null,
          "conninfo":"postgres://secret@host/db"}`
		m, stats := mustRedact(t, "fingerprint", raw)
		if stats.UnknownKeys != 1 {
			t.Fatalf("unknown_keys = %d, want 1", stats.UnknownKeys)
		}
		if m["server_port"] != "5432" || m["database"] != "magneticodb" || m["torrents"] != float64(100) {
			t.Fatalf("safe fingerprint fields lost: %v", m)
		}
		if m["anchor_min_info_hash"] != strings.Repeat("ab", 20) {
			t.Fatalf("hex40 anchor lost: %v", m["anchor_min_info_hash"])
		}
	})

	t.Run("fingerprint port constraint refuses wrong shapes", func(t *testing.T) {
		t.Parallel()
		m, stats := mustRedact(t, "fingerprint", `{"server_port":5432,"server_address":"host:99999"}`)
		// server_port as JSON NUMBER fails the string constraint;
		// a port above 65535... is 5 digits so passes the grammar (documented:
		// the constraint is syntactic); the NUMBER is the refusal here.
		if stats.ConstraintFailures != 1 {
			t.Fatalf("constraint_failures = %d, want 1 (numeric port)", stats.ConstraintFailures)
		}
		if v, ok := m["server_port"].(map[string]any); !ok || v["redacted"] != "constraint" {
			t.Fatalf("numeric server_port not refused: %v", m["server_port"])
		}
	})

	t.Run("settings kinds", func(t *testing.T) {
		t.Parallel()
		m, _ := mustRedact(t, "verify_settings",
			`{"workers":4,"batch_size":64,"max_inflight_bytes":1000000,"accept_source_repairs":true,"dsn":"postgres://x"}`)
		if m["workers"] != float64(4) || m["accept_source_repairs"] != true {
			t.Fatalf("safe settings lost: %v", m)
		}
		if _, present := m["dsn"]; present {
			t.Fatalf("dsn echoed: %v", m)
		}
	})

	t.Run("run_options and identity kinds", func(t *testing.T) {
		t.Parallel()
		m, _ := mustRedact(t, "run_options", `{"workers":2,"batch_size":8,"max_inflight_bytes":5,"extra":1}`)
		if m["workers"] != float64(2) || len(m) != 4 { // 3 safe + redacted stats
			t.Fatalf("run_options shape wrong: %v", m)
		}
		id, _ := mustRedact(t, "identity",
			`{"postmaster_start_time":"2026-11-09T10:00:00Z","database":"magdb","server_address":"","server_port":"","known":true,"password":"p"}`)
		if id["database"] != "magdb" || id["known"] != true {
			t.Fatalf("safe identity fields lost: %v", id)
		}
		if _, present := id["password"]; present {
			t.Fatalf("identity password echoed: %v", id)
		}
	})
}

func TestRedactJSONShapeRefusals(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		kind string
		raw  string
	}{
		{"array top level", "error_details", `[1,2,3]`},
		{"scalar top level", "fingerprint", `"postgres://u:p@h/db"`},
		{"broken json", "error_details", `{"count":3`},
		{"unknown kind", "not_a_kind", `{"count":3}`},
		{"null", "error_details", `null`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, _ := RedactJSON(tc.kind, json.RawMessage(tc.raw))
			var m map[string]any
			if err := json.Unmarshal(out, &m); err != nil {
				t.Fatalf("output not an object: %s", out)
			}
			if m["redacted"] != "shape" && tc.raw != "null" {
				t.Fatalf("expected shape refusal, got: %s", out)
			}
			if tc.raw == "null" && string(out) != "null" {
				t.Fatalf("null input = %s, want null", out)
			}
		})
	}

	t.Run("empty input renders null", func(t *testing.T) {
		t.Parallel()
		out, _ := RedactJSON("error_details", nil)
		if string(out) != "null" {
			t.Fatalf("nil input = %s, want null", out)
		}
	})

	t.Run("whole oversize value collapses to a marker", func(t *testing.T) {
		t.Parallel()
		// Every whitelisted key, filled with constraint-valid values; the
		// marker behavior itself is exercised through the parameterized
		// size gate at a small limit (the whitelist's valid values stay
		// under the production 4096 by construction).
		parts := []string{}
		for key, constraint := range detailsWhitelist {
			var val string
			switch {
			case strings.HasPrefix(constraint, "enum:") || constraint == conSQLState:
				// Values must satisfy the constraint; reuse the first allowed entry.
				if strings.HasPrefix(constraint, "enum:") {
					val = `"` + strings.Split(strings.SplitN(constraint, ":", 2)[1], ",")[0] + `"`
				} else {
					val = `"22001"`
				}
			case constraint == conBool:
				val = `true`
			default:
				val = `1234567890`
			}
			parts = append(parts, `"`+key+`":`+val)
		}
		raw := "{" + strings.Join(parts, ",") + "}"
		out, stats := RedactJSON("error_details", json.RawMessage(raw))
		if stats.ConstraintFailures != 0 || stats.UnknownKeys != 0 {
			t.Fatalf("safe values unexpectedly refused: %+v", stats)
		}
		var m map[string]any
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatalf("output not an object: %s", out)
		}
		if _, has := m["redacted"]; has {
			t.Fatalf("under the production limit the safe value must pass through: %v", m["redacted"])
		}

		marker := oversizeMarker([]byte(strings.Repeat("y", 300)), stats, 100)
		var mm map[string]any
		if err := json.Unmarshal(marker, &mm); err != nil {
			t.Fatalf("marker not an object: %s", marker)
		}
		if mm["redacted"] != "oversize" || mm["bytes"] != float64(300) {
			t.Fatalf("oversize marker wrong: %s", marker)
		}
	})
}

func TestHexBytesMarshal(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   hexBytes
		want string
	}{
		{nil, "null"},
		{hexBytes{}, `""`},
		{hexBytes{0xde, 0xad, 0xbe, 0xef}, `"deadbeef"`},
		{hexBytes{0x0a, 0x0b}, `"0a0b"`},
	}
	for _, tc := range cases {
		out, err := json.Marshal(tc.in)
		if err != nil || string(out) != tc.want {
			t.Fatalf("hexBytes(%v) = %s, %v; want %s", tc.in, out, err, tc.want)
		}
	}
}
