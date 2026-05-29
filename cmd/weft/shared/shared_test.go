// Tests for package shared. The four wrappers (Dial / Client /
// HumanBytes / ProtoStateStr) are thin pass-throughs over vzclient,
// but they still need exercising so the cmd/weft/* tree clears 100 %
// coverage. Renderers and JSON printers eat real proto fixtures.
package shared

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	vzdv1 "github.com/openweft/weft-proto"
)

// captureStdout pipes os.Stdout through an os.Pipe so tests can
// assert on rendered output without each renderer growing a writer
// parameter.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
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

func TestDial_BadSocketReturnsError(t *testing.T) {
	// Bogus path → grpc.WithBlock + 3 s timeout returns within
	// the deadline. We accept either the dial-deadline or a fast
	// "no such file or directory".
	_, err := Dial("/tmp/this-socket-does-not-exist.weft", "", "")
	if err == nil {
		t.Fatal("expected an error dialing a non-existent socket")
	}
}

func TestDial_WithSSHReturnsErrorOnEmptyKey(t *testing.T) {
	// Non-empty sshKey switches to SSH transport; here we just
	// make sure the wrapping logic in Dial sets the option without
	// panicking — the actual SSH dial will fail because there's
	// no SSH server at that path.
	_, err := Dial("/tmp/nope.sock", "/tmp/nope-ssh.sock", "/dev/null")
	if err == nil {
		t.Fatal("expected an error from a nonsense ssh dial")
	}
}

func TestClient_BadSocketReturnsError(t *testing.T) {
	_, _, err := Client("/tmp/this-socket-does-not-exist-2.weft", "", "")
	if err == nil {
		t.Fatal("expected an error dialing a non-existent socket")
	}
}

func TestClient_WithSSHReturnsErrorOnUnreachable(t *testing.T) {
	_, _, err := Client("/tmp/nope.sock", "/tmp/nope-ssh.sock", "/dev/null")
	if err == nil {
		t.Fatal("expected an error from a nonsense ssh client")
	}
}

func TestRenderTable_EmptyAndPopulated(t *testing.T) {
	out := captureStdout(t, func() {
		RenderTable(nil)
	})
	if !strings.Contains(out, "NAME") {
		t.Errorf("empty render missing header: %q", out)
	}
	out = captureStdout(t, func() {
		RenderTable([]*vzdv1.VMInfo{
			{Name: "alpha", State: vzdv1.VMState_VM_STATE_RUNNING, Os: "linux", Cpu: 2, MemMb: 2048, DiskGb: 20, Ip: "10.0.0.1"},
			{Name: "beta", State: vzdv1.VMState_VM_STATE_STOPPED, Os: "linux", Cpu: 1, MemMb: 1024, DiskGb: 10, Ip: ""},
		})
	})
	if !strings.Contains(out, "alpha") || !strings.Contains(out, "beta") {
		t.Errorf("RenderTable: missing rows in %q", out)
	}
}

func TestPrintJSON_FormatsRows(t *testing.T) {
	out := captureStdout(t, func() {
		_ = PrintJSON([]*vzdv1.VMInfo{
			{Name: "alpha", State: vzdv1.VMState_VM_STATE_RUNNING, Os: "linux", Cpu: 2, MemMb: 2048, DiskGb: 20, Ip: "10.0.0.1"},
		})
	})
	if !strings.Contains(out, `"name":"alpha"`) {
		t.Errorf("PrintJSON missing name: %q", out)
	}
	if !strings.Contains(out, `"state":"running"`) {
		t.Errorf("PrintJSON missing state: %q", out)
	}
}

func TestRenderImagesTable(t *testing.T) {
	out := captureStdout(t, func() {
		RenderImagesTable([]*vzdv1.ImageInfo{
			{Name: "ubuntu", Format: "qcow2", Url: "https://example.com/ubuntu.img", SizeBytes: 1024 * 1024 * 512},
		})
	})
	if !strings.Contains(out, "ubuntu") {
		t.Errorf("missing ubuntu in %q", out)
	}
}

func TestPrintImagesJSON(t *testing.T) {
	out := captureStdout(t, func() {
		_ = PrintImagesJSON([]*vzdv1.ImageInfo{
			{Name: "ubuntu", Format: "qcow2", Url: "u", SizeBytes: 42},
		})
	})
	if !strings.Contains(out, `"size_bytes":42`) {
		t.Errorf("missing size: %q", out)
	}
}

func TestHumanBytes(t *testing.T) {
	if got := HumanBytes(0); got != "0 B" {
		t.Errorf("HumanBytes(0) = %q", got)
	}
	if got := HumanBytes(1024 * 1024); !strings.Contains(got, "MiB") {
		t.Errorf("HumanBytes(1MiB) = %q", got)
	}
}

func TestProtoStateStr(t *testing.T) {
	if got := ProtoStateStr(vzdv1.VMState_VM_STATE_RUNNING); got != "running" {
		t.Errorf("ProtoStateStr running = %q", got)
	}
	if got := ProtoStateStr(vzdv1.VMState_VM_STATE_STOPPED); got != "stopped" {
		t.Errorf("ProtoStateStr stopped = %q", got)
	}
}
