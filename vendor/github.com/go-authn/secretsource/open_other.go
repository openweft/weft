//go:build !unix && !windows

package secretsource

import "os"

// openSecretFile has no no-follow open on this platform.
func openSecretFile(path string) (*os.File, error) { return os.Open(path) }
