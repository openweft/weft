package weft

// cmdline.go holds small helpers for manipulating the kernel
// cmdline string we hand to a microVM. They live in their own
// file so adapter.go doesn't grow another 50 lines of string
// fiddling and so the test pinning the edge cases sits next to
// the code under test.

import "strings"

// mergeProjectEnv injects `kv` (a KEY=value string) into the
// cmdline's `weft.env=...` clause so the value reaches the guest
// process environment via weft-microvm-init. The clause value is colon-
// separated to dodge shell quoting through /proc/cmdline (matching
// weft-microvm-init's `strings.Split(v, ":")` parser).
//
// The new entry is appended to the end of the colon list. weft-microvm-init
// applies env entries in order and a later set wins over an earlier
// one, so the platform-injected key always beats a caller-supplied
// duplicate — that's the invariant the per-project subject relies
// on: WEFT_PROJECT_UUID inside the guest must be the project the VM
// actually belongs to, not whatever the operator typed.
//
// If no `weft.env=` token exists, one is appended (with a leading
// space iff `cmdline` is non-empty). The helper is whitespace-
// tolerant on input but normalises to single-space separation on
// output, which is fine for both /proc/cmdline consumers we care
// about (the kernel itself and weft-microvm-init).
func mergeProjectEnv(cmdline, kv string) string {
	if kv == "" {
		return cmdline
	}
	tokens := strings.Fields(cmdline)
	for i, t := range tokens {
		if !strings.HasPrefix(t, "weft.env=") {
			continue
		}
		v := strings.TrimPrefix(t, "weft.env=")
		if v == "" {
			tokens[i] = "weft.env=" + kv
		} else {
			tokens[i] = "weft.env=" + v + ":" + kv
		}
		return strings.Join(tokens, " ")
	}
	if cmdline == "" {
		return "weft.env=" + kv
	}
	return cmdline + " weft.env=" + kv
}
