// Package weftclient — eventstream.go provides the shared "open a
// WatchEvents stream + render rows" helper used by `weft events`
// and `weft-microvm events`. Lifts the rendering out so both CLIs print
// the same shape: kind, subject, project, optional meta. Two
// output formats:
//
//   * default (human): tab-separated columns
//
//	2026-05-23T10:23:45.123Z  vm.state.running   alpine       team-alpha   pid=12345
//
//   * --format json: one JSON object per line, jq-friendly
//
// Both formats stream-flush after every event so a piped consumer
// (`weft-microvm events | grep error`) reacts in real time.
package weftclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	weftv1 "github.com/openweft/weft-proto"
)

// EventStreamOptions configures a tail-the-bus session. Filter
// fields are passed verbatim to weft's WatchEvents RPC and
// re-applied server-side; the client doesn't need to filter
// again locally.
type EventStreamOptions struct {
	KindPrefixes []string // server-side wildcard match
	Project      string   // display name or UUID
	Subject      string   // exact match on event.Subject; canonical use: VM name (`--vm NAME`)
	Format       string   // "" → human; "json"
}

// RenderEvent writes one event to `w` in the requested format
// and flushes. When `resolver` is non-nil, the human format swaps
// the raw project UUID for the cached display name; the JSON
// format keeps both. Pass a nil resolver to skip resolution.
//
// Returns the write error verbatim — caller decides whether a
// broken pipe ends the stream.
func RenderEvent(w io.Writer, ev *weftv1.PlatformEvent, format string, resolver *ProjectResolver) error {
	if format == "json" {
		return renderJSON(w, ev, resolver)
	}
	return renderHuman(w, ev, resolver)
}

// renderHuman emits the tab-separated row. Meta is sorted for
// reproducible output (handy in tests + diffs of recorded
// sessions). When `resolver` is non-nil and knows the event's
// project UUID, the row shows the display name; otherwise it
// falls back to the UUID (or "-" for global events).
func renderHuman(w io.Writer, ev *weftv1.PlatformEvent, resolver *ProjectResolver) error {
	ts := time.Unix(0, ev.TsUnixNs).UTC().Format("2006-01-02T15:04:05.000Z")
	project := ev.ProjectUuid
	if resolver != nil && project != "" {
		project = resolver.Name(project)
	}
	if project == "" {
		project = "-"
	}
	subject := ev.Subject
	if subject == "" {
		subject = "-"
	}
	row := fmt.Sprintf("%s\t%s\t%s\t%s", ts, ev.Kind, subject, project)
	if len(ev.Meta) > 0 {
		row += "\t" + formatMeta(ev.Meta)
	}
	if _, err := fmt.Fprintln(w, row); err != nil {
		return err
	}
	// Flush every event so piped tails react in real time.
	if f, ok := w.(interface{ Flush() error }); ok {
		_ = f.Flush()
	}
	return nil
}

// renderJSON emits one JSON object per line. Carries both the
// UUID (stable, programmatic) and the resolved display name when
// available — JSON consumers shouldn't have to guess.
func renderJSON(w io.Writer, ev *weftv1.PlatformEvent, resolver *ProjectResolver) error {
	var projectName string
	if resolver != nil && ev.ProjectUuid != "" {
		if n := resolver.Name(ev.ProjectUuid); n != ev.ProjectUuid {
			projectName = n
		}
	}
	out := struct {
		TS          string            `json:"ts"`
		Kind        string            `json:"kind"`
		Subject     string            `json:"subject,omitempty"`
		ProjectUUID string            `json:"project_uuid,omitempty"`
		Project     string            `json:"project,omitempty"`
		Meta        map[string]string `json:"meta,omitempty"`
	}{
		TS:          time.Unix(0, ev.TsUnixNs).UTC().Format(time.RFC3339Nano),
		Kind:        ev.Kind,
		Subject:     ev.Subject,
		ProjectUUID: ev.ProjectUuid,
		Project:     projectName,
		Meta:        ev.Meta,
	}
	b, err := json.Marshal(out)
	if err != nil {
		return err
	}
	if _, err := w.Write(append(b, '\n')); err != nil {
		return err
	}
	return nil
}

// formatMeta is the human-format renderer for the meta map. Sorts
// keys so repeated runs of the same event print identically — a
// kindness for grep-diff workflows.
func formatMeta(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, len(keys))
	for i, k := range keys {
		pairs[i] = k + "=" + m[k]
	}
	return strings.Join(pairs, " ")
}

// StreamEvents opens a WatchEvents stream and pumps every event
// through RenderEvent. Returns when the stream ends (server-side
// close), the caller cancels ctx, or a render error occurs.
//
// A ProjectResolver is bootstrapped via one ListProjects RPC
// before the first event is received, then updated in-band from
// the `project.*` events that flow through the same stream — so
// a mid-stream rename is reflected in subsequent rows without
// re-polling weft. Bootstrap failures degrade gracefully: rows
// fall back to the raw UUID.
//
// The caller owns the gRPC client; weft / weft-microvm both pass the one
// they already opened for their other subcommands. The bearer
// interceptor in weftclient.Dial stamps the cached OIDC token
// transparently, so authenticated streams "just work" once the
// operator has run `weft login`.
func StreamEvents(ctx context.Context, client weftv1.WeftAgentClient, opts EventStreamOptions, w io.Writer) error {
	resolver := NewProjectResolver()
	resolver.Bootstrap(ctx, client)
	stream, err := client.WatchEvents(ctx, &weftv1.WatchEventsRequest{
		KindPrefix: opts.KindPrefixes,
		Project:    opts.Project,
		Subject:    opts.Subject,
	})
	if err != nil {
		return fmt.Errorf("watch events: %w", err)
	}
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			// Distinguish context cancellation from real errors so
			// `^C` produces a clean exit rather than a noisy
			// "context canceled" line on stderr.
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("recv event: %w", err)
		}
		// Self-update: keep the resolver fresh against project
		// mutations flowing in the same stream. Cheap and lock-
		// scoped; non-project kinds are a no-op.
		resolver.Apply(ev)
		if err := RenderEvent(w, ev, opts.Format, resolver); err != nil {
			return fmt.Errorf("render event: %w", err)
		}
	}
}
