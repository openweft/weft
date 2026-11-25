//go:build darwin

package objc

import "github.com/ebitengine/purego/objc"

// On darwin the public runtime types are aliases of the purego/objc types.
// Because they are true aliases (not new named types), a MethodDef.Fn closure
// written against objc.ID/objc.SEL satisfies purego's strict NewIMP receiver
// check, and any call site previously importing github.com/ebitengine/purego/objc
// can switch its import to this package and keep compiling.
type (
	// ID is an Objective-C object pointer (`id`); the zero value is nil.
	ID = objc.ID
	// SEL is an Objective-C selector.
	SEL = objc.SEL
	// Class is an Objective-C class object.
	Class = objc.Class
	// IMP is an Objective-C method implementation pointer.
	IMP = objc.IMP
	// MethodDef binds a selector to a Go closure for [RegisterClass]. Its Fn's
	// first two parameters must be (ID, SEL); RegisterClass wraps it into an IMP.
	MethodDef = objc.MethodDef
	// Protocol is an Objective-C protocol, looked up with [GetProtocol] and
	// declared on a class via [RegisterClassWithProtocols].
	Protocol = objc.Protocol
)
