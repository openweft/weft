package main

// guest_secrets.go implements the GuestSecrets gRPC service (weft-proto
// guestv1, secrets.proto) on the guest-only vsock server.
//
// Trust model. The caller is identified ONLY by the AF_VSOCK CID the
// transport reports, mapped to the pod the host bound to that CID at
// StartVM; the guest's own claims are never read. A per-boot token slot
// (BootTokens) is checked in constant time when the host holds one for
// the pod; on KVM none is held today, so identity is CID-only there.
// What a pod may fetch is the authorization table in cluster state
// (weft.Adapter.GuestSecretGrantFor): the guest names an ID, the HOST
// maps it to a secretsource reference. Everything not explicitly
// allowed is PermissionDenied with one constant message, so an unknown
// ID and a forbidden one are indistinguishable.
//
// The value is resolved at request time, never cached or written to
// disk, and the response buffer is wiped once gRPC has sent it
// (wipeSecretsStats). Every fetch writes one audit line that carries
// the pod, CID, ID, version and outcome, never the value or the token.

import (
	"context"
	"crypto/subtle"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/go-authn/secretsource"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"

	"github.com/openweft/weft"
	guestv1 "github.com/openweft/weft-proto/guestv1"
)

const (
	// maxGuestSecretBytes caps one secret value.
	maxGuestSecretBytes = 64 << 10
	// bootTokenLen is the length of a per-boot token.
	bootTokenLen = 32
	// guestSecretsDeadline bounds one Fetch, resolution included.
	guestSecretsDeadline = 10 * time.Second
	// Per-pod rate limits.
	guestSecretsPerSecond = 10
	guestSecretsPerHour   = 600
)

// Constant denial messages: nothing here depends on the request.
const (
	msgDenied      = "guest secrets: not allowed"
	msgUnavailable = "guest secrets: unavailable"
	msgRate        = "guest secrets: rate limit exceeded"
)

// guestSecretsBackend is the slice of the adapter the service needs.
type guestSecretsBackend interface {
	PodForCID(cid uint32) (string, bool)
	GuestSecretGrantFor(podID, id string) (weft.GuestSecretGrant, bool, error)
}

// BootTokens is the slot for the per-boot token a guest presents. Held
// reports the token the host generated for the pod's current boot
// (memory only); ok=false means none is held (KVM today).
type BootTokens interface {
	Held(podID string) (token []byte, ok bool)
}

// heldBootTokens is the adapter's boot-token store (Adapter.HeldBootToken).
type heldBootTokens interface {
	HeldBootToken(podID string) ([]byte, bool)
}

// bootTokensOf adapts a heldBootTokens to BootTokens.
type bootTokensOf struct{ h heldBootTokens }

func (b bootTokensOf) Held(podID string) ([]byte, bool) { return b.h.HeldBootToken(podID) }

// secretOpener resolves a reference to a value the caller owns.
type secretOpener func(ctx context.Context, ref string) ([]byte, error)

// openSecretsource resolves ref with secretsource and returns a copy,
// zeroing the library's buffer.
func openSecretsource(ctx context.Context, ref string) ([]byte, error) {
	s, err := secretsource.Open(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	out := make([]byte, len(s.Bytes()))
	copy(out, s.Bytes())
	return out, nil
}

type guestSecretsServer struct {
	guestv1.UnimplementedGuestSecretsServer
	backend guestSecretsBackend
	tokens  BootTokens // nil: none held anywhere
	open    secretOpener
	limiter *podLimiter
	audit   io.Writer
	now     func() time.Time
	// deadline bounds one Fetch.
	deadline time.Duration
	// peerCID extracts the caller's CID; tests replace it.
	peerCID func(ctx context.Context) (uint32, bool)
}

func newGuestSecretsServer(b guestSecretsBackend) *guestSecretsServer {
	s := &guestSecretsServer{
		backend:  b,
		open:     openSecretsource,
		audit:    os.Stderr,
		now:      time.Now,
		deadline: guestSecretsDeadline,
		peerCID:  vsockPeerCID,
	}
	s.limiter = newPodLimiter(guestSecretsPerSecond, guestSecretsPerHour, s.clock)
	return s
}

func (s *guestSecretsServer) clock() time.Time { return s.now() }

// vsockPeerCID reads the caller's CID from the transport.
func vsockPeerCID(ctx context.Context) (uint32, bool) {
	pr, ok := peer.FromContext(ctx)
	if !ok || pr.Addr == nil {
		return 0, false
	}
	va, ok := pr.Addr.(interface{ CID() uint32 })
	if !ok {
		return 0, false
	}
	cid := va.CID()
	return cid, IsGuestCID(cid)
}

// Fetch returns the value of one secret the calling VM may read.
func (s *guestSecretsServer) Fetch(ctx context.Context, req *guestv1.FetchRequest) (*guestv1.FetchResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, s.deadline)
	defer cancel()

	cid, ok := s.peerCID(ctx)
	if !ok {
		s.log("", 0, "", 0, "denied:no-guest-caller")
		return nil, status.Error(codes.PermissionDenied, msgDenied)
	}
	pod, ok := s.backend.PodForCID(cid)
	if !ok {
		s.log("", cid, "", 0, "denied:unbound-cid")
		return nil, status.Error(codes.PermissionDenied, msgDenied)
	}
	if !s.limiter.allow(pod) {
		s.log(pod, cid, "", 0, "rate-limited")
		return nil, status.Error(codes.ResourceExhausted, msgRate)
	}
	id := req.GetSecretId()
	if !weft.ValidGuestSecretID(id) {
		s.log(pod, cid, "", 0, "invalid-id")
		return nil, status.Error(codes.InvalidArgument, "guest secrets: invalid secret_id")
	}
	tok := req.GetBootToken()
	if n := len(tok); n != 0 && n != bootTokenLen {
		s.log(pod, cid, id, 0, "invalid-token-length")
		return nil, status.Error(codes.InvalidArgument, "guest secrets: invalid boot_token")
	}
	if !s.tokenOK(pod, tok) {
		s.log(pod, cid, id, 0, "denied:boot-token")
		return nil, status.Error(codes.PermissionDenied, msgDenied)
	}
	grant, ok, err := s.backend.GuestSecretGrantFor(pod, id)
	if err != nil {
		s.log(pod, cid, id, 0, "unavailable:table")
		return nil, status.Error(codes.Unavailable, msgUnavailable)
	}
	if !ok {
		s.log(pod, cid, id, 0, "denied:not-allowed")
		return nil, status.Error(codes.PermissionDenied, msgDenied)
	}
	if k := req.GetKnownVersion(); k != 0 && k == grant.Version {
		s.log(pod, cid, id, grant.Version, "unchanged")
		return &guestv1.FetchResponse{Version: grant.Version, Unchanged: true}, nil
	}
	val, err := s.open(ctx, grant.Ref)
	if err != nil || len(val) > maxGuestSecretBytes {
		clear(val)
		s.log(pod, cid, id, grant.Version, "unavailable:resolve")
		return nil, status.Error(codes.Unavailable, msgUnavailable)
	}
	s.log(pod, cid, id, grant.Version, "ok")
	return &guestv1.FetchResponse{Value: val, Version: grant.Version}, nil
}

// tokenOK applies the boot-token rule: when the host holds a token for
// the pod the request must carry exactly it (constant-time compare);
// when none is held a request that presents one is refused, since the
// host cannot vouch for it.
func (s *guestSecretsServer) tokenOK(pod string, presented []byte) bool {
	var held []byte
	var ok bool
	if s.tokens != nil {
		held, ok = s.tokens.Held(pod)
	}
	if !ok {
		return len(presented) == 0
	}
	return len(held) == bootTokenLen && len(presented) == bootTokenLen &&
		subtle.ConstantTimeCompare(held, presented) == 1
}

// log writes one audit line. id is already validated or empty, so it
// cannot carry control characters.
func (s *guestSecretsServer) log(pod string, cid uint32, id string, version uint64, outcome string) {
	if s.audit == nil {
		return
	}
	fmt.Fprintf(s.audit, "weft: guest-secrets audit: time=%s pod=%q cid=%d id=%q version=%d outcome=%s\n",
		s.now().UTC().Format(time.RFC3339), pod, cid, id, version, outcome)
}

// podLimiter is a per-pod fixed-window limiter: perSec per second and
// perHour per hour. Entries exist only for pods already authenticated by
// CID, so the map is bounded by the VM count.
type podLimiter struct {
	mu      sync.Mutex
	perSec  int
	perHour int
	now     func() time.Time
	m       map[string]*podWindow
}

type podWindow struct {
	secStart, hourStart time.Time
	sec, hour           int
}

func newPodLimiter(perSec, perHour int, now func() time.Time) *podLimiter {
	return &podLimiter{perSec: perSec, perHour: perHour, now: now, m: make(map[string]*podWindow)}
}

func (l *podLimiter) allow(pod string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.now()
	w := l.m[pod]
	if w == nil {
		w = &podWindow{secStart: t, hourStart: t}
		l.m[pod] = w
	}
	if t.Sub(w.secStart) >= time.Second {
		w.secStart, w.sec = t, 0
	}
	if t.Sub(w.hourStart) >= time.Hour {
		w.hourStart, w.hour = t, 0
	}
	if w.sec >= l.perSec || w.hour >= l.perHour {
		return false
	}
	w.sec++
	w.hour++
	return true
}

// wipeSecretsStats zeroes a FetchResponse's value once gRPC has sent
// it. (The serialized copy lives in gRPC's pooled buffers, which cannot
// be reached from here.)
type wipeSecretsStats struct{}

func (wipeSecretsStats) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context { return ctx }
func (wipeSecretsStats) HandleRPC(_ context.Context, s stats.RPCStats) {
	if out, ok := s.(*stats.OutPayload); ok {
		if resp, ok := out.Payload.(*guestv1.FetchResponse); ok {
			clear(resp.Value)
		}
	}
}
func (wipeSecretsStats) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}
func (wipeSecretsStats) HandleConn(context.Context, stats.ConnStats) {}
