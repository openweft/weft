// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, the go-tpm2/common authors. All rights reserved.

package common

// The TPM 2.0 wire encoding is BIG-ENDIAN throughout. TCG "TPM 2.0
// Part 1: Architecture" mandates canonical (network) byte order for all
// multi-byte values; every put/get helper below encodes most-significant
// byte first.

// HeaderSize is the byte length of the common TPM 2.0 command and
// response header: a 16-bit tag, a 32-bit size, and a 32-bit code.
//
//	command:  [ tag:u16 | commandSize:u32  | commandCode:u32  | params... ]
//	response: [ tag:u16 | responseSize:u32 | responseCode:u32 | params... ]
//
// TCG "TPM 2.0 Part 1: Architecture", "Command/Response Structure";
// TCG "TPM 2.0 Part 2: Structures", TPM2_COMMAND_HEADER /
// TPM2_RESPONSE_HEADER.
const HeaderSize = 10

// Codec error sentinels. They are typed (Error) and constant, so callers
// may compare with ==.
const (
	// ErrShortBuffer is returned when a buffer is too small to hold the
	// structure being parsed (header, size prefix, or declared payload).
	ErrShortBuffer = Error("tpm2: buffer too short")
	// ErrSizeMismatch is returned when an embedded length field does not
	// agree with the actual buffer length.
	ErrSizeMismatch = Error("tpm2: declared size does not match buffer length")
)

// --- big-endian primitive put/get helpers ---

// PutU8 appends v to dst.
func PutU8(dst []byte, v uint8) []byte { return append(dst, v) }

// PutU16 appends v to dst in big-endian order.
func PutU16(dst []byte, v uint16) []byte {
	return append(dst, byte(v>>8), byte(v))
}

// PutU32 appends v to dst in big-endian order.
func PutU32(dst []byte, v uint32) []byte {
	return append(dst, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

// PutU64 appends v to dst in big-endian order.
func PutU64(dst []byte, v uint64) []byte {
	return append(dst,
		byte(v>>56), byte(v>>48), byte(v>>40), byte(v>>32),
		byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

// GetU8 reads one byte from b at off. ok is false if b is too short.
func GetU8(b []byte, off int) (v uint8, ok bool) {
	if off < 0 || off+1 > len(b) {
		return 0, false
	}
	return b[off], true
}

// GetU16 reads a big-endian uint16 from b at off. ok is false if b is
// too short.
func GetU16(b []byte, off int) (v uint16, ok bool) {
	if off < 0 || off+2 > len(b) {
		return 0, false
	}
	return uint16(b[off])<<8 | uint16(b[off+1]), true
}

// GetU32 reads a big-endian uint32 from b at off. ok is false if b is
// too short.
func GetU32(b []byte, off int) (v uint32, ok bool) {
	if off < 0 || off+4 > len(b) {
		return 0, false
	}
	return uint32(b[off])<<24 | uint32(b[off+1])<<16 |
		uint32(b[off+2])<<8 | uint32(b[off+3]), true
}

// GetU64 reads a big-endian uint64 from b at off. ok is false if b is
// too short.
func GetU64(b []byte, off int) (v uint64, ok bool) {
	if off < 0 || off+8 > len(b) {
		return 0, false
	}
	return uint64(b[off])<<56 | uint64(b[off+1])<<48 |
		uint64(b[off+2])<<40 | uint64(b[off+3])<<32 |
		uint64(b[off+4])<<24 | uint64(b[off+5])<<16 |
		uint64(b[off+6])<<8 | uint64(b[off+7]), true
}

// --- command / response framing ---

// BuildCommand marshals a TPM 2.0 command buffer:
//
//	[ tag:u16 | commandSize:u32 | commandCode:u32 | params... ]
//
// commandSize is the total byte length of the buffer (HeaderSize plus
// len(params)).
func BuildCommand(tag uint16, cc uint32, params []byte) []byte {
	size := uint32(HeaderSize + len(params))
	buf := make([]byte, 0, size)
	buf = PutU16(buf, tag)
	buf = PutU32(buf, size)
	buf = PutU32(buf, cc)
	buf = append(buf, params...)
	return buf
}

// ParseResponse validates and decomposes a TPM 2.0 response buffer:
//
//	[ tag:u16 | responseSize:u32 | responseCode:u32 | params... ]
//
// It checks len(rsp) >= HeaderSize and that the embedded responseSize
// equals len(rsp), then returns the tag, the responseCode (rc), and the
// parameter bytes that follow the 10-byte header. params aliases rsp; it
// is not copied.
func ParseResponse(rsp []byte) (tag uint16, rc uint32, params []byte, err error) {
	if len(rsp) < HeaderSize {
		return 0, 0, nil, ErrShortBuffer
	}
	tag, _ = GetU16(rsp, 0)
	size, _ := GetU32(rsp, 2)
	rc, _ = GetU32(rsp, 6)
	if int(size) != len(rsp) {
		return 0, 0, nil, ErrSizeMismatch
	}
	return tag, rc, rsp[HeaderSize:], nil
}

// --- TPM2B size-prefixed blob ---

// MarshalTPM2B wraps b in a TPM2B structure:
//
//	[ size:u16 | bytes... ]
//
// TCG "TPM 2.0 Part 2: Structures", clause "TPM2B Types".
func MarshalTPM2B(b []byte) []byte {
	out := make([]byte, 0, 2+len(b))
	out = PutU16(out, uint16(len(b)))
	out = append(out, b...)
	return out
}

// UnmarshalTPM2B decodes a TPM2B from the front of b and returns the
// payload (val), the remaining bytes after it (rest), and an error if b
// is too short to hold the 2-byte size or the declared payload. val and
// rest alias b; they are not copied.
func UnmarshalTPM2B(b []byte) (val, rest []byte, err error) {
	size, ok := GetU16(b, 0)
	if !ok {
		return nil, nil, ErrShortBuffer
	}
	end := 2 + int(size)
	if end > len(b) {
		return nil, nil, ErrShortBuffer
	}
	return b[2:end], b[end:], nil
}
