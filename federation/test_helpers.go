package federation

// test_helpers.go exposes a narrow seam for tests in sibling packages
// to drive the Poller's classification deterministically without
// HTTP plumbing. Keep this file lean — the only allowed callers are
// _test.go files in this module.

// SetPeerStateForTest installs a synthetic PeerState row for a peer
// URL. The Poller's internal state map is package-private, so cross-
// package tests (e.g. cmd/weft) reach in through this helper. The
// poller's Snapshot then classifies the row via the standard
// LastSeen / LastError / StaleTTL rule.
//
// Test-only — production code paths populate the state through
// PollOnce / Start.
func SetPeerStateForTest(p *Poller, url string, st *PeerState) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.states == nil {
		p.states = make(map[string]*PeerState)
	}
	p.states[url] = st
}
