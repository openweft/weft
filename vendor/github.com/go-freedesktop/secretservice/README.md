# secretservice

[![CI](https://github.com/go-freedesktop/secretservice/actions/workflows/ci.yml/badge.svg)](https://github.com/go-freedesktop/secretservice/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/go-freedesktop/secretservice.svg)](https://pkg.go.dev/github.com/go-freedesktop/secretservice)
[![License: BSD-3-Clause](https://img.shields.io/badge/License-BSD--3--Clause-blue.svg)](LICENSE)

Pure-Go (`CGO_ENABLED=0`) client for the freedesktop
[Secret Service API](https://specifications.freedesktop.org/secret-service/)
(`org.freedesktop.Secret.Service`) spoken over D-Bus through
[`github.com/godbus/dbus/v5`](https://github.com/godbus/dbus). It talks to GNOME
Keyring, KWallet, or any conforming secret daemon — with **no cgo** and **never
shelling out to `secret-tool`**.

It is the Linux backend of the [`go-keyring`](https://github.com/go-keyring/keyring)
cross-platform façade, but is usable on its own.

## API

Three byte-oriented calls over a `(service, account)` pair — each backed by one
Secret Service item whose lookup attributes are `{service, account}` — plus an
availability probe:

```go
import "github.com/go-freedesktop/secretservice"

// Store (adds on first write, replaces in place afterwards).
err := secretservice.Set("my-app", "alice@example.com", []byte(secret))

// Read (secretservice.ErrNotFound when absent).
secret, err := secretservice.Get("my-app", "alice@example.com")

// Read an item created by another program (KeePassXC, secret-tool...) by any
// of its attributes: the one item carrying all of them, ErrNotFound for none,
// ErrAmbiguous for several.
secret, err = secretservice.GetByAttributes(map[string]string{"Title": "Yubikey PIN"})

// Create (or replace in place) an item with arbitrary attributes.
err = secretservice.SetByAttributes("Yubikey PIN", map[string]string{"Title": "Yubikey PIN"}, []byte(pin))

// Remove (deleting an absent item is not an error).
err = secretservice.Delete("my-app", "alice@example.com")

// Is a daemon reachable at all?
ok := secretservice.Available()
```

Each call opens a fresh session-bus connection, opens a **plain-algorithm**
session, resolves and unlocks the default collection, performs the operation,
and closes the connection.

Errors are typed and comparable with `errors.Is`:

| Error | Meaning |
| --- | --- |
| `ErrNotFound` | `Get` found no item for the `{service, account}` attributes |
| `ErrAmbiguous` | `GetByAttributes` matched several items; none is picked |
| `ErrNoAttributes` | `GetByAttributes` was given no attribute (it would match everything) or more than 32 |
| `ErrUnavailable` | no session bus, no daemon owning `org.freedesktop.secrets`, or a non-Linux platform |

## No silent plaintext fallback

On a headless box with no running Secret Service daemon — or on any non-Linux
platform — every entry point returns `ErrUnavailable`. The package never invents
an alternative store or writes a plaintext secret somewhere: a caller that sees
`ErrUnavailable` knows nothing was read or written.

## Platforms

The Secret Service is a freedesktop/Linux facility, so the live backend is
`//go:build linux`. Every exported symbol is defined on all platforms so
consumers cross-compile; off Linux the functions return `ErrUnavailable`.

## Testing

The OS-independent surface (dispatch, error mapping, availability, the
unreachable-bus path) is covered to **100%** on every lane through an injected
backend seam — no daemon needed. The real godbus backend is proven by a gated,
opt-in live round trip against a running `gnome-keyring-daemon`
(`store → get → overwrite → delete → not-found`), controlled by an env gate so a
missing daemon is a failure, never a silent skip:

```sh
CGO_ENABLED=0 go test ./...          # unit lane, no daemon

# live lane (Linux, with a Secret Service daemon):
dbus-run-session -- sh -c '
  printf "" | gnome-keyring-daemon --unlock --components=secrets &
  sleep 1
  SECRETSERVICE_LIVE=1 go test -run TestLiveSecretService -v ./...
'
```

## License

BSD-3-Clause. See [LICENSE](LICENSE).
