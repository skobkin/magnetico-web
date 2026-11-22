package crawler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/skobkin/magnetico-web/internal/crawlqueue"
)

func queueStats(r *rig) crawlqueue.Stats { return r.q.Stats() }

func TestSenderDeliversAndAcknowledges(t *testing.T) {
	r := newRig(t)
	for i := 1; i <= 10; i++ {
		r.add(t, i, goodPayload(t, i))
	}
	if o := r.drain(t); o != outcomeProgress {
		t.Fatalf("outcome %v", o)
	}
	if r.ingest.count() != 10 || queueStats(r).QueueRecords != 0 || r.cnt.SendAcked.Load() != 10 {
		t.Fatalf("stored=%d stats=%+v", r.ingest.count(), queueStats(r))
	}
}

// The bad record is isolated by bisection and quarantined with its payload
// byte-identical; every other record is delivered. Covers head/middle/tail
// and batch sizes from 2 up to a depth-8 bisection (256 records).
func TestBisectionQuarantinesExactlyTheBadRecord(t *testing.T) {
	for _, tc := range []struct {
		name string
		size int
		bad  int // 1-based position
	}{
		{"pair head", 2, 1}, {"pair tail", 2, 2},
		{"head", 10, 1}, {"middle", 10, 6}, {"tail", 10, 10},
		{"max depth head", 256, 1}, {"max depth middle", 256, 129}, {"max depth tail", 256, 256},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			badRaw := badPayload(tc.bad)
			for i := 1; i <= tc.size; i++ {
				if i == tc.bad {
					r.add(t, i, badRaw)
				} else {
					r.add(t, i, goodPayload(t, i))
				}
			}
			if o := r.drain(t); o != outcomeProgress {
				t.Fatalf("outcome %v", o)
			}
			st := queueStats(r)
			if st.QueueRecords != 0 || st.QuarantineRecords != 1 {
				t.Fatalf("stats = %+v, want empty queue and exactly 1 quarantined", st)
			}
			if r.ingest.count() != tc.size-1 || r.ingest.has(tc.bad) {
				t.Fatalf("delivered %d records (bad delivered: %v)", r.ingest.count(), r.ingest.has(tc.bad))
			}
			if st.QuarantineBytes != int64(len(badRaw)) {
				t.Fatalf("quarantined bytes %d, want the intact payload (%d)", st.QuarantineBytes, len(badRaw))
			}
		})
	}
}

func TestSingleRecordRejectionsQuarantineWithReasonAndNextIsDelivered(t *testing.T) {
	r := newRig(t)
	r.add(t, 1, unsupportedPayload(t, 1))
	r.add(t, 2, goodPayload(t, 2))
	// Batch of two is bisected; then the single unsupported_version record
	// is quarantined and the good one delivered.
	if o := r.drain(t); o != outcomeProgress {
		t.Fatalf("outcome %v", o)
	}
	if !r.ingest.has(2) || r.ingest.has(1) {
		t.Fatal("wrong delivery")
	}
	if st := queueStats(r); st.QuarantineRecords != 1 || st.QueueRecords != 0 {
		t.Fatalf("stats %+v", st)
	}
}

func TestUnrecognisedRejectionsNeverQuarantine(t *testing.T) {
	responses := map[string]func(w http.ResponseWriter){
		"invalid_request json": func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":{"code":"invalid_request","retryable":false}}`))
		},
		"payload_too_large json": func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(413)
			_, _ = w.Write([]byte(`{"error":{"code":"payload_too_large","retryable":false}}`))
		},
		"unknown code": func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":{"code":"something_new"}}`))
		},
		"empty body 400": func(w http.ResponseWriter) { w.WriteHeader(400) },
		"non-json 413":   func(w http.ResponseWriter) { w.WriteHeader(413); _, _ = w.Write([]byte("not json")) },
		"proxy html 413": func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(413)
			_, _ = w.Write([]byte("<html>Request Entity Too Large\x00\x01</html>"))
		},
		"foreign json 400": func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"message":"bad"}`))
		},
		"record code wrong ct": func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":{"code":"invalid_record"}}`))
		},
		"record code wrong status": func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(413)
			_, _ = w.Write([]byte(`{"error":{"code":"invalid_record"}}`))
		},
	}
	for name, respond := range responses {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.override = func(w http.ResponseWriter, _ *http.Request) { respond(w) }
			r.add(t, 1, goodPayload(t, 1))
			if o := r.drain(t); o != outcomePause60 {
				t.Fatalf("outcome %v, want a 60 s pause", o)
			}
			if st := queueStats(r); st.QueueRecords != 1 || st.QuarantineRecords != 0 {
				t.Fatalf("record was not kept: %+v", st)
			}
			if r.cnt.SendUnrecognised.Load() != 1 {
				t.Fatal("unrecognised-rejection counter not bumped")
			}
			lg := r.logged()
			if !strings.Contains(lg, "proxy") || strings.ContainsAny(lg, "\x00\x01") || strings.Contains(lg, token) || strings.Contains(lg, "torrent-1") {
				t.Fatalf("diagnostic unsuitable: %q", lg)
			}
		})
	}
}

func TestMultiRecordProxyRejectionBisectsThenKeepsAtOne(t *testing.T) {
	r := newRig(t)
	var sizes []int
	r.override = func(w http.ResponseWriter, req *http.Request) {
		var body struct{ Records []json.RawMessage }
		_ = json.NewDecoder(req.Body).Decode(&body)
		sizes = append(sizes, len(body.Records))
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(413) // a proxy that rejects every request
	}
	for i := 1; i <= 4; i++ {
		r.add(t, i, goodPayload(t, i))
	}
	if o := r.drain(t); o != outcomePause60 {
		t.Fatalf("outcome %v", o)
	}
	// 4 → 2 → 1, then kept at the first single record.
	if len(sizes) != 3 || sizes[0] != 4 || sizes[1] != 2 || sizes[2] != 1 {
		t.Fatalf("request sizes %v, want [4 2 1]", sizes)
	}
	if st := queueStats(r); st.QueueRecords != 4 || st.QuarantineRecords != 0 {
		t.Fatalf("stats %+v", st)
	}
}

func TestSystemicFailureGuardStopsQuarantining(t *testing.T) {
	r := newRig(t)
	// A server that blames every record.
	r.override = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"code":"invalid_record","retryable":false}}`))
	}
	for i := 1; i <= 12; i++ {
		r.add(t, i, goodPayload(t, i))
	}
	var last outcome
	for i := 0; i < 40; i++ {
		o, n := r.step(t)
		last = o
		if n == 0 || o != outcomeProgress {
			break
		}
	}
	if last != outcomePause60 {
		t.Fatalf("last outcome %v, want the guard pause", last)
	}
	st := queueStats(r)
	if st.QuarantineRecords != GuardLimit || st.QueueRecords != 12-GuardLimit {
		t.Fatalf("stats %+v: want exactly %d quarantined, the rest retained", st, GuardLimit)
	}
	if r.cnt.SendGuardTrips.Load() == 0 {
		t.Fatal("guard trip not counted")
	}
	// A 202 resets the guard.
	r.override = nil
	if r.drain(t); r.sender.consecutive != 0 {
		t.Fatalf("consecutive = %d after an acceptance", r.sender.consecutive)
	}
}

func TestAcknowledgementIsStatus202Only(t *testing.T) {
	bodies := map[string]struct {
		status int
		body   string
		acked  bool
	}{
		"202 empty":      {202, "", true},
		"202 garbled":    {202, "}{not json", true},
		"202 mismatched": {202, `{"accepted":999}`, true},
		"200":            {200, `{"accepted":1}`, false},
		"204":            {204, "", false},
		"301":            {301, "", false},
	}
	for name, tc := range bodies {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.override = func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}
			r.add(t, 1, goodPayload(t, 1))
			o, _ := r.step(t)
			acked := queueStats(r).QueueRecords == 0
			if acked != tc.acked {
				t.Fatalf("acked=%v want %v (outcome %v)", acked, tc.acked, o)
			}
			if !tc.acked && o != outcomeBackoff {
				t.Fatalf("outcome %v, want a retry backoff", o)
			}
		})
	}
}

func TestRetryableAndUnauthorizedKeepRows(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   outcome
	}{{500, outcomeBackoff}, {503, outcomeBackoff}, {429, outcomeBackoff}, {401, outcomeAuth}, {403, outcomeAuth}} {
		r := newRig(t)
		r.override = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(tc.status) }
		r.add(t, 1, goodPayload(t, 1))
		if o, _ := r.step(t); o != tc.want {
			t.Fatalf("status %d: outcome %v, want %v", tc.status, o, tc.want)
		}
		if st := queueStats(r); st.QueueRecords != 1 || st.QuarantineRecords != 0 {
			t.Fatalf("status %d dropped the record: %+v", tc.status, st)
		}
	}
	// Wrong token against the real server: 401, rows kept, token not logged.
	r := newRig(t)
	bad, _ := NewClient(r.ts.URL, "wrong-token-wrong-token-wrong-token-xyz", nil)
	r.sender.Client = bad
	r.add(t, 1, goodPayload(t, 1))
	if o, _ := r.step(t); o != outcomeAuth || strings.Contains(r.logged(), "wrong-token") {
		t.Fatalf("outcome %v logs %q", o, r.logged())
	}
}

// A lost 202 (the server committed but the response never arrived) is
// resolved by resending: idempotent, one copy.
func TestLostAcknowledgementResendsWithoutDuplicates(t *testing.T) {
	r := newRig(t)
	dropNext := true
	inner := r.ts.Config.Handler
	r.ts.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if dropNext {
			rec := &discard{}
			inner.ServeHTTP(rec, req) // committed on the server...
			dropNext = false
			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			conn.Close() // ...but the client never sees the 202
			return
		}
		inner.ServeHTTP(w, req)
	})
	r.add(t, 1, goodPayload(t, 1))
	if o, _ := r.step(t); o != outcomeBackoff {
		t.Fatalf("lost ack: outcome %v", o)
	}
	if queueStats(r).QueueRecords != 1 || !r.ingest.has(1) {
		t.Fatal("record must stay queued after a lost acknowledgement")
	}
	if o := r.drain(t); o != outcomeProgress || queueStats(r).QueueRecords != 0 || r.ingest.count() != 1 {
		t.Fatalf("resend: outcome %v stored=%d", o, r.ingest.count())
	}
}

type discard struct{ h http.Header }

func (d *discard) Header() http.Header {
	if d.h == nil {
		d.h = http.Header{}
	}
	return d.h
}
func (d *discard) Write(b []byte) (int, error) { return len(b), nil }
func (d *discard) WriteHeader(int)             {}

// Quarantined rows stay quarantined across a restart, and the remaining
// records restart from the head.
func TestCrashDuringBisectionKeepsQuarantineAndRestartsRest(t *testing.T) {
	r := newRig(t)
	for i := 1; i <= 8; i++ {
		if i == 3 {
			r.add(t, i, badPayload(i))
		} else {
			r.add(t, i, goodPayload(t, i))
		}
	}
	// Run the first half of the bisection only: deliver the head batch of 8
	// (rejected), then simulate a crash after the left half settled.
	items, _ := r.q.Peek(bg, SendMaxRecords, SendMaxBytes)
	left := items[:4]
	if o := r.sender.deliver(bg, left); o != outcomeProgress { // quarantines record 3, acks 1,2,4
		t.Fatalf("left half: %v", o)
	}
	// "Crash": reopen the queue without delivering the right half.
	if err := r.q.Close(); err != nil {
		t.Fatal(err)
	}
	q2, err := crawlqueue.Open(crawlqueue.Options{Dir: r.dir, Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q2.Close() })
	r.q = q2
	r.sender.Queue = q2
	if st := q2.Stats(); st.QuarantineRecords != 1 || st.QueueRecords != 4 {
		t.Fatalf("after restart: %+v", st)
	}
	if o := r.drain(t); o != outcomeProgress || q2.Stats().QueueRecords != 0 || r.ingest.count() != 7 {
		t.Fatalf("rest not delivered: outcome %v stored %d", o, r.ingest.count())
	}
}
