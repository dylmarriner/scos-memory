package benchmarkharness

import (
	"testing"
)

func TestMetrics(t *testing.T) {
	expected := []string{"doc1", "doc2"}
	retrieved := []string{"doc1", "doc3", "doc2"}

	r1 := ComputeRecallAtK(expected, retrieved, 1)
	if r1 != 0.5 {
		t.Fatalf("expected Recall@1 to be 0.5, got %f", r1)
	}

	p1 := ComputePrecisionAtK(expected, retrieved, 1)
	if p1 != 1.0 {
		t.Fatalf("expected Precision@1 to be 1.0, got %f", p1)
	}

	mrr := ComputeMRR(expected, retrieved)
	if mrr != 1.0 {
		t.Fatalf("expected MRR to be 1.0, got %f", mrr)
	}

	ndcg := ComputeNDCGAtK(expected, retrieved, 3)
	if ndcg <= 0.0 || ndcg > 1.0 {
		t.Fatalf("expected nDCG to be between 0 and 1, got %f", ndcg)
	}
}

func TestMetricsIgnoreDuplicateHits(t *testing.T) {
	expected := []string{"a"}
	retrieved := []string{"a", "a"}
	if r := ComputeRecallAtK(expected, retrieved, 2); r != 1.0 {
		t.Fatalf("recall with a repeated hit = %v, want 1", r)
	}
	if p := ComputePrecisionAtK(expected, retrieved, 2); p != 0.5 {
		t.Fatalf("precision with a repeated hit = %v, want 0.5", p)
	}
	if n := ComputeNDCGAtK(expected, retrieved, 2); n != 1.0 {
		t.Fatalf("nDCG with a repeated hit = %v, want 1", n)
	}
	if r := ComputeRecallAtK([]string{"a", "a"}, []string{"a"}, 1); r != 1.0 {
		t.Fatalf("duplicate expected IDs must count once, recall = %v", r)
	}
}

func TestNDCGUsesKForIdealRanking(t *testing.T) {
	// Two relevant docs, k=3, only one result returned: the ideal ranking
	// still has two hits, so the score is below 1.
	if n := ComputeNDCGAtK([]string{"a", "b"}, []string{"a"}, 3); n >= 1.0 {
		t.Fatalf("short result list scored as ideal: %v", n)
	}
}
