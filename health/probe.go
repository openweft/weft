// Package health implements the active-probe runners that back
// SchedulingRule.RespawnPolicy on the weft agent side. A Probe is a
// stateless network check pointed at a single VM's IP — HTTP or TCP for
// V0.1 ; EXEC defers to V0.2 because it needs an in-VM agent channel
// (gRPC over WireGuard) the respawn loop doesn't have yet.
//
// The package is pure-Go, CGO=0, no platform deps : the respawn
// reconciler embeds a Runner per probed VM and consumes its Result
// stream. It has no opinion on what to do with a verdict — `Healthy`
// vs `Unhealthy` is just a fact the respawn state machine acts on.
package health

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	weftv1 "github.com/openweft/weft-proto"
)

// Status is the verdict of one probe attempt OR the rolling health
// derived from a window of attempts.
//
// The two are distinguished by who reports : a single attempt is a
// Result with status Pass/Fail ; the Runner derives Healthy/Unhealthy
// by stacking N consecutive Pass/Fail and consuming the proto's
// failure_threshold / success_threshold knobs.
type Status int

const (
	StatusUnknown   Status = iota // before initial_delay elapses
	StatusHealthy                 // success_threshold consecutive Pass
	StatusUnhealthy               // failure_threshold consecutive Fail
)

func (s Status) String() string {
	switch s {
	case StatusHealthy:
		return "healthy"
	case StatusUnhealthy:
		return "unhealthy"
	default:
		return "unknown"
	}
}

// Result is one full state observation emitted by Runner after every
// attempt. Latency carries the attempt's wall-clock duration even on
// failure (timeouts surface here as Latency≈Timeout).
type Result struct {
	When    time.Time
	Status  Status
	Pass    bool
	Latency time.Duration
	Err     error
}

// Probe runs a single attempt of the configured check against addr and
// returns its raw pass/fail + reason. Pure side-effect-free apart from
// the one network call. The Runner calls this on its own schedule and
// folds attempts into the rolling Status.
type Probe interface {
	Attempt(ctx context.Context, addr string) (pass bool, latency time.Duration, err error)
	Kind() weftv1.HealthProbe_Type
}

// New translates a proto HealthProbe into a runnable Probe. Returns nil
// for HealthProbe_NONE so a caller can treat "no probe configured" as
// "skip the runner" without a type assertion. Unsupported types
// (EXEC in V0.1) return an error rather than a silently nil-Probe so
// the operator sees the misconfiguration immediately.
func New(p *weftv1.HealthProbe) (Probe, error) {
	if p == nil || p.GetType() == weftv1.HealthProbe_NONE {
		return nil, nil
	}
	switch p.GetType() {
	case weftv1.HealthProbe_HTTP:
		return newHTTP(p)
	case weftv1.HealthProbe_TCP:
		return newTCP(p)
	case weftv1.HealthProbe_EXEC:
		return nil, fmt.Errorf("health: EXEC probe deferred to V0.2 (no in-VM agent channel yet)")
	default:
		return nil, fmt.Errorf("health: unknown probe type %v", p.GetType())
	}
}

// ---- HTTP ---------------------------------------------------------

type httpProbe struct {
	path       string
	port       int
	method     string
	statusOK   map[int32]struct{}
	timeout    time.Duration
	client     *http.Client
	transport  *http.Transport
	defaultOK2 bool // when statusOK is empty, accept any 2xx
}

func newHTTP(p *weftv1.HealthProbe) (Probe, error) {
	if p.GetHttpPort() <= 0 {
		return nil, fmt.Errorf("HTTP probe : http_port must be >0")
	}
	path := p.GetHttpPath()
	if path == "" {
		path = "/"
	}
	method := p.GetHttpMethod()
	if method == "" {
		method = http.MethodGet
	}
	timeout := time.Duration(p.GetTimeoutMs()) * time.Millisecond
	if timeout <= 0 {
		timeout = time.Second
	}
	// Per-Runner Transport so connection pools don't bleed between
	// VMs the respawn loop watches concurrently. DisableKeepAlives
	// because a healthy peer + frequent probes would otherwise pin
	// idle FDs to every VM.
	tr := &http.Transport{
		DisableKeepAlives: true,
		MaxIdleConns:      1,
		IdleConnTimeout:   timeout,
	}
	hp := &httpProbe{
		path:       path,
		port:       int(p.GetHttpPort()),
		method:     method,
		timeout:    timeout,
		transport:  tr,
		defaultOK2: len(p.GetHttpStatusOk()) == 0,
		statusOK:   make(map[int32]struct{}, len(p.GetHttpStatusOk())),
	}
	for _, c := range p.GetHttpStatusOk() {
		hp.statusOK[c] = struct{}{}
	}
	hp.client = &http.Client{
		Transport: tr,
		Timeout:   timeout,
		// Don't follow redirects : we're checking whether *this*
		// endpoint is alive, not whatever it forwards to.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return hp, nil
}

func (h *httpProbe) Kind() weftv1.HealthProbe_Type { return weftv1.HealthProbe_HTTP }

func (h *httpProbe) Attempt(ctx context.Context, addr string) (bool, time.Duration, error) {
	url := "http://" + net.JoinHostPort(addr, strconv.Itoa(h.port)) + h.path
	req, err := http.NewRequestWithContext(ctx, h.method, url, nil)
	if err != nil {
		return false, 0, fmt.Errorf("build request : %w", err)
	}
	start := time.Now()
	resp, err := h.client.Do(req)
	latency := time.Since(start)
	if err != nil {
		return false, latency, err
	}
	defer resp.Body.Close()
	if h.defaultOK2 {
		return resp.StatusCode >= 200 && resp.StatusCode < 300, latency, nil
	}
	_, ok := h.statusOK[int32(resp.StatusCode)]
	if !ok {
		return false, latency, fmt.Errorf("HTTP %d not in accepted set", resp.StatusCode)
	}
	return true, latency, nil
}

// ---- TCP ----------------------------------------------------------

type tcpProbe struct {
	port    int
	timeout time.Duration
}

func newTCP(p *weftv1.HealthProbe) (Probe, error) {
	if p.GetTcpPort() <= 0 {
		return nil, fmt.Errorf("TCP probe : tcp_port must be >0")
	}
	timeout := time.Duration(p.GetTimeoutMs()) * time.Millisecond
	if timeout <= 0 {
		timeout = time.Second
	}
	return &tcpProbe{port: int(p.GetTcpPort()), timeout: timeout}, nil
}

func (t *tcpProbe) Kind() weftv1.HealthProbe_Type { return weftv1.HealthProbe_TCP }

func (t *tcpProbe) Attempt(ctx context.Context, addr string) (bool, time.Duration, error) {
	dialer := net.Dialer{Timeout: t.timeout}
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	start := time.Now()
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(addr, strconv.Itoa(t.port)))
	latency := time.Since(start)
	if err != nil {
		return false, latency, err
	}
	_ = conn.Close()
	return true, latency, nil
}

// ---- Runner -------------------------------------------------------

// Runner periodically calls Probe.Attempt against a target IP and
// emits a Result on every attempt. Consecutive Pass/Fail counts roll
// into Status per the proto's failure_threshold / success_threshold
// knobs. The InitialDelay is honoured exactly once at start.
//
// The Runner is started/stopped by the respawn reconciler — one per
// VM being watched. Stop() drains the goroutine without losing the
// final Result.
type Runner struct {
	probe   Probe
	addr    string
	period  time.Duration
	initial time.Duration

	failureK int
	successK int

	results chan Result
	stop    chan struct{}
	once    sync.Once

	// for tests : injectable clock + counter (no time.Now in
	// hot path), bumped from Loop's runOnce.
	now func() time.Time

	mu      sync.Mutex
	curr    Status
	consecP int
	consecF int
}

// RunnerOptions mirrors the proto knobs the runner reads, made
// explicit so a caller can build one without a *weftv1.HealthProbe
// (tests, custom integrations).
type RunnerOptions struct {
	Period           time.Duration
	InitialDelay     time.Duration
	FailureThreshold int
	SuccessThreshold int
	Now              func() time.Time // optional ; defaults to time.Now
}

// NewRunner builds a runner for probe at addr. ResultBuffer = 32 is a
// safe ceiling : with period defaults of 1s and the reconciler
// consuming each Result, the channel is normally empty.
func NewRunner(probe Probe, addr string, opts RunnerOptions) *Runner {
	if opts.Period <= 0 {
		opts.Period = time.Second
	}
	if opts.FailureThreshold <= 0 {
		opts.FailureThreshold = 3
	}
	if opts.SuccessThreshold <= 0 {
		opts.SuccessThreshold = 1
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Runner{
		probe:    probe,
		addr:     addr,
		period:   opts.Period,
		initial:  opts.InitialDelay,
		failureK: opts.FailureThreshold,
		successK: opts.SuccessThreshold,
		results:  make(chan Result, 32),
		stop:     make(chan struct{}),
		now:      now,
		curr:     StatusUnknown,
	}
}

// NewRunnerFromProto is the sugar most callers use : map the proto
// knobs onto RunnerOptions in one call.
func NewRunnerFromProto(probe Probe, addr string, p *weftv1.HealthProbe) *Runner {
	return NewRunner(probe, addr, RunnerOptions{
		Period:           time.Duration(p.GetPeriodMs()) * time.Millisecond,
		InitialDelay:     time.Duration(p.GetInitialDelayMs()) * time.Millisecond,
		FailureThreshold: int(p.GetFailureThreshold()),
		SuccessThreshold: int(p.GetSuccessThreshold()),
	})
}

// Results returns the channel Result observations land on. The
// channel is closed when the Runner exits.
func (r *Runner) Results() <-chan Result { return r.results }

// Status returns the rolling status the Runner has observed. Cheap +
// safe to call from any goroutine.
func (r *Runner) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.curr
}

// Run blocks until ctx is cancelled OR Stop() is called. The Result
// channel is closed when Run returns. Idempotent : a second Stop()
// is safe.
func (r *Runner) Run(ctx context.Context) {
	defer close(r.results)
	if r.initial > 0 {
		select {
		case <-time.After(r.initial):
		case <-ctx.Done():
			return
		case <-r.stop:
			return
		}
	}
	t := time.NewTicker(r.period)
	defer t.Stop()
	// Fire one attempt immediately so the first Result lands without
	// waiting a full period.
	r.runOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stop:
			return
		case <-t.C:
			r.runOnce(ctx)
		}
	}
}

// Stop signals the Runner to exit. Safe to call multiple times.
func (r *Runner) Stop() { r.once.Do(func() { close(r.stop) }) }

func (r *Runner) runOnce(ctx context.Context) {
	pass, latency, err := r.probe.Attempt(ctx, r.addr)
	r.mu.Lock()
	if pass {
		r.consecP++
		r.consecF = 0
		if r.consecP >= r.successK {
			r.curr = StatusHealthy
		}
	} else {
		r.consecF++
		r.consecP = 0
		if r.consecF >= r.failureK {
			r.curr = StatusUnhealthy
		}
	}
	status := r.curr
	r.mu.Unlock()
	res := Result{
		When:    r.now(),
		Status:  status,
		Pass:    pass,
		Latency: latency,
		Err:     err,
	}
	// Non-blocking send : a consumer that wedged would otherwise
	// freeze every probe in the agent. The respawn loop reads
	// promptly ; drops here are a deliberate safety valve.
	select {
	case r.results <- res:
	default:
	}
}

// IsTimeout helps callers distinguish "probe never reached the VM"
// from "VM said no". Both fail, but the respawn loop's grace_period
// is meant for the former and ignored for the latter.
func IsTimeout(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded)
}
