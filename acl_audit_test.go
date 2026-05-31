//go:build darwin && cgo

package weft

// acl_audit_test.go pins the audit-log wiring : every Allow/Deny
// path through the three ACL primitives must produce exactly one
// auditlog.Record line. The fixture from acl_test.go gives us the
// multi-tenant world ; we just swap an in-memory sink in via
// SetAuditLogger and assert on the captured Records.

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/openweft/weft/auditlog"
)

// captureAudit installs an in-memory audit logger and returns a
// function that drains the captured Records. Restores the
// previous sink on cleanup so parallel tests don't leak state.
func captureAudit(t *testing.T) func() []auditlog.Record {
	t.Helper()
	var buf bytes.Buffer
	sink := auditlog.NewWithWriter(&buf)
	prev := auditSink.Load()
	SetAuditLogger(sink)
	t.Cleanup(func() {
		// Restore the previous sink (typically nil) so we don't
		// bleed our in-memory writer into unrelated tests.
		SetAuditLogger(prev)
	})
	return func() []auditlog.Record {
		var out []auditlog.Record
		for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
			if line == "" {
				continue
			}
			var r auditlog.Record
			if err := json.Unmarshal([]byte(line), &r); err != nil {
				t.Fatalf("audit unmarshal: %v (line=%q)", err, line)
			}
			out = append(out, r)
		}
		return out
	}
}

func TestAuditLog_AuthorizeProjectAllowsAndDenies(t *testing.T) {
	drain := captureAudit(t)
	f := newACLFixture(t)

	aliceCtx := WithCaller(context.Background(), &Caller{Subject: "ldap:alice", Issuer: "https://dex.example"})
	bobCtx := WithCaller(context.Background(), &Caller{Subject: "ldap:bob", Issuer: "https://dex.example"})

	// Allow path : alice on her shared project.
	if _, err := f.a.AuthorizeProject(aliceCtx, f.sharedProject); err != nil {
		t.Fatalf("alice AuthorizeProject: %v", err)
	}
	// Deny path : bob on the shared project.
	if _, err := f.a.AuthorizeProject(bobCtx, f.sharedProject); err == nil {
		t.Fatal("bob AuthorizeProject should fail")
	}

	records := drain()
	if len(records) < 2 {
		t.Fatalf("expected at least 2 audit records (allow + deny), got %d", len(records))
	}
	var sawAllow, sawDeny bool
	for _, r := range records {
		if r.Verb != "AuthorizeProject" {
			continue
		}
		switch r.Subject {
		case "ldap:alice":
			if r.Decision == auditlog.Allow && r.Scope == f.sharedProject {
				sawAllow = true
			}
		case "ldap:bob":
			if r.Decision == auditlog.Deny {
				sawDeny = true
				if r.Reason == "" {
					t.Error("Deny record missing Reason")
				}
			}
		}
	}
	if !sawAllow {
		t.Error("missing Allow record for alice on shared project")
	}
	if !sawDeny {
		t.Error("missing Deny record for bob on shared project")
	}
}

func TestAuditLog_RequireAdminPaths(t *testing.T) {
	drain := captureAudit(t)
	adminCtx := WithCaller(context.Background(), &Caller{Subject: "ldap:admin", Issuer: "https://dex.example", Groups: []string{PlatformAdminGroup}})
	userCtx := WithCaller(context.Background(), &Caller{Subject: "ldap:carol", Issuer: "https://dex.example"})

	if err := RequireAdmin(adminCtx, "delete-project"); err != nil {
		t.Fatalf("admin RequireAdmin: %v", err)
	}
	if err := RequireAdmin(userCtx, "delete-project"); err == nil {
		t.Fatal("non-admin RequireAdmin should fail")
	}
	if err := RequireAdmin(context.Background(), "delete-project"); err == nil {
		t.Fatal("no-caller RequireAdmin should fail")
	}
	records := drain()
	if len(records) != 3 {
		t.Fatalf("expected 3 audit records, got %d (%+v)", len(records), records)
	}
	wantVerb := "RequireAdmin:delete-project"
	if records[0].Decision != auditlog.Allow || records[0].Verb != wantVerb || records[0].Object != "cluster" {
		t.Errorf("record[0] = %+v, want allow/%s/cluster", records[0], wantVerb)
	}
	if records[1].Decision != auditlog.Deny || records[1].Subject != "ldap:carol" {
		t.Errorf("record[1] = %+v, want deny/ldap:carol", records[1])
	}
	if records[2].Decision != auditlog.Deny || records[2].Subject != "" {
		t.Errorf("record[2] = %+v, want deny/no-subject", records[2])
	}
}

func TestAuditLog_VisibleProjectsAllow(t *testing.T) {
	drain := captureAudit(t)
	f := newACLFixture(t)

	adminCtx := WithCaller(context.Background(), &Caller{Subject: "ldap:admin", Issuer: "https://dex.example", Groups: []string{PlatformAdminGroup}})
	if _, _, err := f.a.VisibleProjects(adminCtx); err != nil {
		t.Fatalf("admin VisibleProjects: %v", err)
	}
	bobCtx := WithCaller(context.Background(), &Caller{Subject: "ldap:bob", Issuer: "https://dex.example"})
	if _, _, err := f.a.VisibleProjects(bobCtx); err != nil {
		t.Fatalf("bob VisibleProjects: %v", err)
	}
	records := drain()
	var sawAdmin, sawBob bool
	for _, r := range records {
		if r.Verb != "VisibleProjects" {
			continue
		}
		if r.Decision != auditlog.Allow {
			t.Errorf("VisibleProjects should record Allow, got %+v", r)
		}
		if r.Subject == "ldap:admin" {
			sawAdmin = true
		}
		if r.Subject == "ldap:bob" {
			sawBob = true
		}
	}
	if !sawAdmin || !sawBob {
		t.Errorf("missing VisibleProjects records (admin=%v bob=%v)", sawAdmin, sawBob)
	}
}

func TestAuditLog_DisabledIsZeroCost(t *testing.T) {
	// With no sink installed, the ACL primitives must not panic
	// and must return their usual results. This is the
	// production-default path (operator hasn't opted in).
	SetAuditLogger(nil)
	t.Cleanup(func() { SetAuditLogger(nil) })

	f := newACLFixture(t)
	// Use empty name : alice's default project is her sub claim ;
	// AuthorizeProject(_, "") always succeeds for her.
	ctx := WithCaller(context.Background(), &Caller{Subject: "ldap:alice", Issuer: "https://dex.example"})
	if _, err := f.a.AuthorizeProject(ctx, ""); err != nil {
		t.Fatalf("AuthorizeProject with nil sink: %v", err)
	}
	if _, _, err := f.a.VisibleProjects(ctx); err != nil {
		t.Fatalf("VisibleProjects with nil sink: %v", err)
	}
	if err := RequireAdmin(WithCaller(context.Background(), &Caller{Dev: true}), "noop"); err != nil {
		t.Fatalf("RequireAdmin with nil sink: %v", err)
	}
}
