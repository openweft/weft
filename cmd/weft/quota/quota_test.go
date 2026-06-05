package quota

import (
	"testing"

	weftv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

func strPtr(s string) *string { return &s }

func TestCommand_Structure(t *testing.T) {
	socket, sshSocket, sshKey := strPtr(""), strPtr(""), strPtr("")
	root := Command(socket, sshSocket, sshKey)
	if root.Use != "quota" {
		t.Errorf("root.Use : got %q, want quota", root.Use)
	}
	subs := root.Commands()
	wantTop := map[string]bool{"tenant": false, "project": false}
	for _, sub := range subs {
		wantTop[sub.Use] = true
	}
	for name, seen := range wantTop {
		if !seen {
			t.Errorf("missing top-level subcommand %q", name)
		}
	}
	// Each top-level should have get + set under it.
	for _, top := range subs {
		got := map[string]bool{}
		for _, leaf := range top.Commands() {
			got[leaf.Use] = true
		}
		for _, want := range []string{"get <name|uuid>", "set <name|uuid>"} {
			if !got[want] {
				t.Errorf("%s : missing leaf %q", top.Use, want)
			}
		}
	}
}

func TestQuotaDims_AllProtoFieldsCovered(t *testing.T) {
	// Sanity check : every scalar Int32 field on the Quotas message
	// MUST have an entry in quotaDims, otherwise the operator can't
	// see / set that dimension via the CLI.
	q := &weftv1.Quotas{
		Vcpu: 1, RamGib: 2, Volumes: 3, VolumesGib: 4,
		Shares: 5, SharesGib: 6, Buckets: 7, BucketsGib: 8,
		RegistryGib: 9, FloatingIps: 10, Projects: 11,
	}
	for _, d := range quotaDims {
		v := *d.fieldOf(q)
		if v == 0 {
			t.Errorf("quotaDim %q does not point at a Quotas field with a non-zero seed value — alignment bug?", d.flag)
		}
	}
	if len(quotaDims) != 11 {
		t.Errorf("quotaDims count : got %d, want 11 (vcpu / ram-gib / volumes / volumes-gib / shares / shares-gib / buckets / buckets-gib / registry-gib / floating-ips / projects)", len(quotaDims))
	}
}

func TestTenantSetCmd_FlagsRegistered(t *testing.T) {
	socket, sshSocket, sshKey := strPtr(""), strPtr(""), strPtr("")
	cmd := tenantSetCmd(socket, sshSocket, sshKey)
	for _, d := range quotaDims {
		if cmd.Flags().Lookup(d.flag) == nil {
			t.Errorf("tenant set : --%s flag missing", d.flag)
		}
	}
}

func TestProjectSetCmd_DropsTenantOnlyFlags(t *testing.T) {
	socket, sshSocket, sshKey := strPtr(""), strPtr(""), strPtr("")
	cmd := projectSetCmd(socket, sshSocket, sshKey)
	for _, d := range quotaDims {
		got := cmd.Flags().Lookup(d.flag)
		if d.tenantOnly {
			if got != nil {
				t.Errorf("project set : tenant-only flag --%s should NOT be registered", d.flag)
			}
		} else {
			if got == nil {
				t.Errorf("project set : --%s flag missing", d.flag)
			}
		}
	}
}

func TestApplyQuotaFlags_OnlyPatchesChanged(t *testing.T) {
	socket, sshSocket, sshKey := strPtr(""), strPtr(""), strPtr("")
	cmd := tenantSetCmd(socket, sshSocket, sshKey)
	// We need the backing map. It's created inside tenantSetCmd's
	// closure, so we reach into the cobra Command's flags to inspect
	// what was registered. To exercise applyQuotaFlags directly, we
	// re-register against a fresh dummy command.
	dummy := &cobra.Command{Use: "dummy"}
	backing := make(map[string]*int32, len(quotaDims))
	registerQuotaFlags(dummy, backing, true)

	// Simulate operator passing only --vcpu=42 (the rest stays
	// untouched).
	if err := dummy.ParseFlags([]string{"--vcpu=42"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	q := &weftv1.Quotas{Vcpu: 999, RamGib: 8, Volumes: 5} // pretend existing cap
	applyQuotaFlags(dummy, backing, q, true)
	if q.Vcpu != 42 {
		t.Errorf("Vcpu : got %d, want 42 (operator override)", q.Vcpu)
	}
	if q.RamGib != 8 {
		t.Errorf("RamGib : got %d, want 8 (untouched by absent --ram-gib)", q.RamGib)
	}
	if q.Volumes != 5 {
		t.Errorf("Volumes : got %d, want 5 (untouched)", q.Volumes)
	}
	// Suppress unused-variable lint on the socket trio above.
	_, _, _ = socket, sshSocket, sshKey
	_ = cmd
}

func TestApplyQuotaFlags_ExplicitZeroDistinguishedFromAbsent(t *testing.T) {
	dummy := &cobra.Command{Use: "dummy"}
	backing := make(map[string]*int32, len(quotaDims))
	registerQuotaFlags(dummy, backing, true)
	if err := dummy.ParseFlags([]string{"--vcpu=0"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	q := &weftv1.Quotas{Vcpu: 999}
	applyQuotaFlags(dummy, backing, q, true)
	if q.Vcpu != 0 {
		t.Errorf("explicit --vcpu=0 must apply, got %d (regression : the absent-vs-zero distinction broke)", q.Vcpu)
	}
}

func TestApplyQuotaFlags_ProjectScopeIgnoresProjectsField(t *testing.T) {
	dummy := &cobra.Command{Use: "dummy"}
	backing := make(map[string]*int32, len(quotaDims))
	registerQuotaFlags(dummy, backing, false /* project scope — drops --projects */)
	if dummy.Flags().Lookup("projects") != nil {
		t.Fatal("project scope should not register --projects")
	}
	// Even if we hand-craft a backing entry, project-scope applyQuotaFlags
	// must skip tenant-only dimensions.
	var v int32 = 99
	backing["projects"] = &v
	q := &weftv1.Quotas{Projects: 7}
	applyQuotaFlags(dummy, backing, q, false)
	if q.Projects != 7 {
		t.Errorf("project scope must not patch Quotas.Projects, got %d (want 7)", q.Projects)
	}
}

func TestCloneQuotas(t *testing.T) {
	orig := &weftv1.Quotas{Vcpu: 1, RamGib: 2, Projects: 3}
	cp := cloneQuotas(orig)
	if cp == orig {
		t.Error("clone should return a distinct pointer")
	}
	cp.Vcpu = 999
	if orig.Vcpu == 999 {
		t.Error("clone mutated the source — proto state alias bug")
	}
	if got := cloneQuotas(nil); got == nil {
		t.Error("clone of nil should return a zeroed Quotas, not nil")
	}
}

func TestLooksLikeUUID(t *testing.T) {
	cases := map[string]bool{
		"e9c3b6c4-9ea2-4f3a-9c1d-2d5a2e3d6b7c": true,
		"too-short": false,
		"": false,
	}
	for in, want := range cases {
		if got := looksLikeUUID(in); got != want {
			t.Errorf("looksLikeUUID(%q) : got %v, want %v", in, got, want)
		}
	}
}

// Dial-failure handling is exercised end-to-end by the tenant
// package's TestAllSubcommands_DialError ; the same shared.Client
// path is used here, so we don't re-test it from each leaf.
// Running leaf.Execute() directly with positional args is awkward
// because cobra needs the full command path to know it's running a
// leaf and not looking for a nested subcommand.
