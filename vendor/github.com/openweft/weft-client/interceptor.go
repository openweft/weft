// Package weftclient — interceptor.go installs a gRPC client-side
// interceptor that attaches the cached OIDC access token to every
// outgoing call as `Authorization: Bearer <…>`. Used by weft and
// weft-microvm so the operator who ran `weft login` doesn't have to thread
// the token through every CLI flag.
//
// Absence of a cached token (or an expired one without a refresh
// path wired yet) means: send no Authorization header at all. The
// server side (weft) treats that as a dev-mode caller when its
// validator is unset, and as Unauthenticated when it isn't —
// matching the contract in pkg/openweft/weft/auth.go.
package weftclient

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// BearerInterceptor returns a unary client interceptor that
// stamps the Authorization header onto every outgoing call. The
// `tokenSource` is called per request so a future refresh-token
// path can rotate tokens without restarting the client.
//
// `tokenSource` returning an empty string means "send no
// Authorization header"; the request goes through unchanged.
func BearerInterceptor(tokenSource func() string) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if tok := tokenSource(); tok != "" {
			ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tok)
		}
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

// BearerStreamInterceptor is the streaming counterpart.
func BearerStreamInterceptor(tokenSource func() string) grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		if tok := tokenSource(); tok != "" {
			ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tok)
		}
		return streamer(ctx, desc, cc, method, opts...)
	}
}

// CachedTokenSource is the canonical tokenSource closure: it
// reads the on-disk cache on every call. Cheap (one file read on
// disk, kernel page cache makes it near-free), correct (picks up
// a fresh token after `weft login` without restarting), and
// failure-safe (returns "" rather than panicking on an unreadable
// cache).
func CachedTokenSource() func() string {
	return func() string {
		t, err := LoadCachedToken()
		if err != nil || t == nil {
			return ""
		}
		return t.Bearer()
	}
}
