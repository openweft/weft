package health

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	weftv1 "github.com/openweft/weft-proto"
)

func TestNew_NoneReturnsNil(t *testing.T) {
	p, err := New(nil)
	if err != nil || p != nil {
		t.Errorf("nil HealthProbe → p=%v err=%v ; want nil,nil", p, err)
	}
	p, err = New(&weftv1.HealthProbe{Type: weftv1.HealthProbe_NONE})
	if err != nil || p != nil {
		t.Errorf("NONE → p=%v err=%v ; want nil,nil", p, err)
	}
}

func TestNew_ExecDeferred(t *testing.T) {
	_, err := New(&weftv1.HealthProbe{Type: weftv1.HealthProbe_EXEC})
	if err == nil {
		t.Error("EXEC should error in V0.1 ; got nil")
	}
}

func TestHTTPProbe_2xxAccepted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portStr)

	p, err := New(&weftv1.HealthProbe{
		Type: weftv1.HealthProbe_HTTP, HttpPort: int32(port), HttpPath: "/", TimeoutMs: 500,
	})
	if err != nil {
		t.Fatal(err)
	}
	pass, _, err := p.Attempt(context.Background(), host)
	if err != nil || !pass {
		t.Errorf("Attempt = (%v,%v) ; want (true,nil)", pass, err)
	}
}

func TestHTTPProbe_500FailsByDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portStr)
	p, _ := New(&weftv1.HealthProbe{
		Type: weftv1.HealthProbe_HTTP, HttpPort: int32(port), TimeoutMs: 500,
	})
	pass, _, _ := p.Attempt(context.Background(), host)
	if pass {
		t.Error("500 should fail default 2xx check ; got pass")
	}
}

func TestHTTPProbe_ExplicitStatusOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable) // 503 — would normally fail
	}))
	defer srv.Close()
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portStr)
	p, _ := New(&weftv1.HealthProbe{
		Type: weftv1.HealthProbe_HTTP, HttpPort: int32(port),
		HttpStatusOk: []int32{503}, // explicit override
		TimeoutMs:    500,
	})
	pass, _, err := p.Attempt(context.Background(), host)
	if !pass || err != nil {
		t.Errorf("503 in HttpStatusOk should pass ; got (%v,%v)", pass, err)
	}
}

func TestHTTPProbe_RedirectIsNotFollowed(t *testing.T) {
	hits := int64(0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.Header().Set("Location", "/elsewhere")
		w.WriteHeader(http.StatusFound) // 302 — not 2xx
	}))
	defer srv.Close()
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portStr)
	p, _ := New(&weftv1.HealthProbe{
		Type: weftv1.HealthProbe_HTTP, HttpPort: int32(port), TimeoutMs: 500,
	})
	pass, _, _ := p.Attempt(context.Background(), host)
	if pass {
		t.Error("302 should fail default 2xx check (and not follow redirect)")
	}
	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Errorf("server hit %d times ; redirect was followed", got)
	}
}

func TestHTTPProbe_TimeoutSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portStr)
	p, _ := New(&weftv1.HealthProbe{
		Type: weftv1.HealthProbe_HTTP, HttpPort: int32(port), TimeoutMs: 50,
	})
	pass, _, err := p.Attempt(context.Background(), host)
	if pass {
		t.Error("Slow server should time out ; got pass")
	}
	if !IsTimeout(err) {
		t.Errorf("IsTimeout(%v) = false ; want true", err)
	}
}

func TestTCPProbe_AcceptsOpenPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	p, _ := New(&weftv1.HealthProbe{
		Type: weftv1.HealthProbe_TCP, TcpPort: int32(port), TimeoutMs: 200,
	})
	pass, _, err := p.Attempt(context.Background(), "127.0.0.1")
	if !pass || err != nil {
		t.Errorf("Attempt open port = (%v,%v) ; want (true,nil)", pass, err)
	}
}

func TestTCPProbe_FailsClosedPort(t *testing.T) {
	// Bind + immediately close so we know the port is closed.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	ln.Close()

	p, _ := New(&weftv1.HealthProbe{
		Type: weftv1.HealthProbe_TCP, TcpPort: int32(port), TimeoutMs: 100,
	})
	pass, _, _ := p.Attempt(context.Background(), "127.0.0.1")
	if pass {
		t.Error("Closed port should fail ; got pass")
	}
}

func TestRunner_HealthyAfterSuccessThreshold(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portStr)
	hp := &weftv1.HealthProbe{
		Type:             weftv1.HealthProbe_HTTP,
		HttpPort:         int32(port),
		TimeoutMs:        200,
		PeriodMs:         10,
		FailureThreshold: 2,
		SuccessThreshold: 1,
	}
	probe, _ := New(hp)
	r := NewRunnerFromProto(probe, host, hp)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	deadline := time.After(2 * time.Second)
	for {
		select {
		case res, ok := <-r.Results():
			if !ok {
				t.Fatal("results channel closed before healthy")
			}
			if res.Status == StatusHealthy {
				r.Stop()
				return
			}
		case <-deadline:
			t.Fatal("never went healthy in 2s")
		}
	}
}

func TestRunner_UnhealthyAfterFailureThreshold(t *testing.T) {
	// Bind+close to get a guaranteed-closed port.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	ln.Close()
	hp := &weftv1.HealthProbe{
		Type:             weftv1.HealthProbe_TCP,
		TcpPort:          int32(port),
		TimeoutMs:        50,
		PeriodMs:         10,
		FailureThreshold: 2,
		SuccessThreshold: 1,
	}
	probe, _ := New(hp)
	r := NewRunnerFromProto(probe, "127.0.0.1", hp)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	deadline := time.After(2 * time.Second)
	for {
		select {
		case res, ok := <-r.Results():
			if !ok {
				t.Fatal("results closed before unhealthy")
			}
			if res.Status == StatusUnhealthy {
				r.Stop()
				return
			}
		case <-deadline:
			t.Fatal("never went unhealthy in 2s")
		}
	}
}

func TestRunner_InitialDelayDelaysFirstAttempt(t *testing.T) {
	hits := int64(0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portStr)
	hp := &weftv1.HealthProbe{
		Type: weftv1.HealthProbe_HTTP, HttpPort: int32(port), TimeoutMs: 200,
		PeriodMs:       10,
		InitialDelayMs: 200,
	}
	probe, _ := New(hp)
	r := NewRunnerFromProto(probe, host, hp)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	time.Sleep(80 * time.Millisecond) // well under initial delay
	if got := atomic.LoadInt64(&hits); got != 0 {
		t.Errorf("server hit %d times before initial_delay elapsed ; want 0", got)
	}
	r.Stop()
}

func TestRunner_StopIsIdempotent(t *testing.T) {
	hp := &weftv1.HealthProbe{Type: weftv1.HealthProbe_TCP, TcpPort: 65000, TimeoutMs: 50}
	probe, _ := New(hp)
	r := NewRunnerFromProto(probe, "127.0.0.1", hp)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	r.Stop()
	r.Stop() // must not panic
}

func TestIsTimeout_DistinguishesError(t *testing.T) {
	if IsTimeout(nil) {
		t.Error("nil err → IsTimeout=true")
	}
	if !IsTimeout(context.DeadlineExceeded) {
		t.Error("DeadlineExceeded → IsTimeout=false")
	}
	if IsTimeout(errors.New("connection refused")) {
		t.Error("plain error → IsTimeout=true")
	}
}
