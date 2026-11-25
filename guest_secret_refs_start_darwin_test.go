//go:build darwin

package weft

// StartVM refuses a pod that carries a secret on a driver kind without a
// vsock channel. It lives apart because the lifecycle fixtures
// (adapter_lifecycle_test.go) are darwin-only.

import (
	"errors"
	"io"
	"testing"
)

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
