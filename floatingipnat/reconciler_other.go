//go:build !linux

package floatingipnat

import "sync"

// StubReconciler is the non-Linux fallback. Apply records the
// payload it would have installed so host-side tests on darwin
// can assert on the desired state without touching nftables.
//
// The non-Linux build never reaches a production code path —
// weft-agent always runs on Linux hosts — but the cross-platform
// build needs to stay green so a darwin developer can iterate on
// the integration loop without a Linux VM in the loop.
type StubReconciler struct {
	mu   sync.Mutex
	last []NATMapping
}

// NewStubReconciler returns an empty StubReconciler.
func NewStubReconciler() *StubReconciler { return &StubReconciler{} }

// Apply validates the payload (so a malformed mapping still
// surfaces an error on darwin, matching the linux behaviour) and
// stores it. Always returns nil after Validate succeeds.
func (r *StubReconciler) Apply(mappings []NATMapping) error {
	if err := ValidateMappings(mappings); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]NATMapping, len(mappings))
	copy(cp, mappings)
	r.last = cp
	return nil
}

// Last returns a copy of the most recent Apply payload. The
// integration loop's tests read it to assert the right mappings
// were derived.
func (r *StubReconciler) Last() []NATMapping {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]NATMapping, len(r.last))
	copy(out, r.last)
	return out
}

var _ Reconciler = (*StubReconciler)(nil)
