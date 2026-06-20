package lzfse

import (
	"encoding/binary"
	"errors"
	"math/bits"
)

// ---------------------------------------------------------------------------
// Block magic numbers
// ---------------------------------------------------------------------------

const (
	magicEndOfStream    = 0x24787662 // "bvx$"
	magicUncompressed   = 0x2d787662 // "bvx-"
	magicCompressedV1   = 0x31787662 // "bvx1"
	magicCompressedV2   = 0x32787662 // "bvx2"
	magicCompressedLZVN = 0x6e787662 // "bvxn"
)

// ---------------------------------------------------------------------------
// L / M / D encoding tables (from lzfse_internal.h)
// ---------------------------------------------------------------------------

var lExtraBits = [lSymbols]uint8{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2, 3, 5, 8}
var lBaseValue = [lSymbols]int32{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 20, 28, 60}

var mExtraBits = [mSymbols]uint8{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 3, 5, 8, 11}
var mBaseValue = [mSymbols]int32{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 24, 56, 312}

var dExtraBits = [dSymbols]uint8{
	0, 0, 0, 0, 1, 1, 1, 1, 2, 2, 2, 2, 3, 3, 3, 3,
	4, 4, 4, 4, 5, 5, 5, 5, 6, 6, 6, 6, 7, 7, 7, 7,
	8, 8, 8, 8, 9, 9, 9, 9, 10, 10, 10, 10, 11, 11, 11, 11,
	12, 12, 12, 12, 13, 13, 13, 13, 14, 14, 14, 14, 15, 15, 15, 15,
}
var dBaseValue = [dSymbols]int32{
	0, 1, 2, 3, 4, 6, 8, 10, 12, 16, 20, 24, 28, 36, 44, 52,
	60, 76, 92, 108, 124, 156, 188, 220, 252, 316, 380, 444, 508, 636, 764, 892,
	1020, 1276, 1532, 1788, 2044, 2556, 3068, 3580, 4092, 5116, 6140, 7164,
	8188, 10236, 12284, 14332, 16380, 20476, 24572, 28668, 32764, 40956, 49148,
	57340, 65532, 81916, 98300, 114684, 131068, 163836, 196604, 229372,
}

// ---------------------------------------------------------------------------
// Encoder constants / tunables
// ---------------------------------------------------------------------------

const (
	encodeHashBits      = 14
	encodeHashWidth     = 4
	encodeGoodMatch     = 40
	encodeHashValues    = 1 << encodeHashBits
	literalsPerBlock    = 4 * 10000
	encodeLZVNThreshold = 4096
)

// matchesPerBlock is the upper bound on matches encoded per block.
// Production uses 10 000 (large enough that real disk-image workloads
// fit in one block); tests can lower it to force the multi-block
// split path in compressLZFSE.
var matchesPerBlock = 10000

// Maximum L / M / D values
const (
	maxLValue = 315
	maxMValue = 2359
	maxDValue = 262139
)

// ---------------------------------------------------------------------------
// V1 block header (uncompressed freq tables)
// Size = 772 bytes
// ---------------------------------------------------------------------------

const v1HeaderSize = 772

type v1Header struct {
	magic                uint32
	nRawBytes            uint32
	nPayloadBytes        uint32
	nLiterals            uint32
	nMatches             uint32
	nLiteralPayloadBytes uint32
	nLMDPayloadBytes     uint32
	literalBits          int32
	literalState         [4]uint16
	lmdBits              int32
	lState               uint16
	mState               uint16
	dState               uint16
	lFreq                [lSymbols]uint16
	mFreq                [mSymbols]uint16
	dFreq                [dSymbols]uint16
	literalFreq          [literalSymbols]uint16
}

func readV1Header(b []byte) (v1Header, error) {
	if len(b) < v1HeaderSize {
		return v1Header{}, errors.New("lzfse: V1 header too short")
	}
	var h v1Header
	h.magic = binary.LittleEndian.Uint32(b[0:])
	h.nRawBytes = binary.LittleEndian.Uint32(b[4:])
	h.nPayloadBytes = binary.LittleEndian.Uint32(b[8:])
	h.nLiterals = binary.LittleEndian.Uint32(b[12:])
	h.nMatches = binary.LittleEndian.Uint32(b[16:])
	h.nLiteralPayloadBytes = binary.LittleEndian.Uint32(b[20:])
	h.nLMDPayloadBytes = binary.LittleEndian.Uint32(b[24:])
	h.literalBits = int32(binary.LittleEndian.Uint32(b[28:]))
	for i := 0; i < 4; i++ {
		h.literalState[i] = binary.LittleEndian.Uint16(b[32+i*2:])
	}
	h.lmdBits = int32(binary.LittleEndian.Uint32(b[40:]))
	h.lState = binary.LittleEndian.Uint16(b[44:])
	h.mState = binary.LittleEndian.Uint16(b[46:])
	h.dState = binary.LittleEndian.Uint16(b[48:])
	// + 2 bytes padding at 50
	off := 52
	for i := range h.lFreq {
		h.lFreq[i] = binary.LittleEndian.Uint16(b[off:])
		off += 2
	}
	for i := range h.mFreq {
		h.mFreq[i] = binary.LittleEndian.Uint16(b[off:])
		off += 2
	}
	for i := range h.dFreq {
		h.dFreq[i] = binary.LittleEndian.Uint16(b[off:])
		off += 2
	}
	for i := range h.literalFreq {
		h.literalFreq[i] = binary.LittleEndian.Uint16(b[off:])
		off += 2
	}
	return h, nil
}

func writeV1Header(h v1Header) []byte {
	b := make([]byte, v1HeaderSize)
	binary.LittleEndian.PutUint32(b[0:], h.magic)
	binary.LittleEndian.PutUint32(b[4:], h.nRawBytes)
	binary.LittleEndian.PutUint32(b[8:], h.nPayloadBytes)
	binary.LittleEndian.PutUint32(b[12:], h.nLiterals)
	binary.LittleEndian.PutUint32(b[16:], h.nMatches)
	binary.LittleEndian.PutUint32(b[20:], h.nLiteralPayloadBytes)
	binary.LittleEndian.PutUint32(b[24:], h.nLMDPayloadBytes)
	binary.LittleEndian.PutUint32(b[28:], uint32(h.literalBits))
	for i := 0; i < 4; i++ {
		binary.LittleEndian.PutUint16(b[32+i*2:], h.literalState[i])
	}
	binary.LittleEndian.PutUint32(b[40:], uint32(h.lmdBits))
	binary.LittleEndian.PutUint16(b[44:], h.lState)
	binary.LittleEndian.PutUint16(b[46:], h.mState)
	binary.LittleEndian.PutUint16(b[48:], h.dState)
	off := 52
	for _, v := range h.lFreq {
		binary.LittleEndian.PutUint16(b[off:], v)
		off += 2
	}
	for _, v := range h.mFreq {
		binary.LittleEndian.PutUint16(b[off:], v)
		off += 2
	}
	for _, v := range h.dFreq {
		binary.LittleEndian.PutUint16(b[off:], v)
		off += 2
	}
	for _, v := range h.literalFreq {
		binary.LittleEndian.PutUint16(b[off:], v)
		off += 2
	}
	return b
}

// ---------------------------------------------------------------------------
// V2 freq table decode (variable-length codes)
// ---------------------------------------------------------------------------

// freqNBitsTable / freqValueTable: decode the lower 5 bits of the bitstream.
// From lzfse_decode_base.c, lzfse_freq_nbits_table / lzfse_freq_value_table.
// Each decoded value is a frequency (0..1047); 0 means skip.
var freqNBitsTable = [32]uint8{
	2, 3, 2, 5, 2, 3, 2, 8, 2, 3, 2, 5, 2, 3, 2, 14,
	2, 3, 2, 5, 2, 3, 2, 8, 2, 3, 2, 5, 2, 3, 2, 14,
}

var freqValueTable = [32]int8{
	0, 2, 1, 4, 0, 3, 1, -1, 0, 2, 1, 5, 0, 3, 1, -1,
	0, 2, 1, 6, 0, 3, 1, -1, 0, 2, 1, 7, 0, 3, 1, -1,
}

// decodeV2FreqTableBitstream decodes all 4 freq tables
// (l, m, d, literals) from a V2 header's freq[] payload.
func decodeV2FreqTableBitstream(data []byte) (
	lFreq [lSymbols]uint16,
	mFreq [mSymbols]uint16,
	dFreq [dSymbols]uint16,
	litFreq [literalSymbols]uint16,
	err error,
) {
	// The C code uses a combined bit-stream over all symbols concatenated.
	// We decode them sequentially from a single forward bit accumulator.
	allSymbols := lSymbols + mSymbols + dSymbols + literalSymbols
	all := make([]uint16, allSymbols)

	var accum uint64
	var accumBits int
	pos := 0

	refill := func() {
		for accumBits <= 56 && pos < len(data) {
			accum |= uint64(data[pos]) << uint(accumBits)
			accumBits += 8
			pos++
		}
	}

	pull := func(n uint8) uint64 {
		v := accum & ((1 << uint(n)) - 1)
		accum >>= n
		accumBits -= int(n)
		return v
	}

	refill()
	for i := 0; i < allSymbols; i++ {
		if accumBits < 14 {
			refill()
		}
		lo5 := uint8(accum & 0x1F)
		n := freqNBitsTable[lo5]
		bits5 := pull(n)
		var val uint16
		switch n {
		case 8:
			val = 8 + uint16((bits5>>4)&0xF)
		case 14:
			val = 24 + uint16((bits5>>4)&0x3FF)
		default: // n == 2, 3, or 5: use direct value lookup.
			// freqValueTable has -1 sentinels only at the indices
			// freqNBitsTable maps to n=8 or n=14 (handled above);
			// the entries reached here are always non-negative.
			val = uint16(freqValueTable[lo5])
		}
		all[i] = val
	}

	copy(lFreq[:], all[:lSymbols])
	copy(mFreq[:], all[lSymbols:lSymbols+mSymbols])
	copy(dFreq[:], all[lSymbols+mSymbols:lSymbols+mSymbols+dSymbols])
	copy(litFreq[:], all[lSymbols+mSymbols+dSymbols:])
	return
}

// ---------------------------------------------------------------------------
// V2 header decode
// ---------------------------------------------------------------------------

// v2HeaderMinSize is 32 bytes (magic + n_raw_bytes + 3×uint64 packed).
const v2HeaderMinSize = 32

type v2DecodeResult struct {
	v1Header
	headerSize int // total V2 header bytes including freq[]
}

func decodeV2Header(b []byte) (v2DecodeResult, error) {
	if len(b) < v2HeaderMinSize {
		return v2DecodeResult{}, errors.New("lzfse: V2 header too short")
	}

	v0 := binary.LittleEndian.Uint64(b[8:])
	v1 := binary.LittleEndian.Uint64(b[16:])
	v2 := binary.LittleEndian.Uint64(b[24:])

	nLiterals := int(v0 & ((1 << 20) - 1))
	nLiteralsPayload := int((v0 >> 20) & ((1 << 20) - 1))
	nMatches := int((v0 >> 40) & ((1 << 20) - 1))
	literalBits := int((v0>>60)&7) - 7

	var literalState [4]uint16
	for i := 0; i < 4; i++ {
		literalState[i] = uint16((v1 >> uint(i*10)) & 0x3FF)
	}
	nLMDPayload := int((v1 >> 40) & ((1 << 20) - 1))
	lmdBits := int((v1>>60)&7) - 7

	headerSize := int(v2 & 0xFFFFFFFF)
	lState := int((v2 >> 32) & 0x3FF)
	mState := int((v2 >> 42) & 0x3FF)
	dState := int((v2 >> 52) & 0x3FF)

	if headerSize < v2HeaderMinSize || headerSize > len(b) {
		return v2DecodeResult{}, errors.New("lzfse: V2 header size invalid")
	}

	// Decode freq tables from the variable area. The bitstream
	// decoder consumes a fixed number of variable-length codes and
	// has no failure mode once we're inside the legal headerSize
	// window, so we don't need to propagate an error here.
	freqData := b[v2HeaderMinSize:headerSize]
	lFreq, mFreq, dFreq, litFreq, _ := decodeV2FreqTableBitstream(freqData)

	h := v1Header{
		magic:                magicCompressedV1,
		nRawBytes:            binary.LittleEndian.Uint32(b[4:]),
		nPayloadBytes:        0, // not used for V2
		nLiterals:            uint32(nLiterals),
		nMatches:             uint32(nMatches),
		nLiteralPayloadBytes: uint32(nLiteralsPayload),
		nLMDPayloadBytes:     uint32(nLMDPayload),
		literalBits:          int32(literalBits),
		literalState:         literalState,
		lmdBits:              int32(lmdBits),
		lState:               uint16(lState),
		mState:               uint16(mState),
		dState:               uint16(dState),
		lFreq:                lFreq,
		mFreq:                mFreq,
		dFreq:                dFreq,
		literalFreq:          litFreq,
	}
	return v2DecodeResult{v1Header: h, headerSize: headerSize}, nil
}

// ---------------------------------------------------------------------------
// V2 freq table encode (variable-length codes)
// ---------------------------------------------------------------------------

// encodeFreqValue encodes one frequency value into the bit stream.
// Returns (nbits, codeword).
func encodeFreqValue(v uint16) (uint8, uint64) {
	switch {
	case v == 0:
		return 2, 0b00
	case v == 1:
		return 2, 0b10
	case v == 2:
		return 3, 0b001
	case v == 3:
		return 3, 0b101
	case v <= 7:
		// 5 bits: v*0x10 | 3  → but let's compute directly
		// value-4 ∈ [0,3], encode as (v-4)<<2 | 0b11 then << 1 for the tag bit
		// Actually table says codes 0b00011, 0b01011, 0b10011, 0b11011
		n := v - 4 // [0,3]
		return 5, uint64(n)<<3 | 0b011
	case v <= 23:
		// 8 bits: (v-8)<<4 | 7
		return 8, uint64(v-8)<<4 | 0b00000111
	default: // v <= 1047
		// 14 bits: (v-24)<<4 | 15
		return 14, uint64(v-24)<<4 | 0b000000001111
	}
}

// encodeV2FreqTableBitstream encodes all 4 freq tables into a byte slice.
func encodeV2FreqTableBitstream(
	lFreq [lSymbols]uint16,
	mFreq [mSymbols]uint16,
	dFreq [dSymbols]uint16,
	litFreq [literalSymbols]uint16,
) []byte {
	out := fseOutStream{}
	for _, v := range lFreq {
		n, code := encodeFreqValue(v)
		out.fseOutPush(int(n), code)
		out.fseOutFlush()
	}
	for _, v := range mFreq {
		n, code := encodeFreqValue(v)
		out.fseOutPush(int(n), code)
		out.fseOutFlush()
	}
	for _, v := range dFreq {
		n, code := encodeFreqValue(v)
		out.fseOutPush(int(n), code)
		out.fseOutFlush()
	}
	for _, v := range litFreq {
		n, code := encodeFreqValue(v)
		out.fseOutPush(int(n), code)
		out.fseOutFlush()
	}
	out.fseOutFinish()
	return out.buf
}

// ---------------------------------------------------------------------------
// Compressed block decoder (V1 header → raw bytes)
// ---------------------------------------------------------------------------

func decodeCompressedBlock(h v1Header, payload []byte) ([]byte, error) {
	nLiterals := int(h.nLiterals)
	nMatches := int(h.nMatches)
	nLitPayload := int(h.nLiteralPayloadBytes)
	nLMDPayload := int(h.nLMDPayloadBytes)

	// Validate the initial FSE states from the (untrusted) header against the
	// decoder-table sizes before they are used to index those tables. lzfse V1
	// stores raw uint16 states and V2 masks states to 10 bits, either of which
	// can exceed the lStates/mStates/dStates/literalStates counts and would
	// otherwise panic with an out-of-range index — a DoS on crafted input.
	if int(h.lState) >= lStates || int(h.mState) >= mStates || int(h.dState) >= dStates {
		return nil, errors.New("lzfse: initial L/M/D state out of range")
	}
	for _, s := range h.literalState {
		if int(s) >= literalStates {
			return nil, errors.New("lzfse: initial literal state out of range")
		}
	}

	if nLitPayload+nLMDPayload > len(payload) {
		return nil, errors.New("lzfse: payload too short")
	}

	litPayload := payload[:nLitPayload]
	lmdPayload := payload[nLitPayload : nLitPayload+nLMDPayload]

	// --- Build FSE tables ---
	litTable := make([]fseDecoderEntry, literalStates)
	if err := fseInitDecoderTable(h.literalFreq[:], literalStates, litTable); err != nil {
		return nil, err
	}

	lValueTable := make([]fseValueDecoderEntry, lStates)
	if err := fseInitValueDecoderTable(h.lFreq[:], lStates, lExtraBits[:], lBaseValue[:], lValueTable); err != nil {
		return nil, err
	}

	mValueTable := make([]fseValueDecoderEntry, mStates)
	if err := fseInitValueDecoderTable(h.mFreq[:], mStates, mExtraBits[:], mBaseValue[:], mValueTable); err != nil {
		return nil, err
	}

	dValueTable := make([]fseValueDecoderEntry, dStates)
	if err := fseInitValueDecoderTable(h.dFreq[:], dStates, dExtraBits[:], dBaseValue[:], dValueTable); err != nil {
		return nil, err
	}

	// --- Decode literals (4 interleaved streams) ---
	literals := make([]byte, nLiterals)
	{
		litEnd := nLitPayload
		n := int(h.literalBits)
		in, err := fseInInit(litPayload, litEnd, n)
		if err != nil {
			return nil, err
		}
		var ptr int
		if n != 0 {
			ptr = litEnd - 8
		} else {
			ptr = litEnd - 7
		}
		states := [4]uint16{
			h.literalState[0],
			h.literalState[1],
			h.literalState[2],
			h.literalState[3],
		}
		// Decode 4 at a time
		i := 0
		for i+4 <= nLiterals {
			in.fseInFlush(litPayload, &ptr)
			literals[i+0] = fseDecode(&states[0], litTable, &in)
			literals[i+1] = fseDecode(&states[1], litTable, &in)
			literals[i+2] = fseDecode(&states[2], litTable, &in)
			literals[i+3] = fseDecode(&states[3], litTable, &in)
			i += 4
		}
		// Remaining (0..3)
		for j := 0; j < nLiterals-i; j++ {
			in.fseInFlush(litPayload, &ptr)
			literals[i+j] = fseDecode(&states[j], litTable, &in)
		}
	}

	// --- Decode LMD stream and copy output ---
	out := make([]byte, 0, h.nRawBytes)
	{
		lEnd := nLMDPayload
		in, err := fseInInit(lmdPayload, lEnd, int(h.lmdBits))
		if err != nil {
			return nil, err
		}
		var ptr int
		if h.lmdBits != 0 {
			ptr = lEnd - 8
		} else {
			ptr = lEnd - 7
		}
		lState := h.lState
		mState := h.mState
		dState := h.dState
		litPos := 0
		var D int32

		for i := 0; i < nMatches; i++ {
			in.fseInFlush(lmdPayload, &ptr)
			L := fseValueDecode(&lState, lValueTable, &in)
			M := fseValueDecode(&mState, mValueTable, &in)
			in.fseInFlush(lmdPayload, &ptr)
			newD := fseValueDecode(&dState, dValueTable, &in)
			if newD != 0 {
				D = newD
			}

			// Copy L literals
			end := litPos + int(L)
			if end > len(literals) {
				return nil, errors.New("lzfse: literal index out of range")
			}
			out = append(out, literals[litPos:end]...)
			litPos = end

			// Copy M bytes from earlier in output. M == 0 happens for
			// the synthetic prefix records the encoder emits when a
			// literal run exceeds maxLValue; the D value is logically
			// irrelevant in that case so skip the distance check.
			if M > 0 {
				if D <= 0 || int(D) > len(out) {
					return nil, errors.New("lzfse: invalid match distance")
				}
				matchStart := len(out) - int(D)
				for k := int32(0); k < M; k++ {
					out = append(out, out[matchStart+int(k)])
				}
			}
		}

		// Copy remaining literals
		out = append(out, literals[litPos:]...)
	}

	return out, nil
}

// ---------------------------------------------------------------------------
// Decompress
// ---------------------------------------------------------------------------

// Decompress decompresses LZFSE-compressed data.
// dstLen is a hint for the decompressed size (0 = unknown, allocate dynamically).
func Decompress(src []byte) ([]byte, error) {
	pos := 0
	var out []byte

	for pos < len(src) {
		if pos+4 > len(src) {
			return nil, errors.New("lzfse: truncated stream")
		}
		magic := binary.LittleEndian.Uint32(src[pos:])

		switch magic {
		case magicEndOfStream:
			return out, nil

		case magicUncompressed:
			if pos+8 > len(src) {
				return nil, errors.New("lzfse: uncompressed block header truncated")
			}
			n := int(binary.LittleEndian.Uint32(src[pos+4:]))
			pos += 8
			if pos+n > len(src) {
				return nil, errors.New("lzfse: uncompressed block data truncated")
			}
			out = append(out, src[pos:pos+n]...)
			pos += n

		case magicCompressedV1:
			if pos+v1HeaderSize > len(src) {
				return nil, errors.New("lzfse: V1 block header truncated")
			}
			// readV1Header only errors on a too-short buffer, which
			// the length guard above already rules out.
			h, _ := readV1Header(src[pos:])
			pos += v1HeaderSize
			payloadEnd := pos + int(h.nLiteralPayloadBytes) + int(h.nLMDPayloadBytes)
			if payloadEnd > len(src) {
				return nil, errors.New("lzfse: V1 block payload truncated")
			}
			block, err := decodeCompressedBlock(h, src[pos:payloadEnd])
			if err != nil {
				return nil, err
			}
			out = append(out, block...)
			pos = payloadEnd

		case magicCompressedV2:
			if pos+v2HeaderMinSize > len(src) {
				return nil, errors.New("lzfse: V2 block header truncated")
			}
			res, err := decodeV2Header(src[pos:])
			if err != nil {
				return nil, err
			}
			pos += res.headerSize
			h := res.v1Header
			payloadEnd := pos + int(h.nLiteralPayloadBytes) + int(h.nLMDPayloadBytes)
			if payloadEnd > len(src) {
				return nil, errors.New("lzfse: V2 block payload truncated")
			}
			block, err := decodeCompressedBlock(h, src[pos:payloadEnd])
			if err != nil {
				return nil, err
			}
			out = append(out, block...)
			pos = payloadEnd

		case magicCompressedLZVN:
			// Header layout is { magic, n_raw_bytes, n_payload_bytes },
			// so we need 12 bytes available before any field read.
			if pos+12 > len(src) {
				return nil, errors.New("lzfse: LZVN block header truncated")
			}
			nRawBytes := int(binary.LittleEndian.Uint32(src[pos+4:]))
			nPayloadBytes := int(binary.LittleEndian.Uint32(src[pos+8:]))
			pos += 12
			if pos+nPayloadBytes > len(src) {
				return nil, errors.New("lzfse: LZVN block payload truncated")
			}
			block := make([]byte, nRawBytes)
			n, err := lzvnDecode(block, src[pos:pos+nPayloadBytes])
			if err != nil {
				return nil, err
			}
			out = append(out, block[:n]...)
			pos += nPayloadBytes

		default:
			return nil, errors.New("lzfse: unknown block magic")
		}
	}

	return out, nil
}

// ---------------------------------------------------------------------------
// LZFSE encoder front end (hash-based match search)
// ---------------------------------------------------------------------------

// hashEntry stores HASH_WIDTH positions and values.
type hashEntry struct {
	pos   [encodeHashWidth]int32
	value [encodeHashWidth]uint32
}

// lzfseHash computes the hash of 4-byte value x using Knuth multiplicative hashing.
func lzfseHash(x uint32) int {
	return int((x * 2654435761) >> (32 - encodeHashBits))
}

// match represents a parsed back-reference.
type match struct {
	pos int // position in src
	L   int // literal count before match
	M   int // match length
	D   int // match distance
}

// findMatches scans src and returns a list of LMD triples.
func findMatches(src []byte) []match {
	n := len(src)
	if n < 4 {
		return nil
	}

	table := make([]hashEntry, encodeHashValues)
	matches := make([]match, 0, n/8+1)

	srcEnd := n - 8 // safety margin

	type pendingMatch struct {
		pos int
		lit int
		M   int
		D   int
	}
	const noPending = -1
	pending := pendingMatch{pos: noPending}

	litStart := 0

	for i := 0; i < srcEnd; i++ {
		x := binary.LittleEndian.Uint32(src[i:])
		h := lzfseHash(x)

		e := table[h]

		// Update table (shift-in new entry)
		var updated hashEntry
		updated.pos[0] = int32(i)
		updated.value[0] = x
		copy(updated.pos[1:], e.pos[:])
		copy(updated.value[1:], e.value[:])
		table[h] = updated

		if pending.pos != noPending && i >= pending.pos+pending.M {
			// Emit pending
			matches = append(matches, match{
				pos: pending.pos,
				L:   pending.pos - litStart,
				M:   pending.M,
				D:   pending.D,
			})
			litStart = pending.pos + pending.M
			pending.pos = noPending
		}

		if i < litStart {
			continue
		}

		// Find best match in hash bucket
		bestM := 0
		bestD := 0
		for k := 0; k < encodeHashWidth; k++ {
			if e.value[k] != x {
				continue
			}
			j := int(e.pos[k])
			D := i - j
			if D <= 0 || D > maxDValue {
				continue
			}
			// Extend match
			M := 0
			for i+M < n && j+M < n && src[i+M] == src[j+M] {
				M++
				if M >= maxMValue {
					break
				}
			}
			if M >= 3 && (M > bestM || (M == bestM && D < bestD)) {
				bestM = M
				bestD = D
			}
		}

		if bestM < 3 {
			continue
		}

		// We have a match
		if pending.pos == noPending {
			pending = pendingMatch{pos: i, lit: litStart, M: bestM, D: bestD}
		} else {
			// Compare vs pending
			if bestM > pending.M || (bestM == pending.M && bestD < pending.D) {
				pending = pendingMatch{pos: i, lit: litStart, M: bestM, D: bestD}
			} else {
				// Emit pending and drop the current candidate. The
				// outer "emit when i >= pending.pos + pending.M"
				// check at the top of the loop guarantees we only
				// reach here while i is INSIDE pending's coverage,
				// so installing a new pending at i would overlap
				// the just-emitted match.
				matches = append(matches, match{
					pos: pending.pos,
					L:   pending.pos - litStart,
					M:   pending.M,
					D:   pending.D,
				})
				litStart = pending.pos + pending.M
				pending.pos = noPending
			}
		}

		// Emit immediately if match is long enough ("good match")
		if bestM >= encodeGoodMatch {
			matches = append(matches, match{
				pos: i,
				L:   i - litStart,
				M:   bestM,
				D:   bestD,
			})
			litStart = i + bestM
			i = litStart - 1 // -1 because loop increments
			pending.pos = noPending
		}
	}

	if pending.pos != noPending {
		matches = append(matches, match{
			pos: pending.pos,
			L:   pending.pos - litStart,
			M:   pending.M,
			D:   pending.D,
		})
		litStart = pending.pos + pending.M
	}
	_ = litStart

	return matches
}

// ---------------------------------------------------------------------------
// LZFSE encoder backend (encode one block into a V1 header + payload)
// ---------------------------------------------------------------------------

const (
	// LMD payload padding: 8 null bytes at the beginning (required by decoder).
	lmdPadBytes = 8
)

// encodeBlock encodes src[srcStart:srcEnd] using the given matches.
// Returns the V1 header + payload bytes.
func encodeBlock(src []byte, srcStart, srcEnd int, blockMatches []match) []byte {
	// Separate literals and LMD triples
	literals := make([]byte, 0, literalsPerBlock)
	// Build raw triples first; apply the D-reuse transform AFTER
	// splitting long literal runs so the split-prefix records inherit
	// the right D value.
	rawL := make([]int, 0, matchesPerBlock)
	rawM := make([]int, 0, matchesPerBlock)
	rawD := make([]int, 0, matchesPerBlock)

	litPos := srcStart
	for _, m := range blockMatches {
		literals = append(literals, src[litPos:m.pos]...)
		// Split long literal runs (L > maxLValue=315) into one or more
		// prefix records carrying (L=maxLValue, M=0, D=match's D)
		// followed by the actual (residualL, M, D) record. The M=0
		// prefix records are no-ops at decode time but keep each L in
		// the FSE symbol-table range.
		L := m.L
		for L > maxLValue {
			rawL = append(rawL, maxLValue)
			rawM = append(rawM, 0)
			rawD = append(rawD, m.D)
			L -= maxLValue
		}
		rawL = append(rawL, L)
		rawM = append(rawM, m.M)
		rawD = append(rawD, m.D)
		litPos = m.pos + m.M
	}
	// Trailing literals have no match; they're stored in the literals buffer
	// but not associated with any match — the decoder just appends them after.
	literals = append(literals, src[litPos:srcEnd]...)

	// Apply the D-reuse transform: emit 0 when D matches lastD.
	lValues := rawL
	mValues := rawM
	dValues := make([]int, len(rawD))
	{
		var lastD int
		for i, d := range rawD {
			if d == lastD {
				dValues[i] = 0
			} else {
				dValues[i] = d
				lastD = d
			}
		}
	}

	nLiterals := len(literals)
	nMatches := len(lValues)

	// --- Build frequency tables ---
	lCounts := make([]int, lSymbols)
	mCounts := make([]int, mSymbols)
	dCounts := make([]int, dSymbols)
	litCounts := make([]int, literalSymbols)

	for _, b := range literals {
		litCounts[b]++
	}

	lSyms := make([]int, nMatches)
	mSyms := make([]int, nMatches)
	dSyms := make([]int, nMatches)

	for i, lv := range lValues {
		s := lmdSymbol(int32(lv), lBaseValue[:], lExtraBits[:], lSymbols)
		lSyms[i] = s
		lCounts[s]++
	}
	for i, mv := range mValues {
		s := lmdSymbol(int32(mv), mBaseValue[:], mExtraBits[:], mSymbols)
		mSyms[i] = s
		mCounts[s]++
	}
	for i, dv := range dValues {
		s := lmdSymbol(int32(dv), dBaseValue[:], dExtraBits[:], dSymbols)
		dSyms[i] = s
		dCounts[s]++
	}

	// ensureNonZero ensures each count slice has at least one non-zero
	// entry so fseNormalizeFreq's sCount==0 guard never fires below.
	ensureNonZero(lCounts)
	ensureNonZero(mCounts)
	ensureNonZero(dCounts)
	ensureNonZero(litCounts)

	lFreqs, _ := fseNormalizeFreq(lCounts, lStates)
	mFreqs, _ := fseNormalizeFreq(mCounts, mStates)
	dFreqs, _ := fseNormalizeFreq(dCounts, dStates)
	litFreqs, _ := fseNormalizeFreq(litCounts, literalStates)

	// --- Build encoder tables ---
	lEncTable := fseInitEncoderTable(lFreqs, lStates)
	mEncTable := fseInitEncoderTable(mFreqs, mStates)
	dEncTable := fseInitEncoderTable(dFreqs, dStates)
	litEncTable := fseInitEncoderTable(litFreqs, literalStates)

	// --- Encode LMD stream (BACKWARDS, starting from end of matches) ---
	// The LMD payload has 8 padding bytes at the start, then forward-written encoded data.
	lmdOut := fseOutStream{}
	// initial states: start at 0, matching C encoder (l_state = m_state = d_state = 0)
	lState := uint16(0)
	mState := uint16(0)
	dState := uint16(0)

	// Encode all triples, last to first.
	// Per symbol: push extra bits FIRST (they become LOW bits), then FSE state bits
	// (they become HIGH bits), so the decoder's fseValueDecode reads them correctly.
	for i := nMatches - 1; i >= 0; i-- {
		// encode D: extra bits first, then FSE state
		dv := dValues[i]
		dSym := dSyms[i]
		dExtra := int(dExtraBits[dSym])
		dResid := dv - int(dBaseValue[dSym])
		lmdOut.fseOutPush(dExtra, uint64(dResid))
		fseEncode(&dState, dEncTable, uint8(dSym), &lmdOut)
		lmdOut.fseOutFlush()

		// encode M: extra bits first, then FSE state
		mv := mValues[i]
		mSym := mSyms[i]
		mExtra := int(mExtraBits[mSym])
		mResid := mv - int(mBaseValue[mSym])
		lmdOut.fseOutPush(mExtra, uint64(mResid))
		fseEncode(&mState, mEncTable, uint8(mSym), &lmdOut)
		lmdOut.fseOutFlush()

		// encode L: extra bits first, then FSE state
		lv := lValues[i]
		lSym := lSyms[i]
		lExtra := int(lExtraBits[lSym])
		lResid := lv - int(lBaseValue[lSym])
		lmdOut.fseOutPush(lExtra, uint64(lResid))
		fseEncode(&lState, lEncTable, uint8(lSym), &lmdOut)
		lmdOut.fseOutFlush()
	}
	lmdBits := lmdOut.fseOutFinish()
	// LMD payload: 8 zero padding bytes at front (like C's store8(buf,0); buf+=8),
	// then the forward-written encoded data.
	lmdPayload := make([]byte, lmdPadBytes, lmdPadBytes+len(lmdOut.buf))
	lmdPayload = append(lmdPayload, lmdOut.buf...)

	litPayload, litBit0, litStatesFinal := encodeLiterals4Interleaved(literals, litFreqs, litEncTable)

	// --- Build V1 header ---
	var h v1Header
	h.magic = magicCompressedV1
	h.nRawBytes = uint32(srcEnd - srcStart)
	h.nLiterals = uint32(nLiterals)
	h.nMatches = uint32(nMatches)
	h.nLiteralPayloadBytes = uint32(len(litPayload))
	h.nLMDPayloadBytes = uint32(len(lmdPayload))
	h.literalBits = int32(litBit0)
	h.literalState = litStatesFinal
	h.lmdBits = int32(lmdBits)
	h.lState = lState
	h.mState = mState
	h.dState = dState
	copy(h.lFreq[:], lFreqs)
	copy(h.mFreq[:], mFreqs)
	copy(h.dFreq[:], dFreqs)
	copy(h.literalFreq[:], litFreqs)
	h.nPayloadBytes = h.nLiteralPayloadBytes + h.nLMDPayloadBytes

	hdr := writeV1Header(h)
	result := append(hdr, litPayload...)
	result = append(result, lmdPayload...)
	return result
}

// encodeLiterals4Interleaved encodes the literal stream as 4 interleaved FSE
// channels from a single backward bitstream.
func encodeLiterals4Interleaved(
	literals []byte,
	freqs []uint16,
	encTable []fseEncoderEntry,
) (payload []byte, bits int, states [4]uint16) {
	n := len(literals)
	out := fseOutStream{}

	// Initial literal encoder states = 0, matching C encoder (state0=state1=state2=state3=0)
	st := [4]uint16{0, 0, 0, 0}

	// Encode backwards, 4 at a time
	i := n - (n % 4) // align to multiple of 4
	// Handle tail (< 4 remaining at end, but they go first since we read backwards)
	tail := n % 4
	for j := tail - 1; j >= 0; j-- {
		fseEncode(&st[j], encTable, literals[i+j], &out)
		out.fseOutFlush()
	}
	// Encode in groups of 4 from end to beginning
	for i -= 4; i >= 0; i -= 4 {
		for j := 3; j >= 0; j-- {
			fseEncode(&st[j], encTable, literals[i+j], &out)
			out.fseOutFlush()
		}
	}

	bits = out.fseOutFinish()
	for ch := 0; ch < 4; ch++ {
		states[ch] = st[ch]
	}
	return out.buf, bits, states
}

// lmdSymbol returns the FSE symbol for value v given the base/extra tables.
func lmdSymbol(v int32, base []int32, extra []uint8, nsym int) int {
	for s := nsym - 1; s >= 0; s-- {
		if v >= base[s] {
			return s
		}
	}
	return 0
}

// ensureNonZero ensures at least one count is non-zero (adds 1 to symbol 0).
func ensureNonZero(counts []int) {
	for _, c := range counts {
		if c > 0 {
			return
		}
	}
	counts[0]++
}

// ---------------------------------------------------------------------------
// Compress
// ---------------------------------------------------------------------------

// Compress compresses src using LZFSE and returns the compressed bytes.
func Compress(src []byte) ([]byte, error) {
	n := len(src)

	// Small inputs: use LZVN block
	if n <= encodeLZVNThreshold {
		return compressLZVN(src), nil
	}

	return compressLZFSE(src), nil
}

// compressLZVN wraps data in an LZFSE LZVN block.
func compressLZVN(src []byte) []byte {
	payload := lzvnEncodeBuffer(src)

	// Block: magic(4) + n_raw(4) + n_payload(4) + payload + EOS block
	out := make([]byte, 12+len(payload)+4)
	binary.LittleEndian.PutUint32(out[0:], magicCompressedLZVN)
	binary.LittleEndian.PutUint32(out[4:], uint32(len(src)))
	binary.LittleEndian.PutUint32(out[8:], uint32(len(payload)))
	copy(out[12:], payload)
	binary.LittleEndian.PutUint32(out[12+len(payload):], magicEndOfStream)
	return out
}

// compressLZFSE compresses src using LZFSE V2 blocks.
func compressLZFSE(src []byte) []byte {
	n := len(src)
	allMatches := findMatches(src)

	var result []byte

	// Split matches into blocks of at most matchesPerBlock
	start := 0
	matchStart := 0

	for start < n {
		// Find end of this block
		blockMatchEnd := matchStart + matchesPerBlock
		if blockMatchEnd > len(allMatches) {
			blockMatchEnd = len(allMatches)
		}

		var blockEnd int
		if blockMatchEnd < len(allMatches) {
			// Block ends at start of next block's first match
			blockEnd = allMatches[blockMatchEnd].pos
		} else {
			blockEnd = n
		}

		blockMatches := allMatches[matchStart:blockMatchEnd]

		// Re-base match L values relative to block start
		rebased := make([]match, len(blockMatches))
		litPos := start
		for i, m := range blockMatches {
			rebased[i] = match{
				pos: m.pos,
				L:   m.pos - litPos,
				M:   m.M,
				D:   m.D,
			}
			litPos = m.pos + m.M
		}

		v1Data := encodeBlock(src, start, blockEnd, rebased)

		// V2 always packs the freq tables more tightly than the fixed
		// 772-byte V1 header, so we always re-encode to V2. encodeBlock
		// produces a buffer longer than v1HeaderSize and readV1Header
		// only errors when the buffer is shorter than that — neither
		// failure mode is reachable here.
		h, _ := readV1Header(v1Data)
		v2Data := makeV2Block(h, v1Data[v1HeaderSize:])
		result = append(result, v2Data...)
		start = blockEnd
		matchStart = blockMatchEnd
	}

	// EOS block
	eos := make([]byte, 4)
	binary.LittleEndian.PutUint32(eos, magicEndOfStream)
	result = append(result, eos...)
	return result
}

// makeV2Block converts a V1 header + payload into a V2 block.
func makeV2Block(h v1Header, payload []byte) []byte {
	// Encode freq tables
	var lFreq [lSymbols]uint16
	var mFreq [mSymbols]uint16
	var dFreq [dSymbols]uint16
	var litFreq [literalSymbols]uint16
	copy(lFreq[:], h.lFreq[:])
	copy(mFreq[:], h.mFreq[:])
	copy(dFreq[:], h.dFreq[:])
	copy(litFreq[:], h.literalFreq[:])

	freqBytes := encodeV2FreqTableBitstream(lFreq, mFreq, dFreq, litFreq)
	headerSize := v2HeaderMinSize + len(freqBytes)

	// Pack fields
	var v0, v1, v2 uint64
	v0 = uint64(h.nLiterals) |
		uint64(h.nLiteralPayloadBytes)<<20 |
		uint64(h.nMatches)<<40 |
		uint64(h.literalBits+7)<<60

	v1 = uint64(h.literalState[0]) |
		uint64(h.literalState[1])<<10 |
		uint64(h.literalState[2])<<20 |
		uint64(h.literalState[3])<<30 |
		uint64(h.nLMDPayloadBytes)<<40 |
		uint64(h.lmdBits+7)<<60

	v2 = uint64(headerSize) |
		uint64(h.lState)<<32 |
		uint64(h.mState)<<42 |
		uint64(h.dState)<<52

	out := make([]byte, headerSize+len(payload))
	binary.LittleEndian.PutUint32(out[0:], magicCompressedV2)
	binary.LittleEndian.PutUint32(out[4:], h.nRawBytes)
	binary.LittleEndian.PutUint64(out[8:], v0)
	binary.LittleEndian.PutUint64(out[16:], v1)
	binary.LittleEndian.PutUint64(out[24:], v2)
	copy(out[v2HeaderMinSize:], freqBytes)
	copy(out[headerSize:], payload)
	return out
}

// Ensure math/bits is used (avoids unused import if only fse.go uses it)
var _ = bits.Len
