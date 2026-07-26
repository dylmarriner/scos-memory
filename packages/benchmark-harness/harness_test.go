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
