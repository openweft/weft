// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, the go-tpm2/attest authors. All rights reserved.

package attest

// This file is the deterministic wire codec for the attestation protocol's
// messages. The design goals are: stable across versions, self-describing
// enough to reject a wrong message type, and trivially round-trippable so the
// codec is fully testable off any TPM.
//
// Encoding rules (all integers big-endian, the TPM-native byte order):
//
//	frame  := magic:u32 | version:u8 | kind:u8 | body
//	[]byte := len:u32 | bytes
//	[32]byte (fixed nonce) := 32 raw bytes (no length prefix)
//	map[int][]byte := count:u32 | { index:u32 | []byte }...   (ascending index)
//
// A frame always begins with the 4-byte magic and a 1-byte version so a
// decoder can reject foreign or future-incompatible buffers before touching
// the body, and a 1-byte kind tag so a decoder can refuse to unmarshal a
// message of the wrong type into the wrong struct. Maps are emitted in
// ascending key order so the encoding of a given value is byte-for-byte stable
// (important for hashing/signing and for golden-file tests).

import (
	"encoding/binary"
	"sort"

	"github.com/go-tpm2/common"
)

// wireMagic prefixes every frame: ASCII "ATT2" (go-tpm2 ATTestation, v2 wire
// family). A decoder that does not see this rejects the buffer immediately.
const wireMagic uint32 = 0x41545432 // "ATT2"

// wireVersion is the codec version. Bumping it lets a future revision change
// the body layout while still rejecting old/new peers cleanly. Encoders stamp
// it; decoders require an exact match.
const wireVersion uint8 = 1

// Message kind tags. Each wire message carries exactly one of these in the
// frame header, so the decoder binds a buffer to its concrete type and refuses
// a mismatched unmarshal. They are stable identifiers — never renumber.
const (
	kindEnrollRequest      uint8 = 1
	kindEnrollChallenge    uint8 = 2
	kindEnrollProof        uint8 = 3
	kindAdmissionRequest   uint8 = 4
	kindAdmissionChallenge uint8 = 5
	kindAdmissionResponse  uint8 = 6
)

// Codec error sentinels (typed and constant for ==).
const (
	// ErrShortBuffer is returned when a buffer ends before a field could be
	// fully decoded.
	ErrShortBuffer = common.Error("attest: short buffer")
	// ErrBadMagic is returned when a frame does not begin with wireMagic.
	ErrBadMagic = common.Error("attest: bad frame magic")
	// ErrBadVersion is returned when a frame's version != wireVersion.
	ErrBadVersion = common.Error("attest: unsupported codec version")
	// ErrWrongKind is returned when a frame's kind tag does not match the
	// message type being decoded into.
	ErrWrongKind = common.Error("attest: wrong message kind")
	// ErrTrailingBytes is returned when a frame has bytes left over after the
	// message was fully decoded (a strict, non-malleable decode).
	ErrTrailingBytes = common.Error("attest: trailing bytes after message")
)

// enc accumulates a frame body with big-endian primitives.
type enc struct {
	b []byte
}

// frame starts a new encoder buffer with the magic/version/kind header.
func frame(kind uint8) *enc {
	e := &enc{}
	var hdr [6]byte
	binary.BigEndian.PutUint32(hdr[0:4], wireMagic)
	hdr[4] = wireVersion
	hdr[5] = kind
	e.b = append(e.b, hdr[:]...)
	return e
}

// bytes appends a length-prefixed byte slice (u32 length, then the bytes).
func (e *enc) bytes(p []byte) *enc {
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(p)))
	e.b = append(e.b, l[:]...)
	e.b = append(e.b, p...)
	return e
}

// fixed32 appends a fixed 32-byte field with no length prefix.
func (e *enc) fixed32(p [32]byte) *enc {
	e.b = append(e.b, p[:]...)
	return e
}

// pcrMap appends a map[int][]byte in ascending key order: u32 count, then
// (u32 index, length-prefixed value) per entry.
func (e *enc) pcrMap(m map[int][]byte) *enc {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	var cnt [4]byte
	binary.BigEndian.PutUint32(cnt[:], uint32(len(keys)))
	e.b = append(e.b, cnt[:]...)
	for _, k := range keys {
		var idx [4]byte
		binary.BigEndian.PutUint32(idx[:], uint32(k))
		e.b = append(e.b, idx[:]...)
		e.bytes(m[k])
	}
	return e
}

// out returns the finished frame.
func (e *enc) out() []byte { return e.b }

// dec walks a frame body after the header has been validated.
type dec struct {
	b   []byte
	off int
	err error
}

// open validates the frame header (magic, version, kind) and returns a decoder
// positioned at the body. The first error short-circuits all later reads.
func open(buf []byte, kind uint8) *dec {
	if len(buf) < 6 {
		return &dec{err: ErrShortBuffer}
	}
	if binary.BigEndian.Uint32(buf[0:4]) != wireMagic {
		return &dec{err: ErrBadMagic}
	}
	if buf[4] != wireVersion {
		return &dec{err: ErrBadVersion}
	}
	if buf[5] != kind {
		return &dec{err: ErrWrongKind}
	}
	return &dec{b: buf, off: 6}
}

// bytes reads a length-prefixed byte slice (copied out of the frame).
func (d *dec) bytes() []byte {
	if d.err != nil {
		return nil
	}
	if d.off+4 > len(d.b) {
		d.err = ErrShortBuffer
		return nil
	}
	n := int(binary.BigEndian.Uint32(d.b[d.off : d.off+4]))
	d.off += 4
	if n < 0 || d.off+n > len(d.b) {
		d.err = ErrShortBuffer
		return nil
	}
	out := make([]byte, n)
	copy(out, d.b[d.off:d.off+n])
	d.off += n
	return out
}

// fixed32 reads a fixed 32-byte field.
func (d *dec) fixed32() [32]byte {
	var out [32]byte
	if d.err != nil {
		return out
	}
	if d.off+32 > len(d.b) {
		d.err = ErrShortBuffer
		return out
	}
	copy(out[:], d.b[d.off:d.off+32])
	d.off += 32
	return out
}

// pcrMap reads a map[int][]byte (u32 count, then index/value pairs).
func (d *dec) pcrMap() map[int][]byte {
	if d.err != nil {
		return nil
	}
	if d.off+4 > len(d.b) {
		d.err = ErrShortBuffer
		return nil
	}
	cnt := int(binary.BigEndian.Uint32(d.b[d.off : d.off+4]))
	d.off += 4
	// A 32-bit count cannot exceed the buffer's byte length; any over-large
	// count is caught when the per-entry reads run off the end. (cnt fits in a
	// 64-bit int, so it is never negative.)
	m := make(map[int][]byte, min(cnt, len(d.b)))
	for i := 0; i < cnt; i++ {
		if d.off+4 > len(d.b) {
			d.err = ErrShortBuffer
			return nil
		}
		idx := int(binary.BigEndian.Uint32(d.b[d.off : d.off+4]))
		d.off += 4
		m[idx] = d.bytes()
		if d.err != nil {
			return nil
		}
	}
	return m
}

// finish reports the decode error if any, or ErrTrailingBytes if the frame had
// unconsumed bytes (a strict decode rejects malleable padding).
func (d *dec) finish() error {
	if d.err != nil {
		return d.err
	}
	if d.off != len(d.b) {
		return ErrTrailingBytes
	}
	return nil
}
