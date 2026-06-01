// Package federation is the v0.1.0 stub of weft's multi-cluster
// federation surface. See docs/design/federation.md for the design ;
// this file only carries the data shapes so future PRs (lite in
// v0.2, full primitives in v0.3) can grow against a stable JSON
// contract. Nothing here opens a socket or starts a daemon.
package federation

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Cluster is one federation member. Physical resources (VMs, hosts)
// are NOT federated, so a Cluster only carries what peers need to
// talk to it and bias placement.
type Cluster struct {
	Name             string   `json:"name"`                        // unique within a federation, DNS label
	Region           string   `json:"region"`                      // free-form locality, e.g. "eu-west-3"
	Datacenters      []string `json:"datacenters,omitempty"`       // DCs the cluster's etcd quorum covers
	Weight           int      `json:"weight,omitempty"`            // placement bias ; 0-on-wire means default
	PublicEndpoints  []string `json:"public_endpoints,omitempty"`  // weft-agent leader URLs (https)
	CertificateBytes []byte   `json:"certificate_bytes,omitempty"` // PEM CA cert peers pin
}

// FederationManifest is the signed source of truth for membership.
// Each cluster keeps its own copy ; on disagreement, the highest
// Version signed by a quorum wins.
type FederationManifest struct {
	Name    string    `json:"name"`    // operator-chosen, e.g. "acme-global"
	Version uint64    `json:"version"` // monotonic
	Members []Cluster `json:"members"` // 1..N, unique by Name
}

// Validate runs cheap structural checks. It does NOT verify the
// manifest's signature — that's Verifier's job.
func (m *FederationManifest) Validate() error {
	if m == nil {
		return errors.New("federation: nil manifest")
	}
	if m.Name == "" {
		return errors.New("federation: manifest.name is required")
	}
	if len(m.Members) == 0 {
		return errors.New("federation: manifest must list at least one member")
	}
	seen := make(map[string]struct{}, len(m.Members))
	for i, c := range m.Members {
		if c.Name == "" {
			return fmt.Errorf("federation: members[%d].name is required", i)
		}
		if _, dup := seen[c.Name]; dup {
			return fmt.Errorf("federation: duplicate member %q", c.Name)
		}
		seen[c.Name] = struct{}{}
		if c.Weight < 0 {
			return fmt.Errorf("federation: members[%d].weight %d must be >= 0", i, c.Weight)
		}
	}
	return nil
}

// Marshal returns the canonical JSON encoding of the manifest. The
// signing pipeline hashes this byte stream ; consumers go through
// Marshal so we own the format when it eventually matters.
func (m *FederationManifest) Marshal() ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(m)
}

// Unmarshal parses a JSON manifest and validates its structure.
// Pair with Verifier.Verify to check the signature.
func Unmarshal(b []byte) (*FederationManifest, error) {
	var m FederationManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("federation: decode manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// NormalisedWeight returns Weight with the zero-value default (100)
// applied. The JSON encoding keeps the raw zero so absent and
// explicit-zero remain distinguishable on the wire.
func (c Cluster) NormalisedWeight() int {
	if c.Weight == 0 {
		return 100
	}
	return c.Weight
}

// FindMember returns the member with the given Name, or nil if not
// found. O(N) — federations are small (single digits to low tens).
func (m *FederationManifest) FindMember(name string) *Cluster {
	if m == nil {
		return nil
	}
	for i := range m.Members {
		if m.Members[i].Name == name {
			return &m.Members[i]
		}
	}
	return nil
}

// Verifier checks a manifest's signature. v0.2 will plug in either a
// cosign-keyless verifier (GitHub OIDC) or an admin-key verifier
// behind this interface ; the zero value of DenyAllVerifier is the
// safe default for code paths not yet wired.
type Verifier interface {
	Verify(m *FederationManifest, sig []byte) error
}

// DenyAllVerifier rejects every signature — failing closed beats
// accidentally trusting unsigned manifests.
type DenyAllVerifier struct{}

// Verify implements Verifier.
func (DenyAllVerifier) Verify(_ *FederationManifest, _ []byte) error {
	return errors.New("federation: no verifier configured (deny-all)")
}

// VerifyManifest validates the manifest's structure then asks v to
// verify the signature. Returns the first error encountered.
func VerifyManifest(v Verifier, m *FederationManifest, sig []byte) error {
	if v == nil {
		return errors.New("federation: nil verifier")
	}
	if err := m.Validate(); err != nil {
		return err
	}
	return v.Verify(m, sig)
}
