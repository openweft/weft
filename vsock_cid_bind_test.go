package weft

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBindPodCIDFromConfig(t *testing.T) {
	cases := []struct {
		name    string
		cfg     string
		wantCID uint32
		wantOK  bool
	}{
		{"valid", `{"vsock_cid": 70000}`, 70000, true},
		{"zero", `{"vsock_cid": 0}`, 0, false},
		{"reserved host", `{"vsock_cid": 2}`, 0, false},
		{"any", `{"vsock_cid": 4294967295}`, 0, false},
		{"garbage", `not json`, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(c.cfg), 0o600); err != nil {
				t.Fatal(err)
			}
			a := &Adapter{}
			a.initPodCIDs()
			a.bindPodCIDFromConfig("pod-1", dir)
			got, ok := a.PodCID("pod-1")
			if got != c.wantCID || ok != c.wantOK {
				t.Fatalf("PodCID = (%d,%v), want (%d,%v)", got, ok, c.wantCID, c.wantOK)
			}
		})
	}
}

func TestBindPodCIDFromConfig_MissingFile(t *testing.T) {
	a := &Adapter{}
	a.initPodCIDs()
	a.bindPodCIDFromConfig("pod-1", t.TempDir())
	if _, ok := a.PodCID("pod-1"); ok {
		t.Fatal("bound without a config")
	}
}

func TestInventoryPodCID(t *testing.T) {
	vms := []VM{
		{Name: "a", HostUUID: "h1", VsockCID: 70000},
		{Name: "a", HostUUID: "h2", VsockCID: 80000}, // other host: ignored
		{Name: "b", HostUUID: "h1", VsockCID: 70001},
		{Name: "b", HostUUID: "h1", VsockCID: 70002}, // ambiguous
		{Name: "c", HostUUID: "h1", VsockCID: 0},
		{Name: "d", HostUUID: "h1", VsockCID: 2}, // reserved
		{Name: "e", HostUUID: "h1", VsockCID: 70003},
		{Name: "e", HostUUID: "h1", VsockCID: 70003}, // same value twice: fine
	}
	cases := []struct {
		pod, host string
		want      uint32
		ok        bool
	}{
		{"a", "h1", 70000, true},
		{"a", "h2", 80000, true},
		{"a", "h3", 0, false},
		{"a", "", 0, false},
		{"b", "h1", 0, false},
		{"c", "h1", 0, false},
		{"d", "h1", 0, false},
		{"e", "h1", 70003, true},
		{"zz", "h1", 0, false},
	}
	for _, c := range cases {
		got, ok := inventoryPodCID(vms, c.pod, c.host)
		if got != c.want || ok != c.ok {
			t.Errorf("inventoryPodCID(%q,%q) = (%d,%v), want (%d,%v)", c.pod, c.host, got, ok, c.want, c.ok)
		}
	}
}

func TestPodCID_RegistryWinsOverNothing(t *testing.T) {
	a := &Adapter{}
	a.initPodCIDs()
	a.RegisterPodCID("p", 70000)
	if got, ok := a.PodCID("p"); !ok || got != 70000 {
		t.Fatalf("PodCID = (%d,%v)", got, ok)
	}
	a.UnregisterPodCID("p")
	if _, ok := a.PodCID("p"); ok {
		t.Fatal("still bound after unregister")
	}
}

func TestPodCID_UnknownIsUnbound(t *testing.T) {
	a := &Adapter{}
	a.initPodCIDs()
	if _, ok := a.PodCID("never-started"); ok {
		t.Fatal("an unknown pod must be unbound")
	}
}
