package firewallpub_test

// statusreceiver_integration_test.go runs the StatusReceiver
// against an embedded nats-server, publishes a real
// pod.FirewallStatus on the per-VM subject the way an in-VM agent
// would, and asserts the synthetic "firewall.status" PlatformEvent
// lands on the in-process bus with the right Meta. End-to-end
// validation of the wire contract — no nftables required, runs
// fine on darwin.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	natstest "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"

	weft "github.com/openweft/weft"
	"github.com/openweft/weft/firewallpub"
	"github.com/openweft/weft-microvm-init/pkg/pod"
)

func TestStatusReceiver_EndToEnd_NATS_to_Bus(t *testing.T) {
	srv, url := startNATSServer(t)
	defer srv.Shutdown()

	bus := weft.NewLocalEventBus()
	defer bus.Close()

	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("nats connect: %v", err)
	}
	defer nc.Close()

	rcv, err := firewallpub.NewStatusReceiver(natsWildcardSubscribe(nc), bus, nil)
	if err != nil {
		t.Fatalf("NewStatusReceiver: %v", err)
	}

	// Subscribe to the bus BEFORE Run starts so we don't miss
	// the synthetic event.
	events, cancelSub := bus.Subscribe(weft.EventFilter{
		KindPrefixes: []string{"firewall.status"},
		SeeAll:       true,
	})
	defer cancelSub()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rcv.Run(ctx)

	// Wait until the StatusReceiver's subscription is live on the
	// shared conn before publishing. The goroutine starts async, so
	// a tight test can otherwise publish into the void. Poll for
	// up to 1 s — typical startup is sub-ms ; failing this means
	// the receiver never got off the ground.
	waitDeadline := time.Now().Add(time.Second)
	for nc.NumSubscriptions() == 0 {
		if time.Now().After(waitDeadline) {
			t.Fatal("StatusReceiver subscription never landed")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Simulate an in-VM agent publishing its status.
	payload, _ := json.Marshal(pod.FirewallStatus{
		Overall:         "Healthy",
		TableInstalled:  true,
		RulesInstalled:  9,
		PublishedAtUnix: 1700000123,
	})
	if err := nc.Publish("weft.firewall.vm-42.status", payload); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush2: %v", err)
	}

	select {
	case ev := <-events:
		if ev.Kind != "firewall.status" {
			t.Errorf("Kind = %q", ev.Kind)
		}
		if ev.Subject != "vm-42" {
			t.Errorf("Subject = %q", ev.Subject)
		}
		if ev.Meta["Overall"] != "Healthy" {
			t.Errorf("Meta Overall = %q", ev.Meta["Overall"])
		}
		if ev.Meta["RulesInstalled"] != "9" {
			t.Errorf("Meta RulesInstalled = %q", ev.Meta["RulesInstalled"])
		}
		if ev.Meta["TableInstalled"] != "true" {
			t.Errorf("Meta TableInstalled = %q", ev.Meta["TableInstalled"])
		}
		if ev.Meta["PublishedAtUnix"] != "1700000123" {
			t.Errorf("Meta PublishedAtUnix = %q", ev.Meta["PublishedAtUnix"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no firewall.status event landed on the bus within 2s")
	}
}

// natsWildcardSubscribe mirrors the production adapter in
// cmd/weft/firewall_status_receiver.go — narrow shim so the
// integration test exercises the same NATSSubscribeFunc shape.
func natsWildcardSubscribe(conn *nats.Conn) firewallpub.NATSSubscribeFunc {
	return func(ctx context.Context, subjectPattern string, handler func(string, []byte)) error {
		sub, err := conn.Subscribe(subjectPattern, func(m *nats.Msg) {
			handler(m.Subject, m.Data)
		})
		if err == nil {
			// Flush so the SUB protocol message reached the server
			// before we block on ctx.Done() — otherwise a publish in
			// the next millisecond races the subscription.
			err = conn.Flush()
		}
		if err != nil {
			return err
		}
		defer sub.Unsubscribe()
		<-ctx.Done()
		return ctx.Err()
	}
}

func startNATSServer(t *testing.T) (*natsserver.Server, string) {
	t.Helper()
	opts := natstest.DefaultTestOptions
	opts.Port = -1 // pick a free port
	srv := natstest.RunServer(&opts)
	return srv, srv.ClientURL()
}
