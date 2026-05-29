//go:build darwin

package weft

import "github.com/openweft/weft/imagestore"

// sanitizeRef and unsanitizeRef are thin wrappers around the imagestore
// package equivalents, kept here so tests can use them. Pure Go (the cgo
// constraint was vestigial from when the whole weft package required cgo).
func sanitizeRef(ref string) string { return imagestore.SanitizeRef(ref) }
func unsanitizeRef(s string) string { return imagestore.UnsanitizeRef(s) }
