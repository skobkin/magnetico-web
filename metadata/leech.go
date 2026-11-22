package metadata

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/skobkin/magnetico-web/bencode"
	"github.com/skobkin/magnetico-web/metadata/btconn"
	"github.com/skobkin/magnetico-web/stats"
)

const MAX_METADATA_SIZE = 10 * 1024 * 1024

// metadataPieceSize is the fixed size of a metadata piece, 16 KiB, as
// mandated by BEP 9 (only the last piece may be shorter).
const metadataPieceSize = 1 << 14

// nMetadataPieces returns the number of pieces in a metadata blob of the
// given size (integer ceiling division; no floating point).
func nMetadataPieces(size uint) int {
	return int((size + metadataPieceSize - 1) / metadataPieceSize)
}

// Bounds for peer-controlled input read before any metadata is verified.
const (
	// MaxPeerMessageSize bounds one BitTorrent wire message. The largest
	// legitimate message other than a metadata piece is a bitfield (1 MiB covers
	// 8M pieces); metadata itself is carried in 16 KiB pieces.
	MaxPeerMessageSize = 1 << 20
	// maxExHandshakeSize bounds the payload of the extension handshake (BEP 10),
	// normally well under 1 KiB.
	maxExHandshakeSize = 16 << 10
	// maxUmHeaderSize bounds the bencoded header that precedes the piece data in
	// a ut_metadata message (BEP 9: msg_type, piece, total_size).
	maxUmHeaderSize = 1 << 10
	// MaxExtensionNesting bounds list/dictionary nesting in the extension
	// handshake and ut_metadata headers (legitimate depth is 2). The decoder
	// recurses per level and a Go stack overflow is fatal, so it is checked
	// before decoding.
	MaxExtensionNesting = 16
)

type rootDict struct {
	M            mDict `bencode:"m"`
	MetadataSize int   `bencode:"metadata_size"`
}

type mDict struct {
	UTMetadata int `bencode:"ut_metadata"`
}

type extDict struct {
	MsgType int `bencode:"msg_type"`
	Piece   int `bencode:"piece"`
}

type Leech struct {
	infoHash [20]byte
	peerAddr *net.TCPAddr
	ev       LeechEventHandlers

	conn     net.Conn
	clientID [20]byte

	ut_metadata                    uint8
	metadataReceived, metadataSize uint
	metadata                       []byte
	piecesReceived                 []bool

	connClosed bool
}

type LeechEventHandlers struct {
	OnSuccess func(Metadata)        // must be supplied. args: metadata
	OnError   func([20]byte, error) // must be supplied. args: infohash, error
}

func NewLeech(infoHash [20]byte, peerAddr *net.TCPAddr, clientID []byte, ev LeechEventHandlers) *Leech {
	l := new(Leech)
	l.infoHash = infoHash
	l.peerAddr = peerAddr
	copy(l.clientID[:], clientID)
	l.ev = ev

	return l
}

func (l *Leech) writeAll(b []byte) error {
	for len(b) != 0 {
		n, err := l.conn.Write(b)
		if err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}

func (l *Leech) readExactly(n uint) ([]byte, error) {
	b := make([]byte, n)
	_, err := io.ReadFull(l.conn, b)
	return b, err
}

func (l *Leech) closeConn() {
	if l.connClosed {
		return
	}

	// A cancellation watcher may have closed the connection already.
	if err := l.conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		panic("couldn't close leech connection! " + err.Error())
	}

	l.connClosed = true
}

func (l *Leech) OnError(err error) {
	l.ev.OnError(l.infoHash, err)
}

func (l *Leech) doExHandshake() error {
	err := l.writeAll([]byte("\x00\x00\x00\x1a\x14\x00d1:md11:ut_metadatai1eee"))
	if err != nil {
		return errors.New("writeAll lHandshake " + err.Error())
	}

	rExMessage, err := l.readExMessage()
	if err != nil {
		return errors.New("readExMessage " + err.Error())
	}

	// Extension Handshake has the Extension Message ID = 0x00
	if rExMessage[1] != 0 {
		return errors.New("first extension message is not an extension handshake")
	}

	if len(rExMessage)-2 > maxExHandshakeSize {
		return errors.New("extension handshake too large")
	}
	if err = bencode.CheckDepth(rExMessage[2:], MaxExtensionNesting); err != nil {
		return errors.New("extension handshake " + err.Error())
	}

	rRootDict := new(rootDict)
	err = bencode.Unmarshal(rExMessage[2:], rRootDict)
	if err != nil {
		return errors.New("unmarshal rExMessage " + err.Error())
	}

	if rRootDict.MetadataSize <= 0 || rRootDict.MetadataSize >= MAX_METADATA_SIZE {
		return fmt.Errorf("metadata too big or its size is less than or equal zero")
	}

	if rRootDict.M.UTMetadata <= 0 || rRootDict.M.UTMetadata >= 255 {
		return fmt.Errorf("ut_metadata is not an uint8")
	}

	l.ut_metadata = uint8(rRootDict.M.UTMetadata) // Save the ut_metadata code the remote peer uses
	l.metadataSize = uint(rRootDict.MetadataSize)
	l.metadata = make([]byte, l.metadataSize)
	l.piecesReceived = make([]bool, nMetadataPieces(l.metadataSize))

	return nil
}

func (l *Leech) requestAllPieces() error {
	// Request all the pieces of metadata
	nPieces := nMetadataPieces(l.metadataSize)
	if nPieces == 0 {
		return errors.New("metadataSize is zero")
	}

	for piece := range nPieces {
		// __request_metadata_piece(piece)
		// ...............................
		extDictDump, err := bencode.Marshal(extDict{
			MsgType: 0,
			Piece:   piece,
		})
		if err != nil { // ASSERT
			return errors.New("marshal extDict " + err.Error())
		}

		err = l.writeAll(fmt.Appendf(nil,
			"%s\x14%s%s",
			toBigEndian(uint(2+len(extDictDump)), 4),
			toBigEndian(uint(l.ut_metadata), 1),
			extDictDump,
		))
		if err != nil {
			return errors.New("writeAll piece request " + err.Error())
		}
	}

	return nil
}

// applyMetadataPiece validates one ut_metadata data message and, if it is
// well-formed and new, copies it into the assembled metadata.
//
// Validation happens strictly before any index arithmetic: an out-of-range
// piece index must never reach a slice expression, where it could panic or —
// by wrapping — silently corrupt the buffer.
//
// The expected piece length is derived from the piece index (remaining bytes,
// clamped to 16 KiB), so only the final piece may be shorter than 16 KiB,
// regardless of its arrival order.
//
// Duplicate pieces are ignored (first wins): BEP 9 does not forbid
// retransmission, and the SHA-1 verification in extractMetadata remains the
// final arbiter of correctness. Accounting stays exact because duplicates
// never contribute to metadataReceived.
func (l *Leech) applyMetadataPiece(piece int, data []byte) error {
	// Defensive fallback for direct callers (tests) that did not go through
	// doExHandshake; the production path always allocates both there.
	if l.metadata == nil {
		l.metadata = make([]byte, l.metadataSize)
	}
	if l.piecesReceived == nil {
		l.piecesReceived = make([]bool, nMetadataPieces(l.metadataSize))
	}

	nPieces := nMetadataPieces(l.metadataSize)
	if piece < 0 || piece >= nPieces {
		return fmt.Errorf("piece index %d out of range [0, %d)", piece, nPieces)
	}

	remaining := int(l.metadataSize) - piece*metadataPieceSize
	expected := min(remaining, metadataPieceSize)
	if len(data) != expected {
		return fmt.Errorf("piece %d has length %d, expected %d", piece, len(data), expected)
	}

	if l.piecesReceived[piece] {
		return nil
	}

	offset := piece * metadataPieceSize
	copy(l.metadata[offset:offset+len(data)], data)
	l.piecesReceived[piece] = true
	l.metadataReceived += uint(len(data))

	return nil
}

// readMessage returns a BitTorrent message, sans the first 4 bytes indicating its length.
func (l *Leech) readMessage() ([]byte, error) {
	rLengthB, err := l.readExactly(4)
	if err != nil {
		return nil, errors.New("readExactly rLengthB " + err.Error())
	}

	rLength := uint(binary.BigEndian.Uint32(rLengthB))

	// Some malicious/faulty peers say that they are sending a very long
	// message, and hence causing us to run out of memory.
	// This is a crude check that does not let it happen (i.e. boundary can probably be
	// tightened a lot more.)
	if rLength > MaxPeerMessageSize {
		return nil, errors.New("message is longer than the maximum peer message size")
	}

	rMessage, err := l.readExactly(rLength)
	if err != nil {
		return nil, errors.New("readExactly rMessage " + err.Error())
	}

	return rMessage, nil
}

// readExMessage returns an *extension* message, sans the first 4 bytes indicating its length.
//
// It will IGNORE all non-extension messages!
func (l *Leech) readExMessage() ([]byte, error) {
	for {
		rMessage, err := l.readMessage()
		if err != nil {
			return nil, errors.New("readMessage " + err.Error())
		}

		// Every extension message has at least 2 bytes.
		if len(rMessage) < 2 {
			continue
		}

		// We are interested only in extension messages, whose first byte is always 20
		if rMessage[0] == 20 {
			return rMessage, nil
		}
	}
}

// readUmMessage returns an ut_metadata extension message, sans the first 4 bytes indicating its
// length.
//
// It will IGNORE all non-"ut_metadata extension" messages!
func (l *Leech) readUmMessage() ([]byte, error) {
	for {
		rExMessage, err := l.readExMessage()
		if err != nil {
			return nil, errors.New("readExMessage " + err.Error())
		}

		if rExMessage[1] == 0x01 {
			return rExMessage, nil
		}
	}
}

// decodeUmHeader decodes the bencoded header of a ut_metadata message
// (payload after the two extension bytes) and returns the raw piece data that
// follows it. Everything here is peer-controlled: size and nesting are bounded
// before the recursive decoder runs. CheckDepth scans only the first value,
// i.e. the header; the piece data is never interpreted as bencode.
func decodeUmHeader(payload []byte) (*extDict, []byte, error) {
	if len(payload) > metadataPieceSize+maxUmHeaderSize {
		return nil, nil, errors.New("ut_metadata message too large")
	}
	if err := bencode.CheckDepth(payload, MaxExtensionNesting); err != nil {
		return nil, nil, errors.New("ut_metadata header " + err.Error())
	}
	buf := bytes.NewBuffer(payload)
	d := new(extDict)
	if err := bencode.NewDecoder(buf).Decode(d); err != nil {
		return nil, nil, errors.New("could not decode ext msg in the loop " + err.Error())
	}
	return d, buf.Bytes(), nil
}

func (l *Leech) Do(deadline time.Time) {
	l.DoContext(context.Background(), deadline)
}

// DoContext runs the leech until the deadline or until ctx ends; on
// cancellation the connection is closed, which unblocks every pending read.
func (l *Leech) DoContext(ctx context.Context, deadline time.Time) {
	conn, _, peerExtensions, _, err := btconn.DialContext(
		ctx,
		l.peerAddr,
		deadline,
		[8]byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x10, 0x00, 0x01},
		l.infoHash,
		l.clientID,
	)
	if err != nil {
		l.OnError(errors.New("btconn.Dial " + err.Error()))
		return
	}

	l.conn = conn
	defer l.closeConn()
	stopWatch := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopWatch()
	go stats.GetInstance().IncLeech(peerExtensions)

	err = l.doExHandshake()
	if err != nil {
		l.OnError(errors.New("doExHandshake " + err.Error()))
		return
	}

	err = l.requestAllPieces()
	if err != nil {
		l.OnError(errors.New("requestAllPieces " + err.Error()))
		return
	}

	for l.metadataReceived < l.metadataSize {
		rUmMessage, err := l.readUmMessage()
		if err != nil {
			l.OnError(errors.New("readUmMessage " + err.Error()))
			return
		}

		rExtDict, pieceData, err := decodeUmHeader(rUmMessage[2:])
		if err != nil {
			l.OnError(err)
			return
		}

		if rExtDict.MsgType == 2 { // reject
			l.OnError(fmt.Errorf("remote peer rejected sending metadata"))
			return
		}

		if rExtDict.MsgType == 1 { // data
			if err := l.applyMetadataPiece(rExtDict.Piece, pieceData); err != nil {
				l.OnError(fmt.Errorf("invalid metadata piece: %w", err))
				return
			}
		}
	}

	// We are done with the transfer, close socket as soon as possible (i.e. NOW)
	// Avoid hitting "too many open files" error
	l.closeConn()

	extracted, err := extractMetadata(l.metadata, l.infoHash, time.Now())
	if err != nil {
		l.OnError(err)
		return
	}

	l.ev.OnSuccess(*extracted)
}
