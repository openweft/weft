package cluster

import (
	"os"
	"path/filepath"
	"testing"
)

func writeHCL(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cluster.hcl")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad_SingleHost(t *testing.T) {
	c, err := Load(writeHCL(t, `
cluster "dev" {
  overlay { subnet = "10.9.0.0/24" }
  host "h1" {
    address    = "127.0.0.1"
    hypervisor = "qemu"
  }
}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.IsCluster() {
		t.Error("1 host should not be a cluster")
	}
	if c.Hosts[0].DC != "h1" {
		t.Errorf("DC default = %q, want host id h1", c.Hosts[0].DC)
	}
}

func TestLoad_ThreeDC(t *testing.T) {
	c, err := Load(writeHCL(t, `
cluster "prod" {
  overlay { subnet = "10.9.0.0/24" }
  host "a" {
    address = "192.0.2.1"
    dc      = "dc1"
  }
  host "b" {
    address = "192.0.2.2"
    dc      = "dc2"
  }
  host "c" {
    address = "192.0.2.3"
    dc      = "dc3"
  }
  infra { services = ["etcd", "nats"] }
}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.IsCluster() {
		t.Error("3 hosts should be a cluster")
	}
}

func TestLoad_RejectsTwoHosts(t *testing.T) {
	_, err := Load(writeHCL(t, `
cluster "bad" {
  overlay { subnet = "10.9.0.0/24" }
  host "a" { address = "192.0.2.1" }
  host "b" { address = "192.0.2.2" }
}`))
	if err == nil {
		t.Error("2 hosts must be rejected (1 or 3 only)")
	}
}

func TestLoad_RejectsBadSubnet(t *testing.T) {
	_, err := Load(writeHCL(t, `
cluster "bad" {
  overlay { subnet = "not-a-cidr" }
  host "h1" { address = "127.0.0.1" }
}`))
	if err == nil {
		t.Error("invalid overlay subnet must be rejected")
	}
}

func TestLoad_RejectsDuplicateDC(t *testing.T) {
	_, err := Load(writeHCL(t, `
cluster "bad" {
  overlay { subnet = "10.9.0.0/24" }
  host "a" {
    address = "192.0.2.1"
    dc      = "dc1"
  }
  host "b" {
    address = "192.0.2.2"
    dc      = "dc1"
  }
  host "c" {
    address = "192.0.2.3"
    dc      = "dc1"
  }
}`))
	if err == nil {
		t.Error("hosts sharing a DC must be rejected")
	}
}
