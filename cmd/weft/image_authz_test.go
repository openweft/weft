package main

// image_authz_test.go pins the role check on the host-global image cache
// mutations: a project-scoped caller is refused, a platform admin is not.
// The admin calls use inputs that never touch the network or the cache
// (empty url / empty ops / empty config dir).

import (
	"context"
	"testing"

	weftv1 "github.com/openweft/weft-proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestImageCacheMutationsNeedAdmin(t *testing.T) {
	s, adp := newSnapshotTestServer(t)
	s.cfgDir = t.TempDir()
	proj, _, err := adp.CreateProject("img-authz")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		call func(ctx context.Context) error
	}{
		{"PullImages", func(ctx context.Context) error {
			_, err := s.PullImages(ctx, &weftv1.PullImagesRequest{ConfigDir: t.TempDir()})
			return err
		}},
		{"PullImage", func(ctx context.Context) error {
			_, err := s.PullImage(ctx, &weftv1.PullImageRequest{})
			return err
		}},
		{"PatchImage", func(ctx context.Context) error {
			_, err := s.PatchImage(ctx, &weftv1.PatchImageRequest{Url: "u"})
			return err
		}},
		{"CleanImages", func(ctx context.Context) error {
			_, err := s.CleanImages(ctx, &weftv1.CleanImagesRequest{ConfigDir: t.TempDir()})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(scopedCtx(proj.UUID)); status.Code(err) != codes.PermissionDenied {
				t.Errorf("project-scoped caller: got %v, want PermissionDenied", err)
			}
			if err := tc.call(devCtx()); status.Code(err) == codes.PermissionDenied {
				t.Errorf("admin caller refused: %v", err)
			}
		})
	}
}
