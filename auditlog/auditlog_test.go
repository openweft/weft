package auditlog

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestLoggerNilIsNoop(t *testing.T) {
	var l *Logger
	if err := l.Record(context.Background(), Record{Subject: "x"}); err != nil {
		t.Fatalf("nil Record: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("nil Close: %v", err)
	}
}

func TestNewWithWriterEmitsJSONLines(t *testing.T) {
	var buf bytes.Buffer
	l := NewWithWriter(&buf)
	if l == nil {
		t.Fatal("NewWithWriter returned nil for non-nil writer")
	}
	r := Record{
		Subject:  "ldap:alice",
		Issuer:   "https://dex.example",
		Verb:     "AuthorizeProject",
		Object:   "project",
		Scope:    "abc-uuid",
		Decision: Allow,
	}
	if err := l.Record(context.Background(), r); err != nil {
		t.Fatalf("Record: %v", err)
	}
	out := buf.String()
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("output not LDJSON: %q", out)
	}
	var decoded Record
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Subject != r.Subject || decoded.Decision != Allow {
		t.Errorf("decoded mismatch: %+v vs %+v", decoded, r)
	}
	if decoded.Timestamp.IsZero() {
		t.Error("Timestamp should be filled in by Record when zero")
	}
}

func TestOpenWritesToDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "audit.jsonl")
	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if l == nil {
		t.Fatal("Open returned nil logger for non-empty path")
	}
	defer func() { _ = l.Close() }()

	if err := l.Record(context.Background(), Record{
		Subject:  "ldap:bob",
		Verb:     "RequireAdmin:delete-project",
		Object:   "cluster",
		Scope:    "cluster",
		Decision: Deny,
		Reason:   "delete-project requires platform-admin",
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// Make sure subsequent Records append (file opened with O_APPEND).
	if err := l.Record(context.Background(), Record{Subject: "ldap:alice", Verb: "AuthorizeProject", Decision: Allow}); err != nil {
		t.Fatalf("Record 2: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d (%q)", len(lines), string(data))
	}
	var r1, r2 Record
	if err := json.Unmarshal([]byte(lines[0]), &r1); err != nil {
		t.Fatalf("line1: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &r2); err != nil {
		t.Fatalf("line2: %v", err)
	}
	if r1.Decision != Deny || r2.Decision != Allow {
		t.Errorf("got %s/%s, want deny/allow", r1.Decision, r2.Decision)
	}
	if r1.Reason == "" {
		t.Error("Reason should be preserved on deny")
	}
	// File perm should be 0600 (sensitive material).
	info, _ := os.Stat(path)
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("file mode = %o, want 0600", mode)
	}
}

func TestOpenEmptyPathReturnsNil(t *testing.T) {
	l, err := Open("")
	if err != nil {
		t.Fatalf("Open(\"\"): %v", err)
	}
	if l != nil {
		t.Fatal("Open(\"\") should return nil logger (disabled)")
	}
}

func TestRecordConcurrentDoesNotTear(t *testing.T) {
	var buf bytes.Buffer
	l := NewWithWriter(&buf)
	var wg sync.WaitGroup
	const n = 50
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = l.Record(context.Background(), Record{
				Subject:  "ldap:user",
				Verb:     "AuthorizeProject",
				Decision: Allow,
			})
		}(i)
	}
	wg.Wait()
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != n {
		t.Fatalf("expected %d audit lines, got %d", n, len(lines))
	}
	for i, line := range lines {
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("line %d unmarshal: %v (%q)", i, err, line)
		}
	}
}
