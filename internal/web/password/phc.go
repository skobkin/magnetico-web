// Package password hashes and verifies web account passwords (spec web-v1
// §4.3): argon2id for new hashes, direct verification of the PHP-era legacy
// formats that have independently generated vectors (argon2i, argon2id with
// bounded parameters, bcrypt $2y$), and one process-wide limiter around all
// of it. Nothing here touches the database; callers hash outside transactions.
package password

import (
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
)

// Length rules (spec §4.3.2).
const (
	MinNewLength   = 8
	MaxNewLength   = 1024
	MaxLoginLength = 4096
)

// Parameter bounds a stored hash must respect to be verified at all. They
// keep a crafted m= value in the database from forcing a huge allocation:
// two verifications at the limit are 128 MiB of Argon2 blocks.
const (
	maxArgonMemoryKiB = 65536
	maxArgonTime      = 10
	maxArgonThreads   = 16
	minSaltLen        = 8
	maxSaltLen        = 64
	minKeyLen         = 16
	maxKeyLen         = 64
	minBcryptCost     = 4
	maxBcryptCost     = 16
	bcryptMaxInput    = 72

	// maxPHCLen bounds the whole encoded string before anything is split or
	// decoded. The longest valid form is
	// "$argon2id$v=19$m=65536,t=10,p=16$" (33) + 86 (64-byte salt) + "$" +
	// 86 (64-byte key) = 206 characters; 256 leaves headroom without
	// loosening any parameter bound (those are still checked below).
	maxPHCLen = 256
)

var (
	ErrTooShort        = errors.New("password is too short")
	ErrTooLong         = errors.New("password is too long")
	ErrUnsupportedHash = errors.New("stored password hash is not in a supported format")
	ErrBusy            = errors.New("password hashing capacity exhausted")
)

type argonHash struct {
	id      bool // argon2id, otherwise argon2i
	memory  uint32
	time    uint32
	threads uint8
	salt    []byte
	key     []byte
}

// parseArgon parses $argon2i|argon2id$v=19$m=..,t=..,p=..$salt$hash strictly
// and enforces the parameter bounds. ok is false for anything else.
func parseArgon(s string) (h argonHash, ok bool) {
	if len(s) > maxPHCLen {
		return h, false
	}
	parts := strings.Split(s, "$")
	if len(parts) != 6 || parts[0] != "" || parts[2] != "v=19" {
		return h, false
	}
	switch parts[1] {
	case "argon2id":
		h.id = true
	case "argon2i":
	default:
		return h, false
	}
	params := strings.Split(parts[3], ",")
	if len(params) != 3 {
		return h, false
	}
	var vals [3]uint64
	for i, key := range []string{"m=", "t=", "p="} {
		v, found := strings.CutPrefix(params[i], key)
		if !found || !canonicalUint(v) {
			return h, false
		}
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return h, false
		}
		vals[i] = n
	}
	m, t, p := vals[0], vals[1], vals[2]
	if p < 1 || p > maxArgonThreads || t < 1 || t > maxArgonTime || m < 8*p || m > maxArgonMemoryKiB {
		return h, false
	}
	salt, err1 := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	key, err2 := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err1 != nil || err2 != nil ||
		len(salt) < minSaltLen || len(salt) > maxSaltLen || len(key) < minKeyLen || len(key) > maxKeyLen {
		return h, false
	}
	h.memory, h.time, h.threads, h.salt, h.key = uint32(m), uint32(t), uint8(p), salt, key
	return h, true
}

// canonicalUint accepts plain decimal digits without a sign or leading zero.
func canonicalUint(s string) bool {
	if s == "" || len(s) > 10 || (len(s) > 1 && s[0] == '0') {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func (h argonHash) encode() string {
	enc := base64.RawStdEncoding
	alg := "argon2i"
	if h.id {
		alg = "argon2id"
	}
	return "$" + alg + "$v=19$m=" + strconv.FormatUint(uint64(h.memory), 10) +
		",t=" + strconv.FormatUint(uint64(h.time), 10) +
		",p=" + strconv.FormatUint(uint64(h.threads), 10) +
		"$" + enc.EncodeToString(h.salt) + "$" + enc.EncodeToString(h.key)
}

// bcryptCost returns the cost of a $2y$ hash, or ok=false when the string is
// not a well-formed, in-bounds bcrypt hash of that exact minor version.
func bcryptCost(s string) (cost int, ok bool) {
	// $2y$NN$ + 22 salt chars + 31 hash chars = 60.
	if len(s) != 60 || !strings.HasPrefix(s, "$2y$") || s[6] != '$' {
		return 0, false
	}
	if s[4] < '0' || s[4] > '9' || s[5] < '0' || s[5] > '9' {
		return 0, false
	}
	cost = int(s[4]-'0')*10 + int(s[5]-'0')
	if cost < minBcryptCost || cost > maxBcryptCost {
		return 0, false
	}
	for i := 7; i < 60; i++ {
		c := s[i]
		if c != '.' && c != '/' && (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return 0, false
		}
	}
	return cost, true
}
