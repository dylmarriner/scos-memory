package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// A valid prefix with ambiguous object members must not change an operator-
// approved canary cohort. Go's default JSON decoder otherwise uses the last
// matching member, including case-folded aliases and nested objects.
func TestCanaryStartRejectsDuplicateJSONMembersBeforePersistence(t *testing.T) {
	for _, tc := range []struct {
		name string
		body func(string) string
	}{
		{"duplicate_scope", func(m string) string {
			return fmt.Sprintf(`{"manifest":%s,"scope":{"agent_ids":["allowed"]},"scope":{"agent_ids":["other"]}}`, m)
		}},
		{"case_folded_scope", func(m string) string {
			return fmt.Sprintf(`{"manifest":%s,"scope":{"agent_ids":["allowed"]},"Scope":{"agent_ids":["other"]}}`, m)
		}},
		{"escaped_scope", func(m string) string {
			return fmt.Sprintf(`{"manifest":%s,"scope":{"agent_ids":["allowed"]},"\u0073cope":{"agent_ids":["other"]}}`, m)
		}},
		{"duplicate_nested_scope", func(m string) string {
			return fmt.Sprintf(`{"manifest":%s,"scope":{"organisation":false,"organisation":true}}`, m)
		}},
		{"duplicate_manifest_field", func(m string) string {
			return fmt.Sprintf(`{"manifest":%s,"scope":{"agent_ids":["allowed"]}}`, strings.Replace(m, `"version":"2.0.0"`, `"version":"1.0.0","version":"2.0.0"`, 1))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, key := canaryFixture(t)
			server := canaryHTTP(t, srv)
			raw, err := json.Marshal(canaryManifest(t, key, "2.0.0"))
			if err != nil {
				t.Fatal(err)
			}
			status, response := canaryPostHTTP(t, server, "/v1/extensions/canary.ext/canary", strings.NewReader(tc.body(string(raw))))
			if status != http.StatusBadRequest {
				t.Fatalf("ambiguous canary JSON accepted: status=%d body=%s", status, response)
			}
			if len(srv.canaries) != 0 {
				t.Fatalf("rejected request changed canary state: %+v", srv.canaries)
			}
			if got := resolve(t, newExtServerAt(srv.storePath), CanaryContext{AgentID: "allowed"}); got.Channel != "stable" {
				t.Fatalf("rejected request persisted candidate routing: %+v", got)
			}
		})
	}
}

func TestCanaryResolveRejectsDuplicateCallerIdentity(t *testing.T) {
	srv, key := canaryFixture(t)
	if res := startCanary(t, srv, canaryManifest(t, key, "2.0.0"), CanaryScope{AgentIDs: []string{"allowed"}}); res.Code != http.StatusCreated {
		t.Fatalf("start canary: %d %s", res.Code, res.Body.String())
	}
	server := canaryHTTP(t, srv)
	for _, body := range []string{
		`{"agent_id":"other","agent_id":"allowed"}`,
		`{"agent_id":"other","Agent_ID":"allowed"}`,
		`{"agent_id":"other","\\u0061gent_id":"allowed"}`,
	} {
		status, response := canaryPostHTTP(t, server, "/v1/extensions/canary.ext/canary/resolve", strings.NewReader(body))
		if status != http.StatusBadRequest {
			t.Fatalf("ambiguous identity accepted: status=%d body=%s input=%s", status, response, body)
		}
	}
	if got := resolve(t, srv, CanaryContext{AgentID: "allowed"}); got.Channel != "canary" {
		t.Fatalf("unambiguous caller rejected: %+v", got)
	}
}
