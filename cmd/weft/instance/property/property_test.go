package property

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
	if cmd.Use != "property" {
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

func TestSplitKV(t *testing.T) {
	cases := []struct {
		in, key, value string
		err            bool
	}{
		{"k=v", "k", "v", false},
		{"k=", "k", "", false},
		{"empty=val=ue", "empty", "val=ue", false}, // value half can carry '='
		{"=value", "", "", true},                   // empty key rejected
		{"novalue", "", "", true},                  // missing '='
	}
	for _, c := range cases {
		k, v, err := splitKV(c.in)
		if c.err {
			if err == nil {
				t.Errorf("%q : expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q : %v", c.in, err)
			continue
		}
		if k != c.key || v != c.value {
			t.Errorf("%q : got (%q,%q), want (%q,%q)", c.in, k, v, c.key, c.value)
		}
	}
}

func TestLs_TableFormat(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListVMPropertiesFn = func(_ context.Context, in *vzdv1.ListVMPropertiesRequest) (*vzdv1.ListVMPropertiesResponse, error) {
		if in.VmName != "web-1" {
			t.Errorf("vm name not forwarded : %q", in.VmName)
		}
		return &vzdv1.ListVMPropertiesResponse{Properties: []*vzdv1.VMProperty{
			{Key: "owner", Value: "alice@x", UpdatedAt: "2026-05-29T10:00:00Z"},
			{Key: "exposed", Value: "yes", GuestReadable: true, UpdatedAt: "2026-05-29T10:00:00Z"},
		}}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"ls", "web-1"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})
	for _, want := range []string{"KEY", "owner", "alice@x", "yes"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in :\n%s", want, out)
		}
	}
}

func TestSet_PassesGuestFlag(t *testing.T) {
	srv := testutil.NewServer(t)
	var seen *vzdv1.SetVMPropertyRequest
	srv.SetVMPropertyFn = func(_ context.Context, in *vzdv1.SetVMPropertyRequest) (*vzdv1.SetVMPropertyResponse, error) {
		seen = in
		return &vzdv1.SetVMPropertyResponse{Property: in.Property}, nil
	}
	captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"set", "web-1", "owner=alice@x", "--guest", "--project", "alpha"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})
	if seen == nil || seen.VmName != "web-1" || seen.Project != "alpha" {
		t.Errorf("vm/project not forwarded : %+v", seen)
	}
	if seen.Property == nil || seen.Property.Key != "owner" || seen.Property.Value != "alice@x" || !seen.Property.GuestReadable {
		t.Errorf("property fields not forwarded : %+v", seen.Property)
	}
}

func TestSet_RejectsBadKV(t *testing.T) {
	srv := testutil.NewServer(t)
	cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"set", "web-1", "novalue"})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "key=value") {
		t.Errorf("expected key=value error, got %v", err)
	}
}

func TestRm_PassesKey(t *testing.T) {
	srv := testutil.NewServer(t)
	var seen *vzdv1.DeleteVMPropertyRequest
	srv.DeleteVMPropertyFn = func(_ context.Context, in *vzdv1.DeleteVMPropertyRequest) (*vzdv1.DeleteVMPropertyResponse, error) {
		seen = in
		return &vzdv1.DeleteVMPropertyResponse{}, nil
	}
	out := captureStdout(t, func() {
		cmd := Command(strPtr(srv.Socket()), strPtr(""), strPtr(""))
		cmd.SetArgs([]string{"rm", "web-1", "owner"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})
	if seen == nil || seen.VmName != "web-1" || seen.Key != "owner" {
		t.Errorf("rm did not forward : %+v", seen)
	}
	if !strings.Contains(out, "deleted\tweb-1\towner") {
		t.Errorf("expected 'deleted' line in :\n%s", out)
	}
}
