package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dylmarriner/scos-memory/packages/auth"
)

func TestParseTurnLimit(t *testing.T) {
	tests := []struct {
		name, value string
		want        int
		wantErr     bool
	}{
		{name: "default", want: 50},
		{name: "requested", value: "12", want: 12},
		{name: "bounded", value: "1000", want: 100},
		{name: "invalid", value: "0", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseTurnLimit(tt.value)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %t", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Fatalf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestLive(t *testing.T) {
	s := server{version: "test", dialTimeout: time.Second}
	req := httptest.NewRequest(http.MethodGet, "/health/live", nil)
	res := httptest.NewRecorder()

	s.routes().ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d", http.StatusOK, res.Code)
	}
}

func TestRoutesRejectUnauthenticatedAPIRequests(t *testing.T) {
	handler := auth.AuthenticatedHandler((server{version: "test"}).routes(), auth.Config{Token: "secret"})
	req := httptest.NewRequest(http.MethodGet, "/v1/version", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("expected %d, got %d", http.StatusUnauthorized, res.Code)
	}
}

func TestReadyRejectsMissingDatabaseURL(t *testing.T) {
	s := server{version: "test", dialTimeout: 20 * time.Millisecond}
	req := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	res := httptest.NewRecorder()

	s.routes().ServeHTTP(res, req)
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected %d, got %d", http.StatusServiceUnavailable, res.Code)
	}
}

func TestReadyAcceptsReachablePostgresAddress(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	s := server{
		version:     "test",
		databaseURL: "postgres://user:pass@" + listener.Addr().String() + "/db",
		dialTimeout: time.Second,
	}
	req := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	res := httptest.NewRecorder()

	s.routes().ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d", http.StatusOK, res.Code)
	}
}

// A TCP listener is not a PostgreSQL connection. Readiness must not
// turn green merely because another process accepts packets on port 5432.
func TestReadyRejectsTCPListenerWithoutDatabaseConnection(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	s := server{
		version:     "test",
		databaseURL: "postgres://user:pass@" + listener.Addr().String() + "/db",
		dialTimeout: 20 * time.Millisecond,
	}
	req := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	res := httptest.NewRecorder()
	s.routes().ServeHTTP(res, req)
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("TCP listener without db handle must be unready, got HTTP %d", res.Code)
	}
}

func TestPostgresAddressDefaultsPort(t *testing.T) {
	got, err := postgresAddress("postgres://user:pass@db/scos")
	if err != nil {
		t.Fatal(err)
	}
	if got != "db:5432" {
		t.Fatalf("expected db:5432, got %s", got)
	}
}
