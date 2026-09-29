package benchmarkharness

import (
	"math"
)

// relevance marks, for the first k retrieved results, which ones are hits:
// an expected ID counts only at its first occurrence, so a retriever that
// repeats a relevant result is not credited twice (which would push recall
// and nDCG above 1).
func relevance(expected, retrieved []string, k int) (hits []bool, relevant int) {
	want := make(map[string]bool, len(expected))
	for _, id := range expected {
		want[id] = true
	}
	k = max(0, min(k, len(retrieved)))
	hits = make([]bool, k)
	for i := 0; i < k; i++ {
		if want[retrieved[i]] {
			hits[i] = true
			delete(want, retrieved[i])
		}
	}
	relevant = len(want)
	for _, h := range hits {
		if h {
			relevant++
		}
	}
	return hits, relevant
}

func countHits(hits []bool) int {
	n := 0
	for _, h := range hits {
		if h {
			n++
		}
	}
	return n
}

// ComputeRecallAtK calculates Recall@K (§33 Quality evaluation): the
// fraction of distinct expected IDs found in the top k.
func ComputeRecallAtK(expected []string, retrieved []string, k int) float64 {
	hits, relevant := relevance(expected, retrieved, k)
	if relevant == 0 {
		return 0.0
	}
	return float64(countHits(hits)) / float64(relevant)
}

// ComputePrecisionAtK calculates Precision@K over the results actually
// returned in the top k; repeated results count as misses.
func ComputePrecisionAtK(expected []string, retrieved []string, k int) float64 {
	hits, _ := relevance(expected, retrieved, k)
	if len(hits) == 0 {
		return 0.0
	}
	return float64(countHits(hits)) / float64(len(hits))
}

// ComputeMRR calculates Mean Reciprocal Rank (MRR).
func ComputeMRR(expected []string, retrieved []string) float64 {
	expectedMap := make(map[string]bool)
	for _, id := range expected {
		expectedMap[id] = true
	}
	for i, r := range retrieved {
		if expectedMap[r] {
			return 1.0 / float64(i+1)
		}
	}
	return 0.0
}

// ComputeNDCGAtK calculates Normalized Discounted Cumulative Gain (nDCG@K)
// with binary relevance. The ideal ranking places min(k, distinct expected)
// hits first; it uses k itself, not the number of results returned, so a
// short result list is not scored against a shorter ideal.
func ComputeNDCGAtK(expected []string, retrieved []string, k int) float64 {
	hits, relevant := relevance(expected, retrieved, k)
	dcg := 0.0
	for i, h := range hits {
		if h {
			dcg += 1.0 / math.Log2(float64(i+2))
		}
	}
	idcg := 0.0
	for i := 0; i < min(max(k, 0), relevant); i++ {
		idcg += 1.0 / math.Log2(float64(i+2))
	}
	if idcg == 0.0 {
		return 0.0
	}
	return dcg / idcg
}
