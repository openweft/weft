package federation

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func vm() *FederationManifest {
	return &FederationManifest{
		Name: "acme-global", Version: 7,
		Members: []Cluster{
			{Name: "eu-paris", Region: "eu-west-3", Datacenters: []string{"par1", "par2"}, Weight: 100,
				PublicEndpoints: []string{"https://a.example.com:8443"}, CertificateBytes: []byte("PEM")},
			{Name: "us-iad", Region: "us-east-1", Datacenters: []string{"iad1"}, Weight: 50},
		},
	}
}

func TestValidate(t *testing.T) {
	if err := vm().Validate(); err != nil {
		t.Fatalf("valid : %v", err)
	}
	var nilM *FederationManifest
	if err := nilM.Validate(); err == nil {
		t.Fatal("nil must fail")
	}
	cases := []struct {
		mut  func(m *FederationManifest)
		want string
	}{
		{func(m *FederationManifest) { m.Name = "" }, "manifest.name is required"},
		{func(m *FederationManifest) { m.Members = nil }, "at least one member"},
		{func(m *FederationManifest) { m.Members[1].Name = "" }, "members[1].name is required"},
		{func(m *FederationManifest) { m.Members[1].Name = m.Members[0].Name }, "duplicate member"},
		{func(m *FederationManifest) { m.Members[0].Weight = -1 }, "must be >= 0"},
	}
	for i, tc := range cases {
		m := vm()
		tc.mut(m)
		if err := m.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("case %d : want %q, got %v", i, tc.want, err)
		}
	}
}

func TestRoundtripAndMarshal(t *testing.T) {
	b, err := vm().Marshal()
	if err != nil {
		t.Fatalf("Marshal : %v", err)
	}
	got, err := Unmarshal(b)
	if err != nil {
		t.Fatalf("Unmarshal : %v", err)
	}
	b2, _ := got.Marshal()
	if string(b) != string(b2) || got.Version != 7 || len(got.Members) != 2 || got.Members[0].Region != "eu-west-3" {
		t.Fatalf("roundtrip mismatch : %+v", got)
	}
	m := vm()
	m.Name = ""
	if _, err := m.Marshal(); err == nil {
		t.Fatal("Marshal must reject invalid")
	}
	if _, err := Unmarshal([]byte("not-json")); err == nil {
		t.Fatal("Unmarshal must reject non-JSON")
	}
	bad, _ := json.Marshal(&FederationManifest{Name: "x"})
	if _, err := Unmarshal(bad); err == nil {
		t.Fatal("Unmarshal must reject structurally-invalid")
	}
}

func TestHelpers(t *testing.T) {
	if (Cluster{Weight: 0}).NormalisedWeight() != 100 || (Cluster{Weight: 25}).NormalisedWeight() != 25 {
		t.Fatal("NormalisedWeight")
	}
	m := vm()
	if c := m.FindMember("us-iad"); c == nil || c.Region != "us-east-1" {
		t.Fatalf("hit : %+v", c)
	}
	if m.FindMember("nope") != nil {
		t.Fatal("miss must be nil")
	}
	var nilM *FederationManifest
	if nilM.FindMember("x") != nil {
		t.Fatal("nil receiver must yield nil")
	}
}

type fakeVerifier struct {
	calls int
	err   error
}

func (f *fakeVerifier) Verify(_ *FederationManifest, _ []byte) error { f.calls++; return f.err }

func TestVerifier(t *testing.T) {
	if err := (DenyAllVerifier{}).Verify(vm(), []byte("s")); err == nil {
		t.Fatal("DenyAll must reject")
	}
	if err := VerifyManifest(nil, vm(), nil); err == nil {
		t.Fatal("nil verifier must error")
	}
	v := &fakeVerifier{}
	bad := vm()
	bad.Name = ""
	if err := VerifyManifest(v, bad, []byte("s")); err == nil || v.calls != 0 {
		t.Fatalf("invalid must short-circuit : err=%v calls=%d", err, v.calls)
	}
	if err := VerifyManifest(v, vm(), []byte("s")); err != nil || v.calls != 1 {
		t.Fatalf("ok path : err=%v calls=%d", err, v.calls)
	}
	sentinel := errors.New("bad sig")
	if err := VerifyManifest(&fakeVerifier{err: sentinel}, vm(), nil); !errors.Is(err, sentinel) {
		t.Fatalf("must propagate, got %v", err)
	}
}
