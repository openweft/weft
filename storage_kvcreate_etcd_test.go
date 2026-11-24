package weft

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

func newEtcdClient(t *testing.T) *clientv3.Client {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping in -short: brings up an embedded etcd")
	}
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{startEmbeddedEtcd(t)},
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("clientv3.New: %v", err)
	}
	t.Cleanup(func() { cli.Close() })
	return cli
}

func TestEtcdKVCreateOne_OnlyOneWinnerPerKey(t *testing.T) {
	cli := newEtcdClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	kv := NewEtcdKVStorageWithClient(cli, "/weft/vsockcids/")

	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := kv.CreateOne(ctx, "70000", []byte("owner-"+strconv.Itoa(i)))
			if err != nil {
				t.Errorf("CreateOne: %v", err)
			}
			if ok {
				wins.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d concurrent creators won the same key, want 1", wins.Load())
	}
	if ok, _ := kv.CreateOne(ctx, "70000", []byte("late")); ok {
		t.Fatal("create over an existing key succeeded")
	}
	// Once deleted the key can be claimed again.
	if err := kv.DeleteOne(ctx, "70000"); err != nil {
		t.Fatal(err)
	}
	if ok, err := kv.CreateOne(ctx, "70000", []byte("again")); err != nil || !ok {
		t.Fatalf("create after delete = %v, %v", ok, err)
	}
}

func TestEtcdKVCreateOne_StoreErrorIsReported(t *testing.T) {
	cli := newEtcdClient(t)
	kv := NewEtcdKVStorageWithClient(cli, "/weft/vsockcids/")
	cli.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// A panic here would mean the error was ignored and the nil response used.
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("CreateOne ignored the store error and panicked: %v", r)
		}
	}()
	ok, err := kv.CreateOne(ctx, "1", []byte("x"))
	if err == nil || ok {
		t.Fatalf("CreateOne on a closed client = %v, %v; want an error", ok, err)
	}
}

// Two agents (two adapters, one etcd) must never hand out the same CID,
// even when their hash slots collide.
func TestClaimVsockCID_ClusterWideThroughEtcd(t *testing.T) {
	cli := newEtcdClient(t)
	kv := NewEtcdKVStorageWithClient(cli, "/weft/vsockcids/")
	mk := func() *Adapter {
		return &Adapter{kvStorageFactory: func(string) KVStorage { return kv }}
	}
	a1, a2 := mk(), mk()
	start := AllocateVsockCID("p", "vm")
	// Agent 2's VM already owns the hash slot of agent 1's VM.
	if ok, err := kv.CreateOne(context.Background(), ckey(start), []byte("q/theirs")); err != nil || !ok {
		t.Fatalf("seed: %v %v", ok, err)
	}
	c1 := a1.claimVsockCID("p", "vm")
	if c1 == start || c1 == 0 {
		t.Fatalf("agent 1 got %d (the other VM holds %d)", c1, start)
	}
	// Agent 2 asking for the same VM gets the same CID: the claim is shared.
	if c2 := a2.claimVsockCID("p", "vm"); c2 != c1 {
		t.Fatalf("agent 2 got %d for the same VM, want %d", c2, c1)
	}
	// After the VM is released on one agent, a different VM can take the slot.
	a2.releaseVsockCID("p", "vm", c1)
	if v, _ := kv.GetOne(context.Background(), ckey(c1)); v != nil {
		t.Fatalf("claim %d survived release", c1)
	}
}
