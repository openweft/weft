package uefi

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/openweft/weft/cmd/weft/internal/testutil"
	vzdv1 "github.com/openweft/weft-proto"
)

func strPtr(s string) *string { return &s }

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }()
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	_ = w.Close()
	return <-done
}

func TestCommand_Structure(t *testing.T) {
	cmd := Command(strPtr("/sock"), strPtr(""), strPtr(""))
	if cmd.Use != "uefi" {
		t.Errorf("Use = %q", cmd.Use)
	}
	want := []string{"ls", "set", "rm"}
	got := map[string]bool{}
	for _, c := range cmd.Commands() {
		got[strings.SplitN(c.Use, " ", 2)[0]] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing %q in %v", w, got)
		}
	}
}

func TestLs_TableFormat(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListUEFIVarsFn = func(_ context.Context, _ *vzdv1.ListUEFIVarsRequest) (*vzdv1.ListUEFIVarsResponse, error) {
		return &vzdv1.ListUEFIVarsResponse{Vars: []*vzdv1.UEFIVar{
			{Namespace: efiGlobalNS, Name: "BootOrder", ValueHex: "00000001", Attributes: []string{"NonVolatile"}, UpdatedAt: "2026-05-29T10:00:00Z"},
			{Namespace: efiGlobalNS, Name: "SecureBoot", ValueHex: "", UpdatedAt: "2026-05-29T10:00:00Z"},
		}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"ls", "web-1"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})
	for _, want := range []string{"BootOrder", "00000001", "NonVolatile", "EFI_GLOBAL", "(empty)"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in :\n%s", want, out)
		}
	}
}

func TestSet_DefaultsToEFIGlobalNS(t *testing.T) {
	srv := testutil.NewServer(t)
	var seen *vzdv1.SetUEFIVarRequest
	srv.SetUEFIVarFn = func(_ context.Context, in *vzdv1.SetUEFIVarRequest) (*vzdv1.SetUEFIVarResponse, error) {
		seen = in
		return &vzdv1.SetUEFIVarResponse{Var: in.Var}, nil
	}
	captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"set", "web-1", "BootOrder", "--value", "0000", "--attributes", "NonVolatile,BootServiceAccess"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})
	if seen == nil || seen.Var == nil {
		t.Fatalf("set didn't reach server")
	}
	if seen.Var.Namespace != efiGlobalNS {
		t.Errorf("namespace should default to EFI Global, got %q", seen.Var.Namespace)
	}
	if seen.Var.Name != "BootOrder" || seen.Var.ValueHex != "0000" {
		t.Errorf("fields not forwarded : %+v", seen.Var)
	}
	if len(seen.Var.Attributes) != 2 || seen.Var.Attributes[0] != "NonVolatile" {
		t.Errorf("attributes not split : %v", seen.Var.Attributes)
	}
}

func TestSet_ExplicitNamespace(t *testing.T) {
	srv := testutil.NewServer(t)
	const customNS = "11111111-1111-1111-1111-111111111111"
	var seen *vzdv1.SetUEFIVarRequest
	srv.SetUEFIVarFn = func(_ context.Context, in *vzdv1.SetUEFIVarRequest) (*vzdv1.SetUEFIVarResponse, error) {
		seen = in
		return &vzdv1.SetUEFIVarResponse{Var: in.Var}, nil
	}
	captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"set", "web-1", "Custom", "--value", "01", "--ns", customNS})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})
	if seen.Var.Namespace != customNS {
		t.Errorf("namespace = %q, want %q", seen.Var.Namespace, customNS)
	}
}

func TestRm_DefaultsToEFIGlobalNS(t *testing.T) {
	srv := testutil.NewServer(t)
	var seen *vzdv1.DeleteUEFIVarRequest
	srv.DeleteUEFIVarFn = func(_ context.Context, in *vzdv1.DeleteUEFIVarRequest) (*vzdv1.DeleteUEFIVarResponse, error) {
		seen = in
		return &vzdv1.DeleteUEFIVarResponse{}, nil
	}
	captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"rm", "web-1", "BootOrder"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})
	if seen.Namespace != efiGlobalNS {
		t.Errorf("rm should default ns to EFI Global, got %q", seen.Namespace)
	}
	if seen.Name != "BootOrder" {
		t.Errorf("name not forwarded : %q", seen.Name)
	}
}
