package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func useTempReasoningCacheDir(t *testing.T) string {
	t.Helper()

	oldCacheDir := cfg.CacheDir
	cacheDir := t.TempDir()
	cfg.CacheDir = cacheDir
	resetReasoningCacheForTest()

	t.Cleanup(func() {
		cfg.CacheDir = oldCacheDir
		resetReasoningCacheForTest()
	})

	return cacheDir
}

func resetReasoningCacheForTest() {
	evictReasoningCache()
	reasoningCacheMu.Lock()
	reasoningCacheLoaded = false
	reasoningCacheMu.Unlock()
}

func flushReasoningCacheForTest() {
	saveReasoningCache()
	time.Sleep(25 * time.Millisecond)
}

func assertSQLiteReasoningCacheFile(t *testing.T, cacheDir string) {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(cacheDir, "reasoning_cache.sqlite3"))
	if err != nil {
		t.Fatalf("read sqlite reasoning cache: %v", err)
	}
	if !bytes.HasPrefix(data, []byte("SQLite format 3")) {
		t.Fatalf("reasoning cache file is not SQLite, header = %q", string(data[:min(len(data), 16)]))
	}
}

func TestReasoningCachePersistsToSQLiteByToolCallID(t *testing.T) {
	cacheDir := useTempReasoningCacheDir(t)

	storeReasoningContent("call_sqlite_primary", "cached reasoning")
	flushReasoningCacheForTest()

	assertSQLiteReasoningCacheFile(t, cacheDir)

	resetReasoningCacheForTest()
	if got := getReasoningContent("call_sqlite_primary"); got != "cached reasoning" {
		t.Fatalf("getReasoningContent() = %q, want cached reasoning", got)
	}
	if got := getReasoningContent("unrelated_call"); got != "" {
		t.Fatalf("unrelated tool_call_id returned %q, want empty", got)
	}
}

func TestConvertResponsesToChatRequestInjectsSQLiteReasoningByCallID(t *testing.T) {
	cacheDir := useTempReasoningCacheDir(t)

	storeReasoningContent("call_for_conversion", "reasoning for conversion")
	flushReasoningCacheForTest()
	assertSQLiteReasoningCacheFile(t, cacheDir)
	resetReasoningCacheForTest()

	respReq := &ResponsesRequest{
		Model: "gpt-4o",
		Input: json.RawMessage(`[
			{"type":"function_call","call_id":"call_for_conversion","name":"do_work","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_for_conversion","output":"done"}
		]`),
	}

	chatReq, err := ConvertResponsesToChatRequest(respReq)
	if err != nil {
		t.Fatal(err)
	}
	if len(chatReq.Messages) < 1 {
		t.Fatalf("message count = %d, want at least 1", len(chatReq.Messages))
	}
	if got := chatReq.Messages[0].ReasoningContent; got != "reasoning for conversion" {
		t.Fatalf("reasoning_content = %q, want reasoning for conversion", got)
	}
}

func TestReasoningCacheMigratesLegacyJSONToSQLite(t *testing.T) {
	cacheDir := useTempReasoningCacheDir(t)
	legacy := map[string]reasoningEntry{
		"call_legacy": {
			Content:   "legacy reasoning",
			CreatedAt: time.Now(),
		},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "reasoning_cache.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	if got := getReasoningContent("call_legacy"); got != "legacy reasoning" {
		t.Fatalf("getReasoningContent() = %q, want legacy reasoning", got)
	}
	flushReasoningCacheForTest()
	assertSQLiteReasoningCacheFile(t, cacheDir)
}

func TestConvertChatToResponses_MultiTurnWithToolCalls(t *testing.T) {
	chatReq := &ChatCompletionsRequest{
		Model: "gpt-4o",
		Messages: []ChatMessage{
			{Role: "system", Content: json.RawMessage(`"You are a helper."`)},
			{Role: "user", Content: json.RawMessage(`"What is the weather?"`)},
			{
				Role:    "assistant",
				Content: json.RawMessage(`null`),
				ToolCalls: []ToolCall{
					{
						ID:   "call_abc123",
						Type: "function",
						Function: FunctionCall{
							Name:      "get_weather",
							Arguments: `{"city":"Beijing"}`,
						},
					},
				},
			},
			{
				Role:       "tool",
				ToolCallID: "call_abc123",
				Content:    json.RawMessage(`"Sunny, 25°C"`),
			},
			{Role: "user", Content: json.RawMessage(`"Thanks, what about Shanghai?"`)},
		},
	}

	respReq, err := ConvertChatToResponsesRequest(chatReq)
	if err != nil {
		t.Fatalf("conversion error: %v", err)
	}

	if respReq.Model != "gpt-4o" {
		t.Errorf("model = %v, want gpt-4o", respReq.Model)
	}

	// Instructions should come from system message
	if respReq.Instructions == nil || *respReq.Instructions != "You are a helper." {
		t.Errorf("instructions = %v", respReq.Instructions)
	}

	// Parse input (system messages go to Instructions, not input array)
	var inputs []ResponsesInputMessage
	json.Unmarshal(respReq.Input, &inputs)

	// First: user message
	if inputs[0].Role != "user" {
		t.Errorf("input[0] role = %v, want user", inputs[0].Role)
	}

	// Second: assistant function_call
	if inputs[1].Type != "function_call" {
		t.Errorf("input[1] type = %v, want function_call", inputs[1].Type)
	}
	if inputs[1].CallID != "call_abc123" {
		t.Errorf("input[1] call_id = %v, want call_abc123", inputs[1].CallID)
	}

	// Third: function_call_output
	if inputs[2].Type != "function_call_output" {
		t.Errorf("input[2] type = %v, want function_call_output", inputs[2].Type)
	}
	if inputs[2].CallID != "call_abc123" {
		t.Errorf("input[2] call_id = %v, want call_abc123", inputs[2].CallID)
	}

	// Fourth: user message
	if inputs[3].Role != "user" {
		t.Errorf("input[3] role = %v, want user", inputs[3].Role)
	}
}

func TestConvertChatToResponses_AssistantMessageHasID(t *testing.T) {
	chatReq := &ChatCompletionsRequest{
		Model: "gpt-4o",
		Messages: []ChatMessage{
			{Role: "user", Content: json.RawMessage(`"Hello"`)},
			{Role: "assistant", Content: json.RawMessage(`"Hi there!"`)},
			{Role: "user", Content: json.RawMessage(`"How are you?"`)},
		},
	}

	respReq, err := ConvertChatToResponsesRequest(chatReq)
	if err != nil {
		t.Fatalf("conversion error: %v", err)
	}

	var inputs []ResponsesInputMessage
	json.Unmarshal(respReq.Input, &inputs)

	// Find the assistant message (skip system instruction if any)
	for _, inp := range inputs {
		if inp.Role == "assistant" {
			// Assistant message content should be output_text
			var parts []map[string]interface{}
			json.Unmarshal(inp.Content, &parts)
			if len(parts) == 0 {
				t.Fatal("assistant content is empty")
			}
			if parts[0]["type"] != "output_text" {
				t.Errorf("assistant content type = %v, want output_text", parts[0]["type"])
			}
			return
		}
	}
	t.Error("no assistant message found in input")
}

func TestConvertResponsesToChat_MultiTurnWithToolCalls(t *testing.T) {
	inputJSON := `[
		{"type":"message","id":"msg_1","role":"user","status":"completed","content":[{"type":"input_text","text":"What is the weather?"}]},
		{"type":"function_call","id":"fc_1","call_id":"call_abc","name":"get_weather","arguments":"{\"city\":\"Beijing\"}","status":"completed"},
		{"type":"function_call_output","call_id":"call_abc","output":"Sunny, 25°C"},
		{"type":"message","id":"msg_2","role":"user","status":"completed","content":[{"type":"input_text","text":"Thanks!"}]}
	]`

	respReq := &ResponsesRequest{
		Model: "gpt-4o",
		Input: json.RawMessage(inputJSON),
	}

	chatReq, err := ConvertResponsesToChatRequest(respReq)
	if err != nil {
		t.Fatalf("conversion error: %v", err)
	}

	if chatReq.Model != "gpt-4o" {
		t.Errorf("model = %v, want gpt-4o", chatReq.Model)
	}

	// First: user message
	if chatReq.Messages[0].Role != "user" {
		t.Errorf("msg[0] role = %v, want user", chatReq.Messages[0].Role)
	}

	// Second: assistant with tool_calls
	if chatReq.Messages[1].Role != "assistant" {
		t.Errorf("msg[1] role = %v, want assistant", chatReq.Messages[1].Role)
	}
	if len(chatReq.Messages[1].ToolCalls) != 1 {
		t.Errorf("msg[1] tool_calls count = %d, want 1", len(chatReq.Messages[1].ToolCalls))
	}

	// Third: tool response
	if chatReq.Messages[2].Role != "tool" {
		t.Errorf("msg[2] role = %v, want tool", chatReq.Messages[2].Role)
	}
	if chatReq.Messages[2].ToolCallID != "call_abc" {
		t.Errorf("msg[2] tool_call_id = %v, want call_abc", chatReq.Messages[2].ToolCallID)
	}

	// Fourth: user message
	if chatReq.Messages[3].Role != "user" {
		t.Errorf("msg[3] role = %v, want user", chatReq.Messages[3].Role)
	}
}

func TestConvertResponsesToChat_AssistantMessageOutputText(t *testing.T) {
	inputJSON := `[
		{"type":"message","id":"msg_1","role":"user","status":"completed","content":[{"type":"input_text","text":"Hello"}]},
		{"type":"message","id":"msg_2","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hi there!"}]},
		{"type":"message","id":"msg_3","role":"user","status":"completed","content":[{"type":"input_text","text":"How are you?"}]}
	]`

	respReq := &ResponsesRequest{
		Model: "gpt-4o",
		Input: json.RawMessage(inputJSON),
	}

	chatReq, err := ConvertResponsesToChatRequest(respReq)
	if err != nil {
		t.Fatalf("conversion error: %v", err)
	}

	// Assistant message content — should be a plain string, not an array
	assistantMsg := chatReq.Messages[1]
	if assistantMsg.Role != "assistant" {
		t.Errorf("msg[1] role = %v, want assistant", assistantMsg.Role)
	}

	var text string
	if err := json.Unmarshal(assistantMsg.Content, &text); err != nil {
		t.Fatalf("msg[1] content is not a string: %v (raw: %s)", err, string(assistantMsg.Content))
	}
	if text != "Hi there!" {
		t.Errorf("msg[1] content = %q, want 'Hi there!'", text)
	}
}

func TestConvertResponsesToChat_MissingRoleDefaultsToAssistant(t *testing.T) {
	inputJSON := `[
		{"type":"message","id":"msg_1","content":[{"type":"input_text","text":"Hello"}]},
		{"type":"message","id":"msg_2","content":[{"type":"output_text","text":"Hi!"}]}
	]`

	respReq := &ResponsesRequest{
		Model: "gpt-4o",
		Input: json.RawMessage(inputJSON),
	}

	chatReq, err := ConvertResponsesToChatRequest(respReq)
	if err != nil {
		t.Fatalf("conversion error: %v", err)
	}

	if chatReq.Messages[0].Role != "assistant" {
		t.Errorf("msg[0] role = %v, want assistant (default)", chatReq.Messages[0].Role)
	}
}

func TestConvertResponsesToChat_DeveloperRoleMapsToSystem(t *testing.T) {
	inputJSON := `[
		{"type":"message","id":"msg_1","role":"developer","content":[{"type":"input_text","text":"Developer note."}]},
		{"type":"message","id":"msg_2","role":"user","content":[{"type":"input_text","text":"Hello"}]}
	]`

	respReq := &ResponsesRequest{
		Model: "gpt-4o",
		Input: json.RawMessage(inputJSON),
	}

	chatReq, err := ConvertResponsesToChatRequest(respReq)
	if err != nil {
		t.Fatalf("conversion error: %v", err)
	}

	if chatReq.Messages[0].Role != "system" {
		t.Errorf("msg[0] role = %v, want system", chatReq.Messages[0].Role)
	}
	if string(chatReq.Messages[0].Content) != `"Developer note."` {
		t.Errorf("msg[0] content = %s, want Developer note", chatReq.Messages[0].Content)
	}
	if chatReq.Messages[1].Role != "user" {
		t.Errorf("msg[1] role = %v, want user", chatReq.Messages[1].Role)
	}
}

func TestConvertChatToResponses_DeveloperRolePreservedAsInputMessage(t *testing.T) {
	chatReq := &ChatCompletionsRequest{
		Model: "gpt-4o",
		Messages: []ChatMessage{
			{Role: "developer", Content: json.RawMessage(`"Developer note."`)},
			{Role: "user", Content: json.RawMessage(`"Hello"`)},
		},
	}

	respReq, err := ConvertChatToResponsesRequest(chatReq)
	if err != nil {
		t.Fatalf("conversion error: %v", err)
	}

	if respReq.Instructions != nil {
		t.Fatalf("instructions = %q, want nil", *respReq.Instructions)
	}

	var inputs []ResponsesInputMessage
	if err := json.Unmarshal(respReq.Input, &inputs); err != nil {
		t.Fatalf("input unmarshal error: %v", err)
	}

	if len(inputs) != 2 {
		t.Fatalf("input len = %d, want 2: %+v", len(inputs), inputs)
	}
	if inputs[0].Role != "developer" {
		t.Errorf("input[0] role = %q, want developer", inputs[0].Role)
	}
	if string(inputs[0].Content) != `"Developer note."` {
		t.Errorf("input[0] content = %s, want developer text", inputs[0].Content)
	}
	if inputs[1].Role != "user" {
		t.Errorf("input[1] role = %q, want user", inputs[1].Role)
	}
}

func TestConvertChatToResponses_MultipleSystemMessages(t *testing.T) {
	chatReq := &ChatCompletionsRequest{
		Model: "gpt-4o",
		Messages: []ChatMessage{
			{Role: "system", Content: json.RawMessage(`"System prompt."`)},
			{Role: "system", Content: json.RawMessage(`"Additional instructions."`)},
			{Role: "user", Content: json.RawMessage(`"Hello"`)},
		},
	}

	respReq, err := ConvertChatToResponsesRequest(chatReq)
	if err != nil {
		t.Fatalf("conversion error: %v", err)
	}

	if respReq.Instructions == nil {
		t.Fatal("instructions is nil")
	}
	instructions := *respReq.Instructions
	if !strings.Contains(instructions, "System prompt.") {
		t.Errorf("instructions missing 'System prompt.'")
	}
	if !strings.Contains(instructions, "Additional instructions.") {
		t.Errorf("instructions missing 'Additional instructions.'")
	}
	if !strings.Contains(instructions, "\n\n") {
		t.Errorf("instructions should be joined with \\n\\n")
	}
}

func TestConvertResponsesToChat_RefusalContent(t *testing.T) {
	inputJSON := `[
		{"type":"message","id":"msg_1","role":"user","status":"completed","content":[{"type":"input_text","text":"Do something bad"}]},
		{"type":"message","id":"msg_2","role":"assistant","status":"completed","content":[{"type":"refusal","refusal":"I cannot help with that."}]}
	]`

	respReq := &ResponsesRequest{
		Model: "gpt-4o",
		Input: json.RawMessage(inputJSON),
	}

	chatReq, err := ConvertResponsesToChatRequest(respReq)
	if err != nil {
		t.Fatalf("conversion error: %v", err)
	}

	msg := chatReq.Messages[1]
	if msg.Refusal == nil || *msg.Refusal != "I cannot help with that." {
		t.Errorf("refusal = %v, want 'I cannot help with that.'", msg.Refusal)
	}
}

func TestConvertResponsesToChat_NoStatusField(t *testing.T) {
	inputJSON := `[
		{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello"}]}
	]`

	respReq := &ResponsesRequest{
		Model: "gpt-4o",
		Input: json.RawMessage(inputJSON),
	}

	chatReq, err := ConvertResponsesToChatRequest(respReq)
	if err != nil {
		t.Fatalf("conversion error: %v", err)
	}

	// Serialize to JSON and check no "status" field
	b, _ := json.Marshal(chatReq.Messages[0])
	var m map[string]interface{}
	json.Unmarshal(b, &m)
	if _, exists := m["status"]; exists {
		t.Errorf("msg should not have 'status' field")
	}
}

func TestConvertChatRespToResponsesResp_ToolCallOnlyNoEmptyMessage(t *testing.T) {
	chatResp := &ChatCompletionsResponse{
		ID:      "chatcmpl-123",
		Object:  "chat.completion",
		Created: 1000,
		Model:   "gpt-4o",
		Choices: []ChatChoice{
			{
				Index: 0,
				Message: &ChatMessage{
					Role:    "assistant",
					Content: json.RawMessage(`null`),
					ToolCalls: []ToolCall{
						{
							ID:   "call_xyz",
							Type: "function",
							Function: FunctionCall{
								Name:      "get_weather",
								Arguments: `{"city":"Beijing"}`,
							},
						},
					},
				},
				FinishReason: strPtr("tool_calls"),
			},
		},
	}

	respResp, err := ConvertChatRespToResponsesResp(chatResp, "", "")
	if err != nil {
		t.Fatalf("conversion error: %v", err)
	}

	for _, item := range respResp.Output {
		if item.Type == "message" {
			t.Errorf("should not have message output item for tool-call-only response")
		}
	}

	if len(respResp.Output) != 1 {
		t.Fatalf("expected 1 output item, got %d", len(respResp.Output))
	}
	if respResp.Output[0].Type != "function_call" {
		t.Errorf("output[0] type = %v, want function_call", respResp.Output[0].Type)
	}
}

func TestConvertChatContentToResponses_ImageURL(t *testing.T) {
	content := json.RawMessage(`[{"type":"text","text":"Describe this"},{"type":"image_url","image_url":{"url":"https://example.com/cat.jpg","detail":"high"}}]`)

	result := convertChatContentToResponses(content)
	parts, ok := result.([]map[string]interface{})
	if !ok {
		t.Fatalf("expected []map[string]interface{}, got %T", result)
	}
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(parts))
	}

	if parts[0]["type"] != "input_text" {
		t.Errorf("parts[0] type = %v, want input_text", parts[0]["type"])
	}
	if parts[1]["type"] != "input_image" {
		t.Errorf("parts[1] type = %v, want input_image", parts[1]["type"])
	}
}

func TestGenerateID_Uniqueness(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		id := generateID("test_")
		if seen[id] {
			t.Fatalf("duplicate ID generated: %s", id)
		}
		seen[id] = true
	}
}

func TestConvertChatToResponses_StopParameter(t *testing.T) {
	stop := json.RawMessage(`["\n","STOP"]`)
	chatReq := &ChatCompletionsRequest{
		Model:  "gpt-4o",
		Stop:   stop,
		Stream: false,
		Messages: []ChatMessage{
			{Role: "user", Content: json.RawMessage(`"Hello"`)},
		},
	}

	respReq, err := ConvertChatToResponsesRequest(chatReq)
	if err != nil {
		t.Fatalf("conversion error: %v", err)
	}

	if respReq.Stop == nil {
		t.Errorf("stop parameter should be passed through")
	}
}

func TestConvertChatToResponses_ToolStrictInParameters(t *testing.T) {
	strict := true
	chatReq := &ChatCompletionsRequest{
		Model: "gpt-4o",
		Messages: []ChatMessage{
			{Role: "user", Content: json.RawMessage(`"Hello"`)},
		},
		Tools: []ChatTool{
			{
				Type: "function",
				Function: ChatFunction{
					Name:        "test_func",
					Description: "A test function",
					Parameters:  json.RawMessage(`{"type":"object","properties":{"arg":{"type":"string"}},"required":["arg"]}`),
					Strict:      &strict,
				},
			},
		},
	}

	respReq, err := ConvertChatToResponsesRequest(chatReq)
	if err != nil {
		t.Fatalf("conversion error: %v", err)
	}

	// Parse tools
	var tools []ResponsesTool
	json.Unmarshal(respReq.Tools, &tools)
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}

	if tools[0].Strict == nil || *tools[0].Strict != true {
		t.Errorf("strict should be at tool level and be true")
	}
}

func TestConvertChatToResponses_EmptyResponseFormatIgnored(t *testing.T) {
	chatReq := &ChatCompletionsRequest{
		Model: "gpt-4o",
		Messages: []ChatMessage{
			{Role: "user", Content: json.RawMessage(`"Hello"`)},
		},
		ResponseFormat: json.RawMessage(`{"type":""}`),
	}

	respReq, err := ConvertChatToResponsesRequest(chatReq)
	if err != nil {
		t.Fatalf("conversion error: %v", err)
	}

	if respReq.Text != nil {
		t.Errorf("text should not be set for empty type response_format")
	}
}

func TestConvertResponsesToChat_TextWithOnlyVerbosity(t *testing.T) {
	textJSON := `{"verbosity": "high"}`

	respReq := &ResponsesRequest{
		Model: "gpt-4o",
		Input: json.RawMessage(`[{"type":"message","id":"msg_1","role":"user","status":"completed","content":[{"type":"input_text","text":"Hello"}]}]`),
		Text:  json.RawMessage(textJSON),
	}

	chatReq, err := ConvertResponsesToChatRequest(respReq)
	if err != nil {
		t.Fatalf("conversion error: %v", err)
	}

	if chatReq.ResponseFormat != nil {
		t.Errorf("response_format should not be set for text with only verbosity")
	}
}

func TestConvertResponsesToChat_ToolStrictFromToolLevel(t *testing.T) {
	toolsJSON := `[
		{
			"type": "function",
			"name": "test_func",
			"description": "A test function",
			"strict": false,
			"parameters": {
				"type": "object",
				"properties": {"arg": {"type": "string"}},
				"required": ["arg"]
			}
		}
	]`

	respReq := &ResponsesRequest{
		Model: "gpt-4o",
		Input: json.RawMessage(`[{"type":"message","id":"msg_1","role":"user","status":"completed","content":[{"type":"input_text","text":"Hello"}]}]`),
		Tools: json.RawMessage(toolsJSON),
	}

	chatReq, err := ConvertResponsesToChatRequest(respReq)
	if err != nil {
		t.Fatalf("conversion error: %v", err)
	}

	if len(chatReq.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(chatReq.Tools))
	}

	if chatReq.Tools[0].Function.Strict == nil || *chatReq.Tools[0].Function.Strict != false {
		t.Errorf("function.strict = %v, want false", chatReq.Tools[0].Function.Strict)
	}
}

func TestConvertResponsesToChat_ToolStrictFromInsideParameters(t *testing.T) {
	toolsJSON := `[
		{
			"type": "function",
			"name": "test_func",
			"parameters": {
				"type": "object",
				"properties": {"arg": {"type": "string"}},
				"strict": true
			}
		}
	]`

	respReq := &ResponsesRequest{
		Model: "gpt-4o",
		Input: json.RawMessage(`[{"type":"message","id":"msg_1","role":"user","status":"completed","content":[{"type":"input_text","text":"Hello"}]}]`),
		Tools: json.RawMessage(toolsJSON),
	}

	chatReq, err := ConvertResponsesToChatRequest(respReq)
	if err != nil {
		t.Fatalf("conversion error: %v", err)
	}

	if len(chatReq.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(chatReq.Tools))
	}

	fn := chatReq.Tools[0].Function
	if fn.Strict == nil || *fn.Strict != true {
		t.Errorf("function.strict = %v, want true", fn.Strict)
	}
}

func TestConvertResponsesToChat_NamespaceToolsFlattened(t *testing.T) {
	toolsJSON := `[
		{
			"type": "function",
			"name": "regular_tool",
			"description": "A regular tool",
			"parameters": {"type": "object", "properties": {}}
		},
		{
			"type": "namespace",
			"name": "mcp__context7__",
			"description": "Context7 MCP server",
			"tools": [
				{
					"type": "function",
					"name": "query_docs",
					"description": "Query documentation",
					"strict": false,
					"parameters": {"type": "object", "properties": {"q": {"type": "string"}}}
				},
				{
					"type": "function",
					"name": "resolve_id",
					"description": "Resolve library ID",
					"strict": false,
					"parameters": {"type": "object", "properties": {"name": {"type": "string"}}}
				}
			]
		}
	]`

	respReq := &ResponsesRequest{
		Model: "gpt-4o",
		Input: json.RawMessage(`[{"type":"message","id":"msg_1","role":"user","status":"completed","content":[{"type":"input_text","text":"Hello"}]}]`),
		Tools: json.RawMessage(toolsJSON),
	}

	chatReq, err := ConvertResponsesToChatRequest(respReq)
	if err != nil {
		t.Fatalf("conversion error: %v", err)
	}

	if len(chatReq.Tools) != 3 {
		t.Fatalf("expected 3 tools (1 regular + 2 namespace), got %d", len(chatReq.Tools))
	}

	if chatReq.Tools[0].Function.Name != "regular_tool" {
		t.Errorf("tool[0] name = %v, want regular_tool", chatReq.Tools[0].Function.Name)
	}
	if chatReq.Tools[1].Function.Name != "mcp__context7__query_docs" {
		t.Errorf("tool[1] name = %v, want mcp__context7__query_docs", chatReq.Tools[1].Function.Name)
	}
	if chatReq.Tools[2].Function.Name != "mcp__context7__resolve_id" {
		t.Errorf("tool[2] name = %v, want mcp__context7__resolve_id", chatReq.Tools[2].Function.Name)
	}
}

// ==================== Stream Options Tests ====================

func TestConvertChatToResponses_StreamOptions(t *testing.T) {
	includeUsage := true
	req := &ChatCompletionsRequest{
		Model:  "gpt-4",
		Stream: true,
		StreamOptions: &StreamOptions{
			IncludeUsage: includeUsage,
		},
		Messages: []ChatMessage{
			{Role: "user", Content: jsonString("hello")},
		},
	}
	respReq, err := ConvertChatToResponsesRequest(req)
	if err != nil {
		t.Fatal(err)
	}

	if !respReq.Stream {
		t.Errorf("stream = %v, want true", respReq.Stream)
	}
	if respReq.StreamOptions == nil || !respReq.StreamOptions.IncludeUsage {
		t.Errorf("stream_options.include_usage should be true")
	}
}

func TestConvertChatToResponses_StreamOptionsAutoInclude(t *testing.T) {
	req := &ChatCompletionsRequest{
		Model:  "gpt-4",
		Stream: true,
		Messages: []ChatMessage{
			{Role: "user", Content: jsonString("hello")},
		},
	}
	respReq, err := ConvertChatToResponsesRequest(req)
	if err != nil {
		t.Fatal(err)
	}

	if !respReq.Stream {
		t.Errorf("stream = %v, want true", respReq.Stream)
	}
	if respReq.StreamOptions == nil || !respReq.StreamOptions.IncludeUsage {
		t.Errorf("stream_options.include_usage should be auto-added")
	}
}

func TestConvertChatToResponses_NoStreamOptionsWhenNotStreaming(t *testing.T) {
	req := &ChatCompletionsRequest{
		Model:  "gpt-4",
		Stream: false,
		Messages: []ChatMessage{
			{Role: "user", Content: jsonString("hello")},
		},
	}
	respReq, err := ConvertChatToResponsesRequest(req)
	if err != nil {
		t.Fatal(err)
	}

	if respReq.StreamOptions != nil {
		t.Error("stream_options should not be present when stream=false")
	}
}

func TestConvertResponsesToChat_StreamOptions(t *testing.T) {
	req := &ResponsesRequest{
		Model:  "gpt-4",
		Stream: true,
		Input:  jsonString("hello"),
	}
	chatReq, err := ConvertResponsesToChatRequest(req)
	if err != nil {
		t.Fatal(err)
	}

	if !chatReq.Stream {
		t.Errorf("stream = %v, want true", chatReq.Stream)
	}
	if chatReq.StreamOptions == nil || !chatReq.StreamOptions.IncludeUsage {
		t.Errorf("stream_options.include_usage should be true")
	}
}

// ==================== New Tests ====================

func TestConvertChatToResponses_MaxTokensClamped(t *testing.T) {
	maxTokens := 50
	req := &ChatCompletionsRequest{
		Model:     "gpt-4",
		MaxTokens: &maxTokens,
		Messages:  []ChatMessage{{Role: "user", Content: jsonString("hello")}},
	}
	respReq, err := ConvertChatToResponsesRequest(req)
	if err != nil {
		t.Fatal(err)
	}

	if respReq.MaxOutputTokens == nil || *respReq.MaxOutputTokens != minMaxOutputTokens {
		t.Errorf("max_output_tokens = %v, want %d (clamped)", respReq.MaxOutputTokens, minMaxOutputTokens)
	}
}

func TestConvertChatToResponses_ReasoningEffort(t *testing.T) {
	effort := "high"
	req := &ChatCompletionsRequest{
		Model:           "o3",
		ReasoningEffort: &effort,
		Messages:        []ChatMessage{{Role: "user", Content: jsonString("hello")}},
	}
	respReq, err := ConvertChatToResponsesRequest(req)
	if err != nil {
		t.Fatal(err)
	}

	if respReq.Reasoning == nil {
		t.Fatal("reasoning should be set")
	}
	if respReq.Reasoning.Effort != "high" {
		t.Errorf("reasoning.effort = %v, want high", respReq.Reasoning.Effort)
	}
	if respReq.Reasoning.Summary != "auto" {
		t.Errorf("reasoning.summary = %v, want auto", respReq.Reasoning.Summary)
	}
}

func TestConvertChatToResponses_EmptyBase64ImageSkipped(t *testing.T) {
	content := json.RawMessage(`[{"type":"text","text":"test"},{"type":"image_url","image_url":{"url":"data:image/png;base64,   ","detail":"auto"}}]`)
	req := &ChatCompletionsRequest{
		Model:    "gpt-4o",
		Messages: []ChatMessage{{Role: "user", Content: content}},
	}
	respReq, err := ConvertChatToResponsesRequest(req)
	if err != nil {
		t.Fatal(err)
	}

	var inputs []ResponsesInputMessage
	json.Unmarshal(respReq.Input, &inputs)

	// Find user message
	for _, inp := range inputs {
		if inp.Role == "user" {
			var parts []map[string]interface{}
			json.Unmarshal(inp.Content, &parts)
			if len(parts) != 1 {
				t.Errorf("expected 1 part (empty image skipped), got %d", len(parts))
			}
			if len(parts) > 0 && parts[0]["type"] != "input_text" {
				t.Errorf("expected input_text, got %v", parts[0]["type"])
			}
			return
		}
	}
	t.Error("no user message found")
}

func TestConvertChatToResponses_ThinkingContentWrapped(t *testing.T) {
	content := json.RawMessage(`[{"type":"text","text":"Hello"},{"type":"thinking","thinking":"Let me think..."}]`)
	req := &ChatCompletionsRequest{
		Model:    "gpt-4o",
		Messages: []ChatMessage{{Role: "assistant", Content: content}},
	}
	respReq, err := ConvertChatToResponsesRequest(req)
	if err != nil {
		t.Fatal(err)
	}

	var inputs []ResponsesInputMessage
	json.Unmarshal(respReq.Input, &inputs)

	for _, inp := range inputs {
		if inp.Role == "assistant" {
			var parts []map[string]interface{}
			json.Unmarshal(inp.Content, &parts)
			if len(parts) == 0 {
				t.Fatal("assistant content is empty")
			}
			text, _ := parts[0]["text"].(string)
			if !strings.Contains(text, "<thinking>") || !strings.Contains(text, "</thinking>") {
				t.Errorf("thinking should be wrapped in tags, got: %s", text)
			}
			if !strings.Contains(text, "Hello") {
				t.Errorf("text content should be preserved")
			}
			return
		}
	}
	t.Error("no assistant message found")
}

func TestConvertResponsesRespToChatResp_ReasoningOutput(t *testing.T) {
	resp := &ResponsesResponse{
		ID:     "resp_123",
		Status: "completed",
		Model:  "o3",
		Output: []OutputItem{
			{
				Type: "reasoning",
				Summary: []ResponsesSummary{
					{Type: "summary_text", Text: "I thought about this."},
				},
			},
			{
				Type: "message",
				Role: "assistant",
				Content: []ContentPart{
					{Type: "output_text", Text: "The answer is 42."},
				},
			},
		},
	}

	chatResp, err := ConvertResponsesRespToChatResp(resp)
	if err != nil {
		t.Fatal(err)
	}

	msg := chatResp.Choices[0].Message
	if msg.ReasoningContent != "I thought about this." {
		t.Errorf("reasoning_content = %v, want 'I thought about this.'", msg.ReasoningContent)
	}

	text := contentToString(msg.Content)
	if text != "The answer is 42." {
		t.Errorf("content = %v, want 'The answer is 42.'", text)
	}
}

func TestConvertResponsesRespToChatResp_WebSearchCallFiltered(t *testing.T) {
	resp := &ResponsesResponse{
		ID:     "resp_123",
		Status: "completed",
		Output: []OutputItem{
			{
				Type:   "web_search_call",
				ID:     "ws_1",
				Action: &WebSearchAction{Type: "search", Query: "test"},
			},
			{
				Type: "message",
				Role: "assistant",
				Content: []ContentPart{
					{Type: "output_text", Text: "Search result text."},
				},
			},
		},
	}

	chatResp, err := ConvertResponsesRespToChatResp(resp)
	if err != nil {
		t.Fatal(err)
	}

	text := contentToString(chatResp.Choices[0].Message.Content)
	if text != "Search result text." {
		t.Errorf("content = %v, want 'Search result text.'", text)
	}
}

func TestConvertResponsesRespToChatResp_IncompleteDetails(t *testing.T) {
	resp := &ResponsesResponse{
		ID:     "resp_123",
		Status: "incomplete",
		Model:  "gpt-4o",
		Output: []OutputItem{
			{
				Type: "message",
				Role: "assistant",
				Content: []ContentPart{
					{Type: "output_text", Text: "Partial..."},
				},
			},
		},
		IncompleteDetails: &ResponsesIncompleteDetails{Reason: "max_output_tokens"},
	}

	chatResp, err := ConvertResponsesRespToChatResp(resp)
	if err != nil {
		t.Fatal(err)
	}

	if *chatResp.Choices[0].FinishReason != "length" {
		t.Errorf("finish_reason = %v, want length", *chatResp.Choices[0].FinishReason)
	}
}

// ==================== Streaming State Machine Tests ====================

func TestResponsesEventToChatChunks_TextDelta(t *testing.T) {
	state := NewResponsesEventToChatState()
	state.Model = "gpt-4o"

	// First: response.created
	evt := &ResponsesStreamEvent{
		Type: "response.created",
		Response: &ResponsesResponse{
			ID:    "resp_123",
			Model: "gpt-4o",
		},
	}
	chunks := ResponsesEventToChatChunks(evt, state)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk for created, got %d", len(chunks))
	}
	if chunks[0].Choices[0].Delta.Role != "assistant" {
		t.Errorf("role = %v, want assistant", chunks[0].Choices[0].Delta.Role)
	}

	// Text delta
	evt = &ResponsesStreamEvent{
		Type:  "response.output_text.delta",
		Delta: "Hello",
	}
	chunks = ResponsesEventToChatChunks(evt, state)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	if chunks[0].Choices[0].Delta.Content == nil || *chunks[0].Choices[0].Delta.Content != "Hello" {
		t.Errorf("content = %v, want Hello", chunks[0].Choices[0].Delta.Content)
	}
}

func TestResponsesEventToChatChunks_ToolCallDelta(t *testing.T) {
	state := NewResponsesEventToChatState()
	state.Model = "gpt-4o"

	// Output item added (function_call)
	evt := &ResponsesStreamEvent{
		Type:        "response.output_item.added",
		OutputIndex: 0,
		Item: &OutputItem{
			Type:   "function_call",
			CallID: "call_123",
			Name:   "get_weather",
		},
	}
	chunks := ResponsesEventToChatChunks(evt, state)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	tc := chunks[0].Choices[0].Delta.ToolCalls[0]
	if tc.ID != "call_123" {
		t.Errorf("tool call ID = %v, want call_123", tc.ID)
	}
	if tc.Function.Name != "get_weather" {
		t.Errorf("function name = %v, want get_weather", tc.Function.Name)
	}

	// Args delta
	evt = &ResponsesStreamEvent{
		Type:        "response.function_call_arguments.delta",
		OutputIndex: 0,
		Delta:       `{"city":"`,
	}
	chunks = ResponsesEventToChatChunks(evt, state)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	if chunks[0].Choices[0].Delta.ToolCalls[0].Function.Arguments != `{"city":"` {
		t.Errorf("arguments = %v", chunks[0].Choices[0].Delta.ToolCalls[0].Function.Arguments)
	}
}

func TestResponsesEventToChatChunks_Completed(t *testing.T) {
	state := NewResponsesEventToChatState()
	state.Model = "gpt-4o"
	state.SawText = true

	evt := &ResponsesStreamEvent{
		Type: "response.completed",
		Response: &ResponsesResponse{
			Status: "completed",
			Usage: &ResponsesUsage{
				InputTokens:  100,
				OutputTokens: 50,
				TotalTokens:  150,
			},
		},
	}
	chunks := ResponsesEventToChatChunks(evt, state)
	if len(chunks) < 1 {
		t.Fatal("expected at least 1 chunk")
	}

	// First chunk: finish
	if chunks[0].Choices[0].FinishReason == nil || *chunks[0].Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %v, want stop", chunks[0].Choices[0].FinishReason)
	}

	// Second chunk: usage
	if len(chunks) >= 2 && chunks[1].Usage != nil {
		if chunks[1].Usage.PromptTokens != 100 {
			t.Errorf("prompt_tokens = %v, want 100", chunks[1].Usage.PromptTokens)
		}
	}
}

func TestFinalizeResponsesChatStream(t *testing.T) {
	state := NewResponsesEventToChatState()
	state.Model = "gpt-4o"
	state.SawToolCall = true

	chunks := FinalizeResponsesChatStream(state)
	if len(chunks) == 0 {
		t.Fatal("expected chunks from finalize")
	}

	if chunks[0].Choices[0].FinishReason == nil || *chunks[0].Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %v, want tool_calls", chunks[0].Choices[0].FinishReason)
	}

	// Idempotent: second call returns nil
	chunks2 := FinalizeResponsesChatStream(state)
	if chunks2 != nil {
		t.Errorf("second finalize should return nil, got %d chunks", len(chunks2))
	}
}

func TestStripUnsupportedSchemaFields_DeepNesting(t *testing.T) {
	input := json.RawMessage(`{
		"type": "object",
		"additionalProperties": false,
		"properties": {
			"tags": {
				"type": "array",
				"items": {
					"type": "object",
					"additionalProperties": false,
					"properties": {
						"name": {"type": "string"}
					}
				}
			},
			"config": {
				"type": "object",
				"additionalProperties": {"type": "string"},
				"properties": {
					"key": {"type": "string", "additionalProperties": false}
				}
			}
		}
	}`)

	result := stripUnsupportedSchemaFields(input)

	var m map[string]interface{}
	json.Unmarshal(result, &m)

	if _, ok := m["additionalProperties"]; ok {
		t.Error("root additionalProperties not stripped")
	}

	props := m["properties"].(map[string]interface{})
	items := props["tags"].(map[string]interface{})["items"].(map[string]interface{})
	if _, ok := items["additionalProperties"]; ok {
		t.Error("items.additionalProperties not stripped")
	}
	config := props["config"].(map[string]interface{})
	if _, ok := config["additionalProperties"]; ok {
		t.Error("config.additionalProperties not stripped")
	}
	key := config["properties"].(map[string]interface{})["key"].(map[string]interface{})
	if _, ok := key["additionalProperties"]; ok {
		t.Error("nested property additionalProperties not stripped")
	}
}

func TestStripUnsupportedSchemaFields_RemovesSchemaFields(t *testing.T) {
	input := json.RawMessage(`{
		"type": "object",
		"$schema": "http://json-schema.org/draft-07/schema#",
		"$id": "https://example.com/schema",
		"patternProperties": {"^S_": {"type": "string"}},
		"properties": {
			"name": {"type": "string", "$ref": "#/defs/Name"}
		}
	}`)

	result := stripUnsupportedSchemaFields(input)

	var m map[string]interface{}
	json.Unmarshal(result, &m)

	for _, field := range []string{"$schema", "$id", "patternProperties"} {
		if _, ok := m[field]; ok {
			t.Errorf("field %q not stripped from root", field)
		}
	}

	name := m["properties"].(map[string]interface{})["name"].(map[string]interface{})
	if _, ok := name["$ref"]; ok {
		t.Error("$ref not stripped from nested property")
	}
}

func TestStripUnsupportedSchemaFields_AnyOf(t *testing.T) {
	input := json.RawMessage(`{
		"type": "object",
		"properties": {
			"items": {
				"type": "array",
				"items": {
					"anyOf": [
						{
							"additionalProperties": false,
							"type": "object",
							"properties": {"a": {"type": "string"}}
						},
						{
							"additionalProperties": true,
							"type": "object"
						}
					]
				}
			}
		}
	}`)

	result := stripUnsupportedSchemaFields(input)
	var m map[string]interface{}
	json.Unmarshal(result, &m)

	items := m["properties"].(map[string]interface{})["items"].(map[string]interface{})["items"].(map[string]interface{})
	arr := items["anyOf"].([]interface{})
	for i, v := range arr {
		node := v.(map[string]interface{})
		if _, ok := node["additionalProperties"]; ok {
			t.Errorf("anyOf[%d] additionalProperties not stripped", i)
		}
	}
}

func TestReorderToolMessages_InterleavedAssistantText(t *testing.T) {
	// Reproduces the exact log case: function_call → assistant text → function_call_output
	inputJSON := `[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"Fix the error"}]},
		{"type":"function_call","call_id":"call_1","name":"exec_command","arguments":"{}"},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Let me look."}]},
		{"type":"function_call_output","call_id":"call_1","output":"output here"}
	]`

	respReq := &ResponsesRequest{Model: "mimo", Input: json.RawMessage(inputJSON)}
	chatReq, err := ConvertResponsesToChatRequest(respReq)
	if err != nil {
		t.Fatal(err)
	}

	// Expected: user → assistant(tool_calls) → tool → assistant(text)
	roles := make([]string, len(chatReq.Messages))
	for i, m := range chatReq.Messages {
		roles[i] = m.Role
	}
	expected := []string{"user", "assistant", "tool", "assistant"}
	if len(roles) != len(expected) {
		t.Fatalf("message count = %d, want %d: %v", len(roles), len(expected), roles)
	}
	for i, want := range expected {
		if roles[i] != want {
			t.Errorf("msg[%d] role = %q, want %q (full: %v)", i, roles[i], want, roles)
		}
	}
	// Tool message must follow assistant with tool_calls
	if chatReq.Messages[2].ToolCallID != "call_1" {
		t.Errorf("tool msg tool_call_id = %q, want call_1", chatReq.Messages[2].ToolCallID)
	}
}
