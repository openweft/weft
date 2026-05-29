package weft

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	natsserver "github.com/nats-io/nats-server/v2/test"
	"github.com/openweft/weft-microvm-init/pkg/pod"
)

func cubefsShare() pod.ShareMount {
	return pod.ShareMount{
		ID:         "team-data",
		MountPoint: "/run/weft/shares/team-data",
		CubeFS: &pod.CubeFSMount{
			Volume:  "team-data",
			Masters: []string{"10.9.0.1:17010"},
			Owner:   "team-alpha",
		},
	}
}

// TestAttachShareToProject_FansOutToProjectVMs seeds two VMs in project p1
// (and one in p2), then confirms AttachShareToProject delivers the mount to
// exactly p1's VMs over the event bus — the teacher→class propagation.
func TestAttachShareToProject_FansOutToProjectVMs(t *testing.T) {
	srv := natsserver.RunRandClientPortServer()
	defer srv.Shutdown()
	url := srv.ClientURL()

	bus, err := NewNATSEventBus(NATSConfig{URL: url, Name: "weft-test"})
	if err != nil {
		t.Fatalf("NewNATSEventBus: %v", err)
	}
	defer bus.Close()

	reg, _ := loadVMRegistry(context.Background(), NewMemStorage())
	vmA, _ := reg.create(CreateVMSpec{ProjectUUID: "p1", Name: "a", HostUUID: "h1"})
	vmB, _ := reg.create(CreateVMSpec{ProjectUUID: "p1", Name: "b", HostUUID: "h1"})
	vmC, _ := reg.create(CreateVMSpec{ProjectUUID: "p2", Name: "c", HostUUID: "h1"})

	a := &Adapter{}
	a.vmReg = reg
	a.SetEventBus(bus)

	// A separate connection subscribes to each VM's mount subject.
	sub, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer sub.Close()

	got := make(chan string, 4) // vmID that received a mount
	subscribe := func(vmID string) {
		if _, err := sub.Subscribe("weft.mounts."+vmID, func(m *nats.Msg) {
			var sm pod.ShareMount
			if err := json.Unmarshal(m.Data, &sm); err == nil && sm.ID == "team-data" {
				got <- vmID
			}
		}); err != nil {
			t.Fatalf("subscribe %s: %v", vmID, err)
		}
	}
	subscribe(vmA.UUID)
	subscribe(vmB.UUID)
	subscribe(vmC.UUID)
	_ = sub.Flush()

	if n, err := a.AttachShareToProject("p1", cubefsShare()); err != nil {
		t.Fatalf("AttachShareToProject: %v", err)
	} else if n != 2 {
		t.Errorf("vm_count = %d, want 2", n)
	}

	received := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case id := <-got:
			received[id] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out; received=%v", received)
		}
	}
	if !received[vmA.UUID] || !received[vmB.UUID] {
		t.Errorf("p1 VMs should both receive: %v", received)
	}
	// vmC (p2) must not have received — give a brief window to be sure.
	select {
	case id := <-got:
		if id == vmC.UUID {
			t.Errorf("p2 VM %s received a p1 share", id)
		}
	case <-time.After(200 * time.Millisecond):
	}
}

func TestAttachShareToProject_NonNATSBus(t *testing.T) {
	a := &Adapter{} // no bus set → natsConnFromBus errors
	if _, err := a.AttachShareToProject("p1", cubefsShare()); err == nil {
		t.Fatal("expected error without a NATS bus")
	}
}
