package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/zhyuzh3d/llmserver/internal/config"
	"github.com/zhyuzh3d/llmserver/internal/pricing"
	"github.com/zhyuzh3d/llmserver/internal/provider"
)

const (
	maxErrorBody = 64 << 10
	maxSSEFrame  = 4 << 20
)

type Adapter struct {
	providerID string
	endpoint   string
	apiKey     config.Secret
	authHeader string
	authPrefix string
	client     *http.Client
}

type Config struct {
	ProviderID   string
	BaseURL      string
	ResponsesURL string
	APIKey       config.Secret
	APIKeyHeader string
	APIKeyPrefix string
}

func New(providerID, baseURL string, apiKey config.Secret, client *http.Client) (*Adapter, error) {
	return NewConfigured(Config{ProviderID: providerID, BaseURL: baseURL, APIKey: apiKey}, client)
}

func NewConfigured(providerConfig Config, client *http.Client) (*Adapter, error) {
	if providerConfig.ProviderID == "" {
		return nil, errors.New("provider id is required")
	}
	endpoint, err := responsesEndpoint(providerConfig.BaseURL, providerConfig.ResponsesURL)
	if err != nil {
		return nil, err
	}
	if providerConfig.APIKey.IsEmpty() {
		return nil, errors.New("provider API key is empty")
	}
	authHeader, authPrefix, err := normalizeAuthentication(providerConfig.APIKeyHeader, providerConfig.APIKeyPrefix)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	return &Adapter{providerID: providerConfig.ProviderID, endpoint: endpoint, apiKey: providerConfig.APIKey, authHeader: authHeader, authPrefix: authPrefix, client: client}, nil
}

func (a *Adapter) ID() string { return a.providerID }

func (a *Adapter) Start(ctx context.Context, request provider.Request) (<-chan provider.Event, error) {
	body, streaming, err := buildRequestBody(request)
	if err != nil {
		return nil, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create provider request: %w", err)
	}
	httpRequest.Header.Set(a.authHeader, credentialValue(a.authPrefix, a.apiKey.Reveal()))
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("User-Agent", "llmserver/0.1")
	if streaming {
		httpRequest.Header.Set("Accept", "text/event-stream")
	}

	response, err := a.client.Do(httpRequest)
	if err != nil {
		return nil, fmt.Errorf("send provider request: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxErrorBody))
		return nil, fmt.Errorf("provider returned HTTP %d", response.StatusCode)
	}

	events := make(chan provider.Event)
	go func() {
		defer close(events)
		defer response.Body.Close()
		if streaming {
			consumeStream(ctx, response.Body, events)
			return
		}
		consumeResponse(ctx, response.Body, events)
	}()
	return events, nil
}

func responsesEndpoint(baseURL, explicitURL string) (string, error) {
	if strings.TrimSpace(explicitURL) != "" {
		return validateEndpoint(explicitURL, "responses")
	}
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid provider base URL")
	}
	path := strings.TrimRight(parsed.Path, "/")
	switch {
	case strings.HasSuffix(path, "/v1/responses"):
	case strings.HasSuffix(path, "/v1"):
		path += "/responses"
	default:
		path += "/v1/responses"
	}
	parsed.Path = path
	parsed.Fragment = ""
	return parsed.String(), nil
}

func validateEndpoint(rawURL, label string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid provider %s URL", label)
	}
	parsed.Fragment = ""
	return parsed.String(), nil
}

func normalizeAuthentication(header, prefix string) (string, string, error) {
	header = strings.TrimSpace(header)
	if header == "" {
		header = "Authorization"
	}
	if strings.ContainsAny(header, "\r\n:") {
		return "", "", errors.New("provider API key header is invalid")
	}
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		prefix = "Bearer"
	} else if strings.EqualFold(prefix, "none") {
		prefix = ""
	}
	if strings.ContainsAny(prefix, "\r\n") {
		return "", "", errors.New("provider API key prefix is invalid")
	}
	return header, prefix, nil
}

func credentialValue(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + " " + key
}

func buildRequestBody(request provider.Request) ([]byte, bool, error) {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(request.RawRequest, &body); err != nil {
		return nil, false, fmt.Errorf("decode provider request: %w", err)
	}
	model, err := json.Marshal(request.UpstreamModel)
	if err != nil {
		return nil, false, err
	}
	body["model"] = model
	delete(body, "llmserver")
	if request.ToolCall != nil && request.ToolCall.Enabled() {
		body["tools"] = request.ToolCall.Tools
		body["tool_choice"] = request.ToolCall.ToolChoice
		body["parallel_tool_calls"] = json.RawMessage("false")
	}
	var streaming bool
	if raw, ok := body["stream"]; ok {
		if err := json.Unmarshal(raw, &streaming); err != nil {
			return nil, false, errors.New("stream must be a boolean")
		}
	}
	encoded, err := json.Marshal(body)
	return encoded, streaming, err
}

func consumeResponse(ctx context.Context, body io.Reader, events chan<- provider.Event) {
	var response map[string]any
	decoder := json.NewDecoder(io.LimitReader(body, 64<<20))
	decoder.UseNumber()
	if err := decoder.Decode(&response); err != nil {
		send(ctx, events, provider.Event{Type: provider.EventFailed, Err: fmt.Errorf("decode provider response: %w", err)})
		return
	}
	final := finalFromResponse(response)
	send(ctx, events, provider.Event{Type: provider.EventCompleted, Final: &final})
}

func consumeStream(ctx context.Context, body io.Reader, events chan<- provider.Event) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64<<10), maxSSEFrame)
	var dataLines []string
	completed := false
	flush := func() bool {
		if len(dataLines) == 0 {
			return true
		}
		data := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		if data == "[DONE]" {
			return true
		}
		var event map[string]any
		decoder := json.NewDecoder(strings.NewReader(data))
		decoder.UseNumber()
		if err := decoder.Decode(&event); err != nil {
			send(ctx, events, provider.Event{Type: provider.EventFailed, Err: fmt.Errorf("decode provider stream event: %w", err)})
			return false
		}
		eventType, _ := event["type"].(string)
		switch eventType {
		case "response.output_text.delta":
			delta, _ := event["delta"].(string)
			return send(ctx, events, provider.Event{Type: provider.EventOutputTextDelta, Delta: delta})
		case "response.completed":
			response, ok := event["response"].(map[string]any)
			if !ok {
				send(ctx, events, provider.Event{Type: provider.EventFailed, Err: errors.New("provider completion has no response")})
				return false
			}
			final := finalFromResponse(response)
			completed = true
			return send(ctx, events, provider.Event{Type: provider.EventCompleted, Final: &final})
		case "error", "response.failed", "response.incomplete":
			send(ctx, events, provider.Event{Type: provider.EventFailed, Err: fmt.Errorf("provider event %s", eventType)})
			return false
		default:
			return true
		}
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if !flush() || completed {
				return
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if len(dataLines) > 0 && !completed {
		if !flush() || completed {
			return
		}
	}
	if err := scanner.Err(); err != nil {
		send(ctx, events, provider.Event{Type: provider.EventFailed, Err: fmt.Errorf("read provider stream: %w", err)})
		return
	}
	if !completed {
		send(ctx, events, provider.Event{Type: provider.EventFailed, Err: errors.New("provider stream ended before completion")})
	}
}

func finalFromResponse(response map[string]any) provider.Final {
	model, _ := response["model"].(string)
	return provider.Final{
		Response:       response,
		OutputText:     extractOutputText(response),
		EffectiveModel: model,
		Usage:          extractUsage(response),
		Costs:          extractCosts(response),
	}
}

func extractCosts(response map[string]any) []provider.CostObservation {
	usage, _ := response["usage"].(map[string]any)
	for _, container := range []map[string]any{usage, response} {
		if len(container) == 0 {
			continue
		}
		for _, key := range []string{"cost", "costs"} {
			if object, ok := container[key].(map[string]any); ok {
				if observation, valid := costObject(object, stringValue(container["currency"])); valid {
					return []provider.CostObservation{observation}
				}
			}
		}
		if total, ok := decimalValue(firstValue(container, "total_cost", "cost")); ok {
			unit := stringValue(firstValue(container, "currency", "unit"))
			return []provider.CostObservation{{Unit: unit, Total: total}}
		}
	}
	return nil
}

func costObject(object map[string]any, fallbackUnit string) (provider.CostObservation, bool) {
	unit := stringValue(firstValue(object, "currency", "unit"))
	if unit == "" {
		unit = fallbackUnit
	}
	input, _ := decimalValue(firstValue(object, "input", "input_cost"))
	output, _ := decimalValue(firstValue(object, "output", "output_cost"))
	total, totalOK := decimalValue(firstValue(object, "total", "total_cost"))
	if !totalOK && input != "" && output != "" {
		left, leftErr := pricing.ParseDecimal(input)
		right, rightErr := pricing.ParseDecimal(output)
		if leftErr == nil && rightErr == nil {
			if sum, err := left.Add(right); err == nil {
				total = sum.String()
				totalOK = true
			}
		}
	}
	return provider.CostObservation{Unit: unit, Input: input, Output: output, Total: total}, totalOK
}

func firstValue(values map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := values[key]; ok {
			return value
		}
	}
	return nil
}

func stringValue(value any) string {
	text, _ := value.(string)
	return strings.ToUpper(strings.TrimSpace(text))
}

func decimalValue(value any) (string, bool) {
	var raw string
	switch item := value.(type) {
	case json.Number:
		raw = item.String()
	case string:
		raw = item
	default:
		return "", false
	}
	parsed, err := pricing.ParseDecimal(raw)
	if err != nil || parsed.Nanos() < 0 {
		return "", false
	}
	return parsed.String(), true
}

func extractUsage(response map[string]any) pricing.ReportedUsage {
	usage, _ := response["usage"].(map[string]any)
	input, inputOK := integer(usage["input_tokens"])
	output, outputOK := integer(usage["output_tokens"])
	return pricing.ReportedUsage{
		InputTokens:  pricing.OptionalCount{Value: input, Present: inputOK},
		OutputTokens: pricing.OptionalCount{Value: output, Present: outputOK},
	}
}

func integer(value any) (int64, bool) {
	switch item := value.(type) {
	case json.Number:
		parsed, err := item.Int64()
		return parsed, err == nil && parsed >= 0
	case float64:
		parsed := int64(item)
		return parsed, item == float64(parsed) && parsed >= 0
	default:
		return 0, false
	}
}

func extractOutputText(response map[string]any) string {
	output, _ := response["output"].([]any)
	var text strings.Builder
	for _, itemValue := range output {
		item, _ := itemValue.(map[string]any)
		content, _ := item["content"].([]any)
		for _, contentValue := range content {
			part, _ := contentValue.(map[string]any)
			if part["type"] == "output_text" {
				value, _ := part["text"].(string)
				text.WriteString(value)
			}
		}
	}
	return text.String()
}

func send(ctx context.Context, target chan<- provider.Event, event provider.Event) bool {
	select {
	case <-ctx.Done():
		return false
	case target <- event:
		return true
	}
}
