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

func TestPodCID_UnknownIsUnbound(t *testing.T) {
	a := &Adapter{}
	a.initPodCIDs()
	if _, ok := a.PodCID("never-started"); ok {
		t.Fatal("an unknown pod must be unbound")
	}
}
