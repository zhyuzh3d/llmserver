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
	"strconv"
	"strings"
	"time"

	"github.com/zhyuzh3d/llmserver/internal/config"
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
	server.mux.HandleFunc("POST /admin/api/clients/{clientID}/daily-quota/reset", server.handleDailyQuotaReset)
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

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	snapshot := s.manager.Snapshot()
	if snapshot == nil {
		writeError(w, http.StatusServiceUnavailable, "runtime is not ready")
		return
	}
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()
	quotas, err := s.store.DailyQuotaStatuses(ctx, dailyQuotaSpecs(snapshot.Config.Clients), time.Now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read daily quota status")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"config": snapshot.Config, "secret_status": s.manager.SecretStatus(), "daily_quotas": quotas})
}

func dailyQuotaSpecs(clients []config.ClientConfig) []store.DailyQuotaSpec {
	result := make([]store.DailyQuotaSpec, 0, len(clients))
	for _, client := range clients {
		result = append(result, store.DailyQuotaSpec{ClientID: client.ID, LimitUSD: client.DailyLimitUSD})
	}
	return result
}

func (s *Server) handleDailyQuotaReset(w http.ResponseWriter, r *http.Request) {
	snapshot := s.manager.Snapshot()
	if snapshot == nil {
		writeError(w, http.StatusServiceUnavailable, "runtime is not ready")
		return
	}
	clientID := r.PathValue("clientID")
	var client *config.ClientConfig
	for i := range snapshot.Config.Clients {
		if snapshot.Config.Clients[i].ID == clientID {
			client = &snapshot.Config.Clients[i]
			break
		}
	}
	if client == nil {
		writeError(w, http.StatusNotFound, "access key not found")
		return
	}
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()
	now := time.Now()
	if err := s.store.ResetDailyQuota(ctx, clientID, now); err != nil {
		writeError(w, http.StatusInternalServerError, "could not reset daily quota")
		return
	}
	statuses, err := s.store.DailyQuotaStatuses(ctx, []store.DailyQuotaSpec{{ClientID: client.ID, LimitUSD: client.DailyLimitUSD}}, now)
	if err != nil || len(statuses) != 1 {
		writeError(w, http.StatusInternalServerError, "could not read reset daily quota")
		return
	}
	writeJSON(w, http.StatusOK, statuses[0])
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
	windowValue := 1
	if raw := r.URL.Query().Get("window_value"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "window_value must be an integer")
			return
		}
		windowValue = parsed
	}
	windowUnit := r.URL.Query().Get("window_unit")
	if windowUnit == "" {
		windowUnit = "days"
	}
	var duration time.Duration
	switch windowUnit {
	case "hours":
		if windowValue < 1 || windowValue > 8760 {
			writeError(w, http.StatusBadRequest, "hour window must be between 1 and 8760")
			return
		}
		duration = time.Duration(windowValue) * time.Hour
	case "days":
		if windowValue < 1 || windowValue > 365 {
			writeError(w, http.StatusBadRequest, "day window must be between 1 and 365")
			return
		}
		duration = time.Duration(windowValue) * 24 * time.Hour
	default:
		writeError(w, http.StatusBadRequest, "window_unit must be hours or days")
		return
	}
	groupBy := r.URL.Query().Get("group_by")
	if groupBy == "" {
		groupBy = "provider"
	}
	if groupBy != "provider" && groupBy != "client" {
		writeError(w, http.StatusBadRequest, "group_by must be provider or client")
		return
	}
	var bucketDuration time.Duration
	if raw := r.URL.Query().Get("bucket_minutes"); raw != "" {
		minutes, parseErr := strconv.Atoi(raw)
		if parseErr != nil || (minutes != 5 && minutes != 10 && minutes != 30 && minutes != 60) {
			writeError(w, http.StatusBadRequest, "bucket_minutes must be 5, 10, 30, or 60")
			return
		}
		if groupBy != "client" {
			writeError(w, http.StatusBadRequest, "time-series buckets are only supported for client usage")
			return
		}
		bucketDuration = time.Duration(minutes) * time.Minute
		if duration/bucketDuration > 288 {
			writeError(w, http.StatusBadRequest, "time-series window has too many buckets")
			return
		}
	}
	snapshot := s.manager.Snapshot()
	if snapshot == nil {
		writeError(w, http.StatusServiceUnavailable, "runtime is not ready")
		return
	}
	providers := make(map[string]string, len(snapshot.Config.Deployments))
	for _, deployment := range snapshot.Config.Deployments {
		providers[deployment.ID] = deployment.ProviderID
	}
	until := time.Now().UTC()
	if bucketDuration > 0 {
		until = until.Truncate(bucketDuration).Add(bucketDuration)
	}
	summary, err := s.store.UsageReport(ctx, store.UsageReportFilter{
		Since: until.Add(-duration), Until: until, BucketDuration: bucketDuration, PublicOnly: groupBy == "client" && bucketDuration > 0, GroupBy: groupBy,
		ProviderID: r.URL.Query().Get("provider_id"), ClientID: r.URL.Query().Get("client_id"),
		ProviderByDeployment: providers,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read usage report")
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
