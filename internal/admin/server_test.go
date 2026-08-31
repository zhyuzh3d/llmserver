package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLocalOnlyRejectsNonLoopbackPeer(t *testing.T) {
	handler := localOnly(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/admin/", nil)
	request.RemoteAddr = "192.0.2.10:5000"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestLocalOnlyRejectsDNSRebindingHost(t *testing.T) {
	handler := localOnly(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/admin/api/secrets", nil)
	request.RemoteAddr = "127.0.0.1:5000"
	request.Host = "attacker.example:4816"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestSameOriginRejectsCrossSiteMutation(t *testing.T) {
	handler := sameOrigin(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	request := httptest.NewRequest(http.MethodPut, "http://127.0.0.1:4816/admin/api/config", nil)
	request.Host = "127.0.0.1:4816"
	request.Header.Set("Origin", "https://malicious.example")
	request.RemoteAddr = "127.0.0.1:5000"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestSameOriginAcceptsLocalUIAndNonBrowserClient(t *testing.T) {
	handler := sameOrigin(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	for _, origin := range []string{"http://127.0.0.1:4816", ""} {
		request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4816/admin/api/tokens/generate", nil)
		request.Host = "127.0.0.1:4816"
		if origin != "" {
			request.Header.Set("Origin", origin)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Fatalf("origin %q status = %d", origin, response.Code)
		}
	}
}
