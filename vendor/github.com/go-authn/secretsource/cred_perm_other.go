//go:build !unix

package secretsource

// systemd credentials are Linux-only and refused before a file is read; the
// strict file: check stands in so the package builds everywhere.
var checkCredFile = checkSecretFile
