//go:build !darwin

package objc

// init points the low-level seams at unsupported stubs on non-darwin platforms
// (there is no Objective-C runtime). The OS-independent core in objc.go stays
// functional: Sel/GetClass cache the zero results these stubs return, GoString
// yields "", and Load reports [ErrUnsupported] wrapped in [ErrDlopen].
func init() {
	resolveSel = func(string) uintptr { return 0 }
	resolveClass = func(string) uintptr { return 0 }
	dlopenFn = func(string, int) (uintptr, error) { return 0, ErrUnsupported }
	lengthOfBytesFn = func(ID) int { return 0 }
	getCStringFn = func(ID, []byte) bool { return false }
}
