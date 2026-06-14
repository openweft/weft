package weft

// host_wg.go owns the host-side of weft's host-level WireGuard overlay mesh
// (cluster prereq #3 — the host mesh, distinct from the per-VM overlay in
// package overlay).
//
// The cardinal rule, mirroring overlay.EnsureOperatorKey and the per-VM mint:
// the host MINTS its WireGuard keypair LOCALLY and the private half NEVER
// leaves the node. Only the public key is registered with the control plane
// (Host.WGPublicKey) and only public keys + underlay endpoints + overlay /32s
// travel on the wire when the control plane fans the peer set back out. There
// is therefore no cleartext private key anywhere off-node — the same property
// Part 2 brings to the per-VM keys.
//
// Persistence: <stateDir>/host_wg.key (0o600), exactly like
// overlay.operatorKey, so a host keeps one stable mesh identity across
// restarts and binary upgrades.

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/openweft/weft/wgcoord"
)

// hostWGKeyFile is the host-local file the minted WG private key persists to.
const hostWGKeyFile = "host_wg.key"

// EnsureHostWGKey loads the host's stable WireGuard keypair from stateDir,
// minting and persisting the private half on first use. It returns ONLY the
// public key — the private key is written to <stateDir>/host_wg.key (0o600)
// and never returned to a caller that might put it on the wire.
//
// Mirrors overlay.EnsureOperatorKey: idempotent, stable across restarts, and
// the file is the single source of the node's mesh identity. A second call
// re-derives the public key from the persisted private key rather than
// minting a fresh one, so the host's pubkey is invariant.
func EnsureHostWGKey(stateDir string) (pub string, err error) {
	path := filepath.Join(stateDir, hostWGKeyFile)
	if b, rerr := os.ReadFile(path); rerr == nil {
		priv := string(b)
		pub, derr := wgcoord.PublicFromPrivate(priv)
		if derr != nil {
			return "", fmt.Errorf("host wg key %s: %w", path, derr)
		}
		return pub, nil
	}
	k, err := wgcoord.GenerateKey()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(k.Private), 0o600); err != nil {
		return "", fmt.Errorf("write host wg key: %w", err)
	}
	return k.Public, nil
}

// hostWGOverlayIndex derives the host's stable 1-based index in the overlay
// subnet from its UUID, persisting it alongside the key so the chosen index
// (and thus the wgcoord.Allocator-mapped overlay IP) is invariant across
// restarts even if the derivation ever changes.
//
// The control plane could instead assign indices centrally, but a
// node-derived stable index keeps host-mesh minting fully node-local (no
// round-trip needed to learn one's own overlay IP) and collision-tolerant:
// the index only has to be unique within a subnet that, for weft's 1/3-host
// clusters, holds at most a handful of hosts. maxHostIndex bounds the value
// into the usable host range of the default /24.
const maxHostIndex = 250

// hostWGIndexFromUUID maps a host UUID to a deterministic 1..maxHostIndex
// index by summing its bytes. Pure + tested ; the persistence wrapper lives
// in selfRegisterHost so this stays free of filesystem state.
func hostWGIndexFromUUID(uuid string) int {
	var sum int
	for i := 0; i < len(uuid); i++ {
		sum += int(uuid[i])
	}
	return sum%maxHostIndex + 1
}
