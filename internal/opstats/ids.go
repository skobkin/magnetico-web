package opstats

import (
	"crypto/rand"
	"encoding/hex"
)

// NewInstanceID returns a random canonical UUIDv4 identifying one ingest
// process for the lifetime of that process.
func NewInstanceID() string {
	var u [16]byte
	if _, err := rand.Read(u[:]); err != nil {
		panic("opstats: no randomness: " + err.Error())
	}
	u[6] = u[6]&0x0f | 0x40
	u[8] = u[8]&0x3f | 0x80
	h := hex.EncodeToString(u[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
