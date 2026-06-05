// adminkey.go — multi-key Verifier for the "admin-key" mode of
// federation manifest trust. Operators enrol one or more public
// keys (ed25519 or RSA) ; the manifest signature is accepted if
// it verifies against ANY enrolled key.
//
// Multi-key is the difference between "an admin signs" and "this
// specific person signs" : rotation needs at least 2 keys live
// during the cut-over, and large operations distribute the manifest
// signing duty across several admins. The verifier therefore takes
// a slice ; the Sign side stays bound to a single key (the signer's
// own).
//
// Scheme dispatch is implicit : ed25519 signatures are 64 bytes,
// RSA signatures are 256+ (RSA-2048+ ; we reject sub-2048 keys at
// enrolment to keep this property). The verifier first matches the
// signature size to a key class, then tries each enrolled key of
// that class. This avoids carrying an explicit algorithm tag on
// the wire.
//
// PEM is the loading format : operators paste the same PEM blocks
// SSH / cloud KMS export. `ssh-keygen -m PEM -e` for ed25519,
// `openssl rsa -pubout` for RSA.

package federation

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
)

// minRSAKeyBits is the floor for enrolled RSA admin keys. 2048 is
// the NIST SP 800-131A "acceptable" lower bound until ~2030 ; we
// reject smaller keys at enrolment so the runtime sig-size dispatch
// stays unambiguous (an RSA-1024 signature is 128 bytes, which is
// uncomfortably close to ed25519's 64).
const minRSAKeyBits = 2048

// AdminKey is one enrolled public key. Algorithm is set by the
// loader and reflects the underlying crypto type ; callers don't
// pick it.
type AdminKey struct {
	Algorithm string         // "ed25519" or "rsa"
	Comment   string         // operator hint, e.g. "alice@acme.org" — not trusted
	ed25519   ed25519.PublicKey
	rsa       *rsa.PublicKey
}

// AdminKeyVerifier accepts a manifest signed by ANY enrolled
// public key. Zero value rejects everything (deny-all behaviour
// matches DenyAllVerifier — fail closed when no keys are wired).
type AdminKeyVerifier struct {
	Keys []AdminKey
}

// Verify implements Verifier.
//
// Empty Keys → fail closed. Mismatched signature → first enrolled
// key class wins the dispatch ; a 64-byte sig is tried against
// every ed25519 key, a 256+-byte sig against every RSA key.
func (v AdminKeyVerifier) Verify(m *FederationManifest, sig []byte) error {
	if len(v.Keys) == 0 {
		return errors.New("federation: AdminKeyVerifier has no enrolled keys (deny-all)")
	}
	body, err := m.Marshal()
	if err != nil {
		return err
	}
	switch len(sig) {
	case ed25519.SignatureSize: // 64
		for _, k := range v.Keys {
			if k.Algorithm != "ed25519" {
				continue
			}
			if ed25519.Verify(k.ed25519, body, sig) {
				return nil
			}
		}
		return errors.New("federation: ed25519 signature did not match any enrolled admin key")
	default:
		// Anything else we treat as RSA-PKCS1v15-SHA256. Real
		// sig sizes : 256 (RSA-2048), 384 (3072), 512 (4096).
		// We don't gate on these exact values — VerifyPKCS1v15
		// returns a clean error for length mismatches.
		hash := sha256.Sum256(body)
		var lastErr error
		for _, k := range v.Keys {
			if k.Algorithm != "rsa" {
				continue
			}
			if err := rsa.VerifyPKCS1v15(k.rsa, crypto.SHA256, hash[:], sig); err == nil {
				return nil
			} else {
				lastErr = err
			}
		}
		if lastErr != nil {
			return fmt.Errorf("federation: RSA signature did not match any enrolled admin key (last err: %w)", lastErr)
		}
		return errors.New("federation: no RSA admin key enrolled to verify this signature")
	}
}

// AppendKey enrols an additional admin key. Returns a new
// verifier (value receiver) so the original is unchanged ; mirrors
// the immutability we want around trust roots.
func (v AdminKeyVerifier) AppendKey(k AdminKey) AdminKeyVerifier {
	out := AdminKeyVerifier{Keys: make([]AdminKey, 0, len(v.Keys)+1)}
	out.Keys = append(out.Keys, v.Keys...)
	out.Keys = append(out.Keys, k)
	return out
}

// LoadAdminKeysFromPEM walks a PEM blob (one or more
// `-----BEGIN PUBLIC KEY-----` blocks) and returns the parsed
// AdminKey slice. The blob can mix ed25519 and RSA freely. Empty
// input or a blob without any usable key returns an error — the
// caller almost certainly didn't mean "deny-all".
//
// Comments are read from blocks that look like the SSH PEM
// variant (the `Comment:` header SSH adds when exporting). The
// comment is informational only ; verification ignores it.
func LoadAdminKeysFromPEM(b []byte) ([]AdminKey, error) {
	var keys []AdminKey
	rest := b
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "PUBLIC KEY" && block.Type != "RSA PUBLIC KEY" {
			// Skip private keys, CSRs, certs — the operator
			// might paste a bundle. Don't error : the next
			// block may be what we want.
			continue
		}
		key, err := parseAdminKeyBlock(block)
		if err != nil {
			return nil, fmt.Errorf("federation: parse admin key block: %w", err)
		}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return nil, errors.New("federation: no PUBLIC KEY blocks found in admin-key PEM input")
	}
	return keys, nil
}

func parseAdminKeyBlock(block *pem.Block) (AdminKey, error) {
	comment := block.Headers["Comment"]
	if block.Type == "RSA PUBLIC KEY" {
		// PKCS#1-style RSA-only block (rare, but `openssl rsa
		// -RSAPublicKey_out` produces this).
		pub, err := x509.ParsePKCS1PublicKey(block.Bytes)
		if err != nil {
			return AdminKey{}, fmt.Errorf("PKCS#1 RSA: %w", err)
		}
		if pub.N.BitLen() < minRSAKeyBits {
			return AdminKey{}, fmt.Errorf("RSA key is %d bits ; minimum is %d", pub.N.BitLen(), minRSAKeyBits)
		}
		return AdminKey{Algorithm: "rsa", Comment: comment, rsa: pub}, nil
	}
	// Generic SubjectPublicKeyInfo — covers everything else
	// (ed25519, ECDSA, RSA-as-SPKI). We only accept ed25519
	// and RSA today.
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return AdminKey{}, fmt.Errorf("PKIX SPKI: %w", err)
	}
	switch p := pub.(type) {
	case ed25519.PublicKey:
		if len(p) != ed25519.PublicKeySize {
			return AdminKey{}, fmt.Errorf("ed25519 key has %d bytes ; want %d", len(p), ed25519.PublicKeySize)
		}
		return AdminKey{Algorithm: "ed25519", Comment: comment, ed25519: p}, nil
	case *rsa.PublicKey:
		if p.N.BitLen() < minRSAKeyBits {
			return AdminKey{}, fmt.Errorf("RSA key is %d bits ; minimum is %d", p.N.BitLen(), minRSAKeyBits)
		}
		return AdminKey{Algorithm: "rsa", Comment: comment, rsa: p}, nil
	default:
		return AdminKey{}, fmt.Errorf("unsupported public key type %T (accept ed25519 or RSA-2048+)", pub)
	}
}

// SignRSA produces an RSA-PKCS1v15-SHA256 signature over the
// manifest's canonical JSON bytes. Pair with AdminKeyVerifier
// (an enrolled RSA AdminKey verifies these). Pinning to PKCS#1
// v1.5 keeps the verify side simple ; PSS is fine theoretically
// but PSS sig blob length varies, which would complicate the
// implicit scheme dispatch in Verify.
func (m *FederationManifest) SignRSA(priv *rsa.PrivateKey) ([]byte, error) {
	if priv == nil {
		return nil, errors.New("federation: nil RSA private key")
	}
	if priv.N.BitLen() < minRSAKeyBits {
		return nil, fmt.Errorf("federation: RSA private key is %d bits ; minimum is %d", priv.N.BitLen(), minRSAKeyBits)
	}
	body, err := m.Marshal()
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(body)
	return rsa.SignPKCS1v15(nil, priv, crypto.SHA256, hash[:])
}
