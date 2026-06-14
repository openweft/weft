//go:build !linux

package floatingipl2

import "sync"

// StubProgrammer is the darwin / cross-platform-dev fallback.
// Apply validates the payload + records it so a host-side
// integration test can assert the desired state without touching
// netlink. weft-agent never runs in production off Linux ; the
// stub keeps `go build ./...` green on the darwin dev box.
type StubProgrammer struct {
	mu   sync.Mutex
	last []L2Mapping
}

// NewStubProgrammer returns an empty StubProgrammer.
func NewStubProgrammer() *StubProgrammer { return &StubProgrammer{} }

// Apply validates the payload (so a malformed mapping still
// errors on darwin, matching the linux behaviour) and stores it.
// Always returns nil after Validate succeeds.
func (p *StubProgrammer) Apply(mappings []L2Mapping) error {
	if err := ValidateMappings(mappings); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := make([]L2Mapping, len(mappings))
	copy(cp, mappings)
	p.last = cp
	return nil
}

// Last returns a copy of the most recent Apply payload. The
// integration loop's tests read it to assert the right L2 set
// was derived from the events.
func (p *StubProgrammer) Last() []L2Mapping {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]L2Mapping, len(p.last))
	copy(out, p.last)
	return out
}

var _ Programmer = (*StubProgrammer)(nil)
