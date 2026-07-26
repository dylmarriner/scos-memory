package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLive(t *testing.T) {
	s := server{version: "test", dialTimeout: time.Second}
	req := httptest.NewRequest(http.MethodGet, "/health/live", nil)
	res := httptest.NewRecorder()

	s.routes().ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d", http.StatusOK, res.Code)
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

func TestPostgresAddressDefaultsPort(t *testing.T) {
	got, err := postgresAddress("postgres://user:pass@db/scos")
	if err != nil {
		t.Fatal(err)
	}
	if got != "db:5432" {
		t.Fatalf("expected db:5432, got %s", got)
	}
}
