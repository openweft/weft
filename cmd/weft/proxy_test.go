package main

import (
	"context"
	"errors"
	"testing"
)

// TestBootProxy_RequiresHostUUID covers the input-validation gate. Other
// tests would need a real Caddy binary on PATH (or a mock subprocess) and
// fit better as acceptance tests gated behind a build tag — the lifecycle
// is exercised end-to-end by the proxy package's own tests; this one just
// asserts the CLI-shim contract.
func TestBootProxy_RequiresHostUUID(t *testing.T) {
	_, err := bootProxy(context.Background(), "", nil, proxyOpts{})
	if err == nil {
		t.Fatal("expected error when hostUUID is empty")
	}
	if !errors.Is(err, err) /* placeholder: error type isn't sentinel */ {
		// We don't assert the exact message because it's user-facing
		// copy that may evolve; the existence of an error is the
		// contract.
	}
}
