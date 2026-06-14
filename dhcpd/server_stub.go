package dhcpd

import (
	"context"
	"errors"
	"sync"
)

// StubServer is a no-op Server implementation usable on every
// platform. It records every Resolve hit so tests can assert that
// a downstream pipeline (Port → Source → would-be Lease) produced
// the right lease without actually serving UDP.
//
// Use it cross-platform until the protocol implementation lands :
// the integration surface (Server interface + Options + Source) is
// fully exercisable today, just no real packets fly.
type StubServer struct {
	opts Options
	mu   sync.Mutex
	hits []ResolveHit
}

// ResolveHit records one Source.Resolve call the server would
// have done on a real DISCOVER. Lets tests verify the lease
// computation path end-to-end.
type ResolveHit struct {
	MAC     string
	Lease   Lease
	Issued  bool
}

// NewStub builds a StubServer. Returns an error on bad Options.
// The Stub doesn't actually listen ; Run blocks until ctx is
// cancelled then returns ctx.Err().
func NewStub(opts Options) (*StubServer, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	return &StubServer{opts: opts}, nil
}

// Run blocks until ctx is cancelled. Doesn't bind any sockets.
func (s *StubServer) Run(ctx contextLike) error {
	if ctx == nil {
		return errors.New("dhcpd.StubServer.Run: nil ctx")
	}
	<-ctx.Done()
	return ctx.Err()
}

// SimulateRequest drives the Source as if a DISCOVER for mac
// arrived. Tests use this to verify the lease pipeline. Returns
// the resolved Lease + whether the server would have issued.
func (s *StubServer) SimulateRequest(mac string) (Lease, bool) {
	lease, ok := s.opts.Source.Resolve(mac)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hits = append(s.hits, ResolveHit{MAC: mac, Lease: lease, Issued: ok})
	return lease, ok
}

// Hits returns a copy of every SimulateRequest call recorded so
// far. Used by tests to assert ordering / Lease shape.
func (s *StubServer) Hits() []ResolveHit {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ResolveHit, len(s.hits))
	copy(out, s.hits)
	return out
}

// Compile-time check that context.Context satisfies contextLike
// — the caller will typically pass a context.Context here.
var _ contextLike = context.Background()
