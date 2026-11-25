# keychain

[![CI](https://github.com/go-macos/keychain/actions/workflows/ci.yml/badge.svg)](https://github.com/go-macos/keychain/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/go-macos/keychain.svg)](https://pkg.go.dev/github.com/go-macos/keychain)
[![License: BSD-3-Clause](https://img.shields.io/badge/License-BSD--3--Clause-blue.svg)](LICENSE)

Pure-Go (`CGO_ENABLED=0`) store for macOS Keychain **generic-password** items.
It reaches the OS through [`ebitengine/purego`](https://github.com/ebitengine/purego)
— `dlsym`'d CoreFoundation and Security C functions — so it links with **no cgo**
and **never shells out to `/usr/bin/security`** (the secret never appears in any
process's argv). It is a sibling of `go-macos/notify` and reuses
[`go-macos/objc`](https://github.com/go-macos/objc) for the canonical framework
paths.

## API

Three byte-oriented calls over a `(service, account)` pair, each backed by one
`kSecClassGenericPassword` item:

```go
import "github.com/go-macos/keychain"

// Store (adds on first write, overwrites in place afterwards).
err := keychain.Set("my-app", "alice@example.com", []byte(secretJSON))

// Read (keychain.ErrNotFound when absent).
secret, err := keychain.Get("my-app", "alice@example.com")

// Remove (deleting an absent item is not an error).
err = keychain.Delete("my-app", "alice@example.com")
```

Errors are typed and comparable with `errors.Is` / `errors.As`:

| Error | Meaning |
| --- | --- |
| `ErrNotFound` | `Get` found no item for the pair |
| `ErrEmptySecret` | `Set` was given an empty secret (the Keychain cannot hold one) |
| `ErrUnsupported` | called on a non-darwin platform |
| `*Error` | any other Security-framework failure; carries `Op` and the raw `Status` (OSStatus) |

### Access control

By default an item is a plain generic password. To protect it with a
`SecAccessControl`, pass `WithAccessControl`:

```go
// Gate every read behind Touch ID or the device passcode.
err := keychain.Set("my-app", "alice", secret, keychain.WithAccessControl(keychain.UserPresence))

// At-rest protection (WhenUnlockedThisDeviceOnly) with no interactive prompt.
err := keychain.Set("my-app", "alice", secret, keychain.WithAccessControl(0))
```

Access-controlled items are pinned to
`kSecAttrAccessibleWhenUnlockedThisDeviceOnly` (never synced to iCloud) and are
replaced atomically (delete-then-add) on each write. Reading a `UserPresence`
item triggers the system biometric prompt.

## Why not `zalando/go-keyring` or `os/exec`?

`go-keyring` shells out to the `security` CLI on macOS, which puts the secret on
a command line and adds a runtime dependency on an external binary. This package
binds the Security framework in-process instead — nothing to install, nothing in
argv, and it builds `CGO_ENABLED=0`.

## Platforms

Darwin only. Every exported symbol is defined on all platforms so consumers
cross-compile; on non-darwin `GOOS` the functions return `ErrUnsupported`.

## Testing

The darwin lane runs a real, on-device `store → get → update → delete` round
trip against the login Keychain (plain and access-controlled, no biometric
prompt), plus an FFI-plumbing check that every symbol resolved. The
OS-independent logic (empty-secret guard, not-found mapping, error wrapping,
load-error propagation) is covered to 100% on every lane through injected
backend seams. `CGO_ENABLED=0` throughout.

```
CGO_ENABLED=0 go test ./...
```

## License

BSD-3-Clause. See [LICENSE](LICENSE).
