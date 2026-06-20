package lzfse

import (
	"encoding/binary"
	"errors"
	"math/bits"
)

// ---------------------------------------------------------------------------
// LZVN constants
// ---------------------------------------------------------------------------

const (
	lzvnEncodeHashValues        = 1 << 16 // LZVN_ENCODE_HASH_VALUES
	lzvnEncodeMaxDistance       = 0xFFFF
	lzvnEncodeMaxSrcSize        = (1 << 32) - 1
	lzvnEncodeMinSrcSize        = 8
	lzvnEncodeMinMargin         = 8
	lzvnEncodeMinDstSize        = 8
	lzvnEncodeMaxLiteralBacklog = 271
)

// ---------------------------------------------------------------------------
// LZVN decoder
// ---------------------------------------------------------------------------

// lzvnDecode decodes LZVN-compressed data from src into dst.
// Returns the number of bytes written, or an error.
func lzvnDecode(dst, src []byte) (int, error) {
	dpos := 0
	spos := 0
	dEnd := len(dst)
	sEnd := len(src)
	dPrev := 0 // previous match distance

	for spos < sEnd {
		opc := src[spos]
		spos++

		// Categorise opcode
		switch {
		case opc == 0x06: // eos
			// End of stream: skip 7 more bytes and stop
			spos += 7
			return dpos, nil

		case opc == 0x0E || opc == 0x16: // nop
			continue

		case opc >= 0xF0:
			// Match only: sml_m (0xF1..0xFF) or lrg_m (0xF0)
			var M int
			if opc == 0xF0 {
				if spos >= sEnd {
					return dpos, errors.New("lzvn: truncated lrg_m opcode")
				}
				M = int(src[spos]) + 16
				spos++
			} else {
				M = int(opc & 0x0F)
			}
			if dPrev == 0 {
				return dpos, errors.New("lzvn: match without prior distance")
			}
			if err := lzvnCopyMatch(dst, dpos, dPrev, M, dEnd); err != nil {
				return dpos, err
			}
			dpos += M

		case opc >= 0xE0:
			// Literal only: sml_l (0xE1..0xEF) or lrg_l (0xE0)
			var L int
			if opc == 0xE0 {
				if spos >= sEnd {
					return dpos, errors.New("lzvn: truncated lrg_l opcode")
				}
				L = int(src[spos]) + 16
				spos++
			} else {
				L = int(opc & 0x0F)
			}
			if spos+L > sEnd || dpos+L > dEnd {
				return dpos, errors.New("lzvn: literal overflow")
			}
			copy(dst[dpos:], src[spos:spos+L])
			dpos += L
			spos += L

		case opc == 0xA0 || (opc >= 0xA1 && opc < 0xC0):
			// med_d: 101LLMMM DDDDDDMM DDDDDDDD
			// opc = 1 0 1 L L M M M
			if spos+2 > sEnd {
				return dpos, errors.New("lzvn: truncated med_d opcode")
			}
			b1 := src[spos]
			b2 := src[spos+1]
			spos += 2
			L := int((opc >> 3) & 0x03)
			M := int((opc&0x07)<<2|b1&0x03) + 3
			D := int(b1>>2) | int(b2)<<6
			if D == 0 {
				return dpos, errors.New("lzvn: zero match distance")
			}
			if spos+L > sEnd || dpos+L > dEnd {
				return dpos, errors.New("lzvn: med_d literal overflow")
			}
			copy(dst[dpos:], src[spos:spos+L])
			dpos += L
			spos += L
			if err := lzvnCopyMatch(dst, dpos, D, M, dEnd); err != nil {
				return dpos, err
			}
			dpos += M
			dPrev = D

		default:
			// Determine opcode type from lower 3 bits
			lo3 := opc & 0x07
			switch lo3 {
			case 0, 1, 2, 3, 4, 5: // sml_d: LLMMMDDD DDDDDDDD
				if spos >= sEnd {
					return dpos, errors.New("lzvn: truncated sml_d opcode")
				}
				b1 := src[spos]
				spos++
				L := int(opc >> 6)
				M := int((opc>>3)&0x07) + 3
				D := int(opc&0x07)<<8 | int(b1)
				if D == 0 {
					return dpos, errors.New("lzvn: zero match distance")
				}
				if spos+L > sEnd || dpos+L > dEnd {
					return dpos, errors.New("lzvn: sml_d literal overflow")
				}
				copy(dst[dpos:], src[spos:spos+L])
				dpos += L
				spos += L
				if err := lzvnCopyMatch(dst, dpos, D, M, dEnd); err != nil {
					return dpos, err
				}
				dpos += M
				dPrev = D

			case 6: // pre_d: LLMMM110 (use prev D)
				L := int(opc >> 6)
				M := int((opc>>3)&0x07) + 3
				if dPrev == 0 {
					return dpos, errors.New("lzvn: pre_d without prior distance")
				}
				if spos+L > sEnd || dpos+L > dEnd {
					return dpos, errors.New("lzvn: pre_d literal overflow")
				}
				copy(dst[dpos:], src[spos:spos+L])
				dpos += L
				spos += L
				if err := lzvnCopyMatch(dst, dpos, dPrev, M, dEnd); err != nil {
					return dpos, err
				}
				dpos += M

			case 7: // lrg_d: LLMMM111 DDDDDDDD DDDDDDDD
				if spos+2 > sEnd {
					return dpos, errors.New("lzvn: truncated lrg_d opcode")
				}
				D := int(binary.LittleEndian.Uint16(src[spos:]))
				spos += 2
				L := int(opc >> 6)
				M := int((opc>>3)&0x07) + 3
				if D == 0 {
					return dpos, errors.New("lzvn: zero match distance")
				}
				if spos+L > sEnd || dpos+L > dEnd {
					return dpos, errors.New("lzvn: lrg_d literal overflow")
				}
				copy(dst[dpos:], src[spos:spos+L])
				dpos += L
				spos += L
				if err := lzvnCopyMatch(dst, dpos, D, M, dEnd); err != nil {
					return dpos, err
				}
				dpos += M
				dPrev = D
			}
		}
	}
	return dpos, nil
}

// lzvnCopyMatch copies M bytes from dst[dpos-D:] to dst[dpos:].
// Handles overlapping matches (D < M) correctly.
func lzvnCopyMatch(dst []byte, dpos, D, M, dEnd int) error {
	if D <= 0 || dpos-D < 0 {
		return errors.New("lzvn: invalid match distance")
	}
	if dpos+M > dEnd {
		return errors.New("lzvn: match overflow")
	}
	src := dpos - D
	for i := 0; i < M; i++ {
		dst[dpos+i] = dst[src+i]
	}
	return nil
}

// ---------------------------------------------------------------------------
// LZVN encoder
// ---------------------------------------------------------------------------

// lzvn_encode_entry stores the 4-entry hash bucket (indices + 4-byte values).
type lzvnEncodeEntry struct {
	indices [4]int32
	values  [4]uint32
}

type lzvnMatchInfo struct {
	mBegin int
	mEnd   int
	M      int
	D      int
	K      int // score: M - distance_penalty
}

type lzvnEncoderState struct {
	src           []byte
	srcBegin      int
	srcEnd        int
	srcLiteral    int // start of pending literal
	srcCurrent    int
	srcCurrentEnd int
	dst           []byte
	dPrev         int

	pending lzvnMatchInfo
	table   []lzvnEncodeEntry
}

var noMatch = lzvnMatchInfo{}

// hash3i hashes 3 bytes from i (24-bit input) into [0, lzvnEncodeHashValues).
func hash3i(i uint32) int {
	i &= 0xFFFFFF
	h := (i * (1 + (1 << 6) + (1 << 12))) >> 12
	return int(h & (lzvnEncodeHashValues - 1))
}

// trailingZeroBytes returns the number of zero bytes [0,4] in x starting from LSB.
func trailingZeroBytes(x uint32) int {
	if x == 0 {
		return 4
	}
	return bits.TrailingZeros32(x) >> 3
}

// nmatch4 returns the number of matching bytes [0,4] between src[i] and src[j].
func nmatch4(src []byte, i, j int) int {
	vi := binary.LittleEndian.Uint32(src[i:])
	vj := binary.LittleEndian.Uint32(src[j:])
	return trailingZeroBytes(vi ^ vj)
}

// lzvnFindMatchN looks for a match of at least 3 bytes with n already known.
func lzvnFindMatchN(src []byte, srcBegin, srcEnd, lBegin, m0Begin, mBegin, n int, match *lzvnMatchInfo) bool {
	if n < 3 {
		return false
	}
	D := mBegin - m0Begin
	if D <= 0 || D > lzvnEncodeMaxDistance {
		return false
	}
	mEnd := mBegin + n
	for n == 4 && mEnd+4 < srcEnd {
		n = nmatch4(src, mEnd, mEnd-D)
		mEnd += n
	}
	// expand backwards over literal
	for m0Begin > srcBegin && mBegin > lBegin && src[mBegin-1] == src[m0Begin-1] {
		m0Begin--
		mBegin--
	}
	M := mEnd - mBegin
	match.mBegin = mBegin
	match.mEnd = mEnd
	match.M = M
	match.D = D
	if D < 0x600 {
		match.K = M - 2
	} else {
		match.K = M - 3
	}
	return true
}

func lzvnFindMatch(src []byte, srcBegin, srcEnd, lBegin, m0Begin, mBegin int, match *lzvnMatchInfo) bool {
	n := nmatch4(src, mBegin, m0Begin)
	return lzvnFindMatchN(src, srcBegin, srcEnd, lBegin, m0Begin, mBegin, n, match)
}

func updateBest(best, candidate *lzvnMatchInfo) {
	if candidate.K > best.K || (candidate.K == best.K && candidate.mEnd > best.mEnd+1) {
		*best = *candidate
	}
}

// lzvnInitTable fills the hash table with a sentinel value.
func lzvnInitTable(state *lzvnEncoderState) {
	index := state.srcBegin - lzvnEncodeMaxDistance
	if index < state.srcBegin {
		index = state.srcBegin
	}
	value := binary.LittleEndian.Uint32(state.src[index:])
	e := lzvnEncodeEntry{}
	for i := 0; i < 4; i++ {
		e.indices[i] = int32(index)
		e.values[i] = value
	}
	for u := range state.table {
		state.table[u] = e
	}
}

// lzvnEmitLiteral emits L literal bytes from src[srcLiteral:].
func lzvnEmitLiteral(state *lzvnEncoderState, L int) {
	p := state.src[state.srcLiteral : state.srcLiteral+L]
	state.dst = lzvnEmitLiteralBytes(p, state.dst)
	state.srcLiteral += L
}

func lzvnEmitLiteralBytes(p []byte, dst []byte) []byte {
	// Callers (lzvnEmitLiteral / lzvnEncodeBuffer's trailing emit)
	// always pass L <= 271 — the lzvnEncodeMaxLiteralBacklog cap +
	// lzvnEncodeMinMargin trailing window jointly bound the chunk
	// size. So the for-loop runs at most once and the x>271 clamp
	// is unreachable from production callers.
	L := len(p)
	if L > 15 {
		dst = append(dst, 0xE0, byte(L-16))
		dst = append(dst, p[:L]...)
		return dst
	}
	if L > 0 {
		dst = append(dst, 0xE0|byte(L))
		dst = append(dst, p[:L]...)
	}
	return dst
}

func lzvnEmitMatch(state *lzvnEncoderState, match lzvnMatchInfo) {
	L := match.mBegin - state.srcLiteral
	p := state.src[state.srcLiteral:]
	state.dst = lzvnEmitLMD(p, state.dst, L, match.M, match.D, state.dPrev)
	state.dPrev = match.D
	state.srcLiteral = match.mEnd
}

// lzvnEmitLMD emits a (L literals, M match, D distance) triplet.
// L must be <= 3 after preamble handling below.
func lzvnEmitLMD(src []byte, dst []byte, L, M, D, dPrev int) []byte {
	// Flush large literal prefix first. Like lzvnEmitLiteralBytes,
	// callers cap L at the backlog limit (271) so the for-loop ever
	// runs at most once and the x>271 clamp is unreachable.
	if L > 15 {
		dst = append(dst, 0xE0, byte(L-16))
		dst = append(dst, src[:L]...)
		src = src[L:]
		L = 0
	}
	if L > 3 {
		dst = append(dst, 0xE0|byte(L))
		dst = append(dst, src[:L]...)
		src = src[L:]
		L = 0
	}

	// Determine match opcode (x = min(10-2*L, M) - 3)
	x := 10 - 2*L
	if M < x {
		x = M
	}
	remaining := M - x
	x -= 3
	// x is now in [0, 7-2*L]

	// literal bytes
	var litBytes [4]byte
	copy(litBytes[:], src[:L])

	if D == dPrev {
		if L == 0 {
			dst = append(dst, byte(0xF0|(x+3)))
		} else {
			dst = append(dst, byte((L<<6)|(x<<3)|6))
			dst = append(dst, litBytes[:L]...)
		}
	} else if D < 2048-2*256 {
		// short distance: D>>8 in [0,5]
		dst = append(dst, byte((D>>8)|(L<<6)|(x<<3)))
		dst = append(dst, byte(D&0xFF))
		dst = append(dst, litBytes[:L]...)
	} else if D >= (1<<14) || remaining == 0 || (x+3)+remaining > 34 {
		// long distance
		dst = append(dst, byte((L<<6)|(x<<3)|7))
		dst = append(dst, byte(D), byte(D>>8))
		dst = append(dst, litBytes[:L]...)
	} else {
		// medium distance
		x += remaining
		remaining = 0
		dst = append(dst, byte(0xA0|(x>>2)|(L<<3)))
		dst = append(dst, byte(D<<2|x&3), byte(D>>6))
		dst = append(dst, litBytes[:L]...)
	}

	// Emit remaining match bytes
	for remaining > 15 {
		x2 := remaining
		if x2 > 271 {
			x2 = 271
		}
		dst = append(dst, 0xF0, byte(x2-16))
		remaining -= x2
	}
	if remaining > 0 {
		dst = append(dst, byte(0xF0|remaining))
	}
	return dst
}

func lzvnEmitEndOfStream(state *lzvnEncoderState) {
	// EOS = byte 0x06 + 7 zero bytes
	state.dst = append(state.dst, 0x06, 0, 0, 0, 0, 0, 0, 0)
}

func lzvnEncode(state *lzvnEncoderState) {
	for ; state.srcCurrent < state.srcCurrentEnd; state.srcCurrent++ {
		vi := binary.LittleEndian.Uint32(state.src[state.srcCurrent:])
		h := hash3i(vi)
		e := state.table[h]

		// Update table
		updated := lzvnEncodeEntry{}
		updated.indices[0] = int32(state.srcCurrent)
		updated.indices[1] = e.indices[0]
		updated.indices[2] = e.indices[1]
		updated.indices[3] = e.indices[2]
		updated.values[0] = vi
		updated.values[1] = e.values[0]
		updated.values[2] = e.values[1]
		updated.values[3] = e.values[2]

		if state.srcCurrent < state.srcLiteral {
			state.table[h] = updated
			continue
		}

		diffs := [4]uint32{
			e.values[0] ^ vi,
			e.values[1] ^ vi,
			e.values[2] ^ vi,
			e.values[3] ^ vi,
		}

		incoming := noMatch
		for k := 0; k < 4; k++ {
			ik := int(e.indices[k])
			nk := trailingZeroBytes(diffs[k])
			var m1 lzvnMatchInfo
			if lzvnFindMatchN(state.src, state.srcBegin, state.srcEnd,
				state.srcLiteral, ik, state.srcCurrent, nk, &m1) {
				updateBest(&incoming, &m1)
			}
		}

		// Check candidate at previous distance
		if state.dPrev != 0 {
			var m1 lzvnMatchInfo
			if lzvnFindMatch(state.src, state.srcBegin, state.srcEnd,
				state.srcLiteral, state.srcCurrent-state.dPrev, state.srcCurrent, &m1) {
				m1.K = m1.M - 1 // fix K for D_prev
				updateBest(&incoming, &m1)
			}
		}

		if incoming.M == 0 {
			// No match found
			if state.srcCurrent-state.srcLiteral >= lzvnEncodeMaxLiteralBacklog {
				if state.pending.M != 0 {
					lzvnEmitMatch(state, state.pending)
					state.pending = noMatch
				} else {
					lzvnEmitLiteral(state, 271)
				}
			}
			state.table[h] = updated
			continue
		}

		if state.pending.M == 0 {
			state.pending = incoming
		} else {
			if state.pending.mEnd <= incoming.mBegin {
				// No overlap
				lzvnEmitMatch(state, state.pending)
				state.pending = incoming
			} else {
				if incoming.K > state.pending.K {
					state.pending = incoming
				}
				lzvnEmitMatch(state, state.pending)
				state.pending = noMatch
			}
		}

		state.table[h] = updated
	}
}

// lzvnEncodeBuffer compresses src into a LZVN stream. The caller is
// responsible for keeping len(src) <= lzvnEncodeMaxSrcSize; the LZVN
// block header packs the length into a uint32, so input larger than
// 4 GiB would silently truncate the n_raw_bytes field. The public
// `Compress` entry point only ever passes <= 4 KiB to this function,
// so the limit is never reached in practice.
func lzvnEncodeBuffer(src []byte) []byte {
	srcSize := len(src)

	// Worst case: uncompressed + overhead
	dst := make([]byte, 0, srcSize+64)

	table := make([]lzvnEncodeEntry, lzvnEncodeHashValues)

	state := &lzvnEncoderState{
		src:      src,
		srcBegin: 0,
		srcEnd:   srcSize,
		dst:      dst,
		table:    table,
	}

	if srcSize >= lzvnEncodeMinSrcSize {
		state.srcCurrentEnd = srcSize - lzvnEncodeMinMargin
		lzvnInitTable(state)
		lzvnEncode(state)
	}

	// Emit remaining literals
	remaining := state.srcEnd - state.srcLiteral
	if remaining > 0 {
		lzvnEmitLiteral(state, remaining)
	}

	// Emit EOS
	lzvnEmitEndOfStream(state)

	return state.dst
}
