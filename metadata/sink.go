package metadata

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/skobkin/magnetico-web/dht"
	"github.com/skobkin/magnetico-web/persistence"
)

const (
	// PeerIDLength The peer_id is exactly 20 bytes (characters) long.
	// https://wiki.theory.org/BitTorrentSpecification#peer_id
	PeerIDLength = 20
	// PeerPrefix Azureus-style
	PeerPrefix = "-UT3600-"

	// DefaultMaxActiveLeeches bounds simultaneously running leeches
	// (docs/specs/crawler-ingest-delivery-v1.md §8): worst-case in-flight
	// metadata memory is DefaultMaxActiveLeeches × MAX_METADATA_SIZE.
	DefaultMaxActiveLeeches = 32
	// drainCapacity is the fixed capacity of the result channel.
	drainCapacity = 16
)

type Metadata struct {
	InfoHash []byte
	// Name should be thought of "Title" of the torrent. For single-file torrents, it is the name
	// of the file, and for multi-file torrents, it is the name of the root directory.
	Name         string
	TotalSize    uint64
	DiscoveredOn int64
	// Files must be populated for both single-file and multi-file torrents!
	Files []persistence.File
}

// SinkStatus is the outcome of TrySink.
type SinkStatus int

const (
	// SinkStarted: a new leech was started for the hash.
	SinkStarted SinkStatus = iota
	// SinkJoined: a leech for the hash is already running; the new peers
	// were merged into its candidate list.
	SinkJoined
	// SinkBusy: the active-leech limit is reached; nothing was started.
	SinkBusy
	// SinkIgnored: no usable (allowed) peer address.
	SinkIgnored
	// SinkClosed: the sink was terminated.
	SinkClosed
)

// SinkOptions configures NewSinkWithOptions.
type SinkOptions struct {
	Deadline time.Duration
	// MaxPeersPerHash caps the candidate peers kept per info hash (the
	// historical "leech max n").
	MaxPeersPerHash int
	// MaxActive bounds concurrently running leeches (default 32).
	MaxActive   int
	FilterNodes []net.IPNet
}

// Sink fetches torrent metadata from peers. It is context-aware: Terminate
// cancels every running leech (their connections are closed), waits for all
// of them to return, and only then closes the Drain channel, so a leech can
// never send on a closed channel.
type Sink struct {
	PeerID   []byte
	deadline time.Duration
	drain    chan Metadata

	incomingInfoHashes *infoHashes
	sem                chan struct{}

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.Mutex
	closed bool

	droppedShutdown atomic.Uint64
}

// NewSink keeps the historical signature; the active-leech limit is
// DefaultMaxActiveLeeches.
func NewSink(deadline time.Duration, maxNLeeches int, filterNodes []net.IPNet) *Sink {
	return NewSinkWithOptions(SinkOptions{Deadline: deadline, MaxPeersPerHash: maxNLeeches, FilterNodes: filterNodes})
}

// NewSinkWithOptions builds a Sink.
func NewSinkWithOptions(o SinkOptions) *Sink {
	if o.MaxActive <= 0 {
		o.MaxActive = DefaultMaxActiveLeeches
	}
	ms := new(Sink)
	ms.PeerID = randomID()
	ms.deadline = o.Deadline
	ms.drain = make(chan Metadata, drainCapacity)
	ms.incomingInfoHashes = newInfoHashes(o.MaxPeersPerHash, o.FilterNodes)
	ms.sem = make(chan struct{}, o.MaxActive)
	ms.ctx, ms.cancel = context.WithCancel(context.Background())
	return ms
}

// Active is the number of running leeches.
func (ms *Sink) Active() int { return len(ms.sem) }

// DroppedOnShutdown counts results discarded because the sink was terminating
// while the drain channel had no reader capacity.
func (ms *Sink) DroppedOnShutdown() uint64 { return ms.droppedShutdown.Load() }

// Sink is TrySink with the outcome ignored.
func (ms *Sink) Sink(res dht.Result) { ms.TrySink(res) }

// TrySink never blocks. The address policy applies to every peer including
// the first; a hash that is already being leeched only gains peers.
func (ms *Sink) TrySink(res dht.Result) SinkStatus {
	infoHash := res.InfoHash()
	peerAddrs := res.PeerAddrs()
	if len(peerAddrs) == 0 {
		return SinkIgnored
	}

	ms.mu.Lock()
	defer ms.mu.Unlock()
	if ms.closed {
		return SinkClosed
	}
	if ms.incomingInfoHashes.has(infoHash) {
		ms.incomingInfoHashes.push(infoHash, peerAddrs)
		return SinkJoined
	}

	first := -1
	for i, a := range peerAddrs {
		if ms.incomingInfoHashes.isAllowed(a) {
			first = i
			break
		}
	}
	if first < 0 {
		return SinkIgnored
	}
	select {
	case ms.sem <- struct{}{}:
	default:
		return SinkBusy
	}
	ms.incomingInfoHashes.register(infoHash)
	ms.incomingInfoHashes.push(infoHash, peerAddrs[first+1:])
	ms.wg.Add(1)
	go ms.run(infoHash, peerAddrs[first])
	return SinkStarted
}

// run owns one hash: it tries the first peer, then the remaining candidates
// one at a time, until a leech succeeds, the candidates run out, or the sink
// is terminated.
func (ms *Sink) run(infoHash [20]byte, peer net.TCPAddr) {
	defer ms.wg.Done()
	defer func() { <-ms.sem }()
	defer ms.incomingInfoHashes.flush(infoHash)

	next := &peer
	for next != nil && ms.ctx.Err() == nil {
		var result *Metadata
		NewLeech(infoHash, next, ms.PeerID, LeechEventHandlers{
			OnSuccess: func(m Metadata) { result = &m },
			OnError:   func([20]byte, error) {},
		}).DoContext(ms.ctx, time.Now().Add(ms.deadline))
		if result != nil {
			ms.deliver(*result)
			return
		}
		next = ms.incomingInfoHashes.pop(infoHash)
	}
}

// deliver hands a result to the drain channel. It never blocks past
// termination: a result that cannot be handed over once the sink is
// terminating is counted and dropped.
func (ms *Sink) deliver(m Metadata) {
	select {
	case ms.drain <- m:
		return
	default:
	}
	select {
	case ms.drain <- m:
	case <-ms.ctx.Done():
		ms.droppedShutdown.Add(1)
	}
}

// Drain returns the result channel; it is closed by Terminate after every
// leech has returned.
func (ms *Sink) Drain() <-chan Metadata { return ms.drain }

// Terminate stops accepting work, cancels all leeches, waits for them and
// closes the drain channel. It is idempotent. Keep reading Drain while it
// runs to avoid dropping results that are still arriving.
func (ms *Sink) Terminate() {
	ms.mu.Lock()
	already := ms.closed
	ms.closed = true
	ms.mu.Unlock()
	if already {
		ms.wg.Wait()
		return
	}
	ms.cancel()
	ms.wg.Wait()
	close(ms.drain)
}
