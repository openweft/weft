// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, the go-tpm2/attest authors. All rights reserved.

package attest

import "sync"

// PendingStore persists the Verifier's single-use challenge state across
// replicas. Default is in-memory; back it with a shared store (e.g. etcd) for
// an HA control plane.
//
// The interface is deliberately time/TTL-agnostic: the backing store owns
// expiry (e.g. via etcd leases). This keeps attest free of any clock import,
// so the Node side stays import-clean (it builds for GOOS=tamago, which has no
// host clock). The in-memory default is plain maps with no expiry.
//
// All four methods are single-purpose:
//   - PutEnroll / PutNonce stash a value for an AK Name, overwriting any prior
//     value for that AK.
//   - TakeEnroll / TakeNonce consume (read-and-remove) a value: a value may be
//     taken at most once. A missing or already-taken entry returns ok=false.
//
// Implementations MUST be safe for concurrent use by multiple goroutines.
type PendingStore interface {
	// PutEnroll stashes secret for akName, overwriting any prior pending
	// enrolment for akName.
	PutEnroll(akName, secret []byte) error
	// TakeEnroll consumes the pending activation secret for akName. It returns
	// ok=false if none is pending (or it was already taken).
	TakeEnroll(akName []byte) (secret []byte, ok bool)
	// PutNonce stashes nonce for akName, overwriting any prior pending nonce.
	PutNonce(akName []byte, nonce []byte) error
	// TakeNonce consumes the pending admission nonce for akName. It returns
	// ok=false if none is pending (or it was already taken).
	TakeNonce(akName []byte) (nonce []byte, ok bool)
}

// MemPendingStore is the default in-memory PendingStore: plain maps guarded by
// a mutex, no expiry. It is the behavior the Verifier had before PendingStore
// was introduced, and is used by NewVerifier. It is safe for concurrent use.
//
// Note: MemPendingStore keeps only the activation secret and the nonce — the
// pending AK public area (needed to BindAK on a successful CompleteEnroll) is
// retained by the Verifier itself, since it is not single-use challenge state
// and an HA store need not carry it (a node that fails over re-enrolls).
type MemPendingStore struct {
	mu     sync.Mutex
	enroll map[string][]byte
	nonce  map[string][]byte
}

// NewMemPendingStore returns an empty in-memory PendingStore.
func NewMemPendingStore() *MemPendingStore {
	return &MemPendingStore{
		enroll: make(map[string][]byte),
		nonce:  make(map[string][]byte),
	}
}

// PutEnroll stashes a copy of secret under akName.
func (m *MemPendingStore) PutEnroll(akName, secret []byte) error {
	cp := make([]byte, len(secret))
	copy(cp, secret)
	m.mu.Lock()
	m.enroll[string(akName)] = cp
	m.mu.Unlock()
	return nil
}

// TakeEnroll consumes the pending activation secret for akName.
func (m *MemPendingStore) TakeEnroll(akName []byte) ([]byte, bool) {
	key := string(akName)
	m.mu.Lock()
	v, ok := m.enroll[key]
	delete(m.enroll, key)
	m.mu.Unlock()
	return v, ok
}

// PutNonce stashes a copy of nonce under akName.
func (m *MemPendingStore) PutNonce(akName []byte, nonce []byte) error {
	cp := make([]byte, len(nonce))
	copy(cp, nonce)
	m.mu.Lock()
	m.nonce[string(akName)] = cp
	m.mu.Unlock()
	return nil
}

// TakeNonce consumes the pending admission nonce for akName.
func (m *MemPendingStore) TakeNonce(akName []byte) ([]byte, bool) {
	key := string(akName)
	m.mu.Lock()
	v, ok := m.nonce[key]
	delete(m.nonce, key)
	m.mu.Unlock()
	return v, ok
}
