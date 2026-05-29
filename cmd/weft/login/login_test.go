package login

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/openweft/weft-client"
)

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, _ := os.Pipe()
	old := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = old }()
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	_ = w.Close()
	return <-done
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }()
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	_ = w.Close()
	return <-done
}

// withTempTokenDir reroutes XDG_CONFIG_HOME so vzclient.TokenCachePath
// points into a per-test temp dir. Avoids polluting the developer's
// real ~/.config/vzc.
func withTempTokenDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	return dir
}

// ── login ───────────────────────────────────────────────────────────────────

func TestLoginCommand_MissingIssuer(t *testing.T) {
	// Make sure the env var doesn't accidentally populate --issuer.
	t.Setenv("VZC_OIDC_ISSUER", "")
	cmd := LoginCommand()
	cmd.SetArgs([]string{})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--issuer is required") {
		t.Errorf("expected issuer-required err, got %v", err)
	}
}

func TestLoginCommand_HappyPath(t *testing.T) {
	withTempTokenDir(t)
	// Spin up a fake OIDC server that returns a device code + a
	// token on the very first poll (skipping the
	// "authorization_pending" loop because we override the poll
	// interval to be tiny).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/device/code":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code":               "dc",
				"user_code":                 "USER-CODE",
				"verification_uri":          "https://example.com/verify",
				"verification_uri_complete": "https://example.com/verify?code=USER-CODE",
				"expires_in":                300,
				"interval":                  1, // 1 second poll
			})
		case "/token":
			_ = r.ParseForm()
			if r.Form.Get("client_id") == "" || r.Form.Get("device_code") == "" {
				http.Error(w, "missing fields", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "at",
				"token_type":    "Bearer",
				"refresh_token": "rt",
				"id_token":      "idtoken-payload",
				"expires_in":    3600,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	stderr := captureStderr(t, func() {
		cmd := LoginCommand()
		cmd.SetArgs([]string{"--issuer", srv.URL, "--client-id", "vzc-test", "--scope", "openid", "--scope", "profile"})
		// 5 s should be plenty since we override interval=1 and
		// the test server replies immediately.
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(stderr, "Logged in.") {
		t.Errorf("missing 'Logged in.' in stderr: %q", stderr)
	}
	if !strings.Contains(stderr, "USER-CODE") {
		t.Errorf("missing user code in stderr: %q", stderr)
	}
	// Token should now be cached.
	tok, err := vzclient.LoadCachedToken()
	if err != nil || tok == nil {
		t.Fatalf("token not cached: err=%v tok=%v", err, tok)
	}
	if tok.AccessToken != "at" || tok.IDToken != "idtoken-payload" {
		t.Errorf("token fields: %+v", tok)
	}
}

func TestLoginCommand_DefaultClientIDFromEnv(t *testing.T) {
	withTempTokenDir(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Fail device_code so the rest of the flow is skipped — we
		// only care that the default client-id branch ran.
		if r.URL.Path == "/device/code" {
			http.Error(w, "nope", http.StatusInternalServerError)
			return
		}
	}))
	defer srv.Close()
	t.Setenv("VZC_OIDC_CLIENT_ID", "")
	cmd := LoginCommand()
	cmd.SetArgs([]string{"--issuer", srv.URL})
	_ = captureStderr(t, func() {
		err := cmd.Execute()
		if err == nil {
			t.Fatal("expected device auth error")
		}
	})
}

func TestLoginCommand_DeviceAuthError(t *testing.T) {
	withTempTokenDir(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	defer srv.Close()
	cmd := LoginCommand()
	cmd.SetArgs([]string{"--issuer", srv.URL, "--client-id", "x"})
	_ = captureStderr(t, func() {
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "device auth init") {
			t.Errorf("expected device auth init err, got %v", err)
		}
	})
}

func TestLoginCommand_PollError(t *testing.T) {
	withTempTokenDir(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/device/code":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code":      "dc",
				"user_code":        "U",
				"verification_uri": "https://x/y",
				"expires_in":       300,
				"interval":         1,
			})
		case "/token":
			// Non-200 with a recognised RFC 8628 error code makes
			// PollDeviceToken bail out (access_denied is terminal).
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "access_denied"})
		}
	}))
	defer srv.Close()
	cmd := LoginCommand()
	cmd.SetArgs([]string{"--issuer", srv.URL, "--client-id", "x"})
	_ = captureStderr(t, func() {
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "poll token") {
			t.Errorf("expected poll error, got %v", err)
		}
	})
}

func TestLoginCommand_SaveError(t *testing.T) {
	// Point XDG_CONFIG_HOME at a path that exists as a file so
	// MkdirAll fails inside SaveCachedToken. This way Save returns
	// an error and we cover the "save token" wrap.
	tmp := t.TempDir()
	regularFile := tmp + "/this-is-not-a-dir"
	if err := os.WriteFile(regularFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Setenv("XDG_CONFIG_HOME", regularFile)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/device/code":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code": "dc", "user_code": "U", "verification_uri": "https://x", "expires_in": 300, "interval": 1,
			})
		case "/token":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "expires_in": 60})
		}
	}))
	defer srv.Close()
	cmd := LoginCommand()
	cmd.SetArgs([]string{"--issuer", srv.URL, "--client-id", "x"})
	_ = captureStderr(t, func() {
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "save token") {
			t.Errorf("expected save token err, got %v", err)
		}
	})
}

// ── logout ──────────────────────────────────────────────────────────────────

func TestLogoutCommand_NoCache(t *testing.T) {
	withTempTokenDir(t)
	stderr := captureStderr(t, func() {
		cmd := LogoutCommand()
		cmd.SetArgs([]string{})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(stderr, "Logged out.") {
		t.Errorf("missing 'Logged out.': %q", stderr)
	}
}

func TestLogoutCommand_WithCache(t *testing.T) {
	withTempTokenDir(t)
	tok := &vzclient.CachedToken{
		Issuer: "x", ClientID: "y", AccessToken: "at", ExpiresAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := vzclient.SaveCachedToken(tok); err != nil {
		t.Fatalf("save: %v", err)
	}
	cmd := LogoutCommand()
	cmd.SetArgs([]string{})
	_ = captureStderr(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if _, err := os.Stat(vzclient.TokenCachePath()); !os.IsNotExist(err) {
		t.Errorf("token file should be gone, stat err = %v", err)
	}
}

func TestLogoutCommand_DeleteError(t *testing.T) {
	// Set XDG_CONFIG_HOME to an unwritable path so Remove fails.
	tmp := t.TempDir()
	// Create a directory at the expected token path (Remove fails
	// because it's a non-empty directory).
	tokDir := tmp + "/vzc"
	if err := os.MkdirAll(tokDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(tokDir+"/token.hcl", 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Put a file inside so Remove(path) errors with "directory not empty".
	if err := os.WriteFile(tokDir+"/token.hcl/inner", []byte("x"), 0o600); err != nil {
		t.Fatalf("inner: %v", err)
	}
	t.Setenv("XDG_CONFIG_HOME", tmp)
	cmd := LogoutCommand()
	cmd.SetArgs([]string{})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected delete error")
	}
}

// ── whoami ──────────────────────────────────────────────────────────────────

func TestWhoamiCommand_Anonymous(t *testing.T) {
	withTempTokenDir(t)
	out := captureStdout(t, func() {
		cmd := WhoamiCommand()
		cmd.SetArgs([]string{})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "anonymous") {
		t.Errorf("missing anonymous: %q", out)
	}
}

func TestWhoamiCommand_WithToken(t *testing.T) {
	withTempTokenDir(t)
	tok := &vzclient.CachedToken{
		Issuer: "https://dex", ClientID: "vzc", AccessToken: "at",
		IDToken:   strings.Repeat("x", 60),
		ExpiresAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := vzclient.SaveCachedToken(tok); err != nil {
		t.Fatalf("save: %v", err)
	}
	out := captureStdout(t, func() {
		cmd := WhoamiCommand()
		cmd.SetArgs([]string{})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "issuer:") || !strings.Contains(out, "https://dex") {
		t.Errorf("missing issuer: %q", out)
	}
}

func TestWhoamiCommand_WithShortToken(t *testing.T) {
	// Cover min(40, len(IDToken)) branch: token shorter than 40 chars.
	withTempTokenDir(t)
	tok := &vzclient.CachedToken{
		Issuer: "i", ClientID: "c", AccessToken: "at",
		IDToken:   "short",
		ExpiresAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := vzclient.SaveCachedToken(tok); err != nil {
		t.Fatalf("save: %v", err)
	}
	out := captureStdout(t, func() {
		cmd := WhoamiCommand()
		cmd.SetArgs([]string{})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if !strings.Contains(out, "short") {
		t.Errorf("missing short id_token: %q", out)
	}
}

func TestWhoamiCommand_NoIDToken(t *testing.T) {
	withTempTokenDir(t)
	tok := &vzclient.CachedToken{
		Issuer: "i", ClientID: "c", AccessToken: "at",
		// IDToken empty → conditional print is skipped.
		ExpiresAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := vzclient.SaveCachedToken(tok); err != nil {
		t.Fatalf("save: %v", err)
	}
	out := captureStdout(t, func() {
		cmd := WhoamiCommand()
		cmd.SetArgs([]string{})
		if err := cmd.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})
	if strings.Contains(out, "id_token:") {
		t.Errorf("id_token line should be omitted: %q", out)
	}
}

func TestWhoamiCommand_LoadError(t *testing.T) {
	// XDG_CONFIG_HOME points to a token.hcl path that's actually a
	// non-decodable file → LoadCachedToken returns an error.
	tmp := t.TempDir()
	if err := os.MkdirAll(tmp+"/vzc", 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(tmp+"/vzc/token.hcl", []byte("not valid hcl !!!"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("XDG_CONFIG_HOME", tmp)

	cmd := WhoamiCommand()
	cmd.SetArgs([]string{})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected decode error")
	}
}

// ── preferredURL ────────────────────────────────────────────────────────────

func TestPreferredURL_Complete(t *testing.T) {
	if got := preferredURL(&vzclient.DeviceAuthResponse{VerificationURI: "a", VerificationURIComplete: "b"}); got != "b" {
		t.Errorf("complete should win: %q", got)
	}
}

func TestPreferredURL_Bare(t *testing.T) {
	if got := preferredURL(&vzclient.DeviceAuthResponse{VerificationURI: "a"}); got != "a" {
		t.Errorf("fallback: %q", got)
	}
}

// Silence unused-import warning if any.
var _ = url.Parse
