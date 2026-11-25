//go:build !linux

package secretservice

// The Secret Service is a freedesktop/Linux facility. Off Linux the package
// still builds and cross-compiles, but every entry point reports
// [ErrUnavailable] — never a silent success — by making openBackend fail.
func init() {
	openBackend = func() (backend, error) { return nil, ErrUnavailable }
}
