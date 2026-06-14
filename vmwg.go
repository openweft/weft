package weft

// vmwg.go is Part 2 of the host-WG-mesh work: protecting the per-VM
// WireGuard keys so a VM's private key never travels in cleartext over NATS.
//
// THE PROBLEM. Today mesh.Build (driven by Adapter.PublishMesh) bakes each
// VM's PrivateKey into the pod.WireGuard config it publishes on
// weft.mesh.<vmID> — mesh.Member.Key holds BOTH halves, and the private half
// goes onto the bus in the clear. overlay.Provision has the same shape on the
// CLI/control-plane side. Any NATS subscriber (or a compromised broker) sees
// every VM's private key.
//
// THE CONSTRAINT. The natural fix would be to seal the key to the hosting
// node's TPM PCRs so only an attested node can read it. But the CONTROL PLANE
// HAS NO TPM: tpm2.SealToPCR is a TPM command and must run on the machine
// whose PCRs are being sealed against. The control plane cannot seal a secret
// to a REMOTE node's PCRs with the primitives go-tpm2 ships (CreateEK,
// MakeCredential, ActivateCredential, SealToPCR/UnsealWithPCR, Create/Load/
// Unseal). Doing so would require TPM2_Import / a duplicate-to-remote-SRK-
// under-PCR-policy flow, which go-tpm2 does NOT implement (see the GAP note
// at the bottom of this file).
//
// THE SHIPPING MECHANISM — node-side minting. We mirror Part 1's host-mesh
// design exactly: the agent that runs the VM MINTS the VM's WireGuard private
// key LOCALLY (EnsureVMWGKey, the analogue of EnsureHostWGKey), persists it in
// the VM's per-instance directory (0o600), and the private key NEVER leaves
// the node. The control plane learns only the PUBLIC key and builds the VM
// mesh peer set from public keys via wgcoord.MeshPeers — the same pubkey-only
// distribution BuildHostMesh/PublishHostMesh use for the host mesh. This
// eliminates the cleartext key WITHOUT any sealing primitive, because there is
// no longer a secret for the control plane to deliver: the secret is born and
// dies on the node.
//
// This file is ADDITIVE. The legacy mesh.Build / PublishMesh / overlay.
// Provision paths are untouched; callers opt into the secret-free path by
// minting node-side (EnsureVMWGKey) + publishing via PublishVMMesh.

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"

	"github.com/openweft/weft/wgcoord"
)

// vmWGKeyFile is the per-VM file the node-minted WG private key persists to,
// inside the VM's instance directory. 0o600, same as host_wg.key.
const vmWGKeyFile = "vm_wg.key"

// EnsureVMWGKey mints the VM's WireGuard keypair LOCALLY on the hosting node,
// persisting the private half in vmDir/vm_wg.key (0o600) and returning ONLY
// the public key. The private key never leaves the node — it is staged into
// the VM's config share locally by the agent, never published on NATS.
//
// Idempotent and stable across VM restarts, exactly like EnsureHostWGKey: a
// second call re-derives the public key from the persisted private key rather
// than re-minting, so a VM keeps one mesh identity for its lifetime.
func EnsureVMWGKey(vmDir string) (pub string, err error) {
	path := filepath.Join(vmDir, vmWGKeyFile)
	if b, rerr := os.ReadFile(path); rerr == nil {
		pub, derr := wgcoord.PublicFromPrivate(string(b))
		if derr != nil {
			return "", fmt.Errorf("vm wg key %s: %w", path, derr)
		}
		return pub, nil
	}
	k, err := wgcoord.GenerateKey()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(vmDir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(k.Private), 0o600); err != nil {
		return "", fmt.Errorf("write vm wg key: %w", err)
	}
	return k.Public, nil
}

// VMMeshMember is the control-plane view of one VM on the overlay mesh AFTER
// node-side minting: its id, its PUBLIC key only (the node holds the private
// half), its overlay address, and its underlay endpoint. Contrast
// mesh.Member, which carries the full wgcoord.Key (both halves) — that is the
// type behind the cleartext-on-NATS problem this path replaces.
type VMMeshMember struct {
	VMID      string
	PublicKey string
	OverlayIP netip.Addr
	Endpoint  wgcoord.Endpoint
}

// VMPeerSet is the message published on weft.mesh.<vmID> by the secret-free
// path: the full desired peer set for one VM, carrying PUBLIC material only.
// The in-VM agent applies these peers on top of the private key the hosting
// node minted locally and staged into the VM's config share — the private key
// is never on the bus. Shape mirrors HostPeerSet.
type VMPeerSet struct {
	Self  string       `json:"self"`
	Peers []VMMeshPeer `json:"peers"`
}

// VMMeshPeer is one peer in a VMPeerSet — public key + endpoint + overlay /32.
type VMMeshPeer struct {
	PublicKey           string   `json:"public_key"`
	Endpoint            string   `json:"endpoint,omitempty"`
	AllowedIPs          []string `json:"allowed_ips"`
	PersistentKeepalive uint16   `json:"persistent_keepalive,omitempty"`
}

// BuildVMMesh computes every VM's VMPeerSet from the current (node-minted)
// membership — public keys only, no private material anywhere. Maps VM id →
// its full desired peer set. Pure (no NATS), fully unit-testable.
func BuildVMMesh(subnet netip.Prefix, members []VMMeshMember, keepalive uint16) (map[string]VMPeerSet, error) {
	if !subnet.IsValid() {
		return nil, fmt.Errorf("invalid subnet")
	}
	bits := subnet.Bits()
	peerView := make([]wgcoord.Member, len(members))
	for i, m := range members {
		peerView[i] = wgcoord.Member{PublicKey: m.PublicKey, OverlayIP: m.OverlayIP, Endpoint: m.Endpoint}
	}
	// Deterministic ordering for stable publishing/testing.
	ordered := append([]VMMeshMember(nil), members...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].VMID < ordered[j].VMID })

	out := make(map[string]VMPeerSet, len(ordered))
	for _, m := range ordered {
		ps := VMPeerSet{Self: netip.PrefixFrom(m.OverlayIP, bits).String()}
		for _, p := range wgcoord.MeshPeers(m.OverlayIP, peerView, keepalive) {
			ps.Peers = append(ps.Peers, VMMeshPeer{
				PublicKey:           p.PublicKey,
				Endpoint:            p.Endpoint,
				AllowedIPs:          p.AllowedIPs,
				PersistentKeepalive: p.PersistentKeepalive,
			})
		}
		out[m.VMID] = ps
	}
	return out, nil
}

// PublishVMMesh is the secret-free counterpart of Adapter.PublishMesh: it
// recomputes every VM's peer set from node-minted PUBLIC keys and fans it out
// on each VM's existing mesh subject (mesh.Subject → weft.mesh.<vmID>), so the
// in-VM agent subscribes exactly as before — only now the wire carries no
// private key. Returns the number of VMs published to.
//
// Members come pre-resolved (the hosting nodes minted the keys and reported
// the public halves), mirroring PublishMesh's "already-resolved members"
// contract. The difference is purely that PublicKey replaces the full Key, so
// there is structurally no private key to leak.
func (a *Adapter) PublishVMMesh(members []VMMeshMember, subnet netip.Prefix, keepalive uint16) (int, error) {
	nc, err := natsConnFromBus(a.bus)
	if err != nil {
		return 0, err
	}
	sets, err := BuildVMMesh(subnet, members, keepalive)
	if err != nil {
		return 0, err
	}
	for vmID, ps := range sets {
		data, err := json.Marshal(ps)
		if err != nil {
			return 0, err
		}
		// Reuse the per-VM mesh subject the in-VM agent already subscribes to.
		if err := nc.Publish("weft.mesh."+vmID, data); err != nil {
			return 0, fmt.Errorf("publish vm mesh %s: %w", vmID, err)
		}
	}
	if err := nc.Flush(); err != nil {
		return 0, err
	}
	return len(sets), nil
}

// ─────────────────────────────────────────────────────────────────────────
// go-tpm2 GAP NOTE — control-plane-originated sealed secrets.
//
// The node-mint path above is the SHIPPING mechanism and needs no sealing
// primitive. It is the soundest fit for weft's mesh model: keys are derived
// state, born and held on the node that uses them.
//
// IF a future requirement needs the control plane to MINT a secret and
// deliver it bound to a remote node's PCRs — e.g. a shared mesh PSK, or VM
// keys that MUST originate control-plane-side — that is a fundamentally
// different operation: sealing an EXTERNAL secret to a REMOTE TPM. It cannot
// be done with go-tpm2 today:
//
//   - tpm2.SealToPCR / Create seal a secret UNDER THE LOCAL TPM's storage
//     primary; they run ON the node, against ITS PCRs. The control plane has
//     no TPM, so it can't call them, and it can't target a remote node's PCRs.
//   - MakeCredential / ActivateCredential can encrypt a small secret to a
//     node's EK such that only that TPM recovers it (off-TPM MakeCredential
//     exists in go-tpm2) — BUT activation is NOT bound to a PCR policy, so it
//     proves "this is the right TPM," not "the node is in the right
//     (measured-boot) state." It's an identity bind, not a state bind.
//
// The missing primitive is TPM2_Import (a.k.a. wrapping an external secret to
// a target's storage-key public under an inner+outer wrap with a PCR policy),
// or equivalently a Duplicate-style import. PROPOSED go-tpm2 addition:
//
//     // package tpm2 (new, control-plane usable — no live TPM needed)
//     //
//     // WrapToPCR wraps secret to the target node's storage-key PUBLIC
//     // (srkPub, obtained from the node at admission) under a PCR policy, so
//     // only that TPM, in the policy-matching PCR state, can import+unseal it.
//     // Pure crypto over the public key — runnable on the TPM-less control
//     // plane. Produces a TPM2B_Private + TPM2B_Public the node feeds to:
//     func WrapToPCR(srkPub []byte, secret []byte, pcrPolicy []byte) (duplicate, inSymSeed, outPublic []byte, err error)
//
//     // (tpm *TPM) Import + the existing Load/UnsealWithPCR complete the
//     // node-side recovery:
//     func (tpm *TPM) Import(parent uint32, outPublic, duplicate, inSymSeed []byte) (priv []byte, err error)
//
// Until that lands, control-plane-originated sealed secrets are NOT
// implementable with go-tpm2, and weft uses node-side minting (this file).
// This is mapped, not faked.
// ─────────────────────────────────────────────────────────────────────────
