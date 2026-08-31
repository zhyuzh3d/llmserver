package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/zhyuzh3d/llmserver/internal/auth"
	"github.com/zhyuzh3d/llmserver/internal/gateway"
	"github.com/zhyuzh3d/llmserver/internal/pricing"
)

const maxRequestBytes = 10 << 20

type Server struct {
	runtime func() (*auth.Authenticator, *gateway.Service)
	mux     *http.ServeMux
}

func NewServer(authenticator *auth.Authenticator, gatewayService *gateway.Service) *Server {
	return NewDynamicServer(func() (*auth.Authenticator, *gateway.Service) { return authenticator, gatewayService })
}

func NewDynamicServer(runtime func() (*auth.Authenticator, *gateway.Service)) *Server {
	server := &Server{runtime: runtime, mux: http.NewServeMux()}
	server.mux.HandleFunc("GET /healthz", server.handleHealth)
	server.mux.HandleFunc("GET /readyz", server.handleReady)
	server.mux.HandleFunc("GET /v1/models", server.handleModels)
	server.mux.HandleFunc("POST /v1/responses", server.handleResponses)
	return server
}

func (s *Server) Handler() http.Handler { return s.mux }

type responsesRequest struct {
	Model           string          `json:"model"`
	Instructions    json.RawMessage `json:"instructions"`
	Input           json.RawMessage `json:"input"`
	Stream          bool            `json:"stream"`
	MaxOutputTokens *int64          `json:"max_output_tokens"`
	Reasoning       json.RawMessage `json:"reasoning"`
	Tools           json.RawMessage `json:"tools"`
	ToolChoice      json.RawMessage `json:"tool_choice"`
	Store           *bool           `json:"store"`
	Metadata        json.RawMessage `json:"metadata"`
	LLMServer       struct {
		IdempotencyKey string `json:"idempotency_key"`
		Budget         *struct {
			MaxCharge string `json:"max_charge"`
			Currency  string `json:"currency"`
			Mode      string `json:"mode"`
		} `json:"budget"`
	} `json:"llmserver"`
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	_, gatewayService := s.runtime()
	if gatewayService == nil || !gatewayService.Ready() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	client, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	_, gatewayService := s.runtime()
	models := gatewayService.Models(client)
	data := make([]map[string]any, 0, len(models))
	for _, model := range models {
		data = append(data, map[string]any{
			"id":       model.ID,
			"object":   "model",
			"created":  0,
			"owned_by": "llmserver",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	client, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		writeAPIError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request_too_large", "request body is too large", "")
		return
	}
	var request responsesRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&request); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "invalid_json", "request body is not valid JSON", "")
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "invalid_json", "request body must contain exactly one JSON value", "")
		return
	}
	if request.Model == "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "missing_model", "model is required", "model")
		return
	}
	if request.Store != nil && *request.Store {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "unsupported_feature", "store=true is not supported", "store")
		return
	}
	if len(bytes.TrimSpace(request.Input)) == 0 || bytes.Equal(bytes.TrimSpace(request.Input), []byte("null")) {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "missing_input", "input is required", "input")
		return
	}
	if request.MaxOutputTokens != nil && *request.MaxOutputTokens < 1 {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "invalid_max_output_tokens", "max_output_tokens must be positive", "max_output_tokens")
		return
	}
	if len(request.LLMServer.IdempotencyKey) > 256 {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "invalid_idempotency_key", "idempotency_key must be at most 256 characters", "llmserver.idempotency_key")
		return
	}
	if hasNonEmptyJSONValue(request.Tools) {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "unsupported_feature", "function tools are not implemented in this Stage 1 slice", "tools")
		return
	}
	instructions, err := canonicalJSONText(request.Instructions)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "invalid_instructions", "instructions must be valid JSON", "instructions")
		return
	}
	inputText, err := canonicalJSONText(request.Input)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "invalid_input", "input must be valid JSON", "input")
		return
	}
	canonicalInput := strings.TrimSpace(strings.Join([]string{instructions, inputText}, "\n"))

	gatewayRequest := gateway.Request{
		Model:           request.Model,
		Instructions:    instructions,
		Input:           request.Input,
		CanonicalInput:  canonicalInput,
		MaxOutputTokens: request.MaxOutputTokens,
		RawRequest:      body,
		IdempotencyKey:  request.LLMServer.IdempotencyKey,
	}
	if request.LLMServer.Budget != nil {
		gatewayRequest.Budget = &gateway.BudgetRequest{
			Mode:      pricing.BudgetMode(request.LLMServer.Budget.Mode),
			Currency:  request.LLMServer.Budget.Currency,
			MaxCharge: request.LLMServer.Budget.MaxCharge,
		}
	}

	_, gatewayService := s.runtime()
	events, requestID, err := gatewayService.Start(r.Context(), client, gatewayRequest)
	if err != nil {
		gatewayError := gateway.AsError(err)
		if gatewayError.RequestID != "" {
			w.Header().Set("x-llmserver-request-id", gatewayError.RequestID)
		}
		writeAPIError(w, gatewayError.Status, "invalid_request_error", gatewayError.Code, gatewayError.Message, "")
		return
	}
	w.Header().Set("x-llmserver-request-id", requestID)
	strict := strings.EqualFold(r.Header.Get("x-llmserver-compatibility"), "strict")
	if request.Stream {
		s.writeResponseStream(r.Context(), w, events, requestID, request.Model, strict)
		return
	}
	s.writeResponse(w, events, strict)
}

func (s *Server) writeResponse(w http.ResponseWriter, events <-chan gateway.Event, strict bool) {
	var completed *gateway.Event
	var failed *gateway.Error
	for event := range events {
		if event.Type == gateway.EventRunCompleted {
			copy := event
			completed = &copy
		}
		if event.Type == gateway.EventRunFailed {
			failed = event.Error
		}
	}
	if failed != nil {
		writeAPIError(w, failed.Status, "server_error", failed.Code, failed.Message, "")
		return
	}
	if completed == nil {
		writeAPIError(w, http.StatusBadGateway, "server_error", "provider_incomplete", "provider ended without a completed response", "")
		return
	}
	response := completed.Response
	response["usage"] = map[string]any{
		"input_tokens":  completed.Billing.Usage.InputTokens,
		"output_tokens": completed.Billing.Usage.OutputTokens,
		"total_tokens":  completed.Billing.Usage.InputTokens + completed.Billing.Usage.OutputTokens,
	}
	if !strict {
		response["llmserver_billing"] = completed.Billing
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) writeResponseStream(ctx context.Context, w http.ResponseWriter, events <-chan gateway.Event, requestID, model string, strict bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAPIError(w, http.StatusInternalServerError, "server_error", "streaming_unsupported", "response writer does not support streaming", "")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	writeSSE(w, "response.created", map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id":     strings.Replace(requestID, "req_", "resp_", 1),
			"object": "response",
			"status": "in_progress",
			"model":  model,
			"output": []any{},
		},
	})
	flusher.Flush()

	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-events:
			if !ok {
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
				flusher.Flush()
				return
			}
			switch event.Type {
			case gateway.EventOutputTextDelta:
				writeSSE(w, "response.output_text.delta", map[string]any{"type": "response.output_text.delta", "delta": event.Delta})
			case gateway.EventBillingCompleted:
				if !strict {
					writeSSE(w, "llmserver.billing.completed", map[string]any{"type": "llmserver.billing.completed", "billing": event.Billing})
				}
			case gateway.EventRunCompleted:
				event.Response["usage"] = map[string]any{
					"input_tokens":  event.Billing.Usage.InputTokens,
					"output_tokens": event.Billing.Usage.OutputTokens,
					"total_tokens":  event.Billing.Usage.InputTokens + event.Billing.Usage.OutputTokens,
				}
				if !strict {
					event.Response["llmserver_billing"] = event.Billing
				}
				writeSSE(w, "response.completed", map[string]any{"type": "response.completed", "response": event.Response})
			case gateway.EventRunFailed:
				code := "provider_stream_failed"
				message := "provider stream failed before completion"
				if event.Error != nil {
					code = event.Error.Code
					message = event.Error.Message
				}
				writeSSE(w, "error", map[string]any{"type": "error", "code": code, "message": message})
			}
			flusher.Flush()
		}
	}
}

func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (*auth.Client, bool) {
	authenticator, _ := s.runtime()
	if authenticator == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "server_error", "not_ready", "llmserver runtime is not ready", "")
		return nil, false
	}
	client, err := authenticator.AuthenticateAuthorization(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, http.StatusUnauthorized, "authentication_error", "invalid_api_key", "invalid llmserver API key", "")
		return nil, false
	}
	return client, true
}

func canonicalJSONText(raw json.RawMessage) (string, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", err
	}
	if text, ok := value.(string); ok {
		return text, nil
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(canonical), nil
}

func hasNonEmptyJSONValue(raw json.RawMessage) bool {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return true
	}
	if values, ok := value.([]any); ok {
		return len(values) > 0
	}
	return true
}

func writeSSE(w io.Writer, eventType string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, data)
}

func writeAPIError(w http.ResponseWriter, status int, errorType, code, message, param string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{
		"message": message,
		"type":    errorType,
		"code":    code,
		"param":   nullableString(param),
	}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func RunHTTPServer(ctx context.Context, address string, handler http.Handler) error {
	server := &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
