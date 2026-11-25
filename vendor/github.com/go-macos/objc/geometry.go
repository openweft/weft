// Copyright (c) 2026, the go-macos authors. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license that can be
// found in the LICENSE file.

package objc

// The Foundation geometry structs. They are here rather than redeclared by
// every caller because they are an ABI contract, not a convenience: a message
// that takes an NSSize takes it BY VALUE, in floating-point registers, and a
// struct of the wrong shape is not a compile error — it is two garbage
// numbers arriving at AppKit.
//
// Each is CGFloat-based, which is float64 on every platform macOS still ships
// for. Pass them straight to [ID.Send]; purego marshals them by value, and
// [Send] reads them back the same way:
//
//	img.Send(Sel("setSize:"), NSSize{Width: 18, Height: 18})
//	got := Send[NSSize](img, Sel("size"))

// NSPoint is a location in a coordinate system (Foundation's NSPoint, and
// CoreGraphics' CGPoint, which is the same struct).
type NSPoint struct{ X, Y float64 }

// NSSize is a width and a height (Foundation's NSSize, and CoreGraphics'
// CGSize).
//
// Setting it on an NSImage is not cosmetic: an NSImage built from data takes
// its size from the bitmap's pixel count, so a 36-pixel image reports 36
// POINTS — on a 2x display that is twice the intended size, and in a 22-point
// menu bar it is an icon taller than the bar it sits in. The pixels are the
// resolution; the size is how big it should be drawn.
type NSSize struct{ Width, Height float64 }

// NSRect is an origin and a size (Foundation's NSRect, and CoreGraphics'
// CGRect).
type NSRect struct {
	Origin NSPoint
	Size   NSSize
}
