// Derive the runtime-style process spec weft-microvm-init consumes from the
// OCI image config. Image config-spec carries Entrypoint, Cmd, Env,
// WorkingDir, User — we flatten that to the same shape weft-microvm-init's
// resolveProcess expects (matching the OCI runtime-spec `process`
// block, minimal subset).
//
// The runtime user field accepts forms like "1000", "1000:1000",
// "user", "user:group". The numeric forms are handled here; the
// named forms are passed through as-is and weft-microvm-init resolves them
// against /etc/passwd + /etc/group from inside the rootfs.
// (Named-user resolution is a roadmap item.)

package microvm

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// processSpec is the shape we write into <rootfs>/.weft-microvm/config.json.
// weft-microvm-init's resolveProcess (in init/cmd/weft-microvm-init/exec.go) parses
// exactly this layout — see "process" handling there.
type processSpec struct {
	Args []string `json:"args"`
	Env  []string `json:"env"`
	Cwd  string   `json:"cwd"`
	User struct {
		UID uint32 `json:"uid"`
		GID uint32 `json:"gid"`
	} `json:"user"`
}

// configFile is the on-disk shape we write: `{ "process": {...} }`.
type configFile struct {
	Process processSpec `json:"process"`
}

// processFromImageConfig collapses an OCI image config's
// Entrypoint+Cmd into the runtime-spec args field. Per OCI spec:
//
//   - Entrypoint provides the command;
//   - Cmd provides default arguments to that command;
//   - if Entrypoint is empty, Cmd alone is the argv;
//   - user CLI overrides (via `--`) replace Cmd, not Entrypoint.
//
// We mirror that semantic but don't apply the user-override here —
// that's the caller's job (run.go reads args.Cmd if set).
func processFromImageConfig(c ocispec.ImageConfig) (processSpec, error) {
	var p processSpec
	p.Args = append(p.Args, c.Entrypoint...)
	p.Args = append(p.Args, c.Cmd...)
	if len(p.Args) == 0 {
		// Some images leave both fields empty; fall back to /bin/sh
		// rather than booting an init that immediately panics on
		// execve("", ...).
		p.Args = []string{"/bin/sh"}
	}
	if len(c.Env) > 0 {
		p.Env = append([]string{}, c.Env...)
	} else {
		p.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	}
	p.Cwd = c.WorkingDir
	if p.Cwd == "" {
		p.Cwd = "/"
	}

	if c.User != "" {
		uid, gid, err := parseImageUser(c.User)
		if err != nil {
			return p, fmt.Errorf("image config User=%q: %w", c.User, err)
		}
		p.User.UID = uid
		p.User.GID = gid
	}
	return p, nil
}

// parseImageUser handles the numeric forms of OCI's User field.
// "1000", "1000:1000". Returns an error for the named forms — those
// need /etc/passwd lookup which we defer to weft-microvm-init (future work).
func parseImageUser(u string) (uint32, uint32, error) {
	left, right, hasGID := strings.Cut(u, ":")
	uid64, err := strconv.ParseUint(left, 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("named users not supported yet (got %q); use uid[:gid] for now", u)
	}
	uid := uint32(uid64)
	if hasGID {
		gid64, err := strconv.ParseUint(right, 10, 32)
		if err != nil {
			return 0, 0, fmt.Errorf("named gid not supported yet (got %q after the colon)", right)
		}
		return uid, uint32(gid64), nil
	}
	return uid, 0, nil
}

// applyUserOverrides folds CLI overrides (from `Args.Cmd`, set by the
// caller from a `-- cmd…` tail) into the base process spec derived
// from the image config.
func applyUserOverrides(base processSpec, args Args) processSpec {
	out := base
	if len(args.Cmd) > 0 {
		out.Args = append([]string{}, args.Cmd...)
	}
	return out
}

// marshalConfig produces the bytes that go to <rootfs>/.weft-microvm/config.json.
func marshalConfig(p processSpec) ([]byte, error) {
	return json.MarshalIndent(configFile{Process: p}, "", "  ")
}
