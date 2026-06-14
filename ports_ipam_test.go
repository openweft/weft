package weft

import (
	"testing"
)

// portsIPAMHarness creates an Adapter with one Network + a memory
// storage backend so CreatePort actually runs against a real
// portRegistry. Kept narrow ; reuses the existing test plumbing.
func portsIPAMHarness(t *testing.T) (VZAdapter, Network) {
	t.Helper()
	a := NewWithStorage(t.TempDir(), func(string) Storage { return &memStorage{} })
	proj, _, err := a.CreateProject("p")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	n, err := a.CreateNetwork(CreateNetworkSpec{
		ProjectUUID: proj.UUID,
		Name:        "net",
		CIDR:        "10.0.0.0/29", // 6 host IPs, .1 is gateway
		Gateway:     "10.0.0.1",
	})
	if err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
	return a, n
}

func TestCreatePort_AutoAllocateSkipsGateway(t *testing.T) {
	a, n := portsIPAMHarness(t)
	p, err := a.CreatePort(CreatePortSpec{
		ProjectUUID: n.ProjectUUID,
		VMUUID:      "vm-1",
		NetworkUUID: n.UUID,
		MAC:         "52:54:00:00:00:01",
		// IP intentionally empty → auto-allocate.
	})
	if err != nil {
		t.Fatalf("CreatePort: %v", err)
	}
	// /29 .0 network, .1 gateway → first free = .2.
	if p.IP != "10.0.0.2" {
		t.Errorf("auto-allocated IP = %s, want 10.0.0.2", p.IP)
	}
}

func TestCreatePort_AutoAllocateSkipsAlreadyUsed(t *testing.T) {
	a, n := portsIPAMHarness(t)
	for i, mac := range []string{"52:54:00:00:00:01", "52:54:00:00:00:02", "52:54:00:00:00:03"} {
		p, err := a.CreatePort(CreatePortSpec{
			ProjectUUID: n.ProjectUUID,
			VMUUID:      "vm-" + string(rune('a'+i)),
			NetworkUUID: n.UUID,
			MAC:         mac,
		})
		if err != nil {
			t.Fatalf("CreatePort iter %d: %v", i, err)
		}
		// .2, .3, .4 (gateway .1 + first two ports already taken).
		want := []string{"10.0.0.2", "10.0.0.3", "10.0.0.4"}[i]
		if p.IP != want {
			t.Errorf("iter %d : IP = %s, want %s", i, p.IP, want)
		}
	}
}

func TestCreatePort_AutoAllocateExhausted(t *testing.T) {
	a, n := portsIPAMHarness(t)
	// /29 has 4 free IPs after skipping .0/.1/.7 : .2-.6 = 5
	// addresses, gateway .1 excluded leaves 5 host IPs (.2-.6).
	for i := 0; i < 5; i++ {
		if _, err := a.CreatePort(CreatePortSpec{
			ProjectUUID: n.ProjectUUID,
			VMUUID:      "vm-" + string(rune('a'+i)),
			NetworkUUID: n.UUID,
			MAC:         "52:54:00:00:00:0" + string(rune('1'+i)),
		}); err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
	}
	// 6th should fail : pool exhausted.
	if _, err := a.CreatePort(CreatePortSpec{
		ProjectUUID: n.ProjectUUID,
		VMUUID:      "vm-overflow",
		NetworkUUID: n.UUID,
		MAC:         "52:54:00:00:00:09",
	}); err == nil {
		t.Error("expected exhaustion error on 6th port")
	}
}

func TestCreatePort_ExplicitIPStillValidated(t *testing.T) {
	a, n := portsIPAMHarness(t)
	// Out-of-range IP must still be rejected.
	if _, err := a.CreatePort(CreatePortSpec{
		ProjectUUID: n.ProjectUUID,
		VMUUID:      "vm-1",
		NetworkUUID: n.UUID,
		MAC:         "52:54:00:00:00:01",
		IP:          "10.1.0.5",
	}); err == nil {
		t.Error("expected out-of-cidr error")
	}
}
