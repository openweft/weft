package weft

import (
	"context"
	"strings"
	"testing"
)

func newTestScriptRegistry(t *testing.T) *scriptRegistry {
	t.Helper()
	reg, err := LoadScriptRegistry(context.Background(), NewMemStorage())
	if err != nil {
		t.Fatalf("LoadScriptRegistry: %v", err)
	}
	return reg
}

func TestScriptRegistry_FreshIsEmpty(t *testing.T) {
	reg := newTestScriptRegistry(t)
	if got := reg.List(); len(got) != 0 {
		t.Errorf("fresh registry should be empty, got %d", len(got))
	}
}

func TestScriptRegistry_SetGetRoundtrip(t *testing.T) {
	reg := newTestScriptRegistry(t)
	body := "#!/bin/sh\nset -eu\necho hello\n"
	if err := reg.Set(Script{Name: "hello", Description: "demo", Body: body}, "alice@h"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, ok := reg.Get("hello")
	if !ok {
		t.Fatal("Get not found")
	}
	if got.Body != body {
		t.Errorf("body mismatch : %q", got.Body)
	}
	if got.UpdatedBy != "alice@h" {
		t.Errorf("updated_by = %q", got.UpdatedBy)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("updated_at not stamped")
	}
}

func TestScriptRegistry_ListIsSortedByName(t *testing.T) {
	reg := newTestScriptRegistry(t)
	for _, n := range []string{"setup", "deploy", "ci-build", "nginx"} {
		_ = reg.Set(Script{Name: n, Body: "echo " + n}, "")
	}
	names := []string{}
	for _, s := range reg.List() {
		names = append(names, s.Name)
	}
	for i, w := range []string{"ci-build", "deploy", "nginx", "setup"} {
		if names[i] != w {
			t.Errorf("List order mismatch: %v", names)
			break
		}
	}
}

func TestScriptRegistry_SetReplaces(t *testing.T) {
	reg := newTestScriptRegistry(t)
	_ = reg.Set(Script{Name: "s", Body: "echo v1"}, "")
	_ = reg.Set(Script{Name: "s", Body: "echo v2"}, "")
	got, _ := reg.Get("s")
	if got.Body != "echo v2" {
		t.Errorf("Set should replace, got %q", got.Body)
	}
}

func TestScriptRegistry_DeleteIsIdempotent(t *testing.T) {
	reg := newTestScriptRegistry(t)
	_ = reg.Set(Script{Name: "s", Body: "echo x"}, "")
	if err := reg.Delete("s"); err != nil {
		t.Fatal(err)
	}
	if err := reg.Delete("missing"); err != nil {
		t.Errorf("Delete on missing should be nil, got %v", err)
	}
}

func TestScriptRegistry_SetValidates(t *testing.T) {
	reg := newTestScriptRegistry(t)
	cases := []struct {
		name string
		s    Script
		want string
	}{
		{"empty name", Script{Body: "echo"}, "name"},
		{"empty body", Script{Name: "x", Body: ""}, "body"},
	}
	for _, c := range cases {
		err := reg.Set(c.s, "")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: expected error containing %q, got %v", c.name, c.want, err)
		}
	}
}

func TestScriptRegistry_PersistsBodyVerbatim(t *testing.T) {
	// Multi-line script with HCL-tricky characters should survive
	// the save+load round-trip unchanged.
	storage := NewMemStorage()
	r1, _ := LoadScriptRegistry(context.Background(), storage)
	body := `#!/bin/sh
set -eu
echo "double quotes"
echo 'single quotes'
echo "$HOME and ${PATH}"
# trailing comment
`
	if err := r1.Set(Script{Name: "tricky", Body: body}, ""); err != nil {
		t.Fatal(err)
	}
	r2, err := LoadScriptRegistry(context.Background(), storage)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got, _ := r2.Get("tricky")
	if got.Body != body {
		t.Errorf("body changed across round-trip\nwant: %q\n got: %q", body, got.Body)
	}
}

func TestScriptRegistry_HCLRoundTrip_StableOrdering(t *testing.T) {
	storage := NewMemStorage()
	r1, _ := LoadScriptRegistry(context.Background(), storage)
	for _, n := range []string{"setup", "ci-build", "nginx", "deploy"} {
		_ = r1.Set(Script{Name: n, Body: "echo " + n}, "")
	}
	blob, _ := storage.Load(context.Background())
	text := string(blob)
	want := []string{`script "ci-build"`, `script "deploy"`, `script "nginx"`, `script "setup"`}
	last := -1
	for _, w := range want {
		idx := strings.Index(text, w)
		if idx < 0 {
			t.Errorf("block %q missing", w)
		}
		if idx <= last {
			t.Errorf("block %q out of order", w)
		}
		last = idx
	}
}
