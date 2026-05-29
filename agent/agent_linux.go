//go:build linux

package agent

// On Linux the host driver is the pure-Go QEMU plugin (KVM with nested virt,
// else TCG). buildLocalHandles (agent_plugin.go) launches it.
const (
	localDriverPlugin    = "weft-driver-qemu"
	localHypervisorLabel = "qemu"
)
