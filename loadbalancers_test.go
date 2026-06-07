package weft

import (
	"context"
	"strings"
	"testing"
)

func TestLoadBalancerRegistry_EmptyStartsClean(t *testing.T) {
	reg, err := loadLoadBalancerRegistry(context.Background(), NewMemStorage())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := reg.list(""); len(got) != 0 {
		t.Errorf("fresh registry should be empty, got %d rows", len(got))
	}
}

func TestLoadBalancerRegistry_CreateRoundTrips(t *testing.T) {
	storage := NewMemStorage()
	reg, _ := loadLoadBalancerRegistry(context.Background(), storage)
	backends := []LBBackend{{Address: "10.0.0.1:80", Weight: 1}}
	lb, created, err := reg.create("proj-1", "web", ":443", "l7_https", backends)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !created {
		t.Error("first create should report created=true")
	}
	if lb.UUID == "" || len(lb.Backends) != 1 {
		t.Errorf("fields drifted: %+v", lb)
	}
	// Reload — name index must be rebuilt for idempotency check.
	reg2, err := loadLoadBalancerRegistry(context.Background(), storage)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got, ok := reg2.lookupByUUID(lb.UUID)
	if !ok || got.Name != "web" {
		t.Errorf("reload lost row: %+v ok=%v", got, ok)
	}
	// Idempotent create after reload.
	if _, created2, _ := reg2.create("proj-1", "web", ":443", "l7_https", nil); created2 {
		t.Error("name index not rebuilt — idempotency broke after reload")
	}
}

func TestLoadBalancerRegistry_CreateValidation(t *testing.T) {
	reg, _ := loadLoadBalancerRegistry(context.Background(), NewMemStorage())
	cases := []struct {
		name             string
		proj, n, addr, p string
		wantErrContains  string
	}{
		{"no project", "", "x", ":80", "l4_tcp", "project_uuid"},
		{"no name", "p", "", ":80", "l4_tcp", "name"},
		{"no addr", "p", "x", "", "l4_tcp", "listen_addr"},
		{"bad protocol", "p", "x", ":80", "ftp", "protocol"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := reg.create(tc.proj, tc.n, tc.addr, tc.p, nil); err == nil {
				t.Error("expected error")
			} else if !strings.Contains(err.Error(), tc.wantErrContains) {
				t.Errorf("error must mention %q, got: %v", tc.wantErrContains, err)
			}
		})
	}
}

func TestLoadBalancerRegistry_CreateIdempotent(t *testing.T) {
	reg, _ := loadLoadBalancerRegistry(context.Background(), NewMemStorage())
	lb1, _, _ := reg.create("p", "web", ":80", "l7_http", nil)
	lb2, created, err := reg.create("p", "web", ":443", "l7_https", nil)
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if created {
		t.Error("repeat create with same (project,name) should report created=false")
	}
	if lb2.UUID != lb1.UUID || lb2.ListenAddr != ":80" {
		t.Errorf("idempotent create must return existing row unchanged: %+v", lb2)
	}
}

func TestLoadBalancerRegistry_UpdatePatchAndRename(t *testing.T) {
	reg, _ := loadLoadBalancerRegistry(context.Background(), NewMemStorage())
	lb, _, _ := reg.create("p", "web", ":80", "l7_http", nil)

	upd, err := reg.update(lb.UUID, "webv2", "", "")
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if upd.Name != "webv2" {
		t.Errorf("rename failed: %+v", upd)
	}
	// Old name freed, new name taken — lookup via second create.
	if _, created, _ := reg.create("p", "webv2", ":80", "l7_http", nil); created {
		t.Error("new name must be marked taken after rename")
	}

	// Patch listen_addr only.
	upd2, _ := reg.update(lb.UUID, "", ":8080", "")
	if upd2.ListenAddr != ":8080" {
		t.Errorf("listen_addr patch failed: %+v", upd2)
	}

	// Bad protocol.
	if _, err := reg.update(lb.UUID, "", "", "junk"); err == nil {
		t.Error("invalid protocol must error")
	}
	if _, err := reg.update("no-such", "x", "", ""); err == nil {
		t.Error("update of unknown UUID must error")
	}
}

func TestLoadBalancerRegistry_RenameConflict(t *testing.T) {
	reg, _ := loadLoadBalancerRegistry(context.Background(), NewMemStorage())
	lb1, _, _ := reg.create("p", "web", ":80", "l7_http", nil)
	_, _, _ = reg.create("p", "api", ":80", "l7_http", nil)
	if _, err := reg.update(lb1.UUID, "api", "", ""); err == nil {
		t.Error("rename to already-used name must error")
	}
}

func TestLoadBalancerRegistry_SetBackends(t *testing.T) {
	reg, _ := loadLoadBalancerRegistry(context.Background(), NewMemStorage())
	lb, _, _ := reg.create("p", "web", ":80", "l7_http", nil)
	if len(lb.Backends) != 0 {
		t.Error("initial backends should be empty")
	}
	upd, err := reg.setBackends(lb.UUID, []LBBackend{{Address: "10.0.0.1:80", Weight: 1}, {Address: "10.0.0.2:80", Weight: 2}})
	if err != nil {
		t.Fatalf("setBackends: %v", err)
	}
	if len(upd.Backends) != 2 {
		t.Errorf("backends not stored: %+v", upd.Backends)
	}
	// Replace atomically with one backend.
	upd2, _ := reg.setBackends(lb.UUID, []LBBackend{{Address: "10.0.0.3:80", Weight: 5}})
	if len(upd2.Backends) != 1 || upd2.Backends[0].Address != "10.0.0.3:80" {
		t.Errorf("backend replace failed: %+v", upd2.Backends)
	}
	if _, err := reg.setBackends("no-such", nil); err == nil {
		t.Error("setBackends on unknown UUID must error")
	}
}

func TestLoadBalancerRegistry_DeleteRefusesCascade(t *testing.T) {
	reg, _ := loadLoadBalancerRegistry(context.Background(), NewMemStorage())
	lb, _, _ := reg.create("p", "web", ":80", "l7_http", nil)
	if _, err := reg.delete(lb.UUID, 2); err == nil {
		t.Error("delete with floating-ips > 0 must refuse")
	} else if !strings.Contains(err.Error(), "2 floating-ip") {
		t.Errorf("error should report blocking count, got: %v", err)
	}
}

func TestLoadBalancerRegistry_DeleteCleansIndex(t *testing.T) {
	reg, _ := loadLoadBalancerRegistry(context.Background(), NewMemStorage())
	lb, _, _ := reg.create("p", "web", ":80", "l7_http", nil)
	if _, err := reg.delete(lb.UUID, 0); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := reg.lookupByUUID(lb.UUID); ok {
		t.Error("deleted row still surfaces via UUID lookup")
	}
	// Recreate must succeed (name index freed).
	if _, created, err := reg.create("p", "web", ":80", "l7_http", nil); err != nil || !created {
		t.Errorf("recreate after delete failed: created=%v err=%v", created, err)
	}
	if _, err := reg.delete("no-such", 0); err == nil {
		t.Error("delete unknown UUID must error")
	}
}

func TestLoadBalancerRegistry_ListFiltersByProject(t *testing.T) {
	reg, _ := loadLoadBalancerRegistry(context.Background(), NewMemStorage())
	_, _, _ = reg.create("proj-A", "a", ":80", "l4_tcp", nil)
	_, _, _ = reg.create("proj-A", "b", ":80", "l4_tcp", nil)
	_, _, _ = reg.create("proj-B", "c", ":80", "l4_tcp", nil)
	if got := reg.list(""); len(got) != 3 {
		t.Errorf("list(\"\") should return 3, got %d", len(got))
	}
	gotA := reg.list("proj-A")
	if len(gotA) != 2 {
		t.Errorf("list(proj-A) should return 2, got %d", len(gotA))
	}
}
