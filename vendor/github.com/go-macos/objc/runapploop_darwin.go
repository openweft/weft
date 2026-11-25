//go:build darwin

package objc

import (
	"sync"

	"github.com/ebitengine/purego"
)

// nsEventMaskAny is NSEventMaskAny: every event type there is.
const nsEventMaskAny = ^uint64(0)

// loopWaitSeconds is how long the loop waits for an event before asking its
// caller, again, whether to carry on.
//
// A BOUNDED wait, and it has to be. -[NSApplication nextEventMatchingMask:]
// with distantFuture cannot be interrupted from outside: stopping the run loop
// under it does not break it, because Carbon's ReceiveNextEventCommon simply
// runs the loop again -- measured, with a sample showing the main thread parked
// in exactly that call while the flag it was waiting for had been set minutes
// earlier.
//
// Fifty milliseconds is under the twentieth of a second a person can notice on
// a quit, and it is not a spin: the thread sits in the kernel until an event
// arrives or the wait expires, which for an idle menu-bar item is once every
// fifty milliseconds and nothing else.
const loopWaitSeconds = 0.05

// msgSendDouble is objc_msgSend with a signature that has a DOUBLE in it.
//
// It exists for one call, +[NSDate dateWithTimeIntervalSinceNow:], and it is
// bound separately because the general Send passes every argument in an integer
// register. A double goes in a floating-point one, so sending this selector the
// ordinary way gives a date built from whatever bit pattern happened to be
// there -- silently, since nothing on either side of the bridge knows the
// difference.
var (
	dateOnce sync.Once
	dateFn   func(recv uintptr, sel uintptr, seconds float64) uintptr
	dateErr  error

	// dateOpen opens the Objective-C runtime, which exports objc_msgSend.
	dateOpen = func() (uintptr, error) {
		return purego.Dlopen(LibObjC, purego.RTLD_LAZY|purego.RTLD_GLOBAL)
	}
	dateReg = func(h uintptr) { purego.RegisterLibFunc(&dateFn, h, "objc_msgSend") }
)

// waitUntil returns an NSDate that many seconds from now, or 0 when the
// runtime will not load -- in which case the caller waits for an event with no
// deadline, which is what it would have done anyway.
func waitUntil(seconds float64) ID {
	dateOnce.Do(func() {
		h, err := dateOpen()
		if err != nil {
			dateErr = err
			return
		}
		dateReg(h)
	})
	if dateErr != nil || dateFn == nil {
		return 0
	}
	return ID(dateFn(uintptr(ClassID("NSDate")), uintptr(Sel("dateWithTimeIntervalSinceNow:")), seconds))
}

// RunAppLoop enters an AppKit event loop and LEAVES IT when stop says so.
//
// It exists because -[NSApplication run] cannot be left on demand.
// -[NSApplication stop:] sets a flag that AppKit reads only after it finishes
// processing an event, so an application with nobody touching it waits for an
// event that is never coming and never returns. Waking the run loop under it
// does not help either: Carbon's event wait runs the loop again rather than
// returning. Both were tried, on the machine that needed this, and the sample
// showed the main thread in the same place each time.
//
// This is the loop -[NSApplication run] runs, written out: take the next event,
// send it, and between the two ask the caller whether to carry on. The wait is
// BOUNDED -- see loopWaitSeconds -- so the question gets asked whether or not
// anything happens, which is the whole point for a program that has to stop
// with nobody at the machine.
//
// It is NOT a pump. -[NSApplication sendEvent:] is what tracks a menu, drives a
// window and drags a scrollbar; a program that sampled the run loop instead
// would show a menu-bar item whose menu never opens, which is a real defect
// this fleet has already paid for once. The only thing missing against run is
// the application lifecycle around the loop, which a menu-bar program does not
// have.
//
// It must be called on the process main OS thread. policy is an
// NSApplicationActivationPolicy value (0 = Regular, 1 = Accessory/menu-bar).
// A nil stop runs for ever, which is what run does.
func RunAppLoop(policy int, stop func() bool) {
	app := App()
	app.Send(Sel("setActivationPolicy:"), policy)
	// finishLaunching, not run: the application has to be told it has started
	// -- it posts NSApplicationDidFinishLaunching and unblocks the parts of
	// AppKit that wait for it -- and run is the very thing being replaced.
	app.Send(Sel("finishLaunching"))

	mode := NSString("kCFRunLoopDefaultMode")
	mode.Send(Sel("retain")) // for the life of the loop
	for stop == nil || !stop() {
		until := waitUntil(loopWaitSeconds)
		if until == 0 {
			// No deadline to be had. Wait for an event with none, which is
			// what -[NSApplication run] does and is better than spinning.
			until = ClassID("NSDate").Send(Sel("distantFuture"))
		}
		ev := app.Send(Sel("nextEventMatchingMask:untilDate:inMode:dequeue:"),
			nsEventMaskAny, until, mode, true)
		if ev == 0 {
			continue // the wait expired: ask again whether to carry on
		}
		app.Send(Sel("sendEvent:"), ev)
	}
}
