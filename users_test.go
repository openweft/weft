package weft

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestUserRegistry_EmptyOnFreshStorage(t *testing.T) {
	reg, err := loadUserRegistry(context.Background(), NewMemStorage())
	if err != nil {
		t.Fatalf("loadUserRegistry: %v", err)
	}
	if got := len(reg.byUUID); got != 0 {
		t.Errorf("fresh registry has %d entries, want 0", got)
	}
	if got := reg.list(); len(got) != 0 {
		t.Errorf("list() = %d entries, want 0", len(got))
	}
}

func TestUserRegistry_GetOrCreateFromCaller_CreatesThenRefreshes(t *testing.T) {
	reg, err := loadUserRegistry(context.Background(), NewMemStorage())
	if err != nil {
		t.Fatal(err)
	}
	c := &Caller{
		Subject: "abc-123",
		Issuer:  "https://dex.example.com",
		Email:   "alice@example.com",
		Groups:  []string{"team-alpha"},
	}
	// First call: creates.
	u, created, err := reg.getOrCreateFromCaller(c)
	if err != nil {
		t.Fatalf("first getOrCreate: %v", err)
	}
	if !created {
		t.Errorf("first getOrCreate should report created=true")
	}
	if u.UUID == "" {
		t.Errorf("created user should have UUID")
	}
	if u.Subject != "abc-123" || u.Issuer != "https://dex.example.com" {
		t.Errorf("user fields wrong: %+v", u)
	}
	if u.DisplayName != "alice@example.com" {
		t.Errorf("default display_name should be email, got %q", u.DisplayName)
	}
	firstSeen := u.CreatedAt
	if firstSeen.IsZero() {
		t.Errorf("CreatedAt should be set")
	}

	// Second call with refreshed Email + Groups: same UUID, updated fields, LastSeenAt bumped.
	time.Sleep(2 * time.Millisecond) // ensure LastSeenAt advances
	c2 := &Caller{
		Subject: "abc-123",
		Issuer:  "https://dex.example.com",
		Email:   "alice@new-corp.com", // email changed upstream
		Groups:  []string{"team-alpha", "team-net"},
	}
	u2, created, err := reg.getOrCreateFromCaller(c2)
	if err != nil {
		t.Fatalf("second getOrCreate: %v", err)
	}
	if created {
		t.Errorf("second getOrCreate should report created=false")
	}
	if u2.UUID != u.UUID {
		t.Errorf("UUID changed across refreshes: %q → %q", u.UUID, u2.UUID)
	}
	if u2.Email != "alice@new-corp.com" {
		t.Errorf("email not refreshed: %q", u2.Email)
	}
	if len(u2.Groups) != 2 {
		t.Errorf("groups not refreshed: %v", u2.Groups)
	}
	if !u2.LastSeenAt.After(firstSeen) {
		t.Errorf("LastSeenAt didn't advance: first=%v second=%v", firstSeen, u2.LastSeenAt)
	}
	if u2.CreatedAt != firstSeen {
		t.Errorf("CreatedAt mutated: was %v, now %v", firstSeen, u2.CreatedAt)
	}
}

func TestUserRegistry_RejectsAnonymous(t *testing.T) {
	reg, _ := loadUserRegistry(context.Background(), NewMemStorage())
	cases := []*Caller{
		nil,
		{},
		{Subject: "abc"},                              // missing issuer
		{Issuer: "https://dex"},                       // missing subject
		{Dev: true, Subject: "dev:foo", Issuer: ""},   // dev synthetic caller
	}
	for i, c := range cases {
		_, _, err := reg.getOrCreateFromCaller(c)
		if err == nil {
			t.Errorf("case %d: anonymous caller %+v should be rejected", i, c)
		}
	}
}

func TestUserRegistry_LookupBySubject(t *testing.T) {
	reg, _ := loadUserRegistry(context.Background(), NewMemStorage())
	c := &Caller{Subject: "sub-x", Issuer: "https://i", Email: "x@x"}
	u, _, _ := reg.getOrCreateFromCaller(c)
	got, ok := reg.lookupBySubject(c.Issuer, c.Subject)
	if !ok || got.UUID != u.UUID {
		t.Errorf("lookupBySubject = (%+v, %v), want UUID=%s ok=true", got, ok, u.UUID)
	}
	// Different issuer, same subject: NOT a match (multi-IdP isolation).
	if _, ok := reg.lookupBySubject("https://other", c.Subject); ok {
		t.Errorf("cross-issuer match should NOT happen")
	}
	// Empty issuer / subject: never match.
	if _, ok := reg.lookupBySubject("", c.Subject); ok {
		t.Errorf("empty issuer should never match")
	}
}

func TestUserRegistry_SetDisplayName(t *testing.T) {
	reg, _ := loadUserRegistry(context.Background(), NewMemStorage())
	u, _, _ := reg.getOrCreateFromCaller(&Caller{Subject: "s", Issuer: "i", Email: "x@x"})
	if err := reg.setDisplayName(u.UUID, "Alice the Operator"); err != nil {
		t.Fatalf("setDisplayName: %v", err)
	}
	got, _ := reg.lookupByUUID(u.UUID)
	if got.DisplayName != "Alice the Operator" {
		t.Errorf("display_name not updated: %q", got.DisplayName)
	}
	// Subject + Issuer + UUID stayed put.
	if got.UUID != u.UUID || got.Subject != "s" || got.Issuer != "i" {
		t.Errorf("immutable fields mutated: %+v", got)
	}
	// Empty display_name rejected.
	if err := reg.setDisplayName(u.UUID, ""); err == nil {
		t.Errorf("empty display_name should be rejected")
	}
	// Unknown UUID rejected.
	if err := reg.setDisplayName("does-not-exist", "x"); err == nil {
		t.Errorf("unknown UUID should be rejected")
	}
}

func TestUserRegistry_Delete(t *testing.T) {
	reg, _ := loadUserRegistry(context.Background(), NewMemStorage())
	u, _, _ := reg.getOrCreateFromCaller(&Caller{Subject: "s", Issuer: "i", Email: "x@x"})
	if err := reg.delete(u.UUID); err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.lookupByUUID(u.UUID); ok {
		t.Errorf("user should be gone after delete")
	}
	if _, ok := reg.lookupBySubject("i", "s"); ok {
		t.Errorf("subject index should be gone after delete")
	}
	// Subsequent getOrCreate creates a NEW UUID — the old one is
	// not recycled.
	u2, created, _ := reg.getOrCreateFromCaller(&Caller{Subject: "s", Issuer: "i", Email: "x@x"})
	if !created || u2.UUID == u.UUID {
		t.Errorf("post-delete re-create should mint a new UUID; got created=%v u2.UUID=%q u.UUID=%q", created, u2.UUID, u.UUID)
	}
}

// TestUserRegistry_RoundTripViaStorage confirms that saveLocked +
// loadUserRegistry round-trip through a Storage backend: HCL
// encode + decode + index rebuild all line up.
func TestUserRegistry_RoundTripViaStorage(t *testing.T) {
	storage := NewMemStorage()
	reg, _ := loadUserRegistry(context.Background(), storage)
	u1, _, _ := reg.getOrCreateFromCaller(&Caller{
		Subject: "alice-sub",
		Issuer:  "https://dex.example.com",
		Email:   "alice@example.com",
		Groups:  []string{"team-alpha"},
	})
	u2, _, _ := reg.getOrCreateFromCaller(&Caller{
		Subject: "bob-sub",
		Issuer:  "https://dex.example.com",
		Email:   "bob@example.com",
		Groups:  []string{"team-beta", "team-net"},
	})
	_ = reg.setDisplayName(u1.UUID, "Alice E.")

	// Sanity: HCL blob looks reasonable.
	blob, _ := storage.Load(context.Background())
	if !strings.Contains(string(blob), "user \""+u1.UUID+"\"") {
		t.Errorf("HCL missing user %q block: %s", u1.UUID, blob)
	}
	if !strings.Contains(string(blob), "alice@example.com") {
		t.Errorf("HCL missing alice email")
	}

	// Fresh registry, same Storage: every user re-resolves.
	reg2, err := loadUserRegistry(context.Background(), storage)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	a, ok := reg2.lookupByUUID(u1.UUID)
	if !ok || a.DisplayName != "Alice E." {
		t.Errorf("Alice re-load failed: %+v ok=%v", a, ok)
	}
	if len(a.Groups) != 1 || a.Groups[0] != "team-alpha" {
		t.Errorf("Alice groups: %v", a.Groups)
	}
	b, ok := reg2.lookupByUUID(u2.UUID)
	if !ok || b.Email != "bob@example.com" {
		t.Errorf("Bob re-load failed: %+v ok=%v", b, ok)
	}
	if len(b.Groups) != 2 {
		t.Errorf("Bob groups: %v", b.Groups)
	}
	// Subject index re-built too.
	got, ok := reg2.lookupBySubject("https://dex.example.com", "alice-sub")
	if !ok || got.UUID != u1.UUID {
		t.Errorf("subject index didn't survive reload")
	}
}

// TestUserRegistry_List sorts deterministically by DisplayName.
func TestUserRegistry_List(t *testing.T) {
	reg, _ := loadUserRegistry(context.Background(), NewMemStorage())
	uA, _, _ := reg.getOrCreateFromCaller(&Caller{Subject: "1", Issuer: "i", Email: "alice@x"})
	uB, _, _ := reg.getOrCreateFromCaller(&Caller{Subject: "2", Issuer: "i", Email: "bob@x"})
	uC, _, _ := reg.getOrCreateFromCaller(&Caller{Subject: "3", Issuer: "i", Email: "carol@x"})
	got := reg.list()
	if len(got) != 3 {
		t.Fatalf("list size = %d, want 3", len(got))
	}
	// Default DisplayName is the email; alphabetical order: alice < bob < carol.
	wantOrder := []string{uA.UUID, uB.UUID, uC.UUID}
	for i, u := range got {
		if u.UUID != wantOrder[i] {
			t.Errorf("[%d] UUID = %q, want %q", i, u.UUID, wantOrder[i])
		}
	}
}
