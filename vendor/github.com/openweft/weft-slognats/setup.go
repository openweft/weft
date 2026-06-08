package weftslognats

import (
	"io"
	"log/slog"
	"os"

	"github.com/nats-io/nats.go"
)

// EnvNATSURL is the env var the SetupFromEnv helper reads. Unset →
// the handler degrades to base-only (stderr) cleanly.
const EnvNATSURL = "WEFT_NATS_URL"

// SetupFromEnv wires the canonical openweft slog setup in one call.
// Returned Logger writes JSON to stderr (always) and additionally
// publishes WARN+ERROR records to NATS on the given subject (when
// WEFT_NATS_URL is set + reachable).
//
// Returned io.Closer drains the NATS connection on shutdown ; safe to
// call when no NATS connection was opened (no-op).
//
// Subject convention :
//
//	weft.<component>.<host_or_vm_id>.log
//
// e.g. "weft.agent."+hostUUID+".log",
//      "weft.driver.qemu."+hostUUID+".log",
//      "weft.ha.postgres."+nodeName+".log".
//
// Failure modes :
//   - WEFT_NATS_URL unset           → degraded mode, no error
//   - nats.Connect fails            → degraded mode + WARN to stderr
//   - subject == ""                 → panic (Handler contract)
func SetupFromEnv(subject string) (*slog.Logger, io.Closer) {
	base := slog.NewJSONHandler(os.Stderr, nil)

	var conn Publisher
	var closer io.Closer = noopCloser{}

	if url := os.Getenv(EnvNATSURL); url != "" {
		c, err := nats.Connect(url, nats.Name(subject))
		if err == nil {
			conn = FromConn(c)
			closer = natsCloser{c: c}
		} else {
			// We can't log via the brand-new handler yet — fall back
			// to stderr directly so the operator sees the dial error.
			slog.New(base).Warn("weft-slognats: NATS dial failed, degrading to stderr-only",
				"url", url, "err", err)
		}
	}

	h := NewHandler(Options{
		Base:    base,
		Conn:    conn,
		Subject: subject,
	})
	return slog.New(h), closer
}

type noopCloser struct{}

func (noopCloser) Close() error { return nil }

type natsCloser struct{ c *nats.Conn }

func (n natsCloser) Close() error {
	if n.c == nil {
		return nil
	}
	return n.c.Drain()
}
