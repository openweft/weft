//go:build !darwin

package objc

// PumpRunLoop answers that it cannot: there is no CoreFoundation run loop off
// darwin, and a consumer that cross-compiles gets one clean error rather than a
// missing symbol.
func PumpRunLoop(float64) error { return ErrUnsupported }
