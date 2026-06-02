package drivers

// types.go holds the protobuf-friendly data structures every
// driver consumes. They mirror the corresponding weft registry
// types (weft.Network → drivers.NetworkSpec etc.) but live in this
// package to avoid a cycle: drivers must not import weft, since
// weft uses drivers.
//
// The Adapter in weft/ does the conversion at the boundary:
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
// time.Time appears in weft registries; at the driver boundary we
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
	BackingPath string // /var/lib/weft/.../disk.qcow2 — local file path or rbd:// URI
	ReadOnly    bool
}

// SnapshotSpec is what VolumeDriver.CreateSnapshot consumes. Name is the
// snapshot identifier the driver will key by; empty asks the driver to
// generate one (e.g. timestamped). Labels are opaque key/value pairs
// stored alongside the snapshot for filtering / debugging.
type SnapshotSpec struct {
	VolumeUUID string
	Name       string            // empty → driver-generated
	Labels     map[string]string // optional, e.g. {"reason":"pre-upgrade"}
}

// Snapshot is the descriptor VolumeDriver.{Create,List}Snapshots returns
// for one snapshot. SizeBytes is the on-disk delta from the parent (zero
// for the initial / head-derived snapshot). CreatedAtUnixNs is the
// driver-side creation time in nanoseconds since the Unix epoch (we keep
// the wire shape primitive — see types.go header note).
type Snapshot struct {
	VolumeUUID      string
	Name            string
	Parent          string            // name of the parent snapshot, "" if root
	SizeBytes       int64             // delta on disk (sparse-aware)
	CreatedAtUnixNs int64             // driver-side creation timestamp
	Labels          map[string]string // copy of SnapshotSpec.Labels
	UserCreated     bool              // true if not an automatic / system snapshot
}

// BackupSpec is what VolumeDriver.CreateBackup consumes. Snapshot is the
// source snapshot name (the driver always backs up FROM a snapshot, not
// from the live head — caller is responsible for taking the snapshot
// first). Target is the backupstore URL ("s3://bucket@region/path",
// "nfs://host:/export/dir", "cifs://…", …). Labels are propagated to the
// backupstore metadata.
type BackupSpec struct {
	VolumeUUID   string
	SnapshotName string
	Target       string
	Labels       map[string]string
}

// Backup is the descriptor VolumeDriver.{Create,List}Backups returns for
// one backup. URL is the full backup reference at the target store (e.g.
// "s3://bucket@region/path?backup=...&volume=..."). CreatedAtUnixNs is the
// backup-time wallclock.
type Backup struct {
	VolumeUUID      string
	SnapshotName    string
	URL             string
	SizeBytes       int64
	CreatedAtUnixNs int64
	Labels          map[string]string
	State           string // "in-progress" | "complete" | "error" | "unknown"
	Error           string // human-readable error when State == "error"
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
