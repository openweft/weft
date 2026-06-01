package pluginstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	weftv1 "github.com/openweft/weft-proto"
)

// fakeClient is a hand-rolled Client stub for install tests. It
// records every CreateVM/CreateNetwork/… call so tests can assert
// the placement-spread invariant and the SG-to-network binding.
type fakeClient struct {
	createNetwork                   func(in *weftv1.CreateNetworkRequest) (*weftv1.CreateNetworkResponse, error)
	createSG                        func(in *weftv1.CreateSecurityGroupRequest) (*weftv1.CreateSecurityGroupResponse, error)
	setNetworkDefaultSecurityGroups func(in *weftv1.SetNetworkDefaultSecurityGroupsRequest) (*weftv1.SetNetworkDefaultSecurityGroupsResponse, error)
	createVM                        func(in *weftv1.CreateVMRequest) (*weftv1.CreateVMResponse, error)
	deleteVM                        func(in *weftv1.DeleteVMRequest) (*weftv1.DeleteVMResponse, error)
	deleteNetwork                   func(in *weftv1.DeleteNetworkRequest) (*weftv1.DeleteNetworkResponse, error)
	deleteSG                        func(in *weftv1.DeleteSecurityGroupRequest) (*weftv1.DeleteSecurityGroupResponse, error)
	createVolume                    func(in *weftv1.CreateVolumeRequest) (*weftv1.CreateVolumeResponse, error)
	deleteVolume                    func(in *weftv1.DeleteVolumeRequest) (*weftv1.DeleteVolumeResponse, error)

	// Call counters
	creates     int
	createVMs   []*weftv1.CreateVMRequest
	createSGs   []*weftv1.CreateSecurityGroupRequest
	createNets  []*weftv1.CreateNetworkRequest
	bindCalls   []*weftv1.SetNetworkDefaultSecurityGroupsRequest
	createVols  []*weftv1.CreateVolumeRequest
	delVMs      []*weftv1.DeleteVMRequest
	delNetworks []*weftv1.DeleteNetworkRequest
	delSGs      []*weftv1.DeleteSecurityGroupRequest
	delVols     []*weftv1.DeleteVolumeRequest
}

func (f *fakeClient) CreateNetwork(_ context.Context, in *weftv1.CreateNetworkRequest) (*weftv1.CreateNetworkResponse, error) {
	f.creates++
	f.createNets = append(f.createNets, in)
	if f.createNetwork != nil {
		return f.createNetwork(in)
	}
	return &weftv1.CreateNetworkResponse{Network: &weftv1.NetworkInfo{Uuid: "net-" + in.Name, Name: in.Name, Cidr: in.Cidr, Type: in.Type}}, nil
}
func (f *fakeClient) CreateSecurityGroup(_ context.Context, in *weftv1.CreateSecurityGroupRequest) (*weftv1.CreateSecurityGroupResponse, error) {
	f.creates++
	f.createSGs = append(f.createSGs, in)
	if f.createSG != nil {
		return f.createSG(in)
	}
	return &weftv1.CreateSecurityGroupResponse{Group: &weftv1.SecurityGroupInfo{Uuid: "sg-" + in.Name, Name: in.Name}}, nil
}
func (f *fakeClient) SetNetworkDefaultSecurityGroups(_ context.Context, in *weftv1.SetNetworkDefaultSecurityGroupsRequest) (*weftv1.SetNetworkDefaultSecurityGroupsResponse, error) {
	f.bindCalls = append(f.bindCalls, in)
	if f.setNetworkDefaultSecurityGroups != nil {
		return f.setNetworkDefaultSecurityGroups(in)
	}
	return &weftv1.SetNetworkDefaultSecurityGroupsResponse{}, nil
}
func (f *fakeClient) CreateVM(_ context.Context, in *weftv1.CreateVMRequest) (*weftv1.CreateVMResponse, error) {
	f.creates++
	f.createVMs = append(f.createVMs, in)
	if f.createVM != nil {
		return f.createVM(in)
	}
	return &weftv1.CreateVMResponse{}, nil
}
func (f *fakeClient) DeleteVM(_ context.Context, in *weftv1.DeleteVMRequest) (*weftv1.DeleteVMResponse, error) {
	f.delVMs = append(f.delVMs, in)
	if f.deleteVM != nil {
		return f.deleteVM(in)
	}
	return &weftv1.DeleteVMResponse{}, nil
}
func (f *fakeClient) DeleteNetwork(_ context.Context, in *weftv1.DeleteNetworkRequest) (*weftv1.DeleteNetworkResponse, error) {
	f.delNetworks = append(f.delNetworks, in)
	if f.deleteNetwork != nil {
		return f.deleteNetwork(in)
	}
	return &weftv1.DeleteNetworkResponse{}, nil
}
func (f *fakeClient) DeleteSecurityGroup(_ context.Context, in *weftv1.DeleteSecurityGroupRequest) (*weftv1.DeleteSecurityGroupResponse, error) {
	f.delSGs = append(f.delSGs, in)
	if f.deleteSG != nil {
		return f.deleteSG(in)
	}
	return &weftv1.DeleteSecurityGroupResponse{}, nil
}
func (f *fakeClient) CreateVolume(_ context.Context, in *weftv1.CreateVolumeRequest) (*weftv1.CreateVolumeResponse, error) {
	f.creates++
	f.createVols = append(f.createVols, in)
	if f.createVolume != nil {
		return f.createVolume(in)
	}
	return &weftv1.CreateVolumeResponse{Volume: &weftv1.VolumeInfo{Uuid: "vol-" + in.Name, Name: in.Name, SizeGib: in.SizeGib}}, nil
}
func (f *fakeClient) DeleteVolume(_ context.Context, in *weftv1.DeleteVolumeRequest) (*weftv1.DeleteVolumeResponse, error) {
	f.delVols = append(f.delVols, in)
	if f.deleteVolume != nil {
		return f.deleteVolume(in)
	}
	return &weftv1.DeleteVolumeResponse{}, nil
}

// loadDemo parses the fixture from manifest_test.go.
func loadDemo(t *testing.T) *Manifest {
	t.Helper()
	m, err := ParseManifest("demo.hcl", []byte(minimalManifest))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return m
}

// ── Install : happy path, idempotency, placement spread, SG bind ─

func TestInstall_HappyPath(t *testing.T) {
	c := &fakeClient{}
	mgr := NewManager(c, NewMemStore())
	m := loadDemo(t)
	inst, err := mgr.Install(context.Background(), m, "proj", map[string]any{"token": "abc"})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if got := len(c.createVMs); got != 3 {
		t.Errorf("expected 3 CreateVM calls, got %d", got)
	}
	if got := len(c.createVols); got != 3 {
		t.Errorf("expected 3 CreateVolume calls (one per replica), got %d", got)
	}
	if got := len(c.createSGs); got != 1 {
		t.Errorf("expected 1 CreateSG call, got %d", got)
	}
	if got := len(c.createNets); got != 1 {
		t.Errorf("expected 1 CreateNetwork call, got %d", got)
	}
	if got := len(inst.VMs); got != 3 {
		t.Errorf("instance.VMs = %d", got)
	}
	if inst.UUID == "" {
		t.Errorf("instance UUID empty")
	}
	// Secret input must be masked in stored record.
	if inst.Inputs["token"] != "***" {
		t.Errorf("expected secret token to be masked, got %q", inst.Inputs["token"])
	}
}

func TestInstall_Idempotent(t *testing.T) {
	c := &fakeClient{}
	store := NewMemStore()
	mgr := NewManager(c, store)
	m := loadDemo(t)
	first, err := mgr.Install(context.Background(), m, "proj", map[string]any{"token": "abc"})
	if err != nil {
		t.Fatalf("install first: %v", err)
	}
	firstCalls := c.creates
	second, err := mgr.Install(context.Background(), m, "proj", map[string]any{"token": "abc"})
	if err != nil {
		t.Fatalf("install second: %v", err)
	}
	if first.UUID != second.UUID {
		t.Errorf("expected same UUID on idempotent install, got %q vs %q", first.UUID, second.UUID)
	}
	if c.creates != firstCalls {
		t.Errorf("expected no new RPCs on idempotent install (%d total), got %d", firstCalls, c.creates)
	}
}

func TestInstall_PerDCPlacementSpread(t *testing.T) {
	c := &fakeClient{}
	mgr := NewManager(c, NewMemStore())
	m := loadDemo(t)
	if _, err := mgr.Install(context.Background(), m, "proj", map[string]any{"token": "abc"}); err != nil {
		t.Fatalf("install: %v", err)
	}
	// All three replicas must share the same scheduling rule so
	// the scheduler treats them as a group with az=different.
	if len(c.createVMs) != 3 {
		t.Fatalf("expected 3 CreateVM, got %d", len(c.createVMs))
	}
	rule := c.createVMs[0].SchedulingRule
	if rule == "" || !strings.HasPrefix(rule, "plugin-") {
		t.Errorf("expected plugin-prefixed scheduling rule, got %q", rule)
	}
	for i, vm := range c.createVMs {
		if vm.SchedulingRule != rule {
			t.Errorf("replica %d scheduling rule = %q (want %q)", i, vm.SchedulingRule, rule)
		}
	}
	// VM names must be unique across the 3 replicas (`-0`, `-1`, `-2`).
	seen := map[string]bool{}
	for _, vm := range c.createVMs {
		if seen[vm.Name] {
			t.Errorf("duplicate vm name %q", vm.Name)
		}
		seen[vm.Name] = true
	}
}

func TestInstall_SecurityGroupAttachedToNetwork(t *testing.T) {
	c := &fakeClient{}
	mgr := NewManager(c, NewMemStore())
	m := loadDemo(t)
	if _, err := mgr.Install(context.Background(), m, "proj", map[string]any{"token": "abc"}); err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(c.bindCalls) != 1 {
		t.Fatalf("expected 1 SetNetworkDefaultSecurityGroups call, got %d", len(c.bindCalls))
	}
	if len(c.bindCalls[0].SecurityGroupUuids) != 1 {
		t.Errorf("expected 1 SG bound to network, got %d", len(c.bindCalls[0].SecurityGroupUuids))
	}
	// The bound SG must be the one we just created.
	if c.bindCalls[0].SecurityGroupUuids[0] != c.createSGs[0].Name && c.bindCalls[0].SecurityGroupUuids[0] != "sg-"+c.createSGs[0].Name {
		t.Errorf("bound sg %q doesn't match created sg %q", c.bindCalls[0].SecurityGroupUuids[0], c.createSGs[0].Name)
	}
}

// ── Uninstall + List ─────────────────────────────────────────────

func TestUninstall_ReverseOrder(t *testing.T) {
	c := &fakeClient{}
	mgr := NewManager(c, NewMemStore())
	m := loadDemo(t)
	inst, err := mgr.Install(context.Background(), m, "proj", map[string]any{"token": "abc"})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := mgr.Uninstall(context.Background(), inst.Name, inst.UUID); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if len(c.delVMs) != 3 {
		t.Errorf("expected 3 DeleteVM calls, got %d", len(c.delVMs))
	}
	if len(c.delVols) != 3 {
		t.Errorf("expected 3 DeleteVolume calls, got %d", len(c.delVols))
	}
	if len(c.delSGs) != 1 {
		t.Errorf("expected 1 DeleteSecurityGroup call, got %d", len(c.delSGs))
	}
	if len(c.delNetworks) != 1 {
		t.Errorf("expected 1 DeleteNetwork call, got %d", len(c.delNetworks))
	}
	// Confirm instance is gone from the store.
	got, err := mgr.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, i := range got {
		if i.UUID == inst.UUID {
			t.Errorf("instance %q still in store after uninstall", inst.UUID)
		}
	}
}

func TestList_ListsInstalledInstances(t *testing.T) {
	c := &fakeClient{}
	mgr := NewManager(c, NewMemStore())
	m := loadDemo(t)
	if _, err := mgr.Install(context.Background(), m, "proj-a", map[string]any{"token": "a"}); err != nil {
		t.Fatalf("install a: %v", err)
	}
	if _, err := mgr.Install(context.Background(), m, "proj-b", map[string]any{"token": "b"}); err != nil {
		t.Fatalf("install b: %v", err)
	}
	got, err := mgr.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("expected 2 instances, got %d", len(got))
	}
}

// ── Failure : rollback ───────────────────────────────────────────

func TestInstall_RollbackOnCreateVMFailure(t *testing.T) {
	c := &fakeClient{}
	c.createVM = func(in *weftv1.CreateVMRequest) (*weftv1.CreateVMResponse, error) {
		// Fail the third VM so the first two were already created.
		if strings.HasSuffix(in.Name, "-2") {
			return nil, errors.New("boom")
		}
		return &weftv1.CreateVMResponse{}, nil
	}
	store := NewMemStore()
	mgr := NewManager(c, store)
	m := loadDemo(t)
	if _, err := mgr.Install(context.Background(), m, "proj", map[string]any{"token": "abc"}); err == nil {
		t.Fatal("expected install to fail")
	}
	// Rollback deletes the network, SG, the two already-created
	// VMs, and the volumes attached to those VMs (3 attempted —
	// the third volume was created right before the CreateVM
	// for the third replica failed).
	if len(c.delNetworks) != 1 {
		t.Errorf("rollback should delete the network, got %d calls", len(c.delNetworks))
	}
	if len(c.delSGs) != 1 {
		t.Errorf("rollback should delete the security group, got %d calls", len(c.delSGs))
	}
	if len(c.delVMs) < 2 {
		t.Errorf("rollback should delete the 2 successful VMs, got %d", len(c.delVMs))
	}
	// Store should be empty — failed install must not record.
	all, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("expected empty store after rollback, got %d entries", len(all))
	}
}

// ── FileStore round-trip ─────────────────────────────────────────

func TestFileStore_PersistsAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	s := NewFileStore(dir)
	in := Instance{Name: "demo", UUID: "1234", Project: "p", VMs: []string{"a", "b"}}
	if err := s.Put(in); err != nil {
		t.Fatalf("put: %v", err)
	}
	// Re-open with a fresh handle to confirm persistence.
	s2 := NewFileStore(dir)
	got, ok, err := s2.Get("demo", "1234")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.Project != "p" || len(got.VMs) != 2 {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	list, err := s2.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v, len=%d", err, len(list))
	}
	if err := s2.Delete("demo", "1234"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, ok, _ = s2.Get("demo", "1234")
	if ok {
		t.Errorf("expected delete to remove the record")
	}
}

// Sanity check the qualifiedName / replicaName helpers — they're
// what the agent sees as resource names downstream, so naming
// regressions hurt operators trying to find the VMs.
func TestNameDerivation(t *testing.T) {
	uuid := "abcdef01-2222-3333-4444-555555555555"
	if got := qualifiedName("demo", uuid, "runners"); got != "demo-abcdef01-runners" {
		t.Errorf("qualifiedName = %q", got)
	}
	if got := replicaName("demo", uuid, "runner", 2); got != fmt.Sprintf("demo-abcdef01-runner-%d", 2) {
		t.Errorf("replicaName = %q", got)
	}
}
