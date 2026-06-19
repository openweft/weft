# go-tpm2/common

The transport-agnostic, platform-agnostic foundation shared by every
layer of the pure-Go TPM 2.0 stack in the **go-tpm2** family. It mirrors
the role of [`go-virtio/common`](https://github.com/go-virtio/common):
it owns the small plug-in interfaces, the big-endian TPM 2.0 wire codec,
and the spec-derived constants. The higher layer (the `tpm2` command
layer) and the lower layers (the `crb`/`tis`/`passthrough`/`socket`
transports) sit on either side and import this package.

## Interfaces

Two interfaces stitch the stack together.

### `Transport` (transport.go)

The contract a concrete transport implements and the command layer
consumes. Deliberately minimal:

```go
type Transport interface {
        Send(cmd []byte) (rsp []byte, err error)
}
```

The command layer marshals a complete command buffer (header +
parameters, via `BuildCommand`), hands it to `Send`, and gets the
complete response buffer back (ready for `ParseResponse`). Framing,
MMIO handshakes, locality, and retry stay inside the transport.

### `Regs` (regs.go)

The platform-provided MMIO accessor that the register-level drivers
(CRB, TIS) use to touch the TPM register block — the analogue of
go-virtio's BAR accessor. `off` is a byte offset within the TPM register
window.

```go
type Regs interface {
        Read8(off uint32) uint8
        Read32(off uint32) uint32
        Write8(off uint32, v uint8)
        Write32(off uint32, v uint32)
}
```

Plus byte-stream helpers for the CRB command buffer / TIS FIFO:

```go
func ReadBytes(r Regs, off uint32, p []byte)
func WriteBytes(r Regs, off uint32, p []byte)
```

## Wire codec (codec.go) — BIG-ENDIAN

The TPM 2.0 wire encoding is **big-endian** throughout (TCG "TPM 2.0
Part 1: Architecture", data marshaling / canonicalization). Every
multi-byte field is most-significant-byte first.

```go
func BuildCommand(tag uint16, cc uint32, params []byte) []byte
func ParseResponse(rsp []byte) (tag uint16, rc uint32, params []byte, err error)

func MarshalTPM2B(b []byte) []byte
func UnmarshalTPM2B(b []byte) (val, rest []byte, err error)
```

Command/response header (10 bytes, `HeaderSize`):

```
command:  [ tag:u16 | commandSize:u32  | commandCode:u32  | params... ]
response: [ tag:u16 | responseSize:u32 | responseCode:u32 | params... ]
```

`TPM2B`: `[ size:u16 | bytes... ]`.

Big-endian primitive helpers (`PutU8/16/32/64`, `GetU8/16/32/64`)
back the framing code; the bounds-checked getters return `ok=false`
rather than panic. Typed sentinel errors `ErrShortBuffer` and
`ErrSizeMismatch` cover every short/inconsistent-buffer branch.

## Constants (const.go)

Typed, spec-cited starter sets: `TPM_ST`, `TPM_CC`, `TPM_RC`, `TPM_SU`,
`TPM_ALG`, and well-known `TPM_RH` handles. Each cites TCG "TPM 2.0
Part 2: Structures".

## Conventions

- Pure Go, `CGO_ENABLED=0`, no architecture-specific assembly.
- BSD-3-Clause on every file.
- 100% statement coverage (`GOWORK=off go test -cover ./...`).
- `GOWORK=off` — this module is not part of any workspace.
- Spec-traceable: every struct/const cites its TCG TPM 2.0 Part number.

## License

BSD-3-Clause. See [LICENSE](LICENSE).
