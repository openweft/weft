package weft

// guest_secret_refs.go is the cluster-state map from a GuestSecrets
// secret ID (the value of a *_ref field in a PodSpec) to the secretsource
// reference the HOST resolves, and the control-plane glue that turns a
// PodSpec into the per-pod grant table consumed by the GuestSecrets
// service (guest_secrets.go).
//
// The map holds REFERENCES ONLY. No function here accepts or returns a
// secret value; a reference names where the value lives, and is
// validated against a scheme allowlist before it is stored. Only an
// admin can write the map (the RPC handler gates on RequireAdmin).
//
// Like the grant table, the map lives in cluster state (kvStorageFactory,
// name "guestsecretrefs") and in memory when there is no KV factory; it
// is never written to the local disk.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-authn/secretsource"

	guestv1 "github.com/openweft/weft-proto/guestv1"
)

const guestSecretRefsKVName = "guestsecretrefs"

// MaxGuestSecretRefLen bounds a stored reference.
const MaxGuestSecretRefLen = 1024

// GuestSecretRefSchemes is the allowlist of secretsource schemes an
// operator may map an ID to. It is every scheme secretsource.Parse
// supports; a scheme added to secretsource later is NOT accepted until it
// is added here on purpose.
var GuestSecretRefSchemes = map[string]bool{
	"file":          true,
	"keychain":      true,
	"secretservice": true,
	"keepassxc":     true,
	"systemd-creds": true,
}

// noVsockKinds are the driver kinds that have no vsock channel: a pod
// carrying a secret must not be placed on them (their guests cannot fetch
// by ID, and the design forbids falling back to an inline value).
var noVsockKinds = map[string]bool{"vmd": true, "wasm": true}

// ErrSecretsNeedVsock is returned when a pod that carries a secret is
// bound to a driver without a vsock channel.
var ErrSecretsNeedVsock = errors.New("guest secrets: this VM's driver has no vsock channel; a pod carrying a secret (inline or by reference) cannot run on it")

// needVsockErr answers ErrSecretsNeedVsock for a driver kind without a
// vsock channel, nil for any other.
func needVsockErr(kind string) error {
	if noVsockKinds[kind] {
		return fmt.Errorf("%w (driver %q)", ErrSecretsNeedVsock, kind)
	}
	return nil
}

// GuestSecretRefRecord is one entry of the map.
type GuestSecretRefRecord struct {
	Ref     string `json:"ref"`
	Version uint64 `json:"version"`
}

// ValidateGuestSecretRef checks a reference before it is stored: bounded,
// printable, a parseable secretsource reference, an allowlisted scheme,
// and for file: an absolute, clean path.
func ValidateGuestSecretRef(ref string) error {
	if ref == "" || len(ref) > MaxGuestSecretRefLen {
		return fmt.Errorf("guest secrets: reference must be 1..%d bytes", MaxGuestSecretRefLen)
	}
	for i := 0; i < len(ref); i++ {
		if c := ref[i]; c < 0x20 || c == 0x7f {
			return errors.New("guest secrets: reference contains a control character")
		}
	}
	parsed, err := secretsource.Parse(ref)
	if err != nil {
		return fmt.Errorf("guest secrets: %w", err)
	}
	if !GuestSecretRefSchemes[parsed.Scheme] {
		return fmt.Errorf("guest secrets: scheme %q is not allowed", parsed.Scheme)
	}
	if parsed.Scheme == "file" {
		if !filepath.IsAbs(parsed.Path) || filepath.Clean(parsed.Path) != parsed.Path {
			return errors.New("guest secrets: file: reference must be an absolute, clean path")
		}
	}
	return nil
}

func (a *Adapter) guestSecretRefsKV() KVStorage {
	if a.kvStorageFactory == nil {
		return nil
	}
	return a.kvStorageFactory(guestSecretRefsKVName)
}

// SetGuestSecretRef maps id to ref and returns the new rotation version
// (previous + 1, starting at 1). Pods whose spec uses id are re-granted so
// they see the new reference and version.
func (a *Adapter) SetGuestSecretRef(id, ref string) (uint64, error) {
	if !ValidGuestSecretID(id) {
		return 0, fmt.Errorf("guest secrets: invalid secret id %q", id)
	}
	if err := ValidateGuestSecretRef(ref); err != nil {
		return 0, err
	}
	a.guestSecretsMu.Lock()
	prev, _, err := a.guestSecretRefLocked(id)
	if err != nil {
		a.guestSecretsMu.Unlock()
		return 0, err
	}
	rec := GuestSecretRefRecord{Ref: ref, Version: prev.Version + 1}
	if kv := a.guestSecretRefsKV(); kv != nil {
		blob, _ := json.Marshal(rec) // strings and integers: cannot fail
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err = kv.PutOne(ctx, id, blob)
		cancel()
	} else {
		if a.guestSecretRefsLocal == nil {
			a.guestSecretRefsLocal = make(map[string]GuestSecretRefRecord)
		}
		a.guestSecretRefsLocal[id] = rec
	}
	a.guestSecretsMu.Unlock()
	if err != nil {
		return 0, err
	}
	a.regrantPodsUsing(id)
	return rec.Version, nil
}

// DeleteGuestSecretRef removes the mapping of id. Pods using it lose the
// grant (a fetch is then denied) on their next re-grant; it is applied to
// them immediately.
func (a *Adapter) DeleteGuestSecretRef(id string) error {
	if !ValidGuestSecretID(id) {
		return fmt.Errorf("guest secrets: invalid secret id %q", id)
	}
	a.guestSecretsMu.Lock()
	var err error
	if kv := a.guestSecretRefsKV(); kv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err = kv.DeleteOne(ctx, id)
		cancel()
	} else {
		delete(a.guestSecretRefsLocal, id)
	}
	a.guestSecretsMu.Unlock()
	if err != nil {
		return err
	}
	a.regrantPodsUsing(id)
	return nil
}

// ListGuestSecretRefs returns the whole map.
func (a *Adapter) ListGuestSecretRefs() (map[string]GuestSecretRefRecord, error) {
	a.guestSecretsMu.RLock()
	defer a.guestSecretsMu.RUnlock()
	out := make(map[string]GuestSecretRefRecord)
	if kv := a.guestSecretRefsKV(); kv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		all, err := kv.List(ctx)
		if err != nil {
			return nil, err
		}
		for k, blob := range all {
			var rec GuestSecretRefRecord
			if json.Unmarshal(blob, &rec) != nil {
				continue
			}
			out[strings.TrimPrefix(k, kv.Prefix())] = rec
		}
		return out, nil
	}
	for k, v := range a.guestSecretRefsLocal {
		out[k] = v
	}
	return out, nil
}

// guestSecretRefLocked reads one entry. The caller holds guestSecretsMu.
func (a *Adapter) guestSecretRefLocked(id string) (GuestSecretRefRecord, bool, error) {
	if kv := a.guestSecretRefsKV(); kv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		blob, err := kv.GetOne(ctx, id)
		if err != nil {
			return GuestSecretRefRecord{}, false, err
		}
		if len(blob) == 0 {
			return GuestSecretRefRecord{}, false, nil
		}
		var rec GuestSecretRefRecord
		if err := json.Unmarshal(blob, &rec); err != nil {
			return GuestSecretRefRecord{}, false, err
		}
		return rec, true, nil
	}
	rec, ok := a.guestSecretRefsLocal[id]
	return rec, ok, nil
}

// PodSecretIDs returns the sorted, de-duplicated secret IDs a PodSpec
// refers to (WireGuard private key, CubeFS access/secret keys, also inside
// share mounts).
func PodSecretIDs(spec *guestv1.PodSpec) []string {
	if spec == nil {
		return nil
	}
	set := map[string]bool{}
	add := func(id string) {
		if id != "" {
			set[id] = true
		}
	}
	if w := spec.GetWireguard(); w != nil {
		add(w.GetPrivateKeyRef())
	}
	for _, sm := range spec.GetShareMounts() {
		if c := sm.GetCubefs(); c != nil {
			add(c.GetAccessKeyRef())
			add(c.GetSecretKeyRef())
		}
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// PodCarriesInlineSecret reports whether a PodSpec holds an inline key.
func PodCarriesInlineSecret(spec *guestv1.PodSpec) bool {
	if spec == nil {
		return false
	}
	if spec.GetWireguard().GetPrivateKey() != "" {
		return true
	}
	for _, sm := range spec.GetShareMounts() {
		if c := sm.GetCubefs(); c != nil && (c.GetAccessKey() != "" || c.GetSecretKey() != "") {
			return true
		}
	}
	return false
}

// checkPodSecrets validates the secret side of a spec for podID and
// returns the grant table to install: every ID must be valid and mapped;
// a ref next to its inline field is refused; a pod on a driver without a
// vsock channel may carry no secret at all.
func (a *Adapter) checkPodSecrets(podID string, spec *guestv1.PodSpec) (map[string]GuestSecretGrant, error) {
	ids := PodSecretIDs(spec)
	if len(ids) == 0 && !PodCarriesInlineSecret(spec) {
		return nil, nil
	}
	if err := needVsockErr(a.LookupKindForVM(podID)); err != nil {
		return nil, err
	}
	if w := spec.GetWireguard(); w.GetPrivateKey() != "" && w.GetPrivateKeyRef() != "" {
		return nil, errors.New("guest secrets: wireguard sets both private_key and private_key_ref")
	}
	for _, sm := range spec.GetShareMounts() {
		c := sm.GetCubefs()
		if c == nil {
			continue
		}
		if (c.GetAccessKey() != "" && c.GetAccessKeyRef() != "") || (c.GetSecretKey() != "" && c.GetSecretKeyRef() != "") {
			return nil, errors.New("guest secrets: a cubefs mount sets a key and its _ref together")
		}
	}
	grants := make(map[string]GuestSecretGrant, len(ids))
	a.guestSecretsMu.RLock()
	defer a.guestSecretsMu.RUnlock()
	for _, id := range ids {
		if !ValidGuestSecretID(id) {
			return nil, fmt.Errorf("guest secrets: invalid secret id %q", id)
		}
		rec, ok, err := a.guestSecretRefLocked(id)
		if err != nil {
			return nil, fmt.Errorf("guest secrets: read the reference of %q: %w", id, err)
		}
		if !ok {
			return nil, fmt.Errorf("guest secrets: secret id %q has no reference (an admin must SetGuestSecretRef first)", id)
		}
		grants[id] = GuestSecretGrant{Ref: rec.Ref, Version: rec.Version}
	}
	return grants, nil
}

// PublishPodSpec stores spec for podID and writes the pod's grant table,
// both or neither: a spec naming an unmapped ID, an invalid ID, both an
// inline key and its ref, or a secret on a driver without vsock is
// refused and nothing is stored. A nil spec removes the spec and revokes
// the grants.
func (a *Adapter) PublishPodSpec(podID string, spec *guestv1.PodSpec) error {
	if podID == "" {
		return errors.New("guest secrets: empty pod id")
	}
	if spec == nil {
		a.SetPodSpec(podID, nil)
		return nil
	}
	grants, err := a.checkPodSecrets(podID, spec)
	if err != nil {
		return err
	}
	if len(grants) > 0 {
		if err := a.GrantGuestSecrets(podID, grants); err != nil {
			return err
		}
	} else {
		a.RevokeGuestSecrets(podID)
	}
	a.setPodSpecKeepGrants(podID, spec)
	return nil
}

// regrantPodsUsing rewrites the grants of every pod whose stored spec
// refers to id, after the mapping of id changed.
func (a *Adapter) regrantPodsUsing(id string) {
	if a.podSpecs == nil {
		return
	}
	a.podSpecs.mu.RLock()
	var pods []string
	for pod, spec := range a.podSpecs.m {
		for _, used := range PodSecretIDs(spec) {
			if used == id {
				pods = append(pods, pod)
				break
			}
		}
	}
	a.podSpecs.mu.RUnlock()
	for _, pod := range pods {
		spec, _ := a.PodSpec(pod)
		grants, err := a.checkPodSecrets(pod, spec)
		if err != nil {
			// The mapping was deleted: the pod keeps its other grants
			// only if every ID still resolves, otherwise none.
			a.RevokeGuestSecrets(pod)
			fmt.Fprintf(os.Stderr, "weft: guest secrets of %s revoked: %v\n", pod, err)
			continue
		}
		if err := a.GrantGuestSecrets(pod, grants); err != nil {
			fmt.Fprintf(os.Stderr, "weft: re-grant guest secrets of %s: %v\n", pod, err)
		}
	}
}
