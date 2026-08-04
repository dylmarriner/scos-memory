package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func testJWT(secret, payload string) string {
	header := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9"
	body := base64.RawURLEncoding.EncodeToString([]byte(payload))
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(header + "." + body))
	return fmt.Sprintf("%s.%s.%s", header, body, base64.RawURLEncoding.EncodeToString(mac.Sum(nil)))
}

func TestAuthenticatedHandlerRequiresBearerToken(t *testing.T) {
	handler := AuthenticatedHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if Identity(r.Context()) != "worker" {
			t.Errorf("identity = %q, want worker", Identity(r.Context()))
		}
		w.WriteHeader(http.StatusNoContent)
	}), Config{Token: "secret", Identity: "worker"})

	for _, test := range []struct {
		name   string
		header string
		want   int
	}{
		{name: "missing", want: http.StatusUnauthorized},
		{name: "wrong", header: "Bearer no", want: http.StatusUnauthorized},
		{name: "valid", header: "Bearer secret", want: http.StatusNoContent},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/private", nil)
			req.Header.Set("Authorization", test.header)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			if recorder.Code != test.want {
				t.Fatalf("status = %d, want %d", recorder.Code, test.want)
			}
		})
	}
}

func TestAuthenticatedHandlerLeavesHealthRoutesPublic(t *testing.T) {
	handler := AuthenticatedHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), Config{Token: "secret"})
	for _, path := range []string{"/health/live", "/health/ready"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusNoContent {
			t.Errorf("%s status = %d, want %d", path, recorder.Code, http.StatusNoContent)
		}
	}
}

func TestAuthenticatedHandlerUsesSignedCanonicalClaims(t *testing.T) {
	token := testJWT("issuer", `{"tenant_id":"tenant-a","principal_id":"principal-a"}`)
	handler := AuthenticatedHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, ok := CanonicalClaims(r.Context())
		if !ok || claims.TenantID != "tenant-a" || claims.PrincipalID != "principal-a" {
			t.Fatalf("claims = %+v, ok=%v", claims, ok)
		}
		w.WriteHeader(http.StatusNoContent)
	}), Config{IssuerSecret: "issuer"})
	req := httptest.NewRequest(http.MethodGet, "/v1/private", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d", recorder.Code)
	}
}

func TestControlPlaneHandlerRejectsDirectBearerAndAcceptsControlPlaneToken(t *testing.T) {
	handler := ControlPlaneHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), Config{Token: "service", ControlPlaneToken: "control-plane"})

	for _, test := range []struct {
		name          string
		controlHeader string
		want          int
	}{
		{name: "direct", want: http.StatusUnauthorized},
		{name: "wrong control token", controlHeader: "wrong", want: http.StatusUnauthorized},
		{name: "control plane", controlHeader: "control-plane", want: http.StatusNoContent},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/governed", nil)
			req.Header.Set("Authorization", "Bearer service")
			if test.controlHeader != "" {
				req.Header.Set("X-SCOS-Control-Plane-Token", test.controlHeader)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d", response.Code, test.want)
			}
		})
	}
}

func TestControlPlaneHandlerLeavesHealthRoutesPublic(t *testing.T) {
	handler := ControlPlaneHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), Config{Token: "service", ControlPlaneToken: "control-plane"})

	for _, path := range []string{"/health/live", "/health/ready"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusNoContent {
			t.Errorf("%s status = %d, want %d", path, response.Code, http.StatusNoContent)
		}
	}
}
