//go:build linux

package main

import "github.com/openweft/weft/floatingipnat"

// newFloatingIPNATReconciler returns the real netlink-backed
// reconciler on Linux ; tag-split sibling returns the stub
// elsewhere.
func newFloatingIPNATReconciler() floatingipnat.Reconciler {
	return floatingipnat.NewLinuxReconciler()
}
