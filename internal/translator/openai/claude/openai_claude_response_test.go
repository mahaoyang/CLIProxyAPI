package claude

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestConvertOpenAIResponseToClaude_DedupesToolUseStart(t *testing.T) {
	originalRequest := []byte(`{"stream":true}`)
	var param any

	chunk := `{"id":"chat","object":"chat.completion.chunk","created":1,"model":"glm-4.7","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Edit","arguments":"{\"path\":\".gitignore\""}}]}}]}`
	raw1 := []byte("data: " + chunk + "\n")
	raw2 := []byte("data: " + chunk + "\n")

	out1 := ConvertOpenAIResponseToClaude(context.Background(), "", originalRequest, nil, raw1, &param)
	out2 := ConvertOpenAIResponseToClaude(context.Background(), "", originalRequest, nil, raw2, &param)

	joined := strings.Join(append(out1, out2...), "")
	starts := strings.Count(joined, `event: content_block_start`)
	if starts == 0 {
		t.Fatalf("expected at least one content_block_start, got 0: %q", joined)
	}

	toolStarts := 0
	for _, line := range strings.Split(joined, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
			continue
		}
		if payload["type"] != "content_block_start" {
			continue
		}
		cb, _ := payload["content_block"].(map[string]any)
		if cb == nil {
			continue
		}
		if cb["type"] == "tool_use" {
			toolStarts++
		}
	}

	if toolStarts != 1 {
		t.Fatalf("expected exactly 1 tool_use content_block_start, got %d", toolStarts)
	}
}

func TestConvertOpenAIResponseToClaude_AccumulatesToolArgumentsOnce(t *testing.T) {
	originalRequest := []byte(`{"stream":true}`)
	var param any

	part1 := `{"id":"chat","object":"chat.completion.chunk","created":1,"model":"glm-4.7","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"command\":\"echo "}}]}}]}`
	part2 := `{"id":"chat","object":"chat.completion.chunk","created":1,"model":"glm-4.7","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"REPRO\"}"}}]}}]}`
	finish := `{"id":"chat","object":"chat.completion.chunk","created":1,"model":"glm-4.7","choices":[{"index":0,"finish_reason":"tool_calls","delta":{}}]}`

	ConvertOpenAIResponseToClaude(context.Background(), "", originalRequest, nil, []byte("data: "+part1+"\n"), &param)
	ConvertOpenAIResponseToClaude(context.Background(), "", originalRequest, nil, []byte("data: "+part2+"\n"), &param)
	out := ConvertOpenAIResponseToClaude(context.Background(), "", originalRequest, nil, []byte("data: "+finish+"\n"), &param)

	joined := strings.Join(out, "")
	var partial string
	for _, line := range strings.Split(joined, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
			continue
		}
		if payload["type"] != "content_block_delta" {
			continue
		}
		delta, _ := payload["delta"].(map[string]any)
		if delta == nil || delta["type"] != "input_json_delta" {
			continue
		}
		partial, _ = delta["partial_json"].(string)
		break
	}
	if partial == "" {
		t.Fatalf("expected input_json_delta partial_json, got: %q", joined)
	}

	var args map[string]any
	if err := json.Unmarshal([]byte(partial), &args); err != nil {
		t.Fatalf("partial_json is not valid JSON: %v; value=%q", err, partial)
	}
	if args["command"] != "echo REPRO" {
		t.Fatalf("expected command to be %q, got %v (partial=%q)", "echo REPRO", args["command"], partial)
	}
}
