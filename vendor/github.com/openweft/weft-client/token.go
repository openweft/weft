// Package weftclient — token.go owns the on-disk OAuth2 token cache
// for weft / weft-microvm. The cache is HCL (per [[hcl-over-json]]) so an
// operator can `cat` it, comment out a stale entry, or hand-edit
// when debugging an SSO problem.
//
// Layout: $XDG_CONFIG_HOME/weft/token.hcl (default
// ~/.config/weft/token.hcl). Mode 0600 — tokens are bearer
// credentials, treat them like SSH keys.
//
// Schema:
//
//	# weft auth token cache. Created/updated by `weft login`.
//	issuer        = "https://dex.internal.example.com"
//	client_id     = "weft"
//	access_token  = "eyJ…"
//	refresh_token = "…"   # optional
//	id_token      = "eyJ…"
//	expires_at    = "2026-05-23T12:34:56Z"
//
// Only one cached token at a time — multi-account is deferred.
package weftclient

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/hashicorp/hcl/v2/hclsimple"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
	"golang.org/x/oauth2"
)

// CachedToken is the HCL-decoded shape of token.hcl.
type CachedToken struct {
	Issuer       string `hcl:"issuer"`
	ClientID     string `hcl:"client_id"`
	AccessToken  string `hcl:"access_token"`
	RefreshToken string `hcl:"refresh_token,optional"`
	IDToken      string `hcl:"id_token,optional"`
	ExpiresAt    string `hcl:"expires_at"`
}

// TokenCachePath resolves the on-disk location of the cache.
// Honours XDG_CONFIG_HOME; falls back to $HOME/.config/weft.
func TokenCachePath() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "weft", "token.hcl")
	}
	home, _ := os.UserHomeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "weft", "token.hcl")
}

// LoadCachedToken reads + decodes the cached token. Returns
// (nil, nil) when no cache exists — that's the "not logged in"
// state, not an error.
func LoadCachedToken() (*CachedToken, error) {
	path := TokenCachePath()
	if path == "" {
		return nil, errors.New("token cache: cannot resolve config dir")
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("token cache: stat %s: %w", path, err)
	}
	var t CachedToken
	if err := hclsimple.DecodeFile(path, nil, &t); err != nil {
		return nil, fmt.Errorf("token cache: decode %s: %w", path, err)
	}
	return &t, nil
}

// SaveCachedToken writes a token to TokenCachePath() with mode
// 0600. The output is comment-headered HCL with the fields in a
// stable order so diffs across logins are noise-free.
func SaveCachedToken(t *CachedToken) error {
	if t == nil {
		return errors.New("token cache: refuse to save nil token")
	}
	path := TokenCachePath()
	if path == "" {
		return errors.New("token cache: cannot resolve config dir")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("token cache: mkdir %s: %w", filepath.Dir(path), err)
	}
	f := hclwrite.NewEmptyFile()
	body := f.Body()
	body.AppendUnstructuredTokens(hclwrite.Tokens{{Type: 0, Bytes: []byte(
		"# weft auth token cache. Created/updated by `weft login`.\n" +
			"# Mode 0600 — bearer credentials, treat like an SSH key.\n\n",
	)}})
	body.SetAttributeValue("issuer", cty.StringVal(t.Issuer))
	body.SetAttributeValue("client_id", cty.StringVal(t.ClientID))
	body.SetAttributeValue("access_token", cty.StringVal(t.AccessToken))
	if t.RefreshToken != "" {
		body.SetAttributeValue("refresh_token", cty.StringVal(t.RefreshToken))
	}
	if t.IDToken != "" {
		body.SetAttributeValue("id_token", cty.StringVal(t.IDToken))
	}
	body.SetAttributeValue("expires_at", cty.StringVal(t.ExpiresAt))
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, f.Bytes(), 0o600); err != nil {
		return fmt.Errorf("token cache: write tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("token cache: commit: %w", err)
	}
	return nil
}

// DeleteCachedToken removes the on-disk cache (used by `weft
// logout`). Missing-file is treated as success.
func DeleteCachedToken() error {
	path := TokenCachePath()
	if path == "" {
		return nil
	}
	err := os.Remove(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("token cache: remove %s: %w", path, err)
	}
	return nil
}

// Bearer returns the access token to send in
// `Authorization: Bearer <…>`. Empty when no usable token is
// cached. Does NOT check expiry — caller decides whether to
// refresh.
func (t *CachedToken) Bearer() string {
	if t == nil {
		return ""
	}
	return t.AccessToken
}

// ExpiresAtTime returns the parsed expiry time. Zero value when
// the cache field is empty or malformed (treated by callers as
// "already expired" so a refresh kicks in).
func (t *CachedToken) ExpiresAtTime() time.Time {
	if t == nil || t.ExpiresAt == "" {
		return time.Time{}
	}
	got, err := time.Parse(time.RFC3339, t.ExpiresAt)
	if err != nil {
		return time.Time{}
	}
	return got
}

// FromOAuth2 converts a freshly-issued `*oauth2.Token` (with the
// `id_token` claim already extracted) into a CachedToken ready
// to Save.
func FromOAuth2(tok *oauth2.Token, issuer, clientID, idToken string) *CachedToken {
	return &CachedToken{
		Issuer:       issuer,
		ClientID:     clientID,
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		IDToken:      idToken,
		ExpiresAt:    tok.Expiry.UTC().Format(time.RFC3339),
	}
}
