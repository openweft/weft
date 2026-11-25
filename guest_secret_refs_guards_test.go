package weft

// Tests that pin the value on both sides of each guard of the guest-secrets
// reference map and its pod checks (found by mutsweep: a guard whose only
// assertion was "err != nil" is held by a neighbour, not by itself).

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	guestv1 "github.com/openweft/weft-proto/guestv1"
)

// faultKV is a memory KV whose chosen operations fail.
type faultKV struct {
	*MemKVStorage
	getErr, putErr, delErr, listErr error
}

func (k faultKV) GetOne(ctx context.Context, key string) ([]byte, error) {
	if k.getErr != nil {
		return nil, k.getErr
	}
	return k.MemKVStorage.GetOne(ctx, key)
}

func (k faultKV) PutOne(ctx context.Context, key string, blob []byte) error {
	if k.putErr != nil {
		return k.putErr
	}
	return k.MemKVStorage.PutOne(ctx, key, blob)
}

func (k faultKV) DeleteOne(ctx context.Context, key string) error {
	if k.delErr != nil {
		return k.delErr
	}
	return k.MemKVStorage.DeleteOne(ctx, key)
}

func (k faultKV) List(ctx context.Context) (map[string][]byte, error) {
	if k.listErr != nil {
		return nil, k.listErr
	}
	return k.MemKVStorage.List(ctx)
}

// noPanic turns a panic into a test failure that names the guard.
func noPanic(t *testing.T, what string, f func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("%s panicked: %v", what, r)
		}
	}()
	f()
}

func wantErrContains(t *testing.T, err error, sub string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), sub) {
		t.Fatalf("error %v, want one containing %q", err, sub)
	}
}

func TestValidateGuestSecretRefBounds(t *testing.T) {
	atMax := "file:/" + strings.Repeat("a", MaxGuestSecretRefLen-len("file:/"))
	if len(atMax) != MaxGuestSecretRefLen {
		t.Fatalf("fixture length %d", len(atMax))
	}
	if err := ValidateGuestSecretRef(atMax); err != nil {
		t.Fatalf("a reference of exactly the maximum length refused: %v", err)
	}
	wantErrContains(t, ValidateGuestSecretRef(atMax+"a"), "must be 1..")
	wantErrContains(t, ValidateGuestSecretRef(""), "must be 1..")

	// control characters: 0x00..0x1f and 0x7f are refused, 0x20 and 0x7e are not
	for _, c := range []string{"\x00", "\x01", "\x1f", "\x7f"} {
		wantErrContains(t, ValidateGuestSecretRef("file:/a"+c+"b"), "control character")
	}
	for _, c := range []string{" ", "~"} {
		if err := ValidateGuestSecretRef("file:/a" + c + "b"); err != nil {
			t.Errorf("printable %q refused: %v", c, err)
		}
	}
}

func TestValidateGuestSecretRefParseAndAllowlist(t *testing.T) {
	// a reference secretsource cannot parse fails in the parser, not in the
	// allowlist
	for _, r := range []string{"env:HOME", "no-scheme", "unknownscheme:x"} {
		err := ValidateGuestSecretRef(r)
		if err == nil {
			t.Fatalf("%q accepted", r)
		}
		if strings.Contains(err.Error(), "is not allowed") {
			t.Errorf("%q: refused by the allowlist, not by the parser: %v", r, err)
		}
		if !strings.HasPrefix(err.Error(), "guest secrets: ") {
			t.Errorf("%q: error not attributed: %v", r, err)
		}
	}
	// the shipped allowlist covers every scheme the parser knows, so the
	// allowlist branch is reached only through a narrower list
	only := map[string]bool{"file": true}
	if err := validateGuestSecretRef("file:/x/k", only); err != nil {
		t.Fatalf("allowed scheme refused: %v", err)
	}
	wantErrContains(t, validateGuestSecretRef("keychain:svc/acct", only), `scheme "keychain" is not allowed`)
	if err := validateGuestSecretRef("keychain:svc/acct", GuestSecretRefSchemes); err != nil {
		t.Fatalf("shipped allowlist refuses keychain: %v", err)
	}
}

func TestSetDeleteListGuestSecretRefStoreErrors(t *testing.T) {
	boom := errors.New("store down")
	mem := NewMemKVStorage("r")
	newAdapter := func(k faultKV) *Adapter {
		a := newRefsAdapter()
		a.kvStorageFactory = func(string) KVStorage { k.MemKVStorage = mem; return k }
		return a
	}

	put := newAdapter(faultKV{putErr: boom})
	if v, err := put.SetGuestSecretRef("k", "file:/x/k"); !errors.Is(err, boom) || v != 0 {
		t.Fatalf("a failed write: version %d, err %v", v, err)
	}
	if all, _ := newAdapter(faultKV{}).ListGuestSecretRefs(); len(all) != 0 {
		t.Fatalf("a failed write left a record: %+v", all)
	}

	ok := newAdapter(faultKV{})
	if _, err := ok.SetGuestSecretRef("k", "file:/x/k"); err != nil {
		t.Fatal(err)
	}
	del := newAdapter(faultKV{delErr: boom})
	if err := del.DeleteGuestSecretRef("k"); !errors.Is(err, boom) {
		t.Fatalf("a failed delete: %v", err)
	}
	if all, _ := ok.ListGuestSecretRefs(); len(all) != 1 {
		t.Fatalf("a failed delete removed the record: %+v", all)
	}

	list := newAdapter(faultKV{listErr: boom})
	if all, err := list.ListGuestSecretRefs(); !errors.Is(err, boom) || all != nil {
		t.Fatalf("a failed list: %+v %v", all, err)
	}
}

func cubeSpec(c *guestv1.CubeFSMount) *guestv1.PodSpec {
	return &guestv1.PodSpec{ShareMounts: []*guestv1.ShareMount{{}, {Cubefs: c}}}
}

func TestPublishPodSpecRefusalReasons(t *testing.T) {
	a := newRefsAdapter()
	a.kindOf = func(string) string { return "" }
	for _, id := range []string{"wg", "ak", "sk"} {
		if _, err := a.SetGuestSecretRef(id, "file:/x/"+id); err != nil {
			t.Fatal(err)
		}
	}
	refused := []struct {
		name string
		spec *guestv1.PodSpec
		want string
	}{
		{"invalid id", wgSpec("BAD ID", ""), "invalid secret id"},
		{"unmapped id", wgSpec("nope", ""), "has no reference"},
		{"wireguard key and ref", wgSpec("wg", "KEY"), "both private_key and private_key_ref"},
		{"cubefs access key and ref", cubeSpec(&guestv1.CubeFSMount{AccessKey: "x", AccessKeyRef: "ak"}), "a key and its _ref together"},
		{"cubefs secret key and ref", cubeSpec(&guestv1.CubeFSMount{SecretKey: "x", SecretKeyRef: "sk"}), "a key and its _ref together"},
	}
	for _, c := range refused {
		t.Run(c.name, func(t *testing.T) {
			wantErrContains(t, a.PublishPodSpec("pod", c.spec), c.want)
			if _, ok := a.PodSpec("pod"); ok {
				t.Fatal("a refused spec was stored")
			}
		})
	}
	accepted := map[string]*guestv1.PodSpec{
		"wireguard ref":           wgSpec("wg", ""),
		"wireguard inline":        wgSpec("", "KEY"),
		"cubefs inline only":      cubeSpec(&guestv1.CubeFSMount{AccessKey: "x", SecretKey: "y"}),
		"cubefs refs only":        cubeSpec(&guestv1.CubeFSMount{AccessKeyRef: "ak", SecretKeyRef: "sk"}),
		"cubefs key, other's ref": cubeSpec(&guestv1.CubeFSMount{AccessKey: "x", SecretKeyRef: "sk"}),
		"cubefs ref, other's key": cubeSpec(&guestv1.CubeFSMount{AccessKeyRef: "ak", SecretKey: "y"}),
		"no secret at all":        {},
	}
	for name, spec := range accepted {
		t.Run(name, func(t *testing.T) {
			if err := a.PublishPodSpec("pod", spec); err != nil {
				t.Fatalf("refused: %v", err)
			}
			if _, ok := a.PodSpec("pod"); !ok {
				t.Fatal("an accepted spec was not stored")
			}
		})
	}
}

func TestPublishPodSpecVsockKinds(t *testing.T) {
	a := newRefsAdapter()
	kind := ""
	a.kindOf = func(string) string { return kind }
	if _, err := a.SetGuestSecretRef("wg", "file:/x/wg"); err != nil {
		t.Fatal(err)
	}
	kind = "vmd"
	// a secret, by reference or inline, is refused on a driver without vsock
	for name, spec := range map[string]*guestv1.PodSpec{"ref": wgSpec("wg", ""), "inline": wgSpec("", "KEY")} {
		if err := a.PublishPodSpec("pod", spec); !errors.Is(err, ErrSecretsNeedVsock) {
			t.Errorf("%s on vmd: %v", name, err)
		}
		if _, ok := a.PodSpec("pod"); ok {
			t.Errorf("%s on vmd: the spec was stored", name)
		}
	}
	// a pod with no secret is not concerned by the driver kind
	if err := a.PublishPodSpec("pod", &guestv1.PodSpec{}); err != nil {
		t.Fatalf("a spec without secrets refused on vmd: %v", err)
	}
	if _, ok := a.PodSpec("pod"); !ok {
		t.Fatal("a spec without secrets was not stored")
	}
	kind = "qemu"
	if err := a.PublishPodSpec("pod", wgSpec("wg", "")); err != nil {
		t.Fatalf("a secret refused on a driver with vsock: %v", err)
	}
}

func TestPublishPodSpecStoreErrors(t *testing.T) {
	boom := errors.New("store down")
	refs := NewMemKVStorage("r")
	grants := NewMemKVStorage("g")
	a := newRefsAdapter()
	a.kindOf = func(string) string { return "" }
	mode := ""
	a.kvStorageFactory = func(name string) KVStorage {
		if name == guestSecretsKVName {
			if mode == "grant" {
				return faultKV{MemKVStorage: grants, putErr: boom}
			}
			return grants
		}
		if mode == "read" {
			return faultKV{MemKVStorage: refs, getErr: boom}
		}
		return refs
	}
	if _, err := a.SetGuestSecretRef("wg", "file:/x/wg"); err != nil {
		t.Fatal(err)
	}
	mode = "read"
	err := a.PublishPodSpec("pod", wgSpec("wg", ""))
	if !errors.Is(err, boom) {
		t.Fatalf("a failed read of the reference: %v", err)
	}
	wantErrContains(t, err, `read the reference of "wg"`)
	mode = "grant"
	if err := a.PublishPodSpec("pod", wgSpec("wg", "")); !errors.Is(err, boom) {
		t.Fatalf("a failed grant write: %v", err)
	}
	if _, ok := a.PodSpec("pod"); ok {
		t.Fatal("a spec whose grants were not stored was kept")
	}
}

func TestStartVMRefusesSecretsOnNoVsockKind(t *testing.T) {
	a := newAdapterForRegistries(t)
	fake := installFakeLocalHypervisor(t, a)
	ref := "https://example.invalid/img.raw"
	seedHTTPSImageInCache(t, a, ref)
	p, _, _ := a.CreateProject("p")
	if err := a.CloneVM(ref, p.UUID, "vm1", nil, io.Discard); err != nil {
		t.Fatalf("CloneVM: %v", err)
	}
	if a.podSpecs == nil {
		a.initPodSpecs()
	}
	kind := ""
	a.kindOf = func(string) string { return kind }
	if _, err := a.SetGuestSecretRef("wg", "file:/x/wg"); err != nil {
		t.Fatal(err)
	}
	if err := a.PublishPodSpec("vm1", wgSpec("wg", "")); err != nil {
		t.Fatal(err)
	}
	kind = "vmd"
	if err := a.StartVM("vm1", ""); !errors.Is(err, ErrSecretsNeedVsock) {
		t.Fatalf("StartVM on a driver without vsock: %v", err)
	}
	if len(fake.startCalls) != 0 {
		t.Fatal("the hypervisor was started despite the refusal")
	}
	kind = ""
	if err := a.StartVM("vm1", ""); err != nil {
		t.Fatalf("StartVM on a driver with vsock: %v", err)
	}
	if len(fake.startCalls) != 1 {
		t.Fatalf("start calls %d, want 1", len(fake.startCalls))
	}
}

func TestBootTokenEntropyFailure(t *testing.T) {
	boom := errors.New("no entropy")
	old := randRead
	t.Cleanup(func() { randRead = old })
	randRead = func([]byte) (int, error) { return 0, boom }
	if tok, err := newBootToken(); !errors.Is(err, boom) || tok != nil {
		t.Fatalf("newBootToken: %v %v", tok, err)
	}
	a := &Adapter{}
	h := &tokenHyp{}
	err := a.startHypervisor(context.Background(), h, "vm", "/d")
	if !errors.Is(err, boom) {
		t.Fatalf("startHypervisor: %v", err)
	}
	wantErrContains(t, err, "boot token: ")
	if h.got != nil || h.started != 0 {
		t.Fatal("the VM was started without a token")
	}
	if _, ok := a.HeldBootToken("vm"); ok {
		t.Fatal("a token is held after the entropy failure")
	}
}

// Guards that the code reaches on a zero Adapter: the value each one
// protects is asserted, so a removed guard fails here, not by a crash.
func TestGuardsOnAZeroAdapter(t *testing.T) {
	a := &Adapter{}
	noPanic(t, "plain hypervisor start", func() {
		p := &plainHyp{}
		if err := a.startHypervisor(context.Background(), p, "vm", "/d"); err != nil || p.started != 1 {
			t.Errorf("plain start: %v %d", err, p.started)
		}
	})
	noPanic(t, "refs without a KV factory", func() {
		if a.guestSecretRefsKV() != nil {
			t.Error("a KV store without a factory")
		}
		if m, err := a.ListGuestSecretRefs(); err != nil || len(m) != 0 {
			t.Errorf("list: %+v %v", m, err)
		}
	})
	noPanic(t, "regrant without a pod registry", func() { a.regrantPodsUsing("wg") })
	noPanic(t, "store a spec without a registry", func() {
		a.setPodSpecKeepGrants("pod", wgSpec("wg", ""))
		a.SetPodSpec("pod", wgSpec("wg", ""))
	})

	b := newRefsAdapter()
	b.setPodSpecKeepGrants("", wgSpec("wg", ""))
	if _, ok := b.podSpecs.m[""]; ok || len(b.podSpecs.m) != 0 {
		t.Fatalf("a spec stored under an empty pod id: %+v", b.podSpecs.m)
	}
	b.setPodSpecKeepGrants("pod", wgSpec("wg", ""))
	if _, ok := b.PodSpec("pod"); !ok {
		t.Fatal("a spec was not stored")
	}
}
