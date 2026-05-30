package pod

import (
	"encoding/json"
	"fmt"
	"os"
)

// LoadWireGuard reads a standalone WireGuard overlay config (the same shape
// as Spec.WireGuard) from path. This is how the host delivers a per-VM
// overlay without owning the whole pod spec: weft drops wireguard.json into
// the config share alongside pod.json, and weft-init applies it when the
// pod spec itself carries no WireGuard block.
//
// A missing file is not an error — it just means no overlay was provisioned
// (returns nil, nil).
func LoadWireGuard(path string) (*WireGuard, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var wg WireGuard
	if err := json.Unmarshal(b, &wg); err != nil {
		return nil, fmt.Errorf("wireguard config %s: %w", path, err)
	}
	if err := wg.Validate(); err != nil {
		return nil, fmt.Errorf("wireguard config %s: %w", path, err)
	}
	return &wg, nil
}

// Validate checks the overlay config has the minimum fields the kernel
// configuration needs.
func (w *WireGuard) Validate() error {
	if w.PrivateKey == "" {
		return fmt.Errorf("private_key is required")
	}
	if w.Address == "" {
		return fmt.Errorf("address is required")
	}
	for i, p := range w.Peers {
		if p.PublicKey == "" {
			return fmt.Errorf("peer[%d]: public_key is required", i)
		}
		if len(p.AllowedIPs) == 0 {
			return fmt.Errorf("peer[%d]: at least one allowed_ip is required", i)
		}
	}
	return nil
}
