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
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
// processFromImageConfigWithRootfs is the rootfs-aware variant : it can
// resolve named users (e.g. `User = "nonroot:nonroot"`) by reading
// `<rootfs>/etc/passwd` + `<rootfs>/etc/group`. Pass rootfs = "" to
// fall back to numeric-only parsing, which is what the legacy
// processFromImageConfig does.
func processFromImageConfigWithRootfs(c ocispec.ImageConfig, rootfs string) (processSpec, error) {
	p, err := processFromImageConfig(c)
	if err == nil || rootfs == "" {
		return p, err
	}
	// Numeric parse failed. Try resolving against the extracted rootfs's
	// /etc/passwd + /etc/group. Common in distroless / chainguard /
	// alpine-minirootfs images that use "nonroot" / "65532" interchangeably.
	if c.User == "" {
		return p, err
	}
	uid, gid, lookupErr := resolveNamedUser(c.User, rootfs)
	if lookupErr != nil {
		// Surface the ORIGINAL error wrapping the resolution failure for
		// context — both are useful for debugging.
		return p, fmt.Errorf("image config User=%q: %w (rootfs lookup also failed : %v)", c.User, err, lookupErr)
	}
	p.User.UID = uid
	p.User.GID = gid
	return p, nil
}

// resolveNamedUser parses the OCI User field's named forms by reading
// the extracted rootfs's identity databases. Supports :
//
//	"alice"        → /etc/passwd → uid, default gid
//	"alice:group"  → /etc/passwd uid, /etc/group gid
//	"alice:1000"   → /etc/passwd uid, numeric gid
//	"1000:group"   → numeric uid, /etc/group gid
//
// Names that don't appear in the databases surface a clean error so
// the caller can fall back to whatever its previous behaviour was.
func resolveNamedUser(u, rootfs string) (uint32, uint32, error) {
	left, right, hasGID := strings.Cut(u, ":")
	uid, err := resolveUser(left, rootfs)
	if err != nil {
		return 0, 0, err
	}
	if !hasGID {
		// No explicit group : default to the user's primary GID from
		// /etc/passwd. resolveUser already had it ; re-derive here for
		// simplicity.
		gid, err := primaryGIDFromPasswd(left, rootfs)
		if err != nil {
			return uid, 0, nil // fall back to gid=0
		}
		return uid, gid, nil
	}
	gid, err := resolveGroup(right, rootfs)
	if err != nil {
		return 0, 0, err
	}
	return uid, gid, nil
}

func resolveUser(name, rootfs string) (uint32, error) {
	if n, err := strconv.ParseUint(name, 10, 32); err == nil {
		return uint32(n), nil
	}
	uid, _, err := lookupPasswd(name, rootfs)
	return uid, err
}

func resolveGroup(name, rootfs string) (uint32, error) {
	if n, err := strconv.ParseUint(name, 10, 32); err == nil {
		return uint32(n), nil
	}
	return lookupGroup(name, rootfs)
}

// primaryGIDFromPasswd reads /etc/passwd to find name's primary GID.
// Distinct from resolveGroup because /etc/passwd carries (uid, gid)
// tuples whereas /etc/group is keyed by group name.
func primaryGIDFromPasswd(name, rootfs string) (uint32, error) {
	_, gid, err := lookupPasswd(name, rootfs)
	return gid, err
}

// lookupPasswd parses /etc/passwd line-by-line for an entry matching
// `name`. Returns (uid, gid, nil) on hit ; an error wrapping
// fs.ErrNotExist on miss or io errors.
//
// /etc/passwd format : `name:passwd:uid:gid:gecos:home:shell` — colon-
// separated, '#' or empty lines ignored. We only need the first 4
// fields ; the rest pass through.
func lookupPasswd(name, rootfs string) (uint32, uint32, error) {
	f, err := os.Open(filepath.Join(rootfs, "etc", "passwd"))
	if err != nil {
		return 0, 0, fmt.Errorf("open passwd: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.SplitN(line, ":", 7)
		if len(fields) < 4 {
			continue
		}
		if fields[0] != name {
			continue
		}
		uid64, err := strconv.ParseUint(fields[2], 10, 32)
		if err != nil {
			return 0, 0, fmt.Errorf("passwd : %s : malformed uid %q", name, fields[2])
		}
		gid64, err := strconv.ParseUint(fields[3], 10, 32)
		if err != nil {
			return 0, 0, fmt.Errorf("passwd : %s : malformed gid %q", name, fields[3])
		}
		return uint32(uid64), uint32(gid64), nil
	}
	if err := sc.Err(); err != nil {
		return 0, 0, fmt.Errorf("read passwd: %w", err)
	}
	return 0, 0, fmt.Errorf("passwd : user %q not found", name)
}

// lookupGroup parses /etc/group for an entry matching `name`.
// /etc/group format : `name:passwd:gid:members,…`.
func lookupGroup(name, rootfs string) (uint32, error) {
	f, err := os.Open(filepath.Join(rootfs, "etc", "group"))
	if err != nil {
		return 0, fmt.Errorf("open group: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.SplitN(line, ":", 4)
		if len(fields) < 3 || fields[0] != name {
			continue
		}
		gid64, err := strconv.ParseUint(fields[2], 10, 32)
		if err != nil {
			return 0, fmt.Errorf("group : %s : malformed gid %q", name, fields[2])
		}
		return uint32(gid64), nil
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("read group: %w", err)
	}
	return 0, fmt.Errorf("group : %q not found", name)
}

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
