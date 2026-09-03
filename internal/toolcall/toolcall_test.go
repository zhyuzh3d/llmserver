package toolcall

import (
	"encoding/json"
	"strings"
	"testing"
)

const weatherTool = `[{"type":"function","name":"get_weather","description":"Get weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false},"strict":true}]`

func TestParseAndValidateNativeFunctionCall(t *testing.T) {
	request, err := Parse(json.RawMessage(weatherTool), json.RawMessage(`"required"`), nil)
	if err != nil {
		t.Fatal(err)
	}
	response := map[string]any{"output": []any{map[string]any{
		"type": "function_call", "call_id": "call_1", "name": "get_weather", "arguments": `{"city":"上海"}`,
	}}}
	if err := request.ValidateResponse(response); err != nil {
		t.Fatal(err)
	}
}

func TestRejectsInvalidOrUnexpectedFunctionCall(t *testing.T) {
	request, err := Parse(json.RawMessage(weatherTool), json.RawMessage(`"auto"`), nil)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		response map[string]any
		want     string
	}{
		{
			name: "schema",
			response: map[string]any{"output": []any{
				map[string]any{"type": "function_call", "call_id": "call_1", "name": "get_weather", "arguments": `{"city":7}`},
			}},
			want: "JSON Schema",
		},
		{
			name: "unknown",
			response: map[string]any{"output": []any{
				map[string]any{"type": "function_call", "call_id": "call_1", "name": "delete_all", "arguments": `{}`},
			}},
			want: "undeclared",
		},
		{
			name: "multiple",
			response: map[string]any{"output": []any{
				map[string]any{"type": "function_call", "call_id": "call_1", "name": "get_weather", "arguments": `{"city":"上海"}`},
				map[string]any{"type": "function_call", "call_id": "call_2", "name": "get_weather", "arguments": `{"city":"北京"}`},
			}},
			want: "multiple",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := request.ValidateResponse(test.response)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v want %q", err, test.want)
			}
		})
	}
}

func TestToolChoiceAndPortableSubsetValidation(t *testing.T) {
	parallel := true
	if _, err := Parse(json.RawMessage(weatherTool), nil, &parallel); err == nil {
		t.Fatal("parallel calls were accepted")
	}
	if _, err := Parse(json.RawMessage(`[{"type":"web_search_preview"}]`), nil, nil); err == nil {
		t.Fatal("hosted tool was accepted")
	}
	if _, err := Parse(nil, json.RawMessage(`"required"`), nil); err == nil {
		t.Fatal("required choice without tools was accepted")
	}
	request, err := Parse(nil, nil, nil)
	if err != nil || request.Enabled() {
		t.Fatalf("empty tools request=%#v err=%v", request, err)
	}
}
