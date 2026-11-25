package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-authn/secretsource"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"

	"github.com/openweft/weft"
	guestv1 "github.com/openweft/weft-proto/guestv1"
)

// The real adapter is a backend, and so is the pod-plane lookup.
var (
	_ guestSecretsBackend = (*weft.Adapter)(nil)
	_ podCIDLookup        = (*weft.Adapter)(nil)
)

type fakeSecretsBackend struct {
	mu     sync.Mutex
	pods   map[uint32]string
	grants map[string]map[string]weft.GuestSecretGrant
	err    error
}

func (f *fakeSecretsBackend) PodForCID(cid uint32) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.pods[cid]
	return p, ok
}

func (f *fakeSecretsBackend) GuestSecretGrantFor(pod, id string) (weft.GuestSecretGrant, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return weft.GuestSecretGrant{}, false, f.err
	}
	g, ok := f.grants[pod][id]
	return g, ok, nil
}

type fakeTokens map[string][]byte

func (f fakeTokens) Held(pod string) ([]byte, bool) { t, ok := f[pod]; return t, ok }

type secretsRig struct {
	s     *guestSecretsServer
	b     *fakeSecretsBackend
	audit *bytes.Buffer
	cid   uint32
	clock time.Time
	opens int
}

func newSecretsRig(t *testing.T) *secretsRig {
	t.Helper()
	r := &secretsRig{
		b: &fakeSecretsBackend{
			pods: map[uint32]string{100: "pod-a", 101: "pod-b"},
			grants: map[string]map[string]weft.GuestSecretGrant{
				"pod-a": {"db-pass": {Ref: "file:/x/db", Version: 3}},
				"pod-b": {"other": {Ref: "file:/x/other", Version: 1}},
			},
		},
		audit: &bytes.Buffer{},
		cid:   100,
		clock: time.Unix(1000, 0),
	}
	r.s = newGuestSecretsServer(r.b)
	r.s.audit = r.audit
	r.s.now = func() time.Time { return r.clock }
	r.s.peerCID = func(context.Context) (uint32, bool) { return r.cid, IsGuestCID(r.cid) }
	r.s.open = func(_ context.Context, ref string) ([]byte, error) {
		r.opens++
		return []byte("value-of-" + ref), nil
	}
	return r
}

func wantCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if got := status.Code(err); got != want {
		t.Fatalf("code = %v (%v), want %v", got, err, want)
	}
}

func TestGuestSecretsFetchOK(t *testing.T) {
	r := newSecretsRig(t)
	resp, err := r.s.Fetch(context.Background(), &guestv1.FetchRequest{SecretId: "db-pass"})
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Value) != "value-of-file:/x/db" || resp.Version != 3 || resp.Unchanged {
		t.Fatalf("resp = %+v", resp)
	}
	line := r.audit.String()
	for _, want := range []string{`pod="pod-a"`, "cid=100", `id="db-pass"`, "version=3", "outcome=ok"} {
		if !strings.Contains(line, want) {
			t.Errorf("audit %q lacks %q", line, want)
		}
	}
	if strings.Contains(line, "value-of") {
		t.Errorf("audit leaks the value: %q", line)
	}
}

func TestGuestSecretsUnchangedSkipsResolution(t *testing.T) {
	r := newSecretsRig(t)
	resp, err := r.s.Fetch(context.Background(), &guestv1.FetchRequest{SecretId: "db-pass", KnownVersion: 3})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Unchanged || len(resp.Value) != 0 || resp.Version != 3 || r.opens != 0 {
		t.Fatalf("resp = %+v opens=%d", resp, r.opens)
	}
	resp, err = r.s.Fetch(context.Background(), &guestv1.FetchRequest{SecretId: "db-pass", KnownVersion: 2})
	if err != nil || resp.Unchanged || len(resp.Value) == 0 {
		t.Fatalf("stale known_version must refetch: %+v %v", resp, err)
	}
}

func TestGuestSecretsWrongVM(t *testing.T) {
	r := newSecretsRig(t)
	r.cid = 101 // pod-b asks for pod-a's secret
	_, err := r.s.Fetch(context.Background(), &guestv1.FetchRequest{SecretId: "db-pass"})
	wantCode(t, err, codes.PermissionDenied)
	if r.opens != 0 {
		t.Fatal("resolved a secret for the wrong VM")
	}
}

func TestGuestSecretsUnknownIDSameAsForbidden(t *testing.T) {
	r := newSecretsRig(t)
	_, e1 := r.s.Fetch(context.Background(), &guestv1.FetchRequest{SecretId: "nope"})
	r.cid = 101
	_, e2 := r.s.Fetch(context.Background(), &guestv1.FetchRequest{SecretId: "db-pass"})
	wantCode(t, e1, codes.PermissionDenied)
	wantCode(t, e2, codes.PermissionDenied)
	if e1.Error() != e2.Error() {
		t.Fatalf("denials differ: %q vs %q", e1, e2)
	}
}

func TestGuestSecretsUnboundOrReservedCID(t *testing.T) {
	r := newSecretsRig(t)
	for _, cid := range []uint32{0, 2, 999} {
		r.cid = cid
		_, err := r.s.Fetch(context.Background(), &guestv1.FetchRequest{SecretId: "db-pass"})
		wantCode(t, err, codes.PermissionDenied)
	}
}

type cidAddr uint32

func (cidAddr) Network() string { return "vsock" }
func (cidAddr) String() string  { return "vsock" }
func (c cidAddr) CID() uint32   { return uint32(c) }

type plainAddr struct{}

func (plainAddr) Network() string { return "tcp" }
func (plainAddr) String() string  { return "127.0.0.1:1" }

func TestVsockPeerCID(t *testing.T) {
	ctxWith := func(a net.Addr) context.Context {
		return peer.NewContext(context.Background(), &peer.Peer{Addr: a})
	}
	if _, ok := vsockPeerCID(context.Background()); ok {
		t.Error("no peer accepted")
	}
	if _, ok := vsockPeerCID(ctxWith(nil)); ok {
		t.Error("nil address accepted")
	}
	if _, ok := vsockPeerCID(ctxWith(plainAddr{})); ok {
		t.Error("non-vsock transport accepted")
	}
	if _, ok := vsockPeerCID(ctxWith(cidAddr(2))); ok {
		t.Error("reserved CID accepted")
	}
	if cid, ok := vsockPeerCID(ctxWith(cidAddr(100))); !ok || cid != 100 {
		t.Errorf("guest CID = %d, %v", cid, ok)
	}
}

func TestGuestSecretsDenyBeforeLookup(t *testing.T) {
	r := newSecretsRig(t)
	// The transport says "not a guest" for a CID that IS bound.
	r.s.peerCID = func(context.Context) (uint32, bool) { return 100, false }
	_, err := r.s.Fetch(context.Background(), &guestv1.FetchRequest{SecretId: "db-pass"})
	wantCode(t, err, codes.PermissionDenied)
	if r.opens != 0 {
		t.Fatal("resolved for a non-guest caller")
	}
	// The binding says "unbound" while a table exists under the empty name.
	r = newSecretsRig(t)
	r.b.grants[""] = map[string]weft.GuestSecretGrant{"db-pass": {Ref: "file:/x/db", Version: 1}}
	r.s.peerCID = func(context.Context) (uint32, bool) { return 999, true }
	_, err = r.s.Fetch(context.Background(), &guestv1.FetchRequest{SecretId: "db-pass"})
	wantCode(t, err, codes.PermissionDenied)
	if r.opens != 0 {
		t.Fatal("resolved for an unbound CID")
	}
}

func TestGuestSecretsNilAuditWriter(t *testing.T) {
	r := newSecretsRig(t)
	r.s.audit = nil
	if _, err := r.s.Fetch(context.Background(), &guestv1.FetchRequest{SecretId: "db-pass"}); err != nil {
		t.Fatal(err)
	}
}

func TestGuestSecretsNoVsockPeer(t *testing.T) {
	r := newSecretsRig(t)
	r.s.peerCID = vsockPeerCID // plain context: no peer
	_, err := r.s.Fetch(context.Background(), &guestv1.FetchRequest{SecretId: "db-pass"})
	wantCode(t, err, codes.PermissionDenied)
}

func TestGuestSecretsRevoked(t *testing.T) {
	r := newSecretsRig(t)
	if _, err := r.s.Fetch(context.Background(), &guestv1.FetchRequest{SecretId: "db-pass"}); err != nil {
		t.Fatal(err)
	}
	r.b.mu.Lock()
	delete(r.b.grants, "pod-a") // pod deleted
	r.b.mu.Unlock()
	_, err := r.s.Fetch(context.Background(), &guestv1.FetchRequest{SecretId: "db-pass"})
	wantCode(t, err, codes.PermissionDenied)
}

func TestGuestSecretsTableErrorFailsClosed(t *testing.T) {
	r := newSecretsRig(t)
	r.b.err = errors.New("etcd down")
	_, err := r.s.Fetch(context.Background(), &guestv1.FetchRequest{SecretId: "db-pass"})
	wantCode(t, err, codes.Unavailable)
	if strings.Contains(err.Error(), "etcd") {
		t.Fatalf("error leaks the cause: %v", err)
	}
}

func TestGuestSecretsInvalidRequests(t *testing.T) {
	r := newSecretsRig(t)
	long := strings.Repeat("a", 1<<20) // bomb: 1 MiB id
	for _, id := range []string{"", "UPPER", "-lead", "a b", "a/b", "a\nb", long} {
		_, err := r.s.Fetch(context.Background(), &guestv1.FetchRequest{SecretId: id})
		wantCode(t, err, codes.InvalidArgument)
	}
	_, err := r.s.Fetch(context.Background(), &guestv1.FetchRequest{SecretId: "db-pass", BootToken: make([]byte, 5)})
	wantCode(t, err, codes.InvalidArgument)
	_, err = r.s.Fetch(context.Background(), &guestv1.FetchRequest{SecretId: "db-pass", BootToken: make([]byte, 1<<20)})
	wantCode(t, err, codes.InvalidArgument)
	if r.audit.Len() > 4096 {
		t.Fatalf("audit grew with the request size: %d bytes", r.audit.Len())
	}
}

func TestGuestSecretsBootToken(t *testing.T) {
	r := newSecretsRig(t)
	held := bytes.Repeat([]byte{7}, bootTokenLen)
	r.s.tokens = fakeTokens{"pod-a": held}
	req := func(tok []byte) *guestv1.FetchRequest {
		return &guestv1.FetchRequest{SecretId: "db-pass", BootToken: tok}
	}
	if _, err := r.s.Fetch(context.Background(), req(held)); err != nil {
		t.Fatalf("right token: %v", err)
	}
	wrong := bytes.Repeat([]byte{8}, bootTokenLen)
	_, err := r.s.Fetch(context.Background(), req(wrong))
	wantCode(t, err, codes.PermissionDenied)
	_, err = r.s.Fetch(context.Background(), req(nil))
	wantCode(t, err, codes.PermissionDenied)

	// pod-b holds no token: none accepted, a presented one refused.
	r.cid = 101
	if _, err := r.s.Fetch(context.Background(), &guestv1.FetchRequest{SecretId: "other"}); err != nil {
		t.Fatalf("no token held, none presented: %v", err)
	}
	_, err = r.s.Fetch(context.Background(), &guestv1.FetchRequest{SecretId: "other", BootToken: wrong})
	wantCode(t, err, codes.PermissionDenied)
}

func TestGuestSecretsBadHeldTokenLengthDenies(t *testing.T) {
	r := newSecretsRig(t)
	r.s.tokens = fakeTokens{"pod-a": []byte("short")}
	_, err := r.s.Fetch(context.Background(), &guestv1.FetchRequest{SecretId: "db-pass", BootToken: bytes.Repeat([]byte{1}, bootTokenLen)})
	wantCode(t, err, codes.PermissionDenied)
}

func TestGuestSecretsOversizeValueRefused(t *testing.T) {
	r := newSecretsRig(t)
	r.s.open = func(context.Context, string) ([]byte, error) {
		return make([]byte, maxGuestSecretBytes+1), nil
	}
	_, err := r.s.Fetch(context.Background(), &guestv1.FetchRequest{SecretId: "db-pass"})
	wantCode(t, err, codes.Unavailable)
	r.s.open = func(context.Context, string) ([]byte, error) {
		return make([]byte, maxGuestSecretBytes), nil
	}
	if _, err := r.s.Fetch(context.Background(), &guestv1.FetchRequest{SecretId: "db-pass"}); err != nil {
		t.Fatalf("a value at the cap must pass: %v", err)
	}
}

func TestGuestSecretsResolveErrorsAreUnavailable(t *testing.T) {
	r := newSecretsRig(t)
	for _, e := range []error{secretsource.ErrUnavailable, secretsource.ErrNotFound, errors.New("boom")} {
		e := e
		r.s.open = func(context.Context, string) ([]byte, error) { return []byte("junk"), e }
		_, err := r.s.Fetch(context.Background(), &guestv1.FetchRequest{SecretId: "db-pass"})
		wantCode(t, err, codes.Unavailable)
	}
}

func TestGuestSecretsTimeout(t *testing.T) {
	r := newSecretsRig(t)
	r.s.deadline = 20 * time.Millisecond
	r.s.open = func(ctx context.Context, _ string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	start := time.Now()
	_, err := r.s.Fetch(context.Background(), &guestv1.FetchRequest{SecretId: "db-pass"})
	wantCode(t, err, codes.Unavailable)
	if time.Since(start) > 5*time.Second {
		t.Fatal("deadline not applied")
	}
}

func TestGuestSecretsRateLimit(t *testing.T) {
	r := newSecretsRig(t)
	do := func() error {
		_, err := r.s.Fetch(context.Background(), &guestv1.FetchRequest{SecretId: "db-pass"})
		return err
	}
	for i := 0; i < guestSecretsPerSecond; i++ {
		if err := do(); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	wantCode(t, do(), codes.ResourceExhausted)
	// Another VM is not throttled by this one.
	r.cid = 101
	if _, err := r.s.Fetch(context.Background(), &guestv1.FetchRequest{SecretId: "other"}); err != nil {
		t.Fatalf("other pod throttled: %v", err)
	}
	r.cid = 100
	// Next second: allowed again.
	r.clock = r.clock.Add(time.Second)
	if err := do(); err != nil {
		t.Fatalf("after the window: %v", err)
	}
}

func TestPodLimiterHourlyCap(t *testing.T) {
	now := time.Unix(0, 0)
	l := newPodLimiter(1000, 5, func() time.Time { return now })
	for i := 0; i < 5; i++ {
		if !l.allow("p") {
			t.Fatalf("call %d refused", i)
		}
		now = now.Add(2 * time.Second)
	}
	if l.allow("p") {
		t.Fatal("6th call in the hour allowed")
	}
	now = now.Add(time.Hour)
	if !l.allow("p") {
		t.Fatal("hour window did not reset")
	}
}

func TestWipeSecretsStatsZeroesValue(t *testing.T) {
	resp := &guestv1.FetchResponse{Value: []byte("secret")}
	h := wipeSecretsStats{}
	h.HandleRPC(context.Background(), &stats.InPayload{})
	if string(resp.Value) != "secret" {
		t.Fatal("wiped on a non-OutPayload event")
	}
	h.HandleRPC(context.Background(), &stats.OutPayload{Payload: "not a response"})
	h.HandleRPC(context.Background(), &stats.OutPayload{Payload: resp})
	if !bytes.Equal(resp.Value, make([]byte, 6)) {
		t.Fatalf("value not wiped: %q", resp.Value)
	}
	_ = h.TagConn(context.Background(), &stats.ConnTagInfo{})
	_ = h.TagRPC(context.Background(), &stats.RPCTagInfo{})
	h.HandleConn(context.Background(), &stats.ConnBegin{})
}

func TestOpenSecretsourceCopiesAndRefusesBadRef(t *testing.T) {
	if _, err := openSecretsource(context.Background(), "nonsense"); err == nil {
		t.Fatal("a bad reference resolved")
	}
}

func FuzzGuestSecretsFetchRequest(f *testing.F) {
	f.Add("db-pass", []byte(nil), uint64(0))
	f.Add("", bytes.Repeat([]byte{1}, bootTokenLen), uint64(3))
	f.Add("a\x00b", []byte("x"), uint64(1<<63))
	f.Fuzz(func(t *testing.T, id string, tok []byte, known uint64) {
		r := &secretsRig{
			b: &fakeSecretsBackend{
				pods:   map[uint32]string{100: "pod-a"},
				grants: map[string]map[string]weft.GuestSecretGrant{"pod-a": {"db-pass": {Ref: "file:/x", Version: 3}}},
			},
			audit: &bytes.Buffer{},
		}
		r.s = newGuestSecretsServer(r.b)
		r.s.audit = r.audit
		r.s.peerCID = func(context.Context) (uint32, bool) { return 100, true }
		r.s.open = func(context.Context, string) ([]byte, error) { return []byte("v"), nil }
		resp, err := r.s.Fetch(context.Background(), &guestv1.FetchRequest{SecretId: id, BootToken: tok, KnownVersion: known})
		if err == nil {
			// Success is only possible for the one granted ID with no token.
			if id != "db-pass" || len(tok) != 0 || resp == nil {
				t.Fatalf("allowed id=%q tok=%d", id, len(tok))
			}
		}
		if bytes.Count(r.audit.Bytes(), []byte("\n")) != 1 {
			t.Fatalf("audit lines != 1: %q", r.audit.String())
		}
	})
}
