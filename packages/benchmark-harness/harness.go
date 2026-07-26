package benchmarkharness

import (
	"math"
)

// ComputeRecallAtK calculates Recall@K (§33 Quality evaluation).
func ComputeRecallAtK(expected []string, retrieved []string, k int) float64 {
	if len(expected) == 0 {
		return 0.0
	}
	if k > len(retrieved) {
		k = len(retrieved)
	}
	expectedMap := make(map[string]bool)
	for _, id := range expected {
		expectedMap[id] = true
	}
	hits := 0
	for i := 0; i < k; i++ {
		if expectedMap[retrieved[i]] {
			hits++
		}
	}
	return float64(hits) / float64(len(expected))
}

// ComputePrecisionAtK calculates Precision@K.
func ComputePrecisionAtK(expected []string, retrieved []string, k int) float64 {
	if k <= 0 || len(retrieved) == 0 {
		return 0.0
	}
	if k > len(retrieved) {
		k = len(retrieved)
	}
	expectedMap := make(map[string]bool)
	for _, id := range expected {
		expectedMap[id] = true
	}
	hits := 0
	for i := 0; i < k; i++ {
		if expectedMap[retrieved[i]] {
			hits++
		}
	}
	return float64(hits) / float64(k)
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

// ComputeNDCGAtK calculates Normalized Discounted Cumulative Gain (nDCG@K).
func ComputeNDCGAtK(expected []string, retrieved []string, k int) float64 {
	if k > len(retrieved) {
		k = len(retrieved)
	}
	expectedMap := make(map[string]bool)
	for _, id := range expected {
		expectedMap[id] = true
	}

	dcg := 0.0
	for i := 0; i < k; i++ {
		if expectedMap[retrieved[i]] {
			if i == 0 {
				dcg += 1.0
			} else {
				dcg += 1.0 / math.Log2(float64(i+2))
			}
		}
	}

	idcg := 0.0
	maxHits := k
	if len(expected) < maxHits {
		maxHits = len(expected)
	}
	for i := 0; i < maxHits; i++ {
		if i == 0 {
			idcg += 1.0
		} else {
			idcg += 1.0 / math.Log2(float64(i+2))
		}
	}

	if idcg == 0.0 {
		return 0.0
	}
	return dcg / idcg
}
