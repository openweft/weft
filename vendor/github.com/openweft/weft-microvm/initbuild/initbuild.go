// Package initbuild produces the initramfs cpio.gz consumed by the
// LinuxBootLoader path.
//
// The artefact is a "newc"-format cpio archive (the format the Linux
// kernel's initrd loader expects), gzipped. The kernel unpacks it into the
// initial rootfs (tmpfs) and runs /init as PID 1. For mono-container that
// init is `weft-microvm-init`; for pod mode it's `weft-init`, and the same archive
// also carries the helper binaries the guest execs (crun, cfs-client, the
// in-VM agent) at their own paths — no go:embed, no extraction copy, the
// kernel places them directly.
//
// Cross-compilation is deliberately NOT done here: the operator produces
// the Linux binaries (`GOOS=linux GOARCH=<arch> go build …`) and passes
// their paths. Keeps this package small and reusable.
package initbuild

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/openweft/weft-microvm/cpio"
)

// File is one regular-file entry to place in the initramfs.
type File struct {
	// Path is the in-initramfs path with no leading slash, e.g. "init"
	// or "bin/crun". Parent directories are created automatically.
	Path string
	// Source is the host path to read the bytes from.
	Source string
	// Mode is the unix permission bits; 0 defaults to 0755.
	Mode uint32
}

// PackFiles writes a gzipped newc cpio archive to out containing each File
// at its Path (with parent directory entries emitted first), all owned by
// root:root. This is how a pod-mode initramfs carries /init plus the
// helper binaries (bin/crun, bin/cfs-client, bin/weft-microvm-agent) the guest
// execs from $PATH.
//
// Both the cpio trailer and the gzip footer are flushed explicitly; a
// close failure surfaces as the function's error rather than silently
// producing a truncated archive a kernel cannot unpack.
func PackFiles(files []File, out io.Writer) error {
	gz := gzip.NewWriter(out)
	cw := cpio.NewWriter(gz)

	// Emit every parent directory once, shallowest first, before any
	// file lands in it — the kernel's cpio unpacker needs the dir to
	// exist to create the file.
	seen := map[string]bool{}
	for _, f := range files {
		for _, d := range parentDirs(f.Path) {
			if seen[d] {
				continue
			}
			seen[d] = true
			if err := cw.WriteDir(d, 0o755); err != nil {
				_ = cw.Close()
				_ = gz.Close()
				return fmt.Errorf("write dir %s: %w", d, err)
			}
		}
	}

	for _, f := range files {
		b, err := os.ReadFile(f.Source)
		if err != nil {
			_ = cw.Close()
			_ = gz.Close()
			return fmt.Errorf("read %s: %w", f.Source, err)
		}
		mode := f.Mode
		if mode == 0 {
			mode = 0o755
		}
		// Mode = S_IFREG | perm.
		if err := cw.WriteFile(cpio.Header{Name: f.Path, Mode: 0o100000 | mode}, b); err != nil {
			_ = cw.Close()
			_ = gz.Close()
			return fmt.Errorf("write %s entry: %w", f.Path, err)
		}
	}
	if err := cw.Close(); err != nil {
		_ = gz.Close()
		return fmt.Errorf("finalise cpio: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("finalise gzip: %w", err)
	}
	return nil
}

// PackFilesToFile is the convenience wrapper that opens dst (0644) and
// hands it to PackFiles. Close errors surface as the return value, so a
// short write at flush time (disk full, quota) doesn't leave a truncated
// initramfs on disk under the guise of success.
func PackFilesToFile(files []File, dst string) (rerr error) {
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", dst, err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && rerr == nil {
			rerr = fmt.Errorf("close %s: %w", dst, cerr)
		}
	}()
	return PackFiles(files, f)
}

// PodInitrd packs a pod-mode initramfs at dst: /init (weft-init) plus the
// guest helper binaries at bin/<name> (crun, cfs-client, the in-VM agent).
// Empty helper paths are skipped, so a build can ship only what it needs.
// The guest resolves them from $PATH (/bin) — no go:embed, no extraction.
func PodInitrd(dst, initBin, crunBin, cfsBin, agentBin string) error {
	files := []File{{Path: "init", Source: initBin}}
	add := func(p, src string) {
		if src != "" {
			files = append(files, File{Path: p, Source: src})
		}
	}
	add("bin/crun", crunBin)
	add("bin/cfs-client", cfsBin)
	add("bin/weft-microvm-agent", agentBin)
	return PackFilesToFile(files, dst)
}

// Pack writes a single-entry initramfs (/init = initBinary). Thin
// back-compat wrapper over PackFiles for the mono-binary case.
func Pack(initBinary string, out io.Writer) error {
	return PackFiles([]File{{Path: "init", Source: initBinary, Mode: 0o755}}, out)
}

// PackToFile is the convenience wrapper that opens dst (0644) and hands it
// to Pack. Close errors surface as the return value so a short write at
// flush time doesn't leave a truncated initramfs.
func PackToFile(initBinary, dst string) (rerr error) {
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", dst, err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && rerr == nil {
			rerr = fmt.Errorf("close %s: %w", dst, cerr)
		}
	}()
	return Pack(initBinary, f)
}

// parentDirs returns the ancestor directories of an initramfs path,
// shallowest first: "bin/crun" → ["bin"], "usr/bin/x" → ["usr","usr/bin"],
// "init" → nil.
func parentDirs(p string) []string {
	parts := strings.Split(p, "/")
	var dirs []string
	for i := 1; i < len(parts); i++ {
		dirs = append(dirs, strings.Join(parts[:i], "/"))
	}
	return dirs
}
