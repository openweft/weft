// VM lifecycle timings — per-VM event log persisted as
// <vmDir>/timings.jsonl.
//
// Goal: give operators (and integrators like nano-container-linux's
// `ncl run`) a single place to inspect when each VM crossed every
// lifecycle boundary — registered, start RPC received, VZ
// configured, VZ started, guest boot markers, shutdown — so they
// can answer "how long did each stage take?" without instrumenting
// every consumer ad hoc.
//
// Format: one JSON object per line, append-only. Each event has:
//
//   { "name": "<stage>", "ts_unix_ns": <int64>, "meta": {...} }
//
// `ts_unix_ns` is wall-clock (time.Now().UnixNano()) so events from
// different processes (vzd server, vz-vm-run subprocess, future
// console watcher) can be merged in absolute order. JSONL is chosen
// over JSON-array so concurrent appenders (server + vz-vm-run +
// console watcher) can write without locking the whole file —
// O_APPEND on POSIX guarantees atomic per-line writes for sub-PIPE_BUF
// payloads, which all our lines are (<512 bytes).
//
// Reading: ReadTimings parses the entire file once. For interactive
// "did the VM cross stage X yet?" queries the caller can stat the
// file size + decode incrementally; deferred to a future iteration
// when an actual consumer needs it.

package weft

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

// TimingEvent is one entry in <vmDir>/timings.jsonl.
type TimingEvent struct {
	// Name is the stage label. Convention:
	//   server-side: lowercase_snake (e.g. "registered", "start_rpc_in")
	//   guest-side : ncl_* prefix      (e.g. "ncl_init_entered")
	// Use server.* / vz.* / guest.* prefixes once the volume grows.
	Name string `json:"name"`

	// TsUnixNano is the wall-clock instant the event was recorded,
	// in nanoseconds since the Unix epoch. Chosen over a monotonic
	// clock so events from independent processes can be ordered
	// relative to each other on the same host.
	TsUnixNano int64 `json:"ts_unix_ns"`

	// Meta carries optional per-event tags. Free-form to keep this
	// schema permissive — the alternative is bumping the struct
	// every time a new dimension appears.
	Meta map[string]string `json:"meta,omitempty"`
}

// timingsFilename is the per-VM events file. Kept private so the
// path scheme stays a vzd implementation detail.
const timingsFilename = "timings.jsonl"

// RecordEvent appends one event to <vmDir>/timings.jsonl. Safe to
// call concurrently with itself (O_APPEND on POSIX is per-write
// atomic for our small payloads) and against ReadTimings (a
// half-written line gets caught + skipped by the parser).
//
// Soft-fail: errors writing are logged but never propagated.
// Lifecycle code MUST NOT abort a real action because timings
// recording hit a disk error — timings are observability, not
// correctness-critical.
func RecordEvent(vmDir, name string, meta map[string]string) {
	if vmDir == "" || name == "" {
		return
	}
	ts := time.Now().UnixNano()
	ev := TimingEvent{
		Name:       name,
		TsUnixNano: ts,
		Meta:       meta,
	}
	b, err := json.Marshal(ev)
	if err == nil {
		b = append(b, '\n')
		if f, openErr := os.OpenFile(filepath.Join(vmDir, timingsFilename),
			os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); openErr == nil {
			_, _ = f.Write(b)
			_ = f.Close()
		}
	}
	// After the durable JSONL append, fan out to the live bus.
	// Hook is set by Adapter.New; nil-safe so unit tests that call
	// RecordEvent without an adapter (timings_test.go) still work.
	if h := busPublishHook.Load(); h != nil {
		(*h)(vmDir, name, ts, meta)
	}
}

// busPublishHook lets the Adapter inject its EventBus.Publish
// behaviour into the package-level RecordEvent without making
// every call site adapter-aware. atomic.Pointer keeps swap-on-
// New cheap + race-free; nil reader = no fan-out.
var busPublishHook atomic.Pointer[func(vmDir, kind string, tsUnixNano int64, meta map[string]string)]

// ReadTimings returns all events recorded for the VM rooted at
// vmDir, in file order (which equals append/wall-clock order).
// Lines that fail to decode (concurrent half-write, future schema
// change, manual edit) are dropped silently rather than failing
// the whole call — the caller wants the timings it CAN see, not
// an all-or-nothing error.
func ReadTimings(vmDir string) ([]TimingEvent, error) {
	path := filepath.Join(vmDir, timingsFilename)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	var out []TimingEvent
	sc := bufio.NewScanner(f)
	// 256 KiB max line — plenty for any single event we'd emit.
	sc.Buffer(make([]byte, 0, 4096), 256*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev TimingEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue // half-written or alien — skip
		}
		out = append(out, ev)
	}
	return out, sc.Err()
}
