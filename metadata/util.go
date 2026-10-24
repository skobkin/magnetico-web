package metadata

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	mrand "math/rand/v2"
	"time"

	"github.com/skobkin/magnetico-web/bencode"
	"github.com/skobkin/magnetico-web/metainfo"
	"github.com/skobkin/magnetico-web/persistence"
)

// ErrV2OnlyUnsupported reports BEP 52 metadata that has a v2 file tree but
// no v1 representation. The v1-only pipeline cannot extract a correct file
// list from it; without the explicit guard such metadata used to slip
// through validation and persist as a zero-size single file.
var ErrV2OnlyUnsupported = errors.New("bep 52 v2-only metadata is not supported")

// ErrTotalSizeOverflow reports that the accumulated file sizes do not fit an
// uint64.
var ErrTotalSizeOverflow = errors.New("total file size overflows uint64")

func totalSize(files []persistence.File) (uint64, error) {
	var totalSize uint64
	if len(files) == 0 {
		return 0, errors.New("no files would be persisted")
	}

	for _, file := range files {
		if file.Size < 0 {
			return 0, errors.New("file size less than zero")
		}

		if totalSize > math.MaxUint64-uint64(file.Size) {
			return 0, fmt.Errorf("%w", ErrTotalSizeOverflow)
		}
		totalSize += uint64(file.Size)
	}
	return totalSize, nil
}

// Unmarshal the metainfo from the metadata
func unmarshalMetainfo(metadata []byte) (info *metainfo.Info, err error) {
	info = new(metainfo.Info)
	err = bencode.Unmarshal(metadata, info)
	if err != nil {
		info = nil
		return
	}

	err = validateInfo(info)
	if err != nil {
		info = nil
	}
	return
}

// Check the info dictionary
func validateInfo(info *metainfo.Info) error {
	// First check: without a v1 representation the pipeline below (which
	// reads info.Length/info.Files) would extract a wrong, zero-size file
	// list from v2-only metadata. See the v2/hybrid audit in the task 03
	// implementation report.
	if info.HasV2() && !info.HasV1() {
		return ErrV2OnlyUnsupported
	}
	if len(info.Pieces)%20 != 0 {
		return errors.New("pieces has invalid length")
	}
	if info.PieceLength == 0 {
		return errors.New("zero piece length")
	}
	if int((info.TotalLength()+info.PieceLength-1)/info.PieceLength) != info.NumPieces() {
		return errors.New("piece count and file lengths are at odds")
	}
	return nil
}

// Extract the files from the metainfo
func extractFiles(info *metainfo.Info) (files []persistence.File) {
	if len(info.Files) == 0 {
		// Single file
		files = append(files, persistence.File{
			Size: info.Length,
			Path: info.Name,
		})
		return
	}

	// Multiple files
	for _, file := range info.Files {
		files = append(files, persistence.File{
			Size: file.Length,
			Path: file.DisplayPath(info),
		})
	}
	return
}

// extractMetadata extracts metadata from a byte array and verifies it with an infohash.
// Returns a pointer to a Metadata structure and an error, if any.
//
// Parameters:
// - meta: a byte array containing the metadata to be extracted.
// - infohash: a 20-byte array representing the infohash for verification.
// - discovery: a timestamp representing the discovery time of the metadata.
//
// Returns:
// - A pointer to a Metadata structure containing the extracted and verified metadata.
// - An error if any validation or check does not complete with success.
func extractMetadata(meta []byte, infohash [20]byte, discovery time.Time) (*Metadata, error) {
	sha1Sum := sha1.Sum(meta)
	if !bytes.Equal(sha1Sum[:], infohash[:]) {
		return nil, errors.New("infohash mismatch")
	}

	info, err := unmarshalMetainfo(meta)
	if err != nil {
		return nil, err
	}

	files := extractFiles(info)
	totalSize, err := totalSize(files)
	if err != nil {
		return nil, err
	}

	return &Metadata{
		InfoHash:     infohash[:],
		Name:         info.Name,
		TotalSize:    totalSize,
		DiscoveredOn: discovery.Unix(),
		Files:        files,
	}, nil
}

// randomID generates a random peer ID with a predefined prefix.
// Returns a byte slice representing the generated peer ID.
func randomID() []byte {
	prefix := []byte(PeerPrefix)
	var rando []byte

	peace := PeerIDLength - len(prefix)
	for i := peace; i > 0; i-- {
		rando = append(rando, randomDigit())
	}

	return append(prefix, rando...)
}

// randomDigit as byte (ASCII code range 0-9 digits)
func randomDigit() byte {
	b := make([]byte, 1)
	_, err := rand.Read(b)
	if err != nil {
		b[0] = byte(mrand.IntN(256))
	}
	return (b[0] % 10) + '0'
}

func toBigEndian(i uint, n int) []byte {
	if n < 0 {
		// n must be positive
		return nil
	}

	b := make([]byte, n)
	switch n {
	case 1:
		b = []byte{byte(i)}

	case 2:
		binary.BigEndian.PutUint16(b, uint16(i))

	case 4:
		binary.BigEndian.PutUint32(b, uint32(i))

	default:
		// n must be 1, 2, or 4
		return nil
	}

	if len(b) != n {
		// postcondition failed: len(b) != n in intToBigEndian (i, n)
		return nil
	}

	return b
}
