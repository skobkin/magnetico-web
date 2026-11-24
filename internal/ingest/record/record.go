// Package record defines the metadata record format v1 shared by the crawler
// queue, the HTTP ingest protocol and the server-side queue
// (docs/specs/crawler-ingest-delivery-v1.md §3).
//
// One record is one JSON object; the same bytes are stored locally, sent over
// HTTP and stored in the PostgreSQL queue:
//
//	{"v":1,"info_hash":"<40 lowercase hex>","name":"…","total_size":N,
//	 "discovered_on":N,"files":[{"path":"…","size":N}]}
//
// Encoding is lossless because persistence.ValidateIngest guarantees valid,
// NUL-free UTF-8 before marshalling (Go's encoder would otherwise silently
// replace invalid bytes with U+FFFD). Decoding is strict.
package record

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"unicode/utf8"

	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/metadata"
	"github.com/skobkin/magnetico-web/persistence"
)

const (
	// Version is the only record version this package understands.
	Version = 1

	// MaxRecordBytes bounds the serialized JSON of one record (16 MiB). There
	// is no guaranteed relation to the size of the bencoded metadata (≤ 10
	// MiB): JSON can be smaller or larger (control characters escape to
	// \u00XX, up to 6×), so this explicit limit is the only contract.
	MaxRecordBytes = 16 << 20

	infoHashHexLen = 40
)

var (
	// ErrRecordTooLarge reports a record whose JSON exceeds MaxRecordBytes.
	ErrRecordTooLarge = errors.New("record: serialized record exceeds the size limit")
	// ErrUnsupportedVersion reports a well-formed record with a "v" other
	// than Version.
	ErrUnsupportedVersion = errors.New("record: unsupported record version")
	// ErrInvalidRecord reports a record that is not valid v1 JSON or fails
	// ingest validation. Validation failures additionally wrap
	// persistence.ErrInvalidMetadata.
	ErrInvalidRecord = errors.New("record: invalid record")
)

// Record is a decoded, validated metadata record.
type Record struct {
	InfoHash     [20]byte
	Name         string
	TotalSize    int64
	DiscoveredOn int64
	Files        []persistence.File
}

// wire is the JSON shape. Pointers make every field required: a missing or
// null member decodes to nil and is rejected.
type wire struct {
	V            int                `json:"v"`
	InfoHash     string             `json:"info_hash"`
	Name         string             `json:"name"`
	TotalSize    int64              `json:"total_size"`
	DiscoveredOn int64              `json:"discovered_on"`
	Files        []persistence.File `json:"files"`
}

type wireIn struct {
	V            *int      `json:"v"`
	InfoHash     *string   `json:"info_hash"`
	Name         *string   `json:"name"`
	TotalSize    *int64    `json:"total_size"`
	DiscoveredOn *int64    `json:"discovered_on"`
	Files        *[]fileIn `json:"files"`
}

type fileIn struct {
	Path *string `json:"path"`
	Size *int64  `json:"size"`
}

// FromMetadata converts a drained crawler record. The uint64→int64 boundary
// check lives here, before any conversion, so an oversized total size can
// never wrap into a negative value.
func FromMetadata(md *metadata.Metadata) (Record, error) {
	if md.TotalSize > math.MaxInt64 {
		return Record{}, fmt.Errorf(
			"%w: total size %d exceeds the maximum supported value",
			persistence.ErrInvalidMetadata, md.TotalSize)
	}
	if len(md.InfoHash) != 20 {
		return Record{}, fmt.Errorf("%w: infohash must be exactly 20 bytes, got %d",
			persistence.ErrInvalidMetadata, len(md.InfoHash))
	}
	r := Record{
		Name:         md.Name,
		TotalSize:    int64(md.TotalSize),
		DiscoveredOn: md.DiscoveredOn,
		Files:        md.Files,
	}
	copy(r.InfoHash[:], md.InfoHash)
	return r, nil
}

// Torrent returns the persistence view of the record.
func (r Record) Torrent() persistence.Torrent {
	return persistence.Torrent{
		InfoHash:     append([]byte(nil), r.InfoHash[:]...),
		Name:         r.Name,
		TotalSize:    r.TotalSize,
		DiscoveredOn: r.DiscoveredOn,
	}
}

// Validate applies persistence.ValidateIngest. Failures wrap both
// ErrInvalidRecord and persistence.ErrInvalidMetadata.
func (r Record) Validate(limits codec.Limits) error {
	if err := persistence.ValidateIngest(r.Torrent(), r.Files, limits); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRecord, err)
	}
	return nil
}

// Encode validates the record and returns its JSON. If the JSON exceeds
// MaxRecordBytes it returns the bytes together with ErrRecordTooLarge so the
// caller can quarantine the intact payload; the bytes are never truncated.
func Encode(r Record, limits codec.Limits) ([]byte, error) {
	if err := r.Validate(limits); err != nil {
		return nil, err
	}
	w := wire{
		V:            Version,
		InfoHash:     hex.EncodeToString(r.InfoHash[:]),
		Name:         r.Name,
		TotalSize:    r.TotalSize,
		DiscoveredOn: r.DiscoveredOn,
		Files:        r.Files,
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(&w); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRecord, err)
	}
	out := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	if len(out) > MaxRecordBytes {
		return out, ErrRecordTooLarge
	}
	return out, nil
}

// Decode strictly parses and validates one record. Errors, in precedence
// order: ErrRecordTooLarge, ErrUnsupportedVersion, ErrInvalidRecord.
func Decode(data []byte, limits codec.Limits) (Record, error) {
	if len(data) > MaxRecordBytes {
		return Record{}, ErrRecordTooLarge
	}
	// Go's decoder replaces invalid UTF-8 (and lone surrogate escapes) with
	// U+FFFD; that would silently alter the record, so reject them first.
	if !utf8.Valid(data) {
		return Record{}, fmt.Errorf("%w: input is not valid UTF-8", ErrInvalidRecord)
	}
	if hasLoneSurrogateEscape(data) {
		return Record{}, fmt.Errorf("%w: unpaired surrogate escape", ErrInvalidRecord)
	}

	if err := checkVersion(data); err != nil {
		return Record{}, err
	}

	var in wireIn
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return Record{}, fmt.Errorf("%w: %w", ErrInvalidRecord, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Record{}, fmt.Errorf("%w: trailing data after the record", ErrInvalidRecord)
	}
	if in.InfoHash == nil || in.Name == nil || in.TotalSize == nil ||
		in.DiscoveredOn == nil || in.Files == nil {
		return Record{}, fmt.Errorf("%w: missing required field", ErrInvalidRecord)
	}

	var r Record
	h := *in.InfoHash
	if len(h) != infoHashHexLen || !isLowerHex(h) {
		return Record{}, fmt.Errorf("%w: info_hash must be 40 lowercase hex characters", ErrInvalidRecord)
	}
	if _, err := hex.Decode(r.InfoHash[:], []byte(h)); err != nil {
		return Record{}, fmt.Errorf("%w: info_hash: %w", ErrInvalidRecord, err)
	}
	r.Name = *in.Name
	r.TotalSize = *in.TotalSize
	r.DiscoveredOn = *in.DiscoveredOn
	r.Files = make([]persistence.File, len(*in.Files))
	for i, f := range *in.Files {
		if f.Path == nil || f.Size == nil {
			return Record{}, fmt.Errorf("%w: file %d is missing a required field", ErrInvalidRecord, i)
		}
		r.Files[i] = persistence.File{Path: *f.Path, Size: *f.Size}
	}
	if err := r.Validate(limits); err != nil {
		return Record{}, err
	}
	return r, nil
}

// checkVersion reads only "v" (ignoring other members) so a future record
// with additional fields is reported as an unsupported version instead of an
// unknown-field error.
func checkVersion(data []byte) error {
	var probe struct {
		V json.RawMessage `json:"v"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRecord, err)
	}
	if len(probe.V) == 0 {
		return fmt.Errorf("%w: missing version", ErrInvalidRecord)
	}
	var v int64
	if err := json.Unmarshal(probe.V, &v); err != nil {
		return fmt.Errorf("%w: version is not an integer", ErrInvalidRecord)
	}
	if v != Version {
		return fmt.Errorf("%w: %d", ErrUnsupportedVersion, v)
	}
	return nil
}

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// hasLoneSurrogateEscape reports a \uD800-\uDFFF escape that is not part of
// a valid high+low pair.
func hasLoneSurrogateEscape(data []byte) bool {
	for i := 0; i < len(data); i++ {
		if data[i] != '\\' || i+1 >= len(data) {
			continue
		}
		if data[i+1] != 'u' {
			i++ // skip the escaped character (including an escaped backslash)
			continue
		}
		cp, ok := parseHex4(data, i+2)
		if !ok {
			return false // malformed escape: the JSON decoder rejects it
		}
		i += 5
		switch {
		case cp >= 0xD800 && cp < 0xDC00: // high surrogate: needs a low one next
			if i+6 < len(data) && data[i+1] == '\\' && data[i+2] == 'u' {
				if lo, ok := parseHex4(data, i+3); ok && lo >= 0xDC00 && lo < 0xE000 {
					i += 6
					continue
				}
			}
			return true
		case cp >= 0xDC00 && cp < 0xE000: // low surrogate without a high one
			return true
		}
	}
	return false
}

func parseHex4(data []byte, at int) (rune, bool) {
	if at+4 > len(data) {
		return 0, false
	}
	var v rune
	for _, c := range data[at : at+4] {
		switch {
		case c >= '0' && c <= '9':
			v = v<<4 | rune(c-'0')
		case c >= 'a' && c <= 'f':
			v = v<<4 | rune(c-'a'+10)
		case c >= 'A' && c <= 'F':
			v = v<<4 | rune(c-'A'+10)
		default:
			return 0, false
		}
	}
	return v, true
}

// CheckVersion reports ErrUnsupportedVersion (or ErrInvalidRecord for a
// missing/unreadable "v") without applying any other rule, regardless of the
// record size. The ingest service uses it to honour the documented
// precedence unsupported_version > record_too_large > invalid_record.
func CheckVersion(data []byte) error { return checkVersion(data) }
