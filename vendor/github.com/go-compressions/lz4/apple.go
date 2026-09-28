package lz4

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Apple's LZ4 frame format (the byte stream produced by macOS's Compression
// framework with COMPRESSION_LZ4, and by Apple tooling such as Tart) is a
// sequence of 4-byte-magic blocks:
//
//	"bv41" u32 rawSize u32 compSize  <compSize bytes: a standard LZ4 block>
//	"bv4-" u32 rawSize               <rawSize bytes: stored, uncompressed>
//	"bv4$"                           end-of-stream marker
//
// All integers are little-endian. Crucially — and unlike an isolated LZ4 block —
// a compressed block's matches may reference the previous block's output: the
// blocks share one 64 KiB sliding window (LZ4 offsets are 16-bit). Decoders
// must therefore carry the trailing window across block boundaries, which is
// what DecompressAppleStream does via decodeInto's dictionary support.
//
// This mirrors the "bvx*" frame that github.com/go-compressions/lzfse decodes
// for LZFSE/LZVN; the "bv4*" frame is the LZ4 member of the same family.
var (
	appleMagicCompressed   = [4]byte{'b', 'v', '4', '1'}
	appleMagicUncompressed = [4]byte{'b', 'v', '4', '-'}
	appleMagicEnd          = [4]byte{'b', 'v', '4', '$'}
)

// ErrAppleFrame reports a malformed Apple LZ4 frame (unknown block magic,
// truncated header/payload, or a block whose decoded length disagrees with its
// declared raw size).
var ErrAppleFrame = errors.New("lz4: malformed Apple LZ4 frame")

// DecompressAppleStream decodes Apple's LZ4 frame format from src, writing the
// decompressed bytes to dst, and returns the number of bytes written. It streams
// block by block, holding at most one block plus the 64 KiB cross-block window
// in memory, so it is safe for multi-gigabyte inputs (e.g. Tart disk layers).
//
// A well-formed frame ends with a "bv4$" marker; a stream that ends before one
// (including an empty stream) is reported as ErrAppleFrame.
func DecompressAppleStream(dst io.Writer, src io.Reader) (int64, error) {
	var (
		written int64
		window  []byte // up to winSize bytes of previously decoded output
		magic   [4]byte
		hdr     [8]byte
	)
	for {
		if _, err := io.ReadFull(src, magic[:]); err != nil {
			return written, fmt.Errorf("%w: reading block magic: %w", ErrAppleFrame, err)
		}
		switch magic {
		case appleMagicEnd:
			return written, nil

		case appleMagicCompressed:
			if _, err := io.ReadFull(src, hdr[:]); err != nil {
				return written, fmt.Errorf("%w: reading compressed header: %w", ErrAppleFrame, err)
			}
			rawSize := int(binary.LittleEndian.Uint32(hdr[0:4]))
			compSize := int(binary.LittleEndian.Uint32(hdr[4:8]))
			payload := make([]byte, compSize)
			if _, err := io.ReadFull(src, payload); err != nil {
				return written, fmt.Errorf("%w: reading compressed payload: %w", ErrAppleFrame, err)
			}
			// Prefix the previous window so this block's matches can reference it,
			// then decode the block immediately after it.
			buf := make([]byte, len(window)+rawSize+decodeSlack)
			copy(buf, window)
			buf, di, err := decodeInto(buf, len(window), payload)
			if err != nil {
				return written, err
			}
			out := buf[len(window):di]
			if len(out) != rawSize {
				return written, fmt.Errorf("%w: block decoded to %d bytes, header says %d", ErrAppleFrame, len(out), rawSize)
			}
			if _, err := dst.Write(out); err != nil {
				return written, err
			}
			written += int64(len(out))
			window = slideWindow(window, out)

		case appleMagicUncompressed:
			if _, err := io.ReadFull(src, hdr[:4]); err != nil {
				return written, fmt.Errorf("%w: reading stored header: %w", ErrAppleFrame, err)
			}
			rawSize := int(binary.LittleEndian.Uint32(hdr[0:4]))
			raw := make([]byte, rawSize)
			if _, err := io.ReadFull(src, raw); err != nil {
				return written, fmt.Errorf("%w: reading stored payload: %w", ErrAppleFrame, err)
			}
			if _, err := dst.Write(raw); err != nil {
				return written, err
			}
			written += int64(len(raw))
			window = slideWindow(window, raw)

		default:
			return written, fmt.Errorf("%w: unknown block magic %q", ErrAppleFrame, magic[:])
		}
	}
}

// DecompressApple decodes an in-memory Apple LZ4 frame and returns the
// decompressed bytes. For large inputs prefer DecompressAppleStream, which does
// not buffer the whole output.
func DecompressApple(src []byte) ([]byte, error) {
	var out sliceWriter
	if _, err := DecompressAppleStream(&out, &byteReader{b: src}); err != nil {
		return nil, err
	}
	return out, nil
}

// slideWindow returns the trailing winSize bytes of window++out in a fresh
// buffer, so the returned slice never keeps an oversized decode buffer alive.
func slideWindow(window, out []byte) []byte {
	if len(out) >= winSize {
		nw := make([]byte, winSize)
		copy(nw, out[len(out)-winSize:])
		return nw
	}
	keep := winSize - len(out)
	if keep > len(window) {
		keep = len(window)
	}
	nw := make([]byte, 0, keep+len(out))
	nw = append(nw, window[len(window)-keep:]...)
	nw = append(nw, out...)
	return nw
}

// sliceWriter is a minimal in-memory io.Writer used by DecompressApple.
type sliceWriter []byte

func (w *sliceWriter) Write(p []byte) (int, error) {
	*w = append(*w, p...)
	return len(p), nil
}

// byteReader is a minimal io.Reader over a byte slice (like bytes.Reader, but
// without the extra imports) used by DecompressApple.
type byteReader struct {
	b []byte
	i int
}

func (r *byteReader) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}
