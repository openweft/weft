<p align="center"><img src="https://raw.githubusercontent.com/go-tpm2/brand/main/social/go-tpm2.png" alt="go-tpm2/tpm2" width="720"></p>

# go-tpm2/tpm2

[![CI](https://github.com/go-tpm2/tpm2/actions/workflows/ci.yml/badge.svg)](https://github.com/go-tpm2/tpm2/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/go-tpm2/tpm2.svg)](https://pkg.go.dev/github.com/go-tpm2/tpm2)
[![Coverage](https://img.shields.io/badge/coverage-100%25-brightgreen)](#conventions)
[![License](https://img.shields.io/badge/license-BSD--3--Clause-blue)](LICENSE)

Transport-agnostic, pure-Go **TPM 2.0 command API**. **v0.6.0.**

It sits one layer above
[`github.com/go-tpm2/common`](https://github.com/go-tpm2/common): `common` owns
the big-endian wire codec and the `Transport` contract, and this package turns
that codec into typed TPM 2.0 commands — from `Startup`/`GetRandom`/PCR ops up
through full measured-boot attestation: **Quote**/**VerifyQuote**,
**GetCapability** decoders, **NV** storage, **EK** (Endorsement Key) creation,
**PolicyPCR seal/unseal**, and **MakeCredential**/**ActivateCredential**.

Sibling repos: [`common`](https://github.com/go-tpm2/common) (interfaces +
codec), and the transports it runs over —
[`crb`](https://github.com/go-tpm2/crb) (CRB MMIO) and
[`tis`](https://github.com/go-tpm2/tis) (TIS/FIFO MMIO) — plus
[`validate`](https://github.com/go-tpm2/validate), which proves every flow
below against a real swtpm.

## Install

```sh
go get github.com/go-tpm2/tpm2
```

## Usage

A `TPM` wraps any `common.Transport` (e.g. a `*crb.CRB` or `*tis.TIS`):

```go
tpm := tpm2.New(transport) // transport implements common.Transport

if err := tpm.Startup(uint16(common.SUClear)); err != nil { /* ... */ }

rnd, err := tpm.GetRandom(20)

sel := []tpm2.PCRSelection{{Hash: uint16(common.AlgSHA256), PCRs: []int{0, 7}}}
_, digests, err := tpm.PCRRead(sel)
err = tpm.PCRExtend(0, uint16(common.AlgSHA256), eventDigest)
```

### Attestation (Quote + off-TPM verify)

```go
ak, akPub, _ := tpm.CreatePrimary()                  // ECDSA-P256 AK
quoted, sig, _ := tpm.Quote(ak, nonce, sel)          // TPM2_Quote
info, err := tpm2.VerifyQuote(akPub, quoted, sig, expectedPCRs) // off-TPM
```

### Seal a secret to PCR state (measured-boot payoff)

```go
parent, _ := tpm.CreateStoragePrimary()
priv, pub, _, _ := tpm.SealToPCR(parent, secret, sel, pcrValues)
// …later, unseals ONLY if the PCRs still hold pcrValues:
got, err := tpm.UnsealWithPCR(parent, priv, pub, sel, nonceCaller)
```

### Credential activation (attestation identity)

```go
ek, ekPub, _ := tpm.CreateEK()                        // EK Credential Profile
res, _ := tpm2.MakeCredential(ekPub, akName, secret, rand.Reader) // off-TPM
recovered, err := tpm.ActivateCredential(ak, ek, session,
    res.CredentialBlob, res.Secret)                   // TPM2_ActivateCredential
```

### Object duplication — control-plane seal (v0.6.0)

```go
_, srkPub, _ := tpm.CreateStoragePrimaryPub()          // node's ECC-P256 storage key

// --- control plane: no TPM at all ---
wrapped, _ := tpm2.WrapToPCR(srkPub, secret, sel, pcrValues, nil) // OFFLINE wrap
// … ship wrapped.ObjectPublic / .Duplicate / .InSymSeed to the node …

// --- node: TPM2_Import -> Load -> PolicyPCR -> Unseal ---
recovered, err := tpm.ImportAndUnseal(parent, wrapped.ObjectPublic,
    wrapped.Duplicate, wrapped.InSymSeed, sel, nonceCaller)
```

A control plane with no TPM of its own wraps a secret to a **remote** node's
storage-key public plus a `PolicyPCR` over that node's boot PCRs (`WrapToPCR`,
pure Go, `TPM_CC_Import` duplication-wrap shape); the node recovers it with
`Import`/`ImportAndUnseal` — `TPM2_Import` followed by the same PCR-gated
`Unseal` as `SealToPCR` — but only while its live PCRs still match.

Other groups: **NV** (`NVDefineSpace`/`NVWrite`/`NVRead`/`NVReadPublic`/
`NVUndefineSpace`) and typed **GetCapability** decoders (`GetPCRBanks`,
`GetTPMProperties`, `GetManufacturer`, `GetAlgorithms`, `GetHandles`).

Each method marshals its parameters with `common.BuildCommand` (the right
`TPM_ST` tag + `TPM_CC`), calls `Transport.Send`, parses with
`common.ParseResponse`, checks the response code, and unmarshals the response
parameters. A non-success response code surfaces as a typed `*TPMError`
carrying both the raw `rc` and the `CC` it came from.

## Commands — core set

The table below is the foundation layer; the attestation/seal/NV/EK/credential
commands listed under [Usage](#usage) build on it. Each cites TCG "TPM 2.0
Part 3: Commands" (structure shapes from "Part 2: Structures").

| Method | TPM2_… | CC | Tag |
|---|---|---|---|
| `Startup(su uint16) error` | Startup | `0x144` | NoSessions |
| `Shutdown(su uint16) error` | Shutdown | `0x145` | NoSessions |
| `SelfTest(full bool) error` | SelfTest | `0x143` | NoSessions |
| `GetRandom(n uint16) ([]byte, error)` | GetRandom | `0x17B` | NoSessions |
| `GetCapability(cap, prop, count uint32) (more bool, data []byte, err error)` | GetCapability | `0x17A` | NoSessions |
| `PCRRead(sel []PCRSelection) (updateCounter uint32, digests [][]byte, err error)` | PCR_Read | `0x17E` | NoSessions |
| `PCRExtend(pcr int, hash uint16, digest []byte) error` | PCR_Extend | `0x182` | **Sessions** |

`GetCapability` returns `moreData` plus the **raw** `TPMS_CAPABILITY_DATA` blob
(everything after the one-byte `moreData` flag). The typed decoders
(`GetPCRBanks`, `GetTPMProperties`, `GetManufacturer`, `GetAlgorithms`,
`GetHandles`) parse that union for the common capability classes.

`PCRSelection` is `{ Hash uint16; PCRs []int }`; helpers convert PCR indices to
and from the octet-indexed `pcrSelect` bitmap (PCR *n* = bit *n* mod 8 of octet
*n* div 8, least-significant bit first). PCR[0..23] fit in the three-octet
selection this stack emits.

## Authorization area — `PCRExtend`

`PCR_Extend` is the one command in this set that carries an **authorization
area**, so its command body has three regions (TCG "TPM 2.0 Part 1:
Architecture", *Command Authorization Area Structure*):

```
handle area : pcrHandle (u32)                       — the PCR being extended
auth   area : authorizationSize (u32) || TPMS_AUTH_COMMAND
param  area : TPML_DIGEST_VALUES { count, {hashAlg, digest} }
```

It authorizes with a **cleartext password session over the empty authValue** —
the standard way to extend an unowned platform PCR. The `TPMS_AUTH_COMMAND` is:

```
sessionHandle     = TPM_RS_PW (0x40000009)
nonce             = empty TPM2B          -> 0x0000
sessionAttributes = continueSession      -> 0x01
hmac              = empty TPM2B          -> 0x0000
```

which is **9 bytes** on the wire (4 + 2 + 1 + 2), so `authorizationSize` is
`0x00000009`.

**INFERRED field:** the PCR handle. PCR[*n*] is encoded as the bare index *n*
because `TPM_HT_PCR` (the PCR handle type) is `0x00`, so PCR handles occupy
`0x00000000..0x0000001F` (TCG "TPM 2.0 Part 2: Structures", *TPM_HT (Handle
Types)*). This is the one offset transcribed from the handle-type table rather
than spelled out verbatim in the PCR_Extend command clause.

`TPM_RS_PW` (`0x40000009`) is defined in this package (it is not in `common`).

## Conventions

- Pure Go, `CGO_ENABLED=0`, no assembly.
- BSD-3-Clause on every file.
- 100% statement coverage (`GOWORK=off go test -cover ./...`).
- `GOWORK=off` — this module is not part of any workspace.
- Big-endian: every multi-byte field is MSB-first (TPM 2.0 wire format).
- Every command/flow validated against real swtpm 0.10.1 by
  [`validate`](https://github.com/go-tpm2/validate).

## Specifications

- TCG TPM 2.0 Library, **Parts 1–4** (Architecture, Structures, Commands, Support Routines).
- TCG PC Client Platform TPM Profile (**PTP**).
- TCG **EK Credential Profile** (the EK ECC-P256 L-2 template).

## License

BSD-3-Clause. See [LICENSE](LICENSE).
