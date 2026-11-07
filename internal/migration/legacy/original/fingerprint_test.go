package original

import (
	"encoding/json"
	"testing"
	"time"
)

func TestFingerprintEquivalent(t *testing.T) {
	t.Parallel()

	base := Fingerprint{
		PostmasterStart:   time.Unix(1700000000, 0),
		Database:          "legacy",
		Torrents:          10,
		MinID:             1,
		MaxID:             20,
		AnchorMinInfoHash: "aa",
		AnchorMaxInfoHash: "bb",
		FileRows:          100,
		NullOwnerFiles:    2,
		OrphanFiles:       1,
		OrphanIDMin:       ptrInt64(7),
		OrphanIDMax:       ptrInt64(9),
	}

	// Transport fields may differ (resume over another connection path).
	viaSocket := base
	viaSocket.ServerAddress = ""
	viaSocket.ServerPort = ""
	if !base.Equivalent(viaSocket) {
		t.Fatal("transport-only difference must stay equivalent")
	}

	// A source PostgreSQL restart between sessions changes only the
	// postmaster start time; the data is untouched, so the run stays
	// resumable. (Start-time equality still gates source-vs-target
	// distinctness at preflight, fail-closed — it is a temporal fact, not a
	// content anchor.)
	restarted := base
	restarted.PostmasterStart = time.Unix(1700000001, 0)
	if !base.Equivalent(restarted) {
		t.Fatal("a postmaster restart with unchanged content must stay equivalent")
	}

	change := []func(*Fingerprint){
		func(f *Fingerprint) { f.Database = "other" },
		func(f *Fingerprint) { f.Torrents++ },
		func(f *Fingerprint) { f.MinID-- },
		func(f *Fingerprint) { f.MaxID++ },
		func(f *Fingerprint) { f.AnchorMinInfoHash = "cc" },
		func(f *Fingerprint) { f.AnchorMaxInfoHash = "dd" },
		func(f *Fingerprint) { f.FileRows++ },
		func(f *Fingerprint) { f.NullOwnerFiles++ },
		func(f *Fingerprint) { f.OrphanFiles++ },
		func(f *Fingerprint) { f.OrphanIDMin = ptrInt64(8) },
		func(f *Fingerprint) { f.OrphanIDMax = nil },
	}
	for i, mutate := range change {
		f := base
		mutate(&f)
		if base.Equivalent(f) {
			t.Fatalf("mutation %d must break equivalence", i)
		}
	}

	// JSON round-trip preserves equivalence decisions.
	raw, err := json.Marshal(base)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded Fingerprint
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !base.Equivalent(decoded) {
		t.Fatal("JSON round-trip must preserve equivalence")
	}
}

func ptrInt64(v int64) *int64 { return &v }
