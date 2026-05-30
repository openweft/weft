package registermicrovm

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/openweft/weft/cmd/weft/internal/testutil"
	weftv1 "github.com/openweft/weft-proto"
)

func strPtr(s string) *string { return &s }

func TestCommand_Structure(t *testing.T) {
	cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
	if !strings.HasPrefix(cmd.Use, "register-microvm") {
		t.Errorf("Use = %q", cmd.Use)
	}
}

func TestRegisterMicroVM_NeedsBootIsoOrKernel(t *testing.T) {
	cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"vm"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "either --boot-iso or --kernel") {
		t.Errorf("expected either-or error, got %v", err)
	}
}

func TestRegisterMicroVM_BootIsoAndKernelExclusive(t *testing.T) {
	cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"vm", "--boot-iso", "/iso", "--kernel", "/kernel", "--share", "rootfs=/x"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("expected mutually exclusive error, got %v", err)
	}
}

func TestRegisterMicroVM_RequiresShare(t *testing.T) {
	cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"vm", "--boot-iso", "/iso"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "at least one --share") {
		t.Errorf("expected share-required error, got %v", err)
	}
}

func TestRegisterMicroVM_InvalidShareFormat(t *testing.T) {
	cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"vm", "--boot-iso", "/iso", "--share", "no-equals"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected invalid share error")
	}
}

func TestRegisterMicroVM_DialError(t *testing.T) {
	cmd := Command(strPtr("/tmp/nope-rmv.sock"), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"vm", "--boot-iso", "/iso", "--share", "rootfs0=/etc"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected dial error")
	}
}

func TestRegisterMicroVM_SuccessBootISO(t *testing.T) {
	srv := testutil.NewServer(t)
	var got *weftv1.RegisterMicroVMRequest
	srv.RegisterMicroVMFn = func(_ context.Context, in *weftv1.RegisterMicroVMRequest) (*weftv1.RegisterMicroVMResponse, error) {
		got = in
		return &weftv1.RegisterMicroVMResponse{}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"vm-uki", "--boot-iso", "/path/to/iso", "--share", "rootfs0=/data:ro", "--share", "extra=/var/lib/extra"})
	if err := cmd.Execute(); err != nil {
		t.Errorf("Execute: %v", err)
	}
	if got == nil || got.Name != "vm-uki" || got.BootIso != "/path/to/iso" {
		t.Errorf("got = %+v", got)
	}
	if len(got.Shares) != 2 {
		t.Fatalf("shares len = %d", len(got.Shares))
	}
	if !got.Shares[0].ReadOnly {
		t.Errorf("first share should be RO")
	}
	if got.Shares[1].ReadOnly {
		t.Errorf("second share should not be RO")
	}
}

func TestRegisterMicroVM_SuccessKernelMode(t *testing.T) {
	srv := testutil.NewServer(t)
	var got *weftv1.RegisterMicroVMRequest
	srv.RegisterMicroVMFn = func(_ context.Context, in *weftv1.RegisterMicroVMRequest) (*weftv1.RegisterMicroVMResponse, error) {
		got = in
		return &weftv1.RegisterMicroVMResponse{}, nil
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"vm-kernel", "--kernel", "/k", "--initrd", "/i", "--cmdline", "weft.rootfs=virtiofs:r", "--share", "r=/r"})
	if err := cmd.Execute(); err != nil {
		t.Errorf("Execute: %v", err)
	}
	if got.Kernel != "/k" || got.Initrd != "/i" || got.Cmdline != "weft.rootfs=virtiofs:r" {
		t.Errorf("got = %+v", got)
	}
}

func TestRegisterMicroVM_RPCError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.RegisterMicroVMFn = func(_ context.Context, _ *weftv1.RegisterMicroVMRequest) (*weftv1.RegisterMicroVMResponse, error) {
		return nil, errors.New("boom")
	}
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"vm", "--boot-iso", "/iso", "--share", "r=/r"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected rpc error")
	}
}

func TestParseOneShare_MissingTag(t *testing.T) {
	if _, err := parseOneShare("nopath"); err == nil {
		t.Fatal("expected missing-tag error")
	}
}

func TestParseOneShare_EmptyTag(t *testing.T) {
	if _, err := parseOneShare("=/foo"); err == nil {
		t.Fatal("expected empty-tag error")
	}
}

func TestParseOneShare_EmptyPath(t *testing.T) {
	if _, err := parseOneShare("tag="); err == nil {
		t.Fatal("expected missing-path error")
	}
}

func TestParseOneShare_RelativePath(t *testing.T) {
	if _, err := parseOneShare("tag=relative/path"); err == nil {
		t.Fatal("expected absolute-path error")
	}
}

func TestParseOneShare_AbsoluteReadOnly(t *testing.T) {
	share, err := parseOneShare("rootfs0=/data:ro")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if share.Tag != "rootfs0" || share.Path != "/data" || !share.ReadOnly {
		t.Errorf("share = %+v", share)
	}
}

func TestParseOneShare_AbsoluteRW(t *testing.T) {
	share, err := parseOneShare("rootfs0=/data")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if share.ReadOnly {
		t.Error("expected RW")
	}
}

func TestParseShares_PropagatesErrorWithContext(t *testing.T) {
	_, err := parseShares([]string{"good=/x", "bad"})
	if err == nil || !strings.Contains(err.Error(), "bad") {
		t.Errorf("expected wrap of bad entry, got %v", err)
	}
}
