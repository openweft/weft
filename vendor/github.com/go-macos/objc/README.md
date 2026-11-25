# go-macos/objc

[![CI](https://github.com/go-macos/objc/actions/workflows/ci.yml/badge.svg)](https://github.com/go-macos/objc/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/go-macos/objc.svg)](https://pkg.go.dev/github.com/go-macos/objc)
[![Go 1.27.1](https://img.shields.io/badge/go-1.27.1-00ADD8)](https://go.dev/dl/)
[![License: BSD-3-Clause](https://img.shields.io/badge/license-BSD--3--Clause-blue.svg)](LICENSE)

A pure-Go (**CGO_ENABLED=0**) bridge to the macOS Objective-C runtime, reached
entirely through [ebitengine/purego](https://github.com/ebitengine/purego) —
`dlopen` + `objc_msgSend`, no cgo, no shelling out to `osascript`.

It is the single shared home for the Objective-C plumbing that the fleet's
CGO=0 macOS code kept re-implementing: selector/class lookup, NSString↔Go
bridging, framework `dlopen`, autorelease-pool handling and run loops.

## Surface

| Area | API |
| --- | --- |
| Selector / class lookup (cached) | `Sel`, `RegisterName`, `GetClass`, `ClassID` |
| Messaging | `ID.Send`, generic `Send[T]` (scalars, `CGFloat`, structs) |
| Runtime classes | `RegisterClass`, `RegisterClassWithProtocols`, `GetProtocol`, `MethodDef` |
| Strings | `NSString`, `GoString` |
| Objects / dictionaries | `Stringify`, `DictToMap`, `MapToDict` |
| Geometry | `NSPoint`, `NSSize`, `NSRect` |
| Frameworks | `Load`, `Open` + `Foundation` / `AppKit` / `WebKit` / `CoreFoundation` / `Security` / `LibSystem` |
| Scopes | `AutoreleasePool` |
| Application | `App`, `RunApp`, `SetApplicationIconImage` |
| Leaving a loop | `RunAppLoop(policy, stop)`, `StopApp`, `WakeMainRunLoop` |
| Run loops | `Run(ctx, class, setup)` + `Runner`, `PumpRunLoop` |
| Main-thread hop | `DispatchMain` (libdispatch `dispatch_async` onto the main queue) |
| Blocks | `Block`, `NewBlock`, `Block.Release`, `Block.Copy` |
| Validation | `ValidateName` |

```go
import "github.com/go-macos/objc"

objc.Load(objc.Foundation, objc.AppKit)

s := objc.GoString(objc.NSString("héllo"))                 // "héllo"
n := objc.Send[float64](objc.NSString("3.5"), objc.Sel("doubleValue")) // 3.5

cls, _ := objc.RegisterClass("MyTarget", objc.GetClass("NSObject"),
    []objc.MethodDef{
        {Cmd: objc.Sel("fire:"), Fn: func(self objc.ID, _ objc.SEL, sender objc.ID) {
            // handle sender
        }},
    })
```

Asynchronous Cocoa APIs take a **completion handler block**. The Go function's
first parameter is the block itself; the rest are the block's arguments:

```go
block := objc.NewBlock(func(b objc.Block, content objc.ID, err objc.ID) {
    defer b.Release() // the block is retained until released
    if err != 0 {
        log.Println(objc.Stringify(err))
        return
    }
    use(content)
})
objc.ClassID("SCShareableContent").
    Send(objc.Sel("getShareableContentWithCompletionHandler:"), block)
```

## Threads

AppKit's rules about threads are not advice, and getting them wrong here does
not produce an error — it produces a trap inside AppKit with nothing of your
program on the stack. Two of them are worth knowing before you write anything
against this package.

**`App()`'s first call binds the application to the thread that makes it.**
`+[NSApplication sharedApplication]` creates the shared application and ties it
to whoever asks first. Ask from a goroutine that is not on the process main OS
thread and everything appears to work — the application is returned, messages to
it are answered — until the **main** thread tries to take an event, which dies
with `SIGTRAP` inside `-nextEventMatchingMask:`. Only the *first* call is bound
by this: once the application exists, messaging it from another goroutine is
fine. `RunApp` and `RunAppLoop` must be on that thread anyway, so a program that
starts with either is already right.

**`runtime.LockOSThread()` pins the current thread, not the main one.** It is
the obvious thing to reach for and it does not get you to the main thread; a
goroutine other than the one running `main()` is on that thread only by luck.
Measured in this package's own tests: a locked test goroutine lands there on
most runs and not all of them, which is worse than never — it makes a fixture
that cannot work pass most of the time, and it cost one CI red that took a day
to read. `WakeMainRunLoop` stops the **main** thread's loop while `PumpRunLoop`
runs the **current** thread's, so if you are not on that thread the two are
different loops.

If you need the main thread from somewhere else, the honest ways are
`DispatchMain` (which needs the main thread to be running a loop, so it is for a
program, not a test) or, in a test, a `TestMain` that keeps the main goroutine
locked and serves a channel — see `mainthread_darwin_test.go`, which also
asserts that the channel really reaches that thread rather than assuming it.

## Design

- **On darwin** the runtime types (`ID`, `SEL`, `Class`, `IMP`, `MethodDef`) are
  aliases of the corresponding `purego/objc` types, so method closures satisfy
  purego's strict `NewIMP` `(id, SEL)` receiver check and existing purego-based
  call sites can switch their import to this package and keep compiling.
- **On non-darwin** the same symbols exist as `uintptr`-width types and stubs
  that report [`ErrUnsupported`], so consumers cross-compile. The OS-independent
  core (selector/class caches, name validation, the `GoString` marshalling
  logic and `Load`) stays fully functional and is covered **100%** on every
  lane through injection seams.
- The darwin C/objc calls sit behind fake-injection seams, so their error
  branches are reachable in tests; the real `objc_msgSend` / NSString /
  `RegisterClass` / run-loop paths are verified **on-device** in CI.

## License

BSD-3-Clause © the go-macos/objc authors.
