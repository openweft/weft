package main

// sdnotify.go is a 60-line inline implementation of the systemd
// sd_notify(3) protocol — the AF_UNIX datagram-based liveness signal
// the agent uses to tell systemd "I'm ready" + "I'm still alive". We
// roll our own instead of pulling github.com/coreos/go-systemd to
// keep the weft binary dep-light and CGO=0 across every platform
// (sd_notify is a thin wire protocol, not worth a transitive dep).
//
// Wire protocol :
//   - $NOTIFY_SOCKET is set by systemd when the unit's
//     `NotifyAccess=main` is in effect.
//   - The socket is a unix-dgram address (path or abstract).
//   - Notifications are newline-separated key=value pairs in the
//     payload. We use just READY=1 (init done) + WATCHDOG=1 (alive).
//
// Off-systemd hosts (dev, tests) have an empty $NOTIFY_SOCKET ; every
// function in this file is a clean no-op when that's the case.

import (
	"context"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// sdNotify sends a single sd_notify state string to systemd. Returns
// nil + no-op when $NOTIFY_SOCKET is unset (off-systemd dev paths) so
// callers can call it unconditionally.
func sdNotify(state string) error {
	sock := os.Getenv("NOTIFY_SOCKET")
	if sock == "" {
		return nil
	}
	// Abstract namespace : prefix '@' translates to NUL on the wire.
	net := "unixgram"
	if strings.HasPrefix(sock, "@") {
		sock = "\x00" + sock[1:]
	}
	conn, err := net2DialUnix(net, sock)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write([]byte(state))
	return err
}

// net2DialUnix is a tiny indirection so tests can stub the dial.
var net2DialUnix = func(network, addr string) (net.Conn, error) {
	return net.Dial(network, addr)
}

// sdNotifyReady tells systemd the service has finished initialising
// (listener bound, watchers wired). Type=notify units stay in
// `activating` until READY=1 lands ; we call this from main.go as the
// last step of agent boot.
func sdNotifyReady() {
	if err := sdNotify("READY=1"); err != nil {
		log.Printf("sd_notify READY=1 failed (continuing): %v", err)
	}
}

// startWatchdog launches a ticker goroutine that pings WATCHDOG=1
// every interval until ctx is cancelled. interval should be roughly
// half of the unit's WatchdogSec so a single missed tick doesn't
// cause a kill. With WatchdogSec=30s, interval=10s gives 3× headroom.
//
// Returns immediately ; never blocks. A no-op when $NOTIFY_SOCKET is
// unset or WATCHDOG_USEC is missing (systemd sets the latter only when
// WatchdogSec is configured on the unit).
func startWatchdog(ctx context.Context, logger *log.Logger) {
	if os.Getenv("NOTIFY_SOCKET") == "" {
		return
	}
	usec := os.Getenv("WATCHDOG_USEC")
	if usec == "" {
		return // unit didn't request watchdog
	}
	micro, err := strconv.ParseInt(usec, 10, 64)
	if err != nil || micro <= 0 {
		logger.Printf("sd_notify: invalid WATCHDOG_USEC=%q, watchdog disabled", usec)
		return
	}
	// systemd's documented best-practice : ping at half the period.
	interval := time.Duration(micro/2) * time.Microsecond
	if interval < time.Second {
		interval = time.Second
	}
	logger.Printf("sd_notify watchdog: ping every %s (WatchdogSec=%s)", interval, time.Duration(micro)*time.Microsecond)
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := sdNotify("WATCHDOG=1"); err != nil {
					logger.Printf("sd_notify WATCHDOG=1 failed: %v", err)
				}
			}
		}
	}()
}
