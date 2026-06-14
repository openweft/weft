// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, the go-tpm2/attest authors. All rights reserved.

package attest

import (
	"crypto/subtle"
	"fmt"
	"sort"

	"github.com/go-tpm2/common"
)

// Policy decides whether a set of attested PCR values represents an approved
// boot. The verifier calls Matches AFTER it has cryptographically established
// that the PCR values are genuine (signature + nonce + pcrDigest all checked),
// so a Policy only has to compare values, never crypto.
//
// Matches also receives the attested measured-boot event log (resp.EventLog,
// possibly nil) so a Policy that replays the log — EventLogPolicy — can verify
// it against the genuine PCRs. Value-only policies (GoldenPolicy) ignore it. The
// log is NOT independently trusted: a Policy that uses it MUST prove it is
// consistent with the cryptographically-verified PCRs (replay == PCRs) before
// drawing any conclusion from its contents. The v0.1.0 signature
// Matches(pcrs) gained the eventLog parameter in v0.2.0 (a clean minor change:
// attest has no external API consumers — the only caller is the in-package
// Verifier, and the validate harness is updated in lockstep).
type Policy interface {
	// Matches reports nil if the PCRs represent an approved stack, or a typed
	// error (e.g. *UntrustedBootError) naming the first failing PCR otherwise.
	// eventLog is the attested TCG event log (may be nil); value-only policies
	// ignore it.
	Matches(pcrs map[int][]byte, eventLog []byte) error
}

// GoldenPolicy is the v0.1.0 Policy: a map of required PCR index -> expected
// digest. Matches requires every golden PCR to be present in the attested set
// AND to equal its golden value exactly; the first PCR that is missing or
// differs is named in the returned *UntrustedBootError. Attested PCRs not named
// in the golden map are ignored (the policy constrains only the PCRs it lists).
type GoldenPolicy map[int][]byte

// Matches checks every golden PCR against the attested values in ascending
// index order (so the "first mismatch" reported is deterministic).
func (g GoldenPolicy) Matches(pcrs map[int][]byte, _ []byte) error {
	idxs := make([]int, 0, len(g))
	for i := range g {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	for _, i := range idxs {
		want := g[i]
		got, ok := pcrs[i]
		if !ok {
			return &UntrustedBootError{PCR: i, Reason: "PCR not present in quote"}
		}
		if subtle.ConstantTimeCompare(want, got) != 1 {
			return &UntrustedBootError{PCR: i, Reason: "PCR value != golden"}
		}
	}
	return nil
}

// UntrustedBootError is the typed Policy failure: the index of the first PCR
// that did not match the policy, and a short reason. errors.As lets callers
// recover the PCR index for diagnostics. It satisfies ErrUntrustedBoot under
// errors.Is so callers can branch on the sentinel without unwrapping.
type UntrustedBootError struct {
	// PCR is the index of the first failing PCR.
	PCR int
	// Reason describes the failure (missing vs. value mismatch).
	Reason string
}

// Error implements the error interface.
func (e *UntrustedBootError) Error() string {
	return fmt.Sprintf("attest: untrusted boot: PCR %d: %s", e.PCR, e.Reason)
}

// Is reports that an *UntrustedBootError matches the ErrUntrustedBoot sentinel,
// so callers can write errors.Is(err, ErrUntrustedBoot).
func (e *UntrustedBootError) Is(target error) bool {
	return target == ErrUntrustedBoot
}

// ErrUntrustedBoot is the sentinel every Policy mismatch satisfies under
// errors.Is. A concrete *UntrustedBootError additionally names the PCR.
const ErrUntrustedBoot = common.Error("attest: untrusted boot (PCR policy mismatch)")

// EventLogPolicy is the v0.2.0 Policy that REPLAYS a TCG measured-boot event log
// against the attested PCRs and checks every measured event against an
// allowlist of approved individual measurements — closing the gap that
// GoldenPolicy needs a precomputed golden digest PER platform. Its Matches:
//
//  1. parses the crypto-agile log (ParseEventLog) and replays it into virtual
//     PCRs (ReplayPCRs over the SHA-256 bank);
//  2. confirms every replayed PCR equals the attested value for that index
//     (ErrEventLogMismatch otherwise) — this is what binds the log to the
//     cryptographically-verified quote, so a tampered log cannot be trusted;
//  3. confirms every event's SHA-256 digest is on the allowlist
//     (ErrUnapprovedMeasurement, naming the offending event, otherwise).
//
// The allowlist is a set of approved (PCRindex, SHA-256 digest) measurements,
// so a single image/component update is ONE new allowlist entry rather than a
// full per-platform PCR-digest recompute (see doc.go for the tradeoff).
type EventLogPolicy struct {
	// alg is the digest bank replayed and allowlisted (SHA-256).
	allow map[string]struct{}
	// pcrs, if non-nil, restricts which PCR indices must be reproduced exactly
	// by the replay. When nil, every PCR the log touches must match its
	// attested value.
	restrict map[int]bool
}

// NewEventLogPolicy builds an empty EventLogPolicy whose allowlist is then
// populated with AllowMeasurement. With an empty allowlist, any non-empty log
// is rejected at the first event (so a policy must be explicitly provisioned).
func NewEventLogPolicy() *EventLogPolicy {
	return &EventLogPolicy{allow: make(map[string]struct{})}
}

// AllowMeasurement adds one approved measurement — the SHA-256 digest extended
// into PCR pcr — to the allowlist. It returns the policy for chaining. The
// digest is copied; the key combines the PCR index and the digest so the same
// digest in a different PCR is a distinct (and separately-approved) measurement.
func (p *EventLogPolicy) AllowMeasurement(pcr int, digest []byte) *EventLogPolicy {
	p.allow[allowKey(pcr, digest)] = struct{}{}
	return p
}

// RestrictPCRs limits the replay-equality check to the given PCR indices (the
// log may legitimately touch PCRs the verifier does not gate on). With no
// restriction set, every PCR the log extends must reproduce its attested value.
func (p *EventLogPolicy) RestrictPCRs(pcrs ...int) *EventLogPolicy {
	p.restrict = make(map[int]bool, len(pcrs))
	for _, i := range pcrs {
		p.restrict[i] = true
	}
	return p
}

// Matches replays eventLog into virtual PCRs, requires them to equal the
// attested PCRs (proving the log is consistent with the verified quote), and
// requires every event to be on the allowlist. It satisfies Policy.
func (p *EventLogPolicy) Matches(pcrs map[int][]byte, eventLog []byte) error {
	events, err := ParseEventLog(eventLog)
	if err != nil {
		return err
	}
	replayed, err := ReplayPCRs(events, uint16(common.AlgSHA256))
	if err != nil {
		return err
	}

	// Replay-equality: each replayed PCR must equal the attested value. The log
	// is otherwise untrusted, so this is what binds it to the genuine quote.
	idxs := make([]int, 0, len(replayed))
	for i := range replayed {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	for _, i := range idxs {
		if p.restrict != nil && !p.restrict[i] {
			continue
		}
		got, ok := pcrs[i]
		if !ok {
			return &UntrustedBootError{PCR: i, Reason: "event log extends a PCR absent from the quote"}
		}
		if subtle.ConstantTimeCompare(replayed[i], got) != 1 {
			return ErrEventLogMismatch
		}
	}

	// Allowlist: every measured event's SHA-256 digest must be approved. A
	// successful SHA-256 replay above already guarantees each event carries a
	// SHA-256 digest, so the lookup here is unconditional.
	for _, ev := range events {
		d := ev.Digests[uint16(common.AlgSHA256)]
		if _, ok := p.allow[allowKey(ev.PCR, d)]; !ok {
			return &UnapprovedMeasurementError{PCR: ev.PCR, EventType: ev.Type, Digest: append([]byte(nil), d...)}
		}
	}
	return nil
}

// allowKey is the allowlist map key for a (PCR, digest) measurement: the PCR
// index in four big-endian bytes prepended to the raw digest, so distinct PCRs
// never collide and the key is exact (no hex/string ambiguity).
func allowKey(pcr int, digest []byte) string {
	var k [4]byte
	k[0] = byte(uint32(pcr) >> 24)
	k[1] = byte(uint32(pcr) >> 16)
	k[2] = byte(uint32(pcr) >> 8)
	k[3] = byte(uint32(pcr))
	return string(k[:]) + string(digest)
}

// UnapprovedMeasurementError is the typed EventLogPolicy failure: an event in
// the (replay-verified) log whose digest is not on the allowlist. It names the
// PCR, the TCG event type, and the offending digest. It satisfies the
// ErrUnapprovedMeasurement sentinel under errors.Is.
type UnapprovedMeasurementError struct {
	// PCR is the PCR index the unapproved event extended.
	PCR int
	// EventType is the TCG event type (EV_*) of the unapproved event.
	EventType uint32
	// Digest is the unapproved SHA-256 measurement.
	Digest []byte
}

// Error implements the error interface.
func (e *UnapprovedMeasurementError) Error() string {
	return fmt.Sprintf("attest: unapproved measurement: PCR %d, eventType 0x%08x, digest %x",
		e.PCR, e.EventType, e.Digest)
}

// Is reports that an *UnapprovedMeasurementError matches the
// ErrUnapprovedMeasurement sentinel, so callers can write
// errors.Is(err, ErrUnapprovedMeasurement).
func (e *UnapprovedMeasurementError) Is(target error) bool {
	return target == ErrUnapprovedMeasurement
}

// ErrUnapprovedMeasurement is the sentinel every allowlist rejection satisfies
// under errors.Is. A concrete *UnapprovedMeasurementError additionally names
// the offending event.
const ErrUnapprovedMeasurement = common.Error("attest: event log contains an unapproved measurement")
