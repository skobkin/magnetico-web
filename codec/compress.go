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
// exclusively. Encoders are lazily created and reused via Reset.
type compressState struct {
	// quality is the Brotli quality used by this profile's candidates.
	quality int
	// usesZstd/usesBrotli reflect the profile's candidate set.
	usesZstd, usesBrotli bool

	zenc *zstd.Encoder
	benc *brotli.Writer
	buf  bytes.Buffer
}

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
// encoders when present. The returned slice aliases the state's internal
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
		if s.benc == nil {
			s.benc = brotli.NewWriterOptions(&s.buf, brotli.WriterOptions{Quality: s.quality})
		} else {
			s.benc.Reset(&s.buf)
		}
		if _, err := s.benc.Write(payload); err != nil {
			return nil, fmt.Errorf("brotli write: %w", err)
		}
		if err := s.benc.Close(); err != nil {
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
