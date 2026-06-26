package hostmetrics

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

// recorder is a Publisher stub that just buffers payloads. Lets us
// drive Run() with a short interval and assert what landed on the
// subject without touching a real NATS server.
type recorder struct {
	mu   sync.Mutex
	got  [][]byte
	subj string
	fail error // when non-nil, Publish returns this — used to verify error path
}

func (r *recorder) Publish(subject string, data []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return r.fail
	}
	r.subj = subject
	cp := make([]byte, len(data))
	copy(cp, data)
	r.got = append(r.got, cp)
	return nil
}

func (r *recorder) samples(t *testing.T) []Sample {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Sample, 0, len(r.got))
	for _, b := range r.got {
		var s Sample
		if err := json.Unmarshal(b, &s); err != nil {
			t.Fatalf("unmarshal sample: %v\npayload=%q", err, b)
		}
		out = append(out, s)
	}
	return out
}

func TestCPUDeltaPct(t *testing.T) {
	// 100 jiffies elapsed, 30 of them idle → busy = 70% .
	pct := cpuDeltaPct(
		cpuCounters{user: 100, nice: 0, sys: 50, idle: 200},
		cpuCounters{user: 150, nice: 0, sys: 70, idle: 230},
	)
	got := int(pct + 0.5)
	if got != 70 {
		t.Errorf("cpuDeltaPct = %v ; want ~70", pct)
	}
	// Identical readings → 0% (handles the steady-state case where
	// the agent is idle between ticks).
	if v := cpuDeltaPct(cpuCounters{idle: 100}, cpuCounters{idle: 100}); v != 0 {
		t.Errorf("identical cpuDeltaPct = %v ; want 0", v)
	}
	// New < old (counter rollover / reboot) → 0% rather than negative.
	if v := cpuDeltaPct(cpuCounters{user: 500}, cpuCounters{}); v != 0 {
		t.Errorf("rollover cpuDeltaPct = %v ; want 0 (clamp)", v)
	}
}

func TestDeltaPerSec(t *testing.T) {
	// 1024 bytes over 2s = 512 Bps.
	if v := deltaPerSec(0, 1024, 2); v != 512 {
		t.Errorf("deltaPerSec = %v ; want 512", v)
	}
	// Reset (newV < oldV) → 0, not a giant negative.
	if v := deltaPerSec(2000, 1000, 1); v != 0 {
		t.Errorf("reset deltaPerSec = %v ; want 0", v)
	}
	// elapsed=0 → 0 (defensive, callers already clamp).
	if v := deltaPerSec(0, 100, 0); v != 0 {
		t.Errorf("zero elapsed deltaPerSec = %v ; want 0", v)
	}
}

func TestSamplerSubjectDefault(t *testing.T) {
	s := New(nil, "uuid-xyz", "host", Options{})
	want := "weft.host.uuid-xyz.metrics"
	if s.subject != want {
		t.Errorf("subject = %q ; want %q", s.subject, want)
	}
	if s.interval != 5*time.Second {
		t.Errorf("interval = %v ; want 5s", s.interval)
	}
}

func TestSamplerRunPublishes(t *testing.T) {
	r := &recorder{}
	s := New(r, "uuid-1", "host-1", Options{Interval: 50 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	s.Run(ctx)

	got := r.samples(t)
	if len(got) < 2 {
		t.Fatalf("samples published = %d ; want >=2", len(got))
	}
	// All payloads should carry the host identifiers + a monotonically
	// increasing timestamp.
	var lastTs int64
	for i, s := range got {
		if s.HostUUID != "uuid-1" || s.Hostname != "host-1" {
			t.Errorf("sample[%d] identity = %+v ; want uuid-1/host-1", i, s)
		}
		if s.TsUnixNs <= lastTs {
			t.Errorf("sample[%d] ts %d not > %d", i, s.TsUnixNs, lastTs)
		}
		lastTs = s.TsUnixNs
	}
	if r.subj != "weft.host.uuid-1.metrics" {
		t.Errorf("subject = %q ; want weft.host.uuid-1.metrics", r.subj)
	}
}

func TestSamplerNilPublisherIsNoOp(t *testing.T) {
	// Dev-mode path : WEFT_NATS_URL unset, so the caller passes a nil
	// Publisher. Run() must NOT panic + must still respect ctx cancel.
	s := New(nil, "uuid-nil", "host-nil", Options{Interval: 50 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run() did not exit after ctx cancel with nil publisher")
	}
}

func TestSamplerPublishErrorDoesNotKill(t *testing.T) {
	// A transient NATS publish error must NOT take the sampler down :
	// we want to recover on the next tick instead of going silent for
	// the host's lifetime (same lesson as etcdcoord 2026-06-26).
	r := &recorder{fail: errors.New("nats: not connected")}
	s := New(r, "uuid-2", "host-2", Options{Interval: 50 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	s.Run(ctx)
	// We can't observe successful sends ; the assertion is the absence
	// of panic + clean exit on ctx done.
}
