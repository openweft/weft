package weft

// auth_oidc_test.go exercises the OIDC token-validation path
// (NewOIDCValidator / Validate / Issuer / authenticate /
// StreamAuthInterceptor) against a self-hosted httptest issuer:
// a minimal OpenID discovery doc + JWKS + RS256-signed tokens.
// No external dex / network dependency — everything is in-process.

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// oidcTestIssuer spins up a tiny OpenID provider for tests. It
// serves /.well-known/openid-configuration + a JWKS, and can mint
// RS256 tokens signed by the matching private key.
type oidcTestIssuer struct {
	server  *httptest.Server
	key     *rsa.PrivateKey
	keyID   string
	issuer  string
}

func newOIDCTestIssuer(t *testing.T) *oidcTestIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	it := &oidcTestIssuer{key: key, keyID: "test-key-1"}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 it.issuer,
			"jwks_uri":               it.issuer + "/keys",
			"authorization_endpoint": it.issuer + "/auth",
			"token_endpoint":         it.issuer + "/token",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		jwks := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key:       key.Public(),
			KeyID:     it.keyID,
			Algorithm: "RS256",
			Use:       "sig",
		}}}
		_ = json.NewEncoder(w).Encode(jwks)
	})
	it.server = httptest.NewServer(mux)
	it.issuer = it.server.URL
	t.Cleanup(it.server.Close)
	return it
}

// mintToken signs a JWT with the standard claims plus a `groups`
// claim, valid for one hour by default. We build the claim set by
// hand (the go-jose/jwt subpackage isn't vendored) and sign the
// JSON payload as a compact JWS — exactly what an ID token is.
func (it *oidcTestIssuer) mintToken(t *testing.T, subject, audience, email string, groups []string) string {
	t.Helper()
	sig, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: it.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", it.keyID),
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	claims := map[string]any{
		"iss": it.issuer,
		"sub": subject,
		"aud": audience,
		"exp": now.Add(time.Hour).Unix(),
		"iat": now.Unix(),
	}
	if email != "" {
		claims["email"] = email
	}
	if groups != nil {
		claims["groups"] = groups
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	jws, err := sig.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jws.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestNewOIDCValidator_EmptyIssuerReturnsNil(t *testing.T) {
	v, err := NewOIDCValidator(context.Background(), OIDCConfig{})
	if err != nil {
		t.Fatalf("empty issuer: %v", err)
	}
	if v != nil {
		t.Errorf("empty issuer should return nil validator")
	}
	// nil validator's Issuer() == "".
	if v.Issuer() != "" {
		t.Errorf("nil validator Issuer() should be empty")
	}
}

func TestNewOIDCValidator_DiscoveryFailure(t *testing.T) {
	// Pointing at a closed port → discovery fails.
	_, err := NewOIDCValidator(context.Background(), OIDCConfig{
		Issuer: "http://127.0.0.1:1/does-not-exist",
	})
	if err == nil {
		t.Errorf("discovery against dead endpoint should fail")
	}
}

func TestOIDCValidator_ValidateHappyPath(t *testing.T) {
	it := newOIDCTestIssuer(t)
	v, err := NewOIDCValidator(context.Background(), OIDCConfig{
		Issuer:   it.issuer,
		ClientID: "vzd",
	})
	if err != nil {
		t.Fatalf("NewOIDCValidator: %v", err)
	}
	if v.Issuer() != it.issuer {
		t.Errorf("Issuer() = %q, want %q", v.Issuer(), it.issuer)
	}

	token := it.mintToken(t, "ldap:alice", "vzd", "alice@example.com", []string{"team-alpha"})
	caller, err := v.Validate(context.Background(), token)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if caller.Subject != "ldap:alice" {
		t.Errorf("Subject = %q", caller.Subject)
	}
	if caller.Email != "alice@example.com" {
		t.Errorf("Email = %q", caller.Email)
	}
	if len(caller.Groups) != 1 || caller.Groups[0] != "team-alpha" {
		t.Errorf("Groups = %v", caller.Groups)
	}
	if caller.Issuer != it.issuer {
		t.Errorf("Issuer = %q", caller.Issuer)
	}
	if caller.Raw == nil {
		t.Errorf("Raw token should be set")
	}
}

func TestOIDCValidator_ValidateRejectsBadToken(t *testing.T) {
	it := newOIDCTestIssuer(t)
	v, err := NewOIDCValidator(context.Background(), OIDCConfig{
		Issuer:   it.issuer,
		ClientID: "vzd",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Validate(context.Background(), "garbage.token.value"); err == nil {
		t.Errorf("garbage token should be rejected")
	}
	// Wrong audience.
	wrongAud := it.mintToken(t, "ldap:bob", "other-client", "bob@x", nil)
	if _, err := v.Validate(context.Background(), wrongAud); err == nil {
		t.Errorf("wrong audience should be rejected")
	}
}

func TestOIDCValidator_NilReceiverIsDevMode(t *testing.T) {
	var v *OIDCValidator
	_, err := v.Validate(context.Background(), "anything")
	if err != ErrDevMode {
		t.Errorf("nil validator should return ErrDevMode, got %v", err)
	}
}

// ── Unary interceptor with a real validator + valid token ──────

func TestUnaryInterceptor_ValidToken(t *testing.T) {
	it := newOIDCTestIssuer(t)
	v, _ := NewOIDCValidator(context.Background(), OIDCConfig{Issuer: it.issuer, ClientID: "vzd"})
	token := it.mintToken(t, "ldap:carol", "vzd", "carol@x", nil)

	var persisted *Caller
	interceptor := UnaryAuthInterceptor(v, func(c *Caller) { persisted = c })

	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer "+token))

	var seen *Caller
	handler := func(ctx context.Context, _ any) (any, error) {
		seen, _ = CallerFrom(ctx)
		return "ok", nil
	}
	resp, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{}, handler)
	if err != nil {
		t.Fatalf("interceptor: %v", err)
	}
	if resp != "ok" {
		t.Errorf("resp = %v", resp)
	}
	if seen == nil || seen.Subject != "ldap:carol" {
		t.Errorf("handler got wrong caller: %+v", seen)
	}
	if persisted == nil || persisted.Subject != "ldap:carol" {
		t.Errorf("persister not invoked for non-dev caller")
	}
}

// ── Stream interceptor: dev mode + valid token + missing token ─

type fakeServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *fakeServerStream) Context() context.Context { return s.ctx }

func TestStreamInterceptor_DevMode(t *testing.T) {
	interceptor := StreamAuthInterceptor(nil, nil)
	var seen *Caller
	handler := func(_ any, ss grpc.ServerStream) error {
		seen, _ = CallerFrom(ss.Context())
		return nil
	}
	ss := &fakeServerStream{ctx: context.Background()}
	if err := interceptor(nil, ss, &grpc.StreamServerInfo{}, handler); err != nil {
		t.Fatalf("stream interceptor: %v", err)
	}
	if seen == nil || !seen.IsAnonymous() {
		t.Errorf("dev-mode stream caller should be anonymous: %+v", seen)
	}
}

func TestStreamInterceptor_ValidToken(t *testing.T) {
	it := newOIDCTestIssuer(t)
	v, _ := NewOIDCValidator(context.Background(), OIDCConfig{Issuer: it.issuer, ClientID: "vzd"})
	token := it.mintToken(t, "ldap:dave", "vzd", "dave@x", nil)

	var persisted *Caller
	interceptor := StreamAuthInterceptor(v, func(c *Caller) { persisted = c })

	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer "+token))
	ss := &fakeServerStream{ctx: ctx}

	var seen *Caller
	handler := func(_ any, s grpc.ServerStream) error {
		seen, _ = CallerFrom(s.Context())
		return nil
	}
	if err := interceptor(nil, ss, &grpc.StreamServerInfo{}, handler); err != nil {
		t.Fatalf("stream interceptor: %v", err)
	}
	if seen == nil || seen.Subject != "ldap:dave" {
		t.Errorf("stream handler got wrong caller: %+v", seen)
	}
	if persisted == nil {
		t.Errorf("persister should run once at stream open")
	}
}

func TestStreamInterceptor_MissingToken(t *testing.T) {
	v := &OIDCValidator{issuer: "https://dex.example.com"}
	interceptor := StreamAuthInterceptor(v, nil)
	ss := &fakeServerStream{ctx: context.Background()}
	handler := func(_ any, _ grpc.ServerStream) error {
		t.Fatal("handler must not run")
		return nil
	}
	if err := interceptor(nil, ss, &grpc.StreamServerInfo{}, handler); err == nil {
		t.Errorf("missing token should be rejected")
	}
}

// ── authenticate: validator set + valid token through metadata ─

func TestAuthenticate_ValidToken(t *testing.T) {
	it := newOIDCTestIssuer(t)
	v, _ := NewOIDCValidator(context.Background(), OIDCConfig{Issuer: it.issuer, ClientID: "vzd"})
	token := it.mintToken(t, "ldap:erin", "vzd", "erin@x", nil)
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer "+token))
	c, err := authenticate(ctx, v)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if c.Subject != "ldap:erin" {
		t.Errorf("Subject = %q", c.Subject)
	}
}

func TestAuthenticate_NoMetadata(t *testing.T) {
	v := &OIDCValidator{issuer: "https://x"}
	if _, err := authenticate(context.Background(), v); err == nil {
		t.Errorf("no metadata should error")
	}
}

func TestAuthenticate_InvalidToken(t *testing.T) {
	it := newOIDCTestIssuer(t)
	v, _ := NewOIDCValidator(context.Background(), OIDCConfig{Issuer: it.issuer, ClientID: "vzd"})
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer not-a-jwt"))
	if _, err := authenticate(ctx, v); err == nil {
		t.Errorf("invalid token should error")
	}
}
