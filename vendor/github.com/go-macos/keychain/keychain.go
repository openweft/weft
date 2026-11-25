// Package keychain is a pure-Go (CGO_ENABLED=0) store for macOS Keychain
// generic-password items. It reaches the OS through
// github.com/ebitengine/purego — dlsym'd CoreFoundation and Security C
// functions — so it links with no cgo and never shells out to
// /usr/bin/security (the secret never appears in any process's argv).
//
// The public surface is three byte-oriented calls over a (service, account)
// pair, each mapping to a kSecClassGenericPassword item:
//
//	err          := keychain.Set(service, account, secret)
//	secret, err  := keychain.Get(service, account)   // keychain.ErrNotFound if absent
//	err          := keychain.Delete(service, account)
//
// Errors are typed: [ErrNotFound] for a miss, [ErrEmptySecret] for an empty
// write, [ErrUnsupported] off darwin, and [*Error] (carrying the raw OSStatus)
// for any other Security-framework failure.
package keychain

import (
	"errors"
	"fmt"
)

// OSStatus values from the Security framework that the package branches on.
const (
	errSecSuccess      int32 = 0
	errSecItemNotFound int32 = -25300
	// errSecAccessControlFailed is a synthetic status the darwin backend
	// returns when SecAccessControlCreateWithFlags yields nil (there is no
	// real OSStatus for that path — the failure is reported out-of-band).
	errSecAccessControlFailed int32 = -25291
)

// Sentinel errors. They are stable and may be compared with [errors.Is].
var (
	// ErrNotFound is returned by [Get] when no item exists for the
	// (service, account) pair.
	ErrNotFound = errors.New("keychain: item not found")
	// ErrEmptySecret is returned by [Set] when secret is empty; the Keychain
	// cannot hold a zero-length generic password and [Get] could not tell it
	// apart from a miss.
	ErrEmptySecret = errors.New("keychain: empty secret")
	// ErrUnsupported is returned by every entry point on non-darwin platforms
	// (the Keychain is macOS-only). Exported symbols exist everywhere so
	// consumers cross-compile.
	ErrUnsupported = errors.New("keychain: unsupported on this platform (darwin only)")
)

// Error wraps a non-success, non-not-found OSStatus from the Security
// framework, tagged with the operation that produced it.
type Error struct {
	// Op is the failing operation: "set", "get" or "delete".
	Op string
	// Status is the raw OSStatus returned by the Security framework.
	Status int32
}

// Error implements the error interface.
func (e *Error) Error() string {
	return fmt.Sprintf("keychain: %s: OSStatus %d", e.Op, e.Status)
}

// AccessControlFlags selects the SecAccessControlCreateFlags constraints an
// item created with [WithAccessControl] must satisfy on read. The zero value
// applies no interactive constraint (the item is readable whenever the
// accessibility class is satisfied, with no Touch ID / passcode prompt).
type AccessControlFlags uint

const (
	// UserPresence (kSecAccessControlUserPresence) gates every read behind
	// Touch ID or the device passcode. Reading such an item triggers the
	// system biometric prompt.
	UserPresence AccessControlFlags = 1 << 0
	// BiometryAny (kSecAccessControlBiometryAny) requires any currently
	// enrolled biometric.
	BiometryAny AccessControlFlags = 1 << 1
	// BiometryCurrentSet (kSecAccessControlBiometryCurrentSet) invalidates the
	// item if the enrolled biometric set changes.
	BiometryCurrentSet AccessControlFlags = 1 << 3
	// DevicePasscode (kSecAccessControlDevicePasscode) gates reads behind the
	// device passcode.
	DevicePasscode AccessControlFlags = 1 << 4
)

// config is the resolved set of [Option] values for a [Set].
type config struct {
	useAC   bool
	acFlags AccessControlFlags
}

// Option customises a [Set]. Options are applied left to right.
type Option func(*config)

// WithAccessControl stores the item behind a SecAccessControl protecting it
// with the given flags and the accessibility class
// kSecAttrAccessibleWhenUnlockedThisDeviceOnly (so it never leaves this
// device and is never synced to iCloud). Pass [UserPresence] to gate reads
// behind Touch ID or the passcode; pass 0 for an at-rest-protected item with
// no interactive prompt. Without this option an item is a plain generic
// password created (or updated in place) with the default accessibility.
func WithAccessControl(flags AccessControlFlags) Option {
	return func(c *config) {
		c.useAC = true
		c.acFlags = flags
	}
}

// Backend seams. They are assigned in an init(): on darwin (backend_darwin.go)
// to the real purego-backed implementations, elsewhere (backend_other.go) to
// unsupported stubs. Keeping the raw FFI behind these vars lets the
// OS-independent logic below reach full coverage on every lane — tests swap
// the seams for fakes to drive each branch without a live Keychain.
var (
	// backendLoadErr is non-nil when the backend could not initialise
	// (framework/symbol load failure, or a non-darwin platform).
	backendLoadErr error
	// backendSet writes secret for (service, account) per cfg and returns an
	// OSStatus.
	backendSet func(service, account string, secret []byte, cfg config) int32
	// backendGet reads the secret for (service, account), returning the bytes
	// and an OSStatus.
	backendGet func(service, account string) ([]byte, int32)
	// backendDelete removes the item for (service, account) and returns an
	// OSStatus.
	backendDelete func(service, account string) int32
)

// Set stores secret under the generic-password item identified by
// (service, account), replacing any existing value. An empty secret is
// rejected with [ErrEmptySecret]. On a non-darwin platform it returns
// [ErrUnsupported].
func Set(service, account string, secret []byte, opts ...Option) error {
	if backendLoadErr != nil {
		return backendLoadErr
	}
	if len(secret) == 0 {
		return ErrEmptySecret
	}
	var c config
	for _, o := range opts {
		o(&c)
	}
	if st := backendSet(service, account, secret, c); st != errSecSuccess {
		return &Error{Op: "set", Status: st}
	}
	return nil
}

// Get returns the secret stored under (service, account). It returns
// [ErrNotFound] when no such item exists and [ErrUnsupported] off darwin.
// Reading an item created with [WithAccessControl] and [UserPresence]
// triggers the system biometric prompt.
func Get(service, account string) ([]byte, error) {
	if backendLoadErr != nil {
		return nil, backendLoadErr
	}
	b, st := backendGet(service, account)
	switch st {
	case errSecSuccess:
		return b, nil
	case errSecItemNotFound:
		return nil, ErrNotFound
	default:
		return nil, &Error{Op: "get", Status: st}
	}
}

// Delete removes the item identified by (service, account). Deleting an
// absent item is not an error. It returns [ErrUnsupported] off darwin.
func Delete(service, account string) error {
	if backendLoadErr != nil {
		return backendLoadErr
	}
	if st := backendDelete(service, account); st != errSecSuccess && st != errSecItemNotFound {
		return &Error{Op: "delete", Status: st}
	}
	return nil
}
