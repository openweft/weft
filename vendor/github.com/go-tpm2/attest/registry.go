// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, the go-tpm2/attest authors. All rights reserved.

package attest

import "sync"

// EKRegistry is the verifier's trust store for Endorsement Keys and the
// EK->AK bindings established at enrolment. It is the policy boundary for
// "which TPMs may ever join": Trusted decides whether an EK public is
// acceptable (e.g. its certificate chains to a known manufacturer root, or it
// is on an explicit allowlist), and BindAK records that an AK Name has been
// cryptographically tied — via ActivateCredential — to such an EK.
//
// Implementations MUST be safe for concurrent use by multiple goroutines.
type EKRegistry interface {
	// Trusted reports whether the given EK public area is acceptable for
	// enrolment. ekPub is the TPMT_PUBLIC of the EK.
	Trusted(ekPub []byte) bool
	// BindAK records that akName (the AK's Name) is bound to a trusted EK and
	// stores akPub (the AK's TPMT_PUBLIC) for later signature verification. It
	// is called only after a successful credential activation.
	BindAK(akName, akPub []byte) error
	// AKPub returns the stored AK public area for a bound AK Name, and whether
	// the AK is bound at all.
	AKPub(akName []byte) (akPub []byte, ok bool)
}

// MemRegistry is a simple thread-safe in-memory EKRegistry. EKs are trusted by
// an explicit allowlist keyed on the exact EK public bytes; bound AKs are held
// in a map from AK Name to AK public. It is intended for tests, single-process
// control planes, and as a reference implementation.
type MemRegistry struct {
	mu        sync.RWMutex
	trustedEK map[string]struct{}
	akPub     map[string][]byte
}

// NewMemRegistry returns an empty MemRegistry. Use TrustEK to allow EKs before
// enrolling nodes.
func NewMemRegistry() *MemRegistry {
	return &MemRegistry{
		trustedEK: make(map[string]struct{}),
		akPub:     make(map[string][]byte),
	}
}

// TrustEK adds an EK public area to the allowlist, so a node presenting this
// exact EK may enrol. A production registry would instead validate the EK
// certificate against a manufacturer root; this explicit allowlist keeps the
// reference impl dependency-free.
func (r *MemRegistry) TrustEK(ekPub []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.trustedEK[string(ekPub)] = struct{}{}
}

// Trusted reports whether ekPub is on the allowlist.
func (r *MemRegistry) Trusted(ekPub []byte) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.trustedEK[string(ekPub)]
	return ok
}

// BindAK stores akPub under akName. It never fails for MemRegistry (the error
// is part of the interface so a backing store can report a write failure).
func (r *MemRegistry) BindAK(akName, akPub []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]byte, len(akPub))
	copy(cp, akPub)
	r.akPub[string(akName)] = cp
	return nil
}

// AKPub returns the bound AK public for akName, if any.
func (r *MemRegistry) AKPub(akName []byte) ([]byte, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.akPub[string(akName)]
	if !ok {
		return nil, false
	}
	cp := make([]byte, len(p))
	copy(cp, p)
	return cp, true
}
