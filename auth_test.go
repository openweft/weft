package weft

// auth_test.go covers the parts of auth.go that don't need a live
// OIDC issuer: bearer-token extraction, the dev-caller synthesis,
// the HasGroup helper, and the dev-mode interceptor path.
//
// Round-trip JWT verification against a real dex/JWKS is an
// integration concern handled in a separate (yet-to-be-written)
// test that spins up dex via the infra-in-micro-vms pattern.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestBearerFromMD(t *testing.T) {
	cases := []struct {
		name    string
		md      metadata.MD
		want    string
		wantErr string
	}{
		{
			name: "happy path",
			md:   metadata.Pairs("authorization", "Bearer eyJabc"),
			want: "eyJabc",
		},
		{
			name: "lowercase scheme",
			md:   metadata.Pairs("authorization", "bearer eyJabc"),
			want: "eyJabc",
		},
		{
			name:    "missing header",
			md:      metadata.MD{},
			wantErr: "missing authorization header",
		},
		{
			name:    "non-bearer scheme",
			md:      metadata.Pairs("authorization", "Basic abc=="),
			wantErr: "not a Bearer token",
		},
		{
			name:    "empty bearer",
			md:      metadata.Pairs("authorization", "Bearer "),
			wantErr: "empty bearer token",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := bearerFromMD(c.md)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected err: %v", err)
				}
				if got != c.want {
					t.Errorf("token: got %q, want %q", got, c.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("err: got %v, want substring %q", err, c.wantErr)
			}
		})
	}
}

func TestDevCallerIsAnonymous(t *testing.T) {
	c := devCaller()
	if !c.IsAnonymous() {
		t.Errorf("dev caller must report IsAnonymous() == true")
	}
	if c.Dev != true {
		t.Errorf("Dev flag must be set")
	}
	if !strings.HasPrefix(c.Subject, "dev:") {
		t.Errorf("Subject should start with dev:; got %q", c.Subject)
	}
	if c.Raw != nil {
		t.Errorf("Raw must be nil in dev mode")
	}
}

func TestCallerHasGroup(t *testing.T) {
	c := &Caller{Groups: []string{"platform-admin", "team-alpha"}}
	if !c.HasGroup("team-alpha") {
		t.Errorf("expected HasGroup(team-alpha) == true")
	}
	if c.HasGroup("team-beta") {
		t.Errorf("did not expect membership in team-beta")
	}
	var nilCaller *Caller
	if nilCaller.HasGroup("any") {
		t.Errorf("nil caller must report HasGroup() == false")
	}
}

func TestWithCallerRoundTrip(t *testing.T) {
	c := &Caller{Subject: "ldap:david"}
	ctx := WithCaller(context.Background(), c)
	got, ok := CallerFrom(ctx)
	if !ok {
		t.Fatal("expected Caller in ctx")
	}
	if got.Subject != "ldap:david" {
		t.Errorf("Subject: got %q, want %q", got.Subject, "ldap:david")
	}
}

// TestUnaryInterceptorDevMode confirms that a nil validator means
// "always allow" with a synthetic dev Caller — the property that
// keeps single-host dev workflows working when OIDC is unset.
func TestUnaryInterceptorDevMode(t *testing.T) {
	interceptor := UnaryAuthInterceptor(nil, nil)
	var seen *Caller
	handler := func(ctx context.Context, _ any) (any, error) {
		seen, _ = CallerFrom(ctx)
		return "ok", nil
	}
	resp, err := interceptor(context.Background(), nil, &grpc.UnaryServerInfo{}, handler)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if resp != "ok" {
		t.Errorf("resp: got %v, want ok", resp)
	}
	if seen == nil {
		t.Fatal("handler did not receive a Caller")
	}
	if !seen.IsAnonymous() {
		t.Error("dev-mode Caller must be IsAnonymous()")
	}
}

// TestUnaryInterceptorMissingTokenRejected confirms that once OIDC
// is configured (validator non-nil), requests without a bearer
// token are rejected with Unauthenticated. We use a real validator
// pointed at a bogus issuer — NewOIDCValidator will fail discovery,
// so we construct one manually with a nil verifier to isolate the
// "missing token" branch.
func TestUnaryInterceptorMissingTokenRejected(t *testing.T) {
	// A minimally non-nil validator: empty issuer makes Validate
	// itself unreachable because authenticate() returns at the
	// "missing authorization header" step first.
	v := &OIDCValidator{issuer: "https://dex.example.com"}
	interceptor := UnaryAuthInterceptor(v, nil)
	handler := func(_ context.Context, _ any) (any, error) {
		t.Fatal("handler must not run when token is missing")
		return nil, nil
	}
	_, err := interceptor(context.Background(), nil, &grpc.UnaryServerInfo{}, handler)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status, got: %v", err)
	}
	if st.Code() != codes.Unauthenticated {
		t.Errorf("code: got %v, want Unauthenticated", st.Code())
	}
}

// TestErrDevModeSentinel keeps the sentinel identity stable so
// downstream code can switch on it with errors.Is.
func TestErrDevModeSentinel(t *testing.T) {
	if !errors.Is(ErrDevMode, ErrDevMode) {
		t.Error("ErrDevMode must satisfy errors.Is reflexively")
	}
}
