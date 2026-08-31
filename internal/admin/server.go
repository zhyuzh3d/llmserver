package admin

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/zhyuzh3d/llmserver/internal/discovery"
	"github.com/zhyuzh3d/llmserver/internal/runtimecfg"
	"github.com/zhyuzh3d/llmserver/internal/store"
)

//go:embed web/*
var webFiles embed.FS

type Server struct {
	manager *runtimecfg.Manager
	store   *store.SQLite
	mux     *http.ServeMux
}

func New(manager *runtimecfg.Manager, runStore *store.SQLite) *Server {
	server := &Server{manager: manager, store: runStore, mux: http.NewServeMux()}
	server.mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/admin/", http.StatusTemporaryRedirect)
	})
	assets, _ := fs.Sub(webFiles, "web")
	server.mux.Handle("GET /admin/", http.StripPrefix("/admin/", http.FileServer(http.FS(assets))))
	server.mux.HandleFunc("GET /admin/api/state", server.handleState)
	server.mux.HandleFunc("GET /admin/api/secrets", server.handleSecrets)
	server.mux.HandleFunc("PUT /admin/api/config", server.handleConfigUpdate)
	server.mux.HandleFunc("POST /admin/api/providers/{providerID}/models", server.handleDiscoverModels)
	server.mux.HandleFunc("GET /admin/api/usage", server.handleUsage)
	server.mux.HandleFunc("POST /admin/api/tokens/generate", server.handleGenerateToken)
	return server
}

func (s *Server) Handler() http.Handler {
	return localOnly(sameOrigin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; frame-ancestors 'none'; img-src 'none'; object-src 'none'; connect-src 'self'; script-src 'self'; style-src 'self'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		s.mux.ServeHTTP(w, r)
	})))
}

func (s *Server) handleState(w http.ResponseWriter, _ *http.Request) {
	snapshot := s.manager.Snapshot()
	if snapshot == nil {
		writeError(w, http.StatusServiceUnavailable, "runtime is not ready")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"config": snapshot.Config, "secret_status": s.manager.SecretStatus()})
}

func (s *Server) handleSecrets(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.manager.RevealSecrets())
}

func (s *Server) handleConfigUpdate(w http.ResponseWriter, r *http.Request) {
	var update runtimecfg.Update
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&update); err != nil {
		writeError(w, http.StatusBadRequest, "invalid configuration payload")
		return
	}
	if err := s.manager.Update(r.Context(), update); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "saved"})
}

func (s *Server) handleDiscoverModels(w http.ResponseWriter, r *http.Request) {
	snapshot := s.manager.Snapshot()
	if snapshot == nil {
		writeError(w, http.StatusServiceUnavailable, "runtime is not ready")
		return
	}
	providerID := r.PathValue("providerID")
	var providerIndex = -1
	for i := range snapshot.Config.Providers {
		if snapshot.Config.Providers[i].ID == providerID {
			providerIndex = i
			break
		}
	}
	if providerIndex < 0 {
		writeError(w, http.StatusNotFound, "provider not found")
		return
	}
	models, err := discovery.Models(r.Context(), snapshot.Config.Providers[providerIndex])
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"provider_id": providerID, "models": models})
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()
	summary, err := s.store.UsageSummary(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read usage summary")
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

func (s *Server) handleGenerateToken(w http.ResponseWriter, _ *http.Request) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		writeError(w, http.StatusInternalServerError, "could not generate token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": hex.EncodeToString(value)})
}

func localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil || !isLoopback(host) || !isLoopback(requestHostname(r.Host)) {
			http.Error(w, "local access only", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func requestHostname(hostport string) string {
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		return host
	}
	return strings.Trim(hostport, "[]")
}

func sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			if site := strings.ToLower(r.Header.Get("Sec-Fetch-Site")); site == "cross-site" {
				http.Error(w, "cross-origin request rejected", http.StatusForbidden)
				return
			}
			if rawOrigin := r.Header.Get("Origin"); rawOrigin != "" && !originMatches(rawOrigin, r.Host) {
				http.Error(w, "cross-origin request rejected", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func originMatches(rawOrigin, requestHost string) bool {
	origin, err := url.Parse(rawOrigin)
	if err != nil || (origin.Scheme != "http" && origin.Scheme != "https") {
		return false
	}
	return strings.EqualFold(origin.Host, requestHost)
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func contextWithTimeout(r *http.Request, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), timeout)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"message": message}})
}
