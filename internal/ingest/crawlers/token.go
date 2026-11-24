// Package crawlers is the registry of ingest crawlers (docs/specs/
// crawler-ingest-delivery-v1.md §5): server-generated stable identifiers and
// one revocable credential per crawler, stored as a SHA-256 digest in
// magnetico_ingest.crawlers. The credential is the only source of a crawler's
// identity.
package crawlers

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// TokenPrefix is cosmetic (a leaked token is recognisable); nothing routes on it.
const TokenPrefix = "mgc_"

// tokenBytes is the number of random bytes in a token.
const tokenBytes = 32

// Digest is the stored form of a credential: SHA-256 of the full presented
// string. The plaintext is never stored.
func Digest(presented string) [32]byte { return sha256.Sum256([]byte(presented)) }

func newToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crawlers: reading randomness: %w", err)
	}
	return TokenPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// newUUIDv4 returns a canonical lowercase RFC 4122 version-4 UUID.
func newUUIDv4() (string, error) {
	var u [16]byte
	if _, err := rand.Read(u[:]); err != nil {
		return "", fmt.Errorf("crawlers: reading randomness: %w", err)
	}
	u[6] = u[6]&0x0f | 0x40
	u[8] = u[8]&0x3f | 0x80
	h := hex.EncodeToString(u[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32], nil
}
