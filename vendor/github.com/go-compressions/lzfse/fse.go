// Package lzfse implements pure-Go LZFSE compression and decompression.
// The LZFSE format is documented by the Apple open-source implementation at
// https://github.com/lzfse/lzfse (BSD-licensed).
package lzfse

import (
	"encoding/binary"
	"errors"
	"math/bits"
)

// ---------------------------------------------------------------------------
// FSE constants
// ---------------------------------------------------------------------------

const (
	lSymbols       = 20
	mSymbols       = 20
	dSymbols       = 64
	literalSymbols = 256

	lStates       = 64
	mStates       = 64
	dStates       = 256
	literalStates = 1024
)

// ---------------------------------------------------------------------------
// FSE decoder table entry (32 bits, matching C struct fse_decoder_entry)
//
//  bits[0:8]   = k        (int8: number of bits to read)
//  bits[8:16]  = symbol   (uint8)
//  bits[16:32] = delta    (int16: signed increment for next state)
// ---------------------------------------------------------------------------

type fseDecoderEntry int32

func makeFseDecoderEntry(k int8, sym uint8, delta int16) fseDecoderEntry {
	return fseDecoderEntry(int32(uint8(k)) | int32(sym)<<8 | int32(delta)<<16)
}

func (e fseDecoderEntry) k() int8      { return int8(e & 0xff) }
func (e fseDecoderEntry) sym() uint8   { return uint8((e >> 8) & 0xff) }
func (e fseDecoderEntry) delta() int16 { return int16(e >> 16) }

// ---------------------------------------------------------------------------
// FSE value decoder table entry (64 bits, matching C struct fse_value_decoder_entry)
// ---------------------------------------------------------------------------

type fseValueDecoderEntry struct {
	totalBits uint8 // state bits + extra value bits
	valueBits uint8 // extra value bits
	delta     int16 // state base (delta)
	vbase     int32 // value base
}

// ---------------------------------------------------------------------------
// FSE encoder table entry (matching C struct fse_encoder_entry)
// ---------------------------------------------------------------------------

type fseEncoderEntry struct {
	s0     int16 // first state requiring k-bit shift
	k      int16 // shift amount
	delta0 int16 // delta when state >= s0
	delta1 int16 // delta when state < s0
}

// ---------------------------------------------------------------------------
// FSE bit-input stream
//
// The bit stream is read BACKWARDS through the buffer.
// The accumulator holds bits MSB-first: the next bit to be consumed
// is at position (accumNBits-1). The C code keeps accumNBits in [56,63].
//
// Init: load 8 bytes (or 7 if init_bits==0) from the end of the payload
//       backwards.
// Flush: when accumNBits drops below 56, load more bytes from the buffer.
// Pull(n): consume n bits from MSB; return them as uint64.
// ---------------------------------------------------------------------------

type fseInStream struct {
	accum      uint64
	accumNBits int
}

// fseInInit initialises the stream.
// n is literal_bits or lmd_bits from the block header (range [-7, 0]).
// buf is the complete payload; end is the payload length (buf[0:end] is this stream).
func fseInInit(buf []byte, end int, n int) (fseInStream, error) {
	var s fseInStream
	if n != 0 {
		// Load 8 bytes: buf[end-8 .. end-1]
		if end < 8 {
			return s, errors.New("fse: stream too short (init n!=0)")
		}
		// Little-endian load
		b := buf[end-8 : end]
		s.accum = uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
			uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
		s.accumNBits = n + 64
	} else {
		// Load 7 bytes: buf[end-7 .. end-1]
		if end < 7 {
			if end == 0 {
				// Empty payload: start with 0 bits
				s.accum = 0
				s.accumNBits = 56
				return s, nil
			}
			return s, errors.New("fse: stream too short (init n==0)")
		}
		b := buf[end-7 : end]
		s.accum = uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
			uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48
		s.accumNBits = n + 56
	}
	// The reference (fse_in_checked_init64) requires the bits above accumNBits to
	// be zero — the encoder zeroes them, so a non-zero value means the stream is
	// corrupt. Rejecting it here makes malformed payloads error rather than
	// decode into wrong output.
	if s.accumNBits < 56 || s.accumNBits >= 64 || (s.accum>>uint(s.accumNBits)) != 0 {
		return s, errors.New("fse: invalid stream init (nonzero high bits)")
	}
	return s, nil
}

// fseInFlush loads more bytes from a backward buffer to keep accumNBits in [56, 63].
// ptr is the current backward read pointer (decrements as we read).
// It should point to the byte BEFORE the region we already loaded.
// After init with n==0, ptr = end-7; with n!=0, ptr = end-8.
// Flush reads nbits = (63 - accumNBits) & -8 bits = nbytes bytes from ptr.
func (s *fseInStream) fseInFlush(buf []byte, ptr *int) {
	nbits := (63 - s.accumNBits) & ^7 // bits to load, rounded down to multiple of 8
	nbytes := nbits >> 3
	newPtr := *ptr - nbytes
	if newPtr < 0 {
		// Pad with zeros
		for i := 0; i < nbytes && *ptr > 0; i++ {
			*ptr--
			b := uint64(buf[*ptr])
			s.accum = (s.accum << 8) | b
			s.accumNBits += 8
		}
		return
	}
	*ptr = newPtr
	// Load nbytes (<=7) bytes starting at newPtr, little-endian into incoming.
	// When a full 8-byte little-endian word is in range we load it once and
	// mask to nbits, avoiding the per-byte shift loop; otherwise (near the
	// front edge of buf, where newPtr+8 would read past the end) fall back to
	// the byte-at-a-time load.
	var incoming uint64
	if newPtr+8 <= len(buf) {
		incoming = binary.LittleEndian.Uint64(buf[newPtr:])
	} else {
		for i := 0; i < nbytes; i++ {
			incoming |= uint64(buf[newPtr+i]) << uint(i*8)
		}
	}
	// Shift accum left and OR in new bits
	s.accum = (s.accum << uint(nbits)) | (incoming & ((1 << uint(nbits)) - 1))
	s.accumNBits += nbits
}

// fseInPull pulls n bits from the MSB of the accumulator.
func (s *fseInStream) fseInPull(n int8) uint64 {
	if n <= 0 {
		return 0
	}
	s.accumNBits -= int(n)
	result := s.accum >> uint(s.accumNBits)
	s.accum &= (1 << uint(s.accumNBits)) - 1
	return result
}

// ---------------------------------------------------------------------------
// FSE bit-output stream (writes FORWARDS)
// ---------------------------------------------------------------------------

type fseOutStream struct {
	accum      uint64
	accumNBits int
	buf        []byte
}

// fseOutPush appends n bits (low bits of v) to the output stream (LSB-first).
func (s *fseOutStream) fseOutPush(n int, v uint64) {
	if n <= 0 {
		return
	}
	s.accum |= (v & ((1 << uint(n)) - 1)) << uint(s.accumNBits)
	s.accumNBits += n
}

// fseOutFlush writes complete bytes from the accumulator.
func (s *fseOutStream) fseOutFlush() {
	nbits := s.accumNBits & ^7 // number of bits to flush (multiple of 8)
	nbytes := nbits >> 3
	for i := 0; i < nbytes; i++ {
		s.buf = append(s.buf, byte(s.accum))
		s.accum >>= 8
	}
	s.accumNBits -= nbits
}

// fseOutFinish emits the final partial byte and returns the bit count
// for the header field (n such that "remaining bits = n", range [-7, 0]).
func (s *fseOutStream) fseOutFinish() int {
	if s.accumNBits > 0 {
		s.buf = append(s.buf, byte(s.accum))
		n := s.accumNBits
		s.accum = 0
		s.accumNBits = 0
		return n - 8 // e.g. 3 bits stored → return 3-8 = -5
	}
	return 0
}

// ---------------------------------------------------------------------------
// FSE freq normalization
// ---------------------------------------------------------------------------

var errZeroFreq = errors.New("fse: all-zero frequency table")

// fseNormalizeFreq normalises a histogram into a valid FSE frequency table.
// Port of fse_normalize_freq from lzfse_fse.c.
func fseNormalizeFreq(counts []int, nstates int) ([]uint16, error) {
	sCount := 0
	for _, c := range counts {
		sCount += c
	}
	if sCount == 0 {
		return nil, errZeroFreq
	}

	freqs := make([]uint16, len(counts))
	remaining := nstates
	maxFreq := 0
	maxFreqSym := 0
	shift := bits.LeadingZeros32(uint32(nstates)) - 1
	var highprecStep uint32
	highprecStep = (1 << 31) / uint32(sCount)

	for i, c := range counts {
		if c == 0 {
			continue
		}
		f := int(((uint32(c)*highprecStep)>>uint(shift) + 1) >> 1)
		if f == 0 {
			f = 1
		}
		freqs[i] = uint16(f)
		remaining -= f
		if f > maxFreq {
			maxFreq = f
			maxFreqSym = i
		}
	}
	// sCount > 0 implies at least one non-zero count, so maxFreqSym
	// was set by the loop above. No "all zero" fallthrough needed —
	// the sCount==0 guard at the top handles that case.

	// Adjust
	if -remaining < (maxFreq >> 2) {
		freqs[maxFreqSym] = uint16(int(freqs[maxFreqSym]) + remaining)
	} else {
		// fse_adjust_freqs: remove states from most frequent symbols
		for remaining != 0 {
			any := false
			for shift2 := 3; shift2 >= 0 && remaining != 0; shift2-- {
				for sym := range freqs {
					if freqs[sym] > 1 {
						n := (int(freqs[sym]) - 1) >> uint(shift2)
						if n > -remaining {
							n = -remaining
						}
						if n > 0 {
							freqs[sym] -= uint16(n)
							remaining += n
							any = true
						}
						if remaining == 0 {
							break
						}
					}
				}
			}
			if !any {
				break
			}
		}
	}

	return freqs, nil
}

// ---------------------------------------------------------------------------
// FSE decoder table init (port of fse_init_decoder_table)
// ---------------------------------------------------------------------------

func fseInitDecoderTable(freqs []uint16, nstates int, table []fseDecoderEntry) error {
	nClz := bits.LeadingZeros32(uint32(nstates))
	offset := 0
	for sym, f := range freqs {
		if f == 0 {
			continue
		}
		fi := int(f)
		if offset+fi > nstates {
			return errors.New("lzfse: decoder freq table exceeds state space")
		}
		k := bits.LeadingZeros32(uint32(fi)) - nClz
		j0 := (2 * nstates >> uint(k)) - fi
		for j := 0; j < fi; j++ {
			var ek int
			var delta int16
			if j < j0 {
				ek = k
				delta = int16((fi+j)<<uint(k) - nstates)
			} else {
				ek = k - 1
				delta = int16((j - j0) << uint(k-1))
			}
			table[offset+j] = makeFseDecoderEntry(int8(ek), uint8(sym), delta)
		}
		offset += fi
	}
	return nil
}

// ---------------------------------------------------------------------------
// FSE value decoder table init (port of fse_init_value_decoder_table)
// ---------------------------------------------------------------------------

func fseInitValueDecoderTable(freqs []uint16, nstates int,
	extraBits []uint8, baseValues []int32,
	table []fseValueDecoderEntry) error {

	nClz := bits.LeadingZeros32(uint32(nstates))
	offset := 0
	for sym, f := range freqs {
		if f == 0 {
			continue
		}
		fi := int(f)
		if offset+fi > nstates {
			return errors.New("lzfse: value decoder freq table exceeds state space")
		}
		k := bits.LeadingZeros32(uint32(fi)) - nClz
		j0 := (2 * nstates >> uint(k)) - fi
		vb := extraBits[sym]
		vbase := baseValues[sym]
		for j := 0; j < fi; j++ {
			var tb uint8
			var delta int16
			if j < j0 {
				tb = uint8(k) + vb
				delta = int16((fi+j)<<uint(k) - nstates)
			} else {
				tb = uint8(k-1) + vb
				delta = int16((j - j0) << uint(k-1))
			}
			table[offset+j] = fseValueDecoderEntry{
				totalBits: tb,
				valueBits: vb,
				delta:     delta,
				vbase:     vbase,
			}
		}
		offset += fi
	}
	return nil
}

// ---------------------------------------------------------------------------
// FSE encoder table init (port of fse_init_encoder_table)
// ---------------------------------------------------------------------------

func fseInitEncoderTable(freqs []uint16, nstates int) []fseEncoderEntry {
	table := make([]fseEncoderEntry, len(freqs))
	nClz := bits.LeadingZeros32(uint32(nstates))
	offset := 0
	for i, f := range freqs {
		if f == 0 {
			continue
		}
		fi := int(f)
		k := bits.LeadingZeros32(uint32(fi)) - nClz
		s0 := int16(fi<<uint(k) - nstates)
		delta0 := int16(offset - fi + (nstates >> uint(k)))
		delta1 := int16(offset - fi + (nstates >> uint(k-1)))
		table[i] = fseEncoderEntry{
			s0:     s0,
			k:      int16(k),
			delta0: delta0,
			delta1: delta1,
		}
		offset += fi
	}
	return table
}

// ---------------------------------------------------------------------------
// FSE decode step (port of fse_decode inline)
// ---------------------------------------------------------------------------

// fseDecode decodes one symbol, updates state and pulls from the stream.
// Must call fseInFlush before calling this if needed.
func fseDecode(state *uint16, table []fseDecoderEntry, s *fseInStream) uint8 {
	e := table[*state]
	b := s.fseInPull(e.k())
	*state = uint16(int(e.delta()) + int(b))
	return e.sym()
}

// ---------------------------------------------------------------------------
// FSE value decode step (port of fse_value_decode inline)
// ---------------------------------------------------------------------------

func fseValueDecode(state *uint16, table []fseValueDecoderEntry, s *fseInStream) int32 {
	e := table[*state]
	b := s.fseInPull(int8(e.totalBits))
	*state = uint16(int(e.delta) + int(b>>e.valueBits))
	return e.vbase + int32(b&((1<<e.valueBits)-1))
}

// ---------------------------------------------------------------------------
// FSE encode step (port of fse_encode inline)
// ---------------------------------------------------------------------------

func fseEncode(state *uint16, table []fseEncoderEntry, sym uint8, out *fseOutStream) {
	e := table[sym]
	s := int(*state)
	var nbits int
	var delta int16
	if int16(s) >= e.s0 {
		nbits = int(e.k)
		delta = e.delta0
	} else {
		nbits = int(e.k) - 1
		delta = e.delta1
	}
	b := uint64(s) & ((1 << uint(nbits)) - 1)
	out.fseOutPush(nbits, b)
	*state = uint16(int(delta) + (s >> uint(nbits)))
}
