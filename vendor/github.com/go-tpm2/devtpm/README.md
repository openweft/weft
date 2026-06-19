<p align="center"><img src="https://raw.githubusercontent.com/go-tpm2/brand/main/social/go-tpm2.png" alt="go-tpm2/devtpm" width="720"></p>

# go-tpm2/devtpm

[![CI](https://github.com/go-tpm2/devtpm/actions/workflows/ci.yml/badge.svg)](https://github.com/go-tpm2/devtpm/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/go-tpm2/devtpm.svg)](https://pkg.go.dev/github.com/go-tpm2/devtpm)
[![Coverage](https://img.shields.io/badge/coverage-100%25-brightgreen)](#conventions)
[![License](https://img.shields.io/badge/license-BSD--3--Clause-blue)](LICENSE)

A pure-Go TPM 2.0 transport over the **Linux kernel TPM character device**
(`/dev/tpmrm0`). **v0.1.0.**

`devtpm` implements [`github.com/go-tpm2/common`](https://github.com/go-tpm2/common)'s
`Transport` interface over a Linux TPM character device. It is the
**node-side host-TPM transport** for weft remote attestation: on a real
Linux node it gives the [`attest`](https://github.com/go-tpm2/attest) Node
side a channel to the host's hardware (or firmware/vTPM) TPM.

Sibling repos: [`common`](https://github.com/go-tpm2/common) (interfaces +
codec), [`crb`](https://github.com/go-tpm2/crb) and
[`tis`](https://github.com/go-tpm2/tis) (the MMIO register transports for
firmware/bare-metal), [`tpm2`](https://github.com/go-tpm2/tpm2) (the
command layer that rides on this `Transport`),
[`attest`](https://github.com/go-tpm2/attest) (remote attestation), and
[`validate`](https://github.com/go-tpm2/validate) (live swtpm validation).

## Install

```sh
go get github.com/go-tpm2/devtpm
```

## Device model

The Linux `tpm` subsystem (`drivers/char/tpm`) exposes a TPM through two
character devices, both with the same framing:

| Device         | Role |
|----------------|------|
| `/dev/tpmrm0`  | **Resource-manager** channel (`DefaultDevice`). Each open fd is an independent, multiplexed command channel; the kernel RM virtualizes the TPM's scarce transient-object/session slots and flushes a client's context on close. **Prefer this.** |
| `/dev/tpm0`    | **Raw** channel. Single-open, unmediated, no handle virtualization — a leaked transient handle can wedge the whole TPM. |

The framing this package relies on: one `write()` delivers exactly one
complete command, and the matching `read()` returns exactly one complete
response (the driver buffers the response and returns it whole). `Send`
therefore does one `Write` of the full command and one `Read` of the full
response — no length prefix, no chunking.

Because that contract is just "raw TPM2 bytes, one command per write, one
response per read", `New` also accepts any `io.ReadWriteCloser` that honors
it — most usefully a unix socket to **swtpm's `--server` data channel**,
which speaks the identical raw TPM2 protocol. The `validate` harness uses
exactly that to drive this transport against a real swtpm on a host with no
`/dev/tpmrm0`.

## Usage

```go
import (
    "github.com/go-tpm2/attest"
    "github.com/go-tpm2/devtpm"
    "github.com/go-tpm2/tpm2"
)

// Open the host's resource-manager TPM channel.
dev, err := devtpm.Open(devtpm.DefaultDevice) // "/dev/tpmrm0"
if err != nil {
    // device missing, or insufficient privilege (root / tss group)
}
defer dev.Close()

// *devtpm.Transport satisfies common.Transport, so it plugs straight into
// the go-tpm2/tpm2 command layer and the attestation Node:
tpm := tpm2.New(dev)
node, err := attest.NewNode(tpm, pcrSel)
// node.Quote(nonce) … etc.
```

Or drive raw command buffers directly:

```go
cmd := common.BuildCommand(uint16(common.TagNoSessions),
    uint32(common.CCGetRandom), []byte{0x00, 0x02})
rsp, err := dev.Send(cmd) // common.Transport.Send
```

## Send framing

1. **Write** the entire command in one `Write`. A short count means the
   command was not delivered intact → `ErrShortWrite` (never retried; a
   re-issued tail would be misframed as a new command).
2. **Read** the response in one `Read` into a 4096-byte buffer (the TPM 2.0
   maximum); the driver returns the whole buffered response.
3. **Validate** at least a TPM 2.0 header was returned
   (`common.HeaderSize`) → else `ErrShortResponse`.

Underlying read/write errors are returned unwrapped so callers can inspect
the concrete `os`/`syscall` error.

## Security

`/dev/tpmrm0` is privileged (root or the `tss` group). Any process that can
open it can ask the TPM to sign with usable keys and can read every PCR;
treat the descriptor as a sensitive capability and keep it inside the node
agent. This transport carries opaque bytes only — all attestation policy
(which PCRs, which key, freshness) lives in
[`attest`](https://github.com/go-tpm2/attest).

## Conventions

Pure Go, `CGO_ENABLED=0`, no assembly, big-endian TPM wire (via `common`),
BSD-3-Clause, 100% statement coverage (`GOWORK=off go test -cover`),
`GOWORK=off`.

## References

- Linux kernel `drivers/char/tpm` — `tpm_dev_common.c` (one-write-one-command
  / one-read-one-response framing) and `tpm2-space.c` (the `/dev/tpmrm0`
  in-kernel resource manager).
- TCG TPM 2.0 Library, Parts 1–4 (wire format, via `common`).

## License

BSD-3-Clause. See [LICENSE](LICENSE).
