//go:build darwin && cgo

package weft

import (
	"testing"
)

// ── normMAC ──────────────────────────────────────────────────────────────────

func TestNormMAC_SingleDigitOctet(t *testing.T) {
	// macOS arp -an omits leading zeros: "f" instead of "0f"
	got := normMAC("c2:58:e1:f:e1:10")
	want := "c2:58:e1:0f:e1:10"
	if got != want {
		t.Fatalf("normMAC(%q) = %q, want %q", "c2:58:e1:f:e1:10", got, want)
	}
}

func TestNormMAC_AlreadyPadded(t *testing.T) {
	got := normMAC("c2:58:e1:0f:e1:10")
	want := "c2:58:e1:0f:e1:10"
	if got != want {
		t.Fatalf("normMAC(%q) = %q, want %q", "c2:58:e1:0f:e1:10", got, want)
	}
}

func TestNormMAC_UpperCase(t *testing.T) {
	got := normMAC("C2:58:E1:0F:E1:10")
	want := "c2:58:e1:0f:e1:10"
	if got != want {
		t.Fatalf("normMAC(%q) = %q, want %q", "C2:58:E1:0F:E1:10", got, want)
	}
}

func TestNormMAC_DashSeparated(t *testing.T) {
	got := normMAC("c2-58-e1-0f-e1-10")
	want := "c2:58:e1:0f:e1:10"
	if got != want {
		t.Fatalf("normMAC(%q) = %q, want %q", "c2-58-e1-0f-e1-10", got, want)
	}
}

func TestNormMAC_AllZeroOctets(t *testing.T) {
	got := normMAC("0:0:0:0:0:0")
	want := "00:00:00:00:00:00"
	if got != want {
		t.Fatalf("normMAC(%q) = %q, want %q", "0:0:0:0:0:0", got, want)
	}
}

func TestNormMAC_InvalidReturnsLowercaseInput(t *testing.T) {
	got := normMAC("not-a-mac")
	want := "not-a-mac"
	if got != want {
		t.Fatalf("normMAC(%q) = %q, want %q", "not-a-mac", got, want)
	}
}

// ── parseDHCPLeasesData ───────────────────────────────────────────────────────

const sampleLeases = `
{
	name=mock-ubuntu-1
	ip_address=192.168.64.3
	hw_address=1,c2:58:e1:f:e1:10
	identifier=1,c2:58:e1:f:e1:10
	lease=0x67b4f01a
}
{
	name=mock-debian-1
	ip_address=192.168.64.4
	hw_address=1,aa:bb:cc:dd:ee:ff
	identifier=1,aa:bb:cc:dd:ee:ff
	lease=0x67b4f01b
}
`

func TestParseDHCPLeases_MatchSingleDigitOctet(t *testing.T) {
	// stored as "c2:58:e1:0f:e1:10" (zero-padded), lease file has "f" without zero
	got := parseDHCPLeasesData([]byte(sampleLeases), "c2:58:e1:0f:e1:10")
	if got != "192.168.64.3" {
		t.Fatalf("got %q, want %q", got, "192.168.64.3")
	}
}

func TestParseDHCPLeases_MatchFullyPadded(t *testing.T) {
	got := parseDHCPLeasesData([]byte(sampleLeases), "aa:bb:cc:dd:ee:ff")
	if got != "192.168.64.4" {
		t.Fatalf("got %q, want %q", got, "192.168.64.4")
	}
}

func TestParseDHCPLeases_NoMatch(t *testing.T) {
	got := parseDHCPLeasesData([]byte(sampleLeases), "de:ad:be:ef:00:01")
	if got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestParseDHCPLeases_Empty(t *testing.T) {
	got := parseDHCPLeasesData([]byte(""), "c2:58:e1:0f:e1:10")
	if got != "" {
		t.Fatalf("expected empty on empty input, got %q", got)
	}
}

func TestParseDHCPLeases_NoPrefixStripped(t *testing.T) {
	// hw_address without "1," prefix — still matches
	data := []byte(`{
	ip_address=10.0.0.1
	hw_address=c2:58:e1:f:e1:10
}`)
	got := parseDHCPLeasesData(data, "c2:58:e1:0f:e1:10")
	if got != "10.0.0.1" {
		t.Fatalf("got %q, want %q", got, "10.0.0.1")
	}
}

// ── SetVMUser / ExecInVM user selection ──────────────────────────────────────

func TestSetVMUser_StoresUser(t *testing.T) {
	a := &Adapter{users: make(map[string]string)}
	a.SetVMUser("my-vm", "ubuntu")
	a.mu.Lock()
	got := a.users["my-vm"]
	a.mu.Unlock()
	if got != "ubuntu" {
		t.Fatalf("users[my-vm] = %q, want %q", got, "ubuntu")
	}
}

func TestSetVMUser_EmptyUserIgnored(t *testing.T) {
	a := &Adapter{users: make(map[string]string)}
	a.SetVMUser("my-vm", "")
	a.mu.Lock()
	_, ok := a.users["my-vm"]
	a.mu.Unlock()
	if ok {
		t.Fatal("empty user should not be stored")
	}
}

func TestSetVMUser_NilMapInitialised(t *testing.T) {
	a := &Adapter{} // users is nil
	a.SetVMUser("vm1", "debian")
	a.mu.Lock()
	got := a.users["vm1"]
	a.mu.Unlock()
	if got != "debian" {
		t.Fatalf("users[vm1] = %q, want %q", got, "debian")
	}
}

func TestSetVMUser_MultipleVMs(t *testing.T) {
	a := &Adapter{users: make(map[string]string)}
	a.SetVMUser("vm-ubuntu", "ubuntu")
	a.SetVMUser("vm-debian", "debian")
	a.SetVMUser("vm-rocky", "rocky")

	cases := map[string]string{
		"vm-ubuntu": "ubuntu",
		"vm-debian": "debian",
		"vm-rocky":  "rocky",
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for name, want := range cases {
		if got := a.users[name]; got != want {
			t.Errorf("users[%s] = %q, want %q", name, got, want)
		}
	}
}
