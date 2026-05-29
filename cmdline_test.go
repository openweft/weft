package weft

import "testing"

func TestMergeProjectEnv(t *testing.T) {
	cases := []struct {
		name    string
		cmdline string
		kv      string
		want    string
	}{
		{
			name:    "empty cmdline",
			cmdline: "",
			kv:      "VZD_PROJECT_UUID=abc",
			want:    "ncl.env=VZD_PROJECT_UUID=abc",
		},
		{
			name:    "no existing ncl.env clause",
			cmdline: "console=hvc0 quiet",
			kv:      "VZD_PROJECT_UUID=abc",
			want:    "console=hvc0 quiet ncl.env=VZD_PROJECT_UUID=abc",
		},
		{
			name:    "existing ncl.env clause is extended, not clobbered",
			cmdline: "console=hvc0 ncl.env=FOO=1:BAR=2 quiet",
			kv:      "VZD_PROJECT_UUID=abc",
			want:    "console=hvc0 ncl.env=FOO=1:BAR=2:VZD_PROJECT_UUID=abc quiet",
		},
		{
			name:    "empty ncl.env clause gets the first entry",
			cmdline: "ncl.env= console=hvc0",
			kv:      "VZD_PROJECT_UUID=abc",
			want:    "ncl.env=VZD_PROJECT_UUID=abc console=hvc0",
		},
		{
			name:    "duplicate key appended at end (last-wins guarantee)",
			cmdline: "ncl.env=VZD_PROJECT_UUID=spoofed",
			kv:      "VZD_PROJECT_UUID=real",
			want:    "ncl.env=VZD_PROJECT_UUID=spoofed:VZD_PROJECT_UUID=real",
		},
		{
			name:    "empty kv is a no-op",
			cmdline: "console=hvc0",
			kv:      "",
			want:    "console=hvc0",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := mergeProjectEnv(c.cmdline, c.kv); got != c.want {
				t.Errorf("mergeProjectEnv(%q, %q)\n got: %q\nwant: %q", c.cmdline, c.kv, got, c.want)
			}
		})
	}
}
