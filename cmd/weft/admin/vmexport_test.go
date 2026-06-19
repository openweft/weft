package admin

import (
	"bytes"
	"strings"
	"testing"

	weftv1 "github.com/openweft/weft-proto"
)

func TestParseLabelFilters(t *testing.T) {
	cases := []struct {
		in     []string
		want   int // number of clauses ; 0 means error expected
		errSub string
	}{
		{nil, 0, ""},
		{[]string{"deployment.type=ci"}, 1, ""},
		{[]string{"deployment.type=ci,ha"}, 1, ""}, // OR within
		{[]string{"role=loom", "tier=prod"}, 2, ""},
		{[]string{"badformat"}, 0, "bad filter"},
		{[]string{"k="}, 0, "empty value"},
	}
	for _, tc := range cases {
		clauses, err := parseLabelFilters(tc.in)
		if tc.errSub != "" {
			if err == nil || !strings.Contains(err.Error(), tc.errSub) {
				t.Errorf("%v: expected err containing %q, got %v", tc.in, tc.errSub, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%v: unexpected err %v", tc.in, err)
			continue
		}
		if len(clauses) != tc.want {
			t.Errorf("%v: got %d clauses, want %d", tc.in, len(clauses), tc.want)
		}
	}
}

func TestMatchesAnyAndAll(t *testing.T) {
	ciClause, _ := parseLabelFilters([]string{"deployment.type=ci"})
	haClause, _ := parseLabelFilters([]string{"deployment.type=ha"})

	ciVM := map[string]string{"deployment.type": "ci", "role": "builder"}
	haVM := map[string]string{"deployment.type": "ha", "role": "loom"}
	naked := map[string]string{} // no labels

	// matchesAny — used by --exclude path.
	if !matchesAny(ciVM, ciClause) {
		t.Error("matchesAny(ci, ci) should be true")
	}
	if matchesAny(haVM, ciClause) {
		t.Error("matchesAny(ha, ci) should be false")
	}
	if matchesAny(naked, ciClause) {
		t.Error("matchesAny(naked, ci) should be false (empty key value)")
	}

	// matchesAll — used by --include path.
	if !matchesAll(haVM, haClause) {
		t.Error("matchesAll(ha, ha) should be true")
	}
	if matchesAll(ciVM, haClause) {
		t.Error("matchesAll(ci, ha) should be false")
	}
}

func TestMatchesAny_ORWithinClause(t *testing.T) {
	clauses, err := parseLabelFilters([]string{"deployment.type=ci,ha"})
	if err != nil {
		t.Fatal(err)
	}
	for _, val := range []string{"ci", "ha"} {
		labels := map[string]string{"deployment.type": val}
		if !matchesAny(labels, clauses) {
			t.Errorf("value %q should match OR-clause", val)
		}
	}
	labels := map[string]string{"deployment.type": "stateless"}
	if matchesAny(labels, clauses) {
		t.Errorf("value 'stateless' should NOT match OR-clause")
	}
}

func TestWriteVMsHCL_Shape(t *testing.T) {
	vms := []*weftv1.VMInfo{
		{
			Uuid: "u-1", Name: "web", ProjectUuid: "p-1",
			Image: "img:v1", Cpu: 2, MemMb: 2048,
			Properties: map[string]string{"deployment.type": "ha", "role": "loom"},
		},
		{Uuid: "u-2", Name: "ci-runner", ProjectUuid: "p-2"},
	}
	var buf bytes.Buffer
	if err := writeVMsHCL(&buf, vms); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, `vm "u-1"`) {
		t.Errorf("missing vm header : %s", out)
	}
	if !strings.Contains(out, `name         = "web"`) {
		t.Errorf("missing aligned name : %s", out)
	}
	if !strings.Contains(out, `deployment.type = "ha"`) {
		t.Errorf("missing property : %s", out)
	}
	// propertyless VM still appears, sans properties block
	if !strings.Contains(out, `vm "u-2"`) {
		t.Errorf("u-2 missing")
	}
}
