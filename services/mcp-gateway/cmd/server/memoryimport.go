package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// maxImportItems bounds one scos_memory_import call; a larger import is sent
// as several calls.
const maxImportItems = 200

var importNetworks = map[string]bool{"world": true, "experience": true, "entity": true, "belief": true, "procedure": true, "negative": true}
var importSensitivities = map[string]bool{"public": true, "internal": true, "confidential": true, "restricted": true}

// importResult is the outcome for one imported item.
type importResult struct {
	Index           int      `json:"index"`
	Origin          string   `json:"origin"`
	FactID          string   `json:"fact_id,omitempty"`
	Merged          bool     `json:"merged,omitempty"`
	CanonicalFactID string   `json:"canonical_fact_id,omitempty"`
	Contradictions  []string `json:"contradictions,omitempty"`
	Error           string   `json:"error,omitempty"`
}

// importMemories stores each item as an external fact through
// memory-intelligence, the same governed write path scos_remember uses, so
// every item is deduplicated, checked for contradictions and secret-scanned.
// Items are validated first and none is written if any is malformed; after
// that each item stands alone and a failure is reported against it.
func (s *gatewayServer) importMemories(ctx context.Context, r *http.Request, args map[string]any) (any, error) {
	raw, ok := args["items"].([]any)
	if !ok || len(raw) == 0 {
		return nil, fmt.Errorf("items is required: a non-empty array of {content, origin}")
	}
	if len(raw) > maxImportItems {
		return nil, fmt.Errorf("items holds %d memories; send at most %d per call", len(raw), maxImportItems)
	}
	facts := make([]map[string]any, 0, len(raw))
	for i, entry := range raw {
		item, ok := entry.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("items[%d] is not an object", i)
		}
		content := strings.TrimSpace(stringArg(item, "content"))
		origin := strings.TrimSpace(stringArg(item, "origin"))
		if content == "" || origin == "" {
			return nil, fmt.Errorf("items[%d] needs content and origin", i)
		}
		fact := map[string]any{"content": content, "origin": origin, "source": "external"}
		if v := strings.TrimSpace(stringArg(item, "network")); v != "" {
			if !importNetworks[v] {
				return nil, fmt.Errorf("items[%d].network %q is not a memory network", i, v)
			}
			fact["network"] = v
		}
		if v := strings.TrimSpace(stringArg(item, "sensitivity")); v != "" {
			if !importSensitivities[v] {
				return nil, fmt.Errorf("items[%d].sensitivity %q is not public, internal, confidential or restricted", i, v)
			}
			fact["sensitivity"] = v
		}
		if v := strings.TrimSpace(stringArg(item, "observed_at")); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return nil, fmt.Errorf("items[%d].observed_at is not RFC 3339: %v", i, err)
			}
			fact["observed_at"] = t.UTC().Format(time.RFC3339)
		}
		for _, key := range []string{"scope", "namespace_id"} {
			if v := strings.TrimSpace(stringArg(item, key)); v != "" {
				fact[key] = v
			}
		}
		facts = append(facts, fact)
	}

	results := make([]importResult, 0, len(facts))
	stored, merged, failed := 0, 0, 0
	for i, fact := range facts {
		res := importResult{Index: i, Origin: fact["origin"].(string)}
		out, err := s.forwardPOST(ctx, r, s.memoryIntelligenceURL+"/v1/facts", fact)
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("import stopped after %d of %d items: %w", i, len(facts), ctx.Err())
			}
			res.Error = err.Error()
			failed++
			results = append(results, res)
			continue
		}
		body, _ := out.(map[string]any)
		if f, ok := body["fact"].(map[string]any); ok {
			res.FactID, _ = f["id"].(string)
		}
		if m, _ := body["merged"].(bool); m {
			res.Merged = true
			res.CanonicalFactID, _ = body["canonical_fact_id"].(string)
			merged++
		} else {
			stored++
		}
		if cs, ok := body["contradictions"].([]any); ok {
			for _, c := range cs {
				if id, ok := c.(string); ok {
					res.Contradictions = append(res.Contradictions, id)
				}
			}
		}
		results = append(results, res)
	}
	return map[string]any{"stored": stored, "merged_into_existing": merged, "failed": failed, "results": results}, nil
}
