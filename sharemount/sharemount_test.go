package sharemount

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	natsserver "github.com/nats-io/nats-server/v2/test"
	"github.com/openweft/weft-microvm-init/pkg/pod"
)

func cubefsMount() pod.ShareMount {
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

func TestSubject(t *testing.T) {
	if got := Subject("vm-7"); got != "weft.mounts.vm-7" {
		t.Errorf("Subject = %q", got)
	}
}

func TestPublish_Invalid(t *testing.T) {
	// nil nc is fine — validation fails before any publish.
	if err := Publish(nil, "vm1", pod.ShareMount{MountPoint: "/x"}); err == nil {
		t.Fatal("expected validation error (missing id)")
	}
}

// TestPublishToGroup_FansOut runs an embedded NATS server, subscribes the
// two VMs of a "class", fans one mount out to the group, and confirms each
// VM receives the identical ShareMount — the teacher→class propagation path.
func TestPublishToGroup_FansOut(t *testing.T) {
	srv := natsserver.RunRandClientPortServer()
	defer srv.Shutdown()

	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()

	got := make(chan pod.ShareMount, 2)
	for _, vmID := range []string{"vm1", "vm2"} {
		if _, err := nc.Subscribe(Subject(vmID), func(m *nats.Msg) {
			var sm pod.ShareMount
			if err := json.Unmarshal(m.Data, &sm); err != nil {
				t.Errorf("decode: %v", err)
				return
			}
			got <- sm
		}); err != nil {
			t.Fatalf("subscribe %s: %v", vmID, err)
		}
	}

	if err := PublishToGroup(nc, []string{"vm1", "vm2"}, cubefsMount()); err != nil {
		t.Fatalf("PublishToGroup: %v", err)
	}

	for i := 0; i < 2; i++ {
		select {
		case sm := <-got:
			if sm.ID != "team-data" || sm.CubeFS == nil || sm.CubeFS.Volume != "team-data" {
				t.Errorf("VM received wrong mount: %+v", sm)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for fan-out delivery")
		}
	}
}

func TestPublishToGroup_InvalidNotSent(t *testing.T) {
	srv := natsserver.RunRandClientPortServer()
	defer srv.Shutdown()
	nc, _ := nats.Connect(srv.ClientURL())
	defer nc.Close()

	delivered := make(chan struct{}, 1)
	_, _ = nc.Subscribe(Subject("vm1"), func(*nats.Msg) { delivered <- struct{}{} })

	// Missing CubeFS block → Validate fails → nothing published.
	bad := pod.ShareMount{ID: "x", MountPoint: "/x"}
	if err := PublishToGroup(nc, []string{"vm1"}, bad); err == nil {
		t.Fatal("expected validation error")
	}
	_ = nc.Flush()
	select {
	case <-delivered:
		t.Fatal("an invalid mount was published to the bus")
	case <-time.After(200 * time.Millisecond):
	}
}
