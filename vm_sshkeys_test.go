package weft

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"strings"
	"testing"
)

// genEd25519Line returns a freshly-minted "ssh-ed25519 <b64> <comment>"
// line — same byte-format as ssh-keygen + the in-guest pkg/sshkeys
// validator expects. Reused across tests so we hit the parse path
// with realistic input.
func genEd25519Line(t *testing.T, comment string) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const algo = "ssh-ed25519"
	buf := make([]byte, 0, 4+len(algo)+4+len(pub))
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(algo)))
	buf = append(buf, algo...)
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(pub)))
	buf = append(buf, pub...)
	return algo + " " + base64.StdEncoding.EncodeToString(buf) + " " + comment
}

func newTestSSHKeyRegistry(t *testing.T) *vmSSHKeyRegistry {
	t.Helper()
	reg, err := LoadVMSSHKeyRegistry(context.Background(), NewMemStorage())
	if err != nil {
		t.Fatalf("LoadVMSSHKeyRegistry: %v", err)
	}
	return reg
}

func TestVMSSHKeyRegistry_FreshEmpty(t *testing.T) {
	reg := newTestSSHKeyRegistry(t)
	if got := reg.ListForVM("p", "vm"); len(got) != 0 {
		t.Errorf("fresh should be empty, got %d", len(got))
	}
}

func TestVMSSHKeyRegistry_AddListRoundtrip(t *testing.T) {
	reg := newTestSSHKeyRegistry(t)
	line := genEd25519Line(t, "alice@laptop")
	entry, err := reg.Add("alpha", "web-1", line)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if entry.Type != "ssh-ed25519" {
		t.Errorf("type = %q", entry.Type)
	}
	if entry.Comment != "alice@laptop" {
		t.Errorf("comment = %q", entry.Comment)
	}
	if !strings.HasPrefix(entry.Fingerprint, "SHA256:") {
		t.Errorf("fingerprint missing SHA256: prefix : %q", entry.Fingerprint)
	}
	if entry.AddedAt.IsZero() {
		t.Error("AddedAt zero")
	}
	got := reg.ListForVM("alpha", "web-1")
	if len(got) != 1 || got[0].Fingerprint != entry.Fingerprint {
		t.Errorf("List mismatch : %+v", got)
	}
}

func TestVMSSHKeyRegistry_AddIdempotentOnFingerprint(t *testing.T) {
	reg := newTestSSHKeyRegistry(t)
	line := genEd25519Line(t, "alice@laptop")
	first, _ := reg.Add("p", "vm", line)
	second, err := reg.Add("p", "vm", line)
	if err != nil {
		t.Fatal(err)
	}
	if first.Fingerprint != second.Fingerprint {
		t.Errorf("fingerprint changed on re-add")
	}
	if got := reg.ListForVM("p", "vm"); len(got) != 1 {
		t.Errorf("expected 1 entry after idempotent re-add, got %d", len(got))
	}
}

func TestVMSSHKeyRegistry_AddRejectsGarbage(t *testing.T) {
	reg := newTestSSHKeyRegistry(t)
	cases := []string{
		"",
		"not a key",
		"ssh-ed25519 NOT-BASE64 comment",
		"ssh-unknown AAAA= comment",
	}
	for _, c := range cases {
		if _, err := reg.Add("p", "vm", c); err == nil {
			t.Errorf("expected error for %q", c)
		}
	}
}

func TestVMSSHKeyRegistry_AddRequiresVMName(t *testing.T) {
	reg := newTestSSHKeyRegistry(t)
	line := genEd25519Line(t, "x@h")
	if _, err := reg.Add("p", "", line); err == nil {
		t.Error("empty vm_name should be rejected")
	}
}

func TestVMSSHKeyRegistry_RemoveIdempotent(t *testing.T) {
	reg := newTestSSHKeyRegistry(t)
	line := genEd25519Line(t, "x@h")
	entry, _ := reg.Add("p", "vm", line)
	if err := reg.Remove("p", "vm", entry.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if got := reg.ListForVM("p", "vm"); len(got) != 0 {
		t.Errorf("entry should be gone after Remove : %+v", got)
	}
	if err := reg.Remove("p", "vm", entry.Fingerprint); err != nil {
		t.Errorf("Remove idempotent expected, got %v", err)
	}
}

func TestVMSSHKeyRegistry_ProjectIsolation(t *testing.T) {
	reg := newTestSSHKeyRegistry(t)
	line := genEd25519Line(t, "x@h")
	a, _ := reg.Add("alpha", "vm", line)
	// Same line + different project = same fingerprint but separate
	// per-VM scope.
	b, _ := reg.Add("beta", "vm", line)
	if a.Fingerprint != b.Fingerprint {
		t.Errorf("same key -> different fingerprints : %s vs %s", a.Fingerprint, b.Fingerprint)
	}
	if got := reg.ListForVM("alpha", "vm"); len(got) != 1 {
		t.Errorf("alpha should have 1 entry, got %d", len(got))
	}
	if got := reg.ListForVM("beta", "vm"); len(got) != 1 {
		t.Errorf("beta should have 1 entry, got %d", len(got))
	}
}

func TestVMSSHKeyRegistry_PersistsAcrossReload(t *testing.T) {
	storage := NewMemStorage()
	r1, _ := LoadVMSSHKeyRegistry(context.Background(), storage)
	line := genEd25519Line(t, "alice@laptop")
	entry, _ := r1.Add("p", "vm", line)
	r2, err := LoadVMSSHKeyRegistry(context.Background(), storage)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got := r2.ListForVM("p", "vm")
	if len(got) != 1 || got[0].Fingerprint != entry.Fingerprint {
		t.Errorf("entry lost across reload : %+v", got)
	}
}

func TestVMSSHKeyRegistry_HCLStableOrdering(t *testing.T) {
	storage := NewMemStorage()
	r1, _ := LoadVMSSHKeyRegistry(context.Background(), storage)
	// Multiple keys, multiple VMs. Output sorted by (project, vm,
	// fingerprint).
	for _, c := range []struct{ project, vm, comment string }{
		{"alpha", "vm-z", "c1"},
		{"alpha", "vm-a", "c2"},
		{"beta", "vm-a", "c3"},
	} {
		_, _ = r1.Add(c.project, c.vm, genEd25519Line(t, c.comment))
	}
	blob, _ := storage.Load(context.Background())
	text := string(blob)
	want := []string{
		`ssh_key "alpha/vm-a/`,
		`ssh_key "alpha/vm-z/`,
		`ssh_key "beta/vm-a/`,
	}
	last := -1
	for _, w := range want {
		idx := strings.Index(text, w)
		if idx <= last {
			t.Errorf("block %q out of order", w)
		}
		last = idx
	}
}
