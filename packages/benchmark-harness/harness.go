package benchmarkharness

import "math"

// Relevant documents form a set. A retriever that repeats one ID is not
// allowed to turn one correct answer into several successes.
func relevantIDs(expected []string) map[string]struct{} {
	out := make(map[string]struct{}, len(expected))
	for _, id := range expected {
		out[id] = struct{}{}
	}
	return out
}

func boundedK(retrieved []string, k int) int {
	if k <= 0 {
		return 0
	}
	if k > len(retrieved) {
		return len(retrieved)
	}
	return k
}

// ComputeRecallAtK measures the proportion of distinct relevant documents
// retrieved in the first k positions. Duplicate predictions count once.
func ComputeRecallAtK(expected, retrieved []string, k int) float64 {
	relevant := relevantIDs(expected)
	if len(relevant) == 0 {
		return 0
	}
	seen := make(map[string]struct{})
	hits := 0
	for _, id := range retrieved[:boundedK(retrieved, k)] {
		if _, already := seen[id]; already {
			continue
		}
		seen[id] = struct{}{}
		if _, ok := relevant[id]; ok {
			hits++
		}
	}
	return float64(hits) / float64(len(relevant))
}

// ComputePrecisionAtK counts distinct relevant documents over all returned
// ranking slots, so repeated IDs waste slots rather than inflating quality.
func ComputePrecisionAtK(expected, retrieved []string, k int) float64 {
	k = boundedK(retrieved, k)
	if k == 0 {
		return 0
	}
	relevant := relevantIDs(expected)
	seen := make(map[string]struct{})
	hits := 0
	for _, id := range retrieved[:k] {
		if _, already := seen[id]; already {
			continue
		}
		seen[id] = struct{}{}
		if _, ok := relevant[id]; ok {
			hits++
		}
	}
	return float64(hits) / float64(k)
}

// ComputeMRR gives the reciprocal position of the first relevant result.
func ComputeMRR(expected, retrieved []string) float64 {
	relevant := relevantIDs(expected)
	for i, id := range retrieved {
		if _, ok := relevant[id]; ok {
			return 1 / float64(i+1)
		}
	}
	return 0
}

// ComputeNDCGAtK compares discounted binary relevance to the ideal order.
// A duplicate result never contributes additional gain. The ideal order
// contains only the distinct ground-truth documents.
func ComputeNDCGAtK(expected, retrieved []string, k int) float64 {
	if k <= 0 {
		return 0
	}
	relevant := relevantIDs(expected)
	if len(relevant) == 0 {
		return 0
	}
	seen := make(map[string]struct{})
	dcg := 0.0
	for i, id := range retrieved[:boundedK(retrieved, k)] {
		if _, already := seen[id]; already {
			continue
		}
		seen[id] = struct{}{}
		if _, ok := relevant[id]; ok {
			dcg += 1 / math.Log2(float64(i+2))
		}
	}
	idcg := 0.0
	idealHits := k
	if len(relevant) < idealHits {
		idealHits = len(relevant)
	}
	for i := 0; i < idealHits; i++ {
		idcg += 1 / math.Log2(float64(i+2))
	}
	return dcg / idcg
}
