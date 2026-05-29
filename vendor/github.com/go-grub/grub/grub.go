// Package grub generates and patches GRUB configuration files inside raw
// disk images containing an ext4 filesystem, without requiring any external
// tools or root privileges.
//
// The main entry point is MkConfig, which mirrors what grub-mkconfig does:
// it sources /etc/default/grub and all /etc/default/grub.d/*.cfg drop-ins
// from the disk image itself, then applies the resulting GRUB_CMDLINE_*
// variables to /boot/grub/grub.cfg (or /boot/grub2/grub.cfg).
// No values are hardcoded — the effective configuration is always derived
// from what is present in the image at the time of the call.
package grub

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	ext4 "github.com/go-filesystems/ext4"
)

// FileOp describes a single file to write into a disk image, with an optional
// post-write trigger. The only recognised trigger value is "grub-mkconfig".
type FileOp struct {
	content string
	dst     string
	trigger string // "" | "grub-mkconfig"
}

// NewFileOp creates a FileOp that writes content to dst. trigger may be
// "grub-mkconfig" or empty.
func NewFileOp(content, dst, trigger string) FileOp {
	return FileOp{content: content, dst: dst, trigger: trigger}
}

// Content returns the file content to write.
func (f FileOp) Content() string { return f.content }

// Dst returns the absolute destination path inside the disk image.
func (f FileOp) Dst() string { return f.dst }

// Trigger returns the post-write trigger name (empty means none).
func (f FileOp) Trigger() string { return f.trigger }

// ApplyFileOps writes each FileOp into the ext4 disk image at imagePath and,
// after all writes are done, runs any requested triggers (deduped).
func ApplyFileOps(imagePath string, ops []FileOp) error {
	if len(ops) == 0 {
		return nil
	}
	fs, err := ext4.Open(imagePath, -1)
	if err != nil {
		return fmt.Errorf("grub: open image %s: %w", imagePath, err)
	}
	defer fs.Close()

	triggers := make(map[string]bool)
	for _, op := range ops {
		if err := fs.WriteFile(op.Dst(), []byte(op.Content()), 0o644); err != nil {
			return fmt.Errorf("grub: write %s: %w", op.Dst(), err)
		}
		if op.Trigger() != "" {
			triggers[op.Trigger()] = true
		}
	}
	// Close before running triggers that re-open the disk (e.g. MkConfig).
	fs.Close()

	for trigger := range triggers {
		switch trigger {
		case "grub-mkconfig":
			if err := MkConfig(imagePath); err != nil {
				return fmt.Errorf("grub: grub-mkconfig: %w", err)
			}
		default:
			return fmt.Errorf("grub: unknown trigger %q", trigger)
		}
	}
	return nil
}

// ModOp describes an in-place substitution to apply to a file inside a disk
// image. Old is compiled as a RE2 regular expression (compatible with the vast
// majority of PCRE patterns). Every non-overlapping match of Old in the file
// content is replaced by New. New may reference sub-match capture groups with
// the standard Go syntax: $1, ${name}, etc.
type ModOp struct {
	dst string
	old string
	new string
}

// NewModOp creates a ModOp that replaces every match of the RE2 pattern old
// with new in the file at dst inside the disk image. new may reference capture
// groups using $1 / ${name} notation.
func NewModOp(dst, old, new string) ModOp {
	return ModOp{dst: dst, old: old, new: new}
}

// Dst returns the absolute destination path inside the disk image.
func (m ModOp) Dst() string { return m.dst }

// Old returns the RE2 regular expression pattern to match against.
func (m ModOp) Old() string { return m.old }

// New returns the replacement text.
func (m ModOp) New() string { return m.new }

// ModFileOps applies each ModOp to the ext4 disk image at imagePath. For each
// operation, Old is compiled as a RE2 regular expression and every
// non-overlapping match in the target file is replaced by New. If the file
// content is unchanged the write is skipped. Returns an error if Old is not a
// valid RE2 pattern.
func ModFileOps(imagePath string, ops []ModOp) error {
	if len(ops) == 0 {
		return nil
	}
	fs, err := ext4.Open(imagePath, -1)
	if err != nil {
		return fmt.Errorf("grub: open image %s: %w", imagePath, err)
	}
	defer fs.Close()
	for _, op := range ops {
		re, err := regexp.Compile(op.Old())
		if err != nil {
			return fmt.Errorf("grub: mod %s: invalid pattern %q: %w", op.Dst(), op.Old(), err)
		}
		data, err := fs.ReadFile(op.Dst())
		if err != nil {
			return fmt.Errorf("grub: mod read %s: %w", op.Dst(), err)
		}
		patched := re.ReplaceAll(data, []byte(op.New()))
		if string(patched) == string(data) {
			continue
		}
		if err := fs.WriteFile(op.Dst(), patched, 0o644); err != nil {
			return fmt.Errorf("grub: mod write %s: %w", op.Dst(), err)
		}
	}
	return nil
}

// DeleteFileOps removes each path in dsts from the ext4 disk image at
// imagePath. Missing paths are silently ignored (idempotent).
func DeleteFileOps(imagePath string, dsts []string) error {
	if len(dsts) == 0 {
		return nil
	}
	fs, err := ext4.Open(imagePath, -1)
	if err != nil {
		return fmt.Errorf("grub: open image %s: %w", imagePath, err)
	}
	defer fs.Close()
	for _, dst := range dsts {
		if err := fs.DeleteFile(dst); err != nil {
			return fmt.Errorf("grub: delete %s: %w", dst, err)
		}
	}
	return nil
}

// MkConfig regenerates the GRUB configuration inside the ext4 filesystem at
// imagePath. It sources /etc/default/grub and all /etc/default/grub.d/*.cfg
// files (in sorted order, exactly like the real grub-mkconfig) to build an
// effective GRUB environment, then applies GRUB_CMDLINE_LINUX_DEFAULT and
// GRUB_TERMINAL_OUTPUT to /boot/grub/grub.cfg (or /boot/grub2/grub.cfg).
//
// No values are hardcoded: everything is read from the image at call time.
func MkConfig(imagePath string) error {
	fs, err := ext4.Open(imagePath, -1)
	if err != nil {
		return fmt.Errorf("grub: open image %s: %w", imagePath, err)
	}
	defer fs.Close()

	env, err := mergeGrubDefaults(fs.(*ext4.FS))
	if err != nil {
		return err
	}

	for _, p := range []string{"/boot/grub2/grub.cfg", "/boot/grub/grub.cfg"} {
		ok, err := patchCfgFile(fs.(*ext4.FS), p, env)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
	}
	return nil
}

// mergeGrubDefaults reads /etc/default/grub and all /etc/default/grub.d/*.cfg
// files (sorted by name, like grub-mkconfig), merging them into a single env
// map. Later files override earlier ones.
//
// It is wired through a package-level variable so tests can substitute the
// implementation to exercise the (error-propagating) call site in MkConfig.
var mergeGrubDefaults = func(fs *ext4.FS) (map[string]string, error) {
	env := make(map[string]string)

	if data, err := fs.ReadFile("/etc/default/grub"); err == nil {
		mergeEnv(env, parseGrubEnv(string(data)))
	}

	entries, err := fs.ListDir("/etc/default/grub.d")
	if err == nil {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".cfg") {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		for _, name := range names {
			data, err := fs.ReadFile("/etc/default/grub.d/" + name)
			if err != nil {
				continue
			}
			mergeEnv(env, parseGrubEnv(string(data)))
		}
	}
	return env, nil
}

// parseGrubEnv parses KEY=VALUE and KEY="VALUE" assignments from a
// shell-format grub defaults file. Comments and blank lines are ignored.
// Basic $VAR substitution is performed using already-parsed values.
func parseGrubEnv(content string) map[string]string {
	result := make(map[string]string)
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.IndexByte(line, '=')
		if idx < 1 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
			val = val[1 : len(val)-1]
		}
		// Basic $VAR expansion using values parsed so far in this file.
		val = expandVars(val, result)
		result[key] = val
	}
	return result
}

// expandVars substitutes $KEY and ${KEY} references in s using env.
func expandVars(s string, env map[string]string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		if s[i] != '$' {
			b.WriteByte(s[i])
			i++
			continue
		}
		i++ // skip '$'
		braced := i < len(s) && s[i] == '{'
		if braced {
			i++ // skip '{'
		}
		j := i
		for j < len(s) && (isAlnum(s[j]) || s[j] == '_') {
			j++
		}
		varName := s[i:j]
		if braced && j < len(s) && s[j] == '}' {
			j++
		}
		b.WriteString(env[varName]) // empty string if not found
		i = j
	}
	return b.String()
}

func isAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// mergeEnv copies all entries from src into dst (src overrides dst).
func mergeEnv(dst, src map[string]string) {
	for k, v := range src {
		dst[k] = v
	}
}

// PatchCfgContent rewrites the content of a grub.cfg so that every kernel
// linux / linux16 line gains the extra arguments listed in extraArgs,
// with "quiet" and "splash" removed first. Duplicate args are not added.
//
// It is exported so callers can unit-test the transformation independently.
func PatchCfgContent(content string, extraArgs []string) string {
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		if !strings.HasPrefix(trimmed, "linux ") && !strings.HasPrefix(trimmed, "linux16 ") &&
			!strings.HasPrefix(trimmed, "linux\t") && !strings.HasPrefix(trimmed, "linux16\t") {
			continue
		}
		trimmed = cmdlineRemoveWord(trimmed, "quiet")
		trimmed = cmdlineRemoveWord(trimmed, "splash")
		for _, arg := range extraArgs {
			if !strings.Contains(trimmed, arg) {
				trimmed = strings.TrimRight(trimmed, " ") + " " + arg
			}
		}
		indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		lines[i] = indent + trimmed
	}
	return strings.Join(lines, "\n")
}

// patchCfgFile applies the effective GRUB environment to the grub.cfg at path.
// Returns (true, nil) on success, (false, nil) if absent, (false, err) on I/O error.
func patchCfgFile(fs *ext4.FS, path string, env map[string]string) (bool, error) {
	data, err := fs.ReadFile(path)
	if err != nil {
		return false, nil // file absent — try next candidate
	}
	extraArgs := strings.Fields(env["GRUB_CMDLINE_LINUX_DEFAULT"])
	patched := PatchCfgContent(string(data), extraArgs)
	if patched == string(data) {
		return true, nil
	}
	if err := fs.WriteFile(path, []byte(patched), 0o644); err != nil {
		return false, fmt.Errorf("grub: write %s: %w", path, err)
	}
	return true, nil
}

func cmdlineRemoveWord(s, word string) string {
	s = strings.ReplaceAll(s, " "+word+" ", " ")
	if strings.HasSuffix(s, " "+word) {
		s = s[:len(s)-len(" "+word)]
	}
	return s
}
