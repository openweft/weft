//go:build linux

package floatingipl2

import (
	"strings"
	"testing"
)

// TestMacvlanNameWithinIFNAMSIZ guards the bug where the generated macvlan name
// exceeded the Linux interface-name limit (IFNAMSIZ-1 = 15 bytes), making every
// LinkAdd fail with ERANGE. Runs without root, so normal CI catches a regression.
func TestMacvlanNameWithinIFNAMSIZ(t *testing.T) {
	const maxIfName = 15 // IFNAMSIZ (16) minus the NUL terminator
	for _, uuid := range []string{
		"", "net-vlan-test", "a", strings.Repeat("z", 256),
		"11111111-2222-3333-4444-555555555555",
	} {
		name := macvlanNameFor(uuid)
		if len(name) > maxIfName {
			t.Errorf("macvlanNameFor(%q) = %q (%d bytes) exceeds IFNAMSIZ-1=%d", uuid, name, len(name), maxIfName)
		}
		if !strings.HasPrefix(name, MacvlanPrefix) {
			t.Errorf("macvlanNameFor(%q) = %q missing prefix %q", uuid, name, MacvlanPrefix)
		}
	}

	// Distinct networks must still map to distinct names (no accidental
	// collision from the shortened suffix in the common case).
	if macvlanNameFor("net-a") == macvlanNameFor("net-b") {
		t.Fatal("distinct UUIDs collided to the same macvlan name")
	}
}
