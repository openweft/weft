package drivers

// types.go holds the protobuf-friendly data structures every
// driver consumes. They mirror the corresponding vzd registry
// types (weft.Network → drivers.NetworkSpec etc.) but live in this
// package to avoid a cycle: drivers must not import vzd, since
// vzd uses drivers.
//
// The Adapter in vzd/ does the conversion at the boundary:
//
//   spec := drivers.NetworkSpec{
//       UUID:        n.UUID,
//       ProjectUUID: n.ProjectUUID,
//       …
//   }
//   err := netDriver.EnsureNetwork(ctx, spec)
//
// Keep these struct definitions:
//   * Flat (no nested maps/slices that protobuf can't represent).
//   * String-keyed (for any map field).
//   * Primitive-valued (no pointers, no interfaces, no time.Time
//     in the wire shape — use Unix-ns int64 instead).
//
// time.Time appears in vzd registries; at the driver boundary we
// don't need it (drivers act on the present moment).

// HostInfo identifies a compute node in the cluster. Returned by
// every driver's HostInfo() so the scheduler + audit logs can
// confirm where a side effect landed.
type HostInfo struct {
	UUID         string
	Hostname     string
	AZ           string   // availability zone label, e.g. "us-east-1a"
	Hypervisor   string   // "apple-vz" | "qemu-kvm" | "cloud-hypervisor"
	Architecture string   // "arm64" | "amd64" | "riscv64" | "loongarch64"
}

// NetworkSpec is what NetworkDriver consumes — mirrors weft.Network
// minus the timestamps and minus the DefaultSecurityGroups
// reference (the driver only enforces SG rules; the cross-registry
// resolution happens in the Adapter before the spec is shipped).
type NetworkSpec struct {
	UUID           string
	ProjectUUID    string
	Name           string
	CIDR           string
	Gateway        string
	DNSServers     []string
	Type           string // matches weft.NetworkType
	MeshListenPort int    // mesh only
	MeshEndpoint   string // mesh only
}

// PortSpec is what NetworkDriver consumes when it attaches a VM
// NIC. The driver returns NICHandle so the hypervisor knows the
// device path / tap name to plug into the VM config.
type PortSpec struct {
	UUID            string
	ProjectUUID     string
	VMUUID          string
	NetworkUUID     string
	MAC             string
	IP              string
	WireguardPubKey string // mesh only
	MeshEndpoint    string // mesh only — per-port override of network's endpoint
	// EffectiveSecurityGroups is the resolved SG UUID list the
	// driver should program at the firewall layer. The Adapter
	// merges Port.SecurityGroups (override) with the network's
	// DefaultSecurityGroups before handing the PortSpec down.
	EffectiveSecurityGroups []string
}

// NICHandle is what AttachPort returns: the OS-level identifier
// the hypervisor binds to (e.g. a /dev/tap name on Linux, a
// SocketDeviceConfiguration handle on Apple VZ).
type NICHandle struct {
	Device string // tap0 / vmnet1 / opaque handle ID
	MAC    string // may differ from PortSpec.MAC if the driver enforced uniqueness
}

// VolumeSpec is what VolumeDriver consumes. Mirrors weft.Volume.
type VolumeSpec struct {
	UUID        string
	ProjectUUID string
	Name        string
	SizeGiB     int
	Format      string // "raw" | "qcow2"
}

// AttachedVolume is what AttachVolume returns: the path / URI the
// hypervisor opens, plus access mode.
type AttachedVolume struct {
	BackingPath string // /var/lib/vzd/.../disk.qcow2 — local file path or rbd:// URI
	ReadOnly    bool
}

// VMSpec is what HypervisorDriver consumes at CreateVM time. The
// driver materialises this into the hypervisor's native config
// (vz.VirtualMachineConfiguration, qemu cmdline, etc.).
type VMSpec struct {
	UUID        string
	ProjectUUID string
	Name        string
	CPUCount    int
	MemoryMiB   int
	BootKind    string // "uki" | "direct_linux" | "oci_image"
	BootRef     string // path or ref depending on BootKind
	Cmdline     string // optional kernel cmdline override
	// Disks + NICs are attached separately via AttachDisk / AttachNIC
	// — keeping VMSpec minimal lets create-then-hot-plug flows work
	// the same way as create-with-everything.
}

// DiskSpec describes one disk attachment on a VM. The driver
// looks up the volume via VolumeDriver.BackingPath using
// VolumeUUID, but the Adapter caches that resolution in
// BackingPath so a stand-alone driver call doesn't need to
// re-resolve.
//
// SizeGiB is the requested backing-file size. Used by
// HypervisorDriver.AttachDisk in the transitional "the driver
// also creates the backing file when missing" mode — once the
// VolumeDriver path is wired end-to-end (post-Phase-F), this
// field drops out and creation moves to VolumeDriver.EnsureVolume.
// A value of 0 means "the file must already exist" and the
// driver returns an error if BackingPath is missing.
type DiskSpec struct {
	VolumeUUID  string
	BackingPath string
	Bus         string // "virtio" | "scsi" | "nvme" — hypervisor-dependent
	SizeGiB     int    // transitional: > 0 lets the hypervisor lazily create the backing file
	ReadOnly    bool
	Boot        bool // true for the root disk
}
