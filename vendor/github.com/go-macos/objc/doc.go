// Package objc is a pure-Go (CGO_ENABLED=0) bridge to the macOS Objective-C
// runtime. It reaches the OS entirely through github.com/ebitengine/purego —
// dlopen + objc_msgSend — so it links with no cgo and no shelling out to
// osascript.
//
// It exists to end the copy-paste of the same Objective-C plumbing across the
// fleet's CGO=0 macOS code (go-macos/notify, go-widgets/tray, the
// go-news-reader and go-reddit readers, …): every one of them independently
// re-implemented selector/class lookup, NSString<->Go bridging, framework
// dlopen, autorelease-pool handling and a run loop. This package is the single
// shared home for those primitives.
//
// # Surface
//
//   - [Sel] / [GetClass] / [ClassID]: cached selector and class lookup.
//   - [ID.Send] and the generic [Send]: typed objc_msgSend helpers (the generic
//     form carries float64/CGFloat and struct returns through the correct
//     calling convention).
//   - [RegisterClass]: define an Objective-C class at runtime from Go method
//     closures.
//   - [NSString] / [GoString]: NSString<->Go string conversion. GoString copies
//     via -getCString:maxLength:encoding: (never a raw -UTF8String pointer
//     deref, which trips go vet's unsafeptr check).
//   - [Stringify] / [DictToMap] / [MapToDict]: NSObject/NSDictionary helpers.
//   - [Load] plus the framework-path constants ([Foundation], [AppKit],
//     [WebKit], [CoreFoundation], [LibSystem]): idempotent dlopen.
//   - [AutoreleasePool]: run a closure inside an NSAutoreleasePool scope.
//   - [App] / [RunApp]: the shared NSApplication and its [NSApp run] loop.
//   - [NewBlock] / [Block]: Objective-C blocks, the shape every Cocoa
//     completion handler takes. The wrapped Go function's first parameter is
//     the block itself; release the block once the handler has run.
//   - [DispatchMain]: hop a closure onto the libdispatch main queue.
//   - [Run]: a Foundation run-loop runner pinned to a locked OS thread, with a
//     task queue serviced on that thread — for observer-driven Foundation work
//     (e.g. NSDistributedNotificationCenter).
//
// # Portability
//
// Every exported symbol is defined on all platforms so consumers cross-compile.
// On non-darwin GOOS the runtime entry points report [ErrUnsupported] (or return
// a zero [ID]); the OS-independent logic — the selector/class caches, name
// validation and the string-marshalling core — stays fully functional and
// testable there.
package objc
