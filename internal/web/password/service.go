package password

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/bcrypt"
)

const (
	saltLen = 16
	keyLen  = 32
)

// Params are the argon2id parameters used for new hashes.
type Params struct {
	Memory  uint32 // KiB
	Time    uint32
	Threads uint8
}

// CurrentParams is RFC 9106 §4's second recommended option (64 MiB, 3
// passes, 4 lanes) per spec §4.3.1. Benchmarking on the target host is a
// pre-release item, not part of this package.
var CurrentParams = Params{Memory: 65536, Time: 3, Threads: 4}

// ValidateNewPassword enforces the length rules for a password being set.
func ValidateNewPassword(pw string) error {
	switch {
	case len(pw) < MinNewLength:
		return ErrTooShort
	case len(pw) > MaxNewLength:
		return ErrTooLong
	}
	return nil
}

// Service hashes and verifies through one Limiter.
type Service struct {
	limiter *Limiter
	params  Params

	mu    sync.Mutex
	dummy string
}

// NewService panics on parameters outside the verifier bounds: a hash that
// this package could not verify must never be produced.
func NewService(l *Limiter, p Params) *Service {
	if l == nil || p.Memory > maxArgonMemoryKiB || p.Memory < 8*uint32(p.Threads) ||
		p.Time < 1 || p.Time > maxArgonTime || p.Threads < 1 || p.Threads > maxArgonThreads {
		panic("password: invalid limiter or parameters")
	}
	return &Service{limiter: l, params: p}
}

// Default is the process-wide service: the default limiter and CurrentParams.
var Default = NewService(NewLimiter(DefaultCapacity(), DefaultWait), CurrentParams)

// Limiter exposes the limiter for the busy counter.
func (s *Service) Limiter() *Limiter { return s.limiter }

// Hash returns a new argon2id PHC string. The password must already satisfy
// ValidateNewPassword.
func (s *Service) Hash(ctx context.Context, pw string) (string, error) {
	if err := ValidateNewPassword(pw); err != nil {
		return "", err
	}
	return s.hash(ctx, pw)
}

func (s *Service) hash(ctx context.Context, pw string) (string, error) {
	h := argonHash{id: true, memory: s.params.Memory, time: s.params.Time, threads: s.params.Threads,
		salt: make([]byte, saltLen)}
	if _, err := rand.Read(h.salt); err != nil {
		return "", err
	}
	if err := s.limiter.Do(ctx, func() {
		h.key = argon2.IDKey([]byte(pw), h.salt, h.time, h.memory, h.threads, keyLen)
	}); err != nil {
		return "", err
	}
	return h.encode(), nil
}

// Verify checks pw against a stored hash of any supported format. A wrong
// password is (false, nil). ErrUnsupportedHash means the account needs a
// reset; ErrTooLong means the input exceeded the login limit (nothing was
// hashed); ErrBusy and context errors mean no answer was computed.
func (s *Service) Verify(ctx context.Context, pw, hash string) (bool, error) {
	if len(pw) > MaxLoginLength {
		return false, ErrTooLong
	}
	if h, ok := parseArgon(hash); ok {
		var got []byte
		if err := s.limiter.Do(ctx, func() {
			if h.id {
				got = argon2.IDKey([]byte(pw), h.salt, h.time, h.memory, h.threads, uint32(len(h.key)))
			} else {
				got = argon2.Key([]byte(pw), h.salt, h.time, h.memory, h.threads, uint32(len(h.key)))
			}
		}); err != nil {
			return false, err
		}
		return subtle.ConstantTimeCompare(got, h.key) == 1, nil
	}
	if _, ok := bcryptCost(hash); ok {
		// PHP's crypt() sees a C string: it stops at NUL and ignores bytes
		// beyond 72. A NUL cannot have been in a password PHP hashed, so
		// it never matches rather than matching a different string.
		if strings.IndexByte(pw, 0) >= 0 {
			return false, nil
		}
		in := []byte(pw)
		if len(in) > bcryptMaxInput {
			in = in[:bcryptMaxInput]
		}
		var err error
		if lerr := s.limiter.Do(ctx, func() { err = bcrypt.CompareHashAndPassword([]byte(hash), in) }); lerr != nil {
			return false, lerr
		}
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, bcrypt.ErrMismatchedHashAndPassword):
			return false, nil
		default:
			return false, ErrUnsupportedHash
		}
	}
	return false, ErrUnsupportedHash
}

// DummyVerify spends the work of a real verification at current parameters
// (through the same limiter) so unknown, NULL-hash and inactive accounts
// cost the same as a wrong password (spec §4.3.6). It reports only
// ErrBusy or a context error; the comparison result is discarded.
func (s *Service) DummyVerify(ctx context.Context, pw string) error {
	s.mu.Lock()
	dummy := s.dummy
	s.mu.Unlock()
	if dummy == "" {
		var err error
		if dummy, err = s.hash(ctx, "dummy-password-that-matches-nobody"); err != nil {
			return err
		}
		s.mu.Lock()
		if s.dummy == "" {
			s.dummy = dummy
		}
		dummy = s.dummy
		s.mu.Unlock()
	}
	if len(pw) > MaxLoginLength {
		pw = pw[:MaxLoginLength]
	}
	_, err := s.Verify(ctx, pw, dummy)
	return err
}

// NeedsRehash is Params.Classify == ClassLegacy for this service.
func (s *Service) NeedsRehash(hash string) bool { return s.params.Classify(hash) == ClassLegacy }
