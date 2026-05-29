package weft

// cmdline.go holds small helpers for manipulating the kernel
// cmdline string we hand to a microVM. They live in their own
// file so adapter.go doesn't grow another 50 lines of string
// fiddling and so the test pinning the edge cases sits next to
// the code under test.

import "strings"

// mergeProjectEnv injects `kv` (a KEY=value string) into the
// cmdline's `ncl.env=...` clause so the value reaches the guest
// process environment via ncl-init. The clause value is colon-
// separated to dodge shell quoting through /proc/cmdline (matching
// ncl-init's `strings.Split(v, ":")` parser).
//
// The new entry is appended to the end of the colon list. ncl-init
// applies env entries in order and a later set wins over an earlier
// one, so the platform-injected key always beats a caller-supplied
// duplicate — that's the invariant the per-project subject relies
// on: VZD_PROJECT_UUID inside the guest must be the project the VM
// actually belongs to, not whatever the operator typed.
//
// If no `ncl.env=` token exists, one is appended (with a leading
// space iff `cmdline` is non-empty). The helper is whitespace-
// tolerant on input but normalises to single-space separation on
// output, which is fine for both /proc/cmdline consumers we care
// about (the kernel itself and ncl-init).
func mergeProjectEnv(cmdline, kv string) string {
	if kv == "" {
		return cmdline
	}
	tokens := strings.Fields(cmdline)
	for i, t := range tokens {
		if !strings.HasPrefix(t, "ncl.env=") {
			continue
		}
		v := strings.TrimPrefix(t, "ncl.env=")
		if v == "" {
			tokens[i] = "ncl.env=" + kv
		} else {
			tokens[i] = "ncl.env=" + v + ":" + kv
		}
		return strings.Join(tokens, " ")
	}
	if cmdline == "" {
		return "ncl.env=" + kv
	}
	return cmdline + " ncl.env=" + kv
}
