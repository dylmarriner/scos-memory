package main

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
)

// codeMemoryPaths traverses only code-version -> readable fact ->
// code-version. Every hop uses the same verified tenant/principal forwarding
// as the existing one-hop MCP tools, not a bypassing SQL or service account.
// Bounds guarantee at most 1 + maxFacts upstream requests and at most
// maxFacts*maxCodePerFact returned second-hop links.
func (s *gatewayServer) codeMemoryPaths(ctx context.Context, r *http.Request, args map[string]any) (any, error) {
	start := stringArg(args, "code_symbol_version_id")
	if !isUUID(start) {
		return nil, fmt.Errorf("code_symbol_version_id must be a UUID")
	}
	bound := func(name string, def int) (int, error) {
		raw, ok := args[name]
		if !ok {
			return def, nil
		}
		n, valid := raw.(float64)
		if !valid || math.Trunc(n) != n || n < 1 || n > 10 {
			return 0, fmt.Errorf("%s must be an integer between 1 and 10", name)
		}
		return int(n), nil
	}
	maxFacts, err := bound("max_facts", 5)
	if err != nil {
		return nil, err
	}
	perFact, err := bound("max_code_per_fact", 5)
	if err != nil {
		return nil, err
	}
	query := url.Values{"code_symbol_version_id": {start}, "limit": {strconv.Itoa(maxFacts)}}
	if raw, ok := args["after"]; ok {
		after, valid := raw.(string)
		if !valid || !isUUID(after) {
			return nil, fmt.Errorf("after must be a UUID")
		}
		query.Set("after", after)
	}
	unavailable := func(err error) (any, error) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// HTTP upstream may contain database details or secrets. Do not
		// include its raw response/transport text in an MCP error.
		return nil, fmt.Errorf("governed memory-code traversal unavailable")
	}
	incoming, err := s.forwardGET(ctx, r, s.memoryIntelligenceURL+"/v1/evidence-links/by-code?"+query.Encode())
	if err != nil {
		return unavailable(err)
	}
	first, valid := incoming.(map[string]any)
	if !valid || first["code_symbol_version_id"] != start {
		return nil, fmt.Errorf("governed reverse evidence response invalid")
	}
	inbound, valid := first["links"].([]any)
	if !valid || len(inbound) > maxFacts {
		return nil, fmt.Errorf("governed reverse evidence page invalid")
	}
	paths := make([]any, 0, len(inbound))
	for _, item := range inbound {
		edge, valid := item.(map[string]any)
		if !valid {
			return nil, fmt.Errorf("governed reverse evidence edge invalid")
		}
		fact, _ := edge["fact_id"].(string)
		link, _ := edge["id"].(string)
		symbol, _ := edge["code_symbol_version_id"].(string)
		if !isUUID(fact) || !isUUID(link) || symbol != start {
			return nil, fmt.Errorf("governed reverse evidence identity invalid")
		}
		secondQuery := url.Values{"fact_id": {fact}, "limit": {strconv.Itoa(perFact)}}
		outbound, err := s.forwardGET(ctx, r, s.memoryIntelligenceURL+"/v1/evidence-links?"+secondQuery.Encode())
		if err != nil {
			return unavailable(err)
		}
		page, valid := outbound.(map[string]any)
		if !valid || page["fact_id"] != fact {
			return nil, fmt.Errorf("governed fact evidence response invalid")
		}
		codeLinks, valid := page["links"].([]any)
		if !valid || len(codeLinks) > perFact {
			return nil, fmt.Errorf("governed fact evidence page invalid")
		}
		for _, raw := range codeLinks {
			item, valid := raw.(map[string]any)
			if !valid {
				return nil, fmt.Errorf("governed fact evidence link invalid")
			}
			id, _ := item["id"].(string)
			version, _ := item["code_symbol_version_id"].(string)
			if !isUUID(id) || !isUUID(version) {
				return nil, fmt.Errorf("governed fact evidence provenance invalid")
			}
		}
		segment := map[string]any{
			"via_fact_id":     fact,
			"inbound_link_id": link,
			"code_links":      codeLinks,
		}
		if cursor, ok := page["next_cursor"]; ok {
			next, valid := cursor.(string)
			if !valid || !isUUID(next) {
				return nil, fmt.Errorf("governed fact cursor invalid")
			}
			segment["next_code_cursor"] = next
		}
		paths = append(paths, segment)
	}
	result := map[string]any{
		"start_code_symbol_version_id": start,
		"paths":                        paths,
	}
	if cursor, ok := first["next_cursor"]; ok {
		next, valid := cursor.(string)
		if !valid || !isUUID(next) {
			return nil, fmt.Errorf("governed reverse cursor invalid")
		}
		result["next_cursor"] = next
	}
	return result, nil
}
