package federation

import (
	"testing"
	"time"
)

// placeFixture is a 3-cluster federation used by the placement tests.
// eu-paris : region eu-west-3, weight 100
// us-iad   : region us-east-1, weight 50
// eu-fra   : region eu-west-3, weight 80  (lower than eu-paris in same region)
func placeFixture() PlaceInput {
	local := &FederationManifest{
		Name: "acme-global", Version: 7,
		Members: []Cluster{
			{Name: "eu-paris", Region: "eu-west-3", Weight: 100},
		},
	}
	peers := []PeerState{
		{
			Name: "us-iad", URL: "https://us-iad", Status: "live",
			LastSeen: time.Now(),
			Manifest: &FederationManifest{
				Name: "acme-global", Version: 7,
				Members: []Cluster{{Name: "us-iad", Region: "us-east-1", Weight: 50}},
			},
		},
		{
			Name: "eu-fra", URL: "https://eu-fra", Status: "live",
			LastSeen: time.Now(),
			Manifest: &FederationManifest{
				Name: "acme-global", Version: 7,
				Members: []Cluster{{Name: "eu-fra", Region: "eu-west-3", Weight: 80}},
			},
		},
	}
	return PlaceInput{Local: local, Peers: peers}
}

func TestPlacePicksRegionMatch(t *testing.T) {
	in := placeFixture()
	recs := Place(in, Constraints{Region: "eu-west-3"})
	if len(recs) != 3 {
		t.Fatalf("expected 3 recs, got %d", len(recs))
	}
	top, ok := Top(recs)
	if !ok {
		t.Fatal("Top must return a winner")
	}
	if top.Cluster.Name != "eu-paris" {
		t.Fatalf("top : got %q want eu-paris (region match + highest weight)", top.Cluster.Name)
	}
	// us-iad mismatches region and should be last with the negative
	// region penalty.
	if last := recs[len(recs)-1]; last.Cluster.Name != "us-iad" {
		t.Fatalf("last : got %q want us-iad", last.Cluster.Name)
	}
}

func TestPlaceMinWeightFilters(t *testing.T) {
	in := placeFixture()
	recs := Place(in, Constraints{MinWeight: 75})
	for _, r := range recs {
		if r.Cluster.NormalisedWeight() < 75 {
			t.Fatalf("min_weight filter leaked %q (w=%d)", r.Cluster.Name, r.Cluster.NormalisedWeight())
		}
	}
	if len(recs) != 2 {
		t.Fatalf("expected 2 recs, got %d", len(recs))
	}
}

func TestPlaceExcludeBlacklist(t *testing.T) {
	in := placeFixture()
	recs := Place(in, Constraints{ExcludeNames: []string{"eu-paris"}})
	for _, r := range recs {
		if r.Cluster.Name == "eu-paris" {
			t.Fatal("exclude must drop eu-paris")
		}
	}
	if len(recs) != 2 {
		t.Fatalf("expected 2 recs, got %d", len(recs))
	}
}

func TestPlaceStalePeerPenalised(t *testing.T) {
	in := placeFixture()
	// Mark eu-fra stale ; it should drop below the live eu-paris
	// even though both share the region.
	in.Peers[1].Status = "stale"
	recs := Place(in, Constraints{Region: "eu-west-3"})
	if recs[0].Cluster.Name != "eu-paris" {
		t.Fatalf("eu-paris must win, got %q", recs[0].Cluster.Name)
	}
	var euFra Recommendation
	for _, r := range recs {
		if r.Cluster.Name == "eu-fra" {
			euFra = r
			break
		}
	}
	if !euFra.Stale {
		t.Fatal("stale flag must propagate to recommendation")
	}
	if euFra.Score >= recs[0].Score {
		t.Fatalf("stale peer must score below live region match : %d vs %d", euFra.Score, recs[0].Score)
	}
}

func TestPlaceNoCandidatesEmpty(t *testing.T) {
	// Empty input → empty result, no panic.
	if recs := Place(PlaceInput{}, Constraints{}); len(recs) != 0 {
		t.Fatalf("empty input → empty recs, got %d", len(recs))
	}
	if _, ok := Top(nil); ok {
		t.Fatal("Top of nil must be !ok")
	}
}

func TestParseConstraints(t *testing.T) {
	c, err := ParseConstraints("region=eu-west-3,min_weight=50")
	if err != nil {
		t.Fatalf("parse : %v", err)
	}
	if c.Region != "eu-west-3" || c.MinWeight != 50 {
		t.Fatalf("constraints : %+v", c)
	}
	c2, err := ParseConstraints("exclude=a,b,c")
	if err != nil {
		t.Fatalf("parse exclude : %v", err)
	}
	if len(c2.ExcludeNames) != 3 {
		t.Fatalf("exclude len : %d", len(c2.ExcludeNames))
	}
	if _, err := ParseConstraints("garbage"); err == nil {
		t.Fatal("garbage must error")
	}
	if _, err := ParseConstraints("unknown=x"); err == nil {
		t.Fatal("unknown key must error")
	}
	if _, err := ParseConstraints("min_weight=abc"); err == nil {
		t.Fatal("non-numeric min_weight must error")
	}
	if c, err := ParseConstraints(""); err != nil || c.Region != "" {
		t.Fatalf("empty parse : err=%v c=%+v", err, c)
	}
}
