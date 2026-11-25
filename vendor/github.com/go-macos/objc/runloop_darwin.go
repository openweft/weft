//go:build darwin

package objc

import (
	"sync"

	"github.com/ebitengine/purego"
)

// Pumping this thread's run loop.
//
// The seams are package vars for the same reason the dispatch ones are: a test
// drives the load failure and the success without a run loop worth running.
var (
	rlOnce    sync.Once
	rlRunFn   func(mode uintptr, seconds float64, returnAfterSourceHandled bool) int32
	rlMode    uintptr
	rlLoadErr error

	// rlOpen opens CoreFoundation, which exports the run loop.
	rlOpen = func() (uintptr, error) {
		return purego.Dlopen(CoreFoundation, purego.RTLD_LAZY|purego.RTLD_GLOBAL)
	}
	// rlReg binds CFRunLoopRunInMode.
	rlReg = func(h uintptr) { purego.RegisterLibFunc(&rlRunFn, h, "CFRunLoopRunInMode") }
	// rlModeString makes the mode string.
	//
	// kCFRunLoopDefaultMode is a CFStringRef VARIABLE whose value is this
	// literal, and dereferencing an exported data pointer is what go vet's
	// unsafeptr check rejects — so the literal is spelled out and retained,
	// which is what every other binding in the fleet does with it.
	rlModeString = func() uintptr {
		s := NSString("kCFRunLoopDefaultMode")
		s.Send(Sel("retain")) // for the life of the process: a mode is a constant
		return uintptr(s)
	}
)

// PumpRunLoop runs THIS thread's run loop for at most seconds, then returns.
//
// It exists because several AppKit and Foundation facilities are fed by
// notifications delivered on a run loop, and a process that never runs one
// never hears them. The list behind -[NSWorkspace runningApplications] is the
// one that bites hardest: measured in go-macos/accessibility, an application
// launched a moment earlier appeared within 500 ms with a 50 ms pump, and was
// STILL invisible fifteen seconds later without it — and would have stayed
// invisible for the life of the process, because the list is a cache and the
// cache is only ever refreshed by those notifications.
//
// A library cannot require its caller to run an AppKit run loop. This is the
// smallest thing it can do instead, and it belongs here rather than being bound
// again in every package that meets the problem.
//
// ⚠ PUMPING IS NOT A SLEEP. A run loop with nothing attached to it returns
// kCFRunLoopRunFinished AT ONCE -- measured at 54µs for a 50 ms pump in a bare
// test binary -- so this is a chance for pending work to be delivered, not a
// delay. A caller that needs a delay should say so with time.Sleep.
//
// It is not a substitute for [Run] or [DispatchMain] either: it services
// whatever is attached to the CURRENT thread's run loop and returns. A negative
// or zero duration returns at once, having done nothing at all.
func PumpRunLoop(seconds float64) error {
	rlOnce.Do(func() {
		h, err := rlOpen()
		if err != nil {
			rlLoadErr = err
			return
		}
		rlReg(h)
		rlMode = rlModeString()
	})
	if rlLoadErr != nil {
		return rlLoadErr
	}
	if seconds <= 0 {
		return nil
	}
	rlRunFn(rlMode, seconds, false)
	return nil
}
