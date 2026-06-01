package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeS3 is a minimal httptest-backed S3-compatible server that
// records the requests + responds with canned bodies. We don't
// validate the SigV4 signature byte-for-byte (that's the job of
// the dedicated SigV4 vector test below) but we do assert the
// presence of the Authorization + x-amz-date + x-amz-content-sha256
// headers on every signed request.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	reqs    []recordedReq
	server  *httptest.Server
}

type recordedReq struct {
	Method string
	Path   string
	Query  string
	Auth   string
	Date   string
	Sha    string
	Body   []byte
}

func newFakeS3(t *testing.T) *fakeS3 {
	t.Helper()
	f := &fakeS3{objects: map[string][]byte{}}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeS3) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()
	rec := recordedReq{
		Method: r.Method,
		Path:   r.URL.Path,
		Query:  r.URL.RawQuery,
		Auth:   r.Header.Get("Authorization"),
		Date:   r.Header.Get("X-Amz-Date"),
		Sha:    r.Header.Get("X-Amz-Content-Sha256"),
		Body:   body,
	}
	f.mu.Lock()
	f.reqs = append(f.reqs, rec)
	f.mu.Unlock()

	// Path is "/<bucket>/<key>" — strip the bucket.
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
	if len(parts) < 1 {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	bucket := parts[0]
	_ = bucket
	keyPath := ""
	if len(parts) == 2 {
		keyPath = parts[1]
	}

	switch r.Method {
	case http.MethodPut:
		f.mu.Lock()
		f.objects[keyPath] = body
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		if r.URL.Query().Get("list-type") == "2" {
			// Render minimal ListObjectsV2 XML.
			prefix := r.URL.Query().Get("prefix")
			f.mu.Lock()
			var sb strings.Builder
			sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>`)
			sb.WriteString(`<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)
			sb.WriteString(`<IsTruncated>false</IsTruncated>`)
			for k, v := range f.objects {
				if prefix != "" && !strings.HasPrefix(k, prefix) {
					continue
				}
				fmt.Fprintf(&sb, `<Contents><Key>%s</Key><Size>%d</Size></Contents>`, k, len(v))
			}
			sb.WriteString(`</ListBucketResult>`)
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(sb.String()))
			return
		}
		f.mu.Lock()
		data, ok := f.objects[keyPath]
		f.mu.Unlock()
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_, _ = w.Write(data)
	case http.MethodDelete:
		f.mu.Lock()
		delete(f.objects, keyPath)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (f *fakeS3) lastReq() recordedReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		return recordedReq{}
	}
	return f.reqs[len(f.reqs)-1]
}

// TestS3Backend_Roundtrip exercises Upload → List → Download →
// Delete against the fake server, asserting each signed request
// carries the expected SigV4 headers.
func TestS3Backend_Roundtrip(t *testing.T) {
	f := newFakeS3(t)
	be, err := NewS3Backend("test-bucket", f.server.URL, "us-east-1", "AKIAEXAMPLE", "secret-example")
	if err != nil {
		t.Fatalf("NewS3Backend: %v", err)
	}
	// Pin the clock so signatures are reproducible. We don't
	// assert the signature byte-for-byte here — that's the
	// dedicated vector test below — but the canonical request
	// must build without error.
	be.nowFn = func() time.Time { return time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC) }

	srcPath := filepath.Join(t.TempDir(), "src.bin")
	payload := bytes.Repeat([]byte("ab"), 1024) // 2 KiB
	if err := os.WriteFile(srcPath, payload, 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	key := SnapshotKey("proj-1", "vol-1", "snap-1")
	ctx := context.Background()

	if err := be.Upload(ctx, srcPath, key); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	got := f.lastReq()
	if got.Method != "PUT" || !strings.HasSuffix(got.Path, key) {
		t.Errorf("Upload landed at %s %s, want PUT .../%s", got.Method, got.Path, key)
	}
	if got.Auth == "" || !strings.HasPrefix(got.Auth, "AWS4-HMAC-SHA256 ") {
		t.Errorf("Upload missing SigV4 Authorization header: %q", got.Auth)
	}
	if got.Date == "" || got.Sha == "" {
		t.Errorf("Upload missing X-Amz-Date or X-Amz-Content-Sha256")
	}

	entries, err := be.List(ctx, "proj-1/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].Key != key {
		t.Errorf("List entries = %+v, want one with Key=%q", entries, key)
	}

	dst := filepath.Join(t.TempDir(), "restored.bin")
	if err := be.Download(ctx, key, dst); err != nil {
		t.Fatalf("Download: %v", err)
	}
	gotBytes, _ := os.ReadFile(dst)
	if !bytes.Equal(gotBytes, payload) {
		t.Errorf("Download payload mismatch (len got=%d want=%d)", len(gotBytes), len(payload))
	}

	if err := be.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := be.Download(ctx, key, filepath.Join(t.TempDir(), "after-del.bin")); !errors.Is(err, ErrNotFound) {
		t.Errorf("Download after Delete: err = %v, want ErrNotFound", err)
	}
}

// TestS3Backend_Anonymous covers the "no AccessKeyID" branch :
// the signer becomes a no-op and the request must still go out
// without an Authorization header (for policy-open dev buckets).
func TestS3Backend_Anonymous(t *testing.T) {
	f := newFakeS3(t)
	be, _ := NewS3Backend("test-bucket", f.server.URL, "us-east-1", "", "")
	srcPath := filepath.Join(t.TempDir(), "src.bin")
	if err := os.WriteFile(srcPath, []byte("anon"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if err := be.Upload(context.Background(), srcPath, "p/v/s.qcow2"); err != nil {
		t.Fatalf("anon Upload: %v", err)
	}
	last := f.lastReq()
	if last.Auth != "" {
		t.Errorf("anonymous request carried Authorization header: %q", last.Auth)
	}
}

// TestS3Backend_MissingBucket keeps the NewS3Backend pre-condition
// asserted.
func TestS3Backend_MissingBucket(t *testing.T) {
	if _, err := NewS3Backend("", "https://example.com", "us-east-1", "k", "s"); err == nil {
		t.Errorf("NewS3Backend with empty bucket returned nil error")
	}
}

// TestS3Backend_SigV4Stability does NOT compare against an AWS
// reference vector — that's its own test surface — but it pins
// determinism : two signs of the same request with the same
// clock must produce the same Authorization header.
func TestS3Backend_SigV4Stability(t *testing.T) {
	be, _ := NewS3Backend("b", "https://example.com", "us-east-1", "AKID", "SECRET")
	fixed := time.Date(2025, 5, 31, 10, 0, 0, 0, time.UTC)
	be.nowFn = func() time.Time { return fixed }

	mkReq := func() *http.Request {
		r, _ := http.NewRequest(http.MethodGet, "https://example.com/b/p/v/s.qcow2", nil)
		return r
	}
	r1 := mkReq()
	if err := be.sign(r1, emptyBodyHash); err != nil {
		t.Fatalf("sign #1: %v", err)
	}
	r2 := mkReq()
	if err := be.sign(r2, emptyBodyHash); err != nil {
		t.Fatalf("sign #2: %v", err)
	}
	if r1.Header.Get("Authorization") != r2.Header.Get("Authorization") {
		t.Errorf("signatures diverged: %q vs %q",
			r1.Header.Get("Authorization"), r2.Header.Get("Authorization"))
	}
	if !strings.Contains(r1.Header.Get("Authorization"), "Credential=AKID/20250531/us-east-1/s3/aws4_request") {
		t.Errorf("scope wrong : %q", r1.Header.Get("Authorization"))
	}
}

// TestS3Backend_NotFound_Download covers the explicit 404 → ErrNotFound
// mapping (independent of the roundtrip test for clarity).
func TestS3Backend_NotFound_Download(t *testing.T) {
	f := newFakeS3(t)
	be, _ := NewS3Backend("test-bucket", f.server.URL, "us-east-1", "AKID", "SECRET")
	dst := filepath.Join(t.TempDir(), "out.bin")
	err := be.Download(context.Background(), "missing/key.qcow2", dst)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Download absent key : err = %v, want ErrNotFound", err)
	}
}
