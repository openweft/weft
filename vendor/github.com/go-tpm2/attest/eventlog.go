// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, the go-tpm2/attest authors. All rights reserved.

package attest

// This file parses and replays a TCG measured-boot event log in the
// crypto-agile format defined by the TCG PC Client Platform Firmware Profile
// (PFP) specification, "Event Logging" — specifically clause 10.2.1
// ("TCG_PCR_EVENT2", the crypto-agile event) and clause 9.4.5.1
// ("TCG_EfiSpecIdEvent", the spec-ID header carried by the first, legacy
// TCG_PCR_EVENT). References below cite that profile.
//
// Layout of a crypto-agile log:
//
//	[0]  TCG_PCR_EVENT (LEGACY, SHA-1-shaped header):
//	       PCRIndex   u32          (0)
//	       EventType  u32          (EV_NO_ACTION = 0x00000003)
//	       Digest     [20]byte     (all-zero for the header event)
//	       EventSize  u32
//	       Event[EventSize]        = TCG_EfiSpecIdEvent:
//	           Signature        [16]byte  ("Spec ID Event03\0")
//	           platformClass    u32
//	           specVersionMinor u8
//	           specVersionMajor u8
//	           specErrata       u8
//	           uintnSize        u8
//	           numberOfAlgs     u32
//	           [numberOfAlgs] { algId u16, digestSize u16 }
//	           vendorInfoSize   u8
//	           vendorInfo[vendorInfoSize]
//
//	[1..] TCG_PCR_EVENT2 (CRYPTO-AGILE):
//	       PCRIndex   u32
//	       EventType  u32
//	       Digests    TPML_DIGEST_VALUES:
//	           count  u32
//	           [count] TPMT_HA { algId u16, digest[digestSize(algId)] }
//	       EventSize  u32
//	       Event[EventSize]
//
// The per-algorithm digest sizes are taken from the spec-ID header (the wire
// does NOT length-prefix each digest), so a parser MUST read the header first.

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"

	"github.com/go-tpm2/common"
)

// Event-log error sentinels (typed and constant for ==).
const (
	// ErrMalformedLog is returned when the event log is truncated or otherwise
	// cannot be parsed (a short buffer, a digest of an unknown algorithm, etc.).
	ErrMalformedLog = common.Error("attest: malformed TCG event log")
	// ErrBadSpecID is returned when the first event is not a well-formed
	// TCG_EfiSpecIdEvent (wrong signature, or no algorithms declared).
	ErrBadSpecID = common.Error("attest: missing or bad Spec ID Event03 header")
	// ErrUnknownDigestAlg is returned when a TCG_PCR_EVENT2 carries a digest
	// whose algorithm was not declared in the spec-ID header.
	ErrUnknownDigestAlg = common.Error("attest: event-log digest of undeclared algorithm")
	// ErrEventLogMismatch is returned by EventLogPolicy when replaying the log
	// does not reproduce the attested PCR values.
	ErrEventLogMismatch = common.Error("attest: event-log replay != attested PCRs")
)

// specIDSignature is the 16-byte signature of a TCG_EfiSpecIdEvent for the
// crypto-agile log: ASCII "Spec ID Event03" plus a NUL terminator. PFP clause
// 9.4.5.1, "Signature". A header whose first 16 bytes are not this is rejected.
var specIDSignature = []byte("Spec ID Event03\x00")

// legacyDigestLen is the fixed SHA-1 digest width of the legacy TCG_PCR_EVENT
// header event. PFP clause 10.2.1, "TCG_PCR_EVENT".
const legacyDigestLen = 20

// Event is one parsed measurement from the log: the PCR it extended, the TCG
// event type, the per-algorithm digests that were folded into that PCR, and the
// opaque event data. Digests is keyed by TPM_ALG_ID (e.g. common.AlgSHA256) so
// a replay can pick the bank it is reconstructing. PFP clause 10.2.1.
type Event struct {
	// PCR is the PCR index this event extended.
	PCR int
	// Type is the TCG event type (EV_*). It is carried verbatim; this package
	// does not interpret it (the allowlist keys on PCR + digest).
	Type uint32
	// Digests maps TPM_ALG_ID -> the digest that was extended for that bank.
	Digests map[uint16][]byte
	// Data is the opaque event payload (EventSize bytes).
	Data []byte
}

// evNoAction is EV_NO_ACTION, the event type of the spec-ID header event (which
// is recorded in the log but never extended into a PCR). PFP clause 10.4.1.
const evNoAction uint32 = 0x00000003

// LogBuilder constructs a crypto-agile TCG event log (a spec-ID header followed
// by TCG_PCR_EVENT2 records) for the SHA-256 bank. A platform's measured-boot
// firmware emits this log as it extends PCRs; this builder lets a Go node (or a
// test) record the same SHA-256 measurements it extends so the verifier can
// replay them. Use NewLogBuilder, Add per measurement, then Bytes.
type LogBuilder struct {
	events []byte
}

// NewLogBuilder starts an empty crypto-agile log builder for the SHA-256 bank.
func NewLogBuilder() *LogBuilder { return &LogBuilder{} }

// Add records one measurement: the SHA-256 digest extended into PCR pcr, with
// TCG event type etype and opaque event data (which may be nil). It mirrors one
// TPM2_PCR_Extend and returns the builder for chaining. PFP clause 10.2.1
// ("TCG_PCR_EVENT2").
func (b *LogBuilder) Add(pcr int, etype uint32, sha256Digest, data []byte) *LogBuilder {
	e := b.events
	e = putLE32(e, uint32(pcr))
	e = putLE32(e, etype)
	e = putLE32(e, 1) // count: SHA-256 only
	e = putLE16(e, uint16(common.AlgSHA256))
	e = append(e, sha256Digest...)
	e = putLE32(e, uint32(len(data)))
	e = append(e, data...)
	b.events = e
	return b
}

// putLE16 / putLE32 append v to dst in LITTLE-endian order. The TCG event log
// is little-endian (TCG PC Client Platform Firmware Profile, "Event Logging"),
// unlike common's big-endian TPM-command-stream codec, so the builder and the
// logReader use these LE helpers rather than common.PutU16/PutU32.
func putLE16(dst []byte, v uint16) []byte {
	return append(dst, byte(v), byte(v>>8))
}

func putLE32(dst []byte, v uint32) []byte {
	return append(dst, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}

// Bytes renders the full log: the legacy TCG_PCR_EVENT spec-ID header (which
// declares the SHA-256 bank and its 32-byte digest width) followed by the
// recorded TCG_PCR_EVENT2 events. PFP clauses 9.4.5.1 and 10.2.1.
func (b *LogBuilder) Bytes() []byte {
	// TCG_EfiSpecIdEvent body.
	var spec []byte
	spec = append(spec, specIDSignature...)
	spec = putLE32(spec, 0)      // platformClass
	spec = common.PutU8(spec, 0) // specVersionMinor
	spec = common.PutU8(spec, 2) // specVersionMajor (TPM 2.0)
	spec = common.PutU8(spec, 0) // specErrata
	spec = common.PutU8(spec, 2) // uintnSize (8-byte UINTN)
	spec = putLE32(spec, 1)      // numberOfAlgorithms
	spec = putLE16(spec, uint16(common.AlgSHA256))
	spec = putLE16(spec, sha256Size) // digestSize
	spec = common.PutU8(spec, 0)     // vendorInfoSize (no vendor info)

	// Legacy TCG_PCR_EVENT wrapper carrying the spec-ID event.
	var out []byte
	out = putLE32(out, 0)                               // PCRIndex
	out = putLE32(out, evNoAction)                      // EventType = EV_NO_ACTION
	out = append(out, make([]byte, legacyDigestLen)...) // SHA-1 digest (zero)
	out = putLE32(out, uint32(len(spec)))
	out = append(out, spec...)
	out = append(out, b.events...)
	return out
}

// ParseEventLog parses a crypto-agile TCG event log into its events. The first
// event MUST be the legacy TCG_PCR_EVENT carrying a TCG_EfiSpecIdEvent header
// (which declares the per-algorithm digest sizes used to parse the rest); every
// subsequent event is a crypto-agile TCG_PCR_EVENT2. The header event itself is
// NOT returned (it is metadata, EV_NO_ACTION, never extended into a PCR). PFP
// clauses 9.4.5.1 and 10.2.1.
func ParseEventLog(log []byte) ([]Event, error) {
	r := &logReader{b: log}

	// First event: legacy TCG_PCR_EVENT { PCRIndex, EventType, SHA1[20],
	// EventSize, Event }. Its Event is the TCG_EfiSpecIdEvent.
	if _, ok := r.u32(); !ok { // PCRIndex (ignored: header)
		return nil, ErrMalformedLog
	}
	if _, ok := r.u32(); !ok { // EventType (EV_NO_ACTION)
		return nil, ErrMalformedLog
	}
	if _, ok := r.skip(legacyDigestLen); !ok { // SHA-1 digest (zero)
		return nil, ErrMalformedLog
	}
	hdrSize, ok := r.u32()
	if !ok {
		return nil, ErrMalformedLog
	}
	hdr, ok := r.bytes(int(hdrSize))
	if !ok {
		return nil, ErrMalformedLog
	}
	algSizes, err := parseSpecID(hdr)
	if err != nil {
		return nil, err
	}

	// Subsequent events: TCG_PCR_EVENT2.
	var events []Event
	for !r.eof() {
		ev, err := parseEvent2(r, algSizes)
		if err != nil {
			return nil, err
		}
		events = append(events, ev)
	}
	return events, nil
}

// parseSpecID parses a TCG_EfiSpecIdEvent and returns the declared algorithm ->
// digest-size map. PFP clause 9.4.5.1, "TCG_EfiSpecIdEvent". A header with a
// wrong signature, a truncated body, or zero algorithms is rejected.
func parseSpecID(b []byte) (map[uint16]int, error) {
	r := &logReader{b: b}
	sig, ok := r.bytes(len(specIDSignature))
	if !ok || !bytes.Equal(sig, specIDSignature) {
		return nil, ErrBadSpecID
	}
	if _, ok := r.u32(); !ok { // platformClass
		return nil, ErrBadSpecID
	}
	if _, ok := r.skip(4); !ok { // specVersionMinor/Major, specErrata, uintnSize
		return nil, ErrBadSpecID
	}
	numAlgs, ok := r.u32()
	if !ok || numAlgs == 0 {
		return nil, ErrBadSpecID
	}
	sizes := make(map[uint16]int, numAlgs)
	for i := uint32(0); i < numAlgs; i++ {
		algID, ok := r.u16()
		if !ok {
			return nil, ErrBadSpecID
		}
		ds, ok := r.u16()
		if !ok {
			return nil, ErrBadSpecID
		}
		sizes[algID] = int(ds)
	}
	// vendorInfoSize (u8) + vendorInfo: present in a well-formed header, but the
	// values are not needed for replay. Their absence means a truncated header.
	viSize, ok := r.u8()
	if !ok {
		return nil, ErrBadSpecID
	}
	if _, ok := r.skip(int(viSize)); !ok {
		return nil, ErrBadSpecID
	}
	return sizes, nil
}

// parseEvent2 parses one TCG_PCR_EVENT2 using the digest sizes from the header.
// PFP clause 10.2.1, "TCG_PCR_EVENT2".
func parseEvent2(r *logReader, algSizes map[uint16]int) (Event, error) {
	pcr, ok := r.u32()
	if !ok {
		return Event{}, ErrMalformedLog
	}
	etype, ok := r.u32()
	if !ok {
		return Event{}, ErrMalformedLog
	}
	count, ok := r.u32()
	if !ok {
		return Event{}, ErrMalformedLog
	}
	digests := make(map[uint16][]byte, count)
	for i := uint32(0); i < count; i++ {
		algID, ok := r.u16()
		if !ok {
			return Event{}, ErrMalformedLog
		}
		size, known := algSizes[algID]
		if !known {
			return Event{}, ErrUnknownDigestAlg
		}
		d, ok := r.bytes(size)
		if !ok {
			return Event{}, ErrMalformedLog
		}
		digests[algID] = d
	}
	dataSize, ok := r.u32()
	if !ok {
		return Event{}, ErrMalformedLog
	}
	data, ok := r.bytes(int(dataSize))
	if !ok {
		return Event{}, ErrMalformedLog
	}
	return Event{PCR: int(pcr), Type: etype, Digests: digests, Data: data}, nil
}

// ReplayPCRs recomputes the PCR bank for algorithm alg by folding each event's
// digest into a virtual PCR exactly as the TPM does: a PCR starts at all-zero
// and each extend sets pcr = H(pcr || eventDigest), where H is the bank's hash.
// For the SHA-256 bank (the only bank this stack replays) H is SHA-256 and the
// PCR width is 32 bytes. An event missing a digest for alg is an inconsistent
// log. PFP clause 7.2 ("PCR Usage") / TCG "Part 1", "PCR Extend". Only
// common.AlgSHA256 is supported as the replay bank.
func ReplayPCRs(events []Event, alg uint16) (map[int][]byte, error) {
	if alg != uint16(common.AlgSHA256) {
		return nil, ErrUnknownDigestAlg
	}
	pcrs := make(map[int][]byte)
	for _, ev := range events {
		d, ok := ev.Digests[alg]
		if !ok {
			return nil, ErrUnknownDigestAlg
		}
		cur, seen := pcrs[ev.PCR]
		if !seen {
			cur = make([]byte, sha256Size)
		}
		next := sha256.Sum256(append(append([]byte(nil), cur...), d...))
		pcrs[ev.PCR] = next[:]
	}
	return pcrs, nil
}

// sha256Size is the SHA-256 digest / PCR width in bytes.
const sha256Size = 32

// logReader is a tiny big-endian, bounds-checked cursor over a log buffer. Each
// reader returns ok=false (never panics) on a short read so every truncation
// branch is an ordinary, testable error path.
type logReader struct {
	b   []byte
	off int
}

func (r *logReader) eof() bool { return r.off >= len(r.b) }

func (r *logReader) u8() (uint8, bool) {
	if r.off+1 > len(r.b) {
		return 0, false
	}
	v := r.b[r.off]
	r.off++
	return v, true
}

func (r *logReader) u16() (uint16, bool) {
	if r.off+2 > len(r.b) {
		return 0, false
	}
	v := binary.LittleEndian.Uint16(r.b[r.off:])
	r.off += 2
	return v, true
}

func (r *logReader) u32() (uint32, bool) {
	if r.off+4 > len(r.b) {
		return 0, false
	}
	v := binary.LittleEndian.Uint32(r.b[r.off:])
	r.off += 4
	return v, true
}

// bytes returns the next n bytes (a fresh copy) or ok=false if fewer remain. A
// negative n (from a bogus u32 size cast) reads as a short buffer.
func (r *logReader) bytes(n int) ([]byte, bool) {
	if n < 0 || r.off+n > len(r.b) {
		return nil, false
	}
	out := make([]byte, n)
	copy(out, r.b[r.off:r.off+n])
	r.off += n
	return out, true
}

// skip advances over n bytes without copying them.
func (r *logReader) skip(n int) (struct{}, bool) {
	if n < 0 || r.off+n > len(r.b) {
		return struct{}{}, false
	}
	r.off += n
	return struct{}{}, true
}
