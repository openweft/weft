//go:build !linux

package portqos

import (
	"sync"
	"time"
)

// StubReconciler records the last Apply payload for darwin builds
// + cross-platform tests.
type StubReconciler struct {
	mu   sync.Mutex
	last []PortQoS
}

// NewStubReconciler returns an empty StubReconciler.
func NewStubReconciler() *StubReconciler { return &StubReconciler{} }

// Apply validates + records.
func (r *StubReconciler) Apply(specs []PortQoS) (retErr error) {
	start := time.Now()
	defer func() { recordApply(specs, retErr, time.Since(start).Seconds()) }()
	if err := ValidateSpecs(specs); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]PortQoS, len(specs))
	copy(cp, specs)
	r.last = cp
	return nil
}

// Last returns a copy of the most recent Apply payload.
func (r *StubReconciler) Last() []PortQoS {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]PortQoS, len(r.last))
	copy(out, r.last)
	return out
}

var _ Reconciler = (*StubReconciler)(nil)
