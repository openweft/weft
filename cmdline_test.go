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
			kv:      "WEFT_PROJECT_UUID=abc",
			want:    "weft.env=WEFT_PROJECT_UUID=abc",
		},
		{
			name:    "no existing weft.env clause",
			cmdline: "console=hvc0 quiet",
			kv:      "WEFT_PROJECT_UUID=abc",
			want:    "console=hvc0 quiet weft.env=WEFT_PROJECT_UUID=abc",
		},
		{
			name:    "existing weft.env clause is extended, not clobbered",
			cmdline: "console=hvc0 weft.env=FOO=1:BAR=2 quiet",
			kv:      "WEFT_PROJECT_UUID=abc",
			want:    "console=hvc0 weft.env=FOO=1:BAR=2:WEFT_PROJECT_UUID=abc quiet",
		},
		{
			name:    "empty weft.env clause gets the first entry",
			cmdline: "weft.env= console=hvc0",
			kv:      "WEFT_PROJECT_UUID=abc",
			want:    "weft.env=WEFT_PROJECT_UUID=abc console=hvc0",
		},
		{
			name:    "duplicate key appended at end (last-wins guarantee)",
			cmdline: "weft.env=WEFT_PROJECT_UUID=spoofed",
			kv:      "WEFT_PROJECT_UUID=real",
			want:    "weft.env=WEFT_PROJECT_UUID=spoofed:WEFT_PROJECT_UUID=real",
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
