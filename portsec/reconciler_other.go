//go:build !linux

package portsec

import (
	"sync"
	"time"
)

// StubReconciler is the darwin / cross-platform fallback. Records
// the last Apply payload so a host-side test on darwin can assert
// the desired state without bridge-family nftables (which is
// linux-only). weft-agent never runs in production off Linux.
type StubReconciler struct {
	mu   sync.Mutex
	last []AntispoofRule
}

// NewStubReconciler returns an empty StubReconciler.
func NewStubReconciler() *StubReconciler { return &StubReconciler{} }

// Apply validates + records. Always nil after Validate passes.
func (r *StubReconciler) Apply(rules []AntispoofRule) (retErr error) {
	start := time.Now()
	defer func() { recordApply(rules, retErr, time.Since(start).Seconds()) }()
	if err := ValidateRules(rules); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]AntispoofRule, len(rules))
	copy(cp, rules)
	r.last = cp
	return nil
}

// Last returns a copy of the most recent Apply payload.
func (r *StubReconciler) Last() []AntispoofRule {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]AntispoofRule, len(r.last))
	copy(out, r.last)
	return out
}

var _ Reconciler = (*StubReconciler)(nil)
