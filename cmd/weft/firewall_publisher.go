package main

// firewall_publisher.go wires the firewallpub.Publisher into the
// daemon's startup path : subscribe to the relevant control-plane
// events (security_group.* / port.* / network.default_*) and
// re-publish each impacted VM's effective firewall ruleset on
// the per-VM NATS subject "weft.firewall.<vm-uuid>".
//
// Activated when the event bus has a NATS connection underneath
// (the only transport the guest-side weft-microvm-agent listens
// on for these per-VM subjects). With the in-process LocalEventBus
// the publisher is skipped and the daemon logs why — local dev
// without NATS has no agent to push to.
//
// Wiring call-site lives in main.go ; this file deliberately keeps
// it to one helper so the rest of cmd/weft only adds two lines.

import (
	"context"
	"log"

	weft "github.com/openweft/weft"
	"github.com/openweft/weft/firewallpub"
)

// startFirewallPublisher subscribes to control-plane events and
// fires firewallpub publishes until the returned cancel runs.
// Returns a no-op cancel when the bus isn't NATS-backed.
//
// The initial Resync is omitted on purpose : the agent's first
// boot publishes nothing visible, and the first relevant control-
// plane mutation will trigger the impacted-VM publish. An explicit
// ResyncAll(adp.ListAllVMUUIDs()) could be added once the operator
// has a way to enumerate every VM — for now we let the event-driven
// path catch up naturally.
func startFirewallPublisher(adp weft.VZAdapter, bus weft.EventBus, logger *log.Logger) func() {
	natsBus, ok := bus.(*weft.NATSEventBus)
	if !ok {
		logger.Printf("firewall publisher: local event bus, no per-VM NATS transport — skipping")
		return func() {}
	}
	conn := natsBus.Conn()
	if conn == nil {
		logger.Printf("firewall publisher: NATS connection closed — skipping")
		return func() {}
	}

	pub := firewallpub.New(adp, firewallpub.JSONPublishFunc(natsConnAdapter{conn}), logger)

	// SeeAll = true : the publisher is a platform-internal consumer,
	// not a tenant ; it needs every relevant event regardless of
	// project visibility. KindPrefixes narrow the wire-tap to just
	// the kinds ImpactedVMs reacts to (any miss is silently dropped
	// by ImpactedVMs, but filtering early keeps wakeups cheap).
	events, cancelSub := bus.Subscribe(weft.EventFilter{
		KindPrefixes: []string{
			"security_group.",
			"port.",
			"network.default_security_groups_updated",
			"vm.created",
		},
		SeeAll: true,
	})

	ctx, cancelCtx := context.WithCancel(context.Background())
	go func() {
		if err := pub.Run(ctx, events); err != nil && err != context.Canceled {
			logger.Printf("firewall publisher exited: %v", err)
		}
	}()

	logger.Printf("firewall publisher: subscribed (subjects: weft.firewall.<vm-uuid>)")
	return func() {
		cancelSub()
		cancelCtx()
	}
}

// natsConnAdapter narrows the full *nats.Conn surface down to the
// 2-method firewallpub.Conn the publisher actually uses. Lets the
// firewallpub package stay free of a direct dep on github.com/nats-io
// and lets tests inject a fake without dragging in the whole client.
type natsConnAdapter struct {
	conn natsConn
}

type natsConn interface {
	Publish(subject string, data []byte) error
}

func (a natsConnAdapter) Publish(subject string, data []byte) error {
	return a.conn.Publish(subject, data)
}
