package codec

import (
	"fmt"
	"runtime"
	"sync"
)

// Encoder is a bounded, concurrency-safe encoder whose compressor state is
// reused across calls. At most `workers` Encode calls run concurrently; the
// rest block, which bounds both CPU work and per-worker memory (the Brotli
// q11 writer state is the largest fixed cost). After Close, Encode returns an
// error.
type Encoder struct {
	profile Profile
	limits  Limits

	sem    chan struct{}
	mu     sync.Mutex
	states sync.Pool
	done   bool
}

// NewEncoder returns an Encoder with the given worker bound; workers <= 0
// defaults to min(4, NumCPU).
func NewEncoder(profile Profile, limits Limits, workers int) *Encoder {
	if workers <= 0 {
		workers = min(4, runtime.NumCPU())
	}
	return &Encoder{
		profile: profile,
		limits:  limits,
		sem:     make(chan struct{}, workers),
		states: sync.Pool{
			New: func() any { return newCompressState(profile) },
		},
	}
}

// Encode serializes files exactly like the package-level Encode, reusing
// pooled compressor state.
func (e *Encoder) Encode(files []File) ([]byte, Selection, error) {
	e.mu.Lock()
	if e.done {
		e.mu.Unlock()
		return nil, Selection{}, fmt.Errorf("encoder is closed")
	}
	e.mu.Unlock()

	sorted, err := canonicalize(files, e.limits)
	if err != nil {
		return nil, Selection{}, err
	}

	e.sem <- struct{}{}
	defer func() { <-e.sem }()

	st := e.states.Get().(*compressState)
	defer e.states.Put(st)

	return encodeSelected(st, e.profile, sorted, e.limits)
}

// Close releases the encoder for further use; it does not free pooled states
// beyond letting the garbage collector reclaim them.
func (e *Encoder) Close() {
	e.mu.Lock()
	e.done = true
	e.mu.Unlock()
}
