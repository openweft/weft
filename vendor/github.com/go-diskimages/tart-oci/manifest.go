package tartoci

// Tart OCI media types.
const (
	// MediaTypeDisk is a disk layer: an Apple LZ4 frame of at most 512 MiB
	// (uncompressed) of the disk image.
	MediaTypeDisk = "application/vnd.cirruslabs.tart.disk.v2"
	// MediaTypeConfig is the Tart VM config layer (JSON).
	MediaTypeConfig = "application/vnd.cirruslabs.tart.config.v1"
	// MediaTypeNVRAM is the VM NVRAM store layer (raw bytes).
	MediaTypeNVRAM = "application/vnd.cirruslabs.tart.nvram.v1"

	// MediaTypeOCIManifest is the OCI image manifest media type.
	MediaTypeOCIManifest = "application/vnd.oci.image.manifest.v1+json"
	// MediaTypeOCIIndex is the OCI image index (manifest list) media type.
	MediaTypeOCIIndex = "application/vnd.oci.image.index.v1+json"
	// MediaTypeDockerManifest is Docker's schema-2 manifest media type, accepted
	// for registries that negotiate it.
	MediaTypeDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
)

// Tart OCI annotation keys (per-layer unless noted).
const (
	// AnnUncompressedSize is a disk layer's decompressed length in bytes.
	AnnUncompressedSize = "org.cirruslabs.tart.uncompressed-size"
	// AnnUncompressedDigest is a disk layer's decompressed sha256 content digest.
	AnnUncompressedDigest = "org.cirruslabs.tart.uncompressed-content-digest"
	// AnnUncompressedDiskSize is the manifest-level total decompressed disk size.
	AnnUncompressedDiskSize = "org.cirruslabs.tart.uncompressed-disk-size"
)

// Descriptor is an OCI content descriptor.
type Descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Manifest is a Tart OCI image manifest.
type Manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType,omitempty"`
	Config        Descriptor        `json:"config"`
	Layers        []Descriptor      `json:"layers"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

// index is an OCI image index (manifest list), used only to resolve to a single
// image manifest. Tart normally pushes an image manifest directly.
type index struct {
	Manifests []Descriptor `json:"manifests"`
}

// layersOf returns the manifest layers whose media type equals mt, in order.
func (m Manifest) layersOf(mt string) []Descriptor {
	var out []Descriptor
	for _, l := range m.Layers {
		if l.MediaType == mt {
			out = append(out, l)
		}
	}
	return out
}

// DiskLayers returns the disk.v2 layers in manifest (disk) order.
func (m Manifest) DiskLayers() []Descriptor { return m.layersOf(MediaTypeDisk) }
