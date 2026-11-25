//go:build darwin

package objc

import "github.com/ebitengine/purego/objc"

// Objective-C blocks.
//
// A block is a function pointer bundled with its captured state, which the
// runtime treats as an object — so a [Block] goes wherever [ID.Send] takes an
// argument. It is how Cocoa expresses callbacks and, above all, completion
// handlers: the asynchronous half of ScreenCaptureKit, AVFoundation, WebKit and
// Photos is reachable only through them.

// Block is an Objective-C block. On darwin it is an alias of
// github.com/ebitengine/purego/objc.Block, so a Block may be handed straight to
// [ID.Send] as a block argument (and a call site migrated from purego keeps
// compiling). The zero value is the nil block.
//
// Because it is an alias, Block carries purego's two reference-counting methods
// directly:
//
//   - Release decrements the block's reference count, freeing it on the last
//     reference. It must be called. A block built by [NewBlock] is retained by
//     an internal cache — that is what keeps the Go closure, and everything it
//     captures, alive while Objective-C holds the block — and only Release
//     drops it. Code that creates one block per asynchronous call therefore
//     MUST release it once the completion handler has run (releasing from
//     inside the handler is the usual place), or the cache grows without bound
//     for the life of the process.
//   - Copy copies the block onto the Objective-C heap, or increments its
//     reference count if it already lives there, and returns the heap copy. It
//     is the way to keep a block alive past the frame that created it — one
//     stored in an instance variable, or handed to an API that outlives the
//     call. Each Copy owes a matching Release.
type Block = objc.Block

// NewBlock wraps the Go function fn in an Objective-C block Cocoa can call, and
// returns it. Release it with Block.Release once it is no longer needed (see
// [Block]); a block created per asynchronous call that is never released leaks.
//
// The contract that trips people up: fn's FIRST parameter must be a [Block] —
// the block's own self pointer, which the block ABI passes as the hidden first
// argument — and fn's REMAINING parameters are the block's actual arguments, in
// order. purego panics if the first parameter is not a Block. The remaining
// parameter types must be ones purego can encode: [ID], integers, floats and
// pointers.
//
// A two-argument completion handler — the shape
// +[SCShareableContent getShareableContentWithCompletionHandler:] takes, whose
// Objective-C type is void (^)(SCShareableContent *content, NSError *error) —
// is written:
//
//	block := objc.NewBlock(func(b objc.Block, result objc.ID, err objc.ID) {
//		defer b.Release()
//		if err != 0 {
//			log.Println(objc.Stringify(err))
//			return
//		}
//		use(result)
//	})
//	objc.ClassID("SCShareableContent").
//		Send(objc.Sel("getShareableContentWithCompletionHandler:"), block)
//
// Note the arity: fn takes THREE parameters for a two-argument block, because
// the leading Block is the block itself and not one of the handler's arguments.
func NewBlock(fn any) Block { return objc.NewBlock(fn) }
