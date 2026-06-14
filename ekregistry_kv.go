package weft

// ekregistry_kv.go is the etcd-backed implementation of the
// go-tpm2/attest EKRegistry — the verifier's trust store for TPM
// Endorsement Keys (EKs) and the EK→AK bindings established during
// remote-attestation enrolment.
//
// It mirrors the hosts_kv.go KVStorage pattern : one etcd key per
// record, Put/Delete a single record, surgical in-memory index. Two
// record kinds share one KVStorage prefix, distinguished by a key
// namespace segment :
//
//   <prefix>ek/<hex sha256(ekPub)>  → trustedEKRecord  (operator-trusted EK)
//   <prefix>ak/<hex sha256(akName)> → boundAKRecord    (AK bound to an EK)
//
// Why hash the keys : an EK public area / AK Name is an opaque binary
// blob (a TPMT_PUBLIC / alg||digest) — not a safe etcd key. A SHA-256
// of the blob is a fixed-width, hex-safe key ; the full blob lives in
// the record value so AKPub can return the exact stored bytes.
//
// The control plane never touches a TPM : Trusted / BindAK / AKPub are
// pure-Go map + KV operations. The interface satisfied is exactly
// attest.EKRegistry { Trusted, BindAK, AKPub } ; TrustEK is the extra
// operator-facing provision-time call (mirrors attest.MemRegistry.TrustEK).
//
// HA note : like the other *_kv registries this keeps an in-memory index
// for reads and persists each mutation as one Put. A multi-replica
// control plane should run WatchKeys → applyKVEvent (see hosts_kv.go's
// applyKVEvent) so a BindAK on dc2 lands in dc1's index ; that wiring is
// a follow-up (the verifier's pending enrolment/admission state is also
// in-memory today — see attestation.go).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
)

// trustedEKRecord is the stored form of an operator-trusted EK. The
// full EK public area is retained so future audit / re-trust flows can
// inspect it ; the record key is hex(sha256(EKPub)).
type trustedEKRecord struct {
	EKPub []byte `json:"ek_pub"`
}

// boundAKRecord is the stored form of an AK that completed enrolment :
// its Name (the verifier's lookup key) and its public area (for quote
// signature verification at admission time).
type boundAKRecord struct {
	AKName []byte `json:"ak_name"`
	AKPub  []byte `json:"ak_pub"`
}

// EKRegistry is a weft-side, etcd-backed attest.EKRegistry. It satisfies
// the go-tpm2/attest EKRegistry interface (Trusted / BindAK / AKPub) and
// adds TrustEK for provision-time EK enrolment. Safe for concurrent use.
type EKRegistry struct {
	mu sync.RWMutex
	kv KVStorage // per-record etcd backend ; nil in pure-in-memory tests is NOT supported — use a memKV

	// trustedEK is the in-memory mirror of the trusted-EK set, keyed on
	// hex(sha256(EKPub)) → the raw EKPub.
	trustedEK map[string][]byte
	// akPub maps the AK Name (as a string key) → the bound AK public.
	akPub map[string][]byte
}

const (
	// ekKeyNS / akKeyNS are the per-record key namespaces under the
	// registry's shared KVStorage prefix.
	ekKeyNS = "ek/"
	akKeyNS = "ak/"
)

// ekRecordKey returns the per-record KV key for a trusted EK.
func ekRecordKey(ekPub []byte) string {
	sum := sha256.Sum256(ekPub)
	return ekKeyNS + hex.EncodeToString(sum[:])
}

// akRecordKey returns the per-record KV key for a bound AK, keyed on the
// AK Name (so AKPub's lookup matches BindAK's write).
func akRecordKey(akName []byte) string {
	sum := sha256.Sum256(akName)
	return akKeyNS + hex.EncodeToString(sum[:])
}

// LoadEKRegistry builds an EKRegistry over a KVStorage backend, loading
// every existing record into the in-memory index. The KVStorage prefix
// is conventionally "/weft/attest/" ; this registry namespaces ek/ and
// ak/ records under it.
func LoadEKRegistry(ctx context.Context, kv KVStorage) (*EKRegistry, error) {
	if kv == nil {
		return nil, fmt.Errorf("ek registry: nil KVStorage")
	}
	reg := &EKRegistry{
		kv:        kv,
		trustedEK: make(map[string][]byte),
		akPub:     make(map[string][]byte),
	}
	records, err := kv.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("ek registry: list: %w", err)
	}
	for key, blob := range records {
		switch {
		case hasPrefix(key, ekKeyNS):
			var rec trustedEKRecord
			if err := json.Unmarshal(blob, &rec); err != nil {
				continue // skip a corrupt record rather than fail the whole load
			}
			reg.trustedEK[ekRecordKey(rec.EKPub)] = append([]byte(nil), rec.EKPub...)
		case hasPrefix(key, akKeyNS):
			var rec boundAKRecord
			if err := json.Unmarshal(blob, &rec); err != nil {
				continue
			}
			reg.akPub[string(rec.AKName)] = append([]byte(nil), rec.AKPub...)
		}
	}
	return reg, nil
}

// hasPrefix is a tiny dependency-free strings.HasPrefix (the file's only
// string-prefix need ; avoids importing strings for one call).
func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// TrustEK adds an EK public area to the trust allowlist (provision-time
// enrolment). Idempotent : trusting an already-trusted EK rewrites the
// same record. After this, a node presenting this exact EK may enrol.
func (r *EKRegistry) TrustEK(ekPub []byte) error {
	key := ekRecordKey(ekPub)
	cp := append([]byte(nil), ekPub...)
	blob, err := json.Marshal(trustedEKRecord{EKPub: cp})
	if err != nil {
		return fmt.Errorf("ek registry: marshal trusted ek: %w", err)
	}
	r.mu.Lock()
	r.trustedEK[key] = cp
	r.mu.Unlock()
	if err := r.kv.PutOne(context.Background(), key, blob); err != nil {
		// Roll back the in-memory mirror so it never claims trust the
		// store didn't accept.
		r.mu.Lock()
		delete(r.trustedEK, key)
		r.mu.Unlock()
		return fmt.Errorf("ek registry: persist trusted ek: %w", err)
	}
	return nil
}

// Trusted reports whether ekPub is on the trust allowlist. Implements
// attest.EKRegistry.
func (r *EKRegistry) Trusted(ekPub []byte) bool {
	key := ekRecordKey(ekPub)
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.trustedEK[key]
	return ok
}

// BindAK records that akName is bound to a trusted EK and stores akPub
// for later quote-signature verification. Called by the verifier only
// after a successful credential activation. Implements attest.EKRegistry.
func (r *EKRegistry) BindAK(akName, akPub []byte) error {
	cpName := append([]byte(nil), akName...)
	cpPub := append([]byte(nil), akPub...)
	blob, err := json.Marshal(boundAKRecord{AKName: cpName, AKPub: cpPub})
	if err != nil {
		return fmt.Errorf("ek registry: marshal bound ak: %w", err)
	}
	key := akRecordKey(akName)
	r.mu.Lock()
	r.akPub[string(cpName)] = cpPub
	r.mu.Unlock()
	if err := r.kv.PutOne(context.Background(), key, blob); err != nil {
		r.mu.Lock()
		delete(r.akPub, string(cpName))
		r.mu.Unlock()
		return fmt.Errorf("ek registry: persist bound ak: %w", err)
	}
	return nil
}

// AKPub returns the stored AK public area for a bound AK Name and whether
// the AK is bound. Implements attest.EKRegistry. The returned slice is a
// copy ; callers may not mutate the registry through it.
func (r *EKRegistry) AKPub(akName []byte) ([]byte, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.akPub[string(akName)]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), p...), true
}

// applyEKKVEvent applies a remote per-record KV event to the in-memory
// index (the multi-replica catch-up hook ; mirrors hostRegistry.
// applyKVEvent). Wire it from a WatchKeys loop in an HA deployment.
func (r *EKRegistry) applyEKKVEvent(ev KVEvent) {
	switch ev.Kind {
	case KVPut:
		key := stripKVPrefix(ev.Key, r.kv.Prefix())
		switch {
		case hasPrefix(key, ekKeyNS):
			var rec trustedEKRecord
			if err := json.Unmarshal(ev.Value, &rec); err != nil {
				return
			}
			r.mu.Lock()
			r.trustedEK[ekRecordKey(rec.EKPub)] = append([]byte(nil), rec.EKPub...)
			r.mu.Unlock()
		case hasPrefix(key, akKeyNS):
			var rec boundAKRecord
			if err := json.Unmarshal(ev.Value, &rec); err != nil {
				return
			}
			r.mu.Lock()
			r.akPub[string(rec.AKName)] = append([]byte(nil), rec.AKPub...)
			r.mu.Unlock()
		}
	case KVDelete:
		// Deletes carry the prefixed key ; recover the record namespace +
		// hash and drop from whichever index matches. AK deletes need the
		// PrevValue to recover the AK Name (the hash isn't reversible).
		key := stripKVPrefix(ev.Key, r.kv.Prefix())
		switch {
		case hasPrefix(key, ekKeyNS):
			r.mu.Lock()
			delete(r.trustedEK, key)
			r.mu.Unlock()
		case hasPrefix(key, akKeyNS):
			if ev.PrevValue == nil {
				return
			}
			var rec boundAKRecord
			if err := json.Unmarshal(ev.PrevValue, &rec); err != nil {
				return
			}
			r.mu.Lock()
			delete(r.akPub, string(rec.AKName))
			r.mu.Unlock()
		}
	}
}

// stripKVPrefix removes the KVStorage prefix from a full etcd key,
// recovering the record-relative key (e.g. "ek/<hash>"). Mirrors
// hosts_kv.go's hostKeyToUUID prefix-strip.
func stripKVPrefix(key, prefix string) string {
	if len(key) > len(prefix) && hasPrefix(key, prefix) {
		return key[len(prefix):]
	}
	return key
}
