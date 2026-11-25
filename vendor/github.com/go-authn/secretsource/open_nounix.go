//go:build !unix

package secretsource

import "os"

// openNoFollow has no O_NOFOLLOW here; systemd credentials are Linux-only and
// the scheme is refused before this is reached.
func openNoFollow(path string) (*os.File, error) { return os.Open(path) }
