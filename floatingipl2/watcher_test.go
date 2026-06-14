package floatingipl2

import (
	"context"
	"sync"
	"testing"

	weft "github.com/openweft/weft"
)

type recorderProgrammer struct {
	mu       sync.Mutex
	applied  [][]L2Mapping
	applyErr error
}

func (r *recorderProgrammer) Apply(m []L2Mapping) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]L2Mapping, len(m))
	copy(cp, m)
	r.applied = append(r.applied, cp)
	return r.applyErr
}

func (r *recorderProgrammer) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.applied)
}

func TestWatcher_ShouldReact(t *testing.T) {
	w := New("host-1", &fakeScope{}, &recorderProgrammer{}, nil)
	for _, k := range []string{
		"floating_ip.mapped", "floating_ip.unmapped", "floating_ip.released",
		"vm.created", "vm.deleted", "vm.migrated", "vm.state_changed",
		"network.updated",
	} {
		if !w.shouldReact(weft.PlatformEvent{Kind: k}) {
			t.Errorf("kind %q should trigger reconcile", k)
		}
	}
	for _, k := range []string{
		"floating_ip.allocated", // L2 doesn't care until map
		"port.created",          // private-IP side, NAT path only
		"security_group.rules_updated",
		"",
	} {
		if w.shouldReact(weft.PlatformEvent{Kind: k}) {
			t.Errorf("kind %q should NOT trigger reconcile", k)
		}
	}
}

func TestWatcher_RunInitialSyncThenEventDriven(t *testing.T) {
	scope := &fakeScope{
		vms: map[string][]weft.VM{
			"host-1": {{UUID: "vm-a-uuid", Name: "vm-a"}},
		},
		fips: []weft.FloatingIP{
			{Address: "192.168.50.42", Status: weft.FIPStatusActive,
				TargetKind: weft.FIPTargetVM, MappedTo: "vm-a", NetworkUUID: "net-vlan"},
		},
		networks: map[string]weft.Network{
			"net-vlan": {UUID: "net-vlan", ExternalMode: weft.NetworkExternalVLAN, VLAN: 100, ParentInterface: "eth0"},
		},
	}
	rec := &recorderProgrammer{}
	w := New("host-1", scope, rec, nil)

	events := make(chan weft.PlatformEvent, 3)
	events <- weft.PlatformEvent{Kind: "floating_ip.allocated"} // ignored
	events <- weft.PlatformEvent{Kind: "floating_ip.mapped"}    // triggers
	events <- weft.PlatformEvent{Kind: "vm.migrated"}           // triggers
	close(events)

	if err := w.Run(context.Background(), events); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 1 initial sync + 2 event-driven = 3.
	if got := rec.calls(); got != 3 {
		t.Errorf("expected 3 Apply calls, got %d", got)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for i, c := range rec.applied {
		if len(c) != 1 || c[0].PublicIP != "192.168.50.42" {
			t.Errorf("call[%d] = %+v", i, c)
		}
	}
}
