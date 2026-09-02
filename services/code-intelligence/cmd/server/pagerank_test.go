package main

import (
	"math"
	"testing"
)

// approxEqual mirrors PageRank's own tolerance expectations: power
// iteration converges, it doesn't land on an exact float.
func approxEqual(a, b, eps float64) bool {
	return math.Abs(a-b) < eps
}

func sumRanks(ranks map[string]float64) float64 {
	var total float64
	for _, r := range ranks {
		total += r
	}
	return total
}

func TestPersonalizedPageRank_HubOutranksLeaf(t *testing.T) {
	// a, b, c all call hub; hub calls nothing. leaf is called by nobody
	// and calls nobody. hub should end up ranked well above leaf and above
	// a/b/c themselves, since it is the target of every edge.
	nodes := []string{"a", "b", "c", "hub", "leaf"}
	edges := map[string][]string{
		"a": {"hub"},
		"b": {"hub"},
		"c": {"hub"},
	}
	ranks := personalizedPageRank(nodes, edges, nil, 0.85, 50)

	if ranks["hub"] <= ranks["a"] {
		t.Fatalf("expected hub (%v) to outrank caller a (%v)", ranks["hub"], ranks["a"])
	}
	if ranks["hub"] <= ranks["leaf"] {
		t.Fatalf("expected hub (%v) to outrank isolated leaf (%v)", ranks["hub"], ranks["leaf"])
	}
	if !approxEqual(sumRanks(ranks), 1.0, 1e-6) {
		t.Fatalf("expected total rank mass to stay normalized at 1.0, got %v", sumRanks(ranks))
	}
}

func TestPersonalizedPageRank_SeedBiasesTowardNeighbors(t *testing.T) {
	// changed -> dependent1 -> dependent2. Seeding on "changed" should push
	// rank toward its own out-neighbors more than an unrelated component
	// (x -> y) with the same shape gets under uniform teleportation.
	nodes := []string{"changed", "dependent1", "dependent2", "x", "y"}
	edges := map[string][]string{
		"changed":    {"dependent1"},
		"dependent1": {"dependent2"},
		"x":          {"y"},
	}

	seeded := personalizedPageRank(nodes, edges, []string{"changed"}, 0.85, 50)
	uniform := personalizedPageRank(nodes, edges, nil, 0.85, 50)

	if seeded["dependent1"] <= uniform["dependent1"] {
		t.Fatalf("expected seeding on changed to boost dependent1: seeded=%v uniform=%v", seeded["dependent1"], uniform["dependent1"])
	}
	if seeded["y"] >= uniform["y"] {
		t.Fatalf("expected seeding on changed to reduce y's relative share: seeded=%v uniform=%v", seeded["y"], uniform["y"])
	}
}

func TestPersonalizedPageRank_DanglingNodeDoesNotLeakMass(t *testing.T) {
	// sink has no outgoing edges. Without dangling-mass redistribution,
	// total rank mass decays every iteration instead of staying at 1.0.
	nodes := []string{"source", "sink"}
	edges := map[string][]string{
		"source": {"sink"},
	}
	ranks := personalizedPageRank(nodes, edges, []string{"source"}, 0.85, 50)
	if !approxEqual(sumRanks(ranks), 1.0, 1e-6) {
		t.Fatalf("expected dangling mass to be redistributed, not leaked; total=%v", sumRanks(ranks))
	}
}

func TestPersonalizedPageRank_EmptyNodesReturnsEmpty(t *testing.T) {
	ranks := personalizedPageRank(nil, nil, nil, 0.85, 50)
	if len(ranks) != 0 {
		t.Fatalf("expected empty result for empty node set, got %v", ranks)
	}
}

type rankFixture struct {
	id  string
	fqn string
}

func TestRankByCentrality_OrdersDescendingThenByFQN(t *testing.T) {
	items := []rankFixture{
		{id: "low", fqn: "pkg.Low"},
		{id: "tieB", fqn: "pkg.TieB"},
		{id: "high", fqn: "pkg.High"},
		{id: "tieA", fqn: "pkg.TieA"},
	}
	ranks := map[string]float64{
		"low":  0.1,
		"high": 0.9,
		"tieA": 0.5,
		"tieB": 0.5,
	}
	rankByCentrality(items, ranks, func(r rankFixture) string { return r.id }, func(r rankFixture) string { return r.fqn })

	got := make([]string, len(items))
	for i, it := range items {
		got[i] = it.id
	}
	want := []string{"high", "tieA", "tieB", "low"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rank order = %v, want %v", got, want)
		}
	}
}

func TestRankByCentrality_MissingRankTreatedAsZero(t *testing.T) {
	items := []rankFixture{
		{id: "known", fqn: "pkg.Known"},
		{id: "unknown", fqn: "pkg.Unknown"},
	}
	ranks := map[string]float64{"known": 0.3}
	rankByCentrality(items, ranks, func(r rankFixture) string { return r.id }, func(r rankFixture) string { return r.fqn })

	if items[0].id != "known" {
		t.Fatalf("expected known (nonzero rank) first, got order %v", items)
	}
}
