package main

import "encoding/json"

// estimateCodeEvidenceTokens counts the full serialized code-result metadata,
// including its immutable snapshot/version provenance. Code evidence is not
// free merely because it contains identifiers instead of source text.
func estimateCodeEvidenceTokens(item codeQueryResult) int {
	encoded, err := json.Marshal(item)
	if err != nil {
		// An unencodable item cannot be safely emitted as a JSON response.
		return int(^uint(0) >> 1)
	}
	return (len(encoded) + 3) / 4
}

// budgetCodeEvidence retains complete code records in downstream rank order.
// It never truncates identifiers, paths or provenance, and never mutates the
// downstream response that might be reused by another caller.
func budgetCodeEvidence(input *codeQueryResponse, remaining int) (*codeQueryResponse, int) {
	if input == nil {
		return nil, 0
	}
	out := *input
	out.Results = make([]codeQueryResult, 0, len(input.Results))
	used := 0
	for _, item := range input.Results {
		cost := estimateCodeEvidenceTokens(item)
		if cost <= 0 || cost > remaining-used {
			continue
		}
		out.Results = append(out.Results, item)
		used += cost
	}
	return &out, used
}

func codeEvidenceTokens(input *codeQueryResponse) int {
	if input == nil {
		return 0
	}
	total := 0
	for _, item := range input.Results {
		total += estimateCodeEvidenceTokens(item)
	}
	return total
}
