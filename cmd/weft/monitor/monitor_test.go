package monitor

import (
	"testing"
)

func TestSplitComma(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a", []string{"a"}},
		{"a,b", []string{"a", "b"}},
		{"a,b,c", []string{"a", "b", "c"}},
		{"a,", []string{"a"}},
		{",a", []string{"", "a"}},
	}
	for _, tc := range cases {
		got := splitComma(tc.in)
		if !sliceEq(got, tc.want) {
			t.Errorf("splitComma(%q) = %v ; want %v", tc.in, got, tc.want)
		}
	}
}

func sliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestDashIf(t *testing.T) {
	if dashIf("") != "-" {
		t.Errorf("empty → %q ; want '-'", dashIf(""))
	}
	if dashIf("x") != "x" {
		t.Errorf("x → %q ; want 'x'", dashIf("x"))
	}
}
