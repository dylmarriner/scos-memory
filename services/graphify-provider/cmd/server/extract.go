package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

// extractIngestTimeout bounds one ingestion of a large graph into memory.
const extractIngestTimeout = 2 * time.Hour

// maxReportedErrors caps the per-job error list; error_count keeps the total.
const maxReportedErrors = 50

// extractJob is one ingestion of a repository's graph into memory, written
// as the principal who requested it. It runs detached from the request --
// a large repository is tens of thousands of nodes and edges -- and its
// progress is read back through GET /v1/graphify/extract/{job_id}.
type extractJob struct {
	mu            sync.Mutex
	ID            string    `json:"job_id"`
	TenantID      string    `json:"-"`
	RepoPath      string    `json:"repo_path"`
	NamespaceID   string    `json:"namespace_id,omitempty"`
	Status        string    `json:"status"`
	Nodes         int       `json:"nodes"`
	Edges         int       `json:"edges"`
	NodesIngested int       `json:"nodes_ingested"`
	EdgesIngested int       `json:"edges_ingested"`
	ErrorCount    int       `json:"error_count"`
	Errors        []string  `json:"errors,omitempty"`
	StartedAt     time.Time `json:"started_at"`
	FinishedAt    time.Time `json:"finished_at,omitzero"`
}

func (j *extractJob) recordError(msg string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.ErrorCount++
	if len(j.Errors) < maxReportedErrors {
		j.Errors = append(j.Errors, msg)
	}
}

func (j *extractJob) snapshot() extractJob {
	j.mu.Lock()
	defer j.mu.Unlock()
	return extractJob{
		ID: j.ID, RepoPath: j.RepoPath, NamespaceID: j.NamespaceID, Status: j.Status,
		Nodes: j.Nodes, Edges: j.Edges, NodesIngested: j.NodesIngested, EdgesIngested: j.EdgesIngested,
		ErrorCount: j.ErrorCount, Errors: append([]string(nil), j.Errors...),
		StartedAt: j.StartedAt, FinishedAt: j.FinishedAt,
	}
}

// handleExtract ingests a repository's graph into memory as the caller.
// The memories belong to the authenticated principal and land in the
// namespace they name (or the deployment's configured one, or the
// principal's own writable namespace); memory-intelligence enforces what
// that principal may write. The graph is the same cached build query,
// path and explain use, so a repository still being graphed answers 202
// "building" here too.
func (s *graphifyServer) handleExtract(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	claims, ok := auth.CanonicalClaims(r.Context())
	bearer := auth.BearerToken(r.Context())
	if !ok || bearer == "" {
		http.Error(w, "extraction writes memories as the caller; a claims-bearing identity is required", http.StatusUnauthorized)
		return
	}
	var req struct {
		RepoPath    string `json:"repo_path,omitempty"`
		NamespaceID string `json:"namespace_id,omitempty"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
	}
	workDir, err := s.resolveWorkDir(req.RepoPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	graphPath, ok := s.cachedGraphOrRespond(w, r, workDir)
	if !ok {
		return
	}
	graph, err := loadGraph(graphPath)
	if err != nil {
		http.Error(w, "load graph.json: "+err.Error(), http.StatusInternalServerError)
		return
	}
	namespaceID := strings.TrimSpace(req.NamespaceID)
	if namespaceID == "" {
		namespaceID = s.namespaceID
	}
	idBytes := make([]byte, 12)
	if _, err := rand.Read(idBytes); err != nil {
		http.Error(w, "allocate job id: "+err.Error(), http.StatusInternalServerError)
		return
	}
	job := &extractJob{
		ID: hex.EncodeToString(idBytes), TenantID: claims.TenantID, RepoPath: workDir, NamespaceID: namespaceID,
		Status: "ingesting", Nodes: len(graph.Nodes), Edges: len(graph.Edges), StartedAt: time.Now().UTC(),
	}
	s.jobsMu.Lock()
	if s.jobs == nil {
		s.jobs = make(map[string]*extractJob)
	}
	s.jobs[job.ID] = job
	s.jobsMu.Unlock()
	go s.ingest(job, bearer, graph)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(job.snapshot())
}

// handleExtractStatus reports an ingestion job's progress to the tenant
// that started it; any other tenant sees no such job.
func (s *graphifyServer) handleExtractStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	claims, ok := auth.CanonicalClaims(r.Context())
	if !ok {
		http.Error(w, "a claims-bearing identity is required", http.StatusUnauthorized)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/graphify/extract/")
	s.jobsMu.Lock()
	job := s.jobs[id]
	s.jobsMu.Unlock()
	if job == nil || job.TenantID != claims.TenantID {
		http.Error(w, "no such extraction job", http.StatusNotFound)
		return
	}
	writeJSON(w, job.snapshot())
}

func (s *graphifyServer) ingest(job *extractJob, bearer string, graph *graphDocument) {
	ctx, cancel := context.WithTimeout(context.Background(), extractIngestTimeout)
	defer cancel()
	memoryIDByNode := make(map[string]string, len(graph.Nodes))
	for _, node := range graph.Nodes {
		memoryID, err := s.ingestNode(ctx, bearer, job.NamespaceID, node)
		if err != nil {
			job.recordError("node " + node.ID + ": " + err.Error())
			if errors.Is(err, context.DeadlineExceeded) {
				break
			}
			continue
		}
		memoryIDByNode[node.ID] = memoryID
		job.mu.Lock()
		job.NodesIngested++
		job.mu.Unlock()
	}
	for _, edge := range graph.Edges {
		if ctx.Err() != nil {
			break
		}
		sourceID, sourceOK := memoryIDByNode[edge.Source]
		targetID, targetOK := memoryIDByNode[edge.Target]
		if !sourceOK || !targetOK {
			job.recordError("edge " + edge.Source + "->" + edge.Target + ": endpoint not ingested")
			continue
		}
		if sourceID == targetID {
			// Both endpoints merged into one memory; a self-relation says
			// nothing.
			continue
		}
		if err := s.ingestEdge(ctx, bearer, sourceID, targetID, edge); err != nil {
			job.recordError("edge " + edge.Source + "->" + edge.Target + ": " + err.Error())
			continue
		}
		job.mu.Lock()
		job.EdgesIngested++
		job.mu.Unlock()
	}
	job.mu.Lock()
	defer job.mu.Unlock()
	job.FinishedAt = time.Now().UTC()
	switch {
	case ctx.Err() != nil:
		job.Status = "failed"
		if len(job.Errors) < maxReportedErrors {
			job.Errors = append(job.Errors, "ingestion stopped: "+ctx.Err().Error())
		}
		job.ErrorCount++
	case job.NodesIngested == 0 && job.Nodes > 0:
		job.Status = "failed"
	case job.ErrorCount > 0:
		job.Status = "completed_with_errors"
	default:
		job.Status = "completed"
	}
}
