package firewallpub

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"sort"
	"sync"
	"testing"

	weft "github.com/openweft/weft"
	"github.com/openweft/weft-microvm-init/pkg/pod"
)

// fakeScope extends fakeSnap with the publisher's wider lookups.
type fakeScope struct {
	*fakeSnap
	allPorts []weft.Port
}

func (f *fakeScope) ListAllPorts() []weft.Port { return f.allPorts }

// recorderPub records every (vmUUID, fw) the publisher emits, so
// tests can assert order + payload without involving NATS.
type recorderPub struct {
	mu      sync.Mutex
	entries []recorded
}

type recorded struct {
	vmUUID string
	fw     pod.Firewall
}

func (r *recorderPub) fn() PublishFunc {
	return func(vm string, fw pod.Firewall) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.entries = append(r.entries, recorded{vm, fw})
		return nil
	}
}

func (r *recorderPub) snapshot() []recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recorded, len(r.entries))
	copy(out, r.entries)
	return out
}

func newScope() *fakeScope {
	return &fakeScope{fakeSnap: &fakeSnap{
		portsByVM:      map[string][]weft.Port{},
		portsByNetwork: map[string][]weft.Port{},
		networks:       map[string]weft.Network{},
		sgs:            map[string]weft.SecurityGroup{},
		projNets:       map[string][]string{},
	}}
}

func TestImpactedVMs_SGRulesUpdated_VisitsExplicitAndInheriting(t *testing.T) {
	scope := newScope()
	scope.sgs["sg-1"] = weft.SecurityGroup{UUID: "sg-1", ProjectUUID: "proj-1"}
	scope.networks["net-1"] = weft.Network{UUID: "net-1", ProjectUUID: "proj-1",
		DefaultSecurityGroups: []string{"sg-1"}}
	scope.allPorts = []weft.Port{
		// Explicit reference.
		{UUID: "p1", VMUUID: "vm-a", NetworkUUID: "net-1", IP: "10.0.0.5",
			SecurityGroups: []string{"sg-1"}},
		// Inherits via network defaults.
		{UUID: "p2", VMUUID: "vm-b", NetworkUUID: "net-1", IP: "10.0.0.6"},
		// Same VM as p1, second port — must not double-count.
		{UUID: "p3", VMUUID: "vm-a", NetworkUUID: "net-1", IP: "10.0.0.7",
			SecurityGroups: []string{"sg-1"}},
		// Unrelated.
		{UUID: "p4", VMUUID: "vm-c", NetworkUUID: "net-1", IP: "10.0.0.8",
			SecurityGroups: []string{"sg-other"}},
	}
	scope.sgs["sg-other"] = weft.SecurityGroup{UUID: "sg-other", ProjectUUID: "proj-1"}

	p := New(scope, func(string, pod.Firewall) error { return nil }, silentLog())
	got := p.ImpactedVMs(weft.PlatformEvent{Kind: "security_group.rules_updated", Subject: "sg-1"})
	sort.Strings(got)
	want := []string{"vm-a", "vm-b"}
	if !equalStrings(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestImpactedVMs_PortEvents_UseMetaVMUUID(t *testing.T) {
	p := New(newScope(), nil, silentLog())
	for _, kind := range []string{"port.created", "port.security_groups_updated", "port.deleted"} {
		got := p.ImpactedVMs(weft.PlatformEvent{Kind: kind, Subject: "port-1",
			Meta: map[string]string{"vm_uuid": "vm-x"}})
		if len(got) != 1 || got[0] != "vm-x" {
			t.Errorf("kind %s: got %v, want [vm-x]", kind, got)
		}
	}
	// Missing vm_uuid → nil.
	got := p.ImpactedVMs(weft.PlatformEvent{Kind: "port.created", Subject: "port-1"})
	if got != nil {
		t.Errorf("missing vm_uuid: got %v, want nil", got)
	}
}

func TestImpactedVMs_NetworkDefaults_OnlyInheritingPorts(t *testing.T) {
	scope := newScope()
	scope.portsByNetwork["net-1"] = []weft.Port{
		{UUID: "p1", VMUUID: "vm-a", NetworkUUID: "net-1"},                                            // inherits
		{UUID: "p2", VMUUID: "vm-b", NetworkUUID: "net-1", SecurityGroups: []string{"sg-override"}},   // not affected
		{UUID: "p3", VMUUID: "vm-c", NetworkUUID: "net-1"},                                            // inherits
		{UUID: "p4", VMUUID: "vm-a", NetworkUUID: "net-1"},                                            // dup VM, dedup
	}
	p := New(scope, nil, silentLog())
	got := p.ImpactedVMs(weft.PlatformEvent{
		Kind: "network.default_security_groups_updated", Subject: "net-1",
	})
	sort.Strings(got)
	want := []string{"vm-a", "vm-c"}
	if !equalStrings(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestImpactedVMs_VMCreated_UsesSubject(t *testing.T) {
	p := New(newScope(), nil, silentLog())
	got := p.ImpactedVMs(weft.PlatformEvent{Kind: "vm.created", Subject: "vm-new"})
	if len(got) != 1 || got[0] != "vm-new" {
		t.Errorf("got %v", got)
	}
}

func TestImpactedVMs_IgnoredKinds(t *testing.T) {
	p := New(newScope(), nil, silentLog())
	for _, kind := range []string{
		"port.wireguard_key_rotated",
		"vm.state_changed",
		"network.peers_changed",
		"project.created",
		"host.registered",
		"",
	} {
		if got := p.ImpactedVMs(weft.PlatformEvent{Kind: kind, Subject: "x"}); got != nil {
			t.Errorf("kind %q should be ignored, got %v", kind, got)
		}
	}
}

func TestRun_DispatchesPublishesUntilCancel(t *testing.T) {
	scope := newScope()
	scope.portsByVM["vm-a"] = []weft.Port{
		{UUID: "p1", VMUUID: "vm-a", NetworkUUID: "net-1", IP: "10.0.0.5",
			SecurityGroups: []string{"sg-1"}},
	}
	scope.sgs["sg-1"] = weft.SecurityGroup{Rules: []weft.SecurityRule{
		{Direction: weft.SGDirectionIngress, Protocol: weft.SGProtocolTCP, PortMin: 22, PortMax: 22},
	}}
	rec := &recorderPub{}
	p := New(scope, rec.fn(), silentLog())

	events := make(chan weft.PlatformEvent, 2)
	events <- weft.PlatformEvent{Kind: "port.created", Subject: "p1",
		Meta: map[string]string{"vm_uuid": "vm-a"}}
	events <- weft.PlatformEvent{Kind: "vm.state_changed", Subject: "vm-a"} // ignored
	close(events)

	if err := p.Run(context.Background(), events); err != nil {
		t.Fatalf("Run: %v", err)
	}
	entries := rec.snapshot()
	if len(entries) != 1 {
		t.Fatalf("got %d publishes, want 1", len(entries))
	}
	if entries[0].vmUUID != "vm-a" {
		t.Errorf("vmUUID = %s", entries[0].vmUUID)
	}
	if len(entries[0].fw.Rules) != 1 {
		t.Errorf("rules = %d, want 1", len(entries[0].fw.Rules))
	}
}

func TestResyncAll(t *testing.T) {
	scope := newScope()
	scope.portsByVM["vm-a"] = []weft.Port{
		{UUID: "p1", VMUUID: "vm-a", NetworkUUID: "net-1", IP: "10.0.0.5",
			SecurityGroups: []string{"sg-1"}},
	}
	scope.portsByVM["vm-b"] = []weft.Port{} // no ports → empty fw
	scope.sgs["sg-1"] = weft.SecurityGroup{}
	rec := &recorderPub{}
	p := New(scope, rec.fn(), silentLog())

	p.ResyncAll([]string{"vm-a", "vm-b"})

	entries := rec.snapshot()
	if len(entries) != 2 {
		t.Fatalf("got %d publishes, want 2", len(entries))
	}
}

func TestJSONPublishFunc_EncodesAndDispatches(t *testing.T) {
	got := struct {
		subject string
		data    []byte
	}{}
	conn := connFunc(func(subject string, data []byte) error {
		got.subject = subject
		got.data = data
		return nil
	})
	pf := JSONPublishFunc(conn)
	fw := pod.Firewall{Rules: []pod.FirewallRule{
		{Direction: "ingress", Protocol: "tcp", PortMin: 22, PortMax: 22},
	}}
	if err := pf("vm-42", fw); err != nil {
		t.Fatalf("pf: %v", err)
	}
	if got.subject != "weft.firewall.vm-42" {
		t.Errorf("subject = %q", got.subject)
	}
	var decoded pod.Firewall
	if err := json.Unmarshal(got.data, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded.Rules) != 1 || decoded.Rules[0].PortMin != 22 {
		t.Errorf("decoded payload wrong: %+v", decoded)
	}
}

// connFunc is a tiny Conn adapter for tests.
type connFunc func(subject string, data []byte) error

func (f connFunc) Publish(subject string, data []byte) error { return f(subject, data) }

func silentLog() *log.Logger { return log.New(os.NewFile(0, os.DevNull), "", 0) }

// Compile-time check : the production Adapter must satisfy Scope so
// wiring stays one-line in cmd/weft. A regression here flags the
// publisher contract drifting away from the adapter.
var _ Scope = (*weft.Adapter)(nil)

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
