package main

// firewall_status_receiver.go wires firewallpub.StatusReceiver into
// the daemon : the reverse direction of firewall_publisher.go.
//
// Each in-VM weft-microvm-agent publishes a pod.FirewallStatus on
// "weft.firewall.<vm-uuid>.status" every 10 s (default cadence).
// The receiver subscribes to the wildcard pattern, decodes each
// message, and re-emits a synthetic weft.PlatformEvent of kind
// "firewall.status" on the in-process event bus. That event flows
// through the webui's existing /api/events SSE pipe, so the
// operator sees live per-VM firewall health without a new
// transport.
//
// Kept in a sibling file so firewall_publisher.go stays focused on
// the desired-state push direction and so this wiring is easy to
// disable / replace independently.

import (
	"context"
	"log"

	"github.com/nats-io/nats.go"

	weft "github.com/openweft/weft"
	"github.com/openweft/weft/firewallpub"
)

// startFirewallStatusReceiver subscribes to per-VM firewall status
// messages and republishes them as synthetic platform events.
// Returns a cancel that stops the goroutine + drops the NATS
// subscription. No-op (returns a no-op cancel) when the bus isn't
// NATS-backed.
func startFirewallStatusReceiver(bus weft.EventBus, logger *log.Logger) func() {
	natsBus, ok := bus.(*weft.NATSEventBus)
	if !ok {
		logger.Printf("firewall status receiver: local event bus, no per-VM NATS transport — skipping")
		return func() {}
	}
	conn := natsBus.Conn()
	if conn == nil {
		logger.Printf("firewall status receiver: NATS connection closed — skipping")
		return func() {}
	}

	rcv, err := firewallpub.NewStatusReceiver(natsWildcardSubscribe(conn), bus, logger)
	if err != nil {
		logger.Printf("firewall status receiver: %v ; skipping", err)
		return func() {}
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := rcv.Run(ctx); err != nil && err != context.Canceled {
			logger.Printf("firewall status receiver exited: %v", err)
		}
	}()
	logger.Printf("firewall status receiver: subscribed (weft.firewall.*.status → firewall.status events)")
	return cancel
}

// natsWildcardSubscribe adapts a *nats.Conn into a
// firewallpub.NATSSubscribeFunc. Blocks until ctx is cancelled,
// dropping the subscription on exit so a daemon shutdown doesn't
// leak a NATS callback goroutine.
func natsWildcardSubscribe(conn *nats.Conn) firewallpub.NATSSubscribeFunc {
	return func(ctx context.Context, subjectPattern string, handler func(string, []byte)) error {
		sub, err := conn.Subscribe(subjectPattern, func(m *nats.Msg) {
			handler(m.Subject, m.Data)
		})
		if err != nil {
			return err
		}
		defer sub.Unsubscribe()
		<-ctx.Done()
		return ctx.Err()
	}
}
