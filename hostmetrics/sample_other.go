//go:build !linux

package hostmetrics

// Non-Linux stub : the agent's metrics sampler is /proc-based, which
// only the Linux kernel provides. On macOS / *BSD dev hosts the
// Sampler still constructs cleanly and Run() loops, but every
// sample reports zeros (the agent on those platforms is a dev/test
// flavour, not a production target). Keeps the cross-platform build
// green without #ifdef'ing the call site.

func readCPU() (cpuCounters, error) { return cpuCounters{}, nil }
func readMem() (memCounters, error) { return memCounters{}, nil }
func readNet() (netCounters, error) { return netCounters{}, nil }
