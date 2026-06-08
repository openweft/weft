// Package weftslognats provides a slog.Handler that fans logs out to
// both stderr (or any base handler) and a NATS subject. Errors and
// warnings published on NATS are what weft-doctor's NATSIngester
// subscribes to ; the stderr path stays untouched so existing tooling
// (operator tailing, kubectl-like CLI sniff, journald) keeps working.
//
// Wire it once in main.go :
//
//	conn, _ := nats.Connect("nats://nats.weft.svc:4222")
//	log := slog.New(weftslognats.NewHandler(weftslognats.Options{
//	    Base:    slog.NewJSONHandler(os.Stderr, nil),
//	    Conn:    conn,
//	    Subject: "weft.agent." + hostUUID + ".log",
//	}))
//	slog.SetDefault(log)
//
// Everything that already calls slog.{Info,Warn,Error} keeps working
// unchanged ; WARN+ERROR records additionally flow onto NATS in
// slog.JSONHandler-compatible format so weft-doctor can parse them.
package weftslognats

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go"
)

// Publisher is the NATS subset the handler depends on. Decoupled so
// tests can drop in a fake without spinning up an embedded NATS.
type Publisher interface {
	Publish(subject string, data []byte) error
}

// Options configures the handler.
type Options struct {
	// Base receives every record (stderr / journald / whatever the
	// operator wired). Required ; nil panics — there's no use case
	// for "publish to NATS only", and silent drop on a malformed
	// config would mask outages.
	Base slog.Handler
	// Conn is the NATS connection. nil is ALLOWED and means
	// "publish nowhere, Base only" : useful for dev, tests, and
	// graceful degrade when NATS is unreachable at startup.
	Conn Publisher
	// Subject is the NATS subject WARN+ERROR records are published
	// to. Required when Conn is set. Convention :
	//   weft.<component>.<host_id>.log
	// e.g. weft.agent.dc1-r1-h1.log, weft.microvm-agent.<vm_uuid>.log.
	Subject string
	// MinLevel is the floor at which NATS publishing kicks in.
	// Default slog.LevelWarn — keeps the diagnosis pipeline focused
	// on actionable signal, not DEBUG/INFO chatter.
	MinLevel slog.Level
	// OnPublishError is called when a NATS publish fails. Default :
	// no-op (we don't want logging to itself cascade). Set to a
	// callback when you want visibility (e.g. bump a Prom metric).
	OnPublishError func(err error)
}

// Handler is a slog.Handler fan-out. Exported so callers can type-
// assert if needed ; constructed via NewHandler.
type Handler struct {
	base     slog.Handler
	conn     Publisher
	subject  string
	minLevel slog.Level
	onPubErr func(error)
	// natsHandler renders one record to a NATS payload using the
	// stdlib JSONHandler format so the receiver gets the EXACT same
	// schema as if it had read stderr.
	natsHandler slog.Handler
}

// NewHandler builds the fan-out. Panics on nil Base — see Options.Base
// rationale.
func NewHandler(opts Options) *Handler {
	if opts.Base == nil {
		panic("weftslognats: Options.Base is required")
	}
	if opts.MinLevel == 0 {
		// MinLevel uses slog's zero value (= INFO) when unset ; we
		// override to WARN because that's the floor that matches
		// weft-doctor's ingest filter.
		opts.MinLevel = slog.LevelWarn
	}
	if opts.OnPublishError == nil {
		opts.OnPublishError = func(error) {}
	}
	h := &Handler{
		base:     opts.Base,
		conn:     opts.Conn,
		subject:  opts.Subject,
		minLevel: opts.MinLevel,
		onPubErr: opts.OnPublishError,
	}
	if opts.Conn != nil {
		if opts.Subject == "" {
			panic("weftslognats: Options.Subject required when Conn is set")
		}
		// Reuse stdlib JSONHandler for the NATS payload — same
		// schema as stderr means weft-doctor's ingest.parseSlog has
		// one format to handle.
		h.natsHandler = slog.NewJSONHandler(natsWriter{h: h}, &slog.HandlerOptions{
			Level: opts.MinLevel,
		})
	}
	return h
}

// Enabled : the union of base + nats. Anything either side wants to
// see, we evaluate. In practice base usually allows INFO+ ; nats
// allows WARN+ ; the union is INFO+.
func (h *Handler) Enabled(ctx context.Context, lvl slog.Level) bool {
	if h.base.Enabled(ctx, lvl) {
		return true
	}
	if h.conn != nil && lvl >= h.minLevel {
		return true
	}
	return false
}

// Handle dispatches the record to base (always) and to NATS (when
// conn != nil AND lvl >= minLevel). Errors from base are propagated ;
// NATS publish errors are surfaced via OnPublishError and otherwise
// swallowed — we never want logging itself to fail the caller.
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	baseErr := h.base.Handle(ctx, r)
	if h.conn == nil || r.Level < h.minLevel {
		return baseErr
	}
	// Render via the embedded JSONHandler so the payload matches the
	// stderr schema. The natsWriter side-effects Publish on Write.
	if err := h.natsHandler.Handle(ctx, r); err != nil {
		h.onPubErr(fmt.Errorf("nats handler: %w", err))
	}
	return baseErr
}

// WithAttrs returns a new handler that wraps both sides with the
// same attrs. Required for slog.Logger.With() to compose correctly.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.base = h.base.WithAttrs(attrs)
	if h.natsHandler != nil {
		clone.natsHandler = h.natsHandler.WithAttrs(attrs)
	}
	return &clone
}

// WithGroup is the group equivalent of WithAttrs. Same fan-out shape.
func (h *Handler) WithGroup(name string) slog.Handler {
	clone := *h
	clone.base = h.base.WithGroup(name)
	if h.natsHandler != nil {
		clone.natsHandler = h.natsHandler.WithGroup(name)
	}
	return &clone
}

// natsWriter is the io.Writer slog.JSONHandler emits its serialised
// record to. Each Write becomes one NATS publish ; the handler may
// invoke Write more than once per record (it doesn't, in practice,
// but the contract is per-line so we strip the trailing newline).
type natsWriter struct {
	h *Handler
}

func (w natsWriter) Write(p []byte) (int, error) {
	if w.h.conn == nil {
		return len(p), nil
	}
	// slog.JSONHandler always appends a trailing newline ; the
	// receiver doesn't need it.
	payload := bytes.TrimRight(p, "\n")
	if len(payload) == 0 {
		return len(p), nil
	}
	if err := w.h.conn.Publish(w.h.subject, payload); err != nil {
		w.h.onPubErr(err)
		// Returning the error here would propagate through the
		// JSONHandler back to our Handle, which is fine — but we
		// already report via OnPublishError, so swallow to avoid
		// double-counting.
		return len(p), nil
	}
	return len(p), nil
}

// realPublisher is the production Publisher : a *nats.Conn.
//
// Exported so callers can use FromConn(conn) where they have a
// *nats.Conn (the common case). For test fakes, just implement
// Publisher directly.
type realPublisher struct{ c *nats.Conn }

func (r realPublisher) Publish(subj string, data []byte) error {
	if r.c == nil {
		return errors.New("nats: nil conn")
	}
	return r.c.Publish(subj, data)
}

// FromConn wraps a *nats.Conn into the Publisher interface so the
// common call site (NewHandler(... Conn: weftslognats.FromConn(c)))
// reads naturally.
func FromConn(c *nats.Conn) Publisher {
	if c == nil {
		return nil
	}
	return realPublisher{c: c}
}
