# safeio

[![Go Reference](https://pkg.go.dev/badge/github.com/go-volumes/safeio.svg)](https://pkg.go.dev/github.com/go-volumes/safeio)
[![License: BSD-3-Clause](https://img.shields.io/badge/license-BSD--3--Clause-blue)](LICENSE)
[![CI](https://github.com/go-volumes/safeio/actions/workflows/ci.yml/badge.svg)](https://github.com/go-volumes/safeio/actions/workflows/ci.yml)

Bounds, allocation, and cycle guards for parsing **untrusted binary data**.

`safeio` is a small, dependency-free (stdlib-only, `CGO_ENABLED=0`) toolkit for
the block layer of the [go-volumes](https://github.com/go-volumes) ecosystem.
On-disk images — partition tables, filesystem metadata, boot records — are
attacker-controlled input. A malicious image must never panic the host, read
out of bounds, integer-overflow into a bad allocation or slice, loop forever,
or OOM. This package makes the four near-universal defenses easy to apply:

- **(A) unbounded `make([]byte, N)` → OOM:** `MakeBytes`, `ReadAtFull`
- **(B) unbounded chain/tree traversal → infinite loop:** `LoopGuard`, `VisitSet`
- **(C) fixed-offset read without a length check:** `CheckBounds`, `Slice`
- **(D) unvalidated geometry → divide-by-zero:** (callers compare to 0)

All helpers return errors instead of panicking. The sentinel errors wrap
`ErrSafeIO`, so callers can match either the specific cause
(`errors.Is(err, ErrTooLarge)`) or the family (`errors.Is(err, ErrSafeIO)`).

It is the neutral, shared parsing-hardening library consumed by the
go-volumes block stack and (later) go-bootloaders.

## Usage

```go
import "github.com/go-volumes/safeio"

// (A) Cap an attacker-supplied allocation size.
buf, err := safeio.MakeBytes(n, maxBytes)
if err != nil {
    return err // errors.Is(err, safeio.ErrTooLarge)
}

// (C) Bounds-check a fixed-offset read against the input length.
field, err := safeio.Slice(data, off, length)

// (B) Bound a linked-list / tree walk to a maximum number of steps.
g := safeio.NewLoopGuard(1 << 16)
for next != 0 {
    if err := g.Next(); err != nil {
        return err // errors.Is(err, safeio.ErrLoopLimit)
    }
    // ... advance ...
}
```
