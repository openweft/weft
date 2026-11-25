package weft

// guest_secrets.go is the authorization table behind the GuestSecrets
// vsock service: for each pod (VM name) the set of secret IDs it may
// fetch, each bound to the secretsource reference the HOST resolves and
// the rotation version of the record. The table lives in cluster state
// (kvStorageFactory, name "guestsecrets", key = pod id) so every
// control-plane replica agrees, and is written by the control plane when
// the pod is created and removed when the pod goes away. A guest never
// supplies a reference: it names an ID, the host looks the ID up here.
//
// Without a KV factory (single-host dev, tests) the table is held in
// memory; it is never written to the local disk, since references name
// where a secret lives.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/go-authn/secretsource"
)

// guestSecretsKVName is the kvStorageFactory name of the table.
const guestSecretsKVName = "guestsecrets"

// MaxGuestSecretIDLen bounds a secret ID (and its wire form).
const MaxGuestSecretIDLen = 63

// MaxGuestSecretsPerPod bounds the table of one pod.
const MaxGuestSecretsPerPod = 64

// GuestSecretGrant is one entry of a pod's table.
type GuestSecretGrant struct {
	// Ref is a secretsource reference ("file:/…", "keychain:…", …).
	Ref string `json:"ref"`
	// Version is the control plane's rotation counter for the secret.
	Version uint64 `json:"version"`
}

type guestSecretRecord struct {
	Secrets map[string]GuestSecretGrant `json:"secrets"`
}

// ValidGuestSecretID reports whether id matches [a-z0-9][a-z0-9._-]{0,62}.
func ValidGuestSecretID(id string) bool {
	if id == "" || len(id) > MaxGuestSecretIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case i > 0 && (c == '.' || c == '_' || c == '-'):
		default:
			return false
		}
	}
	return true
}

// GrantGuestSecrets replaces the table of podID with grants. Every ID
// must be valid and every reference must parse; nothing is stored when
// any entry is refused.
func (a *Adapter) GrantGuestSecrets(podID string, grants map[string]GuestSecretGrant) error {
	if podID == "" {
		return errors.New("guest secrets: empty pod id")
	}
	if len(grants) > MaxGuestSecretsPerPod {
		return fmt.Errorf("guest secrets: %d entries exceed the limit of %d", len(grants), MaxGuestSecretsPerPod)
	}
	rec := guestSecretRecord{Secrets: make(map[string]GuestSecretGrant, len(grants))}
	ids := make([]string, 0, len(grants))
	for id := range grants {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		g := grants[id]
		if !ValidGuestSecretID(id) {
			return fmt.Errorf("guest secrets: invalid secret id %q", id)
		}
		if _, err := secretsource.Parse(g.Ref); err != nil {
			return fmt.Errorf("guest secrets: secret %q: %w", id, err)
		}
		rec.Secrets[id] = g
	}
	a.guestSecretsMu.Lock()
	defer a.guestSecretsMu.Unlock()
	if kv := a.guestSecretsKV(); kv != nil {
		blob, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return kv.PutOne(ctx, podID, blob)
	}
	if a.guestSecretsLocal == nil {
		a.guestSecretsLocal = make(map[string]guestSecretRecord)
	}
	a.guestSecretsLocal[podID] = rec
	return nil
}

// RevokeGuestSecrets removes the table of podID. Safe on an unknown pod.
func (a *Adapter) RevokeGuestSecrets(podID string) {
	if podID == "" {
		return
	}
	a.guestSecretsMu.Lock()
	defer a.guestSecretsMu.Unlock()
	delete(a.guestSecretsLocal, podID)
	if kv := a.guestSecretsKV(); kv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := kv.DeleteOne(ctx, podID); err != nil {
			fmt.Fprintf(os.Stderr, "weft: revoke guest secrets of %s: %v\n", podID, err)
		}
	}
}

// GuestSecretGrantFor returns the grant of id in the table of podID.
// (zero, false, nil) means "not allowed" (no table, no such ID); a
// non-nil error means the table could not be read and the caller must
// fail closed.
func (a *Adapter) GuestSecretGrantFor(podID, id string) (GuestSecretGrant, bool, error) {
	if podID == "" || !ValidGuestSecretID(id) {
		return GuestSecretGrant{}, false, nil
	}
	a.guestSecretsMu.RLock()
	defer a.guestSecretsMu.RUnlock()
	if kv := a.guestSecretsKV(); kv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		blob, err := kv.GetOne(ctx, podID)
		if err != nil {
			return GuestSecretGrant{}, false, err
		}
		if len(blob) == 0 {
			return GuestSecretGrant{}, false, nil
		}
		var rec guestSecretRecord
		if err := json.Unmarshal(blob, &rec); err != nil {
			return GuestSecretGrant{}, false, err
		}
		g, ok := rec.Secrets[id]
		return g, ok, nil
	}
	g, ok := a.guestSecretsLocal[podID].Secrets[id]
	return g, ok, nil
}

func (a *Adapter) guestSecretsKV() KVStorage {
	if a.kvStorageFactory == nil {
		return nil
	}
	return a.kvStorageFactory(guestSecretsKVName)
}

// PodForCID returns the pod bound to the guest CID the HOST assigned
// (in-memory binding first, then the VM inventory of this host). Several
// pods on one CID, or none: unbound, so the caller denies.
func (a *Adapter) PodForCID(cid uint32) (string, bool) {
	if !IsVsockGuestCID(cid) {
		return "", false
	}
	if a.podCIDs != nil {
		if name, ok := a.podCIDs.pod(cid); ok {
			return name, true
		}
	}
	if a.vmReg == nil {
		return "", false
	}
	return inventoryPodForCID(a.vmReg.list(), cid, a.localHostUUID())
}

func inventoryPodForCID(vms []VM, cid uint32, hostUUID string) (string, bool) {
	if hostUUID == "" {
		return "", false
	}
	var found string
	for _, v := range vms {
		if v.VsockCID != cid || v.HostUUID != hostUUID || v.Name == "" {
			continue
		}
		if found != "" && found != v.Name {
			return "", false
		}
		found = v.Name
	}
	return found, found != ""
}

// pod returns the single pod registered with cid.
func (r *podCIDRegistry) pod(cid uint32) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var found string
	for name, c := range r.m {
		if c != cid {
			continue
		}
		if found != "" {
			return "", false
		}
		found = name
	}
	return found, found != ""
}
