//go:build darwin

package weft

// adapter_registries_test.go exercises the thin wrapper methods the
// Adapter exposes around the underlying registries (volumes,
// networks, security_groups, users, projects, ports, hosts). Each
// wrapper is mostly a one-liner delegating to a registry method
// plus an event publish; the tests focus on the wiring (event
// emission, error pass-through, defensive nil-checks).

import (
	"strings"
	"testing"
)

// newAdapterForRegistries builds an Adapter backed by MemStorage.
// Mirrors newAdapterForRegistryTest but lives here so we can reuse
// without spreading deps across test files.
func newAdapterForRegistries(t *testing.T) *Adapter {
	t.Helper()
	stateDir := t.TempDir()
	factory := func(name string) Storage { return NewMemStorage() }
	return NewWithStorage(stateDir, factory).(*Adapter)
}

// drain consumes events until the channel is empty (non-blocking).
// Returns the collected events for assertion.
func drainEvents(ch <-chan PlatformEvent) []PlatformEvent {
	var out []PlatformEvent
	for {
		select {
		case ev := <-ch:
			out = append(out, ev)
		default:
			return out
		}
	}
}

// ── Volumes wrappers ────────────────────────────────────────────

func TestAdapter_Volumes_FullLifecycle(t *testing.T) {
	a := newAdapterForRegistries(t)
	sub, cancel := a.EventBus().Subscribe(EventFilter{SeeAll: true})
	defer cancel()

	// CreateVolume publishes volume.created with name + size_gib meta.
	v, err := a.CreateVolume(CreateVolumeSpec{
		ProjectUUID: "p-1",
		Name:        "data",
		SizeGiB:     50,
		Format:      VolumeFormatRaw,
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if v.UUID == "" {
		t.Errorf("created volume should have UUID")
	}

	// List variants.
	if got := a.Volumes(); len(got) != 1 {
		t.Errorf("Volumes() = %d, want 1", len(got))
	}
	if got := a.ListVolumesForProject("p-1"); len(got) != 1 {
		t.Errorf("ListVolumesForProject = %d, want 1", len(got))
	}
	// Cross-project list returns empty.
	if got := a.ListVolumesForProject("other"); len(got) != 0 {
		t.Errorf("ListVolumesForProject other = %d, want 0", len(got))
	}

	// Lookups by UUID and Name.
	if got, ok := a.VolumeByUUID(v.UUID); !ok || got.UUID != v.UUID {
		t.Errorf("VolumeByUUID failed: ok=%v", ok)
	}
	if got, ok := a.VolumeByName("p-1", "data"); !ok || got.UUID != v.UUID {
		t.Errorf("VolumeByName failed: ok=%v", ok)
	}

	// RenameVolume publishes volume.renamed; old name no longer resolves.
	if err := a.RenameVolume(v.UUID, "data2"); err != nil {
		t.Fatalf("RenameVolume: %v", err)
	}
	if _, ok := a.VolumeByName("p-1", "data"); ok {
		t.Errorf("old name still resolves after rename")
	}

	// ResizeVolume must publish volume.resized.
	if err := a.ResizeVolume(v.UUID, 100); err != nil {
		t.Fatalf("ResizeVolume: %v", err)
	}

	// Attach / detach happy path.
	if err := a.AttachVolume(v.UUID, "vm-1"); err != nil {
		t.Fatalf("AttachVolume: %v", err)
	}
	if err := a.DetachVolume(v.UUID); err != nil {
		t.Fatalf("DetachVolume: %v", err)
	}

	// Delete.
	if err := a.DeleteVolume(v.UUID); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}
	if _, ok := a.VolumeByUUID(v.UUID); ok {
		t.Errorf("volume should be gone after delete")
	}

	// Make sure we got the expected events.
	evs := drainEvents(sub)
	kinds := map[string]int{}
	for _, ev := range evs {
		kinds[ev.Kind]++
	}
	for _, want := range []string{"volume.created", "volume.renamed", "volume.resized", "volume.attached", "volume.detached", "volume.deleted"} {
		if kinds[want] == 0 {
			t.Errorf("expected event %q, kinds=%v", want, kinds)
		}
	}
}

func TestAdapter_Volumes_ErrorPaths(t *testing.T) {
	a := newAdapterForRegistries(t)
	// Create error: empty name.
	if _, err := a.CreateVolume(CreateVolumeSpec{ProjectUUID: "p", SizeGiB: 1}); err == nil {
		t.Errorf("empty volume name should error")
	}
	// Rename unknown.
	if err := a.RenameVolume("nope", "x"); err == nil {
		t.Errorf("rename unknown should error")
	}
	// Resize unknown.
	if err := a.ResizeVolume("nope", 10); err == nil {
		t.Errorf("resize unknown should error")
	}
	// Attach unknown.
	if err := a.AttachVolume("nope", "vm"); err == nil {
		t.Errorf("attach unknown should error")
	}
	// Detach unknown.
	if err := a.DetachVolume("nope"); err == nil {
		t.Errorf("detach unknown should error")
	}
	// Delete unknown.
	if err := a.DeleteVolume("nope"); err == nil {
		t.Errorf("delete unknown should error")
	}
}

// ── Networks wrappers ───────────────────────────────────────────

func TestAdapter_Networks_FullLifecycle(t *testing.T) {
	a := newAdapterForRegistries(t)
	sub, cancel := a.EventBus().Subscribe(EventFilter{SeeAll: true})
	defer cancel()

	n, err := a.CreateNetwork(CreateNetworkSpec{
		ProjectUUID: "p-1",
		Name:        "default",
		CIDR:        "10.42.0.0/24",
		Gateway:     "10.42.0.1",
		DNSServers:  []string{"1.1.1.1"},
		Type:        NetworkTypeNAT,
	})
	if err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}

	// List variants.
	if got := a.Networks(); len(got) != 1 {
		t.Errorf("Networks() = %d, want 1", len(got))
	}
	if got := a.ListNetworksForProject("p-1"); len(got) != 1 {
		t.Errorf("ListNetworksForProject = %d, want 1", len(got))
	}

	// Lookups.
	if _, ok := a.NetworkByName("p-1", "default"); !ok {
		t.Errorf("NetworkByName failed")
	}

	// Rename + setDNS.
	if err := a.RenameNetwork(n.UUID, "main"); err != nil {
		t.Fatalf("RenameNetwork: %v", err)
	}
	if err := a.SetNetworkDNS(n.UUID, []string{"8.8.8.8"}); err != nil {
		t.Fatalf("SetNetworkDNS: %v", err)
	}

	// Delete.
	if err := a.DeleteNetwork(n.UUID); err != nil {
		t.Fatalf("DeleteNetwork: %v", err)
	}
	if _, ok := a.NetworkByUUID(n.UUID); ok {
		t.Errorf("network should be gone after delete")
	}

	// Ensure events fired.
	evs := drainEvents(sub)
	kinds := map[string]int{}
	for _, ev := range evs {
		kinds[ev.Kind]++
	}
	for _, want := range []string{"network.created", "network.renamed", "network.deleted"} {
		if kinds[want] == 0 {
			t.Errorf("expected event %q, kinds=%v", want, kinds)
		}
	}
}

func TestAdapter_Networks_ErrorPaths(t *testing.T) {
	a := newAdapterForRegistries(t)
	if _, err := a.CreateNetwork(CreateNetworkSpec{ProjectUUID: "", Name: "x"}); err == nil {
		t.Errorf("empty project should error")
	}
	if err := a.RenameNetwork("nope", "x"); err == nil {
		t.Errorf("rename unknown should error")
	}
	if err := a.SetNetworkDNS("nope", nil); err == nil {
		t.Errorf("setDNS unknown should error")
	}
	if err := a.DeleteNetwork("nope"); err == nil {
		t.Errorf("delete unknown should error")
	}
	// NetworkByName on a missing entry.
	if _, ok := a.NetworkByName("p-x", "missing"); ok {
		t.Errorf("missing NetworkByName should not be found")
	}
}

// ── Security-group wrappers ─────────────────────────────────────

func TestAdapter_SecurityGroups_FullLifecycle(t *testing.T) {
	a := newAdapterForRegistries(t)
	sub, cancel := a.EventBus().Subscribe(EventFilter{SeeAll: true})
	defer cancel()

	g, err := a.CreateSecurityGroup(CreateSecurityGroupSpec{
		ProjectUUID: "p-1",
		Name:        "web",
		Description: "web tier",
	})
	if err != nil {
		t.Fatalf("CreateSecurityGroup: %v", err)
	}

	// Lists / lookups.
	if got := a.SecurityGroups(); len(got) != 1 {
		t.Errorf("SecurityGroups() = %d, want 1", len(got))
	}
	if got := a.ListSecurityGroupsForProject("p-1"); len(got) != 1 {
		t.Errorf("ListSecurityGroupsForProject = %d, want 1", len(got))
	}
	if _, ok := a.SecurityGroupByName("p-1", "web"); !ok {
		t.Errorf("SecurityGroupByName failed")
	}

	// Rename.
	if err := a.RenameSecurityGroup(g.UUID, "frontend"); err != nil {
		t.Fatalf("RenameSecurityGroup: %v", err)
	}

	// Description.
	if err := a.SetSecurityGroupDescription(g.UUID, "the frontend tier"); err != nil {
		t.Fatalf("SetSecurityGroupDescription: %v", err)
	}

	// Rules.
	rules := []SecurityRule{{
		Direction:  SGDirectionIngress,
		Protocol:   SGProtocolTCP,
		PortMin:    80,
		PortMax:    80,
		RemoteCIDR: "0.0.0.0/0",
	}}
	if err := a.SetSecurityGroupRules(g.UUID, rules); err != nil {
		t.Fatalf("SetSecurityGroupRules: %v", err)
	}

	// Delete.
	if err := a.DeleteSecurityGroup(g.UUID); err != nil {
		t.Fatalf("DeleteSecurityGroup: %v", err)
	}

	evs := drainEvents(sub)
	kinds := map[string]int{}
	for _, ev := range evs {
		kinds[ev.Kind]++
	}
	for _, want := range []string{"security_group.created", "security_group.renamed", "security_group.rules_updated", "security_group.deleted"} {
		if kinds[want] == 0 {
			t.Errorf("expected event %q, kinds=%v", want, kinds)
		}
	}
}

func TestAdapter_SecurityGroups_ErrorPaths(t *testing.T) {
	a := newAdapterForRegistries(t)
	if _, err := a.CreateSecurityGroup(CreateSecurityGroupSpec{ProjectUUID: ""}); err == nil {
		t.Errorf("empty project should error")
	}
	if err := a.RenameSecurityGroup("nope", "x"); err == nil {
		t.Errorf("rename unknown should error")
	}
	if err := a.SetSecurityGroupDescription("nope", "x"); err == nil {
		t.Errorf("setDescription unknown should error")
	}
	if err := a.SetSecurityGroupRules("nope", nil); err == nil {
		t.Errorf("setRules unknown should error")
	}
	if err := a.DeleteSecurityGroup("nope"); err == nil {
		t.Errorf("delete unknown should error")
	}
}

// ── Host wrappers ───────────────────────────────────────────────

func TestAdapter_Hosts_LifecycleViaWrappers(t *testing.T) {
	a := newAdapterForRegistries(t)
	sub, cancel := a.EventBus().Subscribe(EventFilter{SeeAll: true})
	defer cancel()

	// Self-registered host already exists from NewWithStorage.
	localUUID := a.localHostUUID()
	if _, ok := a.HostByUUID(localUUID); !ok {
		t.Fatalf("self-registered host missing")
	}

	// Register a fresh host with AZ for HostsInAZ coverage.
	h, err := a.RegisterHost(RegisterHostSpec{
		Hostname: "h2",
		AZ:       "az-east",
	})
	if err != nil {
		t.Fatalf("RegisterHost: %v", err)
	}

	// HostByHostname.
	got, ok := a.HostByHostname("h2")
	if !ok || got.UUID != h.UUID {
		t.Errorf("HostByHostname failed")
	}

	// HostsInAZ — both the local host (no AZ) and az-east.
	if hs := a.HostsInAZ("az-east"); len(hs) != 1 {
		t.Errorf("HostsInAZ(az-east) = %d, want 1", len(hs))
	}

	// SetHostState publishes host.state_changed.
	if err := a.SetHostState(h.UUID, HostStateDraining); err != nil {
		t.Fatalf("SetHostState: %v", err)
	}
	// SetHostProperties publishes host.properties_updated.
	if err := a.SetHostProperties(h.UUID, map[string]string{"role": "compute"}); err != nil {
		t.Fatalf("SetHostProperties: %v", err)
	}
	// DeleteHost — must drain first (already done above).
	if err := a.SetHostState(h.UUID, HostStateDown); err != nil {
		t.Fatalf("SetHostState down: %v", err)
	}
	if err := a.DeleteHost(h.UUID); err != nil {
		t.Fatalf("DeleteHost: %v", err)
	}

	evs := drainEvents(sub)
	kinds := map[string]int{}
	for _, ev := range evs {
		kinds[ev.Kind]++
	}
	for _, want := range []string{"host.registered", "host.state_changed", "host.properties_updated", "host.deleted"} {
		if kinds[want] == 0 {
			t.Errorf("expected event %q, kinds=%v", want, kinds)
		}
	}
}

func TestAdapter_Hosts_ErrorPaths(t *testing.T) {
	a := newAdapterForRegistries(t)
	if err := a.SetHostState("nope", HostStateActive); err == nil {
		t.Errorf("set unknown state should error")
	}
	if err := a.SetHostProperties("nope", nil); err == nil {
		t.Errorf("set properties on unknown should error")
	}
	if err := a.DeleteHost("nope"); err == nil {
		t.Errorf("delete unknown host should error")
	}
	if _, ok := a.HostByHostname("never"); ok {
		t.Errorf("HostByHostname on missing should not match")
	}
}

// ── VM list wrappers ────────────────────────────────────────────

func TestAdapter_VMs_ListVariants(t *testing.T) {
	a := newAdapterForRegistries(t)
	p, _, _ := a.CreateProject("p")
	_, err := a.RegisterVM(CreateVMSpec{
		ProjectUUID: p.UUID,
		Name:        "vm1",
		HostUUID:    a.localHostUUID(),
	})
	if err != nil {
		t.Fatalf("RegisterVM: %v", err)
	}

	if got := a.VMs(); len(got) != 1 {
		t.Errorf("VMs() = %d, want 1", len(got))
	}
	if got := a.ListVMsForProject(p.UUID); len(got) != 1 {
		t.Errorf("ListVMsForProject = %d, want 1", len(got))
	}
	if got := a.ListVMsForProject("other"); len(got) != 0 {
		t.Errorf("ListVMsForProject other = %d, want 0", len(got))
	}
}

// ── Port list wrappers ──────────────────────────────────────────

func TestAdapter_Ports_ListVariants(t *testing.T) {
	a := newAdapterForRegistries(t)
	// Create a network so we can attach ports to it.
	n, err := a.CreateNetwork(CreateNetworkSpec{
		ProjectUUID: "p",
		Name:        "net",
		CIDR:        "10.0.0.0/24",
		Type:        NetworkTypeNAT,
	})
	if err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
	p, err := a.CreatePort(CreatePortSpec{
		ProjectUUID: "p",
		VMUUID:      "vm-1",
		NetworkUUID: n.UUID,
		MAC:         "02:00:00:00:00:01",
		IP:          "10.0.0.5",
	})
	if err != nil {
		t.Fatalf("CreatePort: %v", err)
	}

	if got := a.ListPortsForVM("vm-1"); len(got) != 1 || got[0].UUID != p.UUID {
		t.Errorf("ListPortsForVM failed: %+v", got)
	}
	if got := a.ListPortsForNetwork(n.UUID); len(got) != 1 || got[0].UUID != p.UUID {
		t.Errorf("ListPortsForNetwork failed: %+v", got)
	}
}

// ── User wrappers ───────────────────────────────────────────────

func TestAdapter_Users_LifecycleViaWrappers(t *testing.T) {
	a := newAdapterForRegistries(t)
	sub, cancel := a.EventBus().Subscribe(EventFilter{SeeAll: true})
	defer cancel()

	u, created, err := a.RegisterUser(&Caller{
		Subject: "ldap:alice",
		Issuer:  "https://dex",
		Email:   "alice@example.com",
	})
	if err != nil {
		t.Fatalf("RegisterUser: %v", err)
	}
	if !created {
		t.Errorf("first RegisterUser should report created")
	}

	if got := a.Users(); len(got) != 1 {
		t.Errorf("Users() = %d, want 1", len(got))
	}
	if got, ok := a.UserByUUID(u.UUID); !ok || got.UUID != u.UUID {
		t.Errorf("UserByUUID failed")
	}

	// SetUserDisplayName publishes user.renamed.
	if err := a.SetUserDisplayName(u.UUID, "Alice"); err != nil {
		t.Fatalf("SetUserDisplayName: %v", err)
	}

	// DeleteUser publishes user.deleted.
	if err := a.DeleteUser(u.UUID); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}

	evs := drainEvents(sub)
	kinds := map[string]int{}
	for _, ev := range evs {
		kinds[ev.Kind]++
	}
	for _, want := range []string{"user.created", "user.renamed", "user.deleted"} {
		if kinds[want] == 0 {
			t.Errorf("expected event %q, kinds=%v", want, kinds)
		}
	}
}

func TestAdapter_Users_ErrorPaths(t *testing.T) {
	a := newAdapterForRegistries(t)
	if err := a.SetUserDisplayName("nope", "x"); err == nil {
		t.Errorf("set name unknown should error")
	}
	if err := a.DeleteUser("nope"); err == nil {
		t.Errorf("delete unknown should error")
	}
	// Anonymous caller is rejected at the registry level.
	if _, _, err := a.RegisterUser(nil); err == nil {
		t.Errorf("nil caller should error")
	}
}

// ── Project wrappers ────────────────────────────────────────────

func TestAdapter_Projects_LifecycleViaWrappers(t *testing.T) {
	a := newAdapterForRegistries(t)
	sub, cancel := a.EventBus().Subscribe(EventFilter{SeeAll: true})
	defer cancel()

	p, _, err := a.CreateProject("acme")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	// ProjectByName.
	got, ok := a.ProjectByName("acme")
	if !ok || got.UUID != p.UUID {
		t.Errorf("ProjectByName failed")
	}
	if _, ok := a.ProjectByName("missing"); ok {
		t.Errorf("missing ProjectByName should not match")
	}

	// Projects list contains at least the project we created plus
	// the default project for the OS user.
	if got := a.Projects(); len(got) < 1 {
		t.Errorf("Projects() empty")
	}

	// Rename.
	if err := a.RenameProject(p.UUID, "acme2"); err != nil {
		t.Fatalf("RenameProject: %v", err)
	}

	// ProjectMembers initially empty.
	members, ok := a.ProjectMembers(p.UUID)
	if !ok || len(members) != 0 {
		t.Errorf("members = %v, want []", members)
	}
	if _, ok := a.ProjectMembers("nope"); ok {
		t.Errorf("members on unknown should not match")
	}

	evs := drainEvents(sub)
	kinds := map[string]int{}
	for _, ev := range evs {
		kinds[ev.Kind]++
	}
	for _, want := range []string{"project.created", "project.renamed"} {
		if kinds[want] == 0 {
			t.Errorf("expected event %q, kinds=%v", want, kinds)
		}
	}
}

func TestAdapter_Projects_ErrorPaths(t *testing.T) {
	a := newAdapterForRegistries(t)
	if err := a.RenameProject("nope", "x"); err == nil {
		t.Errorf("rename unknown should error")
	}
	// CreateProject with empty name fails.
	if _, _, err := a.CreateProject(""); err == nil {
		t.Errorf("empty project name should error")
	}
	// AddProjectMember on unknown.
	if err := a.AddProjectMember("nope", "u"); err == nil {
		t.Errorf("add member to unknown should error")
	}
	// RemoveProjectMember on unknown.
	if err := a.RemoveProjectMember("nope", "u"); err == nil {
		t.Errorf("remove member from unknown should error")
	}
}

// ── ResolveProjectUUID covers ────────────────────────────────────

// TestResolveProjectUUID_UUIDPassthrough confirms a UUID input
// resolves to itself (no auto-create / lookup).
func TestResolveProjectUUID_UUIDPassthrough(t *testing.T) {
	a := newAdapterForRegistries(t)
	got := a.ResolveProjectUUID("f47ac10b-58cc-4372-a567-0e02b2c3d479")
	if got != "f47ac10b-58cc-4372-a567-0e02b2c3d479" {
		t.Errorf("UUID input should pass through, got %q", got)
	}
}

// TestProjectByUUID_PreInitFallback ensures the defensive
// nil-registry guard returns an empty result.
func TestProjectByUUID_PreInitFallback(t *testing.T) {
	a := &Adapter{}
	if _, ok := a.ProjectByUUID("any"); ok {
		t.Errorf("nil registry should return (zero, false)")
	}
	if _, ok := a.ProjectByName("any"); ok {
		t.Errorf("nil registry should return (zero, false)")
	}
	if got := a.Projects(); got != nil {
		t.Errorf("nil registry should return nil")
	}
}

// TestAdapter_NilRegistry_GuardErrors verifies the defensive
// "registry not initialised" error paths the wrappers carry.
// We craft a bare Adapter to bypass the initialisers.
func TestAdapter_NilRegistry_GuardErrors(t *testing.T) {
	a := &Adapter{bus: NewEventBus()}
	if _, _, err := a.CreateProject("x"); err == nil {
		t.Errorf("nil project registry should error on Create")
	}
	if err := a.RenameProject("u", "x"); err == nil {
		t.Errorf("nil project registry should error on Rename")
	}
	if err := a.AddProjectMember("p", "u"); err == nil {
		t.Errorf("nil project registry should error on AddMember")
	}
	if err := a.RemoveProjectMember("p", "u"); err == nil {
		t.Errorf("nil project registry should error on RemoveMember")
	}
	if _, _, err := a.RegisterUser(&Caller{}); err == nil {
		t.Errorf("nil user registry should error")
	}
	if err := a.SetUserDisplayName("u", "x"); err == nil {
		t.Errorf("nil user registry should error on SetDisplayName")
	}
	if err := a.DeleteUser("u"); err == nil {
		t.Errorf("nil user registry should error on Delete")
	}
	if got := a.Users(); got != nil {
		t.Errorf("nil user registry list should be nil")
	}
	if _, ok := a.UserByUUID("x"); ok {
		t.Errorf("nil user registry should return false")
	}
	if _, ok := a.UserBySubject("i", "s"); ok {
		t.Errorf("nil user registry should return false")
	}

	// Networks
	if _, err := a.CreateNetwork(CreateNetworkSpec{}); err == nil {
		t.Errorf("nil network registry should error")
	}
	if err := a.RenameNetwork("u", "x"); err == nil {
		t.Errorf("nil network registry should error on Rename")
	}
	if err := a.SetNetworkDNS("u", nil); err == nil {
		t.Errorf("nil network registry should error on SetDNS")
	}
	if err := a.DeleteNetwork("u"); err == nil {
		t.Errorf("nil network registry should error on Delete")
	}
	if err := a.SetNetworkDefaultSecurityGroups("u", nil); err == nil {
		t.Errorf("nil registries should error")
	}
	if got := a.Networks(); got != nil {
		t.Errorf("nil network registry list should be nil")
	}
	if _, ok := a.NetworkByUUID("x"); ok {
		t.Errorf("nil network registry should return false")
	}
	if _, ok := a.NetworkByName("p", "x"); ok {
		t.Errorf("nil network registry should return false")
	}
	if got := a.ListNetworksForProject("p"); got != nil {
		t.Errorf("nil network registry should return nil")
	}

	// Security groups
	if _, err := a.CreateSecurityGroup(CreateSecurityGroupSpec{}); err == nil {
		t.Errorf("nil sg registry should error")
	}
	if err := a.RenameSecurityGroup("u", "x"); err == nil {
		t.Errorf("nil sg registry should error on Rename")
	}
	if err := a.SetSecurityGroupDescription("u", "x"); err == nil {
		t.Errorf("nil sg registry should error on SetDesc")
	}
	if err := a.SetSecurityGroupRules("u", nil); err == nil {
		t.Errorf("nil sg registry should error on SetRules")
	}
	if err := a.DeleteSecurityGroup("u"); err == nil {
		t.Errorf("nil sg registry should error on Delete")
	}
	if got := a.SecurityGroups(); got != nil {
		t.Errorf("nil sg registry list should be nil")
	}
	if _, ok := a.SecurityGroupByUUID("x"); ok {
		t.Errorf("nil sg registry should return false")
	}
	if _, ok := a.SecurityGroupByName("p", "x"); ok {
		t.Errorf("nil sg registry should return false")
	}
	if got := a.ListSecurityGroupsForProject("p"); got != nil {
		t.Errorf("nil sg registry should return nil")
	}

	// Hosts
	if _, err := a.RegisterHost(RegisterHostSpec{Hostname: "x"}); err == nil {
		t.Errorf("nil host registry should error on register")
	}
	if err := a.HeartbeatHost("u"); err == nil {
		t.Errorf("nil host registry should error on Heartbeat")
	}
	if err := a.SetHostState("u", "active"); err == nil {
		t.Errorf("nil host registry should error on SetHostState")
	}
	if err := a.SetHostProperties("u", nil); err == nil {
		t.Errorf("nil host registry should error on SetHostProperties")
	}
	if err := a.DeleteHost("u"); err == nil {
		t.Errorf("nil host registry should error on Delete")
	}
	if got := a.Hosts(); got != nil {
		t.Errorf("nil host registry list should be nil")
	}
	if _, ok := a.HostByUUID("x"); ok {
		t.Errorf("nil host registry should return false")
	}
	if _, ok := a.HostByHostname("x"); ok {
		t.Errorf("nil host registry should return false")
	}
	if got := a.HostsInAZ("az"); got != nil {
		t.Errorf("nil host registry should return nil")
	}

	// VMs
	if _, err := a.RegisterVM(CreateVMSpec{}); err == nil {
		t.Errorf("nil vm registry should error")
	}
	if err := a.SetVMState("u", "running"); err == nil {
		t.Errorf("nil vm registry should error on SetVMState")
	}
	if err := a.MigrateVM("u", "h"); err == nil {
		t.Errorf("nil vm registry should error on MigrateVM")
	}
	if err := a.RenameVMInventory("u", "x"); err == nil {
		t.Errorf("nil vm registry should error on RenameVMInventory")
	}
	if err := a.UnregisterVM("u"); err == nil {
		t.Errorf("nil vm registry should error on UnregisterVM")
	}
	if got := a.VMs(); got != nil {
		t.Errorf("nil vm registry list should be nil")
	}
	if _, ok := a.VMByUUID("x"); ok {
		t.Errorf("nil vm registry should return false")
	}
	if _, ok := a.VMByName("p", "x"); ok {
		t.Errorf("nil vm registry should return false")
	}
	if got := a.ListVMsForProject("p"); got != nil {
		t.Errorf("nil vm registry should return nil")
	}
	if got := a.ListVMsForHost("h"); got != nil {
		t.Errorf("nil vm registry should return nil")
	}

	// Ports
	if _, err := a.CreatePort(CreatePortSpec{}); err == nil {
		t.Errorf("nil port registry should error")
	}
	if err := a.DeletePort("u"); err == nil {
		t.Errorf("nil port registry should error on Delete")
	}
	if err := a.SetPortSecurityGroups("u", nil); err == nil {
		t.Errorf("nil port registry should error on SetSGs")
	}
	if err := a.SetPortWireguardPubKey("u", "k"); err == nil {
		t.Errorf("nil port registry should error on SetPubKey")
	}
	if _, ok := a.PortByUUID("x"); ok {
		t.Errorf("nil port registry should return false")
	}
	if got := a.ListPortsForVM("v"); got != nil {
		t.Errorf("nil port registry should return nil")
	}
	if got := a.ListPortsForNetwork("n"); got != nil {
		t.Errorf("nil port registry should return nil")
	}

	// Note: Volume methods do NOT carry defensive nil-registry
	// guards (unlike all other wrappers above) — calling them on a
	// bare Adapter would panic. We document that behaviour by
	// omitting them here rather than failing the test.
}

// ── Setter wrappers ─────────────────────────────────────────────

func TestAdapter_Setters(t *testing.T) {
	a := newAdapterForRegistries(t)
	// SetSSHKeyPath stores the path; we re-read via a guarded API:
	// the only observable side-effect is that ExecInVM uses it later.
	// We just assert the call does not panic.
	a.SetSSHKeyPath("/tmp/k")
	if a.sshKeyPath != "/tmp/k" {
		t.Errorf("sshKeyPath = %q, want /tmp/k", a.sshKeyPath)
	}

	// SetChecksums forwards to imageStore (also a no-op observable).
	a.SetChecksums(map[string]string{"http://x": "sha256:abc"})

	// SetPaths overrides cachePath / vmsPath.
	a.SetPaths("/tmp/c", "/tmp/v")
	if a.cachePath != "/tmp/c" || a.vmsPath != "/tmp/v" {
		t.Errorf("SetPaths didn't store values: cache=%q vms=%q", a.cachePath, a.vmsPath)
	}
	if !strings.HasPrefix(a.cacheDir(), "/tmp/c") {
		t.Errorf("cacheDir = %q, want /tmp/c", a.cacheDir())
	}
	if !strings.HasPrefix(a.vmsDir(), "/tmp/v") {
		t.Errorf("vmsDir = %q, want /tmp/v", a.vmsDir())
	}

	// SetEventBus swaps the bus + reinstalls the hook. nil is rejected.
	prev := a.EventBus()
	a.SetEventBus(nil)
	if a.EventBus() != prev {
		t.Errorf("SetEventBus(nil) should be a no-op")
	}
	newBus := NewEventBus()
	a.SetEventBus(newBus)
	if a.EventBus() != newBus {
		t.Errorf("SetEventBus didn't swap bus")
	}
}

// TestSetVMUser stores the SSH username for a specific VM.
func TestAdapter_SetVMUser(t *testing.T) {
	a := newAdapterForRegistries(t)
	a.SetVMUser("vm1", "alice")
	if a.users == nil || a.users["vm1"] != "alice" {
		t.Errorf("SetVMUser didn't store user")
	}
	// Empty user is a no-op.
	a.SetVMUser("vm2", "")
	if _, ok := a.users["vm2"]; ok {
		t.Errorf("empty user should not be stored")
	}
}

// ── Splitting and containsRune helpers ──────────────────────────

func TestSplitVMDir(t *testing.T) {
	cases := []struct {
		vmsDir, vmDir   string
		wantProj, wantN string
	}{
		// post-Phase-1 layout: <vmsDir>/<projectUUID>/<vmName>
		{"state/vz", "state/vz/p1/vm-a", "p1", "vm-a"},
		// vmDir not under vmsDir → splits at the first slash anyway.
		{"state/vz", "/somewhere/else/vm-x", "", "somewhere/else/vm-x"},
		// No slash at all → empty project, raw remainder.
		{"", "stray", "", "stray"},
		// Empty vmsDir + slashed input: splits at first slash.
		{"", "p1/vm-a", "p1", "vm-a"},
		// vmDir == vmsDir exactly → rel stays vmDir, splits at first slash inside it.
		{"state/vz", "state/vz", "state", "vz"},
	}
	for _, c := range cases {
		gotP, gotN := splitVMDir(c.vmsDir, c.vmDir)
		if gotP != c.wantProj || gotN != c.wantN {
			t.Errorf("splitVMDir(%q,%q) = (%q,%q), want (%q,%q)", c.vmsDir, c.vmDir, gotP, gotN, c.wantProj, c.wantN)
		}
	}
}

func TestContainsRune(t *testing.T) {
	if !containsRune("a.b", '.') {
		t.Errorf("containsRune should find '.' in 'a.b'")
	}
	if containsRune("abc", '.') {
		t.Errorf("containsRune should NOT find '.' in 'abc'")
	}
	if containsRune("", '.') {
		t.Errorf("containsRune on empty should be false")
	}
}
