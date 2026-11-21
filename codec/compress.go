package codec

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// compressState holds reusable compressor state for one worker. A state must
// be used by a single goroutine at a time; Encoder hands states out
// exclusively.
//
// Determinism invariant (wire-format-v1 §4): for the same payload, profile
// and library version, compress returns the same bytes regardless of what
// the state compressed before. The Zstd encoder and the output buffer are
// always reused. Brotli writer reuse is restricted:
//
// brotli.Writer.Reset clears the stream size hint but keeps the hasher
// object, and at quality <= 9 the hasher type is chosen from that size hint
// on first use (h5 below 1 MiB, h6 from 1 MiB). A reused writer therefore
// compresses a payload differently depending on the size class of the first
// payload it saw. A fresh writer allocates tens of MiB of hasher tables, a
// cost that dominates the encode time of small lists.
//
// So the pooled writer (benc) is reused only for payloads below
// brotliReuseMaxPayload: a single input block, far under the hasher switch,
// so every stream it serves selects the same hasher and has no cross-block
// state. Any larger payload gets a fresh writer and never touches benc.
type compressState struct {
	// quality is the Brotli quality used by this profile's candidates.
	quality int
	// usesZstd/usesBrotli reflect the profile's candidate set.
	usesZstd, usesBrotli bool

	zenc *zstd.Encoder
	benc *brotli.Writer // reused only for payloads < brotliReuseMaxPayload
	buf  bytes.Buffer
}

// brotliReuseMaxPayload bounds the payload size (exclusive) for which the
// pooled Brotli writer is reused. It equals the Brotli input block size at
// quality >= 9 (lgblock 18), so a reused writer only ever compresses a
// single block, and sits 4x below the 1 MiB hasher-selection threshold of
// brotli v1.2.6. Changing it, or upgrading brotli, requires re-running the
// reuse tests in determinism_test.go.
const brotliReuseMaxPayload = 1 << 18

func newCompressState(p Profile) *compressState {
	s := &compressState{quality: p.brotliQuality()}
	for _, c := range p.candidates() {
		switch c.comp {
		case Zstd:
			s.usesZstd = true
		case Brotli:
			s.usesBrotli = true
		}
	}
	return s
}

// compress compresses payload with the given compressor, reusing the state's
// Zstd encoder and output buffer (Brotli reuses its writer only below
// brotliReuseMaxPayload). The returned slice aliases the state's internal
// buffer and is only valid until the next compress call on the same state.
func (s *compressState) compress(comp CompressorID, payload []byte) ([]byte, error) {
	s.buf.Reset()

	switch comp {
	case Zstd:
		if s.zenc == nil {
			enc, err := zstd.NewWriter(&s.buf,
				zstd.WithEncoderLevel(zstd.SpeedDefault),
				zstd.WithEncoderConcurrency(1))
			if err != nil {
				return nil, fmt.Errorf("zstd encoder: %w", err)
			}
			s.zenc = enc
		} else {
			s.zenc.Reset(&s.buf)
		}
		if _, err := s.zenc.Write(payload); err != nil {
			return nil, fmt.Errorf("zstd write: %w", err)
		}
		if err := s.zenc.Close(); err != nil {
			return nil, fmt.Errorf("zstd close: %w", err)
		}

	case Brotli:
		// Reuse the pooled writer only below the gate; see the determinism
		// invariant on compressState.
		var bw *brotli.Writer
		if len(payload) < brotliReuseMaxPayload {
			if s.benc == nil {
				s.benc = brotli.NewWriterOptions(&s.buf, brotli.WriterOptions{Quality: s.quality})
			} else {
				s.benc.Reset(&s.buf)
			}
			bw = s.benc
		} else {
			bw = brotli.NewWriterOptions(&s.buf, brotli.WriterOptions{Quality: s.quality})
		}
		if _, err := bw.Write(payload); err != nil {
			return nil, fmt.Errorf("brotli write: %w", err)
		}
		if err := bw.Close(); err != nil {
			return nil, fmt.Errorf("brotli close: %w", err)
		}

	default:
		return nil, fmt.Errorf("compressor 0x%x: %w", comp, ErrUnsupportedCompressor)
	}

	return s.buf.Bytes(), nil
}

// decompressExact decompresses the compressed payload into exactly declared
// bytes and enforces the strict framing contract of wire-format-v1 §2:
//
//  1. the decompressed stream must terminate exactly after declared bytes
//     (one extra read must return io.EOF — more output means an understated
//     declared length or a concatenated second frame);
//  2. any non-EOF error on that read means trailing garbage;
//  3. the compressed input must be fully consumed by the decoder.
//
// The declared length bounds the single upfront allocation; it has already
// been validated against MaxUncompressedBytes by the envelope.
func decompressExact(comp CompressorID, payload []byte, declared int) ([]byte, error) {
	src := bytes.NewReader(payload)
	out := make([]byte, declared)

	var r io.Reader
	switch comp {
	case Zstd:
		zr, err := zstd.NewReader(src, zstd.WithDecoderConcurrency(1))
		if err != nil {
			return nil, fmt.Errorf("zstd decoder: %w", err)
		}
		defer zr.Close()
		r = zr
	case Brotli:
		r = brotli.NewReader(src)
	default:
		return nil, fmt.Errorf("compressor 0x%x: %w", comp, ErrUnsupportedCompressor)
	}

	if _, err := io.ReadFull(r, out); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, fmt.Errorf("declared length %d: %w", declared, ErrTruncated)
		}
		return nil, fmt.Errorf("%w: %v", ErrCorruptCompressed, err)
	}

	// Exactly declared bytes must end the stream.
	var one [1]byte
	n, err := r.Read(one[:])
	if n > 0 {
		return nil, fmt.Errorf("decompressed output exceeds declared length %d: %w", declared, ErrCorruptCompressed)
	}
	if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: %v", ErrCorruptCompressed, err)
	}
	if src.Len() != 0 {
		return nil, fmt.Errorf("compressed input has %d trailing bytes: %w", src.Len(), ErrTrailingBytes)
	}
	return out, nil
}
