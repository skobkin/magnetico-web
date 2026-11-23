package users

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"time"
)

// hasher builds a SHA-256 over a canonical, length-prefixed encoding so that
// no two different field sequences can collide by concatenation.
type hasher struct{ h hash.Hash }

func newHasher(tag string) *hasher {
	h := &hasher{h: sha256.New()}
	h.str(tag)
	return h
}

func (h *hasher) u64(v uint64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	h.h.Write(b[:])
}

func (h *hasher) i64(v int64) { h.u64(uint64(v)) }

func (h *hasher) str(s string) {
	h.u64(uint64(len(s)))
	h.h.Write([]byte(s))
}

func (h *hasher) bytes(b []byte) {
	h.u64(uint64(len(b)))
	h.h.Write(b)
}

func (h *hasher) opt(p *int64) {
	if p == nil {
		h.u64(0)
		return
	}
	h.u64(1)
	h.i64(*p)
}

func (h *hasher) time(t time.Time) { h.str(t.UTC().Format(time.RFC3339Nano)) }

func (h *hasher) sum() string { return hex.EncodeToString(h.h.Sum(nil)) }
