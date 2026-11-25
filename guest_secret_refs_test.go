package weft

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	driversAPI "github.com/openweft/weft-drivers"
	guestv1 "github.com/openweft/weft-proto/guestv1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newRefsAdapter() *Adapter {
	a := &Adapter{podCIDs: newPodCIDRegistry()}
	a.initPodSpecs()
	return a
}

func TestValidateGuestSecretRef(t *testing.T) {
	ok := []string{"file:/run/secrets/k", "keychain:svc/acct", "systemd-creds:k"}
	bad := []string{
		"", strings.Repeat("a", MaxGuestSecretRefLen+1), "file:/a\nb", "file:/a\x7f", "no-scheme",
		"env:HOME", "file:relative/k", "file:/a/../b", "file:/a//b", "unknownscheme:x",
	}
	for _, r := range ok {
		if err := ValidateGuestSecretRef(r); err != nil {
			t.Errorf("%q refused: %v", r, err)
		}
	}
	for _, r := range bad {
		if err := ValidateGuestSecretRef(r); err == nil {
			t.Errorf("%q accepted", r)
		}
	}
}

func FuzzValidateGuestSecretRef(f *testing.F) {
	f.Add("file:/run/secrets/k")
	f.Add("")
	f.Add("env:X")
	f.Fuzz(func(t *testing.T, ref string) {
		if ValidateGuestSecretRef(ref) != nil {
			return
		}
		if len(ref) == 0 || len(ref) > MaxGuestSecretRefLen {
			t.Fatalf("accepted a bad length: %d", len(ref))
		}
		for i := 0; i < len(ref); i++ {
			if ref[i] < 0x20 || ref[i] == 0x7f {
				t.Fatalf("accepted a control character in %q", ref)
			}
		}
	})
}

func TestSetListDeleteGuestSecretRef(t *testing.T) {
	for name, a := range map[string]*Adapter{
		"local": newRefsAdapter(),
		"kv": func() *Adapter {
			a := newRefsAdapter()
			kv := NewMemKVStorage("guestsecretrefs")
			a.kvStorageFactory = func(n string) KVStorage {
				if n != guestSecretRefsKVName {
					t.Errorf("store name %q", n)
				}
				return kv
			}
			return a
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := a.SetGuestSecretRef("BAD", "file:/x/k"); err == nil {
				t.Fatal("bad id accepted")
			}
			if _, err := a.SetGuestSecretRef("k", "env:HOME"); err == nil {
				t.Fatal("bad ref accepted")
			}
			v, err := a.SetGuestSecretRef("k", "file:/x/k")
			if err != nil || v != 1 {
				t.Fatalf("first set: %d %v", v, err)
			}
			if v, _ = a.SetGuestSecretRef("k", "file:/x/k2"); v != 2 {
				t.Fatalf("rotation version %d", v)
			}
			m, err := a.ListGuestSecretRefs()
			if err != nil || len(m) != 1 || m["k"].Ref != "file:/x/k2" || m["k"].Version != 2 {
				t.Fatalf("list: %+v %v", m, err)
			}
			if a.DeleteGuestSecretRef("BAD") == nil {
				t.Fatal("bad id deleted")
			}
			if err := a.DeleteGuestSecretRef("k"); err != nil {
				t.Fatal(err)
			}
			if m, _ := a.ListGuestSecretRefs(); len(m) != 0 {
				t.Fatalf("not deleted: %+v", m)
			}
		})
	}
}

func TestSetGuestSecretRefKVErrors(t *testing.T) {
	a := newRefsAdapter()
	a.kvStorageFactory = func(string) KVStorage { return failingKV{NewMemKVStorage("x")} }
	if _, err := a.SetGuestSecretRef("k", "file:/x/k"); err == nil {
		t.Fatal("read error ignored")
	}
	if _, err := a.ListGuestSecretRefs(); err != nil {
		t.Fatalf("list on a store whose GetOne fails: %v", err)
	}
	kv := NewMemKVStorage("x")
	if err := kv.PutOne(context.Background(), "bad", []byte("{not json")); err != nil {
		t.Fatal(err)
	}
	b := newRefsAdapter()
	b.kvStorageFactory = func(string) KVStorage { return kv }
	if m, err := b.ListGuestSecretRefs(); err != nil || len(m) != 0 {
		t.Fatalf("corrupt record listed: %+v %v", m, err)
	}
	if _, err := b.SetGuestSecretRef("bad", "file:/x/k"); err == nil {
		t.Fatal("corrupt previous record ignored")
	}
}

func wgSpec(ref, inline string) *guestv1.PodSpec {
	return &guestv1.PodSpec{Wireguard: &guestv1.WireGuard{PrivateKeyRef: ref, PrivateKey: inline}}
}

func TestPodSecretIDsAndInline(t *testing.T) {
	if PodSecretIDs(nil) != nil || PodCarriesInlineSecret(nil) {
		t.Fatal("nil spec")
	}
	spec := &guestv1.PodSpec{
		Wireguard: &guestv1.WireGuard{PrivateKeyRef: "wg"},
		ShareMounts: []*guestv1.ShareMount{
			{}, {Cubefs: &guestv1.CubeFSMount{AccessKeyRef: "ak", SecretKeyRef: "wg"}},
		},
	}
	ids := PodSecretIDs(spec)
	if len(ids) != 2 || ids[0] != "ak" || ids[1] != "wg" {
		t.Fatalf("ids %v", ids)
	}
	if PodCarriesInlineSecret(spec) {
		t.Fatal("refs reported as inline")
	}
	if !PodCarriesInlineSecret(wgSpec("", "KEY")) {
		t.Fatal("inline wireguard key missed")
	}
	if !PodCarriesInlineSecret(&guestv1.PodSpec{ShareMounts: []*guestv1.ShareMount{{}, {Cubefs: &guestv1.CubeFSMount{AccessKey: "x"}}}}) {
		t.Fatal("inline cubefs key missed")
	}
	if !PodCarriesInlineSecret(&guestv1.PodSpec{ShareMounts: []*guestv1.ShareMount{{Cubefs: &guestv1.CubeFSMount{SecretKey: "x"}}}}) {
		t.Fatal("inline cubefs secret key missed")
	}
}

func TestNeedVsockErr(t *testing.T) {
	for _, k := range []string{"vmd", "wasm"} {
		if err := needVsockErr(k); !errors.Is(err, ErrSecretsNeedVsock) {
			t.Errorf("%s: %v", k, err)
		}
	}
	for _, k := range []string{"", "vz", "qemu"} {
		if needVsockErr(k) != nil {
			t.Errorf("%q refused", k)
		}
	}
}

func TestPublishPodSpecGrantsRevokesAndRefuses(t *testing.T) {
	a := newRefsAdapter()
	if a.PublishPodSpec("", &guestv1.PodSpec{}) == nil {
		t.Fatal("empty pod id accepted")
	}
	// unmapped ID: refused, nothing stored
	if err := a.PublishPodSpec("pod", wgSpec("wg", "")); err == nil {
		t.Fatal("unmapped id accepted")
	}
	if _, ok := a.PodSpec("pod"); ok {
		t.Fatal("a refused spec was stored")
	}
	if err := a.PublishPodSpec("pod", wgSpec("BAD ID", "")); err == nil {
		t.Fatal("invalid id accepted")
	}
	if err := a.PublishPodSpec("pod", wgSpec("wg", "KEY")); err == nil {
		t.Fatal("key together with its ref accepted")
	}
	cubeBoth := &guestv1.PodSpec{ShareMounts: []*guestv1.ShareMount{{Cubefs: &guestv1.CubeFSMount{AccessKey: "x", AccessKeyRef: "ak"}}}}
	if err := a.PublishPodSpec("pod", cubeBoth); err == nil {
		t.Fatal("cubefs key together with its ref accepted")
	}
	cubeBoth2 := &guestv1.PodSpec{ShareMounts: []*guestv1.ShareMount{{}, {Cubefs: &guestv1.CubeFSMount{SecretKey: "x", SecretKeyRef: "sk"}}}}
	if err := a.PublishPodSpec("pod", cubeBoth2); err == nil {
		t.Fatal("cubefs secret key together with its ref accepted")
	}

	// grant on create
	if _, err := a.SetGuestSecretRef("wg", "file:/x/wg"); err != nil {
		t.Fatal(err)
	}
	if err := a.PublishPodSpec("pod", wgSpec("wg", "")); err != nil {
		t.Fatal(err)
	}
	g, ok, err := a.GuestSecretGrantFor("pod", "wg")
	if err != nil || !ok || g.Ref != "file:/x/wg" || g.Version != 1 {
		t.Fatalf("grant: %+v %v %v", g, ok, err)
	}
	// rotation re-grants with the new version
	if _, err := a.SetGuestSecretRef("wg", "file:/x/wg2"); err != nil {
		t.Fatal(err)
	}
	if g, _, _ = a.GuestSecretGrantFor("pod", "wg"); g.Version != 2 || g.Ref != "file:/x/wg2" {
		t.Fatalf("rotated grant: %+v", g)
	}
	// delete of the mapping revokes (fail closed)
	if err := a.DeleteGuestSecretRef("wg"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := a.GuestSecretGrantFor("pod", "wg"); ok {
		t.Fatal("grant survived the deletion of its reference")
	}
	// a spec naming no secret revokes
	if _, err := a.SetGuestSecretRef("wg", "file:/x/wg"); err != nil {
		t.Fatal(err)
	}
	if err := a.PublishPodSpec("pod", wgSpec("wg", "")); err != nil {
		t.Fatal(err)
	}
	if err := a.PublishPodSpec("pod", &guestv1.PodSpec{}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := a.GuestSecretGrantFor("pod", "wg"); ok {
		t.Fatal("grant survived a spec that names none")
	}
	// nil removes the spec
	if err := a.PublishPodSpec("pod", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.PodSpec("pod"); ok {
		t.Fatal("nil did not evict")
	}
}

func TestRegrantPodsUsingOtherSecretsUnaffected(t *testing.T) {
	a := newRefsAdapter()
	a.SetGuestSecretRef("wg", "file:/x/wg")
	a.SetGuestSecretRef("other", "file:/x/o")
	a.PublishPodSpec("p1", wgSpec("wg", ""))
	a.PublishPodSpec("p2", wgSpec("other", ""))
	a.SetGuestSecretRef("wg", "file:/x/wg2")
	if g, _, _ := a.GuestSecretGrantFor("p2", "other"); g.Version != 1 {
		t.Fatalf("unrelated pod re-granted: %+v", g)
	}
	// regrant with no pod registry is a no-op
	(&Adapter{}).regrantPodsUsing("wg")
}

func TestSetPodSpecRefusalKeepsNothing(t *testing.T) {
	a := newRefsAdapter()
	a.SetPodSpec("pod", wgSpec("unmapped", ""))
	if _, ok := a.PodSpec("pod"); ok {
		t.Fatal("refused spec stored through SetPodSpec")
	}
}

func TestBootTokens(t *testing.T) {
	a := &Adapter{}
	if _, ok := a.HeldBootToken("p"); ok {
		t.Fatal("token held on an empty store")
	}
	a.ForgetBootToken("p") // unknown pod: no panic
	tok, err := newBootToken()
	if err != nil || len(tok) != BootTokenLen {
		t.Fatalf("token: %d %v", len(tok), err)
	}
	tok2, _ := newBootToken()
	if bytes.Equal(tok, tok2) {
		t.Fatal("two tokens equal")
	}
	a.putBootToken("p", tok)
	got, ok := a.HeldBootToken("p")
	if !ok || !bytes.Equal(got, tok) {
		t.Fatal("token not held")
	}
	got[0] ^= 0xff // a copy: the store is unaffected
	if again, _ := a.HeldBootToken("p"); !bytes.Equal(again, tok) {
		t.Fatal("HeldBootToken returned the stored slice")
	}
	held := a.bootTokens.m["p"]
	a.putBootToken("p", tok2) // replace wipes the previous one
	if !bytes.Equal(held, make([]byte, BootTokenLen)) {
		t.Fatal("replaced token not wiped")
	}
	held = a.bootTokens.m["p"]
	a.ForgetBootToken("p")
	if _, ok := a.HeldBootToken("p"); ok {
		t.Fatal("token survived Forget")
	}
	if !bytes.Equal(held, make([]byte, BootTokenLen)) {
		t.Fatal("forgotten token not wiped")
	}
}

type plainHyp struct {
	driversAPI.HypervisorDriver
	started int
	err     error
}

func (h *plainHyp) StartVM(context.Context, string) error { h.started++; return h.err }

type tokenHyp struct {
	plainHyp
	got     []byte
	tokenEr error
}

func (h *tokenHyp) StartVMWithBootToken(_ context.Context, _ string, tok []byte) error {
	h.got = append([]byte(nil), tok...)
	return h.tokenEr
}

func TestStartHypervisorBootToken(t *testing.T) {
	ctx := context.Background()
	// no BootTokenStarter: plain start, no token
	a := &Adapter{}
	p := &plainHyp{}
	if err := a.startHypervisor(ctx, p, "vm", "/d"); err != nil || p.started != 1 {
		t.Fatalf("plain: %v %d", err, p.started)
	}
	if _, ok := a.HeldBootToken("vm"); ok {
		t.Fatal("token for a driver that cannot deliver one")
	}
	// token driver: token generated, recorded, delivered
	h := &tokenHyp{}
	if err := a.startHypervisor(ctx, h, "vm", "/d"); err != nil {
		t.Fatal(err)
	}
	held, ok := a.HeldBootToken("vm")
	if !ok || len(h.got) != BootTokenLen || !bytes.Equal(held, h.got) {
		t.Fatal("token not recorded or not delivered")
	}
	// failed start forgets the token
	h2 := &tokenHyp{tokenEr: errors.New("boom")}
	if err := a.startHypervisor(ctx, h2, "vm2", "/d"); err == nil {
		t.Fatal("error swallowed")
	}
	if _, ok := a.HeldBootToken("vm2"); ok {
		t.Fatal("token kept after a failed start")
	}
	// old driver: falls back to a plain start, CID-only
	h3 := &tokenHyp{tokenEr: status.Error(codes.Unimplemented, "old")}
	if err := a.startHypervisor(ctx, h3, "vm3", "/d"); err != nil || h3.started != 1 {
		t.Fatalf("fallback: %v %d", err, h3.started)
	}
	if _, ok := a.HeldBootToken("vm3"); ok {
		t.Fatal("token kept for a driver that refused it")
	}
}
