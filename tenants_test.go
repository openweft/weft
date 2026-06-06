package weft

import (
	"context"
	"strings"
	"testing"
)

func TestTenantRegistry_EmptyStartsClean(t *testing.T) {
	reg, err := loadTenantRegistry(context.Background(), NewMemStorage())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := reg.list(); len(got) != 0 {
		t.Errorf("fresh registry should be empty, got %d rows", len(got))
	}
}

func TestTenantRegistry_CreateRoundTrips(t *testing.T) {
	storage := NewMemStorage()
	reg, _ := loadTenantRegistry(context.Background(), storage)
	tn, created, err := reg.create("acme", "acme.example.com")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !created {
		t.Error("first create should report created=true")
	}
	if tn.Name != "acme" || tn.Domain != "acme.example.com" || tn.Status != "active" {
		t.Errorf("row fields drifted : %+v", tn)
	}
	if tn.UUID == "" || len(tn.UUID) != 36 {
		t.Errorf("UUID should be a 36-char RFC4122 v4, got %q", tn.UUID)
	}

	// Reload from storage to confirm persistence.
	reg2, err := loadTenantRegistry(context.Background(), storage)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got, ok := reg2.lookupByUUID(tn.UUID)
	if !ok {
		t.Fatal("reloaded registry forgot the tenant we just created")
	}
	if got.Name != tn.Name {
		t.Errorf("reload mismatch : %+v vs %+v", got, tn)
	}
	gotByName, ok := reg2.lookupByName("acme")
	if !ok || gotByName.UUID != tn.UUID {
		t.Errorf("name → uuid index not rebuilt on load")
	}
}

func TestTenantRegistry_CreateIdempotent(t *testing.T) {
	reg, _ := loadTenantRegistry(context.Background(), NewMemStorage())
	t1, _, _ := reg.create("acme", "acme.example.com")
	t2, created, err := reg.create("acme", "different.example.com")
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if created {
		t.Error("repeat create with same name should report created=false")
	}
	if t2.UUID != t1.UUID {
		t.Error("repeat create should return the existing row, not mint a new UUID")
	}
	if t2.Domain != "acme.example.com" {
		t.Error("repeat create must NOT clobber the existing row's fields")
	}
}

func TestTenantRegistry_CreateRejectsEmptyName(t *testing.T) {
	reg, _ := loadTenantRegistry(context.Background(), NewMemStorage())
	if _, _, err := reg.create("", "x"); err == nil {
		t.Error("create with empty name must error")
	}
}

func TestTenantRegistry_DeleteRefusesCascade(t *testing.T) {
	reg, _ := loadTenantRegistry(context.Background(), NewMemStorage())
	tn, _, _ := reg.create("acme", "")
	if _, err := reg.delete(tn.UUID, 3 /* projects */); err == nil {
		t.Error("delete with projects > 0 must refuse")
	} else if !strings.Contains(err.Error(), "3 project") {
		t.Errorf("error should report the blocking count, got: %v", err)
	}
}

func TestTenantRegistry_DeleteCleansIndex(t *testing.T) {
	reg, _ := loadTenantRegistry(context.Background(), NewMemStorage())
	tn, _, _ := reg.create("acme", "")
	if _, err := reg.delete(tn.UUID, 0); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := reg.lookupByUUID(tn.UUID); ok {
		t.Error("deleted row still surfaces via UUID lookup")
	}
	if _, ok := reg.lookupByName("acme"); ok {
		t.Error("deleted row still surfaces via name lookup — secondary index leaked")
	}
}

func TestTenantRegistry_ListIsSortedByName(t *testing.T) {
	reg, _ := loadTenantRegistry(context.Background(), NewMemStorage())
	_, _, _ = reg.create("zebra", "")
	_, _, _ = reg.create("acme", "")
	_, _, _ = reg.create("mango", "")
	got := reg.list()
	want := []string{"acme", "mango", "zebra"}
	for i, w := range want {
		if got[i].Name != w {
			t.Errorf("list[%d].Name : got %q, want %q", i, got[i].Name, w)
		}
	}
}

func TestTenantRegistry_AdminAddRemoveIdempotent(t *testing.T) {
	reg, _ := loadTenantRegistry(context.Background(), NewMemStorage())
	tn, _, _ := reg.create("acme", "")

	if _, err := reg.addAdmin(tn.UUID, "alice@acme.org"); err != nil {
		t.Fatalf("addAdmin: %v", err)
	}
	// Repeat add → no growth, no error.
	out, err := reg.addAdmin(tn.UUID, "alice@acme.org")
	if err != nil {
		t.Fatalf("addAdmin repeat: %v", err)
	}
	if len(out.Admins) != 1 {
		t.Errorf("addAdmin not idempotent : Admins=%v", out.Admins)
	}

	// Two distinct admins.
	out, _ = reg.addAdmin(tn.UUID, "bob@acme.org")
	if len(out.Admins) != 2 {
		t.Errorf("expected 2 admins, got %v", out.Admins)
	}

	// Remove one.
	out, err = reg.removeAdmin(tn.UUID, "alice@acme.org")
	if err != nil {
		t.Fatalf("removeAdmin: %v", err)
	}
	if len(out.Admins) != 1 || out.Admins[0] != "bob@acme.org" {
		t.Errorf("after remove : got %v, want [bob@acme.org]", out.Admins)
	}

	// Remove again = no-op.
	out, err = reg.removeAdmin(tn.UUID, "alice@acme.org")
	if err != nil {
		t.Fatalf("removeAdmin repeat: %v", err)
	}
	if len(out.Admins) != 1 {
		t.Errorf("removeAdmin missing email should be a no-op")
	}
}

func TestTenantRegistry_AdminAddRejectsEmpty(t *testing.T) {
	reg, _ := loadTenantRegistry(context.Background(), NewMemStorage())
	tn, _, _ := reg.create("acme", "")
	if _, err := reg.addAdmin(tn.UUID, ""); err == nil {
		t.Error("addAdmin with empty email must error")
	}
}

func TestTenantRegistry_AdminAddRejectsUnknownTenant(t *testing.T) {
	reg, _ := loadTenantRegistry(context.Background(), NewMemStorage())
	if _, err := reg.addAdmin("no-such-uuid", "alice@acme.org"); err == nil {
		t.Error("addAdmin on unknown tenant must error")
	}
}

func TestTenantRegistry_MemberAddMergesGroups(t *testing.T) {
	reg, _ := loadTenantRegistry(context.Background(), NewMemStorage())
	tn, _, _ := reg.create("acme", "")

	out, err := reg.addMember(tn.UUID, "alice@acme.org", []string{"net", "ops"})
	if err != nil {
		t.Fatalf("addMember: %v", err)
	}
	if len(out.Members) != 1 {
		t.Fatalf("expected 1 member, got %d", len(out.Members))
	}
	if got := out.Members[0].Groups; len(got) != 2 || got[0] != "net" || got[1] != "ops" {
		t.Errorf("initial groups : got %v, want [net ops]", got)
	}

	// Second add with overlapping + new group = union, deduped.
	out, _ = reg.addMember(tn.UUID, "alice@acme.org", []string{"ops", "ml"})
	if len(out.Members) != 1 {
		t.Errorf("addMember on existing email must not duplicate row")
	}
	got := out.Members[0].Groups
	if len(got) != 3 || got[0] != "net" || got[1] != "ops" || got[2] != "ml" {
		t.Errorf("merged groups : got %v, want [net ops ml]", got)
	}
}

func TestTenantRegistry_MemberRemoveIdempotent(t *testing.T) {
	reg, _ := loadTenantRegistry(context.Background(), NewMemStorage())
	tn, _, _ := reg.create("acme", "")
	_, _ = reg.addMember(tn.UUID, "alice@acme.org", []string{"net"})
	_, _ = reg.addMember(tn.UUID, "bob@acme.org", []string{"ops"})

	out, err := reg.removeMember(tn.UUID, "alice@acme.org")
	if err != nil {
		t.Fatalf("removeMember: %v", err)
	}
	if len(out.Members) != 1 || out.Members[0].Email != "bob@acme.org" {
		t.Errorf("after remove : got %+v, want only bob", out.Members)
	}

	// Repeat remove = no error.
	out, err = reg.removeMember(tn.UUID, "alice@acme.org")
	if err != nil {
		t.Fatalf("removeMember repeat: %v", err)
	}
	if len(out.Members) != 1 {
		t.Errorf("removeMember missing email should be a no-op")
	}
}

func TestTenantRegistry_MemberAddRejectsEmpty(t *testing.T) {
	reg, _ := loadTenantRegistry(context.Background(), NewMemStorage())
	tn, _, _ := reg.create("acme", "")
	if _, err := reg.addMember(tn.UUID, "", nil); err == nil {
		t.Error("addMember with empty email must error")
	}
}

func TestMintTenantUUID_LooksRFC4122v4(t *testing.T) {
	u, err := mintTenantUUID()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if len(u) != 36 {
		t.Errorf("UUID length : got %d, want 36", len(u))
	}
	if u[14] != '4' {
		t.Errorf("UUID version nibble : got %c, want 4 (full: %s)", u[14], u)
	}
}

func TestUnionStrings(t *testing.T) {
	cases := []struct {
		a, b, want []string
	}{
		{nil, nil, []string{}},
		{[]string{"x"}, nil, []string{"x"}},
		{nil, []string{"x"}, []string{"x"}},
		{[]string{"a", "b"}, []string{"b", "c"}, []string{"a", "b", "c"}},
		{[]string{"a", "a"}, []string{"a"}, []string{"a"}},
	}
	for i, c := range cases {
		got := unionStrings(c.a, c.b)
		if len(got) != len(c.want) {
			t.Errorf("case %d: got %v, want %v", i, got, c.want)
			continue
		}
		for j := range got {
			if got[j] != c.want[j] {
				t.Errorf("case %d: got %v, want %v", i, got, c.want)
				break
			}
		}
	}
}
