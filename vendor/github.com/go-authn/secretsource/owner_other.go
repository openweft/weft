//go:build !unix && !windows

package secretsource

import "io/fs"

// checkFileOwner: no owner to compare on this platform.
func checkFileOwner(fs.FileInfo, string) error { return nil }
