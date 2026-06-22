package weft

// Package vz implements the adapters.Adapter interface using Apple
// Virtualization.framework via github.com/Code-Hex/vz/v3 and oras-go for
// OCI image pulling.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-grub/grub"
	weftplugin "github.com/openweft/weft-driver-plugin"
	driversAPI "github.com/openweft/weft-drivers"
	"github.com/openweft/weft-microvm-init/pkg/pod"
	guestv1 "github.com/openweft/weft-proto/guestv1"
	"github.com/openweft/weft/driverplugins"
	"github.com/openweft/weft/imagestore"
	"golang.org/x/crypto/ssh"
)

const (
	defaultUser = "admin"
)

// ExtraDisk carries provisioning metadata for a data disk beyond the boot disk.
type ExtraDisk struct {
	SizeGiB    int    `json:"size_gib"`
	Label      string `json:"label,omitempty"`
	Mountpoint string `json:"mountpoint,omitempty"`
}

// VZAdapter is the interface implemented by the weft adapter. It includes the
// common adapter methods previously defined in the central `adapters` package
// plus weft-specific helpers.
type VZAdapter interface {
	// Basic adapter API (formerly adapters.Adapter)
	Name() string
	Available() bool
	Pull(ctx context.Context, images []string, parallelism int) error
	PullWithOutput(ctx context.Context, image string, w io.Writer) error
	ImageInCache(image string) bool
	ListOCI() ([]map[string]interface{}, error)
	DeleteOCI(name string) error
	VMExists(name string) bool
	ListLocal() (map[string]map[string]interface{}, error)
	CloneVM(image, project, name string, extraDisks []ExtraDisk, w io.Writer) error
	StartVM(name, cloudInitISO string) error
	StopVM(name string) error
	DeleteVM(name string) error
	ExecInVM(vmName, shellCmd string, stdin io.Reader) ([]byte, error)
	IP(name string) (string, error)
	GetOSFromCache(image string) string

	// VZ-specific extensions
	RegisterMicroVM(project, name string, boot MicroVMBoot, shares []MicroVMShare) error
	// PodCID returns the AF_VSOCK CID the agent allocated for the
	// pod_id (= VM.Name). Used by GuestPodPlane.Attach to verify
	// the announced pod_id against the peer's actual CID. Unknown
	// pods return (0, false) ; the handler interprets that as
	// "no strict expectation, autoregister from peer.CID()".
	PodCID(podID string) (uint32, bool)
	// RegisterPodCID stamps a new (pod_id, cid) entry in the host's
	// podCIDs registry. The GuestPodPlane.Attach handler uses this
	// for the v0.4.51 autoregister-on-first-Hello path that lifts
	// the Apple-VZ readback gap : the host can't pre-allocate the
	// CID on VZ, but learns it from the first live stream.
	RegisterPodCID(podID string, cid uint32)
	// PodSpec returns the operator-supplied desired pod state, used
	// by GuestPodPlane.Attach to populate the HelloAck. Empty + ok=
	// false on unknown pods.
	PodSpec(podID string) (*guestv1.PodSpec, bool)
	// SetPodSpec publishes a PodSpec into the in-memory registry +
	// persists the whole registry to <stateDir>/podspecs.hcl. Passing
	// spec=nil evicts the entry. Backs the SetPodSpec WeftAgent RPC.
	SetPodSpec(podID string, spec *guestv1.PodSpec)
	SetVMUser(name, user string)
	SetSSHKeyPath(path string)
	SetChecksums(checksums map[string]string)
	SetPaths(cachePath, vmsPath string)
	SetEventBus(b EventBus)
	VMDir(name string) string
	ListVMDirs() []VMDirEntry
	DeleteVMDir(projectUUID, name string) error
	VMDirFor(project, name string) string
	// RegistryStorage returns the Storage backing the named
	// registry blob. Cmd/weft uses this to construct catalogues
	// (flavors, future scripts/sshkeys etc.) without going
	// through the adapter's project/user APIs.
	RegistryStorage(name string) Storage
	// Project registry surface.
	Projects() []Project
	ProjectByUUID(uuid string) (Project, bool)
	ProjectsByTenant(tenantUUID string) []Project
	SetProjectTenant(projectUUID, tenantUUID string) error
	TenantCap(tenantUUID string) TenantQuota
	SetTenantCap(tenantUUID string, q TenantQuota) error
	TenantAllocation(tenantUUID string) TenantQuota
	CreateProject(name string) (Project, bool, error)
	RenameProject(uuid, newName string) error
	DeleteProject(uuid string) error
	AddProjectMember(projectUUID, userUUID string) error
	RemoveProjectMember(projectUUID, userUUID string) error
	ProjectMembers(projectUUID string) ([]string, bool)
	// Tenant registry surface — top-level multi-tenant boundary
	// above projects. Persisted JSON via Storage, same shape as the
	// AZ registry ; cascade today is trivial because Project does
	// not yet carry a TenantUUID. See tenants.go.
	Tenants() []Tenant
	TenantByUUID(uuid string) (Tenant, bool)
	TenantByName(name string) (Tenant, bool)
	CreateTenant(name, domain string) (Tenant, bool, error)
	DeleteTenant(uuid string) (blockedProjects int32, err error)
	AddTenantAdmin(tenantUUID, email string) (Tenant, error)
	RemoveTenantAdmin(tenantUUID, email string) (Tenant, error)
	AddTenantMember(tenantUUID, email string, groups []string) (Tenant, error)
	RemoveTenantMember(tenantUUID, email string) (Tenant, error)
	// Inventory (AZ + Rack) registry surface — promoted from
	// webui-local persistence to a first-class control-plane
	// concern in proto v0.7.0. AZ/Rack registries persist via the
	// Adapter's Storage backend just like every other UUID-keyed
	// noun ; cascade safety (DeleteAZ refuses when racks/hosts
	// still reference it ; DeleteRack refuses when hosts still
	// reference it) lives in the Adapter because only it sees
	// every registry.
	AZs() []AZ
	AZByUUID(uuid string) (AZ, bool)
	AZByCode(code string) (AZ, bool)
	AZRackCount(uuid string) int32
	AZHostCount(uuid string) int32
	CreateAZ(code, name, region, status string) (AZ, bool, error)
	UpdateAZ(uuid, name, region, status string) (AZ, error)
	DeleteAZ(uuid string) (blockedRacks, blockedHosts int32, err error)
	Racks(azUUID string) []Rack
	RackByUUID(uuid string) (Rack, bool)
	RackHostCount(uuid string) int32
	CreateRack(azUUID, code, name, status string, heightU int32) (Rack, bool, error)
	UpdateRack(uuid, name, status string, heightU int32) (Rack, error)
	DeleteRack(uuid string) (blockedHosts int32, err error)
	// Network-plane registries (proto v0.8.0). Mirror the inventory
	// pattern : UUID-keyed primary, project / parent-network filters
	// on list, cascade safety in the Adapter wrapper.
	Subnets(networkUUID string) []Subnet
	SubnetByUUID(uuid string) (Subnet, bool)
	CreateSubnet(networkUUID, name, description, cidr, gateway string, dnsServers []string) (Subnet, bool, error)
	UpdateSubnet(uuid, name, description, gateway string, clearDNS bool, dnsServers []string) (Subnet, error)
	DeleteSubnet(uuid string) error
	LoadBalancers(projectUUID string) []LoadBalancer
	LoadBalancerByUUID(uuid string) (LoadBalancer, bool)
	LoadBalancerFIPCount(uuid string) int32
	CreateLoadBalancer(projectUUID, name, listenAddr, protocol string, backends []LBBackend) (LoadBalancer, bool, error)
	UpdateLoadBalancer(uuid, name, listenAddr, protocol string) (LoadBalancer, error)
	SetLoadBalancerBackends(uuid string, backends []LBBackend) (LoadBalancer, error)
	DeleteLoadBalancer(uuid string) (blockedByFIPs int32, err error)
	DNSZones(projectUUID string) []DNSZone
	DNSZoneByUUID(uuid string) (DNSZone, bool)
	DNSZoneByName(name string) (DNSZone, bool)
	DNSZoneRecordCount(uuid string) int32
	CreateDNSZone(projectUUID, name, soaEmail string, ttl int32) (DNSZone, bool, error)
	UpdateDNSZone(uuid, soaEmail string, ttl int32) (DNSZone, error)
	DeleteDNSZone(uuid string) (blockedByRecords int32, err error)
	DNSRecords(zoneUUID string) []DNSRecord
	DNSRecordByUUID(uuid string) (DNSRecord, bool)
	CreateDNSRecord(zoneUUID, name, recordType, value string, ttl, priority int32) (DNSRecord, bool, error)
	UpdateDNSRecord(uuid, value string, ttl, priority int32) (DNSRecord, error)
	DeleteDNSRecord(uuid string) error
	// Resource registries (proto v0.9.0). See resources_adapter.go.
	GetVolumeProperty(volumeUUID, key string) (VolumeProperty, bool)
	SetVolumeProperty(volumeUUID, key, value string) (VolumeProperty, error)
	DeleteVolumeProperty(volumeUUID, key string) error
	Shares(projectUUID string) []Share
	ShareByUUID(uuid string) (Share, bool)
	CreateShare(projectUUID, name string, sizeGB int64, readonly bool, backend string) (Share, bool, error)
	ResizeShare(uuid string, newSizeGB int64) (Share, error)
	DeleteShare(uuid string) error
	Buckets(projectUUID string) []Bucket
	BucketByUUID(uuid string) (Bucket, bool)
	CreateBucket(projectUUID, name, endpoint, region, accessKey, secretKey string) (Bucket, bool, error)
	SetBucketPolicy(uuid, policy string) (Bucket, error)
	DeleteBucket(uuid string) error
	SSHKeyCatalogue() []SSHKeyCatalogueEntry
	SSHKeyCatalogueByUUID(uuid string) (SSHKeyCatalogueEntry, bool)
	SSHKeyCatalogueByName(name string) (SSHKeyCatalogueEntry, bool)
	AddSSHKeyCatalogue(name, publicKey, comment string) (SSHKeyCatalogueEntry, bool, error)
	RemoveSSHKeyCatalogue(uuid string) error
	ImportSSHKeyCatalogue(namePrefix, blob, comment string) ([]SSHKeyCatalogueEntry, int32, error)
	SchedulingRules() []SchedulingRuleEntry
	SchedulingRuleByUUID(uuid string) (SchedulingRuleEntry, bool)
	CreateSchedulingRule(name, selector string, targetCount int32, antiAffinity string, respawn *RespawnPolicyJSON) (SchedulingRuleEntry, bool, error)
	UpdateSchedulingRule(uuid, selector string, targetCount int32, antiAffinity string, respawn *RespawnPolicyJSON, clearRespawn bool) (SchedulingRuleEntry, error)
	DeleteSchedulingRule(uuid string) error
	RegistryRemotes() []RegistryRemote
	RegistryRemoteByUUID(uuid string) (RegistryRemote, bool)
	RegistryRemoteByName(name string) (RegistryRemote, bool)
	SetRegistryRemote(name, endpoint string, insecure bool, credSecretRef string) (RegistryRemote, bool, error)
	DeleteRegistryRemote(uuid string) error
	// User registry surface — maps (OIDC issuer, subject) ↔ stable
	// weft UUID. Volatile fields (email, groups, last_seen) refresh
	// on every successful auth via RegisterUser. See users.go.
	Users() []User
	UserByUUID(uuid string) (User, bool)
	UserBySubject(issuer, subject string) (User, bool)
	RegisterUser(c *Caller) (User, bool, error)
	SetUserDisplayName(uuid, name string) error
	DeleteUser(uuid string) error
	// Network registry surface — per-project L3 networks. Names
	// are scoped per project (two projects can both own "default").
	// See networks.go.
	Networks() []Network
	NetworkByUUID(uuid string) (Network, bool)
	NetworkByName(projectUUID, name string) (Network, bool)
	ListNetworksForProject(projectUUID string) []Network
	CreateNetwork(spec CreateNetworkSpec) (Network, error)
	RenameNetwork(uuid, newName string) error
	SetNetworkDNS(uuid string, servers []string) error
	// SetNetworkDefaultSecurityGroups replaces the network's
	// default-SG list. Every SG UUID is validated to exist in the
	// same project as the network — cross-project leakage is
	// refused. Pass nil/empty to clear.
	SetNetworkDefaultSecurityGroups(networkUUID string, sgUUIDs []string) error
	DeleteNetwork(uuid string) error
	// Volume registry surface — per-project block storage. Same
	// multi-tenant naming as networks. See volumes.go.
	Volumes() []Volume
	VolumeByUUID(uuid string) (Volume, bool)
	VolumeByName(projectUUID, name string) (Volume, bool)
	ListVolumesForProject(projectUUID string) []Volume
	CreateVolume(spec CreateVolumeSpec) (Volume, error)
	RenameVolume(uuid, newName string) error
	ResizeVolume(uuid string, newSizeGiB int) error
	AttachVolume(uuid, vmUUID string) error
	DetachVolume(uuid string) error
	DeleteVolume(uuid string) error
	// VolumeSnapshot registry surface — per-volume CoW snapshots
	// backed by reflink-cloned blobs. See volumesnapshots.go +
	// snapshotstore.go.
	VolumeSnapshots() []VolumeSnapshot
	VolumeSnapshotByUUID(uuid string) (VolumeSnapshot, bool)
	ListVolumeSnapshotsForVolume(volumeUUID string) []VolumeSnapshot
	ListVolumeSnapshotsForProject(projectUUID string) []VolumeSnapshot
	RegisterVolumeSnapshot(ctx context.Context, parentVolumeUUID, name, projectUUID string) (VolumeSnapshot, error)
	RestoreVolumeSnapshot(ctx context.Context, snapshotUUID, newVolumeName string) (Volume, error)
	DeleteVolumeSnapshotByUUID(ctx context.Context, uuid string) error
	// RevertVolumeSnapshotByUUID rolls a block-backend parent
	// volume back to the snapshot's state. File-backend volumes
	// reject with a clear "only supported on block" error.
	RevertVolumeSnapshotByUUID(ctx context.Context, uuid string) error

	// Volume backup surface — block-backend only. Backups ship to
	// oci:// / s3:// / sftp:// / fs:// targets via weft-block.
	CreateVolumeBackup(ctx context.Context, snapshotUUID, target string) (driversAPI.Backup, error)
	ListVolumeBackups(ctx context.Context, target, volumeUUID string) ([]driversAPI.Backup, error)
	DeleteVolumeBackup(ctx context.Context, backupURL string) error
	RestoreVolumeBackup(ctx context.Context, backupURL, newVolumeName, projectUUID string) (Volume, error)

	// Share fan-out surface. Used by PublishShareToProject /
	// UnpublishShareFromProject to drop a share mount onto every
	// VM in a project (CubeFS today, more backends as they land).
	AttachShareToProject(projectUUID string, m pod.ShareMount) (int, error)
	DetachShareFromProject(projectUUID, shareID, mountPoint string) (int, error)
	// Security-group registry surface — per-project firewall
	// containers. Each group carries a slice of ingress / egress
	// rules edited atomically via SetSecurityGroupRules. See
	// security_groups.go.
	SecurityGroups() []SecurityGroup
	SecurityGroupByUUID(uuid string) (SecurityGroup, bool)
	SecurityGroupByName(projectUUID, name string) (SecurityGroup, bool)
	ListSecurityGroupsForProject(projectUUID string) []SecurityGroup
	CreateSecurityGroup(spec CreateSecurityGroupSpec) (SecurityGroup, error)
	RenameSecurityGroup(uuid, newName string) error
	SetSecurityGroupDescription(uuid, desc string) error
	SetSecurityGroupRules(uuid string, rules []SecurityRule) error
	DeleteSecurityGroup(uuid string) error
	// Port registry surface — one VM NIC per entry, attached to
	// one Network. For mesh-type networks the port carries the
	// per-VM WireGuard pubkey. See ports.go and
	// [[wireguard-mesh-networks]].
	PortByUUID(uuid string) (Port, bool)
	ListPortsForVM(vmUUID string) []Port
	ListPortsForNetwork(networkUUID string) []Port
	ListAllPorts() []Port
	CreatePort(spec CreatePortSpec) (Port, error)
	SetPortSecurityGroups(uuid string, sgUUIDs []string) error
	SetPortWireguardPubKey(uuid, pubkey string) error
	DeletePort(uuid string) error
	// Floating-IP registry surface — addresses pulled from a
	// network's CIDR and (optionally) NAT-mapped to a VM or LB.
	// See floating_ips.go / adapter_floating_ips.go.
	ListFloatingIPs() []FloatingIP
	ListFloatingIPsForProject(projectUUID string) []FloatingIP
	ListFloatingIPsForTarget(kind FloatingIPTargetKind, target string) []FloatingIP
	FloatingIPByUUID(uuid string) (FloatingIP, bool)
	AllocateFloatingIP(projectUUID, networkUUID, address string) (FloatingIP, error)
	ReleaseFloatingIP(uuid string) error
	MapFloatingIP(uuid string, kind FloatingIPTargetKind, target string, rateLimitPPS int) (FloatingIP, error)
	UnmapFloatingIP(uuid string) (FloatingIP, error)
	// Host registry surface — global compute-node inventory.
	// One entry per registered weft-agent. Scheduler reads these
	// to pick a host for a new VM. See hosts.go and
	// [[weft-driver-registry-split]].
	Hosts() []Host
	HostByUUID(uuid string) (Host, bool)
	HostByHostname(hostname string) (Host, bool)
	HostsInAZ(az string) []Host
	RegisterHost(spec RegisterHostSpec) (Host, error)
	HeartbeatHost(uuid string) error
	SetHostState(uuid string, state HostState) error
	SetHostProperties(uuid string, properties map[string]string) error
	SetVMProperties(projectUUID, name string, properties map[string]string) (VM, error)
	SetHostCordoned(uuid string, cordoned bool) error
	DeleteHost(uuid string) error
	// ScheduleVMGroup picks `req.Replicas` hosts honouring the
	// cross-replica PlacementRule. Used by the infra orchestrator
	// to fan out a multi-replica plan across distinct AZs / racks
	// / hosts. See scheduler.go + [[weft-placement-rules]].
	ScheduleVMGroup(ctx context.Context, req GroupScheduleRequest) ([]Host, error)
	// VM inventory surface — one entry per managed VM, each
	// carrying its host_uuid for multi-host dispatch. See vms.go
	// and [[weft-driver-registry-split]].
	VMs() []VM
	VMByUUID(uuid string) (VM, bool)
	VMByName(projectUUID, name string) (VM, bool)
	ListVMsForProject(projectUUID string) []VM
	ListVMsForHost(hostUUID string) []VM
	RegisterVM(spec CreateVMSpec) (VM, error)
	SetVMState(uuid string, state VMState) error
	MigrateVM(uuid, newHostUUID string) error
	RenameVMInventory(uuid, newName string) error
	UnregisterVM(uuid string) error
	// Event-bus accessor — consumed by the WatchEvents RPC.
	EventBus() EventBus
	// RenderNATSAuthorization emits the NATS-conf authorization
	// block for the operator to splice into nats.conf. See
	// nats_config.go + [[weft-tenant-event-access]] Phase 3.
	RenderNATSAuthorization(opts NATSAuthorizationOptions) (string, error)
	// SetNATSAuthorizationFile turns on auto-render of the
	// authorization block to the given path. Empty path disables.
	// Per [[weft-tenant-event-access]] Phase-5 follow-up.
	SetNATSAuthorizationFile(path, adminPubkey string)
	// VisibleProjects returns the set of project UUIDs the
	// authenticated caller in `ctx` is allowed to see. The bool
	// `all` is true when the caller has the admin-like grant that
	// removes filtering (returned map is then nil). Defined in
	// acl.go alongside the OIDC group → project ACL mapping.
	VisibleProjects(ctx context.Context) (map[string]struct{}, bool, error)
	// AuthorizeProject resolves a project display-name or UUID to
	// its canonical UUID, checking the caller's ACL grants in
	// `ctx`. Returns the UUID on success or a status-coded error
	// (codes.PermissionDenied / NotFound) on failure. Defined in
	// acl.go.
	AuthorizeProject(ctx context.Context, nameOrUUID string) (string, error)
	// Tenant-quota surface — per-project hard caps the CreateVM /
	// RegisterMicroVM / CreateVolume handlers consult at entry.
	// Zero on a dimension = unlimited. ResourceExhausted on cap
	// breach. See tenant_quotas.go +
	// docs/operations/tenant-quotas.md.
	TenantQuota(projectUUID string) TenantQuota
	SetTenantQuota(projectUUID string, q TenantQuota) error
	EnforceTenantQuotaForVM(projectUUID string, cpu, memoryMiB int) error
	EnforceTenantQuotaForVolume(projectUUID string, sizeGiB int) error
	EnforceTenantQuotaForShare(projectUUID string, sizeGiB int) error
	EnforceTenantQuotaForBucket(projectUUID string) error
	EnforceTenantQuotaForFloatingIP(projectUUID string) error
	// EnforceTenantQuotaForGPU returns ResourceExhausted when
	// admitting a VM with the given RequestedGPUs would push the
	// project's gpu_count / gpu_memory_gib allocation past its
	// caps. Aggregate across the project's already-running VMs —
	// catches both the per-request and the n×small-VMs paths.
	// See tenant_quotas.go.
	EnforceTenantQuotaForGPU(projectUUID string, requestedGPUs []GPURequest) error
	// EnforceTenantQuotaForPCI returns ResourceExhausted when
	// admitting a VM with the given RequestedPCI would push the
	// project's pci_count allocation past its cap. Aggregate
	// across the project's already-running VMs, single-dimension
	// (PCI devices don't carry an aggregate-meaningful memory
	// axis like GPUs do). See tenant_quotas.go.
	EnforceTenantQuotaForPCI(projectUUID string, requestedPCI []PCIRequest) error
	DiskPath(name string) string
	CachedImagePath(imageURL string) (string, error)
	ListCachedImages() ([]CachedImage, error)
	WriteCloudInitISO(name string, data []byte) (string, error)
	// LookupKind reports which driver kind (vz / qemu / legacy single-
	// plugin label) would field an RPC routed to (hostUUID, arch). The
	// observability seam — cmd/weft's RPC interceptor labels
	// `weft_rpc_total{driver_kind=…}` with it. See dispatch.go.
	LookupKind(hostUUID, arch string) string
	// LookupKindForVM is the convenience that resolves a VM by display
	// name + its arch off the inventory, then asks LookupKind. Empty
	// when the VM isn't in the inventory (legacy on-disk VM).
	LookupKindForVM(name string) string
}

// Adapter is the Apple Virtualization.framework-backed VM adapter.
type Adapter struct {
	stateDir   string
	cachePath  string // overrides default stateDir/cache when set
	vmsPath    string // overrides default stateDir/vz when set
	mu         sync.Mutex
	imageStore imagestore.ImageStore // image cache and pull logic
	users      map[string]string     // vmName → SSH user override
	sshKeyPath string                // path to private key for key-based SSH auth
	// defaultProjUUID is the auto-created project UUID for the OS
	// user weft runs as. Resolved lazily on first call to
	// DefaultProjectUUID(); Phase 2 swaps this for the
	// authenticated caller's identity.
	defaultProjUUID string
	// projects is the on-disk registry mapping UUID ↔ display name.
	// Loaded once at startup, written on every mutation.
	projects *projectRegistry
	// userReg maps (OIDC issuer + subject) ↔ weft UUID + per-user
	// metadata (email, groups, display_name). Distinct from the
	// `users` map above, which is the VM-name → SSH-user override.
	// See users.go.
	userReg *userRegistry
	// networkReg holds the per-project L3 networks the platform
	// provisions. Name is scoped per project; see networks.go.
	networkReg *networkRegistry
	// volumeReg holds the per-project block volumes. See volumes.go.
	volumeReg *volumeRegistry
	// snapshotReg + snapshotStore back the VolumeSnapshot RPCs : a
	// row-per-snapshot HCL registry alongside the reflink-cloned
	// blob directory. See volumesnapshots.go + snapshotstore.go.
	snapshotReg   *volumeSnapshotRegistry
	snapshotStore SnapshotStore
	// volumesDir, when non-empty, overrides <stateDir>/volumes as
	// the root holding volume image blobs the SnapshotStore reads
	// from. Tests stub this to a t.TempDir() containing a synthetic
	// parent image. Empty = default layout.
	volumesDir string
	// sgReg holds the per-project security groups. See
	// security_groups.go.
	sgReg *securityGroupRegistry
	// portReg holds VM ↔ Network attachments (one Port per VM
	// NIC, carrying MAC, IP, and per-port WireGuard pubkey for
	// mesh networks). See ports.go.
	portReg *portRegistry
	// fipReg holds the per-project floating-IP pool — addresses
	// allocated from edge networks and (optionally) mapped to a
	// VM or LB target. See floating_ips.go.
	fipReg *floatingIPRegistry
	// hostReg holds the cluster's compute-node inventory. See
	// hosts.go and [[weft-driver-registry-split]].
	hostReg *hostRegistry
	// azReg + rackReg hold the top two tiers of the inventory
	// hierarchy. Promoted from webui-local persistence (v0.7.0
	// proto bump) so the CLI + every other client reaches the
	// same source of truth. Cascade safety lives in the Adapter
	// (DeleteAZ counts racks + hosts ; DeleteRack counts hosts)
	// because only the Adapter has visibility on every registry.
	azReg   *azRegistry
	rackReg *rackRegistry
	// tenantReg holds the top-level multi-tenant boundary. Distinct
	// from tenantQuotas (per-project caps) — this is the tenant
	// roster + admin/member grant lists. See tenants.go.
	tenantReg *tenantRegistry
	// Network-plane registries (proto v0.8.0). Subnets are scoped
	// to a parent network ; LoadBalancers + DNSZones are scoped to
	// a project ; DNSRecords are children of a zone. Cascade safety
	// (LB delete refused while a FIP maps to it, DNSZone delete
	// refused while records still attach) lives in the per-noun
	// Adapter wrapper in network_plane_adapter.go.
	subnetReg    *subnetRegistry
	lbReg        *loadBalancerRegistry
	dnsZoneReg   *dnsZoneRegistry
	dnsRecordReg *dnsRecordRegistry
	// Resource registries (proto v0.9.0). Volume properties mirror
	// VMProperty addressed by volume UUID ; shares are control-plane
	// CubeFS share catalogue entries ; buckets are S3 catalogue
	// entries ; sshKeyCat is the cluster-wide named-key catalogue ;
	// schedRules carry selector + target_count for nominal binding ;
	// registryRemotes is the OCI registry alias map. See
	// resources_adapter.go.
	volumePropReg   *volumePropertyRegistry
	shareReg        *shareRegistry
	bucketReg       *bucketRegistry
	sshKeyCatReg    *sshKeyCatalogueRegistry
	schedRuleReg    *schedulingRuleRegistry
	registryRemReg  *registryRemoteRegistry
	// vmReg holds the VM inventory — one entry per managed VM,
	// each carrying its host_uuid for multi-host dispatch. See
	// vms.go.
	vmReg *vmRegistry
	// tenantQuotas holds the per-project hard caps the create
	// handlers enforce (CreateVM, RegisterMicroVM, CreateVolume).
	// Empty cap = unlimited on that dimension. See
	// tenant_quotas.go + docs/operations/tenant-quotas.md.
	tenantQuotas *tenantQuotaRegistry
	// tenantCaps holds the tenant-level hard caps that
	// GetTenantQuota / SetTenantQuota read+write. Distinct from
	// tenantQuotas (project-keyed) ; lives in its own "tenant-caps"
	// storage blob so the two registries don't share state. See
	// tenant_caps.go.
	tenantCaps *tenantCapRegistry
	// podCIDs maps pod_id (VM.Name) → AF_VSOCK CID for in-process
	// lookups by GuestPodPlane.Attach. Persistent state lives on
	// VM.VsockCID ; this is a hot-path cache rebuilt on agent boot
	// from the inventory.
	podCIDs *podCIDRegistry
	// podSpecs holds the operator-supplied GuestPodPlane PodSpec
	// for each microVM. GuestPodPlane.Attach reads through it to
	// populate HelloAck ; without an entry the guest receives an
	// empty PodSpec and the in-guest reconciler stays idle.
	podSpecs *podSpecRegistry
	// scheduler picks which Host runs a new VM. Defaults to
	// FirstFitScheduler; swappable via SetScheduler. See
	// scheduler.go for the interface + the default policy's
	// rationale.
	scheduler Scheduler
	// driverDispatch maps host UUID → HostHandle (the four
	// driver interfaces for that host). Populated for the local
	// host by initLocalDrivers; remote hosts add themselves via
	// RegisterHostHandle when weft-agent comes online. See
	// dispatch.go and [[weft-driver-registry-split]].
	driverDispatch map[string]*HostHandle
	// driverDispatchSet maps host UUID → driver kind → HostHandle for
	// hosts running multi-plugin mode (Apple Silicon VZ + QEMU on the
	// same machine for cross-arch builds). Single-plugin hosts only
	// populate driverDispatch above ; multi-plugin hosts populate
	// BOTH (driverDispatch carries the primary kind so call sites that
	// don't know the VM's arch keep working). HostHandleOnArch is the
	// arch-aware lookup that consults this map first.
	driverDispatchSet map[string]map[string]*HostHandle
	driverDispatchMu  sync.RWMutex
	// driverPlugins holds the go-plugin client closers for locally-launched
	// driver plugins (one per local hypervisor backend). ClosePlugins kills
	// them; they are also Managed, so weftplugin.Cleanup() reaps any leak at
	// process exit. See localDriverBundle.
	driverPlugins   []io.Closer
	driverPluginsMu sync.Mutex
	// hypervisor selects the local driver backend ("" / "apple-vz" →
	// Apple VZ; "qemu" → QEMU/TCG). Defaults from $WEFT_HYPERVISOR in
	// initLocalDrivers. QEMU/TCG is the backend to use where Apple VZ
	// can't nest (e.g. a non-nested dev VM).
	hypervisor string
	// storageFactory produces the Storage backend for a named
	// registry (e.g. "projects", "users", "networks", "volumes").
	// Set at construction time via NewWithStorage; the legacy New
	// uses the file-backed default. See storage.go for the three
	// implementations and [[etcd-control-plane]] for the prod path.
	storageFactory func(name string) Storage
	// kvStorageFactory is the per-record sibling of storageFactory,
	// optional ; non-nil only when the operator wired etcd / embed-
	// etcd. Registries that want surgical per-record etcd semantics
	// (vmRegistry today, others next) prefer this path over the
	// blob Storage on every mutation.
	kvStorageFactory func(name string) KVStorage
	// bus is the in-process pub-sub spine for PlatformEvents. Per
	// [[weft-event-bus]]: every RecordEvent + every project /
	// lifecycle mutation also Publishes here, and the WatchEvents
	// gRPC handler streams from it.
	bus EventBus
	// natsAuthzPath is the optional path the rendered NATS
	// authorization block is auto-written to on every mutation
	// that changes its output (project create/delete, seed mint).
	// Empty disables auto-render; the renderer is still callable
	// via `weft admin nats-authz`. Per [[weft-tenant-event-access]]
	// post-Phase-4 — closes the operator-runs-by-hand gap.
	natsAuthzPath        string
	natsAuthzAdminPubkey string
}

// EventBus returns the adapter's process-wide event bus. Server
// stubs use it to push events on mutations; the WatchEvents RPC
// handler subscribes for the stream. The interface lets the
// concrete type swap between LocalEventBus (dev) and
// NATSEventBus (prod) without producer-side changes.
func (a *Adapter) EventBus() EventBus { return a.bus }

// SetEventBus replaces the adapter's bus with `b`. Used by main
// after parsing the operator's `event_bus` HCL block to swap the
// default LocalEventBus for a NATSEventBus.
func (a *Adapter) SetEventBus(b EventBus) {
	if b == nil {
		return
	}
	// Re-install the timings.RecordEvent fan-out hook against the
	// new bus. Without this, swapping the bus would silently
	// orphan every guest-side mark from the new subscribers.
	a.bus = b
	hook := func(vmDir, kind string, ts int64, meta map[string]string) {
		project, subject := splitVMDir(a.vmsDir(), vmDir)
		b.Publish(PlatformEvent{
			TsUnixNano:  ts,
			Kind:        kind,
			Subject:     subject,
			ProjectUUID: project,
			Meta:        meta,
		})
	}
	a.installBusHook(hook)
}

// installBusHook wires the package-level RecordEvent fan-out to
// this adapter's bus. The hook is a smart wrapper around the raw
// closure passed in: kinds that already carry a dotted prefix
// (e.g. "vz.state.Running", "server.start_attempted",
// "vz_vm_run.entered") pass through as-is; bare kinds (e.g.
// "init_entered" from a guest WEFT_MARK) get a "guest." prefix.
// That keeps the wire taxonomy clean: every event reads as
// `<source>.<phase>` without each call site spelling its prefix.
func (a *Adapter) installBusHook(raw func(vmDir, kind string, ts int64, meta map[string]string)) {
	wrapped := func(vmDir, kind string, ts int64, meta map[string]string) {
		// Pass kinds that already contain a dot through unchanged;
		// they came from a producer that already namespaced.
		if !containsRune(kind, '.') {
			kind = "guest." + kind
		}
		raw(vmDir, kind, ts, meta)
	}
	busPublishHook.Store(&wrapped)
}

// splitVMDir extracts (projectUUID, vmName) from an absolute or
// relative vmDir. Returns ("", "") when the path doesn't match
// the post-Phase-1 layout — graceful degradation: the event still
// publishes with empty project (= "global"), reaching every
// subscriber.
func splitVMDir(vmsDir, vmDir string) (project, name string) {
	// vmsDir is typically `<stateDir>/vz`; vmDir is
	// `<stateDir>/vz/<projectUUID>/<vmName>`. Trim the prefix, split.
	rel := vmDir
	if vmsDir != "" && len(vmDir) > len(vmsDir) {
		if vmDir[:len(vmsDir)] == vmsDir {
			rel = vmDir[len(vmsDir):]
			if len(rel) > 0 && rel[0] == '/' {
				rel = rel[1:]
			}
		}
	}
	for i := 0; i < len(rel); i++ {
		if rel[i] == '/' {
			return rel[:i], rel[i+1:]
		}
	}
	return "", rel
}

func containsRune(s string, r rune) bool {
	for _, c := range s {
		if c == r {
			return true
		}
	}
	return false
}

// SetVMUser stores the SSH username to use for a specific VM.
// When set, ExecInVM will connect as this user instead of defaultUser.
func (a *Adapter) SetVMUser(name, user string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.users == nil {
		a.users = make(map[string]string)
	}
	if user != "" {
		a.users[name] = user
	}
}

// SetSSHKeyPath stores the path to the SSH private key used for key-based
// authentication when connecting to VMs via ExecInVM. Password authentication
// is not used; a valid key must always be provided.
func (a *Adapter) SetSSHKeyPath(path string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sshKeyPath = path
}

// SetChecksums stores a map of imageURL→checksumURL so that PullImage can
// validate HTTP images before deciding to re-download them.
func (a *Adapter) SetChecksums(checksums map[string]string) {
	a.imageStore.SetChecksums(checksums)
}

// SetPaths overrides the default cache and VMs directories derived from stateDir.
// An empty string keeps the default (stateDir/cache and stateDir/vz respectively).
func (a *Adapter) SetPaths(cachePath, vmsPath string) {
	a.cachePath = cachePath
	a.vmsPath = vmsPath
	a.imageStore.SetDir(a.cacheDir())
}

// New returns a new vz Adapter with state stored under stateDir
// (./state by default). Images are cached under stateDir/cache/ and
// VM state under stateDir/vz/. Equivalent to
// NewWithStorage(stateDir, nil), which falls back to a file-backed
// Storage factory (single-host dev path).
func New(stateDir string) VZAdapter {
	return NewWithStorage(stateDir, nil)
}

// NewWithStorage creates a VZAdapter with an explicit registry
// Storage factory. The factory receives the registry name (e.g.
// "projects") and returns the Storage backing it — typically
// FileStorage for dev, EtcdStorage for prod (3-DC control plane),
// or MemStorage for tests.
//
// Passing nil for the factory uses the default file-based one:
// each registry lives at <vmsDir>/.<name>.hcl.
func NewWithStorage(stateDir string, factory func(name string) Storage) VZAdapter {
	if stateDir == "" {
		stateDir = "state"
	}
	a := &Adapter{
		stateDir:       stateDir,
		storageFactory: factory,
		bus:            NewEventBus(),
	}
	return a.afterStorageWired()
}

// NewWithKVStorage is the V0.1.4 sibling of NewWithStorage : the
// caller passes both a blob factory (for legacy / file backends)
// AND a KV factory (for etcd-backed per-record). Registries that
// opt into per-record (vmRegistry today) use the KV path ; others
// stay on blob. Pass nil for kvFactory to fall back to blob-only
// behaviour, exactly equivalent to NewWithStorage.
func NewWithKVStorage(stateDir string, factory func(name string) Storage, kvFactory func(name string) KVStorage) VZAdapter {
	if stateDir == "" {
		stateDir = "state"
	}
	a := &Adapter{
		stateDir:         stateDir,
		storageFactory:   factory,
		kvStorageFactory: kvFactory,
		bus:              NewEventBus(),
	}
	return a.afterStorageWired()
}

// afterStorageWired is the shared tail of both NewWithStorage and
// NewWithKVStorage : everything past the factory assignment is
// identical, so extracting the body keeps the two entrypoints in
// sync.
func (a *Adapter) afterStorageWired() VZAdapter {
	// Install the bus fan-out hook so every RecordEvent (including
	// the dozens already scattered across runvm.go's state-change
	// goroutine + the guest-mark console watcher) reaches the bus
	// without each call site needing to know. The hook closure
	// derives project/subject from the vmDir path (post-Phase-1
	// layout: <vmsDir>/<projectUUID>/<vmName>/).
	hook := func(vmDir, kind string, ts int64, meta map[string]string) {
		project, subject := splitVMDir(a.vmsDir(), vmDir)
		a.bus.Publish(PlatformEvent{
			TsUnixNano:  ts,
			Kind:        "guest." + kind, // RecordEvent kinds are guest-side observability
			Subject:     subject,
			ProjectUUID: project,
			Meta:        meta,
		})
	}
	// Some kinds RecordEvent emits are server-side (vz_vm_run.entered,
	// server.start_attempted, vz.state.*); their callers prefix the
	// kind themselves so the "guest." prefix here would be wrong.
	// Refine: detect kinds that already carry a dotted prefix and
	// leave them alone.
	a.installBusHook(hook)
	a.imageStore = newImageStore(a.cacheDir())
	if a.storageFactory == nil {
		a.storageFactory = func(name string) Storage {
			return NewFileStorageInDir(a.vmsDir(), name)
		}
	}
	a.initProjects()
	a.initInventory()
	a.initTenants()
	a.initUsers()
	a.initNetworks()
	a.initNetworkPlane()
	a.initVolumes()
	a.initVolumeSnapshots()
	a.initSecurityGroups()
	a.initPorts()
	a.initFloatingIPs()
	a.initHosts()
	a.initVMs()
	a.initTenantQuotas()
	a.initTenantCaps()
	a.initPodCIDs()
	a.initPodSpecs()
	a.initResources()
	a.scheduler = FirstFitScheduler{} // operator-overridable via SetScheduler
	if err := a.selfRegisterHost(); err != nil {
		// Non-fatal: weft still serves requests, but the registry
		// won't list this host until a subsequent restart
		// succeeds. Logged so operators notice.
		fmt.Fprintf(os.Stderr, "weft: self-register host: %v\n", err)
	}
	a.initLocalDrivers()
	// Drivers are now wired in the dispatch table : refresh the host
	// registry with the per-driver versions HostInfo() reports. This
	// is a no-op when the local driver didn't surface a Version
	// (legacy / dev builds without -X main.version).
	a.refreshLocalDriverVersions()
	a.migrateLegacyLayout()
	a.migrateNamedProjectDirs()
	return a
}

// refreshLocalDriverVersions queries each loaded driver's HostInfo()
// for its compile-time build version and updates the local Host
// registry entry's DriverVersions map. Called after initLocalDrivers
// so the dispatch table is populated. Best-effort : a driver that
// fails HostInfo or returns empty Version is silently skipped.
func (a *Adapter) refreshLocalDriverVersions() {
	if a.hostReg == nil {
		return
	}
	hostUUID, err := a.loadOrCreateHostUUID()
	if err != nil || hostUUID == "" {
		return
	}
	existing, ok := a.hostReg.byUUID[hostUUID]
	if !ok {
		return
	}
	versions := map[string]string{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	a.mu.Lock()
	for kind, h := range a.driverDispatchSet[hostUUID] {
		if h == nil || h.Hypervisor == nil {
			continue
		}
		info, err := h.Hypervisor.HostInfo(ctx)
		if err != nil {
			continue
		}
		if info.Version != "" {
			versions[kind] = info.Version
		}
	}
	// Single-driver path : the legacy dispatch table holds one entry
	// under the host UUID without a kind label ; that's the local
	// driver. Query it under its self-reported hypervisor label.
	if h, ok2 := a.driverDispatch[hostUUID]; ok2 && len(versions) == 0 && h != nil && h.Hypervisor != nil {
		info, err := h.Hypervisor.HostInfo(ctx)
		if err == nil && info.Version != "" {
			versions[existing.Hypervisor] = info.Version
		}
	}
	a.mu.Unlock()
	if len(versions) == 0 {
		return
	}
	a.hostReg.mu.Lock()
	existing.DriverVersions = versions
	a.hostReg.byUUID[hostUUID] = existing
	_ = a.hostReg.persistOne(existing)
	a.hostReg.mu.Unlock()
}

// initUsers loads the on-disk user registry via storageFactory.
// Failure to load downgrades to an empty in-memory registry — same
// resilience contract as initProjects.
func (a *Adapter) initUsers() {
	storage := a.storageFactory("users")
	reg, err := loadUserRegistry(context.Background(), storage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "weft: load user registry: %v\n", err)
		reg = &userRegistry{
			storage:    storage,
			byUUID:     make(map[string]User),
			subjectIdx: make(map[string]string),
		}
	}
	a.userReg = reg
}

// initNetworks loads the on-disk network registry via storageFactory.
func (a *Adapter) initNetworks() {
	storage := a.storageFactory("networks")
	reg, err := loadNetworkRegistry(context.Background(), storage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "weft: load network registry: %v\n", err)
		reg = &networkRegistry{
			storage:    storage,
			byUUID:     make(map[string]Network),
			nameIdx:    make(map[string]string),
			projectIdx: make(map[string]map[string]struct{}),
		}
	}
	a.networkReg = reg
}

// --- Volume registry wrappers --------------------------------------------
// Thin delegations satisfying the VZAdapter interface. The
// volumeRegistry in volumes.go owns the data; the Adapter just
// surfaces it so server stubs in cmd/weft/main.go can stay
// implementation-agnostic.

func (a *Adapter) Volumes() []Volume { return a.volumeReg.list() }

func (a *Adapter) VolumeByUUID(uuid string) (Volume, bool) {
	return a.volumeReg.lookupByUUID(uuid)
}

func (a *Adapter) VolumeByName(projectUUID, name string) (Volume, bool) {
	return a.volumeReg.lookupByName(projectUUID, name)
}

func (a *Adapter) ListVolumesForProject(projectUUID string) []Volume {
	return a.volumeReg.listForProject(projectUUID)
}

func (a *Adapter) CreateVolume(spec CreateVolumeSpec) (Volume, error) {
	v, err := a.volumeReg.create(spec)
	if err == nil {
		a.bus.Publish(PlatformEvent{
			Kind:        "volume.created",
			Subject:     v.UUID,
			ProjectUUID: v.ProjectUUID,
			Meta:        map[string]string{"name": v.Name, "size_gib": fmt.Sprintf("%d", v.SizeGiB)},
		})
	}
	return v, err
}

func (a *Adapter) RenameVolume(uuid, newName string) error {
	if err := a.volumeReg.setName(uuid, newName); err != nil {
		return err
	}
	a.bus.Publish(PlatformEvent{
		Kind:    "volume.renamed",
		Subject: uuid,
		Meta:    map[string]string{"new_name": newName},
	})
	return nil
}

func (a *Adapter) ResizeVolume(uuid string, newSizeGiB int) error {
	if err := a.volumeReg.resize(uuid, newSizeGiB); err != nil {
		return err
	}
	a.bus.Publish(PlatformEvent{
		Kind:    "volume.resized",
		Subject: uuid,
		Meta:    map[string]string{"size_gib": fmt.Sprintf("%d", newSizeGiB)},
	})
	return nil
}

func (a *Adapter) AttachVolume(uuid, vmUUID string) error {
	if err := a.volumeReg.attach(uuid, vmUUID); err != nil {
		return err
	}
	a.bus.Publish(PlatformEvent{
		Kind:    "volume.attached",
		Subject: uuid,
		Meta:    map[string]string{"vm_uuid": vmUUID},
	})
	return nil
}

func (a *Adapter) DetachVolume(uuid string) error {
	if err := a.volumeReg.detach(uuid); err != nil {
		return err
	}
	a.bus.Publish(PlatformEvent{Kind: "volume.detached", Subject: uuid})
	return nil
}

func (a *Adapter) DeleteVolume(uuid string) error {
	if err := a.volumeReg.delete(uuid); err != nil {
		return err
	}
	a.bus.Publish(PlatformEvent{Kind: "volume.deleted", Subject: uuid})
	return nil
}

// initVolumes loads the on-disk volume registry via storageFactory.
func (a *Adapter) initVolumes() {
	storage := a.storageFactory("volumes")
	reg, err := loadVolumeRegistry(context.Background(), storage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "weft: load volume registry: %v\n", err)
		reg = &volumeRegistry{
			storage:    storage,
			byUUID:     make(map[string]Volume),
			nameIdx:    make(map[string]string),
			projectIdx: make(map[string]map[string]struct{}),
		}
	}
	a.volumeReg = reg
}

// initSecurityGroups loads the security-group registry via storageFactory.
func (a *Adapter) initSecurityGroups() {
	storage := a.storageFactory("security_groups")
	reg, err := loadSecurityGroupRegistry(context.Background(), storage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "weft: load security-group registry: %v\n", err)
		reg = &securityGroupRegistry{
			storage:    storage,
			byUUID:     make(map[string]SecurityGroup),
			nameIdx:    make(map[string]string),
			projectIdx: make(map[string]map[string]struct{}),
		}
	}
	a.sgReg = reg
}

// initPorts loads the port registry via storageFactory.
func (a *Adapter) initPorts() {
	storage := a.storageFactory("ports")
	reg, err := loadPortRegistry(context.Background(), storage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "weft: load port registry: %v\n", err)
		reg = &portRegistry{
			storage:    storage,
			byUUID:     make(map[string]Port),
			vmIdx:      make(map[string]map[string]struct{}),
			networkIdx: make(map[string]map[string]struct{}),
			ipIdx:      make(map[string]string),
			macIdx:     make(map[string]string),
		}
	}
	a.portReg = reg
}

// initLocalDrivers builds the in-process driver Bundle for the
// host weft-control is running on. Must run AFTER selfRegisterHost
// so the bundle's HostUUID matches what the Host registry knows
// about us — keeping the identity consistent end-to-end.
//
// Today the bundle is consulted only by Adapter.DeleteVM; future
// commits port one method at a time (StartVM, StopVM, AttachDisk,
// …) until the Adapter is a thin dispatcher over the driver
// interfaces.
func (a *Adapter) initLocalDrivers() {
	hostUUID, _ := a.loadOrCreateHostUUID() // best-effort; empty UUID is acceptable for tests
	hostname, _ := os.Hostname()

	hv := a.hypervisor
	if hv == "" {
		hv = os.Getenv("WEFT_HYPERVISOR")
	}
	// Bundle construction is platform-specific: darwin uses Apple VZ (or QEMU
	// when hv=="qemu"); linux always uses QEMU/KVM. See adapter_darwin.go /
	// adapter_linux.go. Registered in the multi-host dispatch table under the
	// persisted host UUID (single-host installs have exactly this one entry).
	handle := localDriverBundle(a, hostUUID, hostname, hv)
	if hostUUID != "" && handle != nil {
		_ = a.RegisterHostHandle(hostUUID, handle)
	}
}

// launchDriverPlugin is the seam the rest of the adapter goes through to start
// a driver plugin. Default: resolve the plugin binary (local-first, then OCI
// pull from the configured registry — see driverplugins), then launch it over
// go-plugin (adapting *plugin.Client to an io.Closer). Tests override the whole
// closure to return an in-process fake DriverSet, so weft.New neither resolves
// nor spawns anything.
var launchDriverPlugin = func(opts weftplugin.LaunchOptions) (*weftplugin.DriverSet, io.Closer, error) {
	cacheDir := filepath.Join(opts.StateDir, "plugins")
	path, err := driverplugins.Resolve(context.Background(), opts.Executable, cacheDir, driverplugins.FromEnv())
	if err != nil {
		return nil, nil, err
	}
	opts.Executable = path
	set, client, err := weftplugin.Launch(opts)
	if err != nil {
		return nil, nil, err
	}
	return set, closerFunc(client.Kill), nil
}

type closerFunc func()

func (f closerFunc) Close() error { f(); return nil }

// localDriverBundle launches the host's driver plugin and returns its handle.
// The driver now lives in an external executable (weft-driver-vz on darwin,
// weft-driver-qemu elsewhere or when hv=="qemu") so the weft core stays
// pure-Go — see weft-driver-plugin. Returns nil (and logs) when the plugin
// can't be launched; the dispatch table simply gets no local entry.
func localDriverBundle(a *Adapter, hostUUID, hostname, hv string) *HostHandle {
	exe := defaultDriverPlugin // platform default (adapter_darwin.go / adapter_linux.go)
	if hv == "qemu" {
		exe = "weft-driver-qemu"
	}
	set, closer, err := launchDriverPlugin(weftplugin.LaunchOptions{
		Executable: exe,
		HostUUID:   hostUUID,
		Hostname:   hostname,
		StateDir:   a.stateDir,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "weft: local driver plugin %q unavailable: %v\n", exe, err)
		return nil
	}
	a.driverPluginsMu.Lock()
	a.driverPlugins = append(a.driverPlugins, closer)
	a.driverPluginsMu.Unlock()
	return &HostHandle{
		Hypervisor: set.Hypervisor,
		Network:    set.Network,
		Volume:     set.Volume,
		Image:      set.Image,
	}
}

// ClosePlugins kills every locally-launched driver plugin. Safe to call more
// than once; safe when none were launched.
func (a *Adapter) ClosePlugins() {
	a.driverPluginsMu.Lock()
	closers := a.driverPlugins
	a.driverPlugins = nil
	a.driverPluginsMu.Unlock()
	for _, c := range closers {
		_ = c.Close()
	}
}

// copyTree recursively copies src (file or directory) to dst — the portable
// fallback for staging a share when APFS clonefile isn't available (non-APFS
// darwin, and linux). cloneOrCopyTree picks the fast path per platform.
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		// Replicate symlinks verbatim — never follow. Following a symlink to a
		// directory and feeding the fd to io.Copy lands on copy_file_range with
		// EISDIR (seen in OCI rootfs trees that ship a zoneinfo posix/ mirror).
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			return os.Symlink(link, target)
		}
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		}
		return copyFile(p, target, info.Mode().Perm())
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_RDWR|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, cerr := io.Copy(out, in)
	if cerr2 := out.Close(); cerr2 != nil && cerr == nil {
		cerr = cerr2
	}
	return cerr
}

// initVMs loads the VM inventory via storageFactory.
//
// Two backends today : the legacy blob path (file storage, in-memory
// tests, pre-V0.1.4 etcd clusters that still hold a `vms` key) and
// the V0.1.4 per-record KV path (etcd, /weft/vms/<uuid> keys, surgical
// Put+Watch on every mutation). KV is preferred when the operator
// wired etcd backend ; it migrates the legacy blob on first run.
func (a *Adapter) initVMs() {
	storage := a.storageFactory("vms")
	if a.kvStorageFactory != nil {
		kv := a.kvStorageFactory("vms")
		reg, err := loadVMRegistryKV(context.Background(), kv, storage)
		if err != nil {
			fmt.Fprintf(os.Stderr, "weft: load vm registry (kv): %v\n", err)
			reg = &vmRegistry{
				storage:    storage,
				kv:         kv,
				byUUID:     make(map[string]VM),
				nameIdx:    make(map[string]string),
				projectIdx: make(map[string]map[string]struct{}),
				hostIdx:    make(map[string]map[string]struct{}),
			}
		}
		a.vmReg = reg
		return
	}
	reg, err := loadVMRegistry(context.Background(), storage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "weft: load vm registry: %v\n", err)
		reg = &vmRegistry{
			storage:    storage,
			byUUID:     make(map[string]VM),
			nameIdx:    make(map[string]string),
			projectIdx: make(map[string]map[string]struct{}),
			hostIdx:    make(map[string]map[string]struct{}),
		}
	}
	a.vmReg = reg
}

// WatchVMRegistry starts a background goroutine that keeps the
// in-memory VM registry in sync with the persistent layer.
//
// Two paths :
//
//   - V0.1.4 KV mode (a.vmReg.kv != nil) : the etcd KVStorage
//     streams per-record events (Put / Delete) for /weft/vms/<uuid>
//     ; the watcher applies each event surgically via
//     applyKVEvent — no full reload, no parse of unchanged records.
//     This is the path the user opted into to dodge the full-blob
//     thrash described in the design review.
//
//   - V0.1.3 blob mode (a.vmReg.storage implements WatchableStorage)
//     : etcd Watch on the single `vms` key triggers reloadFromStorage
//     which re-parses the entire blob into a new set of indices.
//
//   - Anything else (file backend, in-memory tests) : no-op —
//     these backends are inherently per-process and don't carry
//     cross-host semantics.
//
// Returns immediately. The goroutine exits when ctx is cancelled.
func (a *Adapter) WatchVMRegistry(ctx context.Context) {
	if a.vmReg == nil {
		return
	}
	if a.vmReg.kv != nil {
		ch := a.vmReg.kv.WatchKeys(ctx)
		go func() {
			for ev := range ch {
				a.vmReg.applyKVEvent(ev)
				// V0.1.7 : no vm.registry_reloaded publish. The original
				// intent was to signal consumers (agentrespawn) to
				// re-evaluate orphans, but no consumer subscribes today
				// and a future subscriber that reacts by writing the
				// registry would close a feedback loop (Save → Watch
				// → reload → Publish → Save). The applyKVEvent above
				// already mutates the indices ; consumers that need a
				// notification specifically for hostIdx changes should
				// subscribe to vm.migrated which the claim path
				// already publishes per-VM.
			}
		}()
		return
	}
	ws, ok := a.vmReg.storage.(WatchableStorage)
	if !ok {
		return
	}
	ch := ws.Watch(ctx)
	go func() {
		for range ch {
			if err := a.vmReg.reloadFromStorage(ctx); err != nil {
				fmt.Fprintf(os.Stderr, "weft: reload vm registry on remote change: %v\n", err)
				continue
			}
		}
	}()
}

// ── VM inventory surface ────────────────────────────────────────

// VMs returns every registered VM across all projects.
func (a *Adapter) VMs() []VM {
	if a.vmReg == nil {
		return nil
	}
	return a.vmReg.list()
}

// VMByUUID resolves a UUID to its VM entry.
func (a *Adapter) VMByUUID(uuid string) (VM, bool) {
	if a.vmReg == nil {
		return VM{}, false
	}
	return a.vmReg.lookupByUUID(uuid)
}

// VMByName resolves (projectUUID, name) to a VM.
func (a *Adapter) VMByName(projectUUID, name string) (VM, bool) {
	if a.vmReg == nil {
		return VM{}, false
	}
	return a.vmReg.lookupByName(projectUUID, name)
}

// ListVMsForProject returns every VM in the project.
func (a *Adapter) ListVMsForProject(projectUUID string) []VM {
	if a.vmReg == nil {
		return nil
	}
	return a.vmReg.listForProject(projectUUID)
}

// ListVMsForHost returns every VM on the host. Used by the
// reconciler when a host disconnects: enumerate the affected
// VMs, mark them stopped, schedule failover.
func (a *Adapter) ListVMsForHost(hostUUID string) []VM {
	if a.vmReg == nil {
		return nil
	}
	return a.vmReg.listForHost(hostUUID)
}

// RegisterVM adds a new VM to the inventory. Cross-registry
// validation:
//
//   - project_uuid must exist in projectRegistry.
//   - host_uuid must exist in hostRegistry AND have a registered
//     driver handle.
//
// The actual VM-state provisioning (vmDir, nvram, machine-id, …)
// is the caller's responsibility — typically CloneVM /
// RegisterMicroVM — which call this AFTER the host-local
// HypervisorDriver.CreateVM has succeeded.
func (a *Adapter) RegisterVM(spec CreateVMSpec) (VM, error) {
	if a.vmReg == nil {
		return VM{}, fmt.Errorf("vm registry not initialised")
	}
	if a.projects != nil {
		if _, ok := a.projects.lookupByUUID(spec.ProjectUUID); !ok {
			return VM{}, fmt.Errorf("project %q not found", spec.ProjectUUID)
		}
	}
	if a.hostReg != nil {
		if _, ok := a.hostReg.lookupByUUID(spec.HostUUID); !ok {
			return VM{}, fmt.Errorf("host %q not found", spec.HostUUID)
		}
	}
	if _, err := a.HostHandleOn(spec.HostUUID); err != nil {
		return VM{}, fmt.Errorf("host %q: %w", spec.HostUUID, err)
	}
	v, err := a.vmReg.create(spec)
	if err == nil {
		a.bus.Publish(PlatformEvent{
			Kind:        "vm.registered",
			Subject:     v.UUID,
			ProjectUUID: v.ProjectUUID,
			Meta:        map[string]string{"name": v.Name, "host_uuid": v.HostUUID},
		})
	}
	return v, err
}

// SetVMState transitions the VM. Validates target state +
// publishes a `vm.state_changed` event.
func (a *Adapter) SetVMState(uuid string, state VMState) error {
	if a.vmReg == nil {
		return fmt.Errorf("vm registry not initialised")
	}
	prev, _ := a.vmReg.lookupByUUID(uuid)
	if err := a.vmReg.setState(uuid, state); err != nil {
		return err
	}
	a.bus.Publish(PlatformEvent{
		Kind:        "vm.state_changed",
		Subject:     uuid,
		ProjectUUID: prev.ProjectUUID,
		Meta:        map[string]string{"old_state": string(prev.State), "new_state": string(state)},
	})
	return nil
}

// MigrateVM flips the host_uuid. The actual data move is the
// caller's job; this just records the new placement so future
// dispatch routes correctly.
func (a *Adapter) MigrateVM(uuid, newHostUUID string) error {
	if a.vmReg == nil {
		return fmt.Errorf("vm registry not initialised")
	}
	if _, err := a.HostHandleOn(newHostUUID); err != nil {
		return fmt.Errorf("destination host %q: %w", newHostUUID, err)
	}
	prev, _ := a.vmReg.lookupByUUID(uuid)
	if err := a.vmReg.setHost(uuid, newHostUUID); err != nil {
		return err
	}
	a.bus.Publish(PlatformEvent{
		Kind:        "vm.migrated",
		Subject:     uuid,
		ProjectUUID: prev.ProjectUUID,
		Meta:        map[string]string{"old_host": prev.HostUUID, "new_host": newHostUUID},
	})
	return nil
}

// RenameVMInventory changes the VM's name within its project.
// Distinct from RenameProject / RenameNetwork etc. by the
// "Inventory" suffix to avoid clashing with future
// RenameVM(uuid, name) integration with the on-disk vmDir
// rename that the existing flows imply.
func (a *Adapter) RenameVMInventory(uuid, newName string) error {
	if a.vmReg == nil {
		return fmt.Errorf("vm registry not initialised")
	}
	prev, _ := a.vmReg.lookupByUUID(uuid)
	if err := a.vmReg.setName(uuid, newName); err != nil {
		return err
	}
	a.bus.Publish(PlatformEvent{
		Kind:        "vm.renamed",
		Subject:     uuid,
		ProjectUUID: prev.ProjectUUID,
		Meta:        map[string]string{"old_name": prev.Name, "new_name": newName},
	})
	return nil
}

// UnregisterVM drops the inventory entry. Callers typically
// invoke this AFTER HypervisorDriver.DeleteVM has removed the
// on-host state.
func (a *Adapter) UnregisterVM(uuid string) error {
	if a.vmReg == nil {
		return fmt.Errorf("vm registry not initialised")
	}
	prev, _ := a.vmReg.lookupByUUID(uuid)
	if err := a.vmReg.delete(uuid); err != nil {
		return err
	}
	// Drop the in-memory pod_id→CID binding so a recycled VM
	// name (same project + same name re-registered later) won't
	// inherit the previous incarnation's CID expectation.
	if prev.Name != "" {
		a.UnregisterPodCID(prev.Name)
	}
	a.bus.Publish(PlatformEvent{
		Kind:        "vm.unregistered",
		Subject:     uuid,
		ProjectUUID: prev.ProjectUUID,
		Meta:        map[string]string{"host_uuid": prev.HostUUID},
	})
	return nil
}

// initHosts loads the host registry via storageFactory.
func (a *Adapter) initHosts() {
	storage := a.storageFactory("hosts")
	if a.kvStorageFactory != nil {
		kv := a.kvStorageFactory("hosts")
		reg, err := loadHostRegistryKV(context.Background(), kv, storage)
		if err != nil {
			fmt.Fprintf(os.Stderr, "weft: load host registry (kv): %v\n", err)
			reg = &hostRegistry{
				storage: storage,
				kv:      kv,
				byUUID:  make(map[string]Host),
				nameIdx: make(map[string]string),
			}
		}
		a.hostReg = reg
		return
	}
	reg, err := loadHostRegistry(context.Background(), storage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "weft: load host registry: %v\n", err)
		reg = &hostRegistry{
			storage: storage,
			byUUID:  make(map[string]Host),
			nameIdx: make(map[string]string),
		}
	}
	a.hostReg = reg
}

// WatchHostRegistry mirrors WatchVMRegistry for host inventory : KV
// mode consumes per-record events into applyKVEvent. No-op on blob
// backends.
func (a *Adapter) WatchHostRegistry(ctx context.Context) {
	if a.hostReg == nil || a.hostReg.kv == nil {
		return
	}
	ch := a.hostReg.kv.WatchKeys(ctx)
	go func() {
		for ev := range ch {
			a.hostReg.applyKVEvent(ev)
			// V0.1.7 : no host.registry_reloaded publish — same
			// rationale as the vm.registry_reloaded removal. See
			// WatchVMRegistry's comment.
		}
	}()
}

// ── Host registry surface ────────────────────────────────────────

// Hosts returns every registered host across the cluster, sorted
// by (AZ, Hostname).
func (a *Adapter) Hosts() []Host {
	if a.hostReg == nil {
		return nil
	}
	return a.hostReg.list()
}

// HostByUUID resolves a UUID to its Host entry.
func (a *Adapter) HostByUUID(uuid string) (Host, bool) {
	if a.hostReg == nil {
		return Host{}, false
	}
	return a.hostReg.lookupByUUID(uuid)
}

// HostByHostname resolves a hostname to its Host entry.
// Hostnames are unique cluster-wide.
func (a *Adapter) HostByHostname(hostname string) (Host, bool) {
	if a.hostReg == nil {
		return Host{}, false
	}
	return a.hostReg.lookupByHostname(hostname)
}

// HostsInAZ returns every host registered in the given AZ.
func (a *Adapter) HostsInAZ(az string) []Host {
	if a.hostReg == nil {
		return nil
	}
	return a.hostReg.listByAZ(az)
}

// RegisterHost adds a host to the cluster. Called by weft-agent
// on startup (or by an admin CLI for static topologies).
func (a *Adapter) RegisterHost(spec RegisterHostSpec) (Host, error) {
	if a.hostReg == nil {
		return Host{}, fmt.Errorf("host registry not initialised")
	}
	h, err := a.hostReg.register(spec)
	if err == nil {
		a.bus.Publish(PlatformEvent{
			Kind:    "host.registered",
			Subject: h.UUID,
			Meta: map[string]string{
				"hostname":   h.Hostname,
				"az":         h.AZ,
				"hypervisor": h.Hypervisor,
			},
		})
	}
	return h, err
}

// HeartbeatHost updates the host's LastSeenAt + flips Down → Active.
// Called by weft-agent on a timer (typical 30s).
func (a *Adapter) HeartbeatHost(uuid string) error {
	if a.hostReg == nil {
		return fmt.Errorf("host registry not initialised")
	}
	// Heartbeats are noisy; do NOT publish on every one. The
	// state-transition cases (Down → Active) are surfaced via
	// SetHostState which DOES publish.
	prev, _ := a.hostReg.lookupByUUID(uuid)
	if err := a.hostReg.heartbeat(uuid); err != nil {
		return err
	}
	if prev.State == HostStateDown {
		a.bus.Publish(PlatformEvent{
			Kind:    "host.state_changed",
			Subject: uuid,
			Meta:    map[string]string{"old_state": "down", "new_state": "active"},
		})
	}
	return nil
}

// SetHostState explicitly transitions the host's state. Used by
// the operator CLI (drain) and the TTL sweeper (mark down).
func (a *Adapter) SetHostState(uuid string, state HostState) error {
	if a.hostReg == nil {
		return fmt.Errorf("host registry not initialised")
	}
	prev, _ := a.hostReg.lookupByUUID(uuid)
	if err := a.hostReg.setState(uuid, state); err != nil {
		return err
	}
	a.bus.Publish(PlatformEvent{
		Kind:    "host.state_changed",
		Subject: uuid,
		Meta:    map[string]string{"old_state": string(prev.State), "new_state": string(state)},
	})
	return nil
}

// SetHostProperties replaces the host's property map.
func (a *Adapter) SetHostProperties(uuid string, properties map[string]string) error {
	if a.hostReg == nil {
		return fmt.Errorf("host registry not initialised")
	}
	if err := a.hostReg.setProperties(uuid, properties); err != nil {
		return err
	}
	a.bus.Publish(PlatformEvent{
		Kind:    "host.properties_updated",
		Subject: uuid,
		Meta:    map[string]string{"property_count": strconv.Itoa(len(properties))},
	})
	return nil
}

// SetVMProperties replaces a VM's property set atomically. V0.1.8 :
// drives SchedulingRule property-based selectors + reserved-key system
// gates (deployment.type=ci|ha, etc.). Returns the updated VM record.
// projectUUID can be the project display name or UUID ;
// ResolveProjectUUID handles both.
func (a *Adapter) SetVMProperties(projectUUID, name string, properties map[string]string) (VM, error) {
	if a.vmReg == nil {
		return VM{}, fmt.Errorf("vm registry not initialised")
	}
	puuid := a.ResolveProjectUUID(projectUUID)
	v, ok := a.vmReg.lookupByName(puuid, name)
	if !ok {
		return VM{}, fmt.Errorf("vm %q not found in project %s", name, puuid)
	}
	if err := a.vmReg.setProperties(v.UUID, properties); err != nil {
		return VM{}, err
	}
	// Reload the record to surface the post-set properties.
	updated, _ := a.vmReg.lookupByUUID(v.UUID)
	a.bus.Publish(PlatformEvent{
		Kind:        "vm.properties_updated",
		Subject:     updated.UUID,
		ProjectUUID: updated.ProjectUUID,
		Meta:        map[string]string{"property_count": strconv.Itoa(len(properties))},
	})
	return updated, nil
}

// SetHostCordoned toggles the per-host cordon flag. Cordoned
// hosts stay Active (existing VMs keep running) but the scheduler
// drops them from candidate sets for new placements. Implements
// the upgrade-runbook primitive previously documented as proposed
// (see docs/operations/upgrade.md).
//
// Idempotent — calling with the current value is a no-op + nil.
// Emits a `host.cordoned` / `host.uncordoned` PlatformEvent on
// actual transitions so dashboards + the audit log can pick up the
// change.
func (a *Adapter) SetHostCordoned(uuid string, cordoned bool) error {
	if a.hostReg == nil {
		return fmt.Errorf("host registry not initialised")
	}
	prev, _ := a.hostReg.lookupByUUID(uuid)
	if err := a.hostReg.setCordoned(uuid, cordoned); err != nil {
		return err
	}
	if prev.Cordoned == cordoned {
		// No-op transition (already in requested state) — skip the
		// event so dashboards don't see redundant flapping when an
		// operator re-runs `weft host cordon` on the same host.
		return nil
	}
	kind := "host.uncordoned"
	if cordoned {
		kind = "host.cordoned"
	}
	a.bus.Publish(PlatformEvent{
		Kind:    kind,
		Subject: uuid,
		Meta:    map[string]string{"hostname": prev.Hostname},
	})
	return nil
}

// DeleteHost removes a host. Refuses when still active (drain first).
func (a *Adapter) DeleteHost(uuid string) error {
	if a.hostReg == nil {
		return fmt.Errorf("host registry not initialised")
	}
	if err := a.hostReg.delete(uuid); err != nil {
		return err
	}
	a.bus.Publish(PlatformEvent{
		Kind:    "host.deleted",
		Subject: uuid,
	})
	return nil
}

// ── Port registry surface ────────────────────────────────────────

// PortByUUID resolves a UUID to its Port entry.
func (a *Adapter) PortByUUID(uuid string) (Port, bool) {
	if a.portReg == nil {
		return Port{}, false
	}
	return a.portReg.lookupByUUID(uuid)
}

// ListPortsForVM returns every port attached to the given VM.
func (a *Adapter) ListPortsForVM(vmUUID string) []Port {
	if a.portReg == nil {
		return nil
	}
	return a.portReg.listForVM(vmUUID)
}

// ListPortsForNetwork returns every port on the given network —
// this is the peer set for mesh-config rendering.
func (a *Adapter) ListPortsForNetwork(networkUUID string) []Port {
	if a.portReg == nil {
		return nil
	}
	return a.portReg.listForNetwork(networkUUID)
}

// CreatePort registers a new VM ↔ Network attachment. Enforces:
//
//   - Network exists.
//   - Network and port belong to the same project.
//   - IP is inside Network.CIDR (delegates to net.ParseCIDR).
//   - Mesh-only fields (WireguardPubKey, MeshEndpoint) are refused
//     when Network.Type != mesh; required (pubkey) when it is.
//   - Each per-port SG exists, is in the same project, and is
//     deduplicated.
func (a *Adapter) CreatePort(spec CreatePortSpec) (Port, error) {
	if a.portReg == nil {
		return Port{}, fmt.Errorf("port registry not initialised")
	}
	if a.networkReg == nil {
		return Port{}, fmt.Errorf("network registry not initialised")
	}
	n, ok := a.networkReg.lookupByUUID(spec.NetworkUUID)
	if !ok {
		return Port{}, fmt.Errorf("network %q not found", spec.NetworkUUID)
	}
	if n.ProjectUUID != spec.ProjectUUID {
		return Port{}, fmt.Errorf("network %q belongs to project %s, not %s", spec.NetworkUUID, n.ProjectUUID, spec.ProjectUUID)
	}
	// IPAM : when spec.IP is empty, auto-allocate the next free
	// address from the network's CIDR, skipping every IP already
	// taken by another port on the same network + the network's
	// gateway. The caller (typically the operator CLI) omits IP
	// for "give me one" semantics ; passing an explicit IP keeps
	// the validation path below.
	if spec.IP == "" {
		excluded := make([]string, 0, 8)
		if n.Gateway != "" {
			excluded = append(excluded, n.Gateway)
		}
		for _, p := range a.portReg.listForNetwork(spec.NetworkUUID) {
			if p.IP != "" {
				excluded = append(excluded, p.IP)
			}
		}
		picked, err := PickFreeAddress(n.CIDR, excluded)
		if err != nil {
			return Port{}, fmt.Errorf("auto-allocate IP on network %q: %w", spec.NetworkUUID, err)
		}
		spec.IP = picked
	}
	if _, cidr, err := net.ParseCIDR(n.CIDR); err == nil {
		ip := net.ParseIP(spec.IP)
		if ip == nil {
			return Port{}, fmt.Errorf("ip %q: invalid", spec.IP)
		}
		if !cidr.Contains(ip) {
			return Port{}, fmt.Errorf("ip %s is outside network cidr %s", spec.IP, n.CIDR)
		}
	}
	// Mesh-field gating.
	if n.Type == NetworkTypeMesh {
		if spec.WireguardPubKey == "" {
			return Port{}, fmt.Errorf("wireguard_pub_key is required on mesh-type networks")
		}
	} else {
		if spec.WireguardPubKey != "" {
			return Port{}, fmt.Errorf("wireguard_pub_key is only valid on mesh-type networks (network type = %s)", n.Type)
		}
		if spec.MeshEndpoint != "" {
			return Port{}, fmt.Errorf("mesh_endpoint is only valid on mesh-type networks (network type = %s)", n.Type)
		}
	}
	// Per-port SG validation: each must exist, must belong to the
	// same project as the network, and the list must be dup-free.
	if err := a.validatePortSecurityGroups(spec.SecurityGroups, n.ProjectUUID); err != nil {
		return Port{}, err
	}
	p, err := a.portReg.create(spec)
	if err == nil {
		a.bus.Publish(PlatformEvent{
			Kind:        "port.created",
			Subject:     p.UUID,
			ProjectUUID: p.ProjectUUID,
			Meta: map[string]string{
				"vm_uuid":      p.VMUUID,
				"network_uuid": p.NetworkUUID,
				"ip":           p.IP,
			},
		})
	}
	return p, err
}

// validatePortSecurityGroups is the shared validator used by
// CreatePort and SetPortSecurityGroups. Returns nil for the empty
// list (inherits Network.DefaultSecurityGroups at boot).
func (a *Adapter) validatePortSecurityGroups(sgUUIDs []string, projectUUID string) error {
	if a.sgReg == nil && len(sgUUIDs) > 0 {
		return fmt.Errorf("security-group registry not initialised")
	}
	seen := make(map[string]struct{}, len(sgUUIDs))
	for _, sg := range sgUUIDs {
		if _, dup := seen[sg]; dup {
			return fmt.Errorf("security-group %q appears twice in the list", sg)
		}
		seen[sg] = struct{}{}
		g, ok := a.sgReg.lookupByUUID(sg)
		if !ok {
			return fmt.Errorf("security-group %q not found", sg)
		}
		if g.ProjectUUID != projectUUID {
			return fmt.Errorf("security-group %q belongs to project %s, not %s — cross-project reference refused", sg, g.ProjectUUID, projectUUID)
		}
	}
	return nil
}

// SetPortSecurityGroups replaces the per-port SG override list.
// Same validation as CreatePort: existence, project match, dedup.
func (a *Adapter) SetPortSecurityGroups(uuid string, sgUUIDs []string) error {
	if a.portReg == nil {
		return fmt.Errorf("port registry not initialised")
	}
	p, ok := a.portReg.lookupByUUID(uuid)
	if !ok {
		return fmt.Errorf("port %q not found", uuid)
	}
	if err := a.validatePortSecurityGroups(sgUUIDs, p.ProjectUUID); err != nil {
		return err
	}
	if err := a.portReg.setSecurityGroups(uuid, sgUUIDs); err != nil {
		return err
	}
	a.bus.Publish(PlatformEvent{
		Kind:        "port.security_groups_updated",
		Subject:     uuid,
		ProjectUUID: p.ProjectUUID,
		Meta:        map[string]string{"sg_count": strconv.Itoa(len(sgUUIDs))},
	})
	return nil
}

// SetPortWireguardPubKey rotates the per-port WireGuard pubkey.
// Refuses when the port's network isn't mesh-type.
func (a *Adapter) SetPortWireguardPubKey(uuid, pubkey string) error {
	if a.portReg == nil {
		return fmt.Errorf("port registry not initialised")
	}
	p, ok := a.portReg.lookupByUUID(uuid)
	if !ok {
		return fmt.Errorf("port %q not found", uuid)
	}
	if pubkey == "" {
		return fmt.Errorf("empty wireguard_pub_key")
	}
	n, ok := a.networkReg.lookupByUUID(p.NetworkUUID)
	if !ok {
		return fmt.Errorf("network %q (referenced by port) not found", p.NetworkUUID)
	}
	if n.Type != NetworkTypeMesh {
		return fmt.Errorf("port %q is on a %s network, not mesh — wireguard key not applicable", uuid, n.Type)
	}
	if err := a.portReg.setWireguardPubKey(uuid, pubkey); err != nil {
		return err
	}
	a.bus.Publish(PlatformEvent{
		Kind:        "port.wireguard_key_rotated",
		Subject:     uuid,
		ProjectUUID: p.ProjectUUID,
		// network.peers_changed is what the in-VM agent listens
		// for; emit both so consumers can subscribe at the layer
		// that makes sense for them.
	})
	a.bus.Publish(PlatformEvent{
		Kind:        "network.peers_changed",
		Subject:     p.NetworkUUID,
		ProjectUUID: p.ProjectUUID,
		Meta:        map[string]string{"port_uuid": uuid},
	})
	return nil
}

// DeletePort drops a port from the registry. Triggers a
// network.peers_changed event so mesh peers update their wg.conf.
func (a *Adapter) DeletePort(uuid string) error {
	if a.portReg == nil {
		return fmt.Errorf("port registry not initialised")
	}
	p, ok := a.portReg.lookupByUUID(uuid)
	if !ok {
		return fmt.Errorf("port %q not found", uuid)
	}
	if err := a.portReg.delete(uuid); err != nil {
		return err
	}
	a.bus.Publish(PlatformEvent{
		Kind:        "port.deleted",
		Subject:     uuid,
		ProjectUUID: p.ProjectUUID,
		Meta:        map[string]string{"vm_uuid": p.VMUUID, "network_uuid": p.NetworkUUID},
	})
	a.bus.Publish(PlatformEvent{
		Kind:        "network.peers_changed",
		Subject:     p.NetworkUUID,
		ProjectUUID: p.ProjectUUID,
	})
	return nil
}

// ── Security-group registry surface ──────────────────────────────

// SecurityGroups returns every registered group across projects.
func (a *Adapter) SecurityGroups() []SecurityGroup {
	if a.sgReg == nil {
		return nil
	}
	return a.sgReg.list()
}

// SecurityGroupByUUID resolves a UUID to its SecurityGroup entry.
func (a *Adapter) SecurityGroupByUUID(uuid string) (SecurityGroup, bool) {
	if a.sgReg == nil {
		return SecurityGroup{}, false
	}
	return a.sgReg.lookupByUUID(uuid)
}

// SecurityGroupByName resolves (projectUUID, name) to a SecurityGroup.
func (a *Adapter) SecurityGroupByName(projectUUID, name string) (SecurityGroup, bool) {
	if a.sgReg == nil {
		return SecurityGroup{}, false
	}
	return a.sgReg.lookupByName(projectUUID, name)
}

// ListSecurityGroupsForProject returns every group in the project.
func (a *Adapter) ListSecurityGroupsForProject(projectUUID string) []SecurityGroup {
	if a.sgReg == nil {
		return nil
	}
	return a.sgReg.listForProject(projectUUID)
}

// CreateSecurityGroup registers a new group. Rules are validated
// before any write.
func (a *Adapter) CreateSecurityGroup(spec CreateSecurityGroupSpec) (SecurityGroup, error) {
	if a.sgReg == nil {
		return SecurityGroup{}, fmt.Errorf("security-group registry not initialised")
	}
	g, err := a.sgReg.create(spec)
	if err == nil {
		a.bus.Publish(PlatformEvent{
			Kind:        "security_group.created",
			Subject:     g.UUID,
			ProjectUUID: g.ProjectUUID,
			Meta:        map[string]string{"name": g.Name},
		})
	}
	return g, err
}

// RenameSecurityGroup updates the display name within the project.
func (a *Adapter) RenameSecurityGroup(uuid, newName string) error {
	if a.sgReg == nil {
		return fmt.Errorf("security-group registry not initialised")
	}
	oldName, projectUUID := "", ""
	if g, ok := a.sgReg.lookupByUUID(uuid); ok {
		oldName = g.Name
		projectUUID = g.ProjectUUID
	}
	if err := a.sgReg.setName(uuid, newName); err != nil {
		return err
	}
	a.bus.Publish(PlatformEvent{
		Kind:        "security_group.renamed",
		Subject:     uuid,
		ProjectUUID: projectUUID,
		Meta:        map[string]string{"old_name": oldName, "new_name": newName},
	})
	return nil
}

// SetSecurityGroupDescription updates the description.
func (a *Adapter) SetSecurityGroupDescription(uuid, desc string) error {
	if a.sgReg == nil {
		return fmt.Errorf("security-group registry not initialised")
	}
	return a.sgReg.setDescription(uuid, desc)
}

// SetSecurityGroupRules atomically replaces the rule set on a
// group. All rules are validated first; nothing is written when
// any rule is invalid.
func (a *Adapter) SetSecurityGroupRules(uuid string, rules []SecurityRule) error {
	if a.sgReg == nil {
		return fmt.Errorf("security-group registry not initialised")
	}
	projectUUID := ""
	if g, ok := a.sgReg.lookupByUUID(uuid); ok {
		projectUUID = g.ProjectUUID
	}
	if err := a.sgReg.setRules(uuid, rules); err != nil {
		return err
	}
	a.bus.Publish(PlatformEvent{
		Kind:        "security_group.rules_updated",
		Subject:     uuid,
		ProjectUUID: projectUUID,
		Meta:        map[string]string{"rule_count": strconv.Itoa(len(rules))},
	})
	return nil
}

// DeleteSecurityGroup drops a group from the registry. Refuses
// when any network still lists the SG in its DefaultSecurityGroups —
// orphaned refs would silently disappear on next reload, leaving
// VMs unexpectedly less restricted. Operators must clear the
// network's default-SG list (or remove this SG from it) first.
func (a *Adapter) DeleteSecurityGroup(uuid string) error {
	if a.sgReg == nil {
		return fmt.Errorf("security-group registry not initialised")
	}
	projectUUID := ""
	if g, ok := a.sgReg.lookupByUUID(uuid); ok {
		projectUUID = g.ProjectUUID
	}
	// Cross-registry cascade checks.
	if a.networkReg != nil {
		if refs := a.networkReg.networksReferencingSecurityGroup(uuid); len(refs) > 0 {
			return fmt.Errorf("security-group %q still referenced by %d network(s): %v — clear the reference first", uuid, len(refs), refs)
		}
	}
	if a.portReg != nil {
		if refs := a.portReg.portsReferencingSecurityGroup(uuid); len(refs) > 0 {
			return fmt.Errorf("security-group %q still referenced by %d port(s): %v — clear the reference first", uuid, len(refs), refs)
		}
	}
	if err := a.sgReg.delete(uuid); err != nil {
		return err
	}
	a.bus.Publish(PlatformEvent{
		Kind:        "security_group.deleted",
		Subject:     uuid,
		ProjectUUID: projectUUID,
	})
	return nil
}

// initProjects loads the on-disk project registry (or starts an
// empty one when the file is missing). Must run before any code
// that resolves projects.
//
// Two backends today : the legacy blob path (file storage, in-memory
// tests, pre-V0.1.4 etcd clusters that still hold a `projects` key)
// and the V0.1.4 per-record KV path (etcd, /weft/projects/<uuid>
// keys, surgical Put+Watch on every mutation). KV is preferred when
// the operator wired the etcd backend ; it migrates the legacy blob
// on first run.
func (a *Adapter) initProjects() {
	if err := os.MkdirAll(a.vmsDir(), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "weft: mkdir vmsDir: %v\n", err)
	}
	// Storage backend comes from the adapter's storageFactory, set
	// at construction time (NewWithStorage). Dev path: file-backed
	// under <vmsDir>/.projects.hcl. Prod path: EtcdStorage —
	// see etcd_control_plane.md. The registry consumer code
	// doesn't change either way.
	storage := a.storageFactory("projects")
	if a.kvStorageFactory != nil {
		kv := a.kvStorageFactory("projects")
		reg, err := loadProjectRegistryKV(context.Background(), kv, storage)
		if err != nil {
			fmt.Fprintf(os.Stderr, "weft: load project registry (kv): %v\n", err)
			reg = &projectRegistry{
				storage: storage,
				kv:      kv,
				byUUID:  make(map[string]Project),
				nameIdx: make(map[string]string),
			}
		}
		a.projects = reg
		return
	}
	reg, err := loadProjectRegistry(context.Background(), storage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "weft: load project registry: %v\n", err)
		// Same Storage but empty in-memory state — subsequent
		// mutations will still try to Save (and likely succeed if
		// the underlying I/O blip was transient).
		reg = &projectRegistry{
			storage: storage,
			byUUID:  make(map[string]Project),
			nameIdx: make(map[string]string),
		}
	}
	a.projects = reg
}

// WatchProjectRegistry starts a background goroutine that keeps the
// in-memory project registry in sync with the persistent layer.
// Mirrors WatchVMRegistry : KV mode consumes WatchKeys → applyKVEvent
// surgically ; blob mode is a no-op (projectRegistry doesn't
// currently expose a reloadFromStorage). Returns immediately ; the
// goroutine exits when ctx is cancelled.
func (a *Adapter) WatchProjectRegistry(ctx context.Context) {
	if a.projects == nil {
		return
	}
	if a.projects.kv != nil {
		ch := a.projects.kv.WatchKeys(ctx)
		go func() {
			for ev := range ch {
				a.projects.applyKVEvent(ev)
				// V0.1.7 : no project.registry_reloaded publish.
			}
		}()
		return
	}
	// blob mode : no projectRegistry.reloadFromStorage today, so
	// remote PUT/DELETE events on the legacy `projects` blob don't
	// propagate. Single-DC dev setups stay correct ; multi-DC blob
	// deploys are explicitly off the V0.1.4 supported path and
	// should opt into KV mode.
}

// migrateLegacyLayout walks <vmsDir>/* looking for entries that
// look like a real vmDir (i.e. have config.json and machine-id.bin
// at top level — the signature of pre-project-namespacing VMs) and
// moves each one to <vmsDir>/<defaultProjectUUID>/<name>/.
// Idempotent.
func (a *Adapter) migrateLegacyLayout() {
	base := a.vmsDir()
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(base, e.Name())
		if _, err := os.Stat(filepath.Join(dir, "machine-id.bin")); err != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
			continue
		}
		dst := filepath.Join(base, a.DefaultProjectUUID(), e.Name())
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			continue
		}
		if err := os.Rename(dir, dst); err == nil {
			fmt.Fprintf(os.Stderr, "weft: migrated flat-layout vm %q -> %s/ (default project)\n", e.Name(), a.DefaultProjectUUID())
		}
	}
}

// migrateNamedProjectDirs handles the second-generation legacy
// layout from the previous multitenancy step where vmDirs lived at
// <vmsDir>/<project-name>/<vm>/. It looks up (or registers) each
// such display name in the project registry and renames the
// directory in-place to use the canonical UUID instead. Idempotent:
// directories whose name is already a UUID are left alone.
func (a *Adapter) migrateNamedProjectDirs() {
	base := a.vmsDir()
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || isUUID(e.Name()) {
			continue
		}
		// Skip the registry file itself.
		if e.Name() == registryFileName {
			continue
		}
		p, _, err := a.projects.getOrCreate(e.Name())
		if err != nil {
			continue
		}
		src := filepath.Join(base, e.Name())
		dst := filepath.Join(base, p.UUID)
		// If dst already exists (re-run after partial migration),
		// merge: move each child VM individually.
		if _, err := os.Stat(dst); err == nil {
			children, _ := os.ReadDir(src)
			for _, c := range children {
				_ = os.Rename(filepath.Join(src, c.Name()), filepath.Join(dst, c.Name()))
			}
			_ = os.Remove(src)
			fmt.Fprintf(os.Stderr, "weft: merged named project %q (%s)\n", e.Name(), p.UUID)
			continue
		}
		if err := os.Rename(src, dst); err == nil {
			fmt.Fprintf(os.Stderr, "weft: migrated named project %q -> uuid %s\n", e.Name(), p.UUID)
		}
	}
}

// Name returns "vz".
func (a *Adapter) Name() string { return "vz" }

// Available reports whether Apple Virtualization.framework is usable.
// The build tag (darwin && cgo) already gates this to macOS + CGO builds.
func (a *Adapter) Available() bool { return true }

// cacheDir returns the path to the OCI image cache directory.
func (a *Adapter) cacheDir() string {
	if a.cachePath != "" {
		return a.cachePath
	}
	return filepath.Join(a.stateDir, "cache")
}

// vmsDir returns the base directory that holds all VM subdirectories.
func (a *Adapter) vmsDir() string {
	if a.vmsPath != "" {
		return a.vmsPath
	}
	return filepath.Join(a.stateDir, "vz")
}

// RegistryStorage returns the Storage backing the named registry
// blob (projects, users, flavors, …). Exported so cmd/weft can
// construct the FlavorRegistry alongside the adapter's own
// registries — flavors don't need to live on the adapter itself
// (no per-call ACL like projects), but they DO need the same
// Storage configuration (file backend in dev, etcd in prod).
func (a *Adapter) RegistryStorage(name string) Storage {
	if a.storageFactory != nil {
		return a.storageFactory(name)
	}
	return NewFileStorageInDir(a.vmsDir(), name)
}

// DefaultProjectUUID returns the UUID of the auto-created project
// for the OS user weft runs as. First call lazily creates it; later
// calls return the cached value. Phase 2 will swap this for the
// authenticated caller's identity.
func (a *Adapter) DefaultProjectUUID() string {
	if a.defaultProjUUID != "" {
		return a.defaultProjUUID
	}
	if a.projects == nil {
		// Pre-init guard: callers that hit this before
		// initProjects() ran (only the listing test paths) get the
		// empty string back. Once the adapter is fully constructed
		// (via New), this branch is never taken.
		return ""
	}
	p, _, err := a.projects.getOrCreate(defaultProjectName())
	if err != nil {
		// Same defensive fallback as DefaultProjectName — never
		// crash the daemon on a fresh-install race.
		return ""
	}
	a.defaultProjUUID = p.UUID
	return p.UUID
}

// ResolveProjectUUID turns a caller-supplied `project` (which can be
// empty, a display name, or a literal UUID) into the canonical
// project UUID. Empty input resolves to DefaultProjectUUID and
// auto-creates the default project on first use. Display names
// are auto-created when missing (Phase 2 will gate this behind a
// create-permission check).
func (a *Adapter) ResolveProjectUUID(project string) string {
	if project == "" {
		return a.DefaultProjectUUID()
	}
	if isUUID(project) {
		return project
	}
	if a.projects == nil {
		return project // pre-init fallback; treat as opaque
	}
	p, _, err := a.projects.getOrCreate(project)
	if err != nil {
		return project
	}
	return p.UUID
}

// ProjectByName returns the registry entry matching a display name.
// Returns (zero, false) when no such project exists — used by
// read-only paths that must not auto-create (e.g. status/logs).
func (a *Adapter) ProjectByName(name string) (Project, bool) {
	if a.projects == nil {
		return Project{}, false
	}
	uuid, ok := a.projects.lookupByName(name)
	if !ok {
		return Project{}, false
	}
	return a.projects.lookupByUUID(uuid)
}

// ProjectByUUID is the inverse lookup. Returns (zero, false) when
// the UUID is unknown.
func (a *Adapter) ProjectByUUID(uuid string) (Project, bool) {
	if a.projects == nil {
		return Project{}, false
	}
	return a.projects.lookupByUUID(uuid)
}

// Projects returns a sorted copy of every registered project. Used
// by the ListProjects RPC and any future inventory tool.
func (a *Adapter) Projects() []Project {
	if a.projects == nil {
		return nil
	}
	return a.projects.list()
}

// ProjectsByTenant returns every project bound to the given tenant
// UUID. Empty tenantUUID returns every UNTENANTED project. Used by
// quota aggregation (`siblings_total` on GetProjectQuota +
// `allocated` on the future tenant-level quota RPC).
func (a *Adapter) ProjectsByTenant(tenantUUID string) []Project {
	if a.projects == nil {
		return nil
	}
	return a.projects.listByTenant(tenantUUID)
}

// SetProjectTenant binds (or unbinds, when tenantUUID is empty) the
// project to a parent tenant. Idempotent ; unknown project UUIDs
// return an error so operators don't silently mistype. Doesn't
// validate that the tenant exists — operators can stage the binding
// before the tenant lands, and DeleteTenant's future cascade refusal
// will surface a missing parent rather than this call.
func (a *Adapter) SetProjectTenant(projectUUID, tenantUUID string) error {
	if a.projects == nil {
		return fmt.Errorf("project registry not initialised")
	}
	return a.projects.setTenant(projectUUID, tenantUUID)
}

// CreateProject explicitly registers a project (no auto-create at
// VM-launch time). Returns the canonical Project and a boolean that
// reports whether the entry is new (true) or already existed (false).
func (a *Adapter) CreateProject(name string) (Project, bool, error) {
	if a.projects == nil {
		return Project{}, false, fmt.Errorf("project registry not initialised")
	}
	p, created, err := a.projects.getOrCreate(name)
	if err == nil && created {
		a.bus.Publish(PlatformEvent{
			Kind:        "project.created",
			Subject:     p.UUID,
			ProjectUUID: p.UUID,
			Meta:        map[string]string{"name": p.Name},
		})
	}
	return p, created, err
}

// RenameProject updates a project's display name. The UUID — and
// therefore every VM path under it — stays unchanged. This is the
// reason we carry a UUID at all.
func (a *Adapter) RenameProject(uuid, newName string) error {
	if a.projects == nil {
		return fmt.Errorf("project registry not initialised")
	}
	// Capture the old name for the event payload — handy when
	// consumers want to redraw their UI on a rename.
	oldName := ""
	if p, ok := a.projects.lookupByUUID(uuid); ok {
		oldName = p.Name
	}
	if err := a.projects.rename(uuid, newName); err != nil {
		return err
	}
	a.bus.Publish(PlatformEvent{
		Kind:        "project.renamed",
		Subject:     uuid,
		ProjectUUID: uuid,
		Meta:        map[string]string{"old_name": oldName, "new_name": newName},
	})
	return nil
}

// DeleteProject drops a project from the registry. Refuses when
// the project still contains VMs on disk to avoid orphaning them.
func (a *Adapter) DeleteProject(uuid string) error {
	if a.projects == nil {
		return fmt.Errorf("project registry not initialised")
	}
	dir := filepath.Join(a.vmsDir(), uuid)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() {
			return fmt.Errorf("project %s still has VMs (e.g. %q) — delete them first", uuid, e.Name())
		}
	}
	_ = os.RemoveAll(dir) // empty by the check above
	if err := a.projects.delete(uuid); err != nil {
		return err
	}
	a.bus.Publish(PlatformEvent{
		Kind:        "project.deleted",
		Subject:     uuid,
		ProjectUUID: uuid,
	})
	// Drop the project's user-block from the rendered nats.conf so
	// its pubkey stops being authorized. Auto-render is a no-op
	// when the path isn't configured (operator-driven mode).
	_ = a.autoRenderNATSAuthorization()
	return nil
}

// AddProjectMember appends a user-UUID to the project's Members
// list. Doubles the dex `groups`-claim path for granting project
// access (callerOwnsProject checks both). Idempotent.
func (a *Adapter) AddProjectMember(projectUUID, userUUID string) error {
	if a.projects == nil {
		return fmt.Errorf("project registry not initialised")
	}
	if err := a.projects.addMember(projectUUID, userUUID); err != nil {
		return err
	}
	a.bus.Publish(PlatformEvent{
		Kind:        "project.member_added",
		Subject:     projectUUID,
		ProjectUUID: projectUUID,
		Meta:        map[string]string{"user_uuid": userUUID},
	})
	return nil
}

// RemoveProjectMember removes a user from the Members list.
// Idempotent. Note: this does NOT revoke a `project:<uuid>` group
// claim — that's a dex concern.
func (a *Adapter) RemoveProjectMember(projectUUID, userUUID string) error {
	if a.projects == nil {
		return fmt.Errorf("project registry not initialised")
	}
	if err := a.projects.removeMember(projectUUID, userUUID); err != nil {
		return err
	}
	a.bus.Publish(PlatformEvent{
		Kind:        "project.member_removed",
		Subject:     projectUUID,
		ProjectUUID: projectUUID,
		Meta:        map[string]string{"user_uuid": userUUID},
	})
	return nil
}

// ProjectMembers returns a defensive copy of the project's member
// list, or (nil, false) when the project doesn't exist.
func (a *Adapter) ProjectMembers(projectUUID string) ([]string, bool) {
	if a.projects == nil {
		return nil, false
	}
	return a.projects.members(projectUUID)
}

// ── Tenant registry surface ──────────────────────────────────────
//
// Tenants live above projects in the multi-tenant hierarchy. The
// registry is JSON-backed via Storage (same pattern as AZs). Mirrors
// the CRUD shape of projects but with admins / members at the tenant
// level rather than members on the project ; the proto distinguishes
// them so the CLI surfaces both verbs separately.

// initTenants loads the tenant registry via storageFactory. Failure
// to load downgrades to an empty in-memory registry — same resilience
// contract as initProjects / initInventory.
func (a *Adapter) initTenants() {
	storage := a.storageFactory(tenantRegistryFileName)
	reg, err := loadTenantRegistry(context.Background(), storage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "weft: load tenant registry: %v\n", err)
		reg = &tenantRegistry{
			storage: storage,
			byUUID:  make(map[string]Tenant),
			nameIdx: make(map[string]string),
		}
	}
	a.tenantReg = reg
}

// Tenants returns a snapshot of every registered tenant, sorted by
// name (matches the AZ list-by-code convention).
func (a *Adapter) Tenants() []Tenant {
	if a.tenantReg == nil {
		return nil
	}
	return a.tenantReg.list()
}

// TenantByUUID resolves a UUID.
func (a *Adapter) TenantByUUID(uuid string) (Tenant, bool) {
	if a.tenantReg == nil {
		return Tenant{}, false
	}
	return a.tenantReg.lookupByUUID(uuid)
}

// TenantByName resolves a display name. Used by the CLI to support
// `weft tenant rm <name>` without a separate ResolveTenantUUID
// round-trip (the CLI does its own resolution via ListTenants — this
// helper keeps the door open for future server-side lookups).
func (a *Adapter) TenantByName(name string) (Tenant, bool) {
	if a.tenantReg == nil {
		return Tenant{}, false
	}
	return a.tenantReg.lookupByName(name)
}

// CreateTenant registers a new tenant. Returns (row, created, err)
// where created=false means the name already existed (idempotent
// insert, mirrors CreateProject / CreateAZ).
func (a *Adapter) CreateTenant(name, domain string) (Tenant, bool, error) {
	if a.tenantReg == nil {
		return Tenant{}, false, fmt.Errorf("tenant registry not initialised")
	}
	t, created, err := a.tenantReg.create(name, domain)
	if err == nil && created {
		a.bus.Publish(PlatformEvent{
			Kind:    "tenant.created",
			Subject: t.UUID,
			Meta:    map[string]string{"name": t.Name, "domain": t.Domain},
		})
	}
	return t, created, err
}

// DeleteTenant drops a tenant. The blockedProjects count is always
// zero today because Project does not carry a TenantUUID column —
// the slot is preserved on the return so the matching gRPC response
// can carry it verbatim once project ↔ tenant linkage lands.
func (a *Adapter) DeleteTenant(uuid string) (int32, error) {
	if a.tenantReg == nil {
		return 0, fmt.Errorf("tenant registry not initialised")
	}
	// Future cascade : count projects whose TenantUUID matches uuid.
	// Today : projects don't reference tenants, so always 0.
	var blockedProjects int32 = 0
	got, err := a.tenantReg.delete(uuid, blockedProjects)
	if err != nil {
		if got > 0 {
			blockedProjects = got
		}
		return blockedProjects, err
	}
	a.bus.Publish(PlatformEvent{
		Kind:    "tenant.deleted",
		Subject: uuid,
	})
	return 0, nil
}

// AddTenantAdmin grants the tenant-admin role to the user identified
// by email. Idempotent.
func (a *Adapter) AddTenantAdmin(tenantUUID, email string) (Tenant, error) {
	if a.tenantReg == nil {
		return Tenant{}, fmt.Errorf("tenant registry not initialised")
	}
	t, err := a.tenantReg.addAdmin(tenantUUID, email)
	if err == nil {
		a.bus.Publish(PlatformEvent{
			Kind:    "tenant.admin_added",
			Subject: tenantUUID,
			Meta:    map[string]string{"email": email},
		})
	}
	return t, err
}

// RemoveTenantAdmin revokes the tenant-admin role. Idempotent.
func (a *Adapter) RemoveTenantAdmin(tenantUUID, email string) (Tenant, error) {
	if a.tenantReg == nil {
		return Tenant{}, fmt.Errorf("tenant registry not initialised")
	}
	t, err := a.tenantReg.removeAdmin(tenantUUID, email)
	if err == nil {
		a.bus.Publish(PlatformEvent{
			Kind:    "tenant.admin_removed",
			Subject: tenantUUID,
			Meta:    map[string]string{"email": email},
		})
	}
	return t, err
}

// AddTenantMember inserts or updates a tenant member (email → groups).
// Groups are merged into any existing entry for the same email.
func (a *Adapter) AddTenantMember(tenantUUID, email string, groups []string) (Tenant, error) {
	if a.tenantReg == nil {
		return Tenant{}, fmt.Errorf("tenant registry not initialised")
	}
	t, err := a.tenantReg.addMember(tenantUUID, email, groups)
	if err == nil {
		a.bus.Publish(PlatformEvent{
			Kind:    "tenant.member_added",
			Subject: tenantUUID,
			Meta:    map[string]string{"email": email},
		})
	}
	return t, err
}

// RemoveTenantMember drops a tenant member by email. Idempotent.
func (a *Adapter) RemoveTenantMember(tenantUUID, email string) (Tenant, error) {
	if a.tenantReg == nil {
		return Tenant{}, fmt.Errorf("tenant registry not initialised")
	}
	t, err := a.tenantReg.removeMember(tenantUUID, email)
	if err == nil {
		a.bus.Publish(PlatformEvent{
			Kind:    "tenant.member_removed",
			Subject: tenantUUID,
			Meta:    map[string]string{"email": email},
		})
	}
	return t, err
}

// ── User registry surface ────────────────────────────────────────

// Users returns every registered user, sorted by display name.
func (a *Adapter) Users() []User {
	if a.userReg == nil {
		return nil
	}
	return a.userReg.list()
}

// UserByUUID resolves a UUID to its User entry.
func (a *Adapter) UserByUUID(uuid string) (User, bool) {
	if a.userReg == nil {
		return User{}, false
	}
	return a.userReg.lookupByUUID(uuid)
}

// UserBySubject resolves the (issuer, subject) tuple OIDC tokens
// carry. Empty issuer / subject never matches.
func (a *Adapter) UserBySubject(issuer, subject string) (User, bool) {
	if a.userReg == nil {
		return User{}, false
	}
	return a.userReg.lookupBySubject(issuer, subject)
}

// RegisterUser creates or refreshes the registry entry for the
// authenticated Caller. Reports created=true on the very first
// sight of this (issuer, subject) pair.
func (a *Adapter) RegisterUser(c *Caller) (User, bool, error) {
	if a.userReg == nil {
		return User{}, false, fmt.Errorf("user registry not initialised")
	}
	u, created, err := a.userReg.getOrCreateFromCaller(c)
	if err == nil && created {
		a.bus.Publish(PlatformEvent{
			Kind:    "user.created",
			Subject: u.UUID,
			Meta:    map[string]string{"email": u.Email, "issuer": u.Issuer},
		})
	}
	return u, created, err
}

// SetUserDisplayName updates the operator-visible name. Immutable
// fields (UUID, Subject, Issuer) stay put.
func (a *Adapter) SetUserDisplayName(uuid, name string) error {
	if a.userReg == nil {
		return fmt.Errorf("user registry not initialised")
	}
	if err := a.userReg.setDisplayName(uuid, name); err != nil {
		return err
	}
	a.bus.Publish(PlatformEvent{
		Kind:    "user.renamed",
		Subject: uuid,
		Meta:    map[string]string{"new_name": name},
	})
	return nil
}

// DeleteUser drops a user from the registry. Caller must reassign
// any ownership (project, VM) the user held first.
func (a *Adapter) DeleteUser(uuid string) error {
	if a.userReg == nil {
		return fmt.Errorf("user registry not initialised")
	}
	if err := a.userReg.delete(uuid); err != nil {
		return err
	}
	a.bus.Publish(PlatformEvent{
		Kind:    "user.deleted",
		Subject: uuid,
	})
	return nil
}

// ── Network registry surface ─────────────────────────────────────

// Networks returns every registered network across all projects.
func (a *Adapter) Networks() []Network {
	if a.networkReg == nil {
		return nil
	}
	return a.networkReg.list()
}

// NetworkByUUID resolves a UUID to its Network entry.
func (a *Adapter) NetworkByUUID(uuid string) (Network, bool) {
	if a.networkReg == nil {
		return Network{}, false
	}
	return a.networkReg.lookupByUUID(uuid)
}

// NetworkByName resolves (projectUUID, name) to a Network. Names
// are scoped per project — cross-project collisions are valid.
func (a *Adapter) NetworkByName(projectUUID, name string) (Network, bool) {
	if a.networkReg == nil {
		return Network{}, false
	}
	return a.networkReg.lookupByName(projectUUID, name)
}

// ListNetworksForProject returns every network owned by the given
// project, sorted by name.
func (a *Adapter) ListNetworksForProject(projectUUID string) []Network {
	if a.networkReg == nil {
		return nil
	}
	return a.networkReg.listForProject(projectUUID)
}

// CreateNetwork registers a new network. Returns an error on name
// collision within the project, invalid CIDR / gateway, or unknown
// type.
func (a *Adapter) CreateNetwork(spec CreateNetworkSpec) (Network, error) {
	if a.networkReg == nil {
		return Network{}, fmt.Errorf("network registry not initialised")
	}
	n, err := a.networkReg.create(spec)
	if err == nil {
		a.bus.Publish(PlatformEvent{
			Kind:        "network.created",
			Subject:     n.UUID,
			ProjectUUID: n.ProjectUUID,
			Meta:        map[string]string{"name": n.Name, "cidr": n.CIDR, "type": string(n.Type)},
		})
	}
	return n, err
}

// RenameNetwork updates the network's display name within its
// project. The UUID and project binding stay put.
func (a *Adapter) RenameNetwork(uuid, newName string) error {
	if a.networkReg == nil {
		return fmt.Errorf("network registry not initialised")
	}
	oldName, projectUUID := "", ""
	if n, ok := a.networkReg.lookupByUUID(uuid); ok {
		oldName = n.Name
		projectUUID = n.ProjectUUID
	}
	if err := a.networkReg.setName(uuid, newName); err != nil {
		return err
	}
	a.bus.Publish(PlatformEvent{
		Kind:        "network.renamed",
		Subject:     uuid,
		ProjectUUID: projectUUID,
		Meta:        map[string]string{"old_name": oldName, "new_name": newName},
	})
	return nil
}

// SetNetworkDNS updates the DNS server list. Pass a nil / empty
// slice to clear.
func (a *Adapter) SetNetworkDNS(uuid string, servers []string) error {
	if a.networkReg == nil {
		return fmt.Errorf("network registry not initialised")
	}
	return a.networkReg.setDNSServers(uuid, servers)
}

// SetNetworkDefaultSecurityGroups replaces the network's
// default-SG list. Every UUID is validated against the SG
// registry: must exist + must belong to the same project as the
// network. Cross-project SG references are refused to preserve
// multi-tenant isolation. Pass nil / empty to clear.
func (a *Adapter) SetNetworkDefaultSecurityGroups(networkUUID string, sgUUIDs []string) error {
	if a.networkReg == nil {
		return fmt.Errorf("network registry not initialised")
	}
	if a.sgReg == nil {
		return fmt.Errorf("security-group registry not initialised")
	}
	n, ok := a.networkReg.lookupByUUID(networkUUID)
	if !ok {
		return fmt.Errorf("network %q not found", networkUUID)
	}
	// Validate every SG UUID before any write. Dedup as we go
	// since a duplicate would be silently kept by the registry —
	// catch the operator typo here instead.
	seen := make(map[string]struct{}, len(sgUUIDs))
	for _, sg := range sgUUIDs {
		if _, dup := seen[sg]; dup {
			return fmt.Errorf("security-group %q appears twice in the list", sg)
		}
		seen[sg] = struct{}{}
		g, ok := a.sgReg.lookupByUUID(sg)
		if !ok {
			return fmt.Errorf("security-group %q not found", sg)
		}
		if g.ProjectUUID != n.ProjectUUID {
			return fmt.Errorf("security-group %q belongs to project %s, not %s — cross-project reference refused", sg, g.ProjectUUID, n.ProjectUUID)
		}
	}
	if err := a.networkReg.setDefaultSecurityGroups(networkUUID, sgUUIDs); err != nil {
		return err
	}
	a.bus.Publish(PlatformEvent{
		Kind:        "network.default_security_groups_updated",
		Subject:     networkUUID,
		ProjectUUID: n.ProjectUUID,
		Meta:        map[string]string{"sg_count": strconv.Itoa(len(sgUUIDs))},
	})
	return nil
}

// DeleteNetwork drops a network from the registry. There is no
// cascade — running VMs attached to the network are not touched.
func (a *Adapter) DeleteNetwork(uuid string) error {
	if a.networkReg == nil {
		return fmt.Errorf("network registry not initialised")
	}
	projectUUID := ""
	if n, ok := a.networkReg.lookupByUUID(uuid); ok {
		projectUUID = n.ProjectUUID
	}
	if err := a.networkReg.delete(uuid); err != nil {
		return err
	}
	a.bus.Publish(PlatformEvent{
		Kind:        "network.deleted",
		Subject:     uuid,
		ProjectUUID: projectUUID,
	})
	return nil
}

// findVMByName scans every project subdir and returns
// (project-uuid, vmDir, true) on the first match. Used for
// project-agnostic lookups (legacy callers). The scan is linear in
// the number of projects.
func (a *Adapter) findVMByName(name string) (string, string, bool) {
	base := a.vmsDir()
	entries, err := os.ReadDir(base)
	if err != nil {
		return "", "", false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		candidate := filepath.Join(base, e.Name(), name)
		if st, err := os.Stat(candidate); err == nil && st.IsDir() {
			return e.Name(), candidate, true
		}
	}
	return "", "", false
}

// vmDir returns the path to a VM, searching every project. Used by
// callers that only know a name (legacy code paths and the
// DiskPath/IP/MAC accessors that pre-date project namespacing).
func (a *Adapter) vmDir(name string) string {
	if _, dir, ok := a.findVMByName(name); ok {
		return dir
	}
	return filepath.Join(a.vmsDir(), a.DefaultProjectUUID(), name)
}

// vmDirIn returns the explicit per-project directory for a named
// VM. `project` may be empty (→ default), a display name, or a
// UUID — resolved via ResolveProjectUUID.
func (a *Adapter) vmDirIn(project, name string) string {
	return filepath.Join(a.vmsDir(), a.ResolveProjectUUID(project), name)
}

// VMDir is the public accessor for vmDir(name).
func (a *Adapter) VMDir(name string) string { return a.vmDir(name) }

// VMDirEntry is one row from ListVMDirs. Used by zombiegc to
// classify orphan_dir zombies — vmDirs on disk without a matching
// registry record.
type VMDirEntry struct {
	ProjectUUID string
	Name        string
	Path        string
	ModTime     time.Time
}

// DeleteVMDir removes an orphan vmDir from disk. Used by zombiegc
// V0.1.14 auto-delete path when OrphanDirAutoDeleteAfter is
// configured ; safe to call on a path that doesn't exist (no-op).
// Refuses to delete anything outside vmsDir (defense against the
// caller stuffing in a "../"-style escape).
func (a *Adapter) DeleteVMDir(projectUUID, name string) error {
	if projectUUID == "" || name == "" {
		return fmt.Errorf("delete vm dir: empty projectUUID or name")
	}
	if strings.Contains(projectUUID, string(filepath.Separator)) ||
		strings.Contains(name, string(filepath.Separator)) ||
		strings.Contains(projectUUID, "..") ||
		strings.Contains(name, "..") {
		return fmt.Errorf("delete vm dir: refuses path escape (%q/%q)", projectUUID, name)
	}
	target := filepath.Join(a.vmsDir(), projectUUID, name)
	return os.RemoveAll(target)
}

// ListVMDirs walks vmsDir and returns one entry per VM directory it
// finds. Used by zombiegc to detect disk-side zombies that the
// registry-iterating sweep can't see (typically the result of a
// crash mid-RegisterMicroVM or operator manual rm against the
// registry). Returns empty + nil if the vmsDir is missing — that's
// a fresh agent with no VMs yet.
func (a *Adapter) ListVMDirs() []VMDirEntry {
	base := a.vmsDir()
	projectEntries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var out []VMDirEntry
	for _, p := range projectEntries {
		if !p.IsDir() {
			continue
		}
		projectPath := filepath.Join(base, p.Name())
		vmEntries, err := os.ReadDir(projectPath)
		if err != nil {
			continue
		}
		for _, v := range vmEntries {
			if !v.IsDir() {
				continue
			}
			vmPath := filepath.Join(projectPath, v.Name())
			st, err := os.Stat(vmPath)
			if err != nil {
				continue
			}
			out = append(out, VMDirEntry{
				ProjectUUID: p.Name(),
				Name:        v.Name(),
				Path:        vmPath,
				ModTime:     st.ModTime(),
			})
		}
	}
	return out
}

// VMDirIn is the public accessor for vmDirIn(project, name).
func (a *Adapter) VMDirIn(project, name string) string { return a.vmDirIn(project, name) }

// VMDirFor is the standard lookup helper for the server RPCs.
// Empty `project` resolves to the caller's default project.
// `project` may be a display name or a UUID — both work.
func (a *Adapter) VMDirFor(project, name string) string {
	return a.vmDirIn(project, name)
}

// DiskPath returns the path to the boot disk image for a named VM.
func (a *Adapter) DiskPath(name string) string {
	return filepath.Join(a.vmDir(name), "disk.img")
}

// CachedImagePath returns the absolute path to the raw disk image file in the
// local cache for the given image URL. Used by PatchImage to apply file ops to
// the cached image before any VM is cloned from it.
// Returns an error if the image is not cached, is QCOW2, or is an OCI ref.
func (a *Adapter) CachedImagePath(imageURL string) (string, error) {
	return a.imageStore.CachedImagePath(imageURL)
}

// Pull downloads OCI images in parallel using oras-go.
func (a *Adapter) Pull(ctx context.Context, images []string, parallelism int) error {
	if len(images) == 0 {
		return nil
	}
	if parallelism <= 0 {
		parallelism = len(images)
	}
	sem := make(chan struct{}, parallelism)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for _, img := range images {
		img := img
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := a.imageStore.PullImage(ctx, img, io.Discard); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return firstErr
}

// PullWithOutput downloads a single image writing progress to w.
func (a *Adapter) PullWithOutput(ctx context.Context, image string, w io.Writer) error {
	return a.imageStore.PullImage(ctx, image, w)
}

// ImageInCache reports whether image is present in the local cache.
func (a *Adapter) ImageInCache(image string) bool {
	return a.imageStore.ImageInCache(image)
}

// ListOCI returns cached images as a list of property maps.
func (a *Adapter) ListOCI() ([]map[string]interface{}, error) {
	return a.imageStore.ListOCI()
}

// CachedImage holds metadata about a locally cached image.
type CachedImage struct {
	url       string
	name      string // filename of the image on disk (e.g. noble-server-cloudimg-arm64.img)
	format    string // raw, qcow2, oci, img, or unknown
	sizeBytes int64
}

// NewCachedImage constructs a CachedImage value.
func NewCachedImage(url, name, format string, sizeBytes int64) CachedImage {
	return CachedImage{url: url, name: name, format: format, sizeBytes: sizeBytes}
}

// URL returns the source URL of the cached image.
func (c CachedImage) URL() string { return c.url }

// Name returns the filename of the image on disk.
func (c CachedImage) Name() string { return c.name }

// Format returns the image format (raw, qcow2, oci, img, or unknown).
func (c CachedImage) Format() string { return c.format }

// SizeBytes returns the total size of the cache entry in bytes.
func (c CachedImage) SizeBytes() int64 { return c.sizeBytes }

// ListCachedImages returns all images present in the local cache.
func (a *Adapter) ListCachedImages() ([]CachedImage, error) {
	dir := a.imageStore.Dir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var result []CachedImage
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		entryPath := filepath.Join(dir, e.Name())
		size := dirSize(entryPath)
		name, format := imageCacheNameAndFormat(entryPath)
		result = append(result, NewCachedImage(
			imagestore.UnsanitizeRef(e.Name()),
			name,
			format,
			size,
		))
	}
	return result, nil
}

// imageCacheNameAndFormat inspects a cache entry directory and returns the
// image filename and its format (raw, qcow2, oci, img, or unknown).
func imageCacheNameAndFormat(entryPath string) (name, format string) {
	// OCI layout: contains index.json
	if _, err := os.Stat(filepath.Join(entryPath, "index.json")); err == nil {
		return "(oci layout)", "oci"
	}
	// HTTP download: a single file in the directory
	files, err := os.ReadDir(entryPath)
	if err != nil {
		return "", "unknown"
	}
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		name = f.Name()
		ext := strings.ToLower(filepath.Ext(name))
		switch ext {
		case ".raw":
			format = "raw"
		case ".qcow2":
			format = "qcow2"
		case ".img":
			format = "img"
		default:
			format = "unknown"
		}
		return
	}
	return "", "unknown"
}

// dirSize returns the total size in bytes of all files under a directory.
func dirSize(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// DeleteOCI removes a cached image directory.
func (a *Adapter) DeleteOCI(name string) error {
	return a.imageStore.DeleteOCI(name)
}

// VMExists reports whether a named VM directory exists, searched
// across every project. Kept for legacy callers; new code that
// knows which project it cares about should use VMExistsIn.
func (a *Adapter) VMExists(name string) bool {
	_, _, ok := a.findVMByName(name)
	return ok
}

// VMExistsIn reports whether a VM exists in the given project.
// Empty project resolves to the caller's default. Used by the
// create paths so the same VM name can be reused across projects.
func (a *Adapter) VMExistsIn(project, name string) bool {
	_, err := os.Stat(a.vmDirIn(project, name))
	return err == nil
}

// ListLocal returns local VM directories as a map[name]properties.
func (a *Adapter) ListLocal() (map[string]map[string]interface{}, error) {
	base := a.vmsDir()
	projects, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]map[string]interface{}{}, nil
		}
		return nil, err
	}
	m := make(map[string]map[string]interface{})
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		// Skip the registry file itself.
		if p.Name() == registryFileName {
			continue
		}
		// Only UUID-named subdirs hold VMs (post-migration). A
		// stray non-UUID dir means migration has not yet completed
		// for that name — skip rather than misrepresent.
		if !isUUID(p.Name()) {
			continue
		}
		projectDir := filepath.Join(base, p.Name())
		entries, err := os.ReadDir(projectDir)
		if err != nil {
			continue
		}
		// Resolve the display name once per project subdir.
		projectName := p.Name() // fallback to UUID if not registered
		if proj, ok := a.ProjectByUUID(p.Name()); ok {
			projectName = proj.Name
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			vmDir := filepath.Join(projectDir, e.Name())
			props := map[string]interface{}{
				"name":         e.Name(),
				"project":      projectName,
				"project_uuid": p.Name(),
			}
			cfg := filepath.Join(vmDir, "config.json")
			if b, err := os.ReadFile(cfg); err == nil {
				var extra map[string]interface{}
				if json.Unmarshal(b, &extra) == nil {
					for k, v := range extra {
						props[k] = v
					}
				}
			}
			// Determine running state.
			//
			// exit.json takes precedence over the PID probe : the qemu
			// driver's reaper goroutine (cmd.Wait) writes it the moment
			// the qemu child exits, carrying pid + exit_code + the
			// nanosecond timestamp. A subsequent PID probe might still
			// see the recycled PID assigned to a different process and
			// falsely report "running" — exit.json closes that race.
			//
			// When exit.json is absent we fall back to the pid probe,
			// matching the pre-reaper behaviour for back-compat with
			// drivers that haven't been updated yet.
			props["State"] = "stopped"
			vmExitFile := filepath.Join(vmDir, "exit.json")
			if data, err := os.ReadFile(vmExitFile); err == nil {
				var ex struct {
					ExitCode   int   `json:"exit_code"`
					ExitedAtNs int64 `json:"exited_at_ns"`
				}
				if json.Unmarshal(data, &ex) == nil {
					props["State"] = "exited"
					props["ExitCode"] = float64(ex.ExitCode)
					props["ExitedAtUnixNs"] = float64(ex.ExitedAtNs)
				}
			} else {
				pidFile := filepath.Join(vmDir, "vm.pid")
				if data, err := os.ReadFile(pidFile); err == nil {
					if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
						if proc, err := os.FindProcess(pid); err == nil {
							if proc.Signal(syscall.Signal(0)) == nil {
								props["State"] = "running"
								props["Running"] = true
							}
						}
					}
				}
			}
			if _, ok := props["cpu"]; !ok {
				props["cpu"] = float64(2)
			}
			if _, ok := props["mem_mb"]; !ok {
				if gib, ok := props["mem_gib"].(float64); ok && gib > 0 {
					props["mem_mb"] = gib * 1024
				} else {
					props["mem_mb"] = float64(2048)
				}
			}
			if _, ok := props["disk_gb"]; !ok {
				diskPath := filepath.Join(vmDir, "disk.img")
				if fi, err := os.Stat(diskPath); err == nil {
					props["disk_gb"] = float64(fi.Size()) / (1024 * 1024 * 1024)
				}
			}
			// Key by `<project>/<name>` so two VMs that share a
			// name across projects both survive. Callers should
			// read props["name"] / props["project"] rather than
			// parse the key.
			m[p.Name()+"/"+e.Name()] = props
		}
	}
	return m, nil
}

// CloneVM "clones" an OCI image by provisioning a new VM directory.
// extraDisks lists additional blank data disks to create alongside the boot
// disk. Each disk is stored as <label>.img (or data-<n>.img as fallback).
// project is the namespace the VM lands in; empty resolves to the caller's
// default project.
func (a *Adapter) CloneVM(image, project, name string, extraDisks []ExtraDisk, w io.Writer) error {
	if !a.imageStore.ImageInCache(image) {
		return fmt.Errorf("vz clone: image %s not in cache; run pull first", image)
	}
	dir := a.vmDirIn(project, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("vz clone: mkdir %s: %w", dir, err)
	}
	// Copy OS image to disk.img before provisionVMDir which only creates metadata.
	diskPath := filepath.Join(dir, "disk.img")
	if err := a.imageStore.CopyImageToDisk(image, diskPath, w); err != nil {
		_ = os.RemoveAll(dir)
		return fmt.Errorf("vz clone %s: copy image: %w", name, err)
	}
	// Patch GRUB in-place so the boot sequence is visible in both the VZ
	// graphical window (VirtIO GPU framebuffer) and the serial console tab
	// from the very first boot, without relying on cloud-init.
	_, _ = fmt.Fprintf(w, "patching grub (console output)…\n")
	_ = grub.MkConfig(diskPath)
	if err := a.provisionVMDir(dir, image, extraDisks); err != nil {
		_ = os.RemoveAll(dir)
		return fmt.Errorf("vz clone %s: %w", name, err)
	}
	// Register in the VM inventory so multi-host dispatch + the
	// future reconciler know about this VM. Best-effort: a
	// failure here doesn't roll back the on-disk state (the VM
	// is fully provisioned + bootable; it just lacks an
	// inventory entry, which the legacy hypervisorForVM
	// fallback handles).
	if a.vmReg != nil {
		projectUUID := a.ResolveProjectUUID(project)
		if _, err := a.RegisterVM(CreateVMSpec{
			ProjectUUID: projectUUID,
			Name:        name,
			HostUUID:    a.localHostUUID(),
			Image:       image,
		}); err != nil {
			fmt.Fprintf(w, "warning: vm inventory registration failed (continuing): %v\n", err)
		}
	}
	_, _ = fmt.Fprintf(w, "cloned %s -> %s\n", image, name)
	if len(extraDisks) > 0 {
		_, _ = fmt.Fprintf(w, "created %d extra data disk(s)\n", len(extraDisks))
	}
	return nil
}

// provisionVMDir wires the host-side state for a VM on top of
// the disk.img that CloneVM has already produced.
//
// Split:
//
//   - The Apple-VZ machine state (nvram, machine-id, mac.txt) is
//     delegated to the local Hypervisor driver's CreateVM. That
//     code lives in pkg/openweft/weft-driver-vz and is what
//     a future weft-agent on another host would reuse.
//   - The data-disk creation + config.json layout stay here
//     because they're still weft-control concerns: a future
//     commit moves data disks behind the VolumeDriver +
//     AttachDisk path, and config.json behind a vmspec.hcl in
//     the VM-inventory registry.
func (a *Adapter) provisionVMDir(dir, image string, extraDisks []ExtraDisk) error {
	hyp, err := a.localHypervisor()
	if err != nil {
		return fmt.Errorf("provisionVMDir: %w", err)
	}
	// boot disk.img is created by CloneVM (copyImageToDisk) before this call.
	type dataDiskEntry struct {
		Name       string `json:"name"`
		Mountpoint string `json:"mountpoint,omitempty"`
	}
	dataDisks := make([]dataDiskEntry, 0, len(extraDisks))
	for i, d := range extraDisks {
		if d.SizeGiB <= 0 {
			continue
		}
		fileName := fmt.Sprintf("data-%d.img", i)
		if d.Label != "" {
			fileName = d.Label + ".img"
		}
		p := filepath.Join(dir, fileName)
		if err := hyp.AttachDisk(context.Background(), dir, driversAPI.DiskSpec{
			BackingPath: p,
			SizeGiB:     d.SizeGiB,
		}); err != nil {
			return fmt.Errorf("create data disk %d: %w", i, err)
		}
		dataDisks = append(dataDisks, dataDiskEntry{Name: fileName, Mountpoint: d.Mountpoint})
	}
	if err := hyp.CreateVM(context.Background(), driversAPI.VMSpec{UUID: dir}); err != nil {
		return fmt.Errorf("create vm state: %w", err)
	}
	cfg := map[string]interface{}{"image": image, "data_disks": dataDisks}
	b, _ := json.Marshal(cfg)
	_ = os.WriteFile(filepath.Join(dir, "config.json"), b, 0o600)
	return nil
}

// MicroVMShare describes one virtio-fs share to expose to a microVM.
// weft-microvm's `weft-microvm run` uses this to plumb an extracted
// OCI image rootfs to the guest, where weft-microvm-init mounts it on
// /newroot before pivot_root.
type MicroVMShare struct {
	// Tag is the mount tag the guest passes to `mount -t virtiofs`.
	// Conventional default for weft-microvm: "rootfs0".
	Tag string
	// Path is the host directory exposed. Must exist + be readable.
	Path string
	// ReadOnly toggles the SharedDirectory's read-only flag. weft-microvm
	// keeps the rootfs read-write so the guest's `.weft-microvm/config.json`
	// override path stays writable; set to true for purely shared
	// inputs (e.g. mounting host source trees into a build container).
	ReadOnly bool
	// Clone asks weft to materialise a copy-on-write clone of Path
	// into <vmDir>/<Tag>/ via macOS clonefile(2) before recording
	// the share. The cloned tree (not the original Path) is what
	// the guest sees over virtio-fs, so multiple VMs sharing the
	// same source rootfs each get their own writable view without
	// blocks being duplicated until written. Host must be APFS.
	// Cleanup is implicit: the clone lives under vmDir and goes
	// away with DeleteVM.
	Clone bool
}

// MicroVMBoot bundles the boot artefacts for a microVM. Exactly
// one of {BootISO} or {Kernel} must be set:
//
//   - BootISO: UKI mode. The ISO is attached as a read-only
//     primary disk; firmware's EFI loader picks up the UKI.
//   - Kernel (+ optional Initrd): direct-Linux mode. weft uses
//     `vz.LinuxBootLoader` and skips EFI/UKI entirely.
//
// Cmdline is the kernel command line. Empty means use the default
// (`console=hvc0 root=/dev/vda2 rw` for the direct-Linux path).
type MicroVMBoot struct {
	BootISO string
	Kernel  string
	Initrd  string
	Cmdline string
	// Image is the OCI ref the microVM was hatched from (e.g.
	// "ghcr.io/openweft/weft-etcd:v3.6.0"). Persisted into the VM
	// dir's config.json so ListLocal can surface it on the wire —
	// the operator-facing IMAGE column in `weft host ls` / TUI
	// reads from there. Optional ; empty leaves the field absent.
	Image string
	// CPU + MemoryMiB are the resource caps the workload requested
	// (typically from `weft infra deploy`'s plan.hcl `resources {}`
	// block). Stored on the inventory record + persisted into
	// config.json so cross-host ListVMs renders the operator-
	// meaningful values instead of 0 when the daemon hasn't yet
	// scanned local disk.
	CPU       int
	MemoryMiB int
}

// RegisterMicroVM creates a VM directory wired for a microVM-style
// boot. Two boot modes are supported, controlled by `boot`:
//
//   - UKI mode    — set boot.BootISO only
//   - direct-Linux — set boot.Kernel (and optionally boot.Initrd)
//
// `boot.Cmdline` overrides the default kernel cmdline; needed for
// weft-microvm-style microVMs which want `weft.rootfs=virtiofs:rootfs0`.
//
// The resulting VM appears in `ListLocal` alongside CloneVM-
// created classic VMs and is started the same way (`StartVM(name,
// "")`). buildVZConfigFromDir auto-detects which boot mode the VM
// dir is wired for by inspecting which files are present.
//
// Idempotent on re-registration: if a VM with the same (project, name)
// already exists, the call claims local ownership (flips host_uuid to
// the calling agent's host UUID) so subsequent StartVM lands on the
// driver bundle this agent owns — fixes [[etcd-vm-host-pinning]].
// Without the claim, a VM record cached in shared etcd from an
// earlier registration on another DC would route every dispatch back
// to that DC's driver, even when the operator clearly ran the
// command on a different agent. The spec (image, cpu, mem, shares)
// is left untouched.
//
// To force a full re-registration (re-copy boot artefacts) the
// operator deletes the VM first (DeleteVM).
func (a *Adapter) RegisterMicroVM(project, name string, boot MicroVMBoot, shares []MicroVMShare) error {
	if a.VMExistsIn(project, name) {
		projUUID := a.ResolveProjectUUID(project)
		local := a.LocalHostUUID()
		existing, _ := a.vmReg.lookupByName(projUUID, name)
		// VM dir on disk + no registry record = the dir survived a
		// daemon / etcd wipe (operator nuked vmReg, agent restarted,
		// dir still on disk). Seed the inventory by REGISTERING the
		// VM now so the etcd / file registry catches up — without
		// this the deploy returns idempotent-skip and the registry
		// stays empty forever, breaking cluster-wide visibility.
		if existing.UUID == "" {
			regImage := boot.Image
			if regImage == "" {
				regImage = "microvm/direct_linux"
			}
			if _, err := a.RegisterVM(CreateVMSpec{
				ProjectUUID: projUUID,
				Name:        name,
				HostUUID:    local,
				Image:       regImage,
				CPUCount:    boot.CPU,
				MemoryMiB:   boot.MemoryMiB,
			}); err != nil {
				fmt.Fprintf(os.Stderr, "weft: register-microvm: re-seed registry for %q failed: %v\n", name, err)
			} else {
				fmt.Fprintf(os.Stderr, "weft: register-microvm: vm %q dir present but no registry record — re-seeded under host %s\n",
					name, local)
			}
			return nil
		}
		if local != "" && existing.UUID != "" && existing.HostUUID != local {
			if err := a.vmReg.setHost(existing.UUID, local); err != nil {
				fmt.Fprintf(os.Stderr, "weft: register-microvm: claim local ownership of %q failed: %v\n", name, err)
			} else {
				a.bus.Publish(PlatformEvent{
					Kind:        "vm.migrated",
					Subject:     existing.UUID,
					ProjectUUID: projUUID,
					Meta: map[string]string{
						"old_host": existing.HostUUID, "new_host": local, "reason": "register-microvm-claim",
					},
				})
				fmt.Fprintf(os.Stderr, "weft: register-microvm: vm %q already in project %s — claimed local ownership (%s → %s)\n",
					name, projUUID, existing.HostUUID, local)
			}
		} else {
			fmt.Fprintf(os.Stderr, "weft: register-microvm: vm %q already in project %s — idempotent skip\n",
				name, projUUID)
		}
		return nil
	}
	if boot.BootISO == "" && boot.Kernel == "" {
		return fmt.Errorf("vz register-microvm: need exactly one of BootISO or Kernel")
	}
	if boot.BootISO != "" && boot.Kernel != "" {
		return fmt.Errorf("vz register-microvm: BootISO and Kernel are mutually exclusive")
	}

	// Inject WEFT_PROJECT_UUID into the guest cmdline so workloads
	// inside the VM can build their per-project NATS subject
	// (weft.events.project.$WEFT_PROJECT_UUID.events.>) without
	// having to be told what project they belong to.
	// Per [[weft-tenant-event-access]] Phase 2.
	//
	// weft-microvm-init's cmdline parser (exec.go in weft-microvm)
	// builds a last-wins map from /proc/cmdline tokens, so adding
	// a fresh `weft.env=...` would clobber any caller-supplied env
	// list. Merge into the existing clause instead: colon-separate
	// inside one value, matching weft-microvm-init's parser.
	projectUUID := a.ResolveProjectUUID(project)
	if projectUUID != "" {
		boot.Cmdline = mergeProjectEnv(boot.Cmdline, "WEFT_PROJECT_UUID="+projectUUID)
	}

	dir := a.vmDirIn(project, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("vz register-microvm: mkdir %s: %w", dir, err)
	}

	// Copy (rather than symlink) the boot artefacts so the VM dir
	// stays self-contained — matches CloneVM's "VM owns its bits"
	// semantics. ISOs / kernels / initrds are typically 5-30 MiB so
	// the copy cost is trivial.
	if boot.BootISO != "" {
		if err := copyFileAtomic(boot.BootISO, filepath.Join(dir, "boot.iso")); err != nil {
			_ = os.RemoveAll(dir)
			return fmt.Errorf("vz register-microvm: copy iso: %w", err)
		}
	} else {
		if err := copyFileAtomic(boot.Kernel, filepath.Join(dir, "kernel")); err != nil {
			_ = os.RemoveAll(dir)
			return fmt.Errorf("vz register-microvm: copy kernel: %w", err)
		}
		if boot.Initrd != "" {
			if err := copyFileAtomic(boot.Initrd, filepath.Join(dir, "initrd")); err != nil {
				_ = os.RemoveAll(dir)
				return fmt.Errorf("vz register-microvm: copy initrd: %w", err)
			}
		}
	}

	// NVRAM + machine-id + MAC: needed by buildVZConfigFromDir
	// for the EFI boot path. Routed through the local Hypervisor
	// driver so classic + microVM dirs use the exact same code
	// path (previously this block was duplicated inline). See
	// driver.Hypervisor.CreateVM for the idempotent semantics.
	hyp, err := a.localHypervisor()
	if err != nil {
		_ = os.RemoveAll(dir)
		return fmt.Errorf("vz register-microvm: %w", err)
	}
	if err := hyp.CreateVM(context.Background(), driversAPI.VMSpec{UUID: dir}); err != nil {
		_ = os.RemoveAll(dir)
		return fmt.Errorf("vz register-microvm: create vm state: %w", err)
	}

	// Per-project NATS user-NKey: drop the seed into <vmDir>/nats/
	// and append a read-only virtio-fs share so the guest can mount
	// it at /run/weft/ (conventional tag "weft-nats"). The seed is
	// minted lazily on the first RegisterMicroVM for the project
	// and reused for every subsequent VM in the same project.
	// Per [[weft-tenant-event-access]] Phase 2; server-side subject
	// permissions land in Phase 3.
	if projectUUID != "" && a.projects != nil {
		seed, err := a.projects.ensureNATSUserSeed(projectUUID)
		if err != nil {
			_ = os.RemoveAll(dir)
			return fmt.Errorf("vz register-microvm: nats seed: %w", err)
		}
		credsDir := filepath.Join(dir, "nats")
		if err := os.MkdirAll(credsDir, 0o700); err != nil {
			_ = os.RemoveAll(dir)
			return fmt.Errorf("vz register-microvm: mkdir nats: %w", err)
		}
		// Write to a tmp file + rename so a reader on the guest
		// never sees a half-written seed (host-side; the share
		// hasn't been attached yet but defensive against future
		// re-register paths).
		credsPath := filepath.Join(credsDir, "nats.nkey")
		tmp := credsPath + ".tmp"
		if err := os.WriteFile(tmp, []byte(seed), 0o600); err != nil {
			_ = os.RemoveAll(dir)
			return fmt.Errorf("vz register-microvm: write nats seed: %w", err)
		}
		if err := os.Rename(tmp, credsPath); err != nil {
			_ = os.RemoveAll(dir)
			return fmt.Errorf("vz register-microvm: rename nats seed: %w", err)
		}
		// Refuse to clobber a caller-supplied "weft-nats" share —
		// that's almost certainly a misconfiguration, not an
		// override the operator meant.
		for _, s := range shares {
			if s.Tag == natsShareTag {
				_ = os.RemoveAll(dir)
				return fmt.Errorf("vz register-microvm: share tag %q is reserved for the per-project NATS creds", natsShareTag)
			}
		}
		shares = append(shares, MicroVMShare{
			Tag:      natsShareTag,
			Path:     credsDir,
			ReadOnly: true,
		})
		// Re-render the authorization block now that this project
		// has a (possibly new) NKey seed. No-op when auto-render is
		// off. Errors are intentionally swallowed: the registry
		// mutation already succeeded and undoing the VM register
		// because nats.conf couldn't be re-written would be worse
		// than asking the operator to re-run `weft admin nats-authz`.
		_ = a.autoRenderNATSAuthorization()
	}

	// config.json shape matching what runvm.go's vmCfgJSON decodes.
	// Keep the JSON keys explicit so a future refactor of either
	// side flags the schema mismatch loudly.
	type shareEntry struct {
		Tag      string `json:"tag"`
		Path     string `json:"path"`
		ReadOnly bool   `json:"read_only,omitempty"`
	}
	entries := make([]shareEntry, len(shares))
	for i, s := range shares {
		if s.Tag == "" || s.Path == "" {
			_ = os.RemoveAll(dir)
			return fmt.Errorf("vz register-microvm: share #%d needs both Tag and Path", i)
		}
		exposePath := s.Path
		if s.Clone {
			// macOS clonefile(2) is recursive on directories and
			// produces an APFS copy-on-write clone in O(metadata).
			// Destination must NOT pre-exist (the syscall fails with
			// EEXIST otherwise) — we create the parent dir but let
			// clonefile create the leaf. The clone lives under vmDir
			// so DeleteVM's RemoveAll handles cleanup automatically.
			clonePath := filepath.Join(dir, s.Tag)
			if err := cloneOrCopyTree(s.Path, clonePath); err != nil {
				_ = os.RemoveAll(dir)
				return fmt.Errorf("vz register-microvm: stage share %q -> %q: %w", s.Path, clonePath, err)
			}
			exposePath = clonePath
		}
		entries[i] = shareEntry{Tag: s.Tag, Path: exposePath, ReadOnly: s.ReadOnly}
	}
	// Pre-allocate the guest's AF_VSOCK CID before writing config.json
	// so the driver picks it up when it bakes its qemu argv (or wires
	// the equivalent on Apple VZ). We can't read it back from the VM
	// inventory yet — RegisterVM lower in this function is what
	// persists the VsockCID on the VM record — but the CID is a pure
	// function of (projectUUID, name) at this point, so we recompute
	// it here. The downstream call to RegisterPodCID will store the
	// same value on the registry, and setVsockCID persists it on the
	// VM record. Three callers, one truth — guaranteed by the hash.
	preCID := AllocateVsockCID(projectUUID, name)
	cfg := struct {
		MicroVM  bool         `json:"microvm"`
		Cmdline  string       `json:"cmdline,omitempty"`
		Shares   []shareEntry `json:"shares,omitempty"`
		VsockCID uint32       `json:"vsock_cid,omitempty"`
		Image    string       `json:"image,omitempty"`
		CPU      int          `json:"cpu,omitempty"`
		MemMB    int          `json:"mem_mb,omitempty"`
	}{MicroVM: true, Cmdline: boot.Cmdline, Shares: entries, VsockCID: preCID, Image: boot.Image, CPU: boot.CPU, MemMB: boot.MemoryMiB}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "config.json"), b, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return fmt.Errorf("vz register-microvm: write config: %w", err)
	}

	// Lifecycle event: the VM dir is fully provisioned and ready
	// for StartVM. Recorded *after* every artefact is on disk so
	// the "registered" stamp truly marks "ready to boot".
	mode := "uki"
	if boot.Kernel != "" {
		mode = "direct_linux"
	}
	RecordEvent(dir, "registered", map[string]string{"mode": mode})
	// VM inventory entry — best-effort. Same rationale as in
	// CloneVM: the VM is fully provisioned on disk; failure to
	// register only loses the multi-host dispatch path (handled
	// by the hypervisorForVM fallback).
	//
	// Image preference : the caller-supplied OCI ref (boot.Image)
	// wins when set — it's the operator-meaningful identity for
	// the workload ("weft-etcd:v3.6.0" reads better than the
	// internal "microvm/direct_linux" boot-mode label). Falls
	// back to "microvm/<mode>" for callers that don't pass an
	// OCI ref (legacy paths + bare-direct-Linux registrations),
	// keeping the existing audit semantics.
	regImage := boot.Image
	if regImage == "" {
		regImage = "microvm/" + mode
	}
	if a.vmReg != nil {
		projectUUID := a.ResolveProjectUUID(project)
		registered, err := a.RegisterVM(CreateVMSpec{
			ProjectUUID: projectUUID,
			Name:        name,
			HostUUID:    a.localHostUUID(),
			Image:       regImage,
			CPUCount:    boot.CPU,
			MemoryMiB:   boot.MemoryMiB,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "weft: register-microvm inventory: %v\n", err)
		} else {
			// AF_VSOCK CID allocation : deterministic hash of
			// (projectUUID, name). Written into config.json (above)
			// + the VM record so the QEMU driver binds the device
			// at StartVM. Re-computed here from the same hash inputs
			// — pure, no need to thread preCID down.
			//
			// Note (v0.4.51) : the in-memory podCIDs registry is NOT
			// stamped here anymore. It self-populates via the
			// GuestPodPlane.Attach autoregister-on-first-Hello path
			// using peer.CID() from the kernel — which is the truth
			// for both backends (QEMU binds the CID we asked for ;
			// Apple VZ picks its own CID we can't query). This
			// removes the v0.4.46-era bug where VZ-backed VMs got
			// rejected by strict-when-known because the allocator's
			// pick disagreed with VZ's kernel assignment.
			cid := AllocateVsockCID(projectUUID, name)
			if cid != 0 {
				if err := a.vmReg.setVsockCID(registered.UUID, cid); err != nil {
					fmt.Fprintf(os.Stderr, "weft: register-microvm: persist vsock_cid: %v\n", err)
				}
			}
		}
	}
	return nil
}

// copyFileAtomic copies src to dst atomically by writing to a temp
// file in the same directory then renaming. Used by RegisterMicroVM
// so a partial copy never leaves a half-written boot.iso behind.
func copyFileAtomic(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".boot-iso-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()
	if _, err := io.Copy(tmp, in); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, dst)
}

// StartVM starts the VM by forking a "vz-vm-run" subprocess that owns the
// VZVirtualMachine lifetime and opens a native graphical window.
// This mirrors how `tart run` works: the VM lives in a child process so
// `mock up` can exit without killing the VMs.
//
// Routed through the local driver Bundle's HypervisorDriver (see
// [[weft-driver-registry-split]]):
//
//   - RecordEvent for `server.start_attempted` / `_failed` /
//     `_forked` happens at the Adapter — the events live in weft's
//     event taxonomy, not the driver's.
//   - The actual fork + vm.pid write + wait-goroutine lives in
//     the driver, which uses Options.SpawnVMCommand (wired in
//     initLocalDrivers below) to know what to fork without
//     baking `vz-vm-run` into the driver's code.
//   - Options.OnVMExit (also wired in initLocalDrivers) reports
//     `server.vz_vm_run_exited` back into weft's RecordEvent
//     stream when the subprocess terminates.
func (a *Adapter) StartVM(name, cloudInitISO string) error {
	dir := a.vmDir(name)
	RecordEvent(dir, "server.start_attempted", nil)
	hyp, err := a.hypervisorForVM(name)
	if err != nil {
		RecordEvent(dir, "server.start_failed", map[string]string{"err": err.Error()})
		return fmt.Errorf("vz start %s: %w", name, err)
	}
	if err := hyp.StartVM(context.Background(), dir); err != nil {
		RecordEvent(dir, "server.start_failed", map[string]string{"err": err.Error()})
		return fmt.Errorf("vz start %s: %w", name, err)
	}
	// The driver wrote vm.pid; surface it in the event so
	// log-scrapers still see what they used to see.
	pidStr := ""
	if data, err := os.ReadFile(filepath.Join(dir, "vm.pid")); err == nil {
		pidStr = strings.TrimSpace(string(data))
	}
	RecordEvent(dir, "server.vz_vm_run_forked", map[string]string{"pid": pidStr})
	return nil
}

// StopVM signals the VM subprocess to terminate gracefully (SIGTERM).
//
// Routed through the local driver Bundle's HypervisorDriver
// (see [[weft-driver-registry-split]]): the Adapter captures the
// PID + event metadata, the driver does the actual signaling.
// The PID is captured here (not asked back from the driver) so
// the `vm.stop` event payload stays unchanged — operators
// inspecting logs still see the PID that received SIGTERM.
func (a *Adapter) StopVM(name string) error {
	hyp, err := a.hypervisorForVM(name)
	if err != nil {
		return fmt.Errorf("vz stop %s: %w", name, err)
	}
	vmDir := a.vmDir(name)
	// Snapshot the PID before the driver call so the published
	// event carries it. Missing / malformed pid file → no-op
	// stop, no event (matches the previous behaviour).
	pidStr := ""
	if data, err := os.ReadFile(filepath.Join(vmDir, "vm.pid")); err == nil {
		pidStr = strings.TrimSpace(string(data))
	}
	if err := hyp.StopVM(context.Background(), vmDir); err != nil {
		return err
	}
	if pidStr == "" {
		return nil // nothing was running, nothing to announce
	}
	project, subject := splitVMDir(a.vmsDir(), vmDir)
	a.bus.Publish(PlatformEvent{
		Kind:        "vm.stop",
		Subject:     subject,
		ProjectUUID: project,
		Meta:        map[string]string{"pid": pidStr},
	})
	return nil
}

// DeleteVM removes the VM directory.
//
// Routed through the local driver Bundle's HypervisorDriver
// (see [[weft-driver-registry-split]]): the Adapter resolves the
// vmDir + captures event metadata (project, subject) before
// invoking the driver, which is responsible for the actual
// teardown. Stop-before-delete is still the Adapter's concern
// for now (StopVM owns the vm.pid subprocess management); when
// it migrates to the driver, this method becomes a single
// driver call.
func (a *Adapter) DeleteVM(name string) error {
	hyp, err := a.hypervisorForVM(name)
	if err != nil {
		return fmt.Errorf("vz delete %s: %w", name, err)
	}
	_ = a.StopVM(name)
	vmDir := a.vmDir(name)
	project, subject := splitVMDir(a.vmsDir(), vmDir)
	if err := hyp.DeleteVM(context.Background(), vmDir); err != nil {
		return err
	}
	// Drop the inventory entry too, if any. Best-effort — legacy
	// VMs (provisioned before the inventory landed) won't have
	// one + that's fine.
	if a.vmReg != nil {
		if vm, ok := a.vmReg.lookupByName(project, subject); ok {
			_ = a.vmReg.delete(vm.UUID)
		}
	}
	a.bus.Publish(PlatformEvent{
		Kind:        "vm.deleted",
		Subject:     subject,
		ProjectUUID: project,
	})
	return nil
}

// ExecInVM runs shellCmd inside the VM via SSH.
func (a *Adapter) ExecInVM(vmName, shellCmd string, stdin io.Reader) ([]byte, error) {
	ip, err := a.IP(vmName)
	if err != nil || ip == "" {
		return nil, fmt.Errorf("vz exec: no IP for %s: %v", vmName, err)
	}
	user := defaultUser
	a.mu.Lock()
	if u, ok := a.users[vmName]; ok {
		user = u
	}
	keyPath := a.sshKeyPath
	a.mu.Unlock()
	return execViaSSH(ip, user, keyPath, shellCmd, stdin)
}

// sshKeyAuth builds an SSH auth method from a private-key file.
func sshKeyAuth(keyPath string) ([]ssh.AuthMethod, error) {
	b, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read ssh key %s: %w", keyPath, err)
	}
	signer, err := ssh.ParsePrivateKey(b)
	if err != nil {
		return nil, fmt.Errorf("parse ssh key %s: %w", keyPath, err)
	}
	return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil
}

// execViaSSH connects to ip:22 and runs cmd using key-based authentication.
// keyPath must point to a valid SSH private key file; password auth is not used.
func execViaSSH(ip, user, keyPath, cmd string, stdin io.Reader) ([]byte, error) {
	if keyPath == "" {
		return nil, fmt.Errorf("ssh to %s: no SSH private key configured (call SetSSHKeyPath before ExecInVM)", ip)
	}
	auth, err := sshKeyAuth(keyPath)
	if err != nil {
		return nil, fmt.Errorf("ssh to %s: %w", ip, err)
	}
	client, err := sshDial("tcp", ip+":22", sshClientConfig(user, auth))
	if err != nil {
		return nil, fmt.Errorf("ssh dial %s: %w", ip, err)
	}
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("ssh session %s: %w", ip, err)
	}
	defer session.Close()
	if stdin != nil {
		session.Stdin = stdin
	}
	var buf bytes.Buffer
	session.Stdout = &buf
	session.Stderr = &buf
	if err := session.Run(cmd); err != nil {
		return buf.Bytes(), err
	}
	return buf.Bytes(), nil
}

// IP returns the IP of a running VM by querying the ARP table for its MAC.
// The MAC address is persisted in the VM directory during CloneVM.
// IP returns the IP of a running VM by looking up its MAC address.
// It first checks the macOS DHCP lease file written by the VZ NAT DHCP server,
// then falls back to querying the ARP table (arp -n to avoid DNS lookups).
func (a *Adapter) IP(name string) (string, error) {
	macBytes, err := os.ReadFile(filepath.Join(a.vmDir(name), "mac.txt"))
	if err != nil {
		return "", fmt.Errorf("vz ip %s: read mac: %w", name, err)
	}
	mac := strings.TrimSpace(string(macBytes))

	// Try DHCP leases first – faster and doesn't require prior ARP traffic.
	if ip := ipFromDHCPLeases(mac); ip != "" {
		return ip, nil
	}
	return ipFromARP(mac)
}

// normMAC normalises a MAC address to lowercase colon-separated zero-padded
// octets (e.g. "c2:58:e1:0f:e1:10"). It handles single-digit octets such as
// "c2:58:e1:f:e1:10" that macOS ARP outputs without leading zeros.
// Returns the original lowercased string unchanged on parse error.
func normMAC(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	sep := ":"
	if !strings.Contains(s, ":") && strings.Contains(s, "-") {
		sep = "-"
	}
	octets := strings.Split(s, sep)
	if len(octets) != 6 && len(octets) != 8 {
		return s
	}
	out := make([]string, len(octets))
	for i, o := range octets {
		v, err := strconv.ParseUint(o, 16, 8)
		if err != nil {
			return s
		}
		out[i] = fmt.Sprintf("%02x", v)
	}
	return strings.Join(out, ":")
}

// parseDHCPLeasesData searches data (contents of a dhcpd_leases-formatted
// file) for the IP address assigned to mac. mac is normalised before
// comparison so both zero-padded and non-padded forms are matched.
func parseDHCPLeasesData(data []byte, mac string) string {
	want := normMAC(mac)
	var curIP string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "ip_address=") {
			curIP = strings.TrimPrefix(line, "ip_address=")
		} else if strings.HasPrefix(line, "hw_address=") {
			// strip leading type prefix "1,"
			raw := strings.TrimPrefix(line, "hw_address=")
			if idx := strings.Index(raw, ","); idx >= 0 {
				raw = raw[idx+1:]
			}
			if normMAC(raw) == want {
				return curIP
			}
		}
	}
	return ""
}

// ipFromDHCPLeases reads /var/db/dhcpd_leases which is maintained by the
// macOS DHCP server that backs Virtualization.framework NAT networking.
// Format per entry:
//
//	{
//	    ip_address=192.168.64.3
//	    hw_address=1,c2:58:e1:f:e1:10
//	    ...
//	}
func ipFromDHCPLeases(mac string) string {
	data, err := os.ReadFile("/var/db/dhcpd_leases")
	if err != nil {
		return ""
	}
	return parseDHCPLeasesData(data, mac)
}

// ipFromARP queries the ARP table for a MAC address and returns its IP.
// Uses -n to suppress reverse DNS lookups (avoids multi-second stalls).
func ipFromARP(mac string) (string, error) {
	out, err := exec.Command("arp", "-an").CombinedOutput()
	if err != nil {
		return "", err
	}
	want := normMAC(mac)
	for _, line := range strings.Split(string(out), "\n") {
		// arp -an line: "? (192.168.64.3) at c2:58:e1:f:e1:10 on vmnet2 ..."
		fields := strings.Fields(line)
		for i, f := range fields {
			if normMAC(f) == want {
				// IP is the field before, formatted as "(192.168.x.x)"
				for j := i - 1; j >= 0; j-- {
					p := fields[j]
					if len(p) > 2 && p[0] == '(' && p[len(p)-1] == ')' {
						return p[1 : len(p)-1], nil
					}
				}
			}
		}
	}
	return "", fmt.Errorf("MAC %s not found in ARP table", mac)
}

// GetOSFromCache infers the guest OS from an OCI image reference.
func (a *Adapter) GetOSFromCache(image string) string {
	base := strings.ToLower(filepath.Base(strings.SplitN(image, ":", 2)[0]))
	switch {
	case strings.Contains(base, "debian"), strings.Contains(base, "ubuntu"),
		strings.Contains(base, "rocky"), strings.Contains(base, "alpine"),
		strings.Contains(base, "centos"), strings.Contains(base, "linux"):
		return "linux"
	case strings.Contains(base, "macos"), strings.Contains(base, "darwin"):
		return "darwin"
	}
	return ""
}

// WriteCloudInitISO writes raw ISO bytes to <vmsDir>/<name>/cloud-init.iso
// and returns the resulting path. It must be called after CloneVM has created
// the VM directory.
func (a *Adapter) WriteCloudInitISO(name string, data []byte) (string, error) {
	isoPath := filepath.Join(a.vmDir(name), "cloud-init.iso")
	if err := os.WriteFile(isoPath, data, 0o644); err != nil {
		return "", fmt.Errorf("write cloud-init ISO for %s: %w", name, err)
	}
	return isoPath, nil
}
