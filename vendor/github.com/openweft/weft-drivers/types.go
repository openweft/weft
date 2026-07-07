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
	AZ           string // availability zone label, e.g. "us-east-1a"
	Hypervisor   string // "apple-vz" | "qemu-kvm" | "cloud-hypervisor"
	Architecture string // "arm64" | "amd64" | "riscv64" | "loongarch64"
	// Version is the driver plugin's compile-time build version
	// (e.g. "v0.6.0"). Reported via the HostInfo() RPC so weft can
	// surface per-driver versions in the TUI / webui chrome.
	// Empty when the driver isn't built with -X main.version (dev
	// builds) — TUI shows "(dev)" or empty in that case.
	Version string
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

// BackupEncryption configures end-to-end encryption applied BEFORE the
// backup body hits the target. Empty algorithm = no encryption (the
// target sees plaintext) ; non-empty enables an AEAD pass over every
// chunk shipped. The same struct is echoed back in the Backup descriptor
// so restore can re-derive the key without the operator restating the
// algorithm / KDF params (only the passphrase env name + salt are
// needed at restore time).
//
// Algorithms supported by weft-block today :
//
//   * "chacha20-poly1305" : pure-Go AEAD via golang.org/x/crypto. 256-bit
//     keys, 96-bit nonces, fast without hardware AES, single-nonce safe
//     up to ≈256 GiB per stream — bigger volumes get auto-chunked with
//     per-chunk nonce derivation.
//   * "aes-256-gcm"       : hardware-accelerated on AESNI/ARM64 ; same
//     security level as above. Pick this on hosts that have AESNI for
//     better throughput.
//
// KDFs :
//
//   * "argon2id" (default) : memory-hard, OWASP-recommended. Params (memory,
//     iterations, parallelism) live in the Backup descriptor so restore
//     uses the SAME settings the create-time operator chose.
type BackupEncryption struct {
	// Algorithm is "" (no encryption) | "chacha20-poly1305" | "aes-256-gcm".
	Algorithm string
	// PassphraseEnv names the env var holding the encryption passphrase
	// (e.g. "WEFT_BACKUP_PASSPHRASE"). Required when Algorithm != "" ;
	// empty disables encryption regardless of Algorithm.
	PassphraseEnv string
	// KDF is the key derivation function : "argon2id" (default) for
	// passphrase-based ; "raw" treats the env var's value as a hex-
	// encoded 256-bit key (advanced, for KMS-managed deployments where
	// the operator's tool already did the KDF).
	KDF string
	// KDFParams are passed to the KDF. For argon2id : "memory_kib" (default
	// 65536), "iterations" (default 3), "parallelism" (default 2). Empty
	// map uses defaults.
	KDFParams map[string]string
}

// BackupEncryptionInfo is the descriptor echoed back in the Backup
// struct. Carries everything restore needs EXCEPT the passphrase :
// algorithm + KDF + per-backup random salt (so the key derivation is
// reproducible).
type BackupEncryptionInfo struct {
	Algorithm string
	KDF       string
	KDFParams map[string]string
	// SaltHex is the per-backup random salt the KDF was seeded with,
	// hex-encoded. Without it the operator's passphrase derives a
	// different key each backup, defeating restore.
	SaltHex string
}

// BackupSpec is what VolumeDriver.CreateBackup consumes. Snapshot is the
// source snapshot name (the driver always backs up FROM a snapshot, not
// from the live head — caller is responsible for taking the snapshot
// first). Target is the backupstore URL ("oci://registry/repo:tag",
// "s3://bucket@region/path", "sftp://user@host:port/path", …). Labels are
// propagated to the backupstore metadata.
//
// ParentURL, when non-empty, makes the backup INCREMENTAL : the driver
// uses BackupStatus.CompareSnapshot(new, parent) to ship only the block
// ranges that differ from the parent backup's snapshot. Empty ParentURL
// = full backup. Restore walks the parent chain back to a full and
// applies the deltas. The parent backup must still exist at the target
// for restore to work ; weft-block guards Delete against unlinking a
// backup that's a parent of another backup in the same target.
//
// Encryption, when Algorithm != "", wraps every shipped chunk in AEAD —
// the target only stores ciphertext. Encryption is orthogonal to
// ParentURL : incremental + encrypted is the common production path.
type BackupSpec struct {
	VolumeUUID   string
	SnapshotName string
	Target       string
	ParentURL    string // "" → full backup ; URL of a prior backup → incremental delta
	Encryption   BackupEncryption
	Labels       map[string]string
}

// Backup is the descriptor VolumeDriver.{Create,List}Backups returns for
// one backup. URL is the full backup reference at the target store (e.g.
// "oci://registry/repo:vol-snap" or "s3://bucket@region/path?…").
// CreatedAtUnixNs is the backup-time wallclock. ParentURL echoes
// BackupSpec.ParentURL (empty for full backups), so List + Restore can
// walk the chain. Encryption echoes the algorithm + KDF params + per-
// backup salt — empty Algorithm = plaintext backup.
type Backup struct {
	VolumeUUID      string
	SnapshotName    string
	URL             string
	ParentURL       string // "" → full backup ; non-empty → incremental, points at the previous backup
	Encryption      BackupEncryptionInfo
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
	// VsockCID is the AF_VSOCK guest CID the agent allocated for
	// this VM (deterministic hash, range [0x10000, 0xfffefffe]).
	// Drivers that support virtio-vsock must bind this exact CID
	// so GuestPodPlane.Attach's strict-when-known peer check sees
	// the agent's expected value. QEMU :
	//   -device vhost-vsock-pci,guest-cid=<VsockCID>
	// Apple VZ : VZVirtioSocketDevice (note: the API doesn't let
	// userland pick a CID, so the field is advisory there). 0 =
	// no CID assigned (legacy VM ; the agent then treats the pod
	// as "unknown" and falls back to the permissive guard).
	VsockCID uint32
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
