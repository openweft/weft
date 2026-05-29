package weft

import (
	"strings"
	"testing"

	jwt "github.com/nats-io/jwt/v2"
	"github.com/nats-io/nkeys"
)

// TestMintOperator pins the operator-NKey shape : seed starts
// with `SO`, public with `O`, JWT decodes back to a claim with
// the right subject + name.
func TestMintOperator(t *testing.T) {
	op, err := MintOperator("weft-test")
	if err != nil {
		t.Fatalf("MintOperator: %v", err)
	}
	if !strings.HasPrefix(string(op.Seed), "SO") {
		t.Errorf("seed prefix = %q, want SO…", string(op.Seed)[:2])
	}
	if !strings.HasPrefix(op.Public, "O") {
		t.Errorf("public prefix = %q, want O…", op.Public[:1])
	}
	claims, err := jwt.DecodeOperatorClaims(op.JWT)
	if err != nil {
		t.Fatalf("DecodeOperatorClaims: %v", err)
	}
	if claims.Subject != op.Public {
		t.Errorf("operator subject mismatch : %q vs %q", claims.Subject, op.Public)
	}
	if claims.Name != "weft-test" {
		t.Errorf("operator name = %q, want weft-test", claims.Name)
	}
}

// TestMintAccount_SignedByOperator pins the trust chain : an
// account JWT must be signed by the operator. The DecodeAccountClaims
// step succeeds for shape ; the explicit Issuer check confirms the
// signature came from our operator pubkey.
func TestMintAccount_SignedByOperator(t *testing.T) {
	op, err := MintOperator("weft-test")
	if err != nil {
		t.Fatalf("MintOperator: %v", err)
	}
	acc, err := MintAccount(op.Seed, "project-alpha")
	if err != nil {
		t.Fatalf("MintAccount: %v", err)
	}
	if !strings.HasPrefix(string(acc.Seed), "SA") {
		t.Errorf("account seed prefix = %q, want SA…", string(acc.Seed)[:2])
	}
	if !strings.HasPrefix(acc.Public, "A") {
		t.Errorf("account public prefix = %q, want A…", acc.Public[:1])
	}
	claims, err := jwt.DecodeAccountClaims(acc.JWT)
	if err != nil {
		t.Fatalf("DecodeAccountClaims: %v", err)
	}
	if claims.Issuer != op.Public {
		t.Errorf("account issuer = %q, want operator %q", claims.Issuer, op.Public)
	}
	if claims.Name != "project-alpha" {
		t.Errorf("account name = %q, want project-alpha", claims.Name)
	}
}

// TestMintUser_PermissionsAndSignature pins the user JWT path :
// signed by the account, carries the supplied subscribe + publish
// permissions, NKey re-derives the same pubkey.
func TestMintUser_PermissionsAndSignature(t *testing.T) {
	op, _ := MintOperator("weft-test")
	acc, _ := MintAccount(op.Seed, "project-alpha")
	u, err := MintUser(acc.Seed, "project:alpha",
		[]string{"weft.events.project.alpha.events.>"},
		[]string{"weft.events.project.alpha.app.>"})
	if err != nil {
		t.Fatalf("MintUser: %v", err)
	}
	if !strings.HasPrefix(string(u.Seed), "SU") {
		t.Errorf("user seed prefix = %q, want SU…", string(u.Seed)[:2])
	}
	// Re-derive the public key from the seed — guards against
	// the helper returning seed + public from different KPs.
	kp, err := nkeys.FromSeed(u.Seed)
	if err != nil {
		t.Fatalf("nkeys.FromSeed: %v", err)
	}
	derived, _ := kp.PublicKey()
	if derived != u.Public {
		t.Errorf("seed/public mismatch: %q vs %q", derived, u.Public)
	}
	claims, err := jwt.DecodeUserClaims(u.JWT)
	if err != nil {
		t.Fatalf("DecodeUserClaims: %v", err)
	}
	if claims.Issuer != acc.Public {
		t.Errorf("user issuer = %q, want account %q", claims.Issuer, acc.Public)
	}
	subs := []string(claims.Sub.Allow)
	if len(subs) != 1 || subs[0] != "weft.events.project.alpha.events.>" {
		t.Errorf("subscribe.allow = %v, want one entry", subs)
	}
	pubs := []string(claims.Pub.Allow)
	if len(pubs) != 1 || pubs[0] != "weft.events.project.alpha.app.>" {
		t.Errorf("publish.allow = %v, want one entry", pubs)
	}
}

// TestFormatCredsFile pins the .creds file shape — the nats-go
// client's `nats.UserCredentials("file")` reads this format,
// so the deployer's per-VM materialisation has to emit the
// exact decorated-block layout.
func TestFormatCredsFile(t *testing.T) {
	op, _ := MintOperator("weft-test")
	acc, _ := MintAccount(op.Seed, "alpha")
	u, _ := MintUser(acc.Seed, "u-1", nil, nil)
	creds, err := FormatCredsFile(u)
	if err != nil {
		t.Fatalf("FormatCredsFile: %v", err)
	}
	s := string(creds)
	for _, want := range []string{
		"BEGIN NATS USER JWT",
		"END NATS USER JWT",
		"BEGIN USER NKEY SEED",
		"END USER NKEY SEED",
		u.JWT,
		string(u.Seed),
	} {
		if !strings.Contains(s, want) {
			t.Errorf("creds file missing %q", want)
		}
	}
}
