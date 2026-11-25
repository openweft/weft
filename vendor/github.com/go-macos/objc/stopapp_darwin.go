//go:build darwin

package objc

import (
	"sync"

	"github.com/ebitengine/purego"
)

// Stopping the AppKit run loop [RunApp] entered.
//
// The seams are package vars for the same reason the run-loop ones are: a test
// drives the load failure and the success without a run loop worth stopping.
var (
	stopOnce    sync.Once
	stopGetMain func() uintptr
	stopStop    func(rl uintptr)
	stopLoadErr error

	// stopOpen opens CoreFoundation, which exports the run loop.
	stopOpen = func() (uintptr, error) {
		return purego.Dlopen(CoreFoundation, purego.RTLD_LAZY|purego.RTLD_GLOBAL)
	}
	// stopReg binds the two calls that reach the MAIN thread's run loop from
	// any thread. CFRunLoopStop is one of the few CoreFoundation calls that is
	// documented as safe to make from another one.
	stopReg = func(h uintptr) {
		purego.RegisterLibFunc(&stopGetMain, h, "CFRunLoopGetMain")
		purego.RegisterLibFunc(&stopStop, h, "CFRunLoopStop")
	}
)

// StopApp asks the run loop [RunApp] entered to return, and WAKES IT so that it
// does.
//
// -[NSApplication stop:] only sets a flag. AppKit reads it after it finishes
// processing an event, so an application sitting in
// -nextEventMatchingMask:untilDate:distantFuture with nothing happening does
// not stop at all: it keeps waiting for an event that is never coming, and
// -[NSApplication run] never returns.
//
// That is not a corner case, it is the ordinary one for a menu-bar program. A
// desk waiting for a pair of glasses to be plugged in ran its tray's loop while
// it waited, found the glasses, asked the loop to stop -- and hung, with 0%
// CPU, until somebody moved the mouse. Nobody was moving the mouse: the whole
// point of that program is that it starts by itself.
//
// So the flag is set and then the main thread's run loop is stopped, which
// makes AppKit's event wait return, which lets it read the flag it was given.
// CFRunLoopStop is one of the few CoreFoundation calls documented as safe from
// another thread, which is what makes this callable from wherever the decision
// to stop was taken.
//
// It is safe to call when nothing is running: -[NSApplication stop:] on an
// application that is not in its run loop is a no-op, and stopping a run loop
// that is not running is one too.
func StopApp() {
	App().Send(Sel("stop:"), 0)
	WakeMainRunLoop()
}

// WakeMainRunLoop makes the main thread's run loop return from whatever it is
// waiting on, from any thread.
//
// It is [StopApp] without the flag: for a caller that runs the main loop
// itself and wants it back for one turn, rather than for good.
func WakeMainRunLoop() {
	stopOnce.Do(func() {
		h, err := stopOpen()
		if err != nil {
			stopLoadErr = err
			return
		}
		stopReg(h)
	})
	if stopLoadErr != nil || stopGetMain == nil || stopStop == nil {
		return
	}
	if rl := stopGetMain(); rl != 0 {
		stopStop(rl)
	}
}
