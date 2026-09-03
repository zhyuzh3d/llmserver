package toolcall

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

const (
	CapabilityUnsupported = "unsupported"
	CapabilityNative      = "native"
	CapabilityEmulated    = "emulated"

	maxFunctions  = 32
	maxToolsBytes = 256 << 10
)

var functionNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Request is the validated, request-scoped function-tool contract. It keeps
// the original OpenAI-compatible JSON for lossless provider forwarding and
// resolved schemas for validating model-generated arguments before exposure.
type Request struct {
	Tools      json.RawMessage
	ToolChoice json.RawMessage
	functions  map[string]function
	choice     choice
}

type function struct {
	name      string
	validator *jsonschema.Resolved
}

type choice struct {
	mode string
	name string
}

type functionWire struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict,omitempty"`
}

// Parse validates the portable Stage 1 subset of OpenAI Responses function
// tools. parallel_tool_calls is deliberately restricted to false.
func Parse(toolsRaw, choiceRaw json.RawMessage, parallel *bool) (*Request, error) {
	if parallel != nil && *parallel {
		return nil, errors.New("parallel_tool_calls=true is not supported")
	}
	toolsRaw = bytes.TrimSpace(toolsRaw)
	if len(toolsRaw) == 0 || bytes.Equal(toolsRaw, []byte("null")) {
		toolsRaw = json.RawMessage("[]")
	}
	if len(toolsRaw) > maxToolsBytes {
		return nil, fmt.Errorf("tools must not exceed %d bytes", maxToolsBytes)
	}
	var wires []functionWire
	decoder := json.NewDecoder(bytes.NewReader(toolsRaw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wires); err != nil {
		return nil, errors.New("tools must be an array of function definitions")
	}
	if len(wires) > maxFunctions {
		return nil, fmt.Errorf("tools must contain at most %d functions", maxFunctions)
	}

	request := &Request{Tools: cloneRaw(toolsRaw), functions: make(map[string]function, len(wires))}
	for index, wire := range wires {
		if wire.Type != "function" {
			return nil, fmt.Errorf("tools[%d].type must be function", index)
		}
		if !functionNamePattern.MatchString(wire.Name) {
			return nil, fmt.Errorf("tools[%d].name must use 1-64 letters, digits, underscore, or hyphen", index)
		}
		if _, duplicate := request.functions[wire.Name]; duplicate {
			return nil, fmt.Errorf("function name %q is duplicated", wire.Name)
		}
		if len(bytes.TrimSpace(wire.Parameters)) == 0 || bytes.Equal(bytes.TrimSpace(wire.Parameters), []byte("null")) {
			return nil, fmt.Errorf("tools[%d].parameters is required", index)
		}
		var schema jsonschema.Schema
		if err := json.Unmarshal(wire.Parameters, &schema); err != nil {
			return nil, fmt.Errorf("tools[%d].parameters is not a valid JSON Schema", index)
		}
		if schema.Type != "object" {
			return nil, fmt.Errorf("tools[%d].parameters must have type object", index)
		}
		resolved, err := schema.Resolve(nil)
		if err != nil {
			return nil, fmt.Errorf("tools[%d].parameters is not a resolvable JSON Schema: %w", index, err)
		}
		request.functions[wire.Name] = function{name: wire.Name, validator: resolved}
	}

	parsedChoice, normalizedChoice, err := parseChoice(choiceRaw, len(wires) > 0)
	if err != nil {
		return nil, err
	}
	if parsedChoice.name != "" {
		if _, exists := request.functions[parsedChoice.name]; !exists {
			return nil, fmt.Errorf("tool_choice names unknown function %q", parsedChoice.name)
		}
	}
	request.choice = parsedChoice
	request.ToolChoice = normalizedChoice
	return request, nil
}

func parseChoice(raw json.RawMessage, hasTools bool) (choice, json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		mode := "none"
		if hasTools {
			mode = "auto"
		}
		return choice{mode: mode}, json.RawMessage(strconvQuote(mode)), nil
	}
	var mode string
	if json.Unmarshal(raw, &mode) == nil {
		mode = strings.ToLower(strings.TrimSpace(mode))
		if mode != "none" && mode != "auto" && mode != "required" {
			return choice{}, nil, errors.New("tool_choice must be none, auto, required, or a named function")
		}
		if !hasTools && mode != "none" {
			return choice{}, nil, errors.New("tool_choice requires at least one function")
		}
		return choice{mode: mode}, json.RawMessage(strconvQuote(mode)), nil
	}
	var named struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&named); err != nil || named.Type != "function" || !functionNamePattern.MatchString(named.Name) {
		return choice{}, nil, errors.New("named tool_choice must be {\"type\":\"function\",\"name\":\"...\"}")
	}
	normalized, _ := json.Marshal(named)
	return choice{mode: "named", name: named.Name}, normalized, nil
}

func strconvQuote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func cloneRaw(value json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), value...)
}

func (r *Request) Enabled() bool { return r != nil && len(r.functions) > 0 }

// ValidateResponse enforces the single-function contract and validates every
// arguments object before it crosses llmserver's public boundary.
func (r *Request) ValidateResponse(response map[string]any) error {
	if r == nil {
		return nil
	}
	output, ok := response["output"].([]any)
	if !ok {
		if r.choice.mode == "required" || r.choice.mode == "named" {
			return errors.New("provider did not return the required function call")
		}
		return nil
	}
	calls := 0
	calledName := ""
	for index, value := range output {
		item, ok := value.(map[string]any)
		if !ok || item["type"] != "function_call" {
			continue
		}
		calls++
		calledName, _ = item["name"].(string)
		callID, _ := item["call_id"].(string)
		arguments, _ := item["arguments"].(string)
		if !functionNamePattern.MatchString(calledName) || strings.TrimSpace(callID) == "" || strings.TrimSpace(arguments) == "" {
			return fmt.Errorf("output[%d] contains an incomplete function_call", index)
		}
		definition, exists := r.functions[calledName]
		if !exists {
			return fmt.Errorf("provider called undeclared function %q", calledName)
		}
		var argumentValue any
		decoder := json.NewDecoder(strings.NewReader(arguments))
		if err := decoder.Decode(&argumentValue); err != nil {
			return fmt.Errorf("function %q returned invalid JSON arguments", calledName)
		}
		if err := ensureEOF(decoder); err != nil {
			return fmt.Errorf("function %q returned multiple JSON values", calledName)
		}
		if err := definition.validator.Validate(argumentValue); err != nil {
			return fmt.Errorf("function %q arguments do not match its JSON Schema: %w", calledName, err)
		}
	}
	if calls > 1 {
		return errors.New("provider returned multiple function calls while parallel_tool_calls is false")
	}
	switch r.choice.mode {
	case "none":
		if calls != 0 {
			return errors.New("provider returned a function call while tool_choice is none")
		}
	case "required":
		if calls != 1 {
			return errors.New("provider did not return the required function call")
		}
	case "named":
		if calls != 1 || calledName != r.choice.name {
			return fmt.Errorf("provider did not call required function %q", r.choice.name)
		}
	}
	return nil
}

func ensureEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return errors.New("extra JSON value")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// EstimatedOutputText returns a deterministic fallback basis when a provider
// omits output usage on a function-call-only response.
func EstimatedOutputText(response map[string]any) string {
	output, exists := response["output"]
	if !exists {
		return ""
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		return ""
	}
	return string(encoded)
}
