package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// ── hierarchy (§25.6) ────────────────────────────────────────────────────
//
// A hierarchy node is an organisational view over facts, not a source of
// truth: it names the facts filed under it (hierarchy_node_facts) and its
// fact count is computed from those facts on every read. A caller cannot
// state a count.

var validHierarchyLevels = map[MemoryHierarchy]bool{
	LevelPalace: true, LevelWing: true, LevelDrawer: true,
	LevelCloset: true, LevelDiary: true, LevelArtifact: true,
}

// hierarchyNodeRequest is the body of POST /v1/hierarchy. FactCount exists
// only so a request that still sends fact_count is refused rather than
// silently ignored.
type hierarchyNodeRequest struct {
	Level       MemoryHierarchy `json:"level"`
	Name        string          `json:"name"`
	ParentID    string          `json:"parent_id,omitempty"`
	Scope       string          `json:"scope"`
	Summary     string          `json:"summary,omitempty"`
	FactIDs     []string        `json:"fact_ids,omitempty"`
	FactCount   *int            `json:"fact_count,omitempty"`
	TenantID    string          `json:"tenant_id,omitempty"`
	PrincipalID string          `json:"principal_id,omitempty"`
}

const factCountIsComputed = "fact_count is computed from the facts filed under the node; send fact_ids instead"

// resolveActiveFacts checks that every id names an active fact of the
// caller's tenant and returns the ids deduplicated, in request order.
func (s *engineServer) resolveActiveFacts(ctx context.Context, identity requestIdentity, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	facts, err := withPrincipalContext(ctx, s.db, identity, func(q querier) ([]*Fact, error) {
		return loadFacts(ctx, q, identity.TenantID, "")
	})
	if err != nil {
		return nil, err
	}
	active := make(map[string]bool, len(facts))
	for _, f := range facts {
		active[f.ID] = true
	}
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !active[id] {
			return nil, &unknownFactError{id: id}
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, nil
}

type unknownFactError struct{ id string }

func (e *unknownFactError) Error() string {
	return fmt.Sprintf("fact_ids references %q, which is not an active fact", e.id)
}

func (s *engineServer) handleHierarchy(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		identity, status, msg := resolveIdentityFromRequest(r)
		if status != 0 {
			http.Error(w, msg, status)
			return
		}
		nodes, err := withPrincipalContext(r.Context(), s.db, identity, func(q querier) ([]*HierarchyNode, error) {
			return loadHierarchy(r.Context(), q, identity.TenantID)
		})
		if err != nil {
			http.Error(w, "load hierarchy: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, nodes)

	case http.MethodPost:
		var req hierarchyNodeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid node: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.FactCount != nil {
			http.Error(w, factCountIsComputed, http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(req.Name) == "" {
			http.Error(w, "name required", http.StatusBadRequest)
			return
		}
		if req.Level == "" {
			req.Level = LevelDrawer
		}
		if !validHierarchyLevels[req.Level] {
			http.Error(w, "level must be palace, wing, drawer, closet, diary or artifact", http.StatusBadRequest)
			return
		}
		identity, status, msg := resolveIdentity(r, req.TenantID, req.PrincipalID)
		if status != 0 {
			http.Error(w, msg, status)
			return
		}
		if req.Scope == "" {
			req.Scope = "global"
		}

		ctx := r.Context()
		if req.ParentID != "" {
			exists, err := withPrincipalContext(ctx, s.db, identity, func(q querier) (bool, error) {
				return hierarchyNodeExists(ctx, q, identity.TenantID, req.ParentID)
			})
			if err != nil {
				http.Error(w, "load parent node: "+err.Error(), http.StatusInternalServerError)
				return
			}
			if !exists {
				http.Error(w, fmt.Sprintf("parent_id references unknown hierarchy node %q", req.ParentID), http.StatusUnprocessableEntity)
				return
			}
		}
		factIDs, err := s.resolveActiveFacts(ctx, identity, req.FactIDs)
		if writeFactResolutionError(w, err) {
			return
		}

		node := HierarchyNode{Level: req.Level, Name: req.Name, ParentID: req.ParentID, Scope: req.Scope, Summary: req.Summary}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			http.Error(w, "begin transaction: "+err.Error(), http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()
		if err := setPrincipalContext(ctx, tx, identity); err != nil {
			http.Error(w, "set principal context: "+err.Error(), http.StatusInternalServerError)
			return
		}
		nodeID, err := insertHierarchyNode(ctx, tx, identity.TenantID, &node, factIDs)
		if err != nil {
			http.Error(w, "failed to persist hierarchy: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := tx.Commit(); err != nil {
			http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
			return
		}
		s.writeHierarchyNode(ctx, w, identity, nodeID)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleHierarchyFacts serves POST /v1/hierarchy/{id}/facts with
// {"fact_ids": [...]}: files existing active facts under an existing node
// and returns the node with its recomputed membership and count.
func (s *engineServer) handleHierarchyFacts(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/hierarchy/")
	nodeID, action, ok := strings.Cut(rest, "/")
	if !ok || action != "facts" || nodeID == "" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		FactIDs     []string `json:"fact_ids"`
		TenantID    string   `json:"tenant_id,omitempty"`
		PrincipalID string   `json:"principal_id,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.FactIDs) == 0 {
		http.Error(w, "fact_ids must not be empty", http.StatusBadRequest)
		return
	}
	if len(req.FactIDs) > batchLimit {
		http.Error(w, fmt.Sprintf("fact_ids must not exceed %d items", batchLimit), http.StatusBadRequest)
		return
	}
	identity, status, msg := resolveIdentity(r, req.TenantID, req.PrincipalID)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	ctx := r.Context()
	exists, err := withPrincipalContext(ctx, s.db, identity, func(q querier) (bool, error) {
		return hierarchyNodeExists(ctx, q, identity.TenantID, nodeID)
	})
	if err != nil {
		http.Error(w, "load hierarchy node: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !exists {
		http.Error(w, "hierarchy node not found", http.StatusNotFound)
		return
	}
	factIDs, err := s.resolveActiveFacts(ctx, identity, req.FactIDs)
	if writeFactResolutionError(w, err) {
		return
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		http.Error(w, "begin transaction: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	if err := setPrincipalContext(ctx, tx, identity); err != nil {
		http.Error(w, "set principal context: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := addHierarchyFacts(ctx, tx, identity.TenantID, nodeID, factIDs); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "commit: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeHierarchyNode(ctx, w, identity, nodeID)
}

func writeFactResolutionError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	if unknown, ok := err.(*unknownFactError); ok {
		http.Error(w, unknown.Error(), http.StatusUnprocessableEntity)
		return true
	}
	http.Error(w, "load facts: "+err.Error(), http.StatusInternalServerError)
	return true
}

// writeHierarchyNode answers with the node as stored, its membership and
// count computed the same way GET /v1/hierarchy computes them.
func (s *engineServer) writeHierarchyNode(ctx context.Context, w http.ResponseWriter, identity requestIdentity, nodeID string) {
	nodes, err := withPrincipalContext(ctx, s.db, identity, func(q querier) ([]*HierarchyNode, error) {
		return loadHierarchy(ctx, q, identity.TenantID)
	})
	if err != nil {
		http.Error(w, "load hierarchy: "+err.Error(), http.StatusInternalServerError)
		return
	}
	for _, n := range nodes {
		if n.ID == nodeID {
			writeJSON(w, n)
			return
		}
	}
	http.Error(w, "hierarchy node "+nodeID+" was stored but is not visible to the caller", http.StatusInternalServerError)
}
