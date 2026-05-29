//go:build darwin

package agent

// On macOS the host driver is the Apple-VZ plugin (cgo, code-signed with the
// virtualization entitlement). buildLocalHandles (agent_plugin.go) launches it.
const (
	localDriverPlugin    = "weft-driver-vz"
	localHypervisorLabel = "apple-vz"
)
