package weft

import (
	"strings"
	"testing"

	guestv1 "github.com/openweft/weft-proto/guestv1"
)

func TestValidGuestSecretID(t *testing.T) {
	ok := []string{"a", "0", "db-pass", "a.b_c-d", strings.Repeat("a", 63)}
	bad := []string{"", "A", "-a", ".a", "_a", "a b", "a/b", "a\x00", "é", strings.Repeat("a", 64)}
	for _, id := range ok {
		if !ValidGuestSecretID(id) {
			t.Errorf("%q refused", id)
		}
	}
	for _, id := range bad {
		if ValidGuestSecretID(id) {
			t.Errorf("%q accepted", id)
		}
	}
}

func FuzzValidGuestSecretID(f *testing.F) {
	f.Add("db-pass")
	f.Add("")
	f.Add("a\nb")
	f.Fuzz(func(t *testing.T, id string) {
		if !ValidGuestSecretID(id) {
			return
		}
		if len(id) == 0 || len(id) > MaxGuestSecretIDLen || id[0] == '.' || id[0] == '_' || id[0] == '-' {
			t.Fatalf("accepted %q", id)
		}
		for i := 0; i < len(id); i++ {
			c := id[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
				t.Fatalf("accepted %q", id)
			}
		}
	})
}

func newSecretsAdapter() *Adapter {
	return &Adapter{podCIDs: newPodCIDRegistry()}
}

func TestGuestSecretsGrantLookupRevoke(t *testing.T) {
	a := newSecretsAdapter()
	if _, ok, err := a.GuestSecretGrantFor("pod", "k"); ok || err != nil {
		t.Fatalf("empty table: ok=%v err=%v", ok, err)
	}
	g := GuestSecretGrant{Ref: "file:/x/k", Version: 2}
	if err := a.GrantGuestSecrets("pod", map[string]GuestSecretGrant{"k": g}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := a.GuestSecretGrantFor("pod", "k")
	if err != nil || !ok || got != g {
		t.Fatalf("got %+v %v %v", got, ok, err)
	}
	if _, ok, _ := a.GuestSecretGrantFor("pod", "other"); ok {
		t.Fatal("unlisted id allowed")
	}
	if _, ok, _ := a.GuestSecretGrantFor("pod2", "k"); ok {
		t.Fatal("another pod's table used")
	}
	if _, ok, _ := a.GuestSecretGrantFor("pod", "BAD ID"); ok {
		t.Fatal("invalid id allowed")
	}
	a.RevokeGuestSecrets("pod")
	if _, ok, _ := a.GuestSecretGrantFor("pod", "k"); ok {
		t.Fatal("still allowed after revoke")
	}
	a.RevokeGuestSecrets("")
	a.RevokeGuestSecrets("never-existed")
}

func TestGuestSecretsGrantRefusals(t *testing.T) {
	a := newSecretsAdapter()
	good := GuestSecretGrant{Ref: "file:/x/k"}
	if a.GrantGuestSecrets("", map[string]GuestSecretGrant{"k": good}) == nil {
		t.Error("empty pod accepted")
	}
	if a.GrantGuestSecrets("p", map[string]GuestSecretGrant{"BAD": good}) == nil {
		t.Error("bad id accepted")
	}
	if a.GrantGuestSecrets("p", map[string]GuestSecretGrant{"k": {Ref: "not a ref"}}) == nil {
		t.Error("bad ref accepted")
	}
	big := map[string]GuestSecretGrant{}
	for i := 0; i <= MaxGuestSecretsPerPod; i++ {
		big[string(rune('a'+i%26))+strings.Repeat("x", i/26)] = good
	}
	if a.GrantGuestSecrets("p", big) == nil {
		t.Error("oversize table accepted")
	}
	if _, ok, _ := a.GuestSecretGrantFor("p", "k"); ok {
		t.Error("a refused grant left state behind")
	}
	// A mix with one bad entry stores nothing.
	if a.GrantGuestSecrets("p", map[string]GuestSecretGrant{"k": good, "BAD": good}) == nil {
		t.Error("mixed table accepted")
	}
	if _, ok, _ := a.GuestSecretGrantFor("p", "k"); ok {
		t.Error("partial table stored")
	}
}

func TestGuestSecretsKVBacked(t *testing.T) {
	kv := NewMemKVStorage("guestsecrets")
	a := newSecretsAdapter()
	a.kvStorageFactory = func(name string) KVStorage {
		if name != guestSecretsKVName {
			t.Errorf("store name %q", name)
		}
		return kv
	}
	if err := a.GrantGuestSecrets("pod", map[string]GuestSecretGrant{"k": {Ref: "file:/x/k", Version: 9}}); err != nil {
		t.Fatal(err)
	}
	// A second adapter on the same store (another control-plane replica) sees it.
	b := newSecretsAdapter()
	b.kvStorageFactory = a.kvStorageFactory
	g, ok, err := b.GuestSecretGrantFor("pod", "k")
	if err != nil || !ok || g.Version != 9 {
		t.Fatalf("replica: %+v %v %v", g, ok, err)
	}
	a.RevokeGuestSecrets("pod")
	if _, ok, _ := b.GuestSecretGrantFor("pod", "k"); ok {
		t.Fatal("replica still allowed after revoke")
	}
}

func TestGuestSecretsRevokedWithThePod(t *testing.T) {
	a := New(t.TempDir()).(*Adapter)
	if err := a.GrantGuestSecrets("pod", map[string]GuestSecretGrant{"k": {Ref: "file:/x/k"}}); err != nil {
		t.Fatal(err)
	}
	a.SetPodSpec("pod", &guestv1.PodSpec{})
	if _, ok, _ := a.GuestSecretGrantFor("pod", "k"); !ok {
		t.Fatal("setting a spec revoked the table")
	}
	a.SetPodSpec("pod", nil)
	if _, ok, _ := a.GuestSecretGrantFor("pod", "k"); ok {
		t.Fatal("deleting the pod spec left the grant")
	}
}

func TestPodForCID(t *testing.T) {
	a := newSecretsAdapter()
	a.RegisterPodCID("a", 100)
	a.RegisterPodCID("b", 101)
	if p, ok := a.PodForCID(100); !ok || p != "a" {
		t.Fatalf("100 -> %q %v", p, ok)
	}
	if _, ok := a.PodForCID(102); ok {
		t.Fatal("unbound CID resolved")
	}
	for _, reserved := range []uint32{0, 1, 2, 0xffffffff} {
		if _, ok := a.PodForCID(reserved); ok {
			t.Fatalf("reserved CID %d resolved", reserved)
		}
	}
	a.RegisterPodCID("c", 100) // ambiguous: fail closed
	if _, ok := a.PodForCID(100); ok {
		t.Fatal("ambiguous CID resolved")
	}
}

func TestInventoryPodForCID(t *testing.T) {
	vms := []VM{
		{Name: "a", HostUUID: "h", VsockCID: 100},
		{Name: "b", HostUUID: "other", VsockCID: 101},
		{Name: "", HostUUID: "h", VsockCID: 102},
		{Name: "x", HostUUID: "h", VsockCID: 103},
		{Name: "y", HostUUID: "h", VsockCID: 103},
	}
	cases := []struct {
		cid  uint32
		host string
		want string
		ok   bool
	}{
		{100, "h", "a", true},
		{101, "h", "", false}, // another host's VM
		{102, "h", "", false}, // nameless
		{103, "h", "", false}, // two names
		{104, "h", "", false},
		{100, "", "", false},
	}
	for _, c := range cases {
		got, ok := inventoryPodForCID(vms, c.cid, c.host)
		if got != c.want || ok != c.ok {
			t.Errorf("cid %d host %q = %q,%v want %q,%v", c.cid, c.host, got, ok, c.want, c.ok)
		}
	}
	// Same name twice with one CID is still one pod.
	dup := []VM{{Name: "a", HostUUID: "h", VsockCID: 100}, {Name: "a", HostUUID: "h", VsockCID: 100}}
	if p, ok := inventoryPodForCID(dup, 100, "h"); !ok || p != "a" {
		t.Errorf("dup = %q %v", p, ok)
	}
}
