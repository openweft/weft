//go:build !linux

package main

import "github.com/openweft/weft/floatingipnat"

// newFloatingIPNATReconciler returns the no-op stub off Linux
// so the cross-platform dev build still links. weft-agent never
// runs in production off Linux ; the stub is exercised by host-
// side tests on darwin.
func newFloatingIPNATReconciler() floatingipnat.Reconciler {
	return floatingipnat.NewStubReconciler()
}
