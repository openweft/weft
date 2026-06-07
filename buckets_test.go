package weft

import (
	"context"
	"strings"
	"testing"
)

func TestBucketRegistry_EmptyStartsClean(t *testing.T) {
	reg, err := loadBucketRegistry(context.Background(), NewMemStorage())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := reg.list(""); len(got) != 0 {
		t.Errorf("fresh registry should be empty, got %d rows", len(got))
	}
}

func TestBucketRegistry_CreateRoundTrips(t *testing.T) {
	storage := NewMemStorage()
	reg, _ := loadBucketRegistry(context.Background(), storage)
	b, created, err := reg.create("proj-1", "logs", "https://s3.example.com", "eu-west-1", "AKID", "SECRET")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !created {
		t.Error("first create should report created=true")
	}
	if b.UUID == "" || len(b.UUID) != 36 {
		t.Errorf("UUID should be a 36-char RFC 4122 v4, got %q", b.UUID)
	}
	if b.ProjectUUID != "proj-1" || b.Name != "logs" || b.Endpoint != "https://s3.example.com" {
		t.Errorf("fields drifted: %+v", b)
	}
	// Reload from storage — secondary indices must be rebuilt.
	reg2, err := loadBucketRegistry(context.Background(), storage)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got, ok := reg2.lookupByUUID(b.UUID)
	if !ok || got.Name != "logs" {
		t.Fatalf("reload lost the bucket: %+v ok=%v", got, ok)
	}
}

func TestBucketRegistry_CreateIdempotent(t *testing.T) {
	reg, _ := loadBucketRegistry(context.Background(), NewMemStorage())
	b1, _, _ := reg.create("proj-1", "logs", "https://s3", "", "", "")
	b2, created, err := reg.create("proj-1", "logs", "https://other", "", "", "")
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if created {
		t.Error("repeat create on same (project,name) should report created=false")
	}
	if b2.UUID != b1.UUID {
		t.Error("repeat create should return the existing row, not mint a new UUID")
	}
	if b2.Endpoint != "https://s3" {
		t.Error("repeat create must NOT clobber existing fields")
	}
}

func TestBucketRegistry_CreateValidation(t *testing.T) {
	reg, _ := loadBucketRegistry(context.Background(), NewMemStorage())
	cases := []struct {
		name              string
		proj, n, endpoint string
		wantErrContains   string
	}{
		{"no project", "", "n", "https://s3", "project_uuid"},
		{"no name", "p", "", "https://s3", "name"},
		{"no endpoint", "p", "n", "", "endpoint"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := reg.create(tc.proj, tc.n, tc.endpoint, "", "", ""); err == nil {
				t.Error("expected error")
			} else if !strings.Contains(err.Error(), tc.wantErrContains) {
				t.Errorf("error must mention %q, got: %v", tc.wantErrContains, err)
			}
		})
	}
}

func TestBucketRegistry_ListFiltersByProject(t *testing.T) {
	reg, _ := loadBucketRegistry(context.Background(), NewMemStorage())
	_, _, _ = reg.create("proj-A", "a", "https://s3", "", "", "")
	_, _, _ = reg.create("proj-A", "b", "https://s3", "", "", "")
	_, _, _ = reg.create("proj-B", "c", "https://s3", "", "", "")
	if got := reg.list(""); len(got) != 3 {
		t.Errorf("list(\"\") should return all 3, got %d", len(got))
	}
	gotA := reg.list("proj-A")
	if len(gotA) != 2 {
		t.Errorf("list(proj-A) should return 2, got %d", len(gotA))
	}
	for _, b := range gotA {
		if b.ProjectUUID != "proj-A" {
			t.Errorf("project filter leaked: %+v", b)
		}
	}
	// List sorts by (project, name).
	gotAll := reg.list("")
	for i := 1; i < len(gotAll); i++ {
		if gotAll[i-1].ProjectUUID > gotAll[i].ProjectUUID {
			t.Errorf("not sorted by project: %+v", gotAll)
		}
	}
}

func TestBucketRegistry_SetPolicy(t *testing.T) {
	reg, _ := loadBucketRegistry(context.Background(), NewMemStorage())
	b, _, _ := reg.create("proj-1", "logs", "https://s3", "", "", "")
	upd, err := reg.setPolicy(b.UUID, `{"Version":"2012-10-17"}`)
	if err != nil {
		t.Fatalf("setPolicy: %v", err)
	}
	if upd.Policy != `{"Version":"2012-10-17"}` {
		t.Errorf("policy not applied: %q", upd.Policy)
	}
	// Unknown UUID.
	if _, err := reg.setPolicy("no-such", "{}"); err == nil {
		t.Error("setPolicy on unknown UUID must error")
	}
}

func TestBucketRegistry_DeleteCleansIndex(t *testing.T) {
	reg, _ := loadBucketRegistry(context.Background(), NewMemStorage())
	b, _, _ := reg.create("proj-1", "logs", "https://s3", "", "", "")
	if err := reg.delete(b.UUID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := reg.lookupByUUID(b.UUID); ok {
		t.Error("deleted row still surfaces via UUID lookup")
	}
	// Recreate must succeed (name index was cleaned).
	if _, _, err := reg.create("proj-1", "logs", "https://s3", "", "", ""); err != nil {
		t.Errorf("recreate after delete failed: %v", err)
	}
	// Unknown UUID.
	if err := reg.delete("no-such"); err == nil {
		t.Error("delete on unknown UUID must error")
	}
}
