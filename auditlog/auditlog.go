// Package auditlog is the structured audit-log sink for RBAC
// decisions. Every ACL primitive in pkg/openweft/weft/acl.go
// (RequireAdmin, AuthorizeProject, VisibleProjects) ships a
// Decision through a Logger when one is wired ; the sink writes
// line-delimited JSON to a configurable path.
//
// Design points :
//
//   - Nil-safe : a nil *Logger.Record is a no-op. Code paths that
//     run without an operator-configured sink (single-host dev,
//     tests) don't pay any cost and don't need a guard at the
//     call site.
//
//   - Append-only LDJSON : one JSON object per line, no rotation
//     in-process. Rotation is the operator's job — logrotate,
//     systemd journal, fluentbit, whatever the deployment uses
//     for the rest of the host's logs. Keeping the writer dumb
//     means we never lose lines to a half-written rotate.
//
//   - Serialised writes : a single mutex guards the file handle so
//     concurrent gRPC handlers don't tear lines. Each Record
//     marshals once and Write()s once ; we don't buffer past the
//     OS page cache.
//
//   - Sink is replaceable : tests + the metrics endpoint can pass
//     their own io.Writer (NewWithWriter) without going through
//     the filesystem.
//
// This is the spine for SOC 2-style "who did what when" reviews ;
// content choices (which fields land in the JSON, which Decision
// constants exist) match the verb/object/scope taxonomy docs/
// operations/rbac.md already encodes.
package auditlog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Decision tags the outcome of an ACL check. We only need two
// today (Allow / Deny) ; rejection-by-misconfig (no caller in
// ctx, dex unreachable) lands as Deny with Reason filled in.
type Decision string

const (
	// Allow means the ACL primitive returned nil.
	Allow Decision = "allow"
	// Deny means the ACL primitive returned a status-coded error.
	Deny Decision = "deny"
)

// Record is the JSON shape emitted per audit line. Field names
// match the rbac.md vocabulary verbatim so a grep against the
// docs lands on the same identifiers as the audit log.
//
// Subject is the OIDC subject claim ("ldap:alice",
// "dev:yannick", …). Verb is the RPC name or imperative op label
// the caller passed (e.g. "RequireAdmin:delete-project",
// "AuthorizeProject", "VisibleProjects"). Object names the
// resource type ("project", "tenant", "cluster") and Scope its
// container UUID when applicable (project UUID, tenant UUID, or
// "cluster" for cluster-scoped ops). Reason carries the gRPC
// status message verbatim on Deny ; empty on Allow.
type Record struct {
	Timestamp time.Time `json:"timestamp"`
	Subject   string    `json:"subject"`
	Issuer    string    `json:"issuer,omitempty"`
	Verb      string    `json:"verb"`
	Object    string    `json:"object,omitempty"`
	Scope     string    `json:"scope,omitempty"`
	Decision  Decision  `json:"decision"`
	Reason    string    `json:"reason,omitempty"`
}

// Logger writes audit Records to an underlying sink. The zero
// value is NOT usable — construct via Open or NewWithWriter. A
// nil *Logger.Record is a no-op so callers that boot without
// audit config can pass nil through without guard-and-call.
type Logger struct {
	mu     sync.Mutex
	w      io.Writer
	closer io.Closer // non-nil iff Logger owns the underlying file
}

// Open opens (or creates) `path` and returns a Logger that writes
// line-delimited JSON to it. Parent directories are created with
// 0700 permissions ; the file is opened append-only with mode
// 0600 because audit lines are sensitive (caller subjects + deny
// reasons can leak attack surfaces).
//
// An empty path returns (nil, nil) — i.e. "audit log disabled,
// no error". This lets the boot path call Open unconditionally
// against the operator's config without a separate "is it set"
// branch.
func Open(path string) (*Logger, error) {
	if path == "" {
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("audit log: mkdir %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("audit log: open %s: %w", path, err)
	}
	return &Logger{w: f, closer: f}, nil
}

// NewWithWriter builds a Logger that emits to `w`. The Logger
// does not take ownership of `w` ; Close is a no-op for
// caller-supplied writers. Used by tests + by callers that want
// to multiplex audit lines through their own logging pipeline.
func NewWithWriter(w io.Writer) *Logger {
	if w == nil {
		return nil
	}
	return &Logger{w: w}
}

// Close flushes the underlying file when the Logger owns it.
// Safe to call on a nil receiver (returns nil) so deferred Close
// statements don't need a guard. After Close, subsequent Record
// calls write to a closed file and return the os.File error to
// the caller via Record's returned-error path.
func (l *Logger) Close() error {
	if l == nil || l.closer == nil {
		return nil
	}
	return l.closer.Close()
}

// Record marshals the decision to JSON and appends a line. Safe
// on a nil receiver (no-op) so callers don't need to guard each
// invocation when the operator left the audit log disabled.
//
// The ctx parameter is reserved for future cancellation ; the
// write is fast enough today that we don't honour it. Keeping
// the signature ctx-first matches the rest of the codebase.
//
// Errors are returned so a test can assert on them but the ACL
// primitives that call this in production ignore the return —
// dropping an audit line must NOT cause an RPC to fail (that
// would let operators DoS themselves by filling the disk).
func (l *Logger) Record(_ context.Context, r Record) error {
	if l == nil {
		return nil
	}
	if r.Timestamp.IsZero() {
		r.Timestamp = time.Now().UTC()
	}
	b, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("audit log: marshal: %w", err)
	}
	b = append(b, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.w == nil {
		return errors.New("audit log: nil writer")
	}
	if _, err := l.w.Write(b); err != nil {
		return fmt.Errorf("audit log: write: %w", err)
	}
	return nil
}

// DefaultPath is the path the agent uses when --audit-log /
// `audit_log {}` is enabled without an explicit path override.
// Matches the standard /var/log convention so logrotate's
// default config picks it up.
const DefaultPath = "/var/log/weft/audit.jsonl"
