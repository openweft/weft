package weft

// pod_specs.go owns the in-memory store of GuestPodPlane PodSpecs.
// A PodSpec is the operator's desired-state description of what
// containers, mounts, restart policy, etc. should be running inside
// a microVM. The host serves it to the guest on GuestPodPlane.Attach
// via the HelloAck frame ; weft-microvm-agent's containers
// subscriber reconciles toward it.
//
// Persistence layout :
//   * In-memory map keyed by pod_id (= VM.Name on the wire).
//   * On every SetPodSpec mutation the whole registry is rendered as
//     an HCL document — one `podspec "<pod_id>" { json = "..." }`
//     block per entry — and written via a temp+rename atomic replace
//     to <stateDir>/podspecs.hcl.
//   * At Adapter construction time initPodSpecs reads the file back
//     in (or starts empty if it doesn't exist).
//
// The protojson-encoded spec is stored verbatim as the `json`
// attribute so the on-disk format survives proto schema additions
// without breaking the deserialiser : every new optional field the
// PodSpec gains just shows up in the next protojson round-trip.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/hashicorp/hcl/v2/hclsimple"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
	"google.golang.org/protobuf/encoding/protojson"

	guestv1 "github.com/openweft/weft-proto/guestv1"
)

// podSpecRegistry is the pod_id → *guestv1.PodSpec map the
// GuestPodPlane handler reads at Hello time. Goroutine-safe ; the
// only mutator is SetPodSpec, the only reader is PodSpec. Both
// surface via the Adapter so callers don't peek through the lock.
type podSpecRegistry struct {
	mu   sync.RWMutex
	m    map[string]*guestv1.PodSpec
	path string // <stateDir>/podspecs.hcl ; empty disables persistence
}

func newPodSpecRegistry() *podSpecRegistry {
	return &podSpecRegistry{m: make(map[string]*guestv1.PodSpec)}
}

// podSpecsDoc is the HCL surface for the on-disk store. One block per
// pod_id ; the spec rides as a protojson string so the file stays
// readable in text editors and survives proto schema additions.
type podSpecsDoc struct {
	Specs []podSpecBlock `hcl:"podspec,block"`
}

type podSpecBlock struct {
	PodID string `hcl:",label"`
	JSON  string `hcl:"json"`
}

// initPodSpecs is the lifecycle hook the Adapter constructor calls.
// Reads <stateDir>/podspecs.hcl when it exists, otherwise starts
// with an empty registry. Errors are logged + non-fatal — the agent
// still serves with an empty store, matching the older in-memory-only
// behaviour.
func (a *Adapter) initPodSpecs() {
	reg := newPodSpecRegistry()
	if a.stateDir != "" {
		reg.path = filepath.Join(a.stateDir, "podspecs.hcl")
		if err := reg.loadFromDisk(); err != nil {
			fmt.Fprintf(os.Stderr, "weft: load podspecs registry: %v\n", err)
		}
	}
	a.podSpecs = reg
}

// loadFromDisk decodes the on-disk HCL doc into the in-memory map.
// Missing file = empty registry (not an error).
func (r *podSpecRegistry) loadFromDisk() error {
	if r.path == "" {
		return nil
	}
	blob, err := os.ReadFile(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", r.path, err)
	}
	if len(blob) == 0 {
		return nil
	}
	var doc podSpecsDoc
	if err := hclsimple.Decode("podspecs.hcl", blob, nil, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", r.path, err)
	}
	for _, b := range doc.Specs {
		if b.PodID == "" || b.JSON == "" {
			continue
		}
		spec := &guestv1.PodSpec{}
		if err := protojson.Unmarshal([]byte(b.JSON), spec); err != nil {
			fmt.Fprintf(os.Stderr, "weft: podspec %q: protojson decode: %v\n", b.PodID, err)
			continue
		}
		spec.PodId = b.PodID
		r.m[b.PodID] = spec
	}
	return nil
}

// saveLocked dumps the full registry to disk (temp+rename). Caller
// must hold r.mu (write or read lock).
func (r *podSpecRegistry) saveLocked() error {
	if r.path == "" {
		return nil
	}
	f := hclwrite.NewEmptyFile()
	body := f.Body()
	ids := make([]string, 0, len(r.m))
	for id := range r.m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	marshaller := protojson.MarshalOptions{UseProtoNames: true}
	for _, id := range ids {
		spec := r.m[id]
		raw, err := marshaller.Marshal(spec)
		if err != nil {
			return fmt.Errorf("marshal podspec %q: %w", id, err)
		}
		block := body.AppendNewBlock("podspec", []string{id})
		block.Body().SetAttributeValue("json", cty.StringVal(string(raw)))
		body.AppendNewline()
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(r.path), err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(r.path), ".podspecs-*.hcl")
	if err != nil {
		return fmt.Errorf("create temp %s: %w", r.path, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(f.Bytes()); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, r.path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename %s → %s: %w", tmpName, r.path, err)
	}
	return nil
}

// SetPodSpec records the operator's desired pod state for the
// given pod_id (= VM.Name on the wire contract). Passing nil
// evicts the entry — gives the operator a clean "drop the spec
// and let the guest stay attached without one" path. After every
// mutation the full registry is flushed to disk so a daemon restart
// rehydrates the same view.
func (a *Adapter) SetPodSpec(podID string, spec *guestv1.PodSpec) {
	if a.podSpecs == nil || podID == "" {
		return
	}
	a.podSpecs.mu.Lock()
	defer a.podSpecs.mu.Unlock()
	if spec == nil {
		delete(a.podSpecs.m, podID)
	} else {
		// Defensive copy of the pod_id field so a caller mutating spec
		// after SetPodSpec doesn't drift the on-wire representation.
		// The rest of the spec is value-passed to the guest via gRPC
		// so internal mutation doesn't leak — keep this minimal.
		spec.PodId = podID
		a.podSpecs.m[podID] = spec
	}
	if err := a.podSpecs.saveLocked(); err != nil {
		fmt.Fprintf(os.Stderr, "weft: persist podspecs: %v\n", err)
	}
}

// PodSpec returns the operator-supplied desired state for the pod
// plus a bool indicating whether one has been published yet.
// Unknown pods → (nil, false), which the GuestPodPlane handler
// surfaces as an empty PodSpec on HelloAck so the guest still
// receives a valid ack.
func (a *Adapter) PodSpec(podID string) (*guestv1.PodSpec, bool) {
	if a.podSpecs == nil {
		return nil, false
	}
	a.podSpecs.mu.RLock()
	defer a.podSpecs.mu.RUnlock()
	s, ok := a.podSpecs.m[podID]
	return s, ok
}
