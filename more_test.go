//go:build darwin

package weft

// more_test.go: additional unit-coverage fillers.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── Registry list cross-project (volumes/networks/sg/vms) ──────

func TestVolumeRegistry_ListAllProjectsSorted(t *testing.T) {
	reg, _ := loadVolumeRegistry(context.Background(), NewMemStorage())
	_, _ = reg.create(CreateVolumeSpec{ProjectUUID: "p-b", Name: "v-b1", SizeGiB: 1})
	_, _ = reg.create(CreateVolumeSpec{ProjectUUID: "p-a", Name: "v-a2", SizeGiB: 1})
	_, _ = reg.create(CreateVolumeSpec{ProjectUUID: "p-a", Name: "v-a1", SizeGiB: 1})
	got := reg.list()
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	// Expect order p-a/v-a1, p-a/v-a2, p-b/v-b1
	if got[0].ProjectUUID != "p-a" || got[0].Name != "v-a1" {
		t.Errorf("[0] = %+v", got[0])
	}
	if got[1].ProjectUUID != "p-a" || got[1].Name != "v-a2" {
		t.Errorf("[1] = %+v", got[1])
	}
	if got[2].ProjectUUID != "p-b" || got[2].Name != "v-b1" {
		t.Errorf("[2] = %+v", got[2])
	}
}

func TestNetworkRegistry_ListAllProjectsSorted(t *testing.T) {
	reg, _ := loadNetworkRegistry(context.Background(), NewMemStorage())
	_, _ = reg.create(CreateNetworkSpec{ProjectUUID: "p-b", Name: "n-b1", CIDR: "10.0.0.0/24"})
	_, _ = reg.create(CreateNetworkSpec{ProjectUUID: "p-a", Name: "n-a2", CIDR: "10.0.0.0/24"})
	_, _ = reg.create(CreateNetworkSpec{ProjectUUID: "p-a", Name: "n-a1", CIDR: "10.0.0.0/24"})
	got := reg.list()
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if got[0].Name != "n-a1" || got[1].Name != "n-a2" || got[2].Name != "n-b1" {
		t.Errorf("order wrong: %v", got)
	}
}

func TestSecurityGroupRegistry_ListAllProjectsSorted(t *testing.T) {
	reg, _ := loadSecurityGroupRegistry(context.Background(), NewMemStorage())
	_, _ = reg.create(CreateSecurityGroupSpec{ProjectUUID: "p-b", Name: "sg-b1"})
	_, _ = reg.create(CreateSecurityGroupSpec{ProjectUUID: "p-a", Name: "sg-a2"})
	_, _ = reg.create(CreateSecurityGroupSpec{ProjectUUID: "p-a", Name: "sg-a1"})
	got := reg.list()
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if got[0].Name != "sg-a1" || got[1].Name != "sg-a2" || got[2].Name != "sg-b1" {
		t.Errorf("order wrong: %v", got)
	}
}

func TestVMRegistry_ListAllProjectsSorted(t *testing.T) {
	reg, _ := loadVMRegistry(context.Background(), NewMemStorage())
	_, _ = reg.create(CreateVMSpec{ProjectUUID: "p-b", Name: "vm-b1", HostUUID: "h"})
	_, _ = reg.create(CreateVMSpec{ProjectUUID: "p-a", Name: "vm-a2", HostUUID: "h"})
	_, _ = reg.create(CreateVMSpec{ProjectUUID: "p-a", Name: "vm-a1", HostUUID: "h"})
	got := reg.list()
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if got[0].Name != "vm-a1" || got[1].Name != "vm-a2" || got[2].Name != "vm-b1" {
		t.Errorf("order wrong: %v", got)
	}
}

func TestPortRegistry_ListSorted(t *testing.T) {
	reg, _ := loadPortRegistry(context.Background(), NewMemStorage())
	// Two ports on different networks/projects.
	_, _ = reg.create(CreatePortSpec{
		ProjectUUID: "p-b", VMUUID: "vm-b", NetworkUUID: "n-b",
		IP: "10.0.0.5", MAC: "02:00:00:00:00:01",
	})
	_, _ = reg.create(CreatePortSpec{
		ProjectUUID: "p-a", VMUUID: "vm-a", NetworkUUID: "n-a",
		IP: "10.0.0.3", MAC: "02:00:00:00:00:02",
	})
	_, _ = reg.create(CreatePortSpec{
		ProjectUUID: "p-a", VMUUID: "vm-a", NetworkUUID: "n-a",
		IP: "10.0.0.2", MAC: "02:00:00:00:00:03",
	})
	got := reg.list()
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	// p-a/n-a/10.0.0.2 < p-a/n-a/10.0.0.3 < p-b/n-b/10.0.0.5
	if got[0].IP != "10.0.0.2" || got[1].IP != "10.0.0.3" || got[2].IP != "10.0.0.5" {
		t.Errorf("order wrong: %v", got)
	}
}

// ── User list with stable secondary key (UUID) ──────────────────

func TestUserRegistry_ListStableSecondaryByUUID(t *testing.T) {
	reg, _ := loadUserRegistry(context.Background(), NewMemStorage())
	// Two users with the same display name (defaults to email) →
	// sorted by UUID as a tiebreaker.
	u1, _, _ := reg.getOrCreateFromCaller(&Caller{Subject: "s1", Issuer: "i", Email: "x@x"})
	u2, _, _ := reg.getOrCreateFromCaller(&Caller{Subject: "s2", Issuer: "i", Email: "x@x"})
	got := reg.list()
	if len(got) != 2 {
		t.Fatalf("got %d", len(got))
	}
	// Compare via UUID order.
	if u1.UUID < u2.UUID {
		if got[0].UUID != u1.UUID || got[1].UUID != u2.UUID {
			t.Errorf("expected u1 first by UUID")
		}
	} else {
		if got[0].UUID != u2.UUID || got[1].UUID != u1.UUID {
			t.Errorf("expected u2 first by UUID")
		}
	}
}

// ── ResolveProjectUUID nil-registry + empty input ─────────────

func TestResolveProjectUUID_NilRegistry(t *testing.T) {
	// Pre-init: projects is nil → empty input returns "".
	a := &Adapter{}
	if got := a.ResolveProjectUUID(""); got != "" {
		t.Errorf("nil registry empty input should be empty, got %q", got)
	}
	// Display name with nil registry → returned verbatim.
	if got := a.ResolveProjectUUID("name"); got != "name" {
		t.Errorf("nil registry name input should pass through, got %q", got)
	}
}

func TestDefaultProjectUUID_NilRegistry(t *testing.T) {
	a := &Adapter{}
	if got := a.DefaultProjectUUID(); got != "" {
		t.Errorf("nil registry DefaultProjectUUID should be empty, got %q", got)
	}
}

// ── WriteCloudInitISO ────────────────────────────────────────────

func TestAdapter_WriteCloudInitISO(t *testing.T) {
	a := newAdapterForRegistries(t)
	p, _, _ := a.CreateProject("p")
	// vmDir is computed but the VM doesn't need to exist for the
	// test — WriteCloudInitISO just writes a file.
	dir := a.vmDirIn(p.UUID, "vm")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path, err := a.WriteCloudInitISO("vm", []byte("iso-data"))
	if err != nil {
		t.Fatalf("WriteCloudInitISO: %v", err)
	}
	if !strings.HasSuffix(path, "cloud-init.iso") {
		t.Errorf("path = %q", path)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	if string(got) != "iso-data" {
		t.Errorf("got %q", got)
	}
}

func TestAdapter_WriteCloudInitISO_BadDir(t *testing.T) {
	a := &Adapter{stateDir: "/var/empty/no-write", vmsPath: "/var/empty/no-write"}
	if _, err := a.WriteCloudInitISO("vm", []byte("x")); err == nil {
		t.Errorf("write to unwritable should error")
	}
}

// ── GetOSFromCache: all branches ────────────────────────────────

func TestAdapter_GetOSFromCache_LinuxFamilies(t *testing.T) {
	a := newAdapterForRegistries(t)
	for _, img := range []string{
		"ghcr.io/foo/ubuntu:24.04",
		"docker.io/library/debian:12",
		"ghcr.io/rocky/rockylinux:9",
		"docker.io/library/alpine:3.18",
		"docker.io/library/centos:7",
		"ghcr.io/foo/linux-builder:latest",
	} {
		if got := a.GetOSFromCache(img); got != "linux" {
			t.Errorf("GetOSFromCache(%q) = %q, want linux", img, got)
		}
	}
	for _, img := range []string{
		"ghcr.io/macos:14",
		"darwin:base",
	} {
		if got := a.GetOSFromCache(img); got != "darwin" {
			t.Errorf("GetOSFromCache(%q) = %q, want darwin", img, got)
		}
	}
	if got := a.GetOSFromCache("ghcr.io/foo/bar:v1"); got != "" {
		t.Errorf("unknown should be empty, got %q", got)
	}
}

// ── normMAC: edge cases ─────────────────────────────────────────

func TestNormMAC_Variants(t *testing.T) {
	cases := []struct{ in, want string }{
		// Already canonical.
		{"c2:58:e1:0f:e1:10", "c2:58:e1:0f:e1:10"},
		// Single-digit octets (macOS arp shows these).
		{"c2:58:e1:f:e1:10", "c2:58:e1:0f:e1:10"},
		// Dashes accepted.
		{"c2-58-e1-0f-e1-10", "c2:58:e1:0f:e1:10"},
		// Upper-case → lower.
		{"C2:58:E1:0F:E1:10", "c2:58:e1:0f:e1:10"},
		// Wrong number of octets returns lowercased original.
		{"abc", "abc"},
		// Garbage octet returns lowercased original (parse error).
		{"zz:58:e1:0f:e1:10", "zz:58:e1:0f:e1:10"},
	}
	for _, c := range cases {
		if got := normMAC(c.in); got != c.want {
			t.Errorf("normMAC(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ── parseDHCPLeasesData: cover the parse path ──────────────────

func TestParseDHCPLeasesData(t *testing.T) {
	data := []byte(`{
	name=foo
	ip_address=192.168.64.3
	hw_address=1,c2:58:e1:f:e1:10
}
{
	name=bar
	ip_address=192.168.64.5
	hw_address=1,02:00:00:00:00:01
}
`)
	if got := parseDHCPLeasesData(data, "c2:58:e1:0f:e1:10"); got != "192.168.64.3" {
		t.Errorf("got %q", got)
	}
	if got := parseDHCPLeasesData(data, "02:00:00:00:00:01"); got != "192.168.64.5" {
		t.Errorf("got %q", got)
	}
	// Not found.
	if got := parseDHCPLeasesData(data, "ff:ff:ff:ff:ff:ff"); got != "" {
		t.Errorf("not-found case: got %q", got)
	}
	// Empty data.
	if got := parseDHCPLeasesData(nil, "any"); got != "" {
		t.Errorf("empty data: got %q", got)
	}
}

// ── IP error path: VM directory exists but no mac.txt ──────────

func TestAdapter_IP_MissingMAC(t *testing.T) {
	a := newAdapterForRegistries(t)
	if _, err := a.IP("never"); err == nil {
		t.Errorf("missing VM should error")
	}
}

// IP() with a mac.txt present: DHCP-lease lookup misses (the test
// MAC won't be in /var/db/dhcpd_leases) and ARP lookup misses too,
// so IP() returns an error. Exercises the full IP() body +
// ipFromDHCPLeases + ipFromARP miss paths.
func TestAdapter_IP_MACPresentButUnresolvable(t *testing.T) {
	a := newAdapterForRegistries(t)
	p, _, _ := a.CreateProject("p")
	dir := a.vmDirIn(p.UUID, "vm")
	_ = os.MkdirAll(dir, 0o700)
	_ = os.WriteFile(filepath.Join(dir, "mac.txt"), []byte("02:00:de:ad:be:ef"), 0o600)
	// VMDir resolution: a.vmDir(name) searches projects; the dir we
	// created lives under p.UUID so findVMByName resolves it.
	if _, err := a.IP("vm"); err == nil {
		t.Errorf("unresolvable MAC should error")
	}
}

// ipFromDHCPLeases against the real host file: returns "" for a
// MAC that isn't leased (or when the file is absent). Either way it
// must not panic.
func TestIPFromDHCPLeases_NoMatch(t *testing.T) {
	if got := ipFromDHCPLeases("02:00:de:ad:be:ef"); got != "" {
		t.Errorf("unexpected lease match for synthetic MAC: %q", got)
	}
}

// ipFromARP shells out to `arp -an` (present on macOS). A synthetic
// MAC won't be in the table → error. Exercises the parse loop.
func TestIPFromARP_NoMatch(t *testing.T) {
	if _, err := ipFromARP("02:00:de:ad:be:ef"); err == nil {
		t.Errorf("synthetic MAC should not be found in ARP table")
	}
}

// ── selfRegisterHost error: a bare adapter with nil hostReg ───

func TestSelfRegisterHost_NilRegistry(t *testing.T) {
	a := &Adapter{stateDir: t.TempDir()}
	if err := a.selfRegisterHost(); err == nil {
		t.Errorf("nil host registry should error")
	}
}

// ── eventbus SubscriberCount on nil receiver ───────────────────

func TestLocalEventBus_SubscriberCount_Nil(t *testing.T) {
	var b *LocalEventBus
	if got := b.SubscriberCount(); got != 0 {
		t.Errorf("nil bus SubscriberCount = %d, want 0", got)
	}
}

// ── eventbus accepts: edge case subject only ──────────────────

func TestEventFilter_Subject(t *testing.T) {
	f := EventFilter{SeeAll: true, Subject: "vm-a"}
	if !f.accepts(PlatformEvent{Subject: "vm-a"}) {
		t.Error("matching subject should pass")
	}
	if f.accepts(PlatformEvent{Subject: "vm-b"}) {
		t.Error("non-matching subject should drop")
	}
}

// ── Volumes registry: name collision on rename ────────────────

func TestVolumeRegistry_SetName_Idempotent(t *testing.T) {
	reg, _ := loadVolumeRegistry(context.Background(), NewMemStorage())
	v, _ := reg.create(CreateVolumeSpec{ProjectUUID: "p", Name: "n", SizeGiB: 1})
	// Same name → no-op.
	if err := reg.setName(v.UUID, "n"); err != nil {
		t.Errorf("no-op rename should succeed: %v", err)
	}
}

// ── projects.go isUUID edge cases ──────────────────────────────

func TestIsUUID(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"f47ac10b-58cc-4372-a567-0e02b2c3d479", true},
		{"F47AC10B-58CC-4372-A567-0E02B2C3D479", true},
		// Wrong length.
		{"too-short", false},
		// Wrong character at position 8.
		{"f47ac10bx58cc-4372-a567-0e02b2c3d479", false},
		// Non-hex character in body.
		{"f47ac10b-58cc-4372-a567-0e02b2c3d47z", false},
	}
	for _, c := range cases {
		if got := isUUID(c.s); got != c.want {
			t.Errorf("isUUID(%q) = %v, want %v", c.s, got, c.want)
		}
	}
}

// ── projects.go newUUID + defaultProjectName cover small cases ─

func TestNewUUID_Shape(t *testing.T) {
	u := newUUID()
	if !isUUID(u) {
		t.Errorf("newUUID produced non-UUID %q", u)
	}
}

func TestDefaultProjectName_AlwaysNonEmpty(t *testing.T) {
	n := defaultProjectName()
	if n == "" {
		t.Errorf("defaultProjectName should never be empty")
	}
}

// ── projects.go lookupByName: known + unknown ────────────────

func TestProjectRegistry_LookupByName(t *testing.T) {
	reg, _ := loadProjectRegistry(context.Background(), NewMemStorage())
	p, _, _ := reg.getOrCreate("name1")
	if uuid, ok := reg.lookupByName("name1"); !ok || uuid != p.UUID {
		t.Errorf("lookupByName: got (%q, %v), want (%q, true)", uuid, ok, p.UUID)
	}
	if _, ok := reg.lookupByName("nope"); ok {
		t.Errorf("missing should not match")
	}
}

// ── projects.go rename: empty name + unknown UUID + collision ───

func TestProjectRegistry_RenameErrors(t *testing.T) {
	reg, _ := loadProjectRegistry(context.Background(), NewMemStorage())
	p, _, _ := reg.getOrCreate("orig")
	other, _, _ := reg.getOrCreate("other")
	if err := reg.rename(p.UUID, ""); err == nil {
		t.Errorf("empty name should error")
	}
	if err := reg.rename("nope", "x"); err == nil {
		t.Errorf("unknown UUID should error")
	}
	if err := reg.rename(p.UUID, other.Name); err == nil {
		t.Errorf("name collision should error")
	}
	// Self-rename succeeds (just a different in-place update).
	if err := reg.rename(p.UUID, "orig"); err != nil {
		t.Errorf("self-rename: %v", err)
	}
}

// ── projects.go delete: empty project succeeds; cascade error tested elsewhere ─

func TestProjectRegistry_Delete(t *testing.T) {
	reg, _ := loadProjectRegistry(context.Background(), NewMemStorage())
	p, _, _ := reg.getOrCreate("d-test")
	if err := reg.delete(p.UUID); err != nil {
		t.Errorf("delete: %v", err)
	}
	if err := reg.delete("nope"); err == nil {
		t.Errorf("delete unknown should error")
	}
}

// ── projects.go ensureNATSUserSeed nil bus shouldn't matter ────

func TestProjectRegistry_EnsureNATSUserSeed(t *testing.T) {
	reg, _ := loadProjectRegistry(context.Background(), NewMemStorage())
	p, _, _ := reg.getOrCreate("ns")
	seed1, err := reg.ensureNATSUserSeed(p.UUID)
	if err != nil {
		t.Fatal(err)
	}
	// Second call returns the same seed without minting again.
	seed2, _ := reg.ensureNATSUserSeed(p.UUID)
	if seed1 != seed2 {
		t.Errorf("seeds differ")
	}
	// Unknown project.
	if _, err := reg.ensureNATSUserSeed("nope"); err == nil {
		t.Errorf("unknown project should error")
	}
}

// ── projects.go members: defensive copy semantics ─────────────

func TestProjectRegistry_MembersIsDefensiveCopy(t *testing.T) {
	reg, _ := loadProjectRegistry(context.Background(), NewMemStorage())
	p, _, _ := reg.getOrCreate("m")
	_ = reg.addMember(p.UUID, "u1")
	got, ok := reg.members(p.UUID)
	if !ok || len(got) != 1 {
		t.Fatalf("members = %v, ok = %v", got, ok)
	}
	// Mutate the returned slice: must NOT affect the registry.
	got[0] = "mutated"
	got2, _ := reg.members(p.UUID)
	if got2[0] != "u1" {
		t.Errorf("registry mutated through returned slice: %v", got2)
	}
}

// ── projects.go addMember + removeMember edge cases ────────────

func TestProjectRegistry_AddRemoveMember(t *testing.T) {
	reg, _ := loadProjectRegistry(context.Background(), NewMemStorage())
	p, _, _ := reg.getOrCreate("m")
	// Empty userUUID rejected.
	if err := reg.addMember(p.UUID, ""); err == nil {
		t.Errorf("empty userUUID should error")
	}
	// Unknown project.
	if err := reg.addMember("nope", "u"); err == nil {
		t.Errorf("unknown project should error")
	}
	// Add + idempotent.
	if err := reg.addMember(p.UUID, "u1"); err != nil {
		t.Fatal(err)
	}
	if err := reg.addMember(p.UUID, "u1"); err != nil {
		t.Errorf("repeat add should be no-op: %v", err)
	}
	// Remove unknown user from existing project: no-op.
	if err := reg.removeMember(p.UUID, "absent"); err != nil {
		t.Errorf("remove non-member should be no-op: %v", err)
	}
	// Remove unknown project.
	if err := reg.removeMember("nope", "u"); err == nil {
		t.Errorf("unknown project should error")
	}
	// Remove existing member.
	if err := reg.removeMember(p.UUID, "u1"); err != nil {
		t.Fatal(err)
	}
}

// ── DeleteProject refuses when on-disk VMs still exist ─────────

func TestAdapter_DeleteProject_RefusesWhenVMsPresent(t *testing.T) {
	a := newAdapterForRegistries(t)
	p, _, _ := a.CreateProject("p")
	// Simulate a leftover VM on disk under the project.
	dir := filepath.Join(a.vmsDir(), p.UUID, "lingering-vm")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := a.DeleteProject(p.UUID); err == nil {
		t.Errorf("delete should refuse when VMs still exist")
	}
	// Clear the dir, then delete succeeds.
	_ = os.RemoveAll(dir)
	if err := a.DeleteProject(p.UUID); err != nil {
		t.Errorf("delete after clear: %v", err)
	}
}

// ── ProjectMembers via Adapter for round-trip ─────────────────

func TestAdapter_AddRemoveMember(t *testing.T) {
	a := newAdapterForRegistries(t)
	p, _, _ := a.CreateProject("test")
	if err := a.AddProjectMember(p.UUID, "u-1"); err != nil {
		t.Fatal(err)
	}
	members, _ := a.ProjectMembers(p.UUID)
	if len(members) != 1 || members[0] != "u-1" {
		t.Errorf("members = %v", members)
	}
	if err := a.RemoveProjectMember(p.UUID, "u-1"); err != nil {
		t.Fatal(err)
	}
	members, _ = a.ProjectMembers(p.UUID)
	if len(members) != 0 {
		t.Errorf("members after remove = %v", members)
	}
}

// ── callerOwnsProject member path (full integration) ─────────

func TestCallerOwnsProject_MemberPath(t *testing.T) {
	a := newAdapterForRegistries(t)
	caller := &Caller{
		Subject: "ldap:alice",
		Issuer:  "https://dex",
		Email:   "alice@x",
	}
	if _, _, err := a.RegisterUser(caller); err != nil {
		t.Fatal(err)
	}
	u, _ := a.UserBySubject(caller.Issuer, caller.Subject)
	p, _, _ := a.CreateProject("acme")
	// Initially not owner.
	if a.callerOwnsProject(caller, p.UUID) {
		t.Errorf("initially should not own")
	}
	// Add as member.
	_ = a.AddProjectMember(p.UUID, u.UUID)
	if !a.callerOwnsProject(caller, p.UUID) {
		t.Errorf("should own via member")
	}
	// Nil caller is never an owner.
	if a.callerOwnsProject(nil, p.UUID) {
		t.Errorf("nil caller should not own")
	}
	// Unknown project.
	if a.callerOwnsProject(caller, "00000000-0000-0000-0000-000000000000") {
		t.Errorf("unknown project should not be owned")
	}
}

// Pin the conversion: a *Adapter satisfies what we expect (compile guard).
var _ = fmt.Sprintf
