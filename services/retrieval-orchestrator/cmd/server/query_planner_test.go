package main

import "testing"

func TestPlanRetrievalClassifiesDeterministically(t *testing.T) {
	tests := []struct {
		name string
		req  RetrieveRequest
		mode string
	}{
		{name: "exact navigation", req: RetrieveRequest{Query: "services/retrieval-orchestrator/cmd/server/main.go"}, mode: "exact_navigation"},
		{name: "memory", req: RetrieveRequest{Query: "what did I tell you about my deployment preference"}, mode: "memory"},
		{name: "code", req: RetrieveRequest{Query: "find the function that resolves namespace membership"}, mode: "code"},
		{name: "temporal", req: RetrieveRequest{Query: "what changed yesterday in the deployment"}, mode: "temporal"},
		{name: "mixed", req: RetrieveRequest{Query: "explain the architecture tradeoffs"}, mode: "mixed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			first := planRetrieval(tc.req, 20)
			second := planRetrieval(tc.req, 20)
			if first != second {
				t.Fatalf("planner is nondeterministic: %#v != %#v", first, second)
			}
			if first.Mode != tc.mode {
				t.Fatalf("mode = %q, want %q", first.Mode, tc.mode)
			}
		})
	}
}

func TestPlanRetrievalControlsLaneEligibilityAndBudgets(t *testing.T) {
	exact := planRetrieval(RetrieveRequest{Query: "pkg.(*Store).QueryDense"}, 24)
	if exact.VectorLimit != 0 || exact.RelationLimit != 0 || exact.SessionLimit != 0 {
		t.Fatalf("exact navigation should avoid speculative lanes: %#v", exact)
	}
	if exact.ExactLimit != 24 || exact.FullTextLimit <= 0 {
		t.Fatalf("exact navigation lost precision lanes: %#v", exact)
	}

	memory := planRetrieval(RetrieveRequest{Query: "remember what I said about storage", IncludeSession: true, SessionID: "session-1"}, 24)
	if memory.VectorLimit == 0 || memory.FullTextLimit == 0 || memory.RelationLimit == 0 || memory.SessionLimit == 0 {
		t.Fatalf("memory mode should use memory-oriented lanes: %#v", memory)
	}

	code := planRetrieval(RetrieveRequest{Query: "find code implementation for token allocation", IncludeSession: true, SessionID: "session-1"}, 24)
	if code.VectorLimit == 0 || code.FullTextLimit == 0 || code.SessionLimit != 0 {
		t.Fatalf("code mode should use lexical+dense but not chat-session recency: %#v", code)
	}
}

func TestPlanRetrievalDoesNotContainSecurityScope(t *testing.T) {
	plan := planRetrieval(RetrieveRequest{
		Query:          "latest memory",
		TenantID:       "tenant-sensitive",
		PrincipalID:    "principal-sensitive",
		Namespaces:     []string{"private"},
		SensitivityMax: "restricted",
	}, 10)
	// The planner type deliberately contains only mode and candidate budgets.
	// Security scope stays in RetrieveRequest and is applied by the existing
	// authorised namespace + RLS path after planning.
	if plan.Mode == "" || plan.ExactLimit < 0 || plan.FullTextLimit < 0 || plan.VectorLimit < 0 || plan.RelationLimit < 0 || plan.SessionLimit < 0 {
		t.Fatalf("invalid plan: %#v", plan)
	}
}
