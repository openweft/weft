package weft

// creds.go owns the small bits of NATS-credential plumbing vzd
// needs to land Phase 2 of [[vzd-tenant-event-access]]: mint one
// per-project NATS user NKey and materialise it into each microVM
// so workloads inside the guest can authenticate to the bus on
// their own (without an OIDC token, which they don't have).
//
// Phase 2 scope (this file):
//
//   - generateProjectUserNKey() returns a fresh user NKey seed +
//     its public key. The seed is what tenants present to the
//     server; the pubkey is what server-side authorization rules
//     will eventually reference (per-pubkey subject permissions
//     on `vzd.events.project.<uuid>.events.>`).
//
//   - The seed lives on the Project struct (NATSUserSeed) so it
//     survives a vzd restart: a tenant that already has the seed
//     baked into its VM image keeps working after a control-plane
//     bounce.
//
// What we explicitly do NOT do here:
//
//   - No JWT minting. A full `.creds` file (the format
//     `nats.UserCredentials` consumes) requires an
//     operator/account NKey hierarchy + nats-io/jwt v2; that
//     lands in Phase 3 alongside server-side enforcement.
//     Today's tenants connect with `nats.Nkey(pubkey, kp.Sign)`
//     directly off the seed file — see runner README.
//
//   - No server-side permissions config. The seed is harmless
//     until the NATS server is set up to know which pubkeys
//     correspond to which projects (Phase 3).

import (
	"fmt"

	"github.com/nats-io/nkeys"
)

// natsShareTag is the conventional virtio-fs mount tag the guest
// uses to find its per-project NATS credentials. ncl-init (or any
// other guest agent) mounts this tag at /run/vzd/ so workloads can
// read /run/vzd/nats.nkey directly. Reserved name — RegisterMicroVM
// refuses caller-supplied shares using the same tag.
const natsShareTag = "vzd-nats"

// projectNKey carries the encoded seed + matching public key.
// Both are ASCII (NKey base32 encoding), safe to embed in HCL.
type projectNKey struct {
	Seed   string // "SU..." — keep secret, 0600 on disk
	Public string // "U..."  — safe to log
}

// generateProjectUserNKey mints a fresh user NKey using the same
// Ed25519-based key system NATS expects. The KeyPair is discarded
// after extraction; callers store the seed and re-load via
// nkeys.FromSeed when they need to sign.
func generateProjectUserNKey() (projectNKey, error) {
	kp, err := nkeys.CreateUser()
	if err != nil {
		return projectNKey{}, fmt.Errorf("nkeys.CreateUser: %w", err)
	}
	seed, err := kp.Seed()
	if err != nil {
		return projectNKey{}, fmt.Errorf("user nkey seed: %w", err)
	}
	pub, err := kp.PublicKey()
	if err != nil {
		return projectNKey{}, fmt.Errorf("user nkey public: %w", err)
	}
	return projectNKey{Seed: string(seed), Public: pub}, nil
}

// publicKeyFromSeed re-derives the public key from a stored seed.
// Used by callers that hold the seed (registry entry) and need
// the pubkey for diagnostics or server-config rendering.
func publicKeyFromSeed(seed string) (string, error) {
	kp, err := nkeys.FromSeed([]byte(seed))
	if err != nil {
		return "", fmt.Errorf("nkeys.FromSeed: %w", err)
	}
	pub, err := kp.PublicKey()
	if err != nil {
		return "", fmt.Errorf("derive pubkey: %w", err)
	}
	return pub, nil
}
