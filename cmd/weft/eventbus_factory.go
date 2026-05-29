package main

// eventbus_factory.go translates the operator-chosen event-bus
// backend (CLI flag or HCL config) into a weft.EventBus the
// Adapter receives via SetEventBus.
//
// Two backends today, matching [[vzd-event-bus-nats]]:
//
//   * "local" (default) — in-process LocalEventBus. No external
//     dep at runtime; perfect for single-host dev.
//
//   * "nats" — NATSEventBus pointed at the cluster URL in
//     `event_bus { nats { url = "..." } }`. Production path.
//     The factory opens the NATS connection at vzd-startup time
//     so a misconfigured URL fails fast rather than at first
//     event publish.

import (
	"fmt"

	"github.com/openweft/weft"
)

// busFactory bundles the chosen bus + its tear-down hook. Close
// is called at vzd shutdown to release any shared connection
// (NATS) the bus keeps alive.
type busFactory struct {
	bus   weft.EventBus
	close func() error
}

// buildEventBus inspects the resolved fileConfigTargets and
// returns the bus the Adapter should run with. Default backend
// is "local" — no environment knobs needed for the single-host
// path.
func buildEventBus(t fileConfigTargets) (*busFactory, error) {
	backend := t.eventBusBackend
	if backend == "" {
		backend = "local"
	}
	switch backend {
	case "local":
		bus := weft.NewLocalEventBus()
		return &busFactory{
			bus:   bus,
			close: bus.Close,
		}, nil
	case "nats":
		if t.natsURL == "" {
			return nil, fmt.Errorf("event_bus backend = nats but no URL configured (set event_bus.nats.url in vzd.hcl)")
		}
		bus, err := weft.NewNATSEventBus(weft.NATSConfig{
			URL:             t.natsURL,
			CredentialsFile: t.natsCredentialsFile,
			Name:            t.natsName,
			SubjectPrefix:   t.natsSubjectPrefix,
		})
		if err != nil {
			return nil, fmt.Errorf("nats event bus: %w", err)
		}
		return &busFactory{
			bus:   bus,
			close: bus.Close,
		}, nil
	default:
		return nil, fmt.Errorf("unknown event_bus backend %q (want local or nats)", backend)
	}
}

// displayEventBusBackend returns the human-readable backend name
// for the startup log line. Empty / "local" both render as "local".
func displayEventBusBackend(b string) string {
	if b == "" {
		return "local"
	}
	return b
}
