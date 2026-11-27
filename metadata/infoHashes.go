package metadata

import (
	"net"
	"sync"

	"github.com/skobkin/magnetico-web/internal/netpolicy"
)

// infoHashes holds the candidate peers of every in-flight leech.
//
// Lifecycle: an entry is created by register, which returns a token that
// identifies this registration. Exactly one goroutine (the leech's owner)
// holds the token; only it may take peers (pop) and it releases the entry
// exactly once (release). Both are no-ops for a token that is not the current
// registration, so a late call can never touch a newer registration of the
// same hash. Other goroutines can only add peers (push), and only to an entry
// that currently exists: a push never creates one.
type infoHashes struct {
	sync.RWMutex
	infoHashes  map[[20]byte]*ihEntry
	nextToken   uint64
	maxNLeeches int
	policy      *netpolicy.Policy
}

type ihEntry struct {
	token uint64
	peers []net.TCPAddr
}

// newInfoHashes builds the candidate store; a nil policy means
// netpolicy.Default().
func newInfoHashes(maxNLeeches int, policy *netpolicy.Policy) *infoHashes {
	if policy == nil {
		policy = netpolicy.Default()
	}
	return &infoHashes{
		infoHashes:  make(map[[20]byte]*ihEntry),
		maxNLeeches: maxNLeeches,
		policy:      policy,
	}
}

func (ih *infoHashes) isAllowed(peer net.TCPAddr) bool {
	if !ih.policy.AdmitLearned(peer.IP) {
		return false
	}

	// In allowlist mode (a private DHT) any port is accepted, as before.
	if ih.policy.AllowlistMode() {
		return true
	}

	if peer.Port < 1024 || peer.Port > 65535 {
		return false
	}

	return true
}

// push adds the allowed, non-duplicate peers to the entry of a registered
// hash and reports whether such an entry exists. It never creates one.
func (ih *infoHashes) push(infoHash [20]byte, peerAddresses []net.TCPAddr) bool {
	ih.Lock()
	defer ih.Unlock()

	e, ok := ih.infoHashes[infoHash]
	if !ok {
		return false
	}
	for _, addr := range peerAddresses {
		if !ih.isAllowed(addr) {
			continue
		}
		if len(e.peers) >= ih.maxNLeeches {
			break
		}
		if checkDuplicate(e.peers, addr) {
			continue
		}
		e.peers = append(e.peers, addr)
	}
	return true
}

func checkDuplicate(peerAddresses []net.TCPAddr, addr net.TCPAddr) bool {
	for _, existingAddr := range peerAddresses {
		if existingAddr.IP.Equal(addr.IP) &&
			existingAddr.Port == addr.Port {
			return true
		}
	}
	return false
}

// register creates the (initially empty) entry of a hash that is about to be
// leeched and returns the owner's token. It reports false, and creates
// nothing, when the hash is already registered.
func (ih *infoHashes) register(infoHash [20]byte) (token uint64, ok bool) {
	ih.Lock()
	defer ih.Unlock()
	if _, exists := ih.infoHashes[infoHash]; exists {
		return 0, false
	}
	ih.nextToken++
	ih.infoHashes[infoHash] = &ihEntry{token: ih.nextToken}
	return ih.nextToken, true
}

// pop returns the next candidate peer for the owner of token, or nil when
// there is none or the token is stale. It never removes the entry.
func (ih *infoHashes) pop(infoHash [20]byte, token uint64) *net.TCPAddr {
	ih.Lock()
	defer ih.Unlock()

	e, ok := ih.infoHashes[infoHash]
	if !ok || e.token != token || len(e.peers) == 0 {
		return nil
	}
	peerAddress := e.peers[0]
	e.peers = e.peers[1:]
	return &peerAddress
}

// release removes the entry registered under token. It is the only way an
// entry is removed and is a no-op for a stale token.
func (ih *infoHashes) release(infoHash [20]byte, token uint64) {
	ih.Lock()
	defer ih.Unlock()
	if e, ok := ih.infoHashes[infoHash]; ok && e.token == token {
		delete(ih.infoHashes, infoHash)
	}
}

// has reports whether a leech for infoHash is registered (in flight).
func (ih *infoHashes) has(infoHash [20]byte) bool {
	ih.RLock()
	defer ih.RUnlock()
	_, ok := ih.infoHashes[infoHash]
	return ok
}
