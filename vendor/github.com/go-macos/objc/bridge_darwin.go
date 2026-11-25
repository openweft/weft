//go:build darwin

package objc

import (
	"context"
	"runtime"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

// init points the OS-independent seams (declared in objc.go) at the real
// purego/objc-backed implementations. Everything above these seams — the
// caches, name validation, the GoString marshalling logic and Load — is shared
// with every platform; only these leaf calls are darwin-specific.
func init() {
	resolveSel = func(name string) uintptr { return uintptr(objc.RegisterName(name)) }
	resolveClass = func(name string) uintptr { return uintptr(objc.GetClass(name)) }
	dlopenFn = func(path string, mode int) (uintptr, error) { return purego.Dlopen(path, mode) }
	lengthOfBytesFn = func(id ID) int {
		return int(id.Send(objc.RegisterName("lengthOfBytesUsingEncoding:"), nsUTF8Encoding))
	}
	getCStringFn = func(id ID, buf []byte) bool {
		return id.Send(objc.RegisterName("getCString:maxLength:encoding:"),
			unsafe.Pointer(&buf[0]), len(buf), nsUTF8Encoding) != 0
	}
}

// Send sends selector sel to id with args and returns the result typed as T.
// Use it when the message returns a scalar or a by-value struct (e.g. a CGFloat
// via Send[float64] or an NSRect via a matching struct), so purego marshals the
// return through the correct calling convention. For an object (or ignored)
// result, use the inherited ID.Send, which returns an [ID].
func Send[T any](id ID, sel SEL, args ...any) T {
	return objc.Send[T](id, sel, args...)
}

// NSString builds an autoreleased NSString from a Go string.
func NSString(s string) ID {
	return ClassID("NSString").Send(Sel("stringWithUTF8String:"), s)
}

// RegisterClass defines a new Objective-C class named name with superclass
// super and the given instance methods, and returns it. Each method's Fn is a
// Go closure whose first two parameters are the receiver [ID] and the invoked
// [SEL]; RegisterClass wraps it into an IMP. It is the runtime-class mechanism
// behind delegates, timer targets and menu-action handlers. When the class must
// formally conform to a protocol, use [RegisterClassWithProtocols]. (Instance
// variables are intentionally omitted; the fleet's classes need none.)
func RegisterClass(name string, super Class, methods []MethodDef) (Class, error) {
	return RegisterClassWithProtocols(name, super, nil, methods)
}

// GetProtocol returns the Objective-C protocol named name (a nil pointer if no
// such protocol is registered in the process). Pass the result to
// [RegisterClassWithProtocols] so a runtime class formally conforms to it —
// required, for example, by APIs that check conformsToProtocol: such as
// -[WKWebViewConfiguration setURLSchemeHandler:forURLScheme:], which demands a
// WKURLSchemeHandler.
func GetProtocol(name string) *Protocol { return objc.GetProtocol(name) }

// RegisterClassWithProtocols is [RegisterClass] plus formal protocol
// conformance: the returned class declares each of protocols (so
// conformsToProtocol: reports true) in addition to implementing methods. Use it
// when an API validates protocol conformance rather than merely probing
// respondsToSelector:.
func RegisterClassWithProtocols(name string, super Class, protocols []*Protocol, methods []MethodDef) (Class, error) {
	return objc.RegisterClass(name, super, protocols, nil, methods)
}

// AutoreleasePool runs fn inside a fresh NSAutoreleasePool, draining the pool
// when fn returns (even if it panics). Use it around a burst of autoreleased
// allocations on a thread that has no ambient pool.
//
// The goroutine is PINNED to its OS thread for the whole of it.
//
// An NSAutoreleasePool belongs to the thread that created it, and Go moves an
// unlocked goroutine to another thread at any preemption point — a function
// call, an allocation, a channel operation, anything fn does. The pool is then
// drained on a thread that never owned it, and the Objective-C runtime does not
// return from that: it is a SIGSEGV inside objc_autoreleasePoolPop, at a
// program counter with nothing of this package on the stack.
//
// It is a race, so it does not fail on the machine it was written on. It failed
// on a CI runner on 2026-08-27, in a caller that had done nothing but add two
// more shortcuts to look up.
func AutoreleasePool(fn func()) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	pool := ClassID("NSAutoreleasePool").Send(Sel("alloc")).Send(Sel("init"))
	defer pool.Send(Sel("drain"))
	fn()
}

// appKitOnce loads AppKit the first time anything here asks for the shared
// application.
var appKitOnce sync.Once

// App returns the shared NSApplication instance (+[NSApplication
// sharedApplication]), creating it on first call, and loads AppKit first.
//
// The load belongs here rather than with the caller. This is the function that
// names NSApplication, and a caller who forgets gets a nil class, a nil
// application, and every message after it returning zero — in silence, because
// Objective-C does not complain about a message to nil. A menu-bar program
// built that way starts, prints its greeting and exits in a millisecond, with
// nothing on screen and nothing in a log. It cost six attempts to find once.
//
// ⛔ THE FIRST CALL MUST BE ON THE PROCESS MAIN OS THREAD. +[NSApplication
// sharedApplication] creates the application and binds it to the thread that
// asks, and a Go goroutine is on the main thread only by luck -- runtime.
// LockOSThread pins the CURRENT thread, not that one. Get this wrong and
// nothing complains: the application is returned, messages to it work, and then
// the MAIN thread cannot take an event. -[NSApplication
// nextEventMatchingMask:untilDate:inMode:dequeue:] dies with
//
//	SIGTRAP: trace trap
//	signal arrived during cgo execution
//
// at a program counter inside AppKit, with nothing of this package and nothing
// of the offending call on the stack -- the same shape of failure as draining
// an autorelease pool on the wrong thread, above, and found the same way.
//
// Measured in this package's tests on 2026-10-11, where a test that called
// App() from its own goroutine made a LATER test trap in an event wait, and
// creating the application on the main thread first fixed it with every other
// call left where it was. So only the first call is bound by this: once the
// application exists, messaging it from another goroutine is fine.
//
// [RunApp] and [RunAppLoop] must be on that thread anyway, so a program that
// starts with either is already right. One that sets a dock icon from a worker
// before anything else is not.
func App() ID {
	appKitOnce.Do(func() { _ = Load(AppKit) })
	return ClassID("NSApplication").Send(Sel("sharedApplication"))
}

// RunApp sets the shared application's activation policy and enters the AppKit
// run loop ([NSApp run]), which blocks until the application terminates. It
// must be called on the process main OS thread. policy is an
// NSApplicationActivationPolicy value (0 = Regular, 1 = Accessory/menu-bar).
func RunApp(policy int) {
	app := App()
	app.Send(Sel("setActivationPolicy:"), policy)
	app.Send(Sel("run"))
}

// SetApplicationIconImage sets the Dock/Cmd+Tab icon NSApp shows for the
// whole process, from PNG bytes — replacing whatever default AppKit
// would otherwise pick, which for an unsigned binary with no
// Info.plist icon resource is no icon at all, not merely a generic
// one. Only visible while activation policy is Regular ([RunAppLoop]'s
// policy 0): an Accessory-policy process keeps the image set but shows
// no Dock tile until it becomes Regular, so it is safe — and normal —
// to call this once at startup regardless of which policy the process
// runs under moment to moment.
//
// Empty png is a no-op: a caller with no icon to offer leaves whatever
// AppKit already has rather than clearing it to nothing.
func SetApplicationIconImage(png []byte) {
	if len(png) == 0 {
		return
	}
	// App(), not a bare Load(AppKit): NSImage is an AppKit class, and a
	// caller reaching this function first — an app's own startup, before
	// any other AppKit call — has never triggered the load otherwise.
	// ClassID("NSImage") before AppKit is loaded resolves to a nil class;
	// alloc and initWithData: on nil are Objective-C's own well-defined
	// "message to nil" no-op, returning nil in turn — so img == 0 below
	// happened SILENTLY, in exactly the shape App()'s own doc comment
	// already warns about. Six years of not having a caller stumble into
	// this, and this function found the way anyway.
	app := App()
	data := ClassID("NSData").Send(Sel("dataWithBytes:length:"), unsafe.Pointer(&png[0]), uintptr(len(png)))
	img := ClassID("NSImage").Send(Sel("alloc")).Send(Sel("initWithData:"), data)
	if img == 0 {
		return
	}
	// An NSImage built from data takes its SIZE from the bitmap's PIXEL
	// count, not a sensible logical size: a source PNG shipped at a
	// crisp 1024x1024 for Retina reports an image that is (as far as
	// AppKit's layout is concerned) 1024 POINTS across — which is why an
	// icon set this way rendered visibly larger than every sibling Dock
	// tile despite looking like an ordinary app icon. dockIconPoints is
	// macOS's own "large icon" convention (the size Finder/the Dock
	// treat as a normal app icon); explicit sizing turns the extra
	// pixels into resolution instead of dimensions, matching
	// go-widgets/tray's identical fix for its own menu-bar/menu-row
	// icons (nsImageFromPNG).
	if sz := Send[NSSize](img, Sel("size")); sz.Height > 0 {
		img.Send(Sel("setSize:"), NSSize{
			Width:  sz.Width * dockIconPoints / sz.Height,
			Height: dockIconPoints,
		})
	}
	app.Send(Sel("setApplicationIconImage:"), img)
}

// dockIconPoints is the logical size (in points) SetApplicationIconImage
// scales its image to — macOS's own "large icon" convention (128pt),
// the size Finder and the Dock already treat as an ordinary app icon
// regardless of how many source pixels back it.
const dockIconPoints = 128

// ---------------------------------------------------------------------------
// NSObject / NSDictionary helpers.
// ---------------------------------------------------------------------------

// Stringify renders any object as a Go string: NSStrings directly, anything
// else through -description (so a non-string value degrades to its textual form
// rather than crashing the getCString path). The nil object yields "".
func Stringify(v ID) string {
	if v == 0 {
		return ""
	}
	if v.Send(Sel("isKindOfClass:"), ClassID("NSString")) != 0 {
		return GoString(v)
	}
	return GoString(v.Send(Sel("description")))
}

// DictToMap flattens an NSDictionary to map[string]string, rendering both keys
// and values with [Stringify]. The nil dictionary yields an empty map.
func DictToMap(dict ID) map[string]string {
	if dict == 0 {
		return map[string]string{}
	}
	keys := dict.Send(Sel("allKeys"))
	n := int(keys.Send(Sel("count")))
	m := make(map[string]string, n)
	for i := 0; i < n; i++ {
		k := keys.Send(Sel("objectAtIndex:"), i)
		v := dict.Send(Sel("objectForKey:"), k)
		m[Stringify(k)] = Stringify(v)
	}
	return m
}

// MapToDict builds an autoreleased NSMutableDictionary of NSString->NSString
// from m.
func MapToDict(m map[string]string) ID {
	dict := ClassID("NSMutableDictionary").Send(Sel("dictionary"))
	for k, v := range m {
		dict.Send(Sel("setObject:forKey:"), NSString(v), NSString(k))
	}
	return dict
}

// ---------------------------------------------------------------------------
// Foundation run-loop runner.
// ---------------------------------------------------------------------------

// runPumpSeconds is how long each run-loop turn blocks waiting for input before
// the loop rechecks cancellation and its task queue. It bounds task latency.
const runPumpSeconds = 0.05

// Runner is the live handle to a [Run] loop. Its methods are safe to call from
// any goroutine; each marshals its work onto the run-loop thread.
type Runner struct {
	obj   ID
	tasks chan func()
}

// Object returns the runner's Objective-C instance (an instance of the class
// passed to [Run]). Observer callbacks and timers should target it. Valid only
// while [Run] is active.
func (r *Runner) Object() ID { return r.obj }

// Submit runs fn on the run-loop thread and blocks until it completes, so all
// Objective-C work stays on the one thread that owns the loop.
func (r *Runner) Submit(fn func()) {
	done := make(chan struct{})
	r.tasks <- func() { fn(); close(done) }
	<-done
}

// Run drives a Foundation run loop on the calling goroutine — which it pins to
// its OS thread — until ctx is cancelled, then returns ctx.Err(). It creates an
// instance of runnerClass (register it with [RegisterClass]), calls setup with
// the [Runner] once the instance exists (attach your observers there, using
// r.Object() and r.Submit), and services work queued through r.Submit on this
// thread. It is the mechanism behind observer-driven Foundation delivery such
// as NSDistributedNotificationCenter, which requires a running CFRunLoop.
//
// runnerClass must implement a no-op keepAlive: method (a repeating timer
// targets it to keep the loop from busy-spinning before observers attach).
// Call Run on a dedicated goroutine; it blocks for the lifetime of ctx.
func Run(ctx context.Context, runnerClass Class, setup func(r *Runner)) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	obj := ID(runnerClass).Send(Sel("alloc")).Send(Sel("init"))
	obj.Send(Sel("retain"))
	r := &Runner{obj: obj, tasks: make(chan func(), 64)}
	if setup != nil {
		setup(r)
	}

	ClassID("NSTimer").Send(Sel("scheduledTimerWithTimeInterval:target:selector:userInfo:repeats:"),
		runPumpSeconds, obj, Sel("keepAlive:"), ID(0), true)

	rl := ClassID("NSRunLoop").Send(Sel("currentRunLoop"))
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case fn := <-r.tasks:
			fn()
			continue
		default:
		}
		date := ClassID("NSDate").Send(Sel("dateWithTimeIntervalSinceNow:"), runPumpSeconds)
		rl.Send(Sel("runUntilDate:"), date)
	}
}
