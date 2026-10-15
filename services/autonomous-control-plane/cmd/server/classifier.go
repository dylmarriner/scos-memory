package main

import (
	"regexp"
	"sort"
	"strings"
)

// Multi-label intent classification (GOVERNANCE.md §21.5).
//
// Classification is deterministic: it combines fixed indicators, structured
// client metadata and active task state. Explicit signals -- a repository
// path, a proposed tool, an artifact reference, an operation type or a safety
// flag -- carry authoritative confidence and override weaker phrase matches,
// as §21.5 requires. Labels that were considered but did not meet the
// admission threshold are returned as alternatives rather than discarded, so
// the classifier never hides uncertainty behind one compulsory category.

// The complete set of intent labels §21.5 requires.
var requiredIntentLabels = []string{
	"conversation",
	"memory_recall",
	"memory_correction",
	"fact_lookup",
	"fresh_external_information",
	"code_lookup",
	"code_change",
	"repository_analysis",
	"artifact_read",
	"artifact_write",
	"command_execution",
	"tool_operation",
	"workflow_continuation",
	"planning",
	"monitoring",
	"recovery",
	"high_impact_action",
}

// Confidence bands. Explicit structured signals are authoritative; phrase
// matches are weaker and can be outranked by an explicit signal.
const (
	confidenceExplicit = 0.97
	confidenceStrong   = 0.85
	confidencePhrase   = 0.62
	// admissionThreshold is the minimum confidence for a label to be applied.
	// Below it, the label is reported as a considered alternative instead.
	admissionThreshold = 0.60
)

// LabelScore is one classification outcome with the evidence that produced it.
type LabelScore struct {
	Label      string   `json:"label"`
	Confidence float64  `json:"confidence"`
	Evidence   []string `json:"evidence"`
}

type classification struct {
	Labels       []LabelScore
	Alternatives []LabelScore
	Confidence   float64
}

// scoreboard accumulates the highest-confidence evidence per label.
type scoreboard struct {
	scores map[string]*LabelScore
}

func newScoreboard() *scoreboard {
	return &scoreboard{scores: make(map[string]*LabelScore, len(requiredIntentLabels))}
}

// observe records evidence for a label, keeping the strongest confidence seen.
func (s *scoreboard) observe(label string, confidence float64, evidence string) {
	existing, ok := s.scores[label]
	if !ok {
		s.scores[label] = &LabelScore{Label: label, Confidence: confidence, Evidence: []string{evidence}}
		return
	}
	existing.Evidence = append(existing.Evidence, evidence)
	if confidence > existing.Confidence {
		existing.Confidence = confidence
	}
}

// phraseGroup binds a label to the phrases that weakly indicate it.
type phraseGroup struct {
	label   string
	phrases []string
}

var phraseIndicators = []phraseGroup{
	{"memory_recall", []string{"remember", "recall", "last time", "previously", "we discussed", "you said"}},
	{"memory_correction", []string{"that's wrong", "thats wrong", "actually no", "correction", "not correct", "update that", "i misspoke", "forget that"}},
	{"fact_lookup", []string{"what is", "who is", "when did", "define", "look up", "how many"}},
	{"fresh_external_information", []string{"current", "latest", "today", "right now", "real-time", "real time", "up to date"}},
	{"code_lookup", []string{"source code", "function", "class", "stack trace", "where is", "implementation of", ".go", ".py", ".ts", ".tsx", ".js"}},
	{"code_change", []string{"refactor", "fix the bug", "implement", "add a test", "rename", "migrate", "patch", "rewrite"}},
	{"repository_analysis", []string{"repository", "repo", "codebase", "architecture", "dependency graph"}},
	{"artifact_read", []string{"open the file", "read the file", "show me the document", "attached file", "uploaded"}},
	{"artifact_write", []string{"save to file", "write a file", "generate a document", "export to", "produce a report"}},
	{"command_execution", []string{"run command", "terminal", "shell", "execute the script"}},
	{"tool_operation", []string{"deploy", "install", "restart service", "call the api", "trigger the"}},
	{"workflow_continuation", []string{"continue", "carry on", "next step", "resume", "where were we", "pick up where"}},
	{"planning", []string{"plan", "design", "strategy", "how should we", "approach for", "break down"}},
	{"monitoring", []string{"status of", "health of", "is it up", "metrics", "dashboard", "alert"}},
	{"recovery", []string{"crashed", "recover", "rollback", "restore", "failed run", "interrupted"}},
	{"high_impact_action", []string{"delete", "remove access", "send email", "make payment", "production deploy", "permission", "drop table", "revoke", "destructive"}},
	{"compile", []string{"compile", "build", "test failure"}},
}

// contextCompilation matches "compile" whose object is the context itself
// ("compile context", "compile a minimal context capsule", "compile the
// working context"): that is context assembly, not code work.
var contextCompilation = regexp.MustCompile(`\bcompil\w*\b[^.;:!?]{0,40}\b(context|capsule)\b`)

// commandRequest matches an explicit request to run a command in any of its
// common phrasings ("execute the command", "run the proxy command safely",
// "run a read-only command", "issue the execute command").
var commandRequest = regexp.MustCompile(`\b(run|execute|exec|issue|invoke)\b[^.;:!?]{0,40}\bcommands?\b`)

// proxyExecution matches work addressed to the execution proxy itself
// ("through the safe execution proxy", "a destructive proxy command").
var proxyExecution = regexp.MustCompile(`\b(proxy|shell|terminal)\s+commands?\b|\bthrough\b[^.;:!?]{0,30}\bproxy\b`)

// classifyIntent produces the multi-label classification for a request.
func classifyIntent(req RouteRequest) classification {
	board := newScoreboard()
	input := strings.ToLower(req.Input)

	// Every interaction is at minimum a conversation that may draw on memory;
	// §21.9 makes automatic recall unconditional rather than input-dependent.
	board.observe("conversation", confidenceExplicit, "mandatory_interception")
	board.observe("memory_recall", confidenceStrong, "automatic_memory_retrieval")

	// Explicit structured signals: authoritative, and strong enough to
	// override any competing phrase match.
	if strings.TrimSpace(req.RepositoryPath) != "" {
		board.observe("repository_analysis", confidenceExplicit, "explicit_repository_path")
		board.observe("code_lookup", confidenceExplicit, "explicit_repository_path")
	}
	if strings.TrimSpace(req.ProposedTool) != "" {
		board.observe("tool_operation", confidenceExplicit, "explicit_proposed_tool")
	}
	if strings.TrimSpace(req.ArtifactID) != "" {
		board.observe("artifact_read", confidenceExplicit, "explicit_artifact_reference")
	}
	if strings.TrimSpace(req.TaskID) != "" || strings.TrimSpace(req.WorkflowState) != "" {
		board.observe("workflow_continuation", confidenceExplicit, "active_task_state")
	}
	if req.RequiresFreshData {
		board.observe("fresh_external_information", confidenceExplicit, "client_freshness_flag")
	}
	switch strings.ToLower(strings.TrimSpace(req.OperationType)) {
	case "read":
		board.observe("artifact_read", confidenceExplicit, "explicit_operation_type")
	case "write":
		board.observe("artifact_write", confidenceExplicit, "explicit_operation_type")
	case "execute":
		board.observe("command_execution", confidenceExplicit, "explicit_operation_type")
	case "correct":
		board.observe("memory_correction", confidenceExplicit, "explicit_operation_type")
	case "recover":
		board.observe("recovery", confidenceExplicit, "explicit_operation_type")
	}
	// Safety flags override weaker semantic guesses.
	risk := strings.ToLower(strings.TrimSpace(req.RiskClass))
	if risk == "high" || risk == "critical" {
		board.observe("high_impact_action", confidenceExplicit, "explicit_risk_class")
	}

	// Weaker phrase indicators.
	for _, group := range phraseIndicators {
		for _, p := range group.phrases {
			if strings.Contains(input, p) {
				label := group.label
				// "compile"/"build"/"test failure" indicate code work rather
				// than a label of their own.
				if label == "compile" {
					if p == "compile" && contextCompilation.MatchString(input) {
						continue
					}
					board.observe("code_lookup", confidencePhrase, "phrase:"+p)
					board.observe("repository_analysis", confidencePhrase, "phrase:"+p)
					continue
				}
				board.observe(label, confidencePhrase, "phrase:"+p)
			}
		}
	}

	if commandRequest.MatchString(input) {
		board.observe("command_execution", confidencePhrase, "phrase:command_request")
	}
	if proxyExecution.MatchString(input) {
		board.observe("command_execution", confidencePhrase, "phrase:proxy_execution")
	}

	// A code change implies the repository must be analysed and read first.
	if sc, ok := board.scores["code_change"]; ok && sc.Confidence >= admissionThreshold {
		board.observe("code_lookup", confidenceStrong, "implied_by_code_change")
		board.observe("repository_analysis", confidenceStrong, "implied_by_code_change")
	}
	// Executing a command is always a tool operation.
	if sc, ok := board.scores["command_execution"]; ok && sc.Confidence >= admissionThreshold {
		board.observe("tool_operation", confidenceStrong, "implied_by_command_execution")
	}

	var labels, alternatives []LabelScore
	for _, sc := range board.scores {
		if sc.Confidence >= admissionThreshold {
			labels = append(labels, *sc)
		} else {
			alternatives = append(alternatives, *sc)
		}
	}
	byConfidenceThenLabel := func(s []LabelScore) func(i, j int) bool {
		return func(i, j int) bool {
			if s[i].Confidence != s[j].Confidence {
				return s[i].Confidence > s[j].Confidence
			}
			return s[i].Label < s[j].Label
		}
	}
	sort.Slice(labels, byConfidenceThenLabel(labels))
	sort.Slice(alternatives, byConfidenceThenLabel(alternatives))

	// Overall confidence is the weakest admitted label: the decision is only
	// as trustworthy as its least certain component.
	overall := 1.0
	for _, l := range labels {
		if l.Confidence < overall {
			overall = l.Confidence
		}
	}
	if len(labels) == 0 {
		overall = 0
	}
	return classification{Labels: labels, Alternatives: alternatives, Confidence: overall}
}

// has reports whether a label was admitted.
func (c classification) has(label string) bool {
	for _, l := range c.Labels {
		if l.Label == label {
			return true
		}
	}
	return false
}

// labelNames returns the admitted label names in sorted order.
func (c classification) labelNames() []string {
	out := make([]string, 0, len(c.Labels))
	for _, l := range c.Labels {
		out = append(out, l.Label)
	}
	sort.Strings(out)
	return out
}
