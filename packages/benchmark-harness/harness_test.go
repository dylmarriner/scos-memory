package benchmarkharness

import (
	"math"
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

func TestMetricsCountEachRelevantDocumentOnlyOnce(t *testing.T) {
	expected := []string{"a", "b"}
	retrieved := []string{"a", "a", "a"}
	if got := ComputeRecallAtK(expected, retrieved, 3); got != 0.5 {
		t.Errorf("duplicate Recall@3 = %v, want 0.5", got)
	}
	if got := ComputePrecisionAtK(expected, retrieved, 3); math.Abs(got-1.0/3.0) > 1e-12 {
		t.Errorf("duplicate Precision@3 = %v, want 1/3", got)
	}
	wantNDCG := 1 / (1 + 1/math.Log2(3))
	if got := ComputeNDCGAtK(expected, retrieved, 3); math.Abs(got-wantNDCG) > 1e-12 {
		t.Errorf("duplicate nDCG@3 = %v, want %v", got, wantNDCG)
	}
}
func TestMetricsDeduplicateGroundTruthAndBoundK(t *testing.T) {
	expected := []string{"a", "a"}
	retrieved := []string{"a", "b"}
	if got := ComputeRecallAtK(expected, retrieved, 2); got != 1 {
		t.Errorf("duplicated ground truth Recall@2 = %v, want 1", got)
	}
	if got := ComputeNDCGAtK(expected, retrieved, 2); got != 1 {
		t.Errorf("duplicated ground truth nDCG@2 = %v, want 1", got)
	}
	for _, k := range []int{-100, 0} {
		if got := ComputeNDCGAtK(expected, retrieved, k); got != 0 {
			t.Errorf("nDCG@%d = %v, want 0", k, got)
		}
		if got := ComputeRecallAtK(expected, retrieved, k); got != 0 {
			t.Errorf("Recall@%d = %v, want 0", k, got)
		}
		if got := ComputePrecisionAtK(expected, retrieved, k); got != 0 {
			t.Errorf("Precision@%d = %v, want 0", k, got)
		}
	}
}

// Missing ranking slots must not shrink the ideal ranking: a single relevant
// result cannot earn perfect nDCG when another relevant result was omitted.
func TestNDCGDoesNotNormalizeAwayMissingResults(t *testing.T) {
	expected := []string{"a", "b"}
	retrieved := []string{"a"}
	want := 1 / (1 + 1/math.Log2(3))
	if got := ComputeNDCGAtK(expected, retrieved, 5); math.Abs(got-want) > 1e-12 {
		t.Fatalf("incomplete ranking nDCG@5 = %v, want %v", got, want)
	}
}
