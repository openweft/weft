// Package lz4 implements the LZ4 block format (compress and decompress). The
// compressor's match extension — LZ4's hot "count the matching bytes" inner
// loop — is delegated to github.com/go-simd/matchlen, whose SIMD
// common-prefix kernel makes it fast on all six 64-bit Go targets.
//
// The match-finder is a single-cell hash table keyed on a 6-byte sequence (the
// reference LZ4 / pierrec hash), probing several adjacent positions per step
// and using lazy matching — if position+1 yields a strictly longer match the
// shorter one is deferred — for a better ratio on text.
package lz4

import (
	"encoding/binary"

	"github.com/go-simd/matchlen"
)

const (
	minMatch     = 4
	hashLog      = 16 // 64 KiB table — the reference LZ4 fast-mode size
	hashTableLen = 1 << hashLog
	winSize      = 1 << 16 // 64 KiB window; the max encodable match distance is winSize-1 (65535), since LZ4 stores the offset in a 16-bit field (a distance of exactly 65536 would encode as 0 and be rejected as corrupt on decode). Match-finder distance checks therefore use `< winSize`, not `<= winSize`.
	mfLimit      = 12      // matches may not start in the last 12 bytes
	lastLiterals = 5       // the last 5 bytes are always literals
	adaptSkipLog = 7       // skip ramp on incompressible spans (matches pierrec)

	lazyEnabled = true // lazy matching: defer a match if pos+1 is longer
	// lazyMaxLen bounds the lazy lookahead to short matches. A long match is
	// already a big win, so peeking ahead rarely improves it and the extra
	// hash+confirm+count is wasted; skipping the peek there recovers speed at a
	// negligible ratio cost.
	lazyMaxLen = 64
)

// hash6 hashes the lower 6 bytes of x, exactly as pierrec/reference LZ4 do for
// fast-mode compression. A 6-byte key disperses far better than a 4-byte one,
// which both finds more candidates and cuts false-positive confirmations. Note
// it reads only the low 48 bits, so the key for position p+k is hash6(seq>>8*k)
// where seq is the 8-byte load at p — no extra memory load is needed.
func hash6(x uint64) uint32 {
	const prime6bytes = 227718039650203
	return uint32(((x << (64 - 48)) * prime6bytes) >> (64 - hashLog))
}

func u32(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }
func u64(b []byte) uint64 { return binary.LittleEndian.Uint64(b) }

// CompressBlock compresses src into a standard LZ4 block.
func CompressBlock(src []byte) []byte { return compress(src, matchlen.MatchLen) }

// compress is parameterised by the match-counter so benchmarks can compare the
// SIMD matchlen against a scalar reference.
//
// The parse keeps one position per 6-byte hash. Each step probes the table at
// ip, ip+1 and ip+2 (cheap: a single 8-byte load covers all three), taking the
// first confirmed hit. On a hit it then applies lazy matching: it looks one
// byte ahead and, if that yields a strictly longer match, it defers the current
// one (emitting one extra literal) and takes the longer match instead. That is
// the classic LZ4 ratio lever for text.
func compress(src []byte, mlen func(a, b []byte) int) []byte {
	dst := make([]byte, 0, len(src)/2+16)
	if len(src) < mfLimit+minMatch {
		return emitLast(dst, src)
	}
	// Zero-init (Go memclr): a slot of 0 means "empty or position 0"; the win
	// + u32 checks below disambiguate, so no -1 fill is needed.
	var table [hashTableLen]int32
	matchLimit := len(src) - lastLiterals
	limit := len(src) - mfLimit
	anchor := 0
	ip := 0

	for {
		// Search forward for the next match, probing three adjacent positions
		// per outer step from a single 8-byte load (the keys for ip, ip+1, ip+2
		// are hash6(seq), hash6(seq>>8), hash6(seq>>16)), ramping the skip
		// distance on incompressible spans. Every probed position is inserted so
		// later matches see more candidates.
		var ref int
		seq := u64(src[ip:])
		for {
			h0 := hash6(seq)
			r0 := int(table[h0])
			table[h0] = int32(ip)
			if ip-r0 < winSize && r0 < ip && u32(src[r0:]) == uint32(seq) {
				ref = r0
				break
			}
			h1 := hash6(seq >> 8)
			r1 := int(table[h1])
			table[h1] = int32(ip + 1)
			if ip+1-r1 < winSize && r1 < ip+1 && u32(src[r1:]) == uint32(seq>>8) {
				ip, ref = ip+1, r1
				break
			}
			h2 := hash6(seq >> 16)
			r2 := int(table[h2])
			table[h2] = int32(ip + 2)
			if ip+2-r2 < winSize && r2 < ip+2 && u32(src[r2:]) == uint32(seq>>16) {
				ip, ref = ip+2, r2
				break
			}
			ip += 3 + (ip-anchor)>>adaptSkipLog
			if ip > limit {
				return emitLast(dst, src[anchor:])
			}
			seq = u64(src[ip:])
		}

		// Lazy matching: measure the match at ip, then peek one byte ahead. If
		// ip+1 yields a strictly longer match, defer the current one (emit one
		// extra literal) and take the longer match instead. A single lookahead
		// step captures most of the ratio win at a fraction of the cost of a
		// chained lazy search.
		ml := minMatch + mlen(src[ip+minMatch:matchLimit], src[ref+minMatch:])
		if lazyEnabled && ml < lazyMaxLen && ip+1 <= limit {
			lseq := u64(src[ip+1:])
			h := hash6(lseq)
			nref := int(table[h])
			table[h] = int32(ip + 1)
			if ip+1-nref < winSize && nref < ip+1 && u32(src[nref:]) == uint32(lseq) {
				if nml := minMatch + mlen(src[ip+1+minMatch:matchLimit], src[nref+minMatch:]); nml > ml {
					ip++
					ref, ml = nref, nml
				}
			}
		}

		// Extend the match backwards over the pending literals.
		mStart, mref := ip, ref
		for mStart > anchor && mref > 0 && src[mStart-1] == src[mref-1] {
			mStart--
			mref--
			ml++
		}
		dst = emitSequence(dst, src[anchor:mStart], mStart-mref, ml)
		ip = mStart + ml
		anchor = ip
		if ip > limit {
			return emitLast(dst, src[anchor:])
		}
		// Seed the table with an interior position for better next matches.
		table[hash6(u64(src[ip-2:]))] = int32(ip - 2)
	}
}

func emitLength(dst []byte, n int) []byte {
	for n >= 255 {
		dst = append(dst, 255)
		n -= 255
	}
	return append(dst, byte(n))
}

func emitSequence(dst, lits []byte, offset, matchLen int) []byte {
	ll, ml := len(lits), matchLen-minMatch
	var token byte
	if ll >= 15 {
		token = 0xF0
	} else {
		token = byte(ll) << 4
	}
	if ml >= 15 {
		token |= 0x0F
	} else {
		token |= byte(ml)
	}
	dst = append(dst, token)
	if ll >= 15 {
		dst = emitLength(dst, ll-15)
	}
	dst = append(dst, lits...)
	dst = append(dst, byte(offset), byte(offset>>8))
	if ml >= 15 {
		dst = emitLength(dst, ml-15)
	}
	return dst
}

func emitLast(dst, lits []byte) []byte {
	ll := len(lits)
	if ll >= 15 {
		dst = append(dst, 0xF0)
		dst = emitLength(dst, ll-15)
	} else {
		dst = append(dst, byte(ll)<<4)
	}
	return append(dst, lits...)
}

// DecompressBlock decompresses an LZ4 block. dstCap is a hint for the output
// capacity (the decompressed size if known).
//
// The decode hot loop follows pierrec/lz4's pure-Go structure, which is ~2×
// faster than a naive append-based loop: the output is written by *index* into
// a preallocated buffer (no per-token slice-append cap checks), and the two
// dominant cases — a literal run of ≤16 bytes and a non-overlapping match of
// ≤16 bytes — each take a *fixed* 16-byte copy that the compiler lowers to a
// couple of word moves, with no length-variable copy() call and no overlap
// loop. Longer/overlapping runs fall to the bulk copy paths.
//
// The buffer is overallocated by decodeSlack bytes so those unconditional
// 16-byte copies never run off the end; the real output length is di.
func DecompressBlock(src []byte, dstCap int) ([]byte, error) {
	if dstCap < 0 {
		dstCap = 0
	}
	dst := make([]byte, dstCap+decodeSlack)
	dst, di, err := decodeInto(dst, 0, src)
	if err != nil {
		return nil, err
	}
	return dst[:di], nil
}

// decodeInto decodes a single LZ4 block src into dst, starting writes at index
// diStart, and returns the (possibly grown) buffer and the new write index.
//
// Matches may reference any earlier byte in dst — including bytes written
// before diStart. That "dictionary" behaviour is what Apple's LZ4 frame format
// relies on: each 64 KiB block's matches may reach back into the previous
// block's output (a shared 64 KiB window). Callers that decode an isolated
// block pass diStart == 0; DecompressApple prefixes the previous window and
// passes diStart == len(window).
func decodeInto(dst []byte, diStart int, src []byte) ([]byte, int, error) {
	di := diStart // bytes written so far (dst[:di] is valid output + history)
	ip := 0
	for ip < len(src) {
		token := int(src[ip])
		ip++
		ll := token >> 4
		if ll == 15 {
			for {
				if ip >= len(src) {
					return nil, 0, errCorrupt
				}
				b := src[ip]
				ip++
				ll += int(b)
				if b != 255 {
					break
				}
			}
		}
		if ip+ll > len(src) {
			return nil, 0, errCorrupt
		}
		// Ensure room for the literals plus the 16-byte shortcut overrun.
		if di+ll+decodeSlack > len(dst) {
			dst = growDecode(dst, di+ll+decodeSlack)
		}
		if ll <= 16 && ip+16 <= len(src) {
			// Shortcut 1: enough slack in src and dst — copy a fixed 16
			// bytes (the compiler inlines it), even if only ll are literals.
			copy(dst[di:di+16], src[ip:ip+16])
		} else {
			copy(dst[di:di+ll], src[ip:ip+ll])
		}
		ip += ll
		di += ll
		if ip == len(src) {
			break
		}
		if ip+2 > len(src) {
			return nil, 0, errCorrupt
		}
		offset := int(src[ip]) | int(src[ip+1])<<8
		ip += 2
		ml := token&0x0F + minMatch
		mpos := di - offset
		if offset <= 0 || mpos < 0 {
			return nil, 0, errCorrupt
		}
		if ml <= 16 {
			// Shortcut 2: a short match (ml <= 16) that is fully behind di
			// (ml <= offset, so source and the 16-byte copy don't overlap) and
			// has output room — one fixed 16-byte copy the compiler inlines,
			// then on to the next token. The bytes past ml are a harmless
			// read-ahead that the next write overwrites.
			if ml <= offset && di+16 <= len(dst) {
				copy(dst[di:di+16], dst[mpos:mpos+16])
				di += ml
				continue
			}
		} else if token&0x0F == 15 {
			for {
				if ip >= len(src) {
					return nil, 0, errCorrupt
				}
				b := src[ip]
				ip++
				ml += int(b)
				if b != 255 {
					break
				}
			}
		}
		if di+ml+decodeSlack > len(dst) {
			dst = growDecode(dst, di+ml+decodeSlack)
		}
		di = matchCopy(dst, di, mpos, ml)
	}
	return dst, di, nil
}

// decodeSlack is the overrun the ≤16-byte shortcut copies may write past the
// logical end of the output; the decode buffer always keeps this much spare.
const decodeSlack = 16

// growDecode grows dst to at least n bytes (amortised doubling), preserving its
// contents. It is only hit when the dstCap hint underestimates the output.
func growDecode(dst []byte, n int) []byte {
	c := cap(dst) * 2
	if c < n {
		c = n
	}
	grown := make([]byte, c)
	copy(grown, dst)
	return grown
}

// matchCopy writes an ml-byte LZ4 match into dst[di:di+ml], copying from
// dst[mpos:] (mpos = di-offset), and returns the new write index di+ml. The
// caller has already ensured dst has room. The destination region overlaps the
// source whenever offset (= di-mpos) < ml, which LZ4 uses for run-length fills
// (e.g. offset 1 = repeat one byte). The result is byte-identical to the scalar
// `for k := 0; k < ml; k++ { dst[di+k] = dst[mpos+k] }` loop: each output byte
// reads the byte one offset earlier in the *output being built*.
//
//   - offset >= ml: source and destination are disjoint, so a single copy() —
//     the runtime's word-at-a-time memmove — is exact.
//   - offset < ml: lay down the `offset`-byte pattern, then double it forward;
//     copy() within one slice handles the forward overlap correctly because
//     each doubling copies a region that is fully written before it is read.
func matchCopy(dst []byte, di, mpos, ml int) int {
	offset := di - mpos
	if offset >= ml {
		copy(dst[di:di+ml], dst[mpos:mpos+ml])
		return di + ml
	}
	// Overlapping run-length fill (offset < ml). expanded is the output region
	// being grown, starting at the match source mpos and running to di+ml. We
	// double the already-written pattern forward; each copy is clamped to the
	// region's end so it never runs past the buffer. This is pierrec/lz4's
	// expansion (clamped instead of relying on an exactly-sized dst).
	expanded := dst[mpos : di+ml]
	for n := offset; n < len(expanded); n *= 2 {
		copy(expanded[n:], expanded[:n])
	}
	return di + ml
}

type lzError string

func (e lzError) Error() string { return string(e) }

const errCorrupt = lzError("lz4: corrupt block")
