//go:build !linux

package weft

// host_facts_other.go is the non-Linux stub for the OS / kernel /
// network / storage fact collectors. macOS / *BSD dev hosts don't
// run real weft-agent in production ; collection returns zero values
// so register paths still work without surfacing fake data.
//
// The Linux build (host_facts_linux.go) carries the real /etc/os-
// release + /proc + /sys + statfs path. Build-tag gated so neither
// the cgo-free contract nor the Linux-only syscalls leak across.

func collectOSRelease(_ string) string         { return "" }
func collectKernelVersion() string             { return "" }
func collectNetworkInterfaces() []NetworkInterface { return nil }
func collectStorageMounts() []StorageMount     { return nil }
