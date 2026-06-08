package main

// respawn_subscriber.go wires agentrespawn.Subscriber into the daemon :
// subscribes to vm.state_changed + schedulingrule.* events on the
// existing platform event bus and drives the respawn state machine.
//
// VM actions are routed through the Adapter (local host case) ; the
// V0.1 surface respawns VMs by name on the agent that received the
// down signal. A dispatched-respawn path (rule says "VM X on host Y",
// the rule's home agent sends a remote StartVM) is V0.1.1 work, same
// dependency as label-selector matching — both need a clearer
// host-binding shape on SchedulingRuleEntry.
//
// Kept in its own file so the call-site in main.go is one defer and
// the wiring is easy to disable while we land follow-ups.

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	weft "github.com/openweft/weft"
	"github.com/openweft/weft/agentrespawn"
)

// startRespawnSubscriber starts the bus subscriber + reconciler loop.
// Returns a cancel that stops the goroutine and tears down the bus
// subscription. Always returns a non-nil cancel ; an init failure
// logs + returns a no-op so the daemon shutdown path stays simple.
func startRespawnSubscriber(adp weft.VZAdapter, bus weft.EventBus, logger *log.Logger) func() {
	actions := &respawnActions{adp: adp}
	sub := agentrespawn.
		New(bus, respawnRules{adp: adp}, actions, nil).
		WithStatusReader(respawnStatus{adp: adp})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := sub.Run(ctx); err != nil && err != context.Canceled {
			logger.Printf("respawn subscriber exited: %v", err)
		}
	}()
	logger.Printf("respawn subscriber: bus subscribed + 2s poll fallback for microVM death")
	return cancel
}

// respawnActions adapts Adapter.StartVM/StopVM onto the
// respawn.VMActions interface. cloudInitISO is empty on respawn —
// the VM was already provisioned and its cidata.iso (if any) lives in
// the vmDir from the original create.
type respawnActions struct{ adp weft.VZAdapter }

func (a *respawnActions) StartVM(_ context.Context, name string) error {
	return a.adp.StartVM(name, "")
}
func (a *respawnActions) StopVM(_ context.Context, name string) error {
	return a.adp.StopVM(name)
}

// respawnRules is the SchedulingRulesReader projection of the
// adapter. Kept here (not in agentrespawn) for the same reason as
// watcherScope in floating_ip_nat.go : the dep direction stays
// agentrespawn → weft, not the other way.
type respawnRules struct{ adp weft.VZAdapter }

func (r respawnRules) SchedulingRules() []weft.SchedulingRuleEntry {
	return r.adp.SchedulingRules()
}

// respawnStatus is the VMStatusReader projection : tells the
// subscriber whether a microVM is currently alive. We mirror the
// adapter's StatusVM probe logic (exit.json takes precedence over
// the pid liveness check) so the poller sees the same truth a
// `weft microvm ls` call would surface.
type respawnStatus struct{ adp weft.VZAdapter }

// IsVMRunning reads the vmDir for the named VM and returns true iff
// the qemu/vz reaper has NOT written exit.json AND the recorded
// vm.pid maps to a non-zombie process. Mirrors adapter.go's status
// probe in the StatusVM RPC, plus a /proc/<pid>/status zombie check
// the original probe lacks : a SIGKILL'd qemu whose parent driver
// hasn't yet reaped it sits in state 'Z' (defunct), and signal-0
// against a zombie returns nil because the kernel still has the
// PID entry. Without the State check we'd report "running" for the
// duration of the unreaped zombie window — exactly when the respawn
// reconciler wants to see "stopped".
func (r respawnStatus) IsVMRunning(name string) bool {
	vmDir := r.adp.VMDir(name)
	if vmDir == "" {
		return false
	}
	if _, err := os.Stat(filepath.Join(vmDir, "exit.json")); err == nil {
		return false
	}
	pidBytes, err := os.ReadFile(filepath.Join(vmDir, "vm.pid"))
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if proc.Signal(syscall.Signal(0)) != nil {
		return false
	}
	// Zombie check via /proc/<pid>/status. Format line "State:\t<X>
	// (...)". On a non-zombie host process X is one of R/S/D/T/t/I
	// (running, sleeping, etc.). 'Z' means defunct — the process is
	// dead but its exit code hasn't been reaped by the parent yet.
	// Treating Z as "stopped" is what `weft microvm ls` does too via
	// the exit.json path ; we just race ahead of the reaper here.
	return !isZombie(pid)
}

// isZombie returns true when /proc/<pid>/status reports State Z.
// Linux-only ; safe no-op (returns false) when /proc is unavailable
// because the dev/test host isn't Linux.
func isZombie(pid int) bool {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "State:") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				return f[1] == "Z"
			}
		}
	}
	return false
}
