// configfile.go — operator-facing HCL read/write helpers for the
// `federation { … }` block in weft.hcl. `weft federation join` and
// `weft federation leave` round-trip a single block in place
// without disturbing other top-level config (oidc, storage, proxy,
// etc.). The hclwrite-based merge preserves comments and ordering
// inside untouched blocks ; only the federation block is rewritten
// from scratch.
//
// The on-disk shape :
//
//	federation {
//	  listen        = ":9102"
//	  poll_interval = "30s"
//	  peer_stale_ttl = "5m"
//
//	  peers = [
//	    "https://peer-a.example.com:9102",
//	    "https://peer-b.example.com:9102",
//	  ]
//
//	  peer_keys = {
//	    "https://peer-a.example.com:9102" = "base64-encoded-ed25519-pubkey"
//	    "https://peer-b.example.com:9102" = "base64-encoded-ed25519-pubkey"
//	  }
//	}
//
// peer_keys maps peer URL → base64 ed25519 public key. Operators pre-
// share keys out of band (signed email, ssh-copy-id-style script,
// etc.) ; `weft federation join` writes the pair the operator passes
// on the CLI. Storing pubkeys (not URLs alone) in the config means
// a rogue cluster can't impersonate a peer by hijacking DNS.

package federation

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

// FileBlock is the in-memory shape of the HCL `federation { }`
// block. Pointers distinguish "absent" from "present and empty",
// matching the precedence rule in cmd/weft/config.go (CLI > env >
// HCL > built-in default).
type FileBlock struct {
	Listen       *string           // empty / nil → disabled
	PollInterval *time.Duration    // nil → DefaultPollInterval
	PeerStaleTTL *time.Duration    // nil → DefaultPeerStaleTTL
	Peers        []string          // peer URLs (mirrors the slice below)
	PeerKeys     map[string]string // url → base64(ed25519 pubkey)
}

// Validate runs cheap shape checks on the decoded block. Operators
// who hand-edit weft.hcl get a clear error instead of a poller that
// silently refuses to start.
func (b *FileBlock) Validate() error {
	if b == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(b.Peers))
	for i, u := range b.Peers {
		if u == "" {
			return fmt.Errorf("federation: peers[%d] is empty", i)
		}
		if _, dup := seen[u]; dup {
			return fmt.Errorf("federation: duplicate peer %q", u)
		}
		seen[u] = struct{}{}
		key, ok := b.PeerKeys[u]
		if !ok {
			return fmt.Errorf("federation: peer %q has no entry in peer_keys", u)
		}
		raw, err := base64.StdEncoding.DecodeString(key)
		if err != nil {
			return fmt.Errorf("federation: peer %q pubkey: %w", u, err)
		}
		if len(raw) != ed25519.PublicKeySize {
			return fmt.Errorf("federation: peer %q pubkey must be %d bytes, got %d", u, ed25519.PublicKeySize, len(raw))
		}
	}
	return nil
}

// PeerConfigs returns the decoded peers as PeerConfig slices ready
// to hand to NewPoller. Returns nil when no peers are configured —
// the agent boot wiring uses that as the "don't spawn the poller"
// signal.
func (b *FileBlock) PeerConfigs() ([]PeerConfig, error) {
	if b == nil || len(b.Peers) == 0 {
		return nil, nil
	}
	if err := b.Validate(); err != nil {
		return nil, err
	}
	out := make([]PeerConfig, 0, len(b.Peers))
	for _, u := range b.Peers {
		raw, _ := base64.StdEncoding.DecodeString(b.PeerKeys[u])
		out = append(out, PeerConfig{
			URL:       u,
			PublicKey: ed25519.PublicKey(raw),
		})
	}
	return out, nil
}

// ReadFileBlock decodes the federation block from path. Returns a
// nil block when the file is missing — federation is opt-in, an
// absent file is not an error. A missing federation { } block in an
// otherwise-valid weft.hcl returns (nil, nil) too. Wraps hcl errors
// with the file path for operator legibility.
func ReadFileBlock(path string) (*FileBlock, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("federation: read %s: %w", path, err)
	}
	parser := hclparse.NewParser()
	f, diags := parser.ParseHCL(src, path)
	if diags.HasErrors() {
		return nil, fmt.Errorf("federation: parse %s: %s", path, diags.Error())
	}
	return decodeFederationBlock(f.Body)
}

func decodeFederationBlock(body hcl.Body) (*FileBlock, error) {
	content, _, _ := body.PartialContent(&hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{{Type: "federation"}},
	})
	var fb *FileBlock
	for _, blk := range content.Blocks {
		if blk.Type != "federation" {
			continue
		}
		if fb != nil {
			return nil, errors.New("federation: duplicate federation { } block")
		}
		decoded, err := decodeFederationBody(blk.Body)
		if err != nil {
			return nil, err
		}
		fb = decoded
	}
	return fb, nil
}

func decodeFederationBody(body hcl.Body) (*FileBlock, error) {
	attrs, diags := body.JustAttributes()
	if diags.HasErrors() {
		return nil, fmt.Errorf("federation: %s", diags.Error())
	}
	out := &FileBlock{}
	for name, attr := range attrs {
		val, vdiags := attr.Expr.Value(nil)
		if vdiags.HasErrors() {
			return nil, fmt.Errorf("federation: %s: %s", name, vdiags.Error())
		}
		switch name {
		case "listen":
			s := val.AsString()
			out.Listen = &s
		case "poll_interval":
			d, err := time.ParseDuration(val.AsString())
			if err != nil {
				return nil, fmt.Errorf("federation: poll_interval: %w", err)
			}
			out.PollInterval = &d
		case "peer_stale_ttl":
			d, err := time.ParseDuration(val.AsString())
			if err != nil {
				return nil, fmt.Errorf("federation: peer_stale_ttl: %w", err)
			}
			out.PeerStaleTTL = &d
		case "peers":
			if !val.Type().IsListType() && !val.Type().IsTupleType() {
				return nil, errors.New("federation: peers must be a list of strings")
			}
			for it := val.ElementIterator(); it.Next(); {
				_, v := it.Element()
				out.Peers = append(out.Peers, v.AsString())
			}
		case "peer_keys":
			if !val.Type().IsObjectType() && !val.Type().IsMapType() {
				return nil, errors.New("federation: peer_keys must be a map of url → pubkey")
			}
			out.PeerKeys = make(map[string]string, val.LengthInt())
			for it := val.ElementIterator(); it.Next(); {
				k, v := it.Element()
				out.PeerKeys[k.AsString()] = v.AsString()
			}
		default:
			return nil, fmt.Errorf("federation: unknown attribute %q", name)
		}
	}
	return out, nil
}

// WriteFileBlock writes (or rewrites) the `federation { }` block of
// the HCL file at path. Other top-level blocks are preserved
// verbatim — we use hclwrite's block-find / drop-block / append
// flow rather than re-serialising from a typed struct. Operators
// who customise their weft.hcl don't lose comments outside the
// federation block.
//
// When path doesn't exist the function creates a fresh file
// containing only the federation block — that's the right shape
// for an operator who runs `weft federation join` before they ever
// wrote a weft.hcl. Parent directory is created with 0700 (config
// files should never be world-readable).
//
// The block is rendered with peers / peer_keys sorted by URL so
// repeated joins produce a deterministic file (git diffs reviewable,
// idempotency provable).
func WriteFileBlock(path string, fb *FileBlock) error {
	if fb == nil {
		return errors.New("federation: nil FileBlock")
	}
	if err := fb.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(dirOf(path), 0o700); err != nil {
		return fmt.Errorf("federation: ensure config dir: %w", err)
	}
	var f *hclwrite.File
	if existing, err := os.ReadFile(path); err == nil {
		parsed, diags := hclwrite.ParseConfig(existing, path, hcl.Pos{Line: 1, Column: 1})
		if diags.HasErrors() {
			return fmt.Errorf("federation: parse %s for rewrite: %s", path, diags.Error())
		}
		f = parsed
	} else if os.IsNotExist(err) {
		f = hclwrite.NewEmptyFile()
	} else {
		return fmt.Errorf("federation: read %s for rewrite: %w", path, err)
	}
	root := f.Body()
	// Drop any existing federation block(s) — defends against an
	// accidentally-duplicated block in a hand-edited config too.
	for _, blk := range root.Blocks() {
		if blk.Type() == "federation" {
			root.RemoveBlock(blk)
		}
	}
	if !fb.IsEmpty() {
		writeFederationBlock(root, fb)
	}
	return os.WriteFile(path, f.Bytes(), 0o600)
}

// IsEmpty reports whether the block carries any operator-set value.
// A FileBlock whose every field is the zero / nil value serializes
// to nothing — writing weft.hcl with an empty federation { } block
// would be noise.
func (b *FileBlock) IsEmpty() bool {
	if b == nil {
		return true
	}
	return b.Listen == nil && b.PollInterval == nil && b.PeerStaleTTL == nil && len(b.Peers) == 0 && len(b.PeerKeys) == 0
}

func writeFederationBlock(root *hclwrite.Body, fb *FileBlock) {
	blk := root.AppendNewBlock("federation", nil)
	body := blk.Body()
	if fb.Listen != nil {
		body.SetAttributeValue("listen", cty.StringVal(*fb.Listen))
	}
	if fb.PollInterval != nil {
		body.SetAttributeValue("poll_interval", cty.StringVal(fb.PollInterval.String()))
	}
	if fb.PeerStaleTTL != nil {
		body.SetAttributeValue("peer_stale_ttl", cty.StringVal(fb.PeerStaleTTL.String()))
	}
	if len(fb.Peers) > 0 {
		sorted := append([]string(nil), fb.Peers...)
		sort.Strings(sorted)
		peerVals := make([]cty.Value, len(sorted))
		for i, p := range sorted {
			peerVals[i] = cty.StringVal(p)
		}
		body.SetAttributeValue("peers", cty.ListVal(peerVals))
	}
	if len(fb.PeerKeys) > 0 {
		// cty.ObjectVal preserves insertion order ; render with
		// keys sorted so diffs stay stable.
		keys := make([]string, 0, len(fb.PeerKeys))
		for k := range fb.PeerKeys {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		m := make(map[string]cty.Value, len(keys))
		for _, k := range keys {
			m[k] = cty.StringVal(fb.PeerKeys[k])
		}
		body.SetAttributeValue("peer_keys", cty.ObjectVal(m))
	}
}

func dirOf(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			return p[:i]
		}
	}
	return "."
}

// AddPeer mutates fb to include (url, pubkey). Idempotent : calling
// twice with the same url is a no-op except for pubkey rotation,
// where the new key replaces the old one (operator intent — they
// re-ran join after a key roll). The pubkey is the base64 encoding
// of a 32-byte ed25519 public key.
func (b *FileBlock) AddPeer(url, pubkeyB64 string) error {
	if url == "" {
		return errors.New("federation: peer url is required")
	}
	raw, err := base64.StdEncoding.DecodeString(pubkeyB64)
	if err != nil {
		return fmt.Errorf("federation: decode pubkey: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return fmt.Errorf("federation: pubkey must decode to %d bytes, got %d", ed25519.PublicKeySize, len(raw))
	}
	if b.PeerKeys == nil {
		b.PeerKeys = make(map[string]string, 1)
	}
	if _, exists := b.PeerKeys[url]; !exists {
		b.Peers = append(b.Peers, url)
	}
	b.PeerKeys[url] = pubkeyB64
	return nil
}

// RemovePeer drops a peer (by URL or by name : the URL is matched
// directly, the name walks the slice looking for a member whose
// label-or-host equals it). Returns false when nothing matched —
// the CLI surfaces that as "not joined" rather than silently
// succeeding.
func (b *FileBlock) RemovePeer(nameOrURL string) bool {
	if b == nil {
		return false
	}
	if _, ok := b.PeerKeys[nameOrURL]; ok {
		delete(b.PeerKeys, nameOrURL)
		b.Peers = filterOut(b.Peers, nameOrURL)
		return true
	}
	// Fallback : a CLI user typed `leave eu-paris` ; without a
	// manifest cache to consult, the best we can do is a suffix /
	// substring match against the URL. v0.2 keeps the simple
	// exact-URL contract ; v0.3 can enrich once `weft federation
	// list` ships a stable name index.
	return false
}

func filterOut(slice []string, drop string) []string {
	out := make([]string, 0, len(slice))
	for _, s := range slice {
		if s != drop {
			out = append(out, s)
		}
	}
	return out
}
