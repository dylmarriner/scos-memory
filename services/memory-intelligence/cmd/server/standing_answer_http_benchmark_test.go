//go:build integration

package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"
)

type p7ReadBenchmarkCase struct {
	name string
	path string
	want int
}

// The same restricted-role fixture feeds both the correctness regression and
// the measured HTTP handler benchmark. It never bypasses the RLS read path.
func setupP7ReadBenchmarkCases(tb testing.TB) (*behaviourFixture, []p7ReadBenchmarkCase) {
	tb.Helper()
	f := newBehaviourFixture(tb)
	ctx := context.Background()
	expiry := time.Now().UTC().Add(3 * time.Second)
	fresh := f.scalar(`INSERT INTO memory_items(
        tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,
        authority_level,confidence_score,sensitivity)
        VALUES($1,$2,$3,'semantic','active','fresh benchmark','fresh benchmark',80,0.9,'internal')
        RETURNING id::text`, f.tenant, f.ns, f.owner)
	revoked := f.scalar(`INSERT INTO memory_items(
        tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,
        authority_level,confidence_score,sensitivity)
        VALUES($1,$2,$3,'semantic','active','revoked benchmark','revoked benchmark',80,0.9,'internal')
        RETURNING id::text`, f.tenant, f.ns, f.owner)
	expiring := f.scalar(`INSERT INTO memory_items(
        tenant_id,namespace_id,owner_principal_id,type,status,title,canonical_summary,
        authority_level,confidence_score,sensitivity,expires_at)
        VALUES($1,$2,$3,'semantic','active','expired benchmark','expired benchmark',80,0.9,'internal',$4)
        RETURNING id::text`, f.tenant, f.ns, f.owner, expiry)
	for _, item := range []struct{ key, fact string }{
		{"p7 bench fresh", fresh},
		{"p7 bench revoked", revoked},
		{"p7 bench expired", expiring},
	} {
		_, err := f.admin.ExecContext(ctx, `INSERT INTO memory_standing_answers(
                tenant_id,namespace_id,question_key,answer,evidence_fact_ids)
                VALUES($1,$2,$3,'verified benchmark answer',$4)`,
			f.tenant, f.ns, item.key, fmt.Sprintf("[%q]", item.fact))
		if err != nil {
			tb.Fatal(err)
		}
	}
	if _, err := f.admin.ExecContext(ctx, `UPDATE memory_items SET status='revoked' WHERE id=$1`, revoked); err != nil {
		tb.Fatal(err)
	}
	if delay := time.Until(expiry.Add(200 * time.Millisecond)); delay > 0 {
		time.Sleep(delay)
	}
	path := func(key string) string {
		return "/v1/standing-answers?namespace_id=" + f.ns + "&question_key=" + url.QueryEscape(key)
	}
	return f, []p7ReadBenchmarkCase{
		{"fresh", path("p7 bench fresh"), http.StatusOK},
		{"revoked", path("p7 bench revoked"), http.StatusNotFound},
		{"clock_expired", path("p7 bench expired"), http.StatusNotFound},
	}
}

func TestP7StandingAnswerBenchmarkCasesFailClosed(t *testing.T) {
	f, cases := setupP7ReadBenchmarkCases(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, _, _ := f.call(f.srv.handleStandingAnswerRead, http.MethodGet, tc.path, "")
			if status != tc.want {
				t.Fatalf("HTTP status=%d, want %d", status, tc.want)
			}
		})
	}
}

// Run explicitly against a disposable migrated PostgreSQL/pgvector fixture:
// go test -tags=integration -run '^$' -bench '^BenchmarkP7StandingAnswerHTTPRead$' -benchmem ./services/memory-intelligence/cmd/server
// This is a reproducible local latency fixture, not production qualification.
func BenchmarkP7StandingAnswerHTTPRead(b *testing.B) {
	f, cases := setupP7ReadBenchmarkCases(b)
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				status, _, _ := f.call(f.srv.handleStandingAnswerRead, http.MethodGet, tc.path, "")
				if status != tc.want {
					b.Fatalf("HTTP status=%d, want %d", status, tc.want)
				}
			}
		})
	}
}
