//go:build !darwin

package keychain

// On non-darwin platforms there is no Keychain. The seams are pointed at
// stubs and backendLoadErr is set to [ErrUnsupported], so every entry point
// returns ErrUnsupported while the package still builds and cross-compiles.
func init() {
	backendLoadErr = ErrUnsupported
	backendSet = func(string, string, []byte, config) int32 { return 0 }
	backendGet = func(string, string) ([]byte, int32) { return nil, 0 }
	backendDelete = func(string, string) int32 { return 0 }
}
