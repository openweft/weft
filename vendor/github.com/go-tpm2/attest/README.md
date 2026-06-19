# go-tpm2/attest

[![CI](https://github.com/go-tpm2/attest/actions/workflows/ci.yml/badge.svg)](https://github.com/go-tpm2/attest/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/go-tpm2/attest.svg)](https://pkg.go.dev/github.com/go-tpm2/attest)
[![Coverage](https://img.shields.io/badge/coverage-100%25-brightgreen)](#conventions)
[![License](https://img.shields.io/badge/license-BSD--3--Clause-blue)](LICENSE)

Pure-Go **TPM 2.0 remote-attestation protocol**: a **Verifier** (control plane)
and a **Node** (agent) that together implement **node-admission-on-Quote**. A
node joins a fleet only if it proves — via a TPM **Quote** over a fresh
verifier nonce, signed by an **EK-bound Attestation Key** — that it booted an
approved stack.

It sits one layer above
[`github.com/go-tpm2/tpm2`](https://github.com/go-tpm2/tpm2): the Verifier is
pure Go and **never touches a TPM** (`MakeCredential` + `VerifyQuote` are
off-TPM crypto); the Node drives a real `*tpm2.TPM`
(`CreateEK`/`CreatePrimary`/`ActivateCredential`/`Quote`).

## Install

```sh
go get github.com/go-tpm2/attest
```

## Protocol

Two phases. **Enrolment** (once per node) binds an AK to a trusted EK;
**Admission** (every join) proves an approved boot.

```
Node                                   Verifier
 ── EnrollRequest{EKPub,EKCert,          ──▶  Trusted(EKPub)? MakeCredential
                  AKPub,AKName}                (off-TPM) → blob+secret
 ◀─ EnrollChallenge{CredentialBlob,Secret} ──
 ActivateCredential(AK,EK,…) → secret
 ── EnrollProof{ActivationSecret}        ──▶  const-time compare → BindAK
 ─────────────────────────────────────────────────────────────────────────
 ── AdmissionRequest{AKName}             ──▶  AK bound? fresh Nonce
 ◀─ AdmissionChallenge{Nonce}            ──
 Quote(AK,pcrSel,Nonce) + PCRRead
 ── AdmissionResponse{Quoted,Signature,  ──▶  VerifyQuote (sig+pcrDigest),
                      PCRs,EventLog}            extraData==Nonce (anti-replay),
                                               Policy.Matches(PCRs) → granted
```

## Usage — the two-phase handshake

```go
// --- Verifier (control plane, pure Go, no TPM). ---
reg := attest.NewMemRegistry()
reg.TrustEK(ekPub)                        // or chain the EK cert to a root
v := attest.NewVerifier(reg, attest.GoldenPolicy{ /* set after first boot */ },
	attest.RandNonce)

// --- Node (agent, on the attesting platform). ---
node, _ := attest.NewNode(tpm2.New(transport),
	[]tpm2.PCRSelection{{Hash: uint16(common.AlgSHA256), PCRs: []int{0, 7}}})

// Enrolment: bind the AK to a trusted EK.
chal, _ := v.Enroll(node.EnrollRequest(ekCert))
proof, _ := node.RespondEnroll(chal)      // TPM2_ActivateCredential
if err := v.CompleteEnroll(node.AKName(), proof); err != nil {
	log.Fatal(err)                        // attest.ErrActivationFailed
}

// Admission: prove an approved boot on each join.
adChal, _ := v.Challenge(node.AdmissionRequest())
resp, _ := node.RespondAdmission(adChal)  // TPM2_Quote + PCR_Read
granted, err := v.Admit(node.AKName(), resp)
// err is one of: ErrUnboundAK, ErrStaleNonce, ErrQuoteSignature,
// ErrPCRDigestMismatch, ErrUntrustedBoot (errors.As → *UntrustedBootError),
// ErrEventLogMismatch, ErrUnapprovedMeasurement (errors.As →
// *UnapprovedMeasurementError) when an EventLogPolicy is installed.
```

### Event-log replay policy (v0.2.0)

```go
// Approve individual measurements instead of a whole-PCR golden digest.
elp := attest.NewEventLogPolicy().
    RestrictPCRs(16).                      // gate only on PCR 16
    AllowMeasurement(16, shimDigest).      // each is one allowlist entry
    AllowMeasurement(16, grubDigest).
    AllowMeasurement(16, kernelDigest)
v.SetPolicy(elp)

// The node attaches its measured-boot log; Admit replays it, requires the
// replay to equal the verified PCRs, and requires every event allowlisted.
resp, _ := node.RespondAdmission(adChal)
resp.EventLog = eventLog                   // crypto-agile TCG log
granted, err := v.Admit(node.AKName(), resp)
```

### Pluggable challenge state for HA (v0.3.0)

The Verifier's single-use challenge state — the enrolment activation secret and
the admission nonce — defaults to an in-memory `MemPendingStore`. For a
multi-replica (HA) control plane, back it with a shared `PendingStore` (e.g.
etcd) so a `Challenge` served by one replica is admissible by another:

```go
type PendingStore interface {
    PutEnroll(akName, secret []byte) error            // stash; overwrite prior
    TakeEnroll(akName []byte) (secret []byte, ok bool) // consume, single-use
    PutNonce(akName []byte, nonce []byte) error
    TakeNonce(akName []byte) (nonce []byte, ok bool)   // consume, single-use
}

// Default (in-memory) — NewVerifier uses this:
v := attest.NewVerifier(reg, policy, attest.RandNonce)
// HA — back the challenge state with a shared store:
v := attest.NewVerifierWithStore(reg, policy, attest.RandNonce, etcdStore)
```

The interface is deliberately **time/TTL-agnostic**: the backing store owns
expiry (e.g. etcd leases), so `attest` imports no clock and the Node side stays
import-clean for `GOOS=tamago`. `Take*` is read-and-remove, so a challenge is
consumed exactly once regardless of which replica serves the follow-up. This is
fully backward-compatible — `NewVerifier(reg, policy, nonce)` is unchanged and
uses the in-memory default.

## Wire format

A small, versioned, big-endian, length-prefixed codec (`codec.go`): every frame
is `magic("ATT2") | version | kind | body`; `[]byte` is `u32 len | bytes`; the
32-byte admission nonce is fixed-width; PCR maps are emitted in ascending key
order so the encoding is byte-stable. Decoders reject foreign magic, wrong
version, wrong message kind, and trailing bytes; every message round-trips.

## Security model

- **EK enrolment / identity.** `MakeCredential` binds the activation secret to
  the AK's *Name* and the EK's public key; only a TPM holding the EK private key
  and an AK with that Name can recover it via `ActivateCredential`. A successful
  `CompleteEnroll` proves the AK and a **trusted** EK live on the same TPM.
- **Nonce anti-replay.** Every admission uses a fresh nonce folded into the
  quote's `extraData`; `Admit` checks it and consumes it, so a captured quote
  cannot be replayed.
- **Golden PCR policy.** `GoldenPolicy` admits only if every required PCR equals
  its golden digest, naming the first mismatch in `*UntrustedBootError`.
- **Event-log replay policy (v0.2.0).** `EventLogPolicy` admits by **replaying**
  the node's TCG measured-boot event log (the crypto-agile `TCG_PCR_EVENT2`
  stream, TCG PC Client Platform Firmware Profile) instead of matching
  whole-PCR golden digests. `Matches` parses the log, folds each event into a
  virtual PCR (`pcr = SHA256(pcr ‖ digest)` from all-zero), requires the
  replayed PCRs to **equal** the attested PCRs (`ErrEventLogMismatch` otherwise —
  this binds the otherwise-untrusted log to the verified quote), and requires
  every event to be on an **allowlist** of approved `(PCR, digest)` measurements
  (`ErrUnapprovedMeasurement`, naming the offending event, otherwise). Rolling
  out a new image is **one** allowlist entry, not a per-platform golden-PCR
  recompute. See `Policy.Matches(pcrs, eventLog)` (the v0.2.0 signature).
- **TOCTOU caveat.** A Quote attests **boot-time** measurements, not runtime
  state; attestation gates admission, it is not a continuous integrity monitor.
  Pair it with short admission leases / re-attestation.

## Conventions

Pure Go, `CGO_ENABLED=0`, no workspace (`GOWORK=off`), BSD-3 (SPDX per file),
**100 % statement coverage** (CI gate). Validated end-to-end against a real
**swtpm** in [`go-tpm2/validate`](https://github.com/go-tpm2/validate): the full
handshake runs in a tamago/amd64 guest against live TPM crypto. `cmd/attestvalidate`
asserts ADMITTED and rejects bad-PCR, stale-nonce, and wrong-AK; `cmd/attesteventlog`
(v0.2.0) does several real `PCR_Extend`s, builds a crypto-agile TCG event log of
them, and asserts the `EventLogPolicy` replay equals the real swtpm PCRs and is
allowlisted (ADMITTED), then rejects an unapproved measurement
(`ErrUnapprovedMeasurement`) and a tampered log (`ErrEventLogMismatch`).

Sibling repos: [`common`](https://github.com/go-tpm2/common),
[`tpm2`](https://github.com/go-tpm2/tpm2),
[`crb`](https://github.com/go-tpm2/crb), [`tis`](https://github.com/go-tpm2/tis),
[`validate`](https://github.com/go-tpm2/validate).
