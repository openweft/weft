package main

import (
	"testing"

	weft "github.com/openweft/weft"
)

// closePluginsAtCleanup kills the driver plugin weft.New* launched for a test
// once the test ends. The weft package's own tests swap launchDriverPlugin for
// an in-process fake, but that seam is unexported, so every adapter built here
// spawns a real weft-driver-vz / weft-driver-qemu — which, without this, keeps
// running long after the test binary has exited.
func closePluginsAtCleanup[A weft.VZAdapter](t *testing.T, a A) A {
	t.Helper()
	c, ok := any(a).(interface{ ClosePlugins() })
	if !ok {
		t.Fatalf("%T has no ClosePlugins; its driver plugin would outlive the test", a)
	}
	t.Cleanup(c.ClosePlugins)
	return a
}
