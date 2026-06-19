# go-tpm2/tpm2

Transport-agnostic, pure-Go **TPM 2.0 command API**. It sits one layer above
[`github.com/go-tpm2/common`](https://github.com/go-tpm2/common): `common` owns
the big-endian wire codec and the `Transport` contract, and this package turns
that codec into typed TPM 2.0 commands.

Pure Go, `CGO_ENABLED=0`, no assembly, BSD-3-Clause, 100% statement coverage,
`GOWORK=off`. Every multi-byte field is big-endian, as the TPM 2.0 wire format
mandates (TCG "TPM 2.0 Part 1: Architecture", *Data Marshaling*).

## Usage

A `TPM` wraps a `common.Transport`:

```go
tpm := tpm2.New(transport) // transport implements common.Transport

if err := tpm.Startup(uint16(common.SUClear)); err != nil { /* ... */ }

rnd, err := tpm.GetRandom(20)

_, digests, err := tpm.PCRRead([]tpm2.PCRSelection{
    {Hash: uint16(common.AlgSHA256), PCRs: []int{0, 7}},
})

err = tpm.PCRExtend(0, uint16(common.AlgSHA256), eventDigest)
```

Each method marshals its parameters with `common.BuildCommand` (the right
`TPM_ST` tag + `TPM_CC`), calls `Transport.Send`, parses with
`common.ParseResponse`, checks the response code, and unmarshals the response
parameters. A non-success response code surfaces as a typed `*TPMError`
carrying both the raw `rc` and the `CC` it came from.

## Commands (starter set — first measured-boot milestone)

Each cites TCG "TPM 2.0 Part 3: Commands" (structure shapes from "Part 2:
Structures").

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
(everything after the one-byte `moreData` flag). Full union decoding of that
structure is a deliberate follow-up; the documented boundary is the complete
`{ capability:u32, data:union }` bytes as the TPM sent them.

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

## License

BSD-3-Clause. See [LICENSE](LICENSE).
