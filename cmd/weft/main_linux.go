//go:build linux

package main

import "github.com/spf13/cobra"

// registerHostCommands is a no-op on linux: the QEMU driver execs qemu-system
// directly (no forked per-VM runner like Apple-VZ's vz-vm-run), and there's
// no vz-provision. The agent uses the QEMU/KVM bundle (see adapter_linux.go).
func registerHostCommands(*cobra.Command) {}
