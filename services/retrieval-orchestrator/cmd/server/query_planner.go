package main

import "strings"

type retrievalPlan struct {
	Mode          string `json:"mode"`
	ExactLimit    int    `json:"exact_limit"`
	FullTextLimit int    `json:"fulltext_limit"`
	VectorLimit   int    `json:"vector_limit"`
	RelationLimit int    `json:"relation_limit"`
	SessionLimit  int    `json:"session_limit"`
	CodeLimit     int    `json:"code_limit"`
}

func planRetrieval(req RetrieveRequest, maxResults int) retrievalPlan {
	if maxResults <= 0 {
		maxResults = 20
	}
	q := strings.ToLower(strings.TrimSpace(req.Query))
	mode := classifyRetrievalQuery(q)
	half := maxResults / 2
	if half < 1 {
		half = 1
	}
	third := maxResults / 3
	if third < 1 {
		third = 1
	}
	session := 0
	if req.IncludeSession && strings.TrimSpace(req.SessionID) != "" {
		session = third
	}

	codeLimit := 0
	hasRepo := strings.TrimSpace(req.RepositoryID) != "" && strings.TrimSpace(req.Branch) != ""
	switch mode {
	case "exact_navigation":
		if hasRepo {
			codeLimit = maxResults
		}
		return retrievalPlan{Mode: mode, ExactLimit: maxResults, FullTextLimit: half, CodeLimit: codeLimit}
	case "memory":
		return retrievalPlan{Mode: mode, ExactLimit: maxResults, FullTextLimit: maxResults, VectorLimit: maxResults, RelationLimit: half, SessionLimit: session}
	case "code":
		if hasRepo {
			codeLimit = maxResults
		}
		return retrievalPlan{Mode: mode, ExactLimit: half, FullTextLimit: half, VectorLimit: half, RelationLimit: third, CodeLimit: codeLimit}
	case "temporal":
		if hasRepo {
			codeLimit = half
		}
		return retrievalPlan{Mode: mode, ExactLimit: half, FullTextLimit: maxResults, VectorLimit: half, RelationLimit: half, SessionLimit: session, CodeLimit: codeLimit}
	default:
		if hasRepo {
			codeLimit = maxResults
		}
		return retrievalPlan{Mode: "mixed", ExactLimit: maxResults, FullTextLimit: maxResults, VectorLimit: maxResults, RelationLimit: half, SessionLimit: session, CodeLimit: codeLimit}
	}
}

func classifyRetrievalQuery(q string) string {
	if looksLikeExactNavigation(q) {
		return "exact_navigation"
	}
	if containsAny(q,
		"yesterday", "today", "latest", "recent", "recently", "last week", "last month",
		"what changed", "changed since", "new since", "timeline", "previous version",
	) {
		return "temporal"
	}
	if containsAny(q,
		"remember", "memory", "what did i tell", "what did we discuss", "what have i said",
		"my preference", "my preferences", "i told you", "i said", "we talked about", "recall",
	) {
		return "memory"
	}
	if containsAny(q,
		"function", "method", "class", "symbol", "code", "implementation", "repository", " repo ",
		"callers", "callee", "package", "struct", "interface", "source file", "source code",
	) {
		return "code"
	}
	return "mixed"
}

func looksLikeExactNavigation(q string) bool {
	if q == "" {
		return false
	}
	if strings.Contains(q, "(*") || strings.Contains(q, "::") {
		return true
	}
	if strings.Contains(q, "/") && !strings.ContainsAny(q, " \t\n") {
		return true
	}
	for _, ext := range []string{".go", ".py", ".ts", ".tsx", ".js", ".jsx", ".rs", ".java", ".kt", ".kts", ".sql", ".md"} {
		if strings.HasSuffix(q, ext) || strings.Contains(q, ext+":") {
			return true
		}
	}
	if strings.Count(q, ".") >= 2 && !strings.ContainsAny(q, " \t\n") {
		return true
	}
	return false
}

func containsAny(value string, needles ...string) bool {
	padded := " " + value + " "
	for _, needle := range needles {
		if strings.Contains(padded, needle) || strings.Contains(value, needle) {
			return true
		}
	}
	return false
}
