// Package etcdjobs implements an etcd-mediated dispatch queue for
// driver ops the active control-plane can't reach via in-process
// session streams.
//
// Why : every weft agent in a multi-DC cluster shares one etcd, but
// each agent runs in all-in-one mode (no `weft agent --client` →
// no AgentDispatch stream towards another host). When a request
// lands on the VIP holder and asks to RegisterMicroVM on a peer
// host, the in-process sessions[hostUUID] is empty and the call
// fails Unavailable. Per the openweft pull model
// ([[openweft_pull_model]]), cross-daemon traffic goes through
// etcd : the active CP writes the op at /weft/jobs/<host>/<id> ;
// the target host's worker watches its prefix, applies via its
// local Adapter, writes the reply, and deletes the job.
//
// Schema :
//
//	/weft/jobs/<host_uuid>/<job_id>       -> DriverRequest bytes (lease)
//	/weft/jobs/<host_uuid>/<job_id>.reply -> DriverReply   bytes (lease)
//
// Both keys carry the same short lease so a crashed agent / a
// hanging worker doesn't leave stale data forever. The reply key
// remains until the caller deletes it OR the lease expires
// (whichever happens first). The job key is deleted by the worker
// AFTER it has written the reply, so a worker crash between
// "apply" and "delete job" doesn't lose the reply ; the caller's
// watch on the reply key fires regardless.
//
// Idempotency : the underlying ops (RegisterMicroVM, StartVM, …)
// are documented as idempotent in the proto, so an at-least-once
// re-apply on agent restart is safe. The caller observes only the
// first reply it sees ; later duplicates are no-ops on its side.
package etcdjobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/protobuf/proto"

	weftv1 "github.com/openweft/weft-proto"
)

// KeyPrefix is the root of the per-host job tree. Exposed so
// tooling (etcdctl, ops scripts) can inspect or fan-out cleanups
// without re-deriving the prefix.
const KeyPrefix = "/weft/jobs/"

// DefaultLeaseSeconds is the TTL stamped on both the job and the
// reply key. 90s lets a slow op finish (image pulls, kernel boot)
// without leaking stale keys when an agent dies mid-flight.
const DefaultLeaseSeconds = 90

// DefaultPollTimeout is the worst-case wait on the reply key
// before Enqueue gives up. Long enough to cover the heaviest
// RegisterMicroVM cold-boot (first-time OCI pull on the target
// host typically dominates) ; the caller can override with a
// context deadline if they need a shorter ceiling.
const DefaultPollTimeout = 5 * time.Minute

// Handler is the per-host callback that runs the op against the
// local Adapter and returns the reply. Matches the existing
// buildDriverHandler signature in cmd/weft/run_client.go so the
// same closure can serve both the AgentDispatch stream path AND
// this etcd-jobs worker.
type Handler func(context.Context, *weftv1.DriverRequest) *weftv1.DriverReply

// Enqueue submits an op to the target host's queue and blocks
// until the reply key appears (or ctx fires). The DriverReply's
// Error field is set when the target's Handler returned an error ;
// the network/etcd transport itself surfaces as a Go error.
//
// Returns ErrNoReply when the reply key never materialises before
// the lease expires — typically because the target host's worker
// isn't running (agent down, host unreachable, watcher not yet
// up). Callers that need a tighter ceiling should pass a ctx with
// their own deadline.
func Enqueue(ctx context.Context, cli *clientv3.Client, hostUUID string, req *weftv1.DriverRequest) (*weftv1.DriverReply, error) {
	if cli == nil {
		return nil, errors.New("etcdjobs: nil etcd client")
	}
	if hostUUID == "" {
		return nil, errors.New("etcdjobs: empty hostUUID")
	}
	if req == nil {
		return nil, errors.New("etcdjobs: nil DriverRequest")
	}
	jobID, err := randomID()
	if err != nil {
		return nil, fmt.Errorf("etcdjobs: gen job id: %w", err)
	}
	jobKey := jobKeyFor(hostUUID, jobID)
	replyKey := jobKey + ".reply"

	// One lease covers both the request + reply so a hanging
	// worker / lost reply doesn't leak keys indefinitely. The
	// caller can extend by extending its own ctx deadline ; we
	// honor that on the watch but the lease still expires.
	leaseResp, err := cli.Grant(ctx, DefaultLeaseSeconds)
	if err != nil {
		return nil, fmt.Errorf("etcdjobs: grant lease: %w", err)
	}
	leaseID := leaseResp.ID

	// Stamp the request_id BEFORE we wire-format so the receiver
	// sees the same value back in the reply (mirrors the in-process
	// dispatch path's behaviour : the server overrides any
	// caller-set value).
	req.RequestId = jobID

	body, err := proto.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("etcdjobs: marshal request: %w", err)
	}

	if _, err := cli.Put(ctx, jobKey, string(body), clientv3.WithLease(leaseID)); err != nil {
		return nil, fmt.Errorf("etcdjobs: put job: %w", err)
	}

	// Watch the reply key. Get first in case the worker is so
	// fast that the reply lands before we wire the watcher ; the
	// existing-key check covers that race.
	if resp, err := cli.Get(ctx, replyKey); err == nil && len(resp.Kvs) > 0 {
		out, perr := unmarshalReply(resp.Kvs[0].Value)
		_, _ = cli.Delete(ctx, replyKey)
		return out, perr
	} else if err != nil && !errors.Is(err, context.Canceled) {
		// Best-effort cleanup so the worker doesn't apply for nothing.
		_, _ = cli.Delete(ctx, jobKey)
		return nil, fmt.Errorf("etcdjobs: get reply: %w", err)
	}

	wctx, wcancel := context.WithCancel(ctx)
	defer wcancel()
	wch := cli.Watch(wctx, replyKey)
	for {
		select {
		case <-ctx.Done():
			// Caller bailed before reply arrived — try to clean
			// the job key so the worker doesn't pointlessly apply
			// (best effort ; the worker may have already picked it
			// up, in which case the reply will go stale until the
			// lease expires).
			_, _ = cli.Delete(context.Background(), jobKey)
			return nil, ctx.Err()
		case wresp, ok := <-wch:
			if !ok {
				return nil, ErrNoReply
			}
			if wresp.Err() != nil {
				return nil, fmt.Errorf("etcdjobs: watch reply: %w", wresp.Err())
			}
			for _, ev := range wresp.Events {
				if ev.Type != clientv3.EventTypePut {
					continue
				}
				out, perr := unmarshalReply(ev.Kv.Value)
				_, _ = cli.Delete(context.Background(), replyKey)
				return out, perr
			}
		}
	}
}

// RunWorker watches the local host's job queue and dispatches
// every incoming DriverRequest to handler. Blocks until ctx fires.
//
// Recovery on boot : the agent may have died mid-flight with
// unconsumed job keys. The worker fetches the current prefix once,
// applies whatever it finds, then transitions to the watch loop.
// The handler is responsible for being idempotent (the underlying
// Adapter ops are documented as such).
func RunWorker(ctx context.Context, cli *clientv3.Client, hostUUID string, handler Handler) error {
	if cli == nil {
		return errors.New("etcdjobs: nil etcd client")
	}
	if hostUUID == "" {
		return errors.New("etcdjobs: empty hostUUID")
	}
	if handler == nil {
		return errors.New("etcdjobs: nil handler")
	}
	prefix := KeyPrefix + hostUUID + "/"

	// Replay : any job key already present at start-time. Skip
	// reply keys (they're our outputs, not inputs).
	resp, err := cli.Get(ctx, prefix, clientv3.WithPrefix())
	if err != nil {
		return fmt.Errorf("etcdjobs: initial Get: %w", err)
	}
	for _, kv := range resp.Kvs {
		if isReplyKey(string(kv.Key)) {
			continue
		}
		processJob(ctx, cli, string(kv.Key), kv.Value, handler)
	}

	// Steady-state watch from the next revision so we don't
	// re-process the replayed events.
	wch := cli.Watch(ctx, prefix, clientv3.WithPrefix(), clientv3.WithRev(resp.Header.Revision+1))
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case wresp, ok := <-wch:
			if !ok {
				return errors.New("etcdjobs: watch channel closed")
			}
			if err := wresp.Err(); err != nil {
				return fmt.Errorf("etcdjobs: watch error: %w", err)
			}
			for _, ev := range wresp.Events {
				if ev.Type != clientv3.EventTypePut {
					continue
				}
				key := string(ev.Kv.Key)
				if isReplyKey(key) {
					continue
				}
				processJob(ctx, cli, key, ev.Kv.Value, handler)
			}
		}
	}
}

// processJob applies one job and writes the reply. Errors decoding
// the wire payload are surfaced as a synthetic DriverReply.Error so
// the caller's Enqueue sees a clean failure instead of timing out.
func processJob(ctx context.Context, cli *clientv3.Client, jobKey string, value []byte, handler Handler) {
	req := &weftv1.DriverRequest{}
	if err := proto.Unmarshal(value, req); err != nil {
		writeReply(ctx, cli, jobKey, &weftv1.DriverReply{Error: fmt.Sprintf("etcdjobs: unmarshal job: %v", err)})
		// Best-effort delete : a malformed job will never apply,
		// so clearing it now avoids replaying on every restart.
		_, _ = cli.Delete(ctx, jobKey)
		return
	}
	reply := handler(ctx, req)
	if reply == nil {
		reply = &weftv1.DriverReply{RequestId: req.RequestId, Error: "etcdjobs: handler returned nil reply"}
	}
	writeReply(ctx, cli, jobKey, reply)
	// Delete the job AFTER the reply lands so the caller's watch
	// has the reply visible. Order matters : a delete-before-put
	// race would surface as ErrNoReply on the caller side.
	_, _ = cli.Delete(ctx, jobKey)
}

// writeReply puts the marshaled reply under the jobKey + ".reply"
// suffix. Inherits the job key's lease via best-effort lookup so
// the two keys age out together ; on lookup failure we fall back
// to a fresh DefaultLeaseSeconds lease so the reply still gets
// garbage-collected.
func writeReply(ctx context.Context, cli *clientv3.Client, jobKey string, reply *weftv1.DriverReply) {
	body, err := proto.Marshal(reply)
	if err != nil {
		// Replace with a synthetic minimal reply so the caller
		// at least sees an error rather than a stuck watch.
		body, _ = proto.Marshal(&weftv1.DriverReply{Error: fmt.Sprintf("etcdjobs: marshal reply: %v", err)})
	}
	replyKey := jobKey + ".reply"
	// Try to share the job key's lease ; fall back to a fresh one
	// on miss so a fast worker doesn't outlive its caller.
	leaseID := lookupLease(ctx, cli, jobKey)
	if leaseID == 0 {
		leaseResp, lerr := cli.Grant(ctx, DefaultLeaseSeconds)
		if lerr == nil {
			leaseID = leaseResp.ID
		}
	}
	if leaseID != 0 {
		_, _ = cli.Put(ctx, replyKey, string(body), clientv3.WithLease(leaseID))
	} else {
		_, _ = cli.Put(ctx, replyKey, string(body))
	}
}

// lookupLease returns the lease ID attached to key, or 0 when the
// key carries none / is gone. Best-effort ; never an error path.
func lookupLease(ctx context.Context, cli *clientv3.Client, key string) clientv3.LeaseID {
	resp, err := cli.Get(ctx, key)
	if err != nil || len(resp.Kvs) == 0 {
		return 0
	}
	return clientv3.LeaseID(resp.Kvs[0].Lease)
}

// isReplyKey reports whether the key is a reply-suffixed sibling.
// Worker filters these out so it doesn't try to re-apply its own
// outputs.
func isReplyKey(key string) bool {
	return strings.HasSuffix(key, ".reply")
}

// jobKeyFor renders the per-host job key. Exposed via package
// constants so tests + tooling can construct the same path.
func jobKeyFor(hostUUID, jobID string) string {
	return path.Join(KeyPrefix+hostUUID, jobID)
}

// randomID returns a 16-hex job identifier sourced from
// crypto/rand. Short enough for log lines, long enough that
// concurrent enqueues from many CPs don't collide.
func randomID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// unmarshalReply decodes the bytes from etcd back into a typed
// DriverReply, surfacing decode errors as a Go error (rather than
// silently returning an empty reply).
func unmarshalReply(b []byte) (*weftv1.DriverReply, error) {
	out := &weftv1.DriverReply{}
	if err := proto.Unmarshal(b, out); err != nil {
		return nil, fmt.Errorf("etcdjobs: unmarshal reply: %w", err)
	}
	return out, nil
}

// ErrNoReply marks an Enqueue call whose reply key never appeared
// before the watch terminated. Typically the target host's worker
// isn't running ; callers that retry must guard against double
// application (the underlying ops are idempotent).
var ErrNoReply = errors.New("etcdjobs: no reply received before watch ended")
