package main

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dylmarriner/scos-memory/packages/telemetry"
)

// A client can finish the first JSON object but never send the final chunk.
// The second JSON decode must not leave a code-query worker blocked forever.
func TestPGSingleJSONReadDeadlineOnIncompleteBody(t *testing.T) {
	const deadline = 120 * time.Millisecond
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Query string `json:"query"`
		}
		if err := decodeSinglePGJSON(w, r, &request, deadline); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	// Production wraps handlers in telemetry, whose response recorder must
	// expose the underlying network connection to http.ResponseController.
	server := httptest.NewServer(telemetry.Instrument("code-intelligence-test", handler))
	defer server.Close()

	for _, tc := range []struct {
		name    string
		framing string
	}{
		{"chunked", "Transfer-Encoding: chunked"},
		{"large-content-length", "Content-Length: 300000"},
		{"incomplete-first-object", "Content-Length: 300000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
				t.Fatal(err)
			}
			body := `{"query":"pkg.fn"}`
			if tc.name == "incomplete-first-object" {
				body = `{"query":`
			}
			if tc.name == "chunked" {
				_, err = fmt.Fprintf(conn, "POST / HTTP/1.1\r\nHost: test\r\n%s\r\nConnection: close\r\n\r\n%x\r\n%s\r\n", tc.framing, len(body), body)
			} else {
				_, err = fmt.Fprintf(conn, "POST / HTTP/1.1\r\nHost: test\r\n%s\r\nConnection: close\r\n\r\n%s", tc.framing, body)
			}
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			response, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatalf("incomplete body left handler blocked: %v", err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("expected HTTP 400 on stalled trailing decode, got %d", response.StatusCode)
			}
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Fatalf("request exceeded bounded read time: %s", elapsed)
			}
		})
	}
}

func TestPGSingleJSONReadDeadlineAllowsCompleteKeepAliveRequests(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Query string `json:"query"`
		}
		if err := decodeSinglePGJSON(w, r, &request, 150*time.Millisecond); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	server := httptest.NewServer(telemetry.Instrument("code-intelligence-test", handler))
	defer server.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	body := `{"query":"pkg.fn"}`
	for i := 0; i < 2; i++ {
		if _, err := fmt.Fprintf(conn, "POST / HTTP/1.1\r\nHost: test\r\nContent-Length: %d\r\n\r\n%s", len(body), body); err != nil {
			t.Fatal(err)
		}
		response, err := http.ReadResponse(reader, nil)
		if err != nil {
			t.Fatalf("keep-alive request %d: %v", i+1, err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			t.Fatalf("keep-alive request %d: got HTTP %d", i+1, response.StatusCode)
		}
	}
}
