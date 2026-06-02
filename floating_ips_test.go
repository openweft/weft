package weft

import (
	"context"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"testing"
)

// memStorage is a minimal in-memory Storage so registry tests
// don't need a real file backend. Returns the last Save bytes on
// Load.
type memStorage struct {
	mu  sync.Mutex
	buf []byte
}

func (m *memStorage) Load(context.Context) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]byte, len(m.buf))
	copy(out, m.buf)
	return out, nil
}

func (m *memStorage) Save(_ context.Context, b []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.buf = append(m.buf[:0], b...)
	return nil
}

func newReg(t *testing.T) (*floatingIPRegistry, *memStorage) {
	t.Helper()
	storage := &memStorage{}
	reg, err := loadFloatingIPRegistry(context.Background(), storage)
	if err != nil {
		t.Fatalf("loadFloatingIPRegistry: %v", err)
	}
	return reg, storage
}

func TestFloatingIPAllocate_AutoPicksNextFreeSkippingNetworkAndBroadcast(t *testing.T) {
	reg, _ := newReg(t)
	// /29 has hosts .1 to .6 ; .0 network ; .7 broadcast.
	for _, want := range []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4", "10.0.0.5", "10.0.0.6"} {
		fip, err := reg.allocate(AllocateFloatingIPSpec{
			ProjectUUID: "p1", NetworkUUID: "net1",
		}, "10.0.0.0/29")
		if err != nil {
			t.Fatalf("allocate %s: %v", want, err)
		}
		if fip.Address != want {
			t.Errorf("got %s, want %s", fip.Address, want)
		}
		if fip.Status != FIPStatusAvailable {
			t.Errorf("status = %s", fip.Status)
		}
	}
	// Pool exhausted now.
	if _, err := reg.allocate(AllocateFloatingIPSpec{ProjectUUID: "p1", NetworkUUID: "net1"}, "10.0.0.0/29"); err == nil {
		t.Error("expected exhaustion error")
	}
}

func TestFloatingIPAllocate_SkipsPortInUseAndReserved(t *testing.T) {
	reg, _ := newReg(t)
	fip, err := reg.allocate(AllocateFloatingIPSpec{
		ProjectUUID: "p1", NetworkUUID: "net1",
		PortInUse: []string{"10.0.0.1", "10.0.0.2"},
		Reserved:  []string{"10.0.0.3"},
	}, "10.0.0.0/29")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if fip.Address != "10.0.0.4" {
		t.Errorf("got %s, want 10.0.0.4 (first free after exclusions)", fip.Address)
	}
}

func TestFloatingIPAllocate_ExplicitAddress(t *testing.T) {
	reg, _ := newReg(t)
	fip, err := reg.allocate(AllocateFloatingIPSpec{
		ProjectUUID: "p1", NetworkUUID: "net1", Address: "10.0.0.5",
	}, "10.0.0.0/24")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if fip.Address != "10.0.0.5" {
		t.Errorf("got %s", fip.Address)
	}
}

func TestFloatingIPAllocate_ExplicitRejectsOutOfRange(t *testing.T) {
	reg, _ := newReg(t)
	cases := []struct {
		name string
		addr string
		err  string
	}{
		{"outside cidr", "10.1.0.5", "not in network"},
		{"network address", "10.0.0.0", "network address"},
		{"broadcast address", "10.0.0.255", "broadcast"},
		{"unparseable", "not-an-ip", "parse address"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := reg.allocate(AllocateFloatingIPSpec{
				ProjectUUID: "p1", NetworkUUID: "net1", Address: c.addr,
			}, "10.0.0.0/24")
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("err = %v, want substring %q", err, c.err)
			}
		})
	}
}

func TestFloatingIPAllocate_ExplicitRejectsAlreadyAllocated(t *testing.T) {
	reg, _ := newReg(t)
	if _, err := reg.allocate(AllocateFloatingIPSpec{
		ProjectUUID: "p1", NetworkUUID: "net1", Address: "10.0.0.5",
	}, "10.0.0.0/24"); err != nil {
		t.Fatalf("first allocate: %v", err)
	}
	_, err := reg.allocate(AllocateFloatingIPSpec{
		ProjectUUID: "p1", NetworkUUID: "net1", Address: "10.0.0.5",
	}, "10.0.0.0/24")
	if err == nil || !strings.Contains(err.Error(), "already allocated") {
		t.Errorf("err = %v", err)
	}
}

func TestFloatingIPMap_HappyPath(t *testing.T) {
	reg, _ := newReg(t)
	fip, _ := reg.allocate(AllocateFloatingIPSpec{ProjectUUID: "p1", NetworkUUID: "net1"}, "10.0.0.0/24")
	mapped, err := reg.mapTo(fip.UUID, FIPTargetVM, "vm-web-1")
	if err != nil {
		t.Fatalf("mapTo: %v", err)
	}
	if mapped.Status != FIPStatusActive || mapped.MappedTo != "vm-web-1" || mapped.TargetKind != FIPTargetVM {
		t.Errorf("mapped = %+v", mapped)
	}
	got := reg.listForTarget(FIPTargetVM, "vm-web-1")
	if len(got) != 1 || got[0].UUID != fip.UUID {
		t.Errorf("listForTarget = %+v, want [%s]", got, fip.UUID)
	}
}

func TestFloatingIPMap_IdempotentOnSameTarget(t *testing.T) {
	reg, _ := newReg(t)
	fip, _ := reg.allocate(AllocateFloatingIPSpec{ProjectUUID: "p1", NetworkUUID: "net1"}, "10.0.0.0/24")
	first, err := reg.mapTo(fip.UUID, FIPTargetVM, "vm-web")
	if err != nil {
		t.Fatalf("first map: %v", err)
	}
	again, err := reg.mapTo(fip.UUID, FIPTargetVM, "vm-web")
	if err != nil {
		t.Fatalf("second map: %v", err)
	}
	if again.AllocatedAt != first.AllocatedAt {
		t.Error("idempotent map must not stamp a new AllocatedAt")
	}
}

func TestFloatingIPMap_RejectsDifferentTargetWithoutUnmap(t *testing.T) {
	reg, _ := newReg(t)
	fip, _ := reg.allocate(AllocateFloatingIPSpec{ProjectUUID: "p1", NetworkUUID: "net1"}, "10.0.0.0/24")
	if _, err := reg.mapTo(fip.UUID, FIPTargetVM, "vm-web"); err != nil {
		t.Fatalf("first map: %v", err)
	}
	_, err := reg.mapTo(fip.UUID, FIPTargetVM, "vm-other")
	if err == nil || !strings.Contains(err.Error(), "unmap first") {
		t.Errorf("err = %v", err)
	}
}

func TestFloatingIPUnmap_HappyPathAndIdempotent(t *testing.T) {
	reg, _ := newReg(t)
	fip, _ := reg.allocate(AllocateFloatingIPSpec{ProjectUUID: "p1", NetworkUUID: "net1"}, "10.0.0.0/24")
	reg.mapTo(fip.UUID, FIPTargetVM, "vm-web")
	got, err := reg.unmap(fip.UUID)
	if err != nil {
		t.Fatalf("unmap: %v", err)
	}
	if got.Status != FIPStatusAvailable || got.MappedTo != "" || got.TargetKind != "" {
		t.Errorf("got %+v", got)
	}
	// Second unmap = no-op.
	if _, err := reg.unmap(fip.UUID); err != nil {
		t.Errorf("idempotent unmap: %v", err)
	}
	// Target index cleared.
	if got := reg.listForTarget(FIPTargetVM, "vm-web"); len(got) != 0 {
		t.Errorf("target index not cleared: %+v", got)
	}
}

func TestFloatingIPRelease_RefusesWhenActive(t *testing.T) {
	reg, _ := newReg(t)
	fip, _ := reg.allocate(AllocateFloatingIPSpec{ProjectUUID: "p1", NetworkUUID: "net1"}, "10.0.0.0/24")
	reg.mapTo(fip.UUID, FIPTargetVM, "vm-web")
	if _, err := reg.release(fip.UUID); err == nil {
		t.Error("expected refusal to release active FIP")
	}
	reg.unmap(fip.UUID)
	if _, err := reg.release(fip.UUID); err != nil {
		t.Errorf("release after unmap: %v", err)
	}
}

func TestFloatingIPRoundTrip_PersistAndReload(t *testing.T) {
	reg, storage := newReg(t)
	a, _ := reg.allocate(AllocateFloatingIPSpec{ProjectUUID: "p1", NetworkUUID: "net1"}, "10.0.0.0/24")
	b, _ := reg.allocate(AllocateFloatingIPSpec{ProjectUUID: "p2", NetworkUUID: "net2"}, "192.0.2.0/29")
	reg.mapTo(a.UUID, FIPTargetVM, "vm-web")

	// Reload via a fresh registry.
	reg2, err := loadFloatingIPRegistry(context.Background(), storage)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got, ok := reg2.lookupByUUID(a.UUID)
	if !ok || got.MappedTo != "vm-web" || got.Status != FIPStatusActive {
		t.Errorf("reload lost mapping: %+v ok=%v", got, ok)
	}
	if got, ok := reg2.lookupByUUID(b.UUID); !ok || got.Address != "192.0.2.1" {
		t.Errorf("reload lost address: %+v ok=%v", got, ok)
	}
}

func TestFloatingIPListForProject_Sorted(t *testing.T) {
	reg, _ := newReg(t)
	for _, a := range []string{"10.0.0.5", "10.0.0.3", "10.0.0.1"} {
		_, _ = reg.allocate(AllocateFloatingIPSpec{ProjectUUID: "p1", NetworkUUID: "net1", Address: a}, "10.0.0.0/24")
	}
	got := reg.listForProject("p1")
	addrs := make([]string, len(got))
	for i, f := range got {
		addrs[i] = f.Address
	}
	sortedCopy := append([]string{}, addrs...)
	sort.Strings(sortedCopy)
	for i := range addrs {
		if addrs[i] != sortedCopy[i] {
			t.Errorf("not sorted: %v", addrs)
			break
		}
	}
}

func TestIsIPv4Broadcast(t *testing.T) {
	cases := []struct {
		addr, cidr string
		want       bool
	}{
		{"10.0.0.255", "10.0.0.0/24", true},
		{"10.0.0.0", "10.0.0.0/24", false}, // network address — not broadcast
		{"10.0.0.7", "10.0.0.0/29", true},
		{"10.0.0.6", "10.0.0.0/29", false},
		{"10.0.0.1", "10.0.0.1/32", false},          // /32 = single host, no broadcast
		{"2001:db8::ffff", "2001:db8::/120", false}, // IPv6 has no broadcast concept
	}
	for _, c := range cases {
		addr := netip.MustParseAddr(c.addr)
		prefix := netip.MustParsePrefix(c.cidr)
		if got := isIPv4Broadcast(addr, prefix); got != c.want {
			t.Errorf("isIPv4Broadcast(%s in %s) = %v, want %v", c.addr, c.cidr, got, c.want)
		}
	}
}
