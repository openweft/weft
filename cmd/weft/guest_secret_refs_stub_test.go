package main

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/openweft/weft"
	weftv1 "github.com/openweft/weft-proto"
)

// nonValidatingAdapter accepts anything, so only the handler's own guard can
// refuse a bad reference (defense in depth must be observable).
type nonValidatingAdapter struct {
	weft.VZAdapter
	calls int
}

func (n *nonValidatingAdapter) SetGuestSecretRef(string, string) (uint64, error) {
	n.calls++
	return 7, nil
}

func TestSetGuestSecretRefHandlerValidatesByItself(t *testing.T) {
	adp := &nonValidatingAdapter{}
	s := &weftServer{adp: adp}
	ctx := devCtx()
	for _, ref := range []string{"", "env:HOME", "file:relative", "file:/a/../b", "file:/a\x01b"} {
		_, err := s.SetGuestSecretRef(ctx, &weftv1.SetGuestSecretRefRequest{SecretId: "k", Ref: ref})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("ref %q: %v", ref, err)
		}
	}
	if adp.calls != 0 {
		t.Fatalf("the adapter was reached %d times with a bad ref", adp.calls)
	}
	r, err := s.SetGuestSecretRef(ctx, &weftv1.SetGuestSecretRefRequest{SecretId: "k", Ref: "file:/x/k"})
	if err != nil || r.Version != 7 || adp.calls != 1 {
		t.Fatalf("good ref: %v %v calls=%d", r, err, adp.calls)
	}
}
