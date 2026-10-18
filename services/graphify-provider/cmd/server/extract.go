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

// jobTokenLifetime is how long each token an extraction job signs for its
// writes is valid; jobTokenRenewMargin is how close to expiry it is renewed.
const (
	jobTokenLifetime    = 5 * time.Minute
	jobTokenRenewMargin = time.Minute
)

// errCallerTokenExpired stops a job that has no signing secret once the
// caller's own token has expired.
var errCallerTokenExpired = errors.New("the caller's token expired before ingestion finished; re-run the extraction")

// jobCredential is the bearer an extraction job writes under. A large graph
// takes far longer to ingest than the caller's short-lived token lives, so
// with the issuer secret the job signs fresh short-lived tokens carrying
// exactly the caller's claims -- same tenant, principal, agent and scopes,
// so memory-intelligence enforces the same authority -- and never past the
// job's own deadline. Without the secret it uses the caller's token and
// reports errCallerTokenExpired once that has expired.
type jobCredential struct {
	mu       sync.Mutex
	claims   auth.Claims
	secret   string
	deadline time.Time
	token    string
	expires  time.Time
	now      func() time.Time
}

func newJobCredential(claims auth.Claims, callerToken, secret string, deadline time.Time) *jobCredential {
	return &jobCredential{claims: claims, secret: secret, deadline: deadline,
		token: callerToken, expires: claims.ExpiresAt, now: time.Now}
}

func (c *jobCredential) bearer() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.secret == "" {
		if !c.expires.IsZero() && !now.Before(c.expires) {
			return "", errCallerTokenExpired
		}
		return c.token, nil
	}
	if now.Add(jobTokenRenewMargin).Before(c.expires) {
		return c.token, nil
	}
	if !now.Before(c.deadline) {
		return "", context.DeadlineExceeded
	}
	expires := now.Add(jobTokenLifetime)
	if expires.After(c.deadline) {
		expires = c.deadline
	}
	jti := make([]byte, 12)
	if _, err := rand.Read(jti); err != nil {
		return "", err
	}
	claims := c.claims
	claims.ExpiresAt = expires
	claims.JTI = "graphify-extract-" + hex.EncodeToString(jti)
	token, err := auth.SignClaims(claims, c.secret)
	if err != nil {
		return "", err
	}
	c.token, c.expires = token, expires
	return token, nil
}

// maxReportedErrors caps the per-job error list; error_count keeps the total.
const maxReportedErrors = 50

// extractJob is one ingestion of a repository's graph into memory, written
// as the principal who requested it. It runs detached from the request --
// a large repository is tens of thousands of nodes and edges -- and its
// progress is read back through GET /v1/graphify/extract/{job_id}.
type extractJob struct {
	mu       sync.Mutex
	tenantID string
	status   extractJobStatus
}

// extractJobStatus is an ingestion job as GET /v1/graphify/extract/{id}
// reports it.
type extractJobStatus struct {
	ID            string    `json:"job_id"`
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
	j.status.ErrorCount++
	if len(j.status.Errors) < maxReportedErrors {
		j.status.Errors = append(j.status.Errors, msg)
	}
}

func (j *extractJob) snapshot() extractJobStatus {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := j.status
	out.Errors = append([]string(nil), j.status.Errors...)
	return out
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
	job := &extractJob{tenantID: claims.TenantID, status: extractJobStatus{
		ID: hex.EncodeToString(idBytes), RepoPath: workDir, NamespaceID: namespaceID,
		Status: "ingesting", Nodes: len(graph.Nodes), Edges: len(graph.Edges), StartedAt: time.Now().UTC(),
	}}
	s.jobsMu.Lock()
	if s.jobs == nil {
		s.jobs = make(map[string]*extractJob)
	}
	s.jobs[job.status.ID] = job
	s.jobsMu.Unlock()
	go s.ingest(job, newJobCredential(claims, bearer, s.issuerSecret, time.Now().Add(extractIngestTimeout)), graph)

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
	if job == nil || job.tenantID != claims.TenantID {
		http.Error(w, "no such extraction job", http.StatusNotFound)
		return
	}
	writeJSON(w, job.snapshot())
}

func (s *graphifyServer) ingest(job *extractJob, cred *jobCredential, graph *graphDocument) {
	ctx, cancel := context.WithTimeout(context.Background(), extractIngestTimeout)
	defer cancel()
	// stopped is why ingestion stopped early, if it did.
	var stopped error
	stop := func(err error) {
		stopped = err
		cancel()
	}
	memoryIDByNode := make(map[string]string, len(graph.Nodes))
	for _, node := range graph.Nodes {
		bearer, err := cred.bearer()
		if err != nil {
			stop(err)
			break
		}
		memoryID, err := s.ingestNode(ctx, bearer, job.status.NamespaceID, node)
		if err != nil {
			job.recordError("node " + node.ID + ": " + err.Error())
			if errors.Is(err, context.DeadlineExceeded) {
				break
			}
			continue
		}
		memoryIDByNode[node.ID] = memoryID
		job.mu.Lock()
		job.status.NodesIngested++
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
		bearer, err := cred.bearer()
		if err != nil {
			stop(err)
			break
		}
		if err := s.ingestEdge(ctx, bearer, sourceID, targetID, edge); err != nil {
			job.recordError("edge " + edge.Source + "->" + edge.Target + ": " + err.Error())
			continue
		}
		job.mu.Lock()
		job.status.EdgesIngested++
		job.mu.Unlock()
	}
	job.mu.Lock()
	defer job.mu.Unlock()
	job.status.FinishedAt = time.Now().UTC()
	switch {
	case stopped != nil || ctx.Err() != nil:
		if stopped == nil {
			stopped = ctx.Err()
		}
		job.status.Status = "failed"
		if len(job.status.Errors) < maxReportedErrors {
			job.status.Errors = append(job.status.Errors, "ingestion stopped: "+stopped.Error())
		}
		job.status.ErrorCount++
	case job.status.NodesIngested == 0 && job.status.Nodes > 0:
		job.status.Status = "failed"
	case job.status.ErrorCount > 0:
		job.status.Status = "completed_with_errors"
	default:
		job.status.Status = "completed"
	}
}
