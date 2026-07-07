//go:build linux

package weft

// host_facts_linux.go implements the OS / kernel / network / storage
// fact collectors using /etc/os-release + /proc + /sys + statfs.
// Pure-Go (no cgo) ; every call is best-effort + degrades silently
// on parse failure so a partially-broken host still registers.
//
// Build-tag gated : darwin / *BSD use the stubs in
// host_facts_other.go. Test path : the Linux dc{1,2,3} VMs run the
// real code ; CI cross-builds catch the stub seams.

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// collectOSRelease returns the value of one /etc/os-release key
// (e.g. "ID" → "debian"). Strips surrounding double-quotes which
// os-release uses on multi-word values like PRETTY_NAME. Empty on
// any read / parse failure.
func collectOSRelease(key string) string {
	// Try /etc/os-release first ; fall back to /usr/lib/os-release
	// (the systemd-recommended secondary location for read-only roots).
	for _, path := range []string{"/etc/os-release", "/usr/lib/os-release"} {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		defer f.Close()
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := scanner.Text()
			eq := strings.IndexByte(line, '=')
			if eq <= 0 {
				continue
			}
			if line[:eq] != key {
				continue
			}
			val := strings.TrimSpace(line[eq+1:])
			val = strings.TrimPrefix(val, `"`)
			val = strings.TrimSuffix(val, `"`)
			return val
		}
	}
	return ""
}

// collectKernelVersion reads /proc/sys/kernel/osrelease (the same
// string `uname -r` returns). Pure file read ; no syscall fork.
func collectKernelVersion() string {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// collectNetworkInterfaces walks net.Interfaces() + reads /sys/class/net
// for the bits the stdlib doesn't expose (link speed, oper state).
// Skips loopback + interfaces without an IP. Returns nil when no
// interface qualifies — caller treats nil + empty equivalently.
func collectNetworkInterfaces() []NetworkInterface {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	out := make([]NetworkInterface, 0, len(ifaces))
	for _, iface := range ifaces {
		// Skip loopback : not informative for operators.
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		var v4, v6 []string
		for _, a := range addrs {
			cidr := a.String()
			if strings.Contains(cidr, ":") {
				v6 = append(v6, cidr)
			} else {
				v4 = append(v4, cidr)
			}
		}
		// Skip interfaces with no IP at all (typically down /
		// not configured) — they'd add noise to the drawer.
		if len(v4) == 0 && len(v6) == 0 {
			continue
		}
		out = append(out, NetworkInterface{
			Name:          iface.Name,
			MAC:           iface.HardwareAddr.String(),
			IPv4CIDRs:     v4,
			IPv6CIDRs:     v6,
			LinkSpeedMbps: readSysfsInt64("/sys/class/net/"+iface.Name+"/speed", 0),
			MTU:           iface.MTU,
			OperState:     readSysfsString("/sys/class/net/" + iface.Name + "/operstate"),
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// readSysfsInt64 reads a one-line int from /sys/* with a default
// fallback. /sys returns -1 / parse errors for some virtual ifaces
// (loopback, bridge, tun) — those become the default value.
func readSysfsInt64(path string, def int64) int64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return def
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || v < 0 {
		return def
	}
	return v
}

func readSysfsString(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// collectStorageMounts parses /proc/mounts, filters to "real" local
// filesystems (skip tmpfs / proc / sysfs / cgroup / overlay), and
// stats each via syscall.Statfs to recover total + free bytes. The
// filter is allow-list based : ext{2,3,4} / xfs / btrfs / zfs / 9p /
// virtio-fs / fuse.* + nfs. Other types fall through silently.
func collectStorageMounts() []StorageMount {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return nil
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	out := make([]StorageMount, 0, 16)
	seen := make(map[string]struct{}, 16)
	for scanner.Scan() {
		// Format : <device> <mountpoint> <fstype> <opts> <dump> <fsck>
		parts := strings.Fields(scanner.Text())
		if len(parts) < 3 {
			continue
		}
		device, mountpoint, fstype := parts[0], parts[1], parts[2]
		if !storageFSTypeAllowed(fstype) {
			continue
		}
		// Deduplicate by mountpoint : bind-mounts + auto-remounts
		// can show the same mountpoint twice.
		if _, dup := seen[mountpoint]; dup {
			continue
		}
		var stat syscall.Statfs_t
		if err := syscall.Statfs(mountpoint, &stat); err != nil {
			continue
		}
		bsize := int64(stat.Bsize)
		total := int64(stat.Blocks) * bsize
		free := int64(stat.Bavail) * bsize
		// Skip 0-sized filesystems (special / pseudo entries the
		// allow-list missed) — they add noise.
		if total == 0 {
			continue
		}
		seen[mountpoint] = struct{}{}
		out = append(out, StorageMount{
			Mountpoint: mountpoint,
			Device:     unescapeMountField(device),
			FSType:     fstype,
			TotalBytes: total,
			FreeBytes:  free,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// storageFSTypeAllowed gates the /proc/mounts walk to filesystem
// types operators care about. Pseudo filesystems (proc / sysfs /
// devtmpfs / cgroup* / tmpfs / overlay / squashfs / autofs) are
// rejected.
func storageFSTypeAllowed(fstype string) bool {
	switch fstype {
	case "ext2", "ext3", "ext4",
		"xfs", "btrfs", "zfs",
		"9p", "virtiofs",
		"nfs", "nfs4",
		"vfat", "exfat", "ntfs", "ntfs3":
		return true
	}
	if strings.HasPrefix(fstype, "fuse.") {
		return true
	}
	return false
}

// unescapeMountField rewrites the \040 / \011 / \012 / \134 octal
// escapes /proc/mounts uses for spaces / tabs / newlines /
// backslashes in device + mountpoint fields. Cheap : most real
// device paths contain none.
func unescapeMountField(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			n, err := strconv.ParseUint(s[i+1:i+4], 8, 8)
			if err == nil {
				out = append(out, byte(n))
				i += 3
				continue
			}
		}
		out = append(out, s[i])
	}
	return string(out)
}

// _ keeps filepath imported when the readSysfsInt64/string callers
// happen to inline. The package's other linux-only paths use it.
var _ = filepath.Join

// collectMemoryMiB reads MemTotal from /proc/meminfo and returns it
// in MiB. /proc/meminfo reports kB ; we divide by 1024. Returns 0
// on any parse failure so the field stays "unknown" rather than
// half-populated.
func collectMemoryMiB() int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		// Format : "MemTotal:        16384000 kB"
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kB, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kB / 1024
	}
	return 0
}
