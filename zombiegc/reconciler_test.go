package zombiegc

import (
	"context"
	"testing"
	"time"

	weft "github.com/openweft/weft"
)

// fakeAdapter implements the subset of weft.VZAdapter zombiegc needs.
// The full interface is large but we only call VMs / Hosts / Projects
// / SetVMState / DeleteVM, so we embed the no-op stub.
type fakeAdapter struct {
	weft.VZAdapter // embed for zero-value defaults on the rest

	vms      []weft.VM
	hosts    []weft.Host
	projects []weft.Project

	deletions  []string
	stateSets  []stateSet
	deleteFail error
}

type stateSet struct {
	uuid  string
	state weft.VMState
}

func (a *fakeAdapter) VMs() []weft.VM           { return a.vms }
func (a *fakeAdapter) Hosts() []weft.Host       { return a.hosts }
func (a *fakeAdapter) Projects() []weft.Project { return a.projects }
func (a *fakeAdapter) SetVMState(uuid string, state weft.VMState) error {
	a.stateSets = append(a.stateSets, stateSet{uuid, state})
	// Reflect the new state back into the in-memory VM so a follow-up
	// Sweep sees it as zombie (mirrors the registry round-trip).
	for i, v := range a.vms {
		if v.UUID == uuid {
			a.vms[i].State = state
		}
	}
	return nil
}
func (a *fakeAdapter) DeleteVM(name string) error {
	if a.deleteFail != nil {
		return a.deleteFail
	}
	a.deletions = append(a.deletions, name)
	for i, v := range a.vms {
		if v.Name == name {
			a.vms = append(a.vms[:i], a.vms[i+1:]...)
			break
		}
	}
	return nil
}

type fakeProbe struct {
	alive map[string]bool
}

func (p *fakeProbe) IsVMRunning(name string) bool { return p.alive[name] }

const localHost = "host-local-uuid"

func setup() (*fakeAdapter, *fakeProbe) {
	now := time.Now().UTC()
	return &fakeAdapter{
			hosts: []weft.Host{
				{UUID: localHost, State: weft.HostStateActive, LastSeenAt: now},
			},
			projects: []weft.Project{{UUID: "p-1"}},
		}, &fakeProbe{alive: map[string]bool{}}
}

func TestSweep_LocalZombie_MarkedNotDeleted(t *testing.T) {
	adp, probe := setup()
	adp.vms = []weft.VM{
		{UUID: "vm-1", Name: "web", ProjectUUID: "p-1", HostUUID: localHost, State: weft.VMStateRunning},
	}
	// Probe says VM is not running → local zombie.
	r := New(adp, probe, localHost, Options{})
	rep := r.Sweep(context.Background())
	if len(rep.Zombies) != 1 || rep.Zombies[0].Kind != ZombieLocal {
		t.Fatalf("expected 1 local zombie, got %+v", rep.Zombies)
	}
	if rep.Deleted != 0 {
		t.Error("local zombies must never be auto-deleted")
	}
	if len(adp.stateSets) != 1 || adp.stateSets[0].state != weft.VMStateZombie {
		t.Errorf("expected 1 mark-zombie SetVMState, got %v", adp.stateSets)
	}
}

func TestSweep_CIZombie_MarkedThenDeletedAfterGrace(t *testing.T) {
	adp, probe := setup()
	deadHost := "host-dead"
	hostDownTime := time.Now().Add(-2 * time.Hour).UTC()
	adp.hosts = append(adp.hosts, weft.Host{UUID: deadHost, State: weft.HostStateDown, LastSeenAt: hostDownTime})
	adp.vms = []weft.VM{
		{UUID: "vm-ci", Name: "runner", ProjectUUID: "p-1", HostUUID: deadHost,
			State: weft.VMStateRunning, Labels: map[string]string{"deployment.type": "ci"}},
	}
	r := New(adp, probe, localHost, Options{CIGracePeriod: 1 * time.Hour})

	// First sweep marks zombie + deletes (host has been down 2h, grace=1h).
	rep := r.Sweep(context.Background())
	if len(rep.Zombies) != 1 || rep.Zombies[0].Kind != ZombieCICrossHost {
		t.Fatalf("expected 1 ci_cross_host zombie, got %+v", rep.Zombies)
	}
	if rep.Deleted != 1 {
		t.Errorf("CI zombie past grace should be deleted, got Deleted=%d", rep.Deleted)
	}
	if len(adp.deletions) != 1 || adp.deletions[0] != "runner" {
		t.Errorf("expected DeleteVM(runner), got %v", adp.deletions)
	}
}

func TestSweep_CIZombie_GraceNotElapsed_MarksOnly(t *testing.T) {
	adp, probe := setup()
	deadHost := "host-dead"
	hostDownTime := time.Now().Add(-10 * time.Minute).UTC()
	adp.hosts = append(adp.hosts, weft.Host{UUID: deadHost, State: weft.HostStateDown, LastSeenAt: hostDownTime})
	adp.vms = []weft.VM{
		{UUID: "vm-ci", Name: "runner", ProjectUUID: "p-1", HostUUID: deadHost,
			State: weft.VMStateRunning, Labels: map[string]string{"deployment.type": "ci"}},
	}
	r := New(adp, probe, localHost, Options{CIGracePeriod: 1 * time.Hour})

	rep := r.Sweep(context.Background())
	if len(rep.Zombies) != 1 || rep.Zombies[0].Kind != ZombieCICrossHost {
		t.Fatalf("expected ci_cross_host, got %+v", rep.Zombies)
	}
	if rep.Deleted != 0 {
		t.Errorf("grace not elapsed yet — got Deleted=%d", rep.Deleted)
	}
}

func TestSweep_HAZombie_NeverAutoDeleted(t *testing.T) {
	adp, probe := setup()
	deadHost := "host-dead"
	hostDownTime := time.Now().Add(-48 * time.Hour).UTC() // 2 days
	adp.hosts = append(adp.hosts, weft.Host{UUID: deadHost, State: weft.HostStateDown, LastSeenAt: hostDownTime})
	adp.vms = []weft.VM{
		{UUID: "vm-ha", Name: "postgres-1", ProjectUUID: "p-1", HostUUID: deadHost,
			State: weft.VMStateRunning, Labels: map[string]string{"deployment.type": "ha"}},
	}
	r := New(adp, probe, localHost, Options{CIGracePeriod: 1 * time.Hour})

	rep := r.Sweep(context.Background())
	if len(rep.Zombies) != 1 || rep.Zombies[0].Kind != ZombieHACrossHost {
		t.Fatalf("expected ha_cross_host, got %+v", rep.Zombies)
	}
	if rep.Deleted != 0 {
		t.Errorf("HA zombies must NEVER be auto-deleted, got Deleted=%d", rep.Deleted)
	}
}

func TestSweep_OrphanProject(t *testing.T) {
	adp, probe := setup()
	adp.vms = []weft.VM{
		{UUID: "vm-x", Name: "old-vm", ProjectUUID: "p-deleted", HostUUID: localHost, State: weft.VMStateRunning},
	}
	r := New(adp, probe, localHost, Options{})
	rep := r.Sweep(context.Background())
	if len(rep.Zombies) != 1 || rep.Zombies[0].Kind != ZombieOrphanProject {
		t.Fatalf("expected orphan_project, got %+v", rep.Zombies)
	}
	if rep.Deleted != 0 {
		t.Error("orphan-project must never be auto-deleted")
	}
}

func TestSweep_RunningHealthy_NoZombie(t *testing.T) {
	adp, probe := setup()
	adp.vms = []weft.VM{
		{UUID: "vm-h", Name: "web", ProjectUUID: "p-1", HostUUID: localHost, State: weft.VMStateRunning},
	}
	probe.alive["web"] = true
	r := New(adp, probe, localHost, Options{})
	rep := r.Sweep(context.Background())
	if len(rep.Zombies) != 0 {
		t.Errorf("healthy VM should not be flagged, got %+v", rep.Zombies)
	}
}

func TestSweep_DryRun_NoMutations(t *testing.T) {
	adp, probe := setup()
	adp.vms = []weft.VM{
		{UUID: "vm-1", Name: "web", ProjectUUID: "p-1", HostUUID: localHost, State: weft.VMStateRunning},
	}
	r := New(adp, probe, localHost, Options{DryRun: true})
	rep := r.Sweep(context.Background())
	if len(rep.Zombies) != 1 {
		t.Fatalf("expected 1 zombie in report, got %+v", rep.Zombies)
	}
	if len(adp.stateSets) != 0 || len(adp.deletions) != 0 {
		t.Errorf("dry-run must not mutate, got state=%v delete=%v", adp.stateSets, adp.deletions)
	}
}

func TestSweep_LocalCIZombie_AutoDeleted(t *testing.T) {
	adp, probe := setup()
	// CreatedAt 2h ago, grace 1h → past grace.
	createdLong := time.Now().Add(-2 * time.Hour).UTC()
	adp.vms = []weft.VM{
		{UUID: "vm-ci-local", Name: "ci-runner-7", ProjectUUID: "p-1", HostUUID: localHost,
			State: weft.VMStateCreated, CreatedAt: createdLong,
			Labels: map[string]string{"deployment.type": "ci"}},
	}
	// probe.alive["ci-runner-7"] not set → IsVMRunning=false → local zombie
	r := New(adp, probe, localHost, Options{CIGracePeriod: 1 * time.Hour})

	rep := r.Sweep(context.Background())
	if len(rep.Zombies) != 1 || rep.Zombies[0].Kind != ZombieLocal {
		t.Fatalf("expected 1 local zombie, got %+v", rep.Zombies)
	}
	if rep.Deleted != 1 {
		t.Errorf("local CI past grace should auto-delete, got Deleted=%d", rep.Deleted)
	}
	if len(adp.deletions) != 1 || adp.deletions[0] != "ci-runner-7" {
		t.Errorf("expected DeleteVM(ci-runner-7), got %v", adp.deletions)
	}
}

func TestSweep_LocalNonCIZombie_NeverDeleted(t *testing.T) {
	adp, probe := setup()
	createdLong := time.Now().Add(-48 * time.Hour).UTC()
	adp.vms = []weft.VM{
		{UUID: "vm-ha-local", Name: "web", ProjectUUID: "p-1", HostUUID: localHost,
			State: weft.VMStateCreated, CreatedAt: createdLong,
			Labels: map[string]string{"deployment.type": "ha"}},
	}
	r := New(adp, probe, localHost, Options{CIGracePeriod: 1 * time.Hour})

	rep := r.Sweep(context.Background())
	if len(rep.Zombies) != 1 || rep.Zombies[0].Kind != ZombieLocal {
		t.Fatalf("expected 1 local zombie, got %+v", rep.Zombies)
	}
	if rep.Deleted != 0 {
		t.Errorf("local HA must NEVER be auto-deleted, got Deleted=%d", rep.Deleted)
	}
}

func TestSweep_OrphanHost(t *testing.T) {
	adp, probe := setup()
	adp.vms = []weft.VM{
		{UUID: "vm-h", Name: "wanderer", ProjectUUID: "p-1", HostUUID: "host-never-existed",
			State: weft.VMStateRunning, Labels: map[string]string{"deployment.type": "ha"}},
	}
	r := New(adp, probe, localHost, Options{CIGracePeriod: 1 * time.Hour})
	rep := r.Sweep(context.Background())
	if len(rep.Zombies) != 1 || rep.Zombies[0].Kind != ZombieHACrossHost {
		t.Fatalf("unknown host should produce ha_cross_host zombie, got %+v", rep.Zombies)
	}
	if rep.Deleted != 0 {
		t.Error("unknown-host VM must never be auto-deleted")
	}
}

func TestStatsSnapshot_Concurrent(t *testing.T) {
	adp, probe := setup()
	adp.vms = []weft.VM{
		{UUID: "vm-1", Name: "web", ProjectUUID: "p-1", HostUUID: localHost, State: weft.VMStateRunning},
	}
	r := New(adp, probe, localHost, Options{})
	_ = r.Sweep(context.Background())
	stats := r.StatsSnapshot()
	if stats.ZombiesByKind[ZombieLocal] != 1 {
		t.Errorf("expected 1 local in stats, got %v", stats.ZombiesByKind)
	}
	if stats.LastSweepAt.IsZero() {
		t.Error("LastSweepAt not set")
	}
}
