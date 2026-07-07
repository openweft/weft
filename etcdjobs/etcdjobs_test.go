package etcdjobs

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/server/v3/embed"

	weftv1 "github.com/openweft/weft-proto"
)

// TestEnqueueAndWorker_RoundTrip pins the core happy path :
// Enqueue submits a DriverRequest ; a RunWorker on the SAME host
// UUID picks it up, applies a fixed reply, and the Enqueue caller
// gets that reply back. Validates the encode/decode + the watch +
// the lease wiring end-to-end against a real embed.Etcd.
func TestEnqueueAndWorker_RoundTrip(t *testing.T) {
	cli := embeddedEtcdJobs(t)
	hostUUID := "host-A"

	handler := func(_ context.Context, req *weftv1.DriverRequest) *weftv1.DriverReply {
		return &weftv1.DriverReply{
			RequestId: req.RequestId,
			Result: &weftv1.DriverReply_RegisterMicroVm{
				RegisterMicroVm: &weftv1.RegisterMicroVMResult{},
			},
		}
	}

	workerCtx, workerCancel := context.WithCancel(context.Background())
	defer workerCancel()
	workerDone := make(chan error, 1)
	go func() { workerDone <- RunWorker(workerCtx, cli, hostUUID, handler) }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	op := &weftv1.DriverRequest{Op: &weftv1.DriverRequest_RegisterMicroVm{
		RegisterMicroVm: &weftv1.RegisterMicroVMOp{
			Project: "demo",
			Name:    "vm-a",
		},
	}}
	reply, err := Enqueue(ctx, cli, hostUUID, op)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if reply.Error != "" {
		t.Errorf("reply.Error = %q, want empty", reply.Error)
	}
	if _, ok := reply.Result.(*weftv1.DriverReply_RegisterMicroVm); !ok {
		t.Errorf("reply.Result type = %T, want *DriverReply_RegisterMicroVm", reply.Result)
	}

	workerCancel()
	if err := <-workerDone; err != nil && !errors.Is(err, context.Canceled) {
		t.Errorf("worker exit: %v", err)
	}
}

// TestWorker_HandlerErrorPropagates pins that a handler returning
// an Error-only DriverReply reaches the caller verbatim — so
// transport-layer "no reply" is distinguishable from "agent said
// no" upstream.
func TestWorker_HandlerErrorPropagates(t *testing.T) {
	cli := embeddedEtcdJobs(t)
	hostUUID := "host-err"

	handler := func(_ context.Context, req *weftv1.DriverRequest) *weftv1.DriverReply {
		return &weftv1.DriverReply{
			RequestId: req.RequestId,
			Error:     "RegisterMicroVM: project not found",
		}
	}
	workerCtx, workerCancel := context.WithCancel(context.Background())
	defer workerCancel()
	go func() { _ = RunWorker(workerCtx, cli, hostUUID, handler) }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	reply, err := Enqueue(ctx, cli, hostUUID, &weftv1.DriverRequest{
		Op: &weftv1.DriverRequest_RegisterMicroVm{
			RegisterMicroVm: &weftv1.RegisterMicroVMOp{Project: "missing"},
		},
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if reply.Error != "RegisterMicroVM: project not found" {
		t.Errorf("reply.Error = %q", reply.Error)
	}
}

// TestEnqueue_TimeoutWhenNoWorker proves Enqueue surfaces the
// caller's context cancellation cleanly when no worker is running
// for the target host. Critical : a hung dispatch must not pin
// the install pipeline forever.
func TestEnqueue_TimeoutWhenNoWorker(t *testing.T) {
	cli := embeddedEtcdJobs(t)
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_, err := Enqueue(ctx, cli, "host-with-no-worker", &weftv1.DriverRequest{
		Op: &weftv1.DriverRequest_RegisterMicroVm{
			RegisterMicroVm: &weftv1.RegisterMicroVMOp{Project: "p"},
		},
	})
	if err == nil {
		t.Fatal("expected Enqueue to error on timeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
}

// TestWorker_IsolatesPerHost confirms that worker A only consumes
// jobs whose key prefix matches its host_uuid — a job posted for
// host B doesn't get picked up by host A's worker. The host-level
// isolation is the whole point of the per-host key prefix.
func TestWorker_IsolatesPerHost(t *testing.T) {
	cli := embeddedEtcdJobs(t)
	hostA := "host-A"
	hostB := "host-B"
	var aSeen, bSeen sync.WaitGroup
	aSeen.Add(1)
	bSeen.Add(1)
	aHandler := func(_ context.Context, req *weftv1.DriverRequest) *weftv1.DriverReply {
		aSeen.Done()
		return &weftv1.DriverReply{RequestId: req.RequestId}
	}
	bHandler := func(_ context.Context, req *weftv1.DriverRequest) *weftv1.DriverReply {
		bSeen.Done()
		return &weftv1.DriverReply{RequestId: req.RequestId}
	}
	wctx, wcancel := context.WithCancel(context.Background())
	defer wcancel()
	go func() { _ = RunWorker(wctx, cli, hostA, aHandler) }()
	go func() { _ = RunWorker(wctx, cli, hostB, bHandler) }()
	// Submit one job per host, expect each worker's handler to run
	// exactly once.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, host := range []string{hostA, hostB} {
		if _, err := Enqueue(ctx, cli, host, &weftv1.DriverRequest{
			Op: &weftv1.DriverRequest_StartVm{StartVm: &weftv1.StartVMOp{Name: "vm-" + host}},
		}); err != nil {
			t.Fatalf("Enqueue %s: %v", host, err)
		}
	}
	done := make(chan struct{})
	go func() {
		aSeen.Wait()
		bSeen.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("workers didn't see their respective jobs in time")
	}
}

// embeddedEtcdJobs boots a single-node embed.Etcd on random loopback
// ports + returns a connected client. Same shape as the hostvip test
// harness — verified reliable on CI.
func embeddedEtcdJobs(t *testing.T) *clientv3.Client {
	t.Helper()
	root := filepath.Join(t.TempDir(), "etcd")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	cu := pickURLJobs(t)
	pu := pickURLJobs(t)

	cfg := embed.NewConfig()
	cfg.Name = "etcdjobs-test"
	cfg.Dir = root
	cfg.ListenClientUrls = []url.URL{*cu}
	cfg.AdvertiseClientUrls = []url.URL{*cu}
	cfg.ListenPeerUrls = []url.URL{*pu}
	cfg.AdvertisePeerUrls = []url.URL{*pu}
	cfg.InitialCluster = cfg.Name + "=" + pu.String()
	cfg.InitialClusterToken = "weft-etcdjobs-test"
	cfg.LogLevel = "error"
	cfg.LogOutputs = []string{"stderr"}

	srv, err := embed.StartEtcd(cfg)
	if err != nil {
		t.Fatalf("embed etcd: %v", err)
	}
	t.Cleanup(func() { srv.Close() })
	select {
	case <-srv.Server.ReadyNotify():
	case <-time.After(30 * time.Second):
		t.Fatal("etcd not ready in 30s")
	}
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{cu.String()},
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial etcd: %v", err)
	}
	t.Cleanup(func() { cli.Close() })
	return cli
}

func pickURLJobs(t *testing.T) *url.URL {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port
	u, err := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	return u
}
