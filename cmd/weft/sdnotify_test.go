package main

import (
	"errors"
	"net"
	"os"
	"testing"
)

func TestSdNotify_NoSocketIsNoop(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	if err := sdNotify("READY=1"); err != nil {
		t.Errorf("empty NOTIFY_SOCKET should be no-op ; got %v", err)
	}
}

func TestSdNotify_SendsPayloadToUnixgramSocket(t *testing.T) {
	dir := t.TempDir()
	sock := dir + "/notify.sock"
	addr, err := net.ResolveUnixAddr("unixgram", sock)
	if err != nil {
		t.Fatal(err)
	}
	server, err := net.ListenUnixgram("unixgram", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	defer os.Remove(sock)

	t.Setenv("NOTIFY_SOCKET", sock)
	if err := sdNotify("READY=1"); err != nil {
		t.Fatalf("sdNotify: %v", err)
	}

	buf := make([]byte, 64)
	n, _, err := server.ReadFromUnix(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := string(buf[:n]); got != "READY=1" {
		t.Errorf("payload=%q ; want READY=1", got)
	}
}

func TestSdNotify_AbstractSocketTranslatesAtPrefix(t *testing.T) {
	// We only assert the dial sees a NUL-prefixed name ; the actual
	// abstract-namespace bind is Linux-only, so we intercept the dial
	// to inspect the address rather than create a real listener.
	t.Setenv("NOTIFY_SOCKET", "@my-abstract")
	var seenAddr string
	old := net2DialUnix
	defer func() { net2DialUnix = old }()
	net2DialUnix = func(network, addr string) (net.Conn, error) {
		seenAddr = addr
		return nil, errors.New("intercepted")
	}
	_ = sdNotify("READY=1")
	if seenAddr != "\x00my-abstract" {
		t.Errorf("abstract dial addr=%q ; want NUL+my-abstract", seenAddr)
	}
}
