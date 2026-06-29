package main

// self_heal.go — boot-time self-healing for local VMs.
//
// When the agent itself just came back (host reboot, systemd restart),
// every microVM whose recorded state is "running" or "starting" is
// silently dead : the qemu process died with the agent, no event
// fires. The respawn subscriber only reacts to `vm.down` events that
// happen *while it's running*, so without help, those VMs stay
// stopped forever after a reboot.
//
// selfHealLocalVMs closes that gap. After the agent finishes wiring
// (drivers loaded, registry hydrated), it walks every locally-owned
// VM, checks whether the qemu process is alive, and calls StartVM
// for each one that should be running but isn't.
//
// Idempotent : a VM whose process IS alive is left alone. A VM the
// operator explicitly stopped (VMStateStopped) is also left alone —
// self-heal is about crash recovery, not about overriding intent.
// Likewise VMStateDeleting and VMStateZombie are skipped : the
// zombiegc reconciler owns those.

import (
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/openweft/weft"
)

// selfHealLocalVMs iterates the local VM registry once and restarts
// every entry that's in a "should be running" state but has no live
// qemu process. Designed to be called in a goroutine from
// agent-startup ; logs its findings + actions to the supplied logger.
//
// The implementation is conservative :
//
//   - Only acts on VMs whose host_uuid matches the local agent's
//     host UUID (cross-host VMs are claimed by their owning agent
//     via the etcdcoord HostWatcher path, not by us).
//   - Only restarts VMs in state Running, Created, or Zombie.
//     Stopped/Deleting/Error are operator-driven states we don't
//     override.
//   - A 2-second per-VM startup delay throttles the burst when a
//     host comes back with dozens of VMs to rehydrate ; otherwise
//     the qemu fan-out floods CPU during the first 30s.
//   - Errors don't abort the loop : a single VM that can't be
//     restarted (missing image, kernel mismatch) shouldn't block
//     the remaining ones.
func selfHealLocalVMs(adp weft.VZAdapter, logger *log.Logger) {
	if adp == nil {
		return
	}
	localUUID := localHostUUID(adp)
	if localUUID == "" {
		// Host UUID hasn't been minted yet — nothing to self-heal,
		// nothing on disk to compare against. Subsequent agent
		// restarts will pick up where this one left off.
		return
	}

	// Give the agent a moment to settle. RegisterHost + driver
	// dispatch wiring run in parallel ; starting VMs before the
	// hypervisor driver is fully addressable just produces a wave
	// of "host has no driver handle" errors.
	time.Sleep(3 * time.Second)

	vms := adp.ListVMsForHost(localUUID)
	if len(vms) == 0 {
		return
	}

	logger.Printf("self-heal: checking %d local VMs on host %s", len(vms), localUUID)

	// Heal pass also normalises the state field of any VM whose
	// registry record is stale relative to the local qemu process :
	//
	//   - process alive AND state != Running → set Running (zombiegc
	//     no longer flags it on its next sweep, the UI stops
	//     showing "stopped" for a live VM).
	//   - process dead AND state suggests-running → StartVM, then
	//     set Running (the original self-heal contract).
	//
	// Without the alive-but-state-stale branch, the first batch of
	// VMs that self-heal restarted CAN have their state correctly
	// updated, but the rest stay at "stopped" / "zombie" forever
	// because their qemu survived an agent crash + zombiegc flipped
	// them to "zombie" without a recovery path (the bug the operator
	// reported : "le self heal ne semble pas fonctionner, on a
	// plein de zombie").
	healed, stateFixed := 0, 0
	for _, vm := range vms {
		switch vm.State {
		case weft.VMStateRunning, weft.VMStateCreated, weft.VMStateZombie, weft.VMStateStopped:
			// Eligible for self-heal.
		default:
			// Deleting / Error — operator-driven states, skip.
			continue
		}

		dir := adp.VMDir(vm.Name)
		if dir == "" {
			continue
		}

		if vmProcessAlive(dir) {
			// Live process. Normalise the registry state to Running
			// so zombiegc + the operator's view agree with reality.
			if vm.State != weft.VMStateRunning {
				if err := adp.SetVMState(vm.UUID, weft.VMStateRunning); err != nil {
					logger.Printf("self-heal: SetVMState %s → running (alive) : %v", vm.Name, err)
				} else {
					stateFixed++
					logger.Printf("self-heal: state %s : %s → running (qemu alive)", vm.Name, vm.State)
				}
			}
			continue
		}

		// Process dead. The "stopped" branch is operator-stopped
		// territory ; respect it AND skip a restart there to avoid
		// re-launching a VM the operator just shut down. Running /
		// Created / Zombie all imply "should be alive but isn't"
		// and warrant a restart.
		if vm.State == weft.VMStateStopped {
			continue
		}

		logger.Printf("self-heal: restarting VM %s (project_uuid=%s state=%s)", vm.Name, vm.ProjectUUID, vm.State)
		if err := adp.StartVM(vm.Name, ""); err != nil {
			logger.Printf("self-heal: StartVM %s: %v", vm.Name, err)
			continue
		}
		// StartVM forks the qemu subprocess but doesn't transition the
		// registry state (the state-update path runs only through the
		// event-bus subscriber, which doesn't currently translate
		// "vm.start" → SetVMState). Without an explicit SetVMState the
		// VM stays at "zombie" / "stopped" even though the qemu is
		// alive, and zombiegc keeps flagging it on its next sweep.
		// Closing the loop here.
		if err := adp.SetVMState(vm.UUID, weft.VMStateRunning); err != nil {
			logger.Printf("self-heal: SetVMState %s → running (restart) : %v", vm.Name, err)
		}
		// Bump the cumulative restart counter for the k8s-style
		// RESTARTS column. Errors are non-fatal — the restart
		// itself already succeeded.
		if err := adp.IncrementVMRestarts(vm.UUID); err != nil {
			logger.Printf("self-heal: IncrementVMRestarts %s : %v", vm.Name, err)
		}
		healed++
		// 2-second pacing between starts so qemu's first-minute
		// CPU spike doesn't overlap across the whole fleet.
		time.Sleep(2 * time.Second)
	}

	logger.Printf("self-heal: done — %d restarted, %d state normalised (qemu was alive but registry said otherwise)", healed, stateFixed)
}

// vmProcessAlive returns true when the VM's qemu pid file points to
// a live process. Conservative : a missing or unparsable vm.pid is
// treated as "not alive" so the caller restarts the VM.
func vmProcessAlive(vmDir string) bool {
	data, err := os.ReadFile(filepath.Join(vmDir, "vm.pid"))
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return false
	}
	return true
}
