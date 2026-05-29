package wgtransport

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/curve25519"
)

// resolvePrivateKey returns the raw 32-byte key from an inline base64 key
// when provided, otherwise loads (or creates) it from path.
func resolvePrivateKey(inline, path string) ([]byte, error) {
	if inline != "" {
		return decodeKey(strings.TrimSpace(inline))
	}
	return loadOrCreatePrivateKey(path)
}

// loadOrCreatePrivateKey reads a base64-encoded Curve25519 private key from
// path. If the file does not exist, a fresh key is generated and persisted
// with 0600 permissions. Returns the raw 32-byte key.
func loadOrCreatePrivateKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return generateAndSavePrivateKey(path)
	}
	if err != nil {
		return nil, err
	}
	return decodeKey(strings.TrimSpace(string(data)))
}

func generateAndSavePrivateKey(path string) ([]byte, error) {
	priv, err := newPrivateKey()
	if err != nil {
		return nil, fmt.Errorf("generate private key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	encoded := base64.StdEncoding.EncodeToString(priv) + "\n"
	if err := os.WriteFile(path, []byte(encoded), 0o600); err != nil {
		return nil, fmt.Errorf("save private key: %w", err)
	}
	return priv, nil
}

// newPrivateKey generates a clamped 32-byte Curve25519 private key suitable
// for WireGuard.
func newPrivateKey() ([]byte, error) {
	priv := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, priv); err != nil {
		return nil, err
	}
	// RFC 7748 §5: clamp the scalar so it's a valid Curve25519 secret.
	priv[0] &= 248
	priv[31] &= 127
	priv[31] |= 64
	return priv, nil
}

// publicKey derives the public key from a 32-byte private key.
func publicKey(priv []byte) ([]byte, error) {
	if len(priv) != 32 {
		return nil, fmt.Errorf("private key must be 32 bytes, got %d", len(priv))
	}
	return curve25519.X25519(priv, curve25519.Basepoint)
}

// decodeKey parses a base64-encoded 32-byte key.
func decodeKey(s string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("decode key: %w", err)
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("key must be 32 bytes, got %d", len(raw))
	}
	return raw, nil
}
