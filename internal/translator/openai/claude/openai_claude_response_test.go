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

func TestConvertOpenAIResponseToClaude_EmitsMessageDeltaOnDoneWithoutFinishReason(t *testing.T) {
	originalRequest := []byte(`{"stream":true}`)
	var param any

	part := `{"id":"chat","object":"chat.completion.chunk","created":1,"model":"glm-4.7","choices":[{"index":0,"delta":{"content":"Hello"}}]}`
	out1 := ConvertOpenAIResponseToClaude(context.Background(), "", originalRequest, nil, []byte("data: "+part+"\n"), &param)
	out2 := ConvertOpenAIResponseToClaude(context.Background(), "", originalRequest, nil, []byte("data: [DONE]\n"), &param)

	joined := strings.Join(append(out1, out2...), "")
	if !strings.Contains(joined, "event: message_delta") {
		t.Fatalf("expected message_delta before message_stop, got: %q", joined)
	}
	if !strings.Contains(joined, "event: message_stop") {
		t.Fatalf("expected message_stop, got: %q", joined)
	}

	foundStopReason := ""
	for _, line := range strings.Split(joined, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
			continue
		}
		if payload["type"] != "message_delta" {
			continue
		}
		delta, _ := payload["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		if v, ok := delta["stop_reason"].(string); ok {
			foundStopReason = v
			break
		}
	}
	if foundStopReason != "end_turn" {
		t.Fatalf("expected stop_reason %q, got %q (full=%q)", "end_turn", foundStopReason, joined)
	}
}

func TestConvertOpenAIResponseToClaude_DedupesTextSnapshots(t *testing.T) {
	originalRequest := []byte(`{"stream":true}`)
	var param any

	part1 := `{"id":"chat","object":"chat.completion.chunk","created":1,"model":"glm-4.7","choices":[{"index":0,"delta":{"content":"Hello"}}]}`
	part2 := `{"id":"chat","object":"chat.completion.chunk","created":1,"model":"glm-4.7","choices":[{"index":0,"delta":{"content":"Hello world"}}]}`

	out1 := ConvertOpenAIResponseToClaude(context.Background(), "", originalRequest, nil, []byte("data: "+part1+"\n"), &param)
	out2 := ConvertOpenAIResponseToClaude(context.Background(), "", originalRequest, nil, []byte("data: "+part2+"\n"), &param)

	joined := strings.Join(append(out1, out2...), "")

	var deltas []string
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
		if delta == nil || delta["type"] != "text_delta" {
			continue
		}
		if text, ok := delta["text"].(string); ok {
			deltas = append(deltas, text)
		}
	}
	got := strings.Join(deltas, "")
	if got != "Hello world" {
		t.Fatalf("expected reconstructed text %q, got %q (deltas=%v full=%q)", "Hello world", got, deltas, joined)
	}
	if len(deltas) >= 2 && deltas[1] == "Hello world" {
		t.Fatalf("expected second delta to be a suffix, got full snapshot %q (deltas=%v)", deltas[1], deltas)
	}
}

func TestToolUseIDMapping_RewritesToolResultToUpstreamID(t *testing.T) {
	originalRequest := []byte(`{"stream":true,"_seed":"TestToolUseIDMapping_RewritesToolResultToUpstreamID"}`)
	var param any

	upstreamID := "call_upstream_1"
	chunk := `{"id":"chatcmpl_abc","object":"chat.completion.chunk","created":1,"model":"glm-4.7","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"` + upstreamID + `","type":"function","function":{"name":"Update","arguments":"{\"path\":\".gitignore\",\"patch\":\"noop\"}"}}]}}]}`
	out := ConvertOpenAIResponseToClaude(context.Background(), "", originalRequest, nil, []byte("data: "+chunk+"\n"), &param)

	stableID := ""
	for _, line := range strings.Split(strings.Join(out, ""), "\n") {
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
		if cb == nil || cb["type"] != "tool_use" {
			continue
		}
		stableID, _ = cb["id"].(string)
		break
	}
	if stableID == "" {
		t.Fatalf("expected tool_use content_block_start id, got: %q", strings.Join(out, ""))
	}
	if stableID == upstreamID {
		t.Fatalf("expected stable tool_use id to differ from upstream id %q", upstreamID)
	}

	claudeReq := `{"model":"claude-sonnet-latest","stream":false,"messages":[` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"` + stableID + `","name":"Update","input":{"path":".gitignore","patch":"noop"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + stableID + `","content":"Error editing file"}]}` +
		`]}`

	openAIReqBytes := ConvertClaudeRequestToOpenAI("glm-4.7", []byte(claudeReq), false)
	var openAIReq map[string]any
	if err := json.Unmarshal(openAIReqBytes, &openAIReq); err != nil {
		t.Fatalf("expected openai request json, got err=%v body=%q", err, string(openAIReqBytes))
	}

	msgs, _ := openAIReq["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatalf("expected openai messages, got: %q", string(openAIReqBytes))
	}

	foundToolCallID := ""
	foundToolResultID := ""
	for _, m := range msgs {
		msg, _ := m.(map[string]any)
		if msg == nil {
			continue
		}
		role, _ := msg["role"].(string)
		switch role {
		case "assistant":
			if tcAny, ok := msg["tool_calls"].([]any); ok && len(tcAny) > 0 {
				tc0, _ := tcAny[0].(map[string]any)
				if tc0 != nil {
					if id, ok := tc0["id"].(string); ok {
						foundToolCallID = id
					}
				}
			}
		case "tool":
			if id, ok := msg["tool_call_id"].(string); ok {
				foundToolResultID = id
			}
		}
	}

	if foundToolCallID != upstreamID {
		t.Fatalf("expected assistant tool_call id %q, got %q (body=%q)", upstreamID, foundToolCallID, string(openAIReqBytes))
	}
	if foundToolResultID != upstreamID {
		t.Fatalf("expected tool_result tool_call_id %q, got %q (body=%q)", upstreamID, foundToolResultID, string(openAIReqBytes))
	}
}
