package weft

// auth.go is the OIDC bearer-token validation layer. Every weft
// gRPC request that arrives with an `Authorization: Bearer <jwt>`
// header passes through here:
//
//   1. The interceptor extracts the raw JWT from gRPC metadata.
//   2. OIDCValidator verifies the signature against the issuer's
//      JWKS (cached + auto-rotated by coreos/go-oidc).
//   3. Standard claims (sub / email / groups / aud / iss / exp)
//      are unpacked into a Caller struct.
//   4. The Caller is attached to the request context so RPC
//      handlers can pull it via CallerFrom(ctx) for ACL checks.
//
// Per [[cloud-platform-direction]] and [[oidc-server-dex]] the
// issuer in production is dex; tokens carry `groups` that map
// onto project membership. Phase-2 multitenancy (project ACLs)
// builds on top of the Caller struct this file defines.
//
// Dev mode: when no --oidc-issuer / oidc { } block is set, the
// interceptor still runs but treats every request as coming from
// a synthetic local user (Subject `dev:<os-user>`, no groups).
// That preserves the single-host workflow while keeping all
// downstream handler code uniform — they always see a non-nil
// Caller in ctx.

import (
	"context"
	"errors"
	"fmt"
	"os/user"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Caller is the authenticated identity behind a gRPC request.
// One per request; survives only for the lifetime of the handler.
//
// Subject is the stable identity (dex's `sub` claim — typically
// `<connector>:<id>` like `ldap:david.delavennat` or
// `github:david-delavennat`). Email + Groups are best-effort
// claims; absence is not an error.
//
// Raw retains the parsed *oidc.IDToken so handlers that need
// uncommon claims can `caller.Raw.Claims(&dst)` themselves.
type Caller struct {
	Subject string
	Email   string
	Groups  []string
	Issuer  string
	// Dev is true when the Caller was synthesised in dev mode (no
	// OIDC issuer configured). Handlers can use this to allow or
	// refuse operations that should require real authentication.
	Dev bool
	// Raw is nil in dev mode. Otherwise it's the verified token.
	Raw *oidc.IDToken
}

// IsAnonymous reports whether the caller is the dev-mode synthetic
// user. Equivalent to `c.Dev`, but reads better at call sites that
// want to refuse risky operations under dev.
func (c *Caller) IsAnonymous() bool { return c == nil || c.Dev }

// HasGroup is a small helper for ACL checks; case-sensitive
// match. Returns false for nil callers so chained ACL evaluation
// stays panic-safe.
func (c *Caller) HasGroup(g string) bool {
	if c == nil {
		return false
	}
	for _, x := range c.Groups {
		if x == g {
			return true
		}
	}
	return false
}

// callerCtxKey is the unexported key under which Caller is
// stashed in request contexts.
type callerCtxKey struct{}

// WithCaller returns a derived context that carries the Caller.
// Interceptors call this once per request; handlers consume it
// via CallerFrom.
func WithCaller(ctx context.Context, c *Caller) context.Context {
	return context.WithValue(ctx, callerCtxKey{}, c)
}

// CallerFrom retrieves the Caller previously attached by
// WithCaller. (nil, false) when no Caller is present — only
// possible when the interceptor wasn't installed, i.e. in test
// code that exercises handlers directly.
func CallerFrom(ctx context.Context) (*Caller, bool) {
	c, ok := ctx.Value(callerCtxKey{}).(*Caller)
	return c, ok
}

// devCaller returns a synthetic Caller for dev mode. Memoised so
// the OS user lookup happens once per process.
func devCaller() *Caller {
	u, err := user.Current()
	sub := "dev:unknown"
	if err == nil && u.Username != "" {
		sub = "dev:" + u.Username
	}
	return &Caller{
		Subject: sub,
		Issuer:  "weft:dev",
		Dev:     true,
	}
}

// ----------------------------------------------------------------
// OIDCValidator — production token verifier.
// ----------------------------------------------------------------

// OIDCValidator wraps coreos/go-oidc's IDTokenVerifier and
// remembers the issuer URL for diagnostic messages. nil receivers
// are valid and put the caller in dev mode.
type OIDCValidator struct {
	issuer   string
	verifier *oidc.IDTokenVerifier
}

// OIDCConfig is the construction surface for OIDCValidator. All
// fields except Issuer are optional; the defaults match dex.
type OIDCConfig struct {
	// Issuer is the OIDC issuer URL (e.g. https://dex.internal.example.com).
	// Empty disables OIDC — interceptor falls through to dev mode.
	Issuer string
	// ClientID is the audience the token must be issued for. Set
	// to the OAuth client name weft was registered under in dex
	// (typically "weft"). Empty disables audience checking, which
	// is fine for single-tenant deployments but dangerous for
	// multi-tenant; warn when this is empty.
	ClientID string
	// SkipClientIDCheck, when true, accepts tokens regardless of
	// `aud`. Use only for trusted multi-client setups.
	SkipClientIDCheck bool
	// SkipExpiryCheck disables exp validation — TESTS ONLY.
	SkipExpiryCheck bool
	// SkipIssuerCheck disables iss validation — TESTS ONLY.
	SkipIssuerCheck bool
}

// NewOIDCValidator constructs a validator by fetching the issuer's
// OpenID configuration + JWKS. Returns (nil, nil) when cfg.Issuer
// is empty so callers can treat "no OIDC configured" as a
// non-error.
func NewOIDCValidator(ctx context.Context, cfg OIDCConfig) (*OIDCValidator, error) {
	if cfg.Issuer == "" {
		return nil, nil
	}
	provider, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc provider discovery for %q: %w", cfg.Issuer, err)
	}
	vCfg := &oidc.Config{
		ClientID:          cfg.ClientID,
		SkipClientIDCheck: cfg.SkipClientIDCheck || cfg.ClientID == "",
		SkipExpiryCheck:   cfg.SkipExpiryCheck,
		SkipIssuerCheck:   cfg.SkipIssuerCheck,
	}
	return &OIDCValidator{
		issuer:   cfg.Issuer,
		verifier: provider.Verifier(vCfg),
	}, nil
}

// Issuer returns the configured issuer URL (or "" in dev mode).
func (v *OIDCValidator) Issuer() string {
	if v == nil {
		return ""
	}
	return v.issuer
}

// Validate verifies a raw JWT and returns a populated Caller.
// On a nil receiver returns (nil, ErrDevMode) so the interceptor
// knows to synthesise a dev caller instead.
func (v *OIDCValidator) Validate(ctx context.Context, raw string) (*Caller, error) {
	if v == nil {
		return nil, ErrDevMode
	}
	tok, err := v.verifier.Verify(ctx, raw)
	if err != nil {
		return nil, fmt.Errorf("verify token: %w", err)
	}
	var claims struct {
		Email  string   `json:"email"`
		Groups []string `json:"groups"`
	}
	if err := tok.Claims(&claims); err != nil {
		// Token verified but claims unmarshal failed — surface as
		// a hard failure since downstream ACL checks would silently
		// see an empty Groups slice otherwise.
		return nil, fmt.Errorf("decode claims: %w", err)
	}
	return &Caller{
		Subject: tok.Subject,
		Email:   claims.Email,
		Groups:  claims.Groups,
		Issuer:  tok.Issuer,
		Raw:     tok,
	}, nil
}

// ErrDevMode marks the validator as not configured.
var ErrDevMode = errors.New("oidc: validator not configured (dev mode)")

// ----------------------------------------------------------------
// gRPC interceptors.
// ----------------------------------------------------------------

// UserPersister is invoked fire-and-forget after a non-dev Caller
// is successfully validated. main passes Adapter.RegisterUser so
// every authenticated request refreshes the user's last_seen +
// group cache in the registry. Nil disables the persist hook —
// useful in tests that don't bootstrap a full Adapter.
type UserPersister func(*Caller)

// UnaryAuthInterceptor returns a grpc.UnaryServerInterceptor that
// validates the bearer token, attaches a Caller to ctx, runs the
// optional UserPersister, and rejects unauthenticated requests
// when the validator is configured. In dev mode (validator == nil)
// it always attaches a devCaller and forwards — and skips the
// persister, since synthetic dev users would clutter the registry.
func UnaryAuthInterceptor(v *OIDCValidator, persist UserPersister) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		c, err := authenticate(ctx, v)
		if err != nil {
			return nil, err
		}
		if persist != nil && c != nil && !c.Dev {
			persist(c)
		}
		return handler(WithCaller(ctx, c), req)
	}
}

// StreamAuthInterceptor is the streaming counterpart of
// UnaryAuthInterceptor. Wraps the server stream so the embedded
// ctx carries the Caller for the whole stream lifetime, and runs
// the persister exactly once at stream open (not per message).
func StreamAuthInterceptor(v *OIDCValidator, persist UserPersister) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		c, err := authenticate(ss.Context(), v)
		if err != nil {
			return err
		}
		if persist != nil && c != nil && !c.Dev {
			persist(c)
		}
		wrapped := &authedStream{ServerStream: ss, ctx: WithCaller(ss.Context(), c)}
		return handler(srv, wrapped)
	}
}

type authedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *authedStream) Context() context.Context { return s.ctx }

// authenticate extracts and validates a bearer token from the
// incoming gRPC metadata. Returns the synthesised dev Caller when
// the validator is nil. Returns gRPC status errors so the
// interceptors can return them directly.
func authenticate(ctx context.Context, v *OIDCValidator) (*Caller, error) {
	if v == nil {
		return devCaller(), nil
	}
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "missing gRPC metadata")
	}
	raw, err := bearerFromMD(md)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, err.Error())
	}
	verifyCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c, err := v.Validate(verifyCtx, raw)
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "invalid token: %v", err)
	}
	return c, nil
}

// bearerFromMD pulls the JWT out of `authorization: Bearer <jwt>`.
// Case-insensitive on the header name (gRPC normalises to lower
// case) and on the "Bearer" scheme prefix.
func bearerFromMD(md metadata.MD) (string, error) {
	values := md.Get("authorization")
	if len(values) == 0 {
		return "", errors.New("missing authorization header")
	}
	for _, v := range values {
		const scheme = "bearer "
		if len(v) >= len(scheme) && strings.EqualFold(v[:len(scheme)], scheme) {
			tok := strings.TrimSpace(v[len(scheme):])
			if tok == "" {
				return "", errors.New("empty bearer token")
			}
			return tok, nil
		}
	}
	return "", errors.New("authorization header is not a Bearer token")
}
