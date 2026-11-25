package objc

import (
	"errors"
	"fmt"
	"sync"
)

// The core Objective-C runtime types — ID, SEL, Class, IMP and MethodDef — are
// declared per-platform (types_darwin.go / types_other.go). On darwin they are
// aliases of the corresponding github.com/ebitengine/purego/objc types, so a
// method closure registered with [RegisterClass] satisfies purego's strict
// (id, SEL) receiver check and existing purego-based call sites compile
// unchanged; elsewhere they are plain uintptr-width types so consumers
// cross-compile.

// Errors reported by the package. They are stable and may be tested with
// errors.Is.
var (
	// ErrUnsupported is returned by the runtime entry points on non-darwin
	// platforms (the whole runtime is macOS-only).
	ErrUnsupported = errors.New("objc: unsupported on this platform (darwin only)")
	// ErrEmptyName is returned when a selector or class name is empty.
	ErrEmptyName = errors.New("objc: empty name")
	// ErrNameHasNUL is returned when a selector or class name contains a NUL
	// byte (the C runtime terminates names at the first NUL, so an embedded one
	// would silently truncate).
	ErrNameHasNUL = errors.New("objc: name contains NUL byte")
	// ErrDlopen wraps a framework dlopen failure from [Load].
	ErrDlopen = errors.New("objc: dlopen failed")
)

// Standard framework and dylib paths for [Load].
const (
	Foundation     = "/System/Library/Frameworks/Foundation.framework/Foundation"
	AppKit         = "/System/Library/Frameworks/AppKit.framework/AppKit"
	WebKit         = "/System/Library/Frameworks/WebKit.framework/WebKit"
	CoreFoundation = "/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation"
	CoreGraphics   = "/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics"
	Security       = "/System/Library/Frameworks/Security.framework/Security"
	LibSystem      = "/usr/lib/libSystem.B.dylib"
	// LibObjC is the Objective-C runtime itself, which exports objc_msgSend.
	// A caller needs it to bind a message send with a signature the general one
	// cannot express -- a double, which goes in a floating-point register.
	LibObjC = "/usr/lib/libobjc.A.dylib"
)

// nsUTF8Encoding is NSUTF8StringEncoding, the encoding used for every
// NSString<->Go conversion.
const nsUTF8Encoding = 4

// RTLD flags for [Load]'s dlopen seam, mirroring purego's values so the darwin
// wiring can pass them straight through without importing purego here.
const (
	rtldNow    = 0x2
	rtldGlobal = 0x8
)

// ValidateName rejects names the C runtime cannot carry: empty ([ErrEmptyName])
// or containing a NUL byte ([ErrNameHasNUL], since objc_registerName /
// objc_getClass terminate at the first NUL and would silently truncate). It is
// a convenience for consumers that build selector or class names from untrusted
// input before passing them to [Sel], [GetClass] or [RegisterClass].
func ValidateName(name string) error {
	switch {
	case name == "":
		return ErrEmptyName
	case indexNUL(name):
		return ErrNameHasNUL
	}
	return nil
}

// indexNUL reports whether s contains a NUL byte.
func indexNUL(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == 0 {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Low-level seams. They are assigned in an init(): on darwin (bridge_darwin.go)
// to the real purego-backed implementations, elsewhere (seams_other.go) to
// unsupported stubs. Keeping the assignments out of this file lets the
// OS-independent core here reach 100% coverage on every lane, and tests swap
// the seams to fakes so every branch below is reachable without a live runtime.
// ---------------------------------------------------------------------------

// resolveSel registers/looks up a selector by name (objc_registerName).
var resolveSel func(string) uintptr

// resolveClass looks up a class by name (objc_getClass).
var resolveClass func(string) uintptr

// dlopenFn opens a shared library (dlopen).
var dlopenFn func(string, int) (uintptr, error)

// lengthOfBytesFn returns the NSString's UTF-8 byte length
// (-lengthOfBytesUsingEncoding:).
var lengthOfBytesFn func(ID) int

// getCStringFn fills buf with the NSString's UTF-8 bytes and reports success
// (-getCString:maxLength:encoding:).
var getCStringFn func(ID, []byte) bool

// ---------------------------------------------------------------------------
// Selector / class lookup with caching.
// ---------------------------------------------------------------------------

var (
	cacheMu  sync.RWMutex
	selCache = map[string]SEL{}
	clsCache = map[string]Class{}
)

// Sel returns the selector named name, caching the result so repeated lookups
// are a map hit. An empty name yields the zero selector.
func Sel(name string) SEL {
	if name == "" {
		return 0
	}
	cacheMu.RLock()
	s, ok := selCache[name]
	cacheMu.RUnlock()
	if ok {
		return s
	}
	s = SEL(resolveSel(name))
	cacheMu.Lock()
	selCache[name] = s
	cacheMu.Unlock()
	return s
}

// GetClass returns the class named name, caching the result. An unknown class
// (or the zero name) yields the zero class.
func GetClass(name string) Class {
	if name == "" {
		return 0
	}
	cacheMu.RLock()
	c, ok := clsCache[name]
	cacheMu.RUnlock()
	if ok {
		return c
	}
	c = Class(resolveClass(name))
	cacheMu.Lock()
	clsCache[name] = c
	cacheMu.Unlock()
	return c
}

// RegisterName is an alias for [Sel]. It carries purego's spelling so a call
// site migrated from github.com/ebitengine/purego/objc compiles unchanged while
// gaining the selector cache.
func RegisterName(name string) SEL { return Sel(name) }

// ClassID returns the class named name as an [ID], so class ("factory")
// messages (alloc, sharedApplication, …) can be sent to it directly.
func ClassID(name string) ID { return ID(GetClass(name)) }

// ---------------------------------------------------------------------------
// NSString <-> Go.
// ---------------------------------------------------------------------------

// GoString copies an NSString's UTF-8 bytes into a Go-owned buffer and returns
// them. It returns "" for the nil object or an empty string. It fills a Go
// slice via -getCString:maxLength:encoding: rather than dereferencing the
// ObjC-owned -UTF8String pointer, so it stays free of any uintptr->Pointer
// arithmetic (the buffer handed to ObjC is a live Go pointer the collector
// tracks).
func GoString(id ID) string {
	if id == 0 {
		return ""
	}
	n := lengthOfBytesFn(id)
	if n <= 0 {
		return ""
	}
	buf := make([]byte, n+1) // +1 for the NUL getCString writes
	if !getCStringFn(id, buf) {
		return ""
	}
	return string(buf[:n])
}

// ---------------------------------------------------------------------------
// Framework loading.
// ---------------------------------------------------------------------------

var loadMu sync.Mutex

// Load dlopens each of paths (idempotently — dlopen refcounts, and a repeat
// open of an already-resident framework is cheap). It returns the first
// failure wrapped in [ErrDlopen]. Use the framework-path constants, e.g.
// Load(Foundation, AppKit).
func Load(paths ...string) error {
	loadMu.Lock()
	defer loadMu.Unlock()
	for _, p := range paths {
		if _, err := dlopenFn(p, rtldNow|rtldGlobal); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrDlopen, p, err)
		}
	}
	return nil
}

// Open dlopens path and returns the library HANDLE, so a caller can bind plain
// C entry points out of it with purego.RegisterLibFunc. A failure is wrapped in
// [ErrDlopen], naming the path.
//
// It exists because not everything macOS offers is an Objective-C message.
// CGGetActiveDisplayList, CGDisplayBounds and pthread_main_np are C functions,
// and binding one needs the handle that [Load] deliberately discards. Without
// Open, every consumer that wants a C symbol writes its own purego.Dlopen with
// its own copy of the path and its own choice of RTLD flags — which is how a
// process ends up with two different opinions about which libraries it has
// loaded, and how a framework that one path loads and another does not becomes
// a nil class somewhere far away.
//
// The flags are RTLD_NOW|RTLD_GLOBAL, the same ones Load uses, so a library
// opened through either is resolved eagerly and its symbols are visible to
// everything else in the process. dlopen refcounts, so opening an
// already-resident library is cheap and Open may be called freely.
func Open(path string) (uintptr, error) {
	loadMu.Lock()
	defer loadMu.Unlock()
	h, err := dlopenFn(path, rtldNow|rtldGlobal)
	if err != nil {
		return 0, fmt.Errorf("%w: %s: %v", ErrDlopen, path, err)
	}
	return h, nil
}
