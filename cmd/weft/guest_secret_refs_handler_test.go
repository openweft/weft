package main

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	weftv1 "github.com/openweft/weft-proto"
)

func TestGuestSecretRefHandlers(t *testing.T) {
	s, _ := newSnapshotTestServer(t)
	ctx := devCtx()
	anon := context.Background()

	if _, err := s.SetGuestSecretRef(anon, &weftv1.SetGuestSecretRefRequest{SecretId: "k", Ref: "file:/x/k"}); err == nil {
		t.Fatal("set without a caller")
	}
	if _, err := s.DeleteGuestSecretRef(anon, &weftv1.DeleteGuestSecretRefRequest{SecretId: "k"}); err == nil {
		t.Fatal("delete without a caller")
	}
	if _, err := s.ListGuestSecretRefs(anon, &weftv1.ListGuestSecretRefsRequest{}); err == nil {
		t.Fatal("list without a caller")
	}
	if _, err := s.SetPodSpec(anon, &weftv1.SetPodSpecRequest{PodId: "p"}); err == nil {
		t.Fatal("set pod spec without a caller")
	}

	if _, err := s.SetGuestSecretRef(ctx, &weftv1.SetGuestSecretRefRequest{SecretId: "k", Ref: "env:HOME"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad ref: %v", err)
	}
	if _, err := s.SetGuestSecretRef(ctx, &weftv1.SetGuestSecretRefRequest{SecretId: "BAD", Ref: "file:/x/k"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad id: %v", err)
	}
	for _, id := range []string{"b", "a"} {
		if _, err := s.SetGuestSecretRef(ctx, &weftv1.SetGuestSecretRefRequest{SecretId: id, Ref: "file:/x/" + id}); err != nil {
			t.Fatal(err)
		}
	}
	r, err := s.SetGuestSecretRef(ctx, &weftv1.SetGuestSecretRefRequest{SecretId: "a", Ref: "file:/x/a2"})
	if err != nil || r.Version != 2 {
		t.Fatalf("rotation: %v %v", r, err)
	}
	l, err := s.ListGuestSecretRefs(ctx, &weftv1.ListGuestSecretRefsRequest{})
	if err != nil || len(l.Refs) != 2 || l.Refs[0].SecretId != "a" || l.Refs[0].Ref != "file:/x/a2" || l.Refs[1].SecretId != "b" {
		t.Fatalf("list: %v %v", l, err)
	}
	if _, err := s.DeleteGuestSecretRef(ctx, &weftv1.DeleteGuestSecretRefRequest{SecretId: "BAD"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("delete bad id: %v", err)
	}
	if _, err := s.DeleteGuestSecretRef(ctx, &weftv1.DeleteGuestSecretRefRequest{SecretId: "a"}); err != nil {
		t.Fatal(err)
	}
}

func TestSetPodSpecGrantsAndRefuses(t *testing.T) {
	s, adp := newSnapshotTestServer(t)
	ctx := devCtx()
	if _, err := s.SetPodSpec(ctx, &weftv1.SetPodSpecRequest{});status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty pod: %v", err)
	}
	if _, err := s.SetPodSpec(ctx, &weftv1.SetPodSpecRequest{PodId: "p", SpecJson: []byte("{nope")}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad json: %v", err)
	}
	spec := []byte(`{"wireguard":{"privateKeyRef":"wg"}}`)
	if _, err := s.SetPodSpec(ctx, &weftv1.SetPodSpecRequest{PodId: "p", SpecJson: spec}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("unmapped secret: %v", err)
	}
	if _, err := s.SetGuestSecretRef(ctx, &weftv1.SetGuestSecretRefRequest{SecretId: "wg", Ref: "file:/x/wg"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetPodSpec(ctx, &weftv1.SetPodSpecRequest{PodId: "p", SpecJson: spec}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := adp.GuestSecretGrantFor("p", "wg"); !ok {
		t.Fatal("no grant after publish")
	}
	if _, err := s.SetPodSpec(ctx, &weftv1.SetPodSpecRequest{PodId: "p", SpecJson: []byte("null")}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := adp.GuestSecretGrantFor("p", "wg"); ok {
		t.Fatal("grant survived deletion")
	}
}
