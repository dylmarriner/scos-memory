package database

import "testing"

func TestRequireTLS(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  string
		ok   bool
	}{
		{"require", "postgres://user:pass@db/scos?sslmode=require", true},
		{"verify ca", "postgresql://user:pass@db/scos?sslmode=verify-ca", true},
		{"verify full", "postgres://user:pass@db/scos?sslmode=verify-full", true},
		{"disable", "postgres://user:pass@db/scos?sslmode=disable", false},
		{"missing", "postgres://user:pass@db/scos", false},
		{"prefer", "postgres://user:pass@db/scos?sslmode=prefer", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := RequireTLS(tc.url)
			if (err == nil) != tc.ok {
				t.Fatalf("RequireTLS(%q) error = %v, want success = %v", tc.url, err, tc.ok)
			}
		})
	}
}
