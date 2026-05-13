package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"
)

// ==================== Chat Completions → Responses API ====================

func ConvertChatToResponsesRequest(chatReq *ChatCompletionsRequest) (*ResponsesRequest, error) {
	input, instructions, err := convertChatMessagesToResponsesInput(chatReq.Messages)
	if err != nil {
		return nil, err
	}

	inputJSON, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}

	out := &ResponsesRequest{
		Model:        chatReq.Model,
		Input:        inputJSON,
		Temperature:  chatReq.Temperature,
		TopP:         chatReq.TopP,
		Stream:       chatReq.Stream,
		ServiceTier:  chatReq.ServiceTier,
	}

	if instructions != "" {
		out.Instructions = &instructions
	}

	// Set defaults for upstream streaming
	if chatReq.Stream {
		out.Include = []string{"reasoning.encrypted_content"}
		storeFalse := false
		out.Store = &storeFalse
	}

	// Extract instructions from system/developer messages
	// (already handled inside convertChatMessagesToResponsesInput)

	if chatReq.MaxCompletionTokens != nil {
		v := *chatReq.MaxCompletionTokens
		if v < minMaxOutputTokens {
			v = minMaxOutputTokens
		}
		out.MaxOutputTokens = &v
	} else if chatReq.MaxTokens != nil {
		v := *chatReq.MaxTokens
		if v < minMaxOutputTokens {
			v = minMaxOutputTokens
		}
		out.MaxOutputTokens = &v
	}

	if chatReq.N != nil && *chatReq.N > 1 {
		log.Printf("[chat->resp] WARNING: n=%d requested but Responses API only supports 1 output", *chatReq.N)
	}

	if chatReq.FrequencyPenalty != nil {
		out.FrequencyPenalty = chatReq.FrequencyPenalty
	}
	if chatReq.PresencePenalty != nil {
		out.PresencePenalty = chatReq.PresencePenalty
	}
	if chatReq.Stop != nil {
		out.Stop = chatReq.Stop
	}
	if chatReq.Seed != nil {
		out.Seed = chatReq.Seed
	}
	if chatReq.Store != nil {
		out.Store = chatReq.Store
	}
	if chatReq.Metadata != nil {
		out.Metadata = chatReq.Metadata
	}
	if chatReq.TopLogprobs != nil {
		out.TopLogprobs = chatReq.TopLogprobs
	} else if chatReq.Logprobs != nil && *chatReq.Logprobs {
		v := 1
		out.TopLogprobs = &v
	}
	if chatReq.ParallelToolCalls != nil {
		out.ParallelToolCalls = chatReq.ParallelToolCalls
	}
	if chatReq.User != nil {
		out.User = chatReq.User
	}

	// reasoning_effort → reasoning.effort + summary="auto"
	if chatReq.ReasoningEffort != nil {
		out.Reasoning = &ResponsesReasoning{
			Effort:  *chatReq.ReasoningEffort,
			Summary: "auto",
		}
	}

	// response_format → text.format
	if chatReq.ResponseFormat != nil {
		if text := convertResponseFormatToText(chatReq.ResponseFormat); text != nil {
			out.Text = json.RawMessage(mustMarshal(text))
		}
	}

	// tools
	if len(chatReq.Tools) > 0 {
		out.Tools = json.RawMessage(mustMarshal(convertChatToolsToResponses(chatReq.Tools)))
	}

	// tool_choice
	if chatReq.ToolChoice != nil {
		out.ToolChoice = chatReq.ToolChoice
	}

	// stream_options
	if chatReq.Stream {
		if chatReq.StreamOptions != nil {
			out.StreamOptions = &StreamOptions{IncludeUsage: chatReq.StreamOptions.IncludeUsage}
		} else {
			out.StreamOptions = &StreamOptions{IncludeUsage: true}
		}
	}

	return out, nil
}

// convertChatMessagesToResponsesInput converts messages to Responses input items.
// System/developer messages are collected into instructions.
func convertChatMessagesToResponsesInput(msgs []ChatMessage) ([]ResponsesInputMessage, string, error) {
	var out []ResponsesInputMessage
	var instructionsParts []string

	for _, m := range msgs {
		switch m.Role {
		case "system", "developer":
			text := contentToString(m.Content)
			if text != "" {
				instructionsParts = append(instructionsParts, text)
			}
		default:
			items, err := chatMessageToResponsesItems(m)
			if err != nil {
				return nil, "", err
			}
			out = append(out, items...)
		}
	}

	instructions := strings.Join(instructionsParts, "\n\n")
	return out, instructions, nil
}

func chatMessageToResponsesItems(m ChatMessage) ([]ResponsesInputMessage, error) {
	switch m.Role {
	case "user":
		return chatUserToResponses(m)
	case "assistant":
		return chatAssistantToResponses(m)
	case "tool":
		return chatToolToResponses(m)
	case "function":
		return chatFunctionToResponses(m)
	default:
		return chatUserToResponses(m)
	}
}

func chatUserToResponses(m ChatMessage) ([]ResponsesInputMessage, error) {
	if m.Content != nil {
		return []ResponsesInputMessage{{
			Role:    "user",
			Content: json.RawMessage(mustMarshal(convertChatContentToResponses(m.Content))),
		}}, nil
	}
	return []ResponsesInputMessage{{Role: "user"}}, nil
}

func chatAssistantToResponses(m ChatMessage) ([]ResponsesInputMessage, error) {
	var items []ResponsesInputMessage

	if len(m.Content) > 0 {
		s := parseAssistantContent(m.Content)
		if s != "" {
			parts := []map[string]interface{}{
				{"type": "output_text", "text": s, "annotations": []interface{}{}},
			}
			items = append(items, ResponsesInputMessage{
				Role:    "assistant",
				Content: json.RawMessage(mustMarshal(parts)),
			})
		}
	}

	for _, tc := range m.ToolCalls {
		args := tc.Function.Arguments
		if args == "" {
			args = "{}"
		}
		items = append(items, ResponsesInputMessage{
			Type:      "function_call",
			ID:        tc.ID,
			CallID:    tc.ID,
			Name:      tc.Function.Name,
			Arguments: args,
			Status:    "completed",
		})
	}

	return items, nil
}

// parseAssistantContent returns assistant content as plain text.
// For structured thinking/reasoning parts, wraps in <thinking>...</thinking> tags.
func parseAssistantContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}

	var parts []map[string]any
	if err := json.Unmarshal(raw, &parts); err != nil {
		return contentToString(raw)
	}

	var b strings.Builder
	for _, p := range parts {
		typ, _ := p["type"].(string)
		text, _ := p["text"].(string)
		thinking, _ := p["thinking"].(string)

		switch typ {
		case "thinking", "reasoning":
			content := thinking
			if content == "" {
				content = text
			}
			if content != "" {
				b.WriteString("<thinking>")
				b.WriteString(content)
				b.WriteString("</thinking>")
			}
		default:
			if text != "" {
				b.WriteString(text)
			}
		}
	}

	return b.String()
}

func chatToolToResponses(m ChatMessage) ([]ResponsesInputMessage, error) {
	output := contentToString(m.Content)
	if output == "" {
		output = "(empty)"
	}
	return []ResponsesInputMessage{{
		Type:   "function_call_output",
		CallID: m.ToolCallID,
		Output: output,
	}}, nil
}

func chatFunctionToResponses(m ChatMessage) ([]ResponsesInputMessage, error) {
	output := contentToString(m.Content)
	if output == "" {
		output = "(empty)"
	}
	return []ResponsesInputMessage{{
		Type:   "function_call_output",
		CallID: m.Name,
		Output: output,
	}}, nil
}

func convertChatToolsToResponses(tools []ChatTool) []ResponsesTool {
	var out []ResponsesTool
	for _, t := range tools {
		if t.Type != "function" {
			continue
		}
		rt := ResponsesTool{
			Type: "function",
			Name: t.Function.Name,
		}
		if t.Function.Description != "" {
			rt.Description = t.Function.Description
		}
		if t.Function.Parameters != nil {
			rt.Parameters = t.Function.Parameters
		}
		if t.Function.Strict != nil {
			rt.Strict = t.Function.Strict
		}
		out = append(out, rt)
	}
	return out
}

// ==================== Responses API → Chat Completions (Non-Streaming) ====================

func ConvertResponsesRespToChatResp(respResp *ResponsesResponse) (*ChatCompletionsResponse, error) {
	chatResp := &ChatCompletionsResponse{
		ID:          convertID(respResp.ID, "chatcmpl-"),
		Object:      "chat.completion",
		Created:     respResp.CreatedAt,
		Model:       respResp.Model,
		ServiceTier: respResp.ServiceTier,
	}

	var textParts []string
	var reasoningText string
	var toolCalls []ToolCall
	var refusal *string

	for _, item := range respResp.Output {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				switch part.Type {
				case "output_text":
					textParts = append(textParts, part.Text)
				case "refusal":
					r := part.Refusal
					if r == "" {
						r = part.Text
					}
					refusal = &r
				}
			}
		case "function_call":
			toolCalls = append(toolCalls, ToolCall{
				ID:   item.CallID,
				Type: "function",
				Function: FunctionCall{
					Name:      item.Name,
					Arguments: item.Arguments,
				},
			})
		case "reasoning":
			for _, s := range item.Summary {
				if s.Type == "summary_text" && s.Text != "" {
					reasoningText += s.Text
				}
			}
		case "web_search_call":
			// silently consumed — results already incorporated into text output
		}
	}

	finishReason := responsesStatusToChatFinishReason(respResp.Status, respResp.IncompleteDetails, toolCalls)

	msg := ChatMessage{
		Role:    "assistant",
		Refusal: refusal,
	}
	if len(textParts) > 0 {
		combined := strings.Join(textParts, "")
		msg.Content = jsonString(combined)
	} else {
		msg.Content = json.RawMessage("null")
	}
	if len(toolCalls) > 0 {
		msg.ToolCalls = toolCalls
	}
	if reasoningText != "" {
		msg.ReasoningContent = reasoningText
	}

	chatResp.Choices = []ChatChoice{
		{
			Index:        0,
			Message:      &msg,
			FinishReason: &finishReason,
		},
	}

	// Convert usage
	if respResp.Usage != nil {
		chatResp.Usage = &ChatUsage{
			PromptTokens:     respResp.Usage.InputTokens,
			CompletionTokens: respResp.Usage.OutputTokens,
			TotalTokens:      respResp.Usage.TotalTokens,
		}
		if respResp.Usage.OutputTokensDetails != nil && respResp.Usage.OutputTokensDetails.ReasoningTokens > 0 {
			chatResp.Usage.CompletionTokensDetails = &CompletionTokensDetails{
				ReasoningTokens: respResp.Usage.OutputTokensDetails.ReasoningTokens,
			}
		}
		if respResp.Usage.InputTokensDetails != nil && respResp.Usage.InputTokensDetails.CachedTokens > 0 {
			chatResp.Usage.PromptTokensDetails = &PromptTokensDetails{
				CachedTokens: respResp.Usage.InputTokensDetails.CachedTokens,
			}
		}
	}

	return chatResp, nil
}

func responsesStatusToChatFinishReason(status string, details *ResponsesIncompleteDetails, toolCalls []ToolCall) string {
	switch status {
	case "incomplete":
		if details != nil && details.Reason == "max_output_tokens" {
			return "length"
		}
		return "stop"
	case "completed":
		if len(toolCalls) > 0 {
			return "tool_calls"
		}
		return "stop"
	default:
		return "stop"
	}
}

// ==================== Responses API → Chat Completions (Request Conversion) ====================

func ConvertResponsesToChatRequest(respReq *ResponsesRequest) (*ChatCompletionsRequest, error) {
	chatReq := &ChatCompletionsRequest{
		Model:  respReq.Model,
		Stream: respReq.Stream,
	}

	var messages []ChatMessage

	// instructions → system message
	if respReq.Instructions != nil && *respReq.Instructions != "" {
		messages = append(messages, ChatMessage{
			Role:    "system",
			Content: jsonString(*respReq.Instructions),
		})
	}

	// Parse input
	if respReq.Input != nil {
		var inputStr string
		if err := json.Unmarshal(respReq.Input, &inputStr); err == nil {
			messages = append(messages, ChatMessage{
				Role:    "user",
				Content: jsonString(inputStr),
			})
		} else {
			var inputMsgs []json.RawMessage
			if err := json.Unmarshal(respReq.Input, &inputMsgs); err == nil {
				for _, rawMsg := range inputMsgs {
					var im ResponsesInputMessage
					json.Unmarshal(rawMsg, &im)

					switch {
					case im.Type == "function_call_output":
						messages = append(messages, ChatMessage{
							Role:       "tool",
							Content:    jsonString(im.Output),
							ToolCallID: im.CallID,
						})
					case im.Type == "function_call":
						messages = append(messages, ChatMessage{
							Role: "assistant",
							ToolCalls: []ToolCall{{
								ID:   im.CallID,
								Type: "function",
								Function: FunctionCall{
									Name:      im.Name,
									Arguments: im.Arguments,
								},
							}},
						})
					default:
						role := im.Role
						if role == "" {
							role = "assistant"
						}
						msg := ChatMessage{Role: role}
						if im.Content != nil {
							msg.Content = json.RawMessage(mustMarshal(convertResponsesContentToChat(im.Content)))
							// Extract refusal
							var parts []ResponsesContentPart
							if err := json.Unmarshal(im.Content, &parts); err == nil {
								for _, p := range parts {
									if p.Type == "refusal" && p.Refusal != "" {
										msg.Refusal = &p.Refusal
										break
									}
								}
							}
						}
						messages = append(messages, msg)
					}
				}
			}
		}
	}
	chatReq.Messages = messages

	// Parameter mapping
	if respReq.MaxOutputTokens != nil {
		chatReq.MaxCompletionTokens = respReq.MaxOutputTokens
	}
	chatReq.Temperature = respReq.Temperature
	chatReq.TopP = respReq.TopP
	chatReq.FrequencyPenalty = respReq.FrequencyPenalty
	chatReq.PresencePenalty = respReq.PresencePenalty
	chatReq.Store = respReq.Store
	chatReq.Metadata = respReq.Metadata
	chatReq.ServiceTier = respReq.ServiceTier
	chatReq.ParallelToolCalls = respReq.ParallelToolCalls
	chatReq.User = respReq.User
	chatReq.ToolChoice = respReq.ToolChoice

	// top_logprobs → logprobs + top_logprobs
	if respReq.TopLogprobs != nil && *respReq.TopLogprobs > 0 {
		logprobs := true
		chatReq.Logprobs = &logprobs
		chatReq.TopLogprobs = respReq.TopLogprobs
	}

	// reasoning.effort → reasoning_effort
	if respReq.Reasoning != nil && respReq.Reasoning.Effort != "" {
		chatReq.ReasoningEffort = &respReq.Reasoning.Effort
	}

	// text.format → response_format
	if respReq.Text != nil {
		if rf := convertTextToResponseFormat(respReq.Text); rf != nil {
			chatReq.ResponseFormat = json.RawMessage(mustMarshal(rf))
		}
	}

	// Convert tools
	if respReq.Tools != nil {
		var respTools []map[string]interface{}
		if err := json.Unmarshal(respReq.Tools, &respTools); err == nil {
			var chatTools []ChatTool
			for _, rt := range respTools {
				toolType, _ := rt["type"].(string)
				switch toolType {
				case "function":
					fn := ChatFunction{}
					if name, ok := rt["name"].(string); ok {
						fn.Name = name
					}
					if desc, ok := rt["description"].(string); ok {
						fn.Description = desc
					}
					if params, ok := rt["parameters"]; ok {
						if paramsMap, ok := params.(map[string]interface{}); ok {
							if s, hasStrict := paramsMap["strict"]; hasStrict {
								boolVal, ok := s.(bool)
								if ok {
									fn.Strict = &boolVal
								}
								delete(paramsMap, "strict")
							}
						}
						fn.Parameters = json.RawMessage(mustMarshal(params))
					}
					if strict, ok := rt["strict"].(bool); ok {
						fn.Strict = &strict
					}
					chatTools = append(chatTools, ChatTool{Type: "function", Function: fn})
				case "namespace":
					nsName, _ := rt["name"].(string)
					var nsTools []map[string]interface{}
					if raw, ok := rt["tools"]; ok {
						b, _ := json.Marshal(raw)
						json.Unmarshal(b, &nsTools)
					}
					for _, nt := range nsTools {
						ntType, _ := nt["type"].(string)
						if ntType != "function" {
							continue
						}
						fn := ChatFunction{}
						if name, ok := nt["name"].(string); ok {
							fn.Name = nsName + name
						}
						if desc, ok := nt["description"].(string); ok {
							fn.Description = desc
						}
						if params, ok := nt["parameters"]; ok {
							fn.Parameters = json.RawMessage(mustMarshal(params))
						}
						if strict, ok := nt["strict"].(bool); ok {
							fn.Strict = &strict
						}
						chatTools = append(chatTools, ChatTool{Type: "function", Function: fn})
					}
				}
			}
			if len(chatTools) > 0 {
				chatReq.Tools = chatTools
			}
		}
	}

	if respReq.Stream {
		chatReq.StreamOptions = &StreamOptions{IncludeUsage: true}
	}

	return chatReq, nil
}

// ==================== Chat Completions Response → Responses API Response ====================

func ConvertChatRespToResponsesResp(chatResp *ChatCompletionsResponse) (*ResponsesResponse, error) {
	respResp := &ResponsesResponse{
		ID:          convertID(chatResp.ID, "resp_"),
		Object:      "response",
		CreatedAt:   chatResp.Created,
		Status:      "completed",
		Model:       chatResp.Model,
		ServiceTier: chatResp.ServiceTier,
	}

	for _, choice := range chatResp.Choices {
		if choice.Message == nil {
			continue
		}
		msg := choice.Message

		if choice.FinishReason != nil && *choice.FinishReason == "length" {
			respResp.Status = "incomplete"
			respResp.IncompleteDetails = &ResponsesIncompleteDetails{Reason: "max_output_tokens"}
		}

		text := contentToString(msg.Content)
		hasText := text != ""
		if hasText || len(msg.ToolCalls) == 0 {
			outputItem := OutputItem{
				ID:     fmt.Sprintf("msg_%d", time.Now().UnixNano()),
				Type:   "message",
				Status: "completed",
				Role:   "assistant",
			}

			if msg.Refusal != nil && *msg.Refusal != "" {
				outputItem.Content = []ContentPart{{
					Type:    "refusal",
					Refusal: *msg.Refusal,
				}}
			} else {
				outputItem.Content = []ContentPart{{
					Type:        "output_text",
					Text:        text,
					Annotations: json.RawMessage("[]"),
				}}
			}

			respResp.Output = append(respResp.Output, outputItem)
		}

		for _, tc := range msg.ToolCalls {
			respResp.Output = append(respResp.Output, OutputItem{
				ID:        tc.ID,
				Type:      "function_call",
				Status:    "completed",
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
				CallID:    tc.ID,
			})
		}
	}

	// Convert usage
	if chatResp.Usage != nil {
		respResp.Usage = &ResponsesUsage{
			InputTokens:  chatResp.Usage.PromptTokens,
			OutputTokens: chatResp.Usage.CompletionTokens,
			TotalTokens:  chatResp.Usage.TotalTokens,
		}
		if chatResp.Usage.CompletionTokensDetails != nil {
			respResp.Usage.OutputTokensDetails = &OutputTokensDetails{
				ReasoningTokens: chatResp.Usage.CompletionTokensDetails.ReasoningTokens,
			}
		}
		if chatResp.Usage.PromptTokensDetails != nil {
			respResp.Usage.InputTokensDetails = &InputTokensDetails{
				CachedTokens: chatResp.Usage.PromptTokensDetails.CachedTokens,
			}
		}
	}

	return respResp, nil
}

// ==================== Streaming State Machine ====================

// ResponsesEventToChatState tracks state for converting Responses SSE events to Chat chunks.
type ResponsesEventToChatState struct {
	ID      string
	Model   string
	Created int64

	SentRole    bool
	SawToolCall bool
	SawText     bool
	Finalized   bool

	NextToolCallIndex      int
	OutputIndexToToolIndex map[int]int

	IncludeUsage bool
	Usage        *ChatUsage
}

func NewResponsesEventToChatState() *ResponsesEventToChatState {
	return &ResponsesEventToChatState{
		ID:                     generateID("chatcmpl-"),
		Created:                nowUnix(),
		OutputIndexToToolIndex: make(map[int]int),
		IncludeUsage:           true,
	}
}

// ResponsesEventToChatChunks converts a single Responses SSE event into Chat chunks.
func ResponsesEventToChatChunks(evt *ResponsesStreamEvent, state *ResponsesEventToChatState) []ChatCompletionsChunk {
	switch evt.Type {
	case "response.created":
		return resToChatHandleCreated(evt, state)
	case "response.output_text.delta":
		return resToChatHandleTextDelta(evt, state)
	case "response.output_item.added":
		return resToChatHandleOutputItemAdded(evt, state)
	case "response.function_call_arguments.delta":
		return resToChatHandleFuncArgsDelta(evt, state)
	case "response.reasoning_summary_text.delta":
		return resToChatHandleReasoningDelta(evt, state)
	case "response.reasoning_summary_text.done":
		return nil
	case "response.completed", "response.done", "response.incomplete", "response.failed":
		return resToChatHandleCompleted(evt, state)
	default:
		return nil
	}
}

// FinalizeResponsesChatStream emits a final chunk if stream ended without completion event.
func FinalizeResponsesChatStream(state *ResponsesEventToChatState) []ChatCompletionsChunk {
	if state.Finalized {
		return nil
	}
	state.Finalized = true

	finishReason := "stop"
	if state.SawToolCall {
		finishReason = "tool_calls"
	}

	chunks := []ChatCompletionsChunk{makeChatFinishChunk(state, finishReason)}

	if state.IncludeUsage && state.Usage != nil {
		chunks = append(chunks, ChatCompletionsChunk{
			ID: state.ID, Object: "chat.completion.chunk",
			Created: state.Created, Model: state.Model,
			Choices: []ChatChunkChoice{},
			Usage:   state.Usage,
		})
	}

	return chunks
}

func resToChatHandleCreated(evt *ResponsesStreamEvent, state *ResponsesEventToChatState) []ChatCompletionsChunk {
	if evt.Response != nil {
		if evt.Response.ID != "" {
			state.ID = evt.Response.ID
		}
		if state.Model == "" && evt.Response.Model != "" {
			state.Model = evt.Response.Model
		}
	}
	if state.SentRole {
		return nil
	}
	state.SentRole = true
	role := "assistant"
	return []ChatCompletionsChunk{makeChatDeltaChunk(state, ChatDelta{Role: role})}
}

func resToChatHandleTextDelta(evt *ResponsesStreamEvent, state *ResponsesEventToChatState) []ChatCompletionsChunk {
	if evt.Delta == "" {
		return nil
	}
	state.SawText = true
	content := evt.Delta
	return []ChatCompletionsChunk{makeChatDeltaChunk(state, ChatDelta{Content: &content})}
}

func resToChatHandleOutputItemAdded(evt *ResponsesStreamEvent, state *ResponsesEventToChatState) []ChatCompletionsChunk {
	if evt.Item == nil || evt.Item.Type != "function_call" {
		return nil
	}
	state.SawToolCall = true
	idx := state.NextToolCallIndex
	state.OutputIndexToToolIndex[evt.OutputIndex] = idx
	state.NextToolCallIndex++

	return []ChatCompletionsChunk{makeChatDeltaChunk(state, ChatDelta{
		ToolCalls: []ToolCall{{
			Index: &idx,
			ID:    evt.Item.CallID,
			Type:  "function",
			Function: FunctionCall{Name: evt.Item.Name},
		}},
	})}
}

func resToChatHandleFuncArgsDelta(evt *ResponsesStreamEvent, state *ResponsesEventToChatState) []ChatCompletionsChunk {
	if evt.Delta == "" {
		return nil
	}
	idx, ok := state.OutputIndexToToolIndex[evt.OutputIndex]
	if !ok {
		return nil
	}
	return []ChatCompletionsChunk{makeChatDeltaChunk(state, ChatDelta{
		ToolCalls: []ToolCall{{
			Index: &idx,
			Function: FunctionCall{Arguments: evt.Delta},
		}},
	})}
}

func resToChatHandleReasoningDelta(evt *ResponsesStreamEvent, state *ResponsesEventToChatState) []ChatCompletionsChunk {
	if evt.Delta == "" {
		return nil
	}
	reasoning := evt.Delta
	return []ChatCompletionsChunk{makeChatDeltaChunk(state, ChatDelta{ReasoningContent: &reasoning})}
}

func resToChatHandleCompleted(evt *ResponsesStreamEvent, state *ResponsesEventToChatState) []ChatCompletionsChunk {
	state.Finalized = true
	finishReason := "stop"

	if evt.Response != nil {
		if evt.Response.Usage != nil {
			u := evt.Response.Usage
			usage := &ChatUsage{
				PromptTokens:     u.InputTokens,
				CompletionTokens: u.OutputTokens,
				TotalTokens:      u.InputTokens + u.OutputTokens,
			}
			if u.InputTokensDetails != nil && u.InputTokensDetails.CachedTokens > 0 {
				usage.PromptTokensDetails = &PromptTokensDetails{CachedTokens: u.InputTokensDetails.CachedTokens}
			}
			state.Usage = usage
		}
		switch evt.Response.Status {
		case "incomplete":
			if evt.Response.IncompleteDetails != nil && evt.Response.IncompleteDetails.Reason == "max_output_tokens" {
				finishReason = "length"
			}
		case "completed":
			if state.SawToolCall {
				finishReason = "tool_calls"
			}
		}
	} else if state.SawToolCall {
		finishReason = "tool_calls"
	}

	var chunks []ChatCompletionsChunk
	chunks = append(chunks, makeChatFinishChunk(state, finishReason))

	if state.IncludeUsage && state.Usage != nil {
		chunks = append(chunks, ChatCompletionsChunk{
			ID: state.ID, Object: "chat.completion.chunk",
			Created: state.Created, Model: state.Model,
			Choices: []ChatChunkChoice{},
			Usage:   state.Usage,
		})
	}

	return chunks
}

func makeChatDeltaChunk(state *ResponsesEventToChatState, delta ChatDelta) ChatCompletionsChunk {
	return ChatCompletionsChunk{
		ID: state.ID, Object: "chat.completion.chunk",
		Created: state.Created, Model: state.Model,
		Choices: []ChatChunkChoice{{Index: 0, Delta: delta}},
	}
}

func makeChatFinishChunk(state *ResponsesEventToChatState, finishReason string) ChatCompletionsChunk {
	empty := ""
	return ChatCompletionsChunk{
		ID: state.ID, Object: "chat.completion.chunk",
		Created: state.Created, Model: state.Model,
		Choices: []ChatChunkChoice{{
			Index: 0,
			Delta: ChatDelta{Content: &empty},
			FinishReason: &finishReason,
		}},
	}
}

// ChatChunkToSSE formats a chunk as SSE data line.
func ChatChunkToSSE(chunk ChatCompletionsChunk) (string, error) {
	data, err := json.Marshal(chunk)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("data: %s\n\n", data), nil
}

// ==================== Vision Content Conversion ====================

func convertChatContentToResponses(raw json.RawMessage) interface{} {
	if raw == nil {
		return nil
	}

	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}

	var parts []ChatContentPart
	if err := json.Unmarshal(raw, &parts); err == nil {
		var result []map[string]interface{}
		for _, p := range parts {
			switch p.Type {
			case "text":
				result = append(result, map[string]interface{}{
					"type": "input_text",
					"text": p.Text,
				})
			case "image_url":
				if p.ImageURL != nil && p.ImageURL.URL != "" && !isEmptyBase64DataURI(p.ImageURL.URL) {
					img := map[string]interface{}{
						"type":      "input_image",
						"image_url": p.ImageURL.URL,
					}
					if p.ImageURL.Detail != "" {
						img["detail"] = p.ImageURL.Detail
					}
					result = append(result, img)
				}
			default:
				var raw map[string]interface{}
				b, _ := json.Marshal(p)
				json.Unmarshal(b, &raw)
				result = append(result, raw)
			}
		}
		return result
	}

	var raw2 interface{}
	json.Unmarshal(raw, &raw2)
	return raw2
}

func convertResponsesContentToChat(raw json.RawMessage) interface{} {
	if raw == nil {
		return nil
	}

	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}

	var parts []ResponsesContentPart
	if err := json.Unmarshal(raw, &parts); err == nil {
		var result []map[string]interface{}
		for _, p := range parts {
			switch p.Type {
			case "input_text", "output_text", "text":
				result = append(result, map[string]interface{}{
					"type": "text",
					"text": p.Text,
				})
			case "input_image":
				img := map[string]interface{}{
					"type": "image_url",
					"image_url": map[string]interface{}{
						"url": p.ImageURL,
					},
				}
				if p.Detail != "" {
					img["image_url"].(map[string]interface{})["detail"] = p.Detail
				}
				result = append(result, img)
			case "refusal":
				refusalText := p.Refusal
				if refusalText == "" {
					refusalText = p.Text
				}
				result = append(result, map[string]interface{}{
					"type":    "refusal",
					"refusal": refusalText,
				})
			default:
				var unknown map[string]interface{}
				b, _ := json.Marshal(p)
				json.Unmarshal(b, &unknown)
				result = append(result, unknown)
			}
		}
		return result
	}

	var raw2 interface{}
	json.Unmarshal(raw, &raw2)
	return raw2
}

func isEmptyBase64DataURI(raw string) bool {
	if !strings.HasPrefix(raw, "data:") {
		return false
	}
	rest := strings.TrimPrefix(raw, "data:")
	semicolonIdx := strings.Index(rest, ";")
	if semicolonIdx < 0 {
		return false
	}
	rest = rest[semicolonIdx+1:]
	if !strings.HasPrefix(rest, "base64,") {
		return false
	}
	return strings.TrimSpace(strings.TrimPrefix(rest, "base64,")) == ""
}

// ==================== Structured Output Conversion ====================

func convertResponseFormatToText(rf json.RawMessage) map[string]interface{} {
	var rfObj ResponseFormatObj
	if err := json.Unmarshal(rf, &rfObj); err != nil {
		return nil
	}
	if rfObj.Type == "" {
		return nil
	}

	result := map[string]interface{}{}

	switch rfObj.Type {
	case "json_object":
		result["format"] = map[string]interface{}{"type": "json_object"}
	case "json_schema":
		if rfObj.JSONSchema != nil {
			format := map[string]interface{}{
				"type": "json_schema",
				"name": rfObj.JSONSchema.Name,
			}
			if rfObj.JSONSchema.Description != "" {
				format["description"] = rfObj.JSONSchema.Description
			}
			if rfObj.JSONSchema.Schema != nil {
				var s interface{}
				json.Unmarshal(rfObj.JSONSchema.Schema, &s)
				format["schema"] = s
			}
			if rfObj.JSONSchema.Strict != nil {
				format["strict"] = *rfObj.JSONSchema.Strict
			}
			result["format"] = format
		}
	case "text":
		result["format"] = map[string]interface{}{"type": "text"}
	default:
		var raw interface{}
		json.Unmarshal(rf, &raw)
		result["format"] = raw
	}

	return result
}

func convertTextToResponseFormat(text json.RawMessage) interface{} {
	var tf ResponsesTextFormat
	if err := json.Unmarshal(text, &tf); err != nil {
		return nil
	}

	if tf.Format.Type == "" {
		return nil
	}

	switch tf.Format.Type {
	case "json_object":
		return map[string]interface{}{"type": "json_object"}
	case "json_schema":
		result := map[string]interface{}{
			"type": "json_schema",
			"json_schema": map[string]interface{}{
				"name": tf.Format.Name,
			},
		}
		js := result["json_schema"].(map[string]interface{})
		if tf.Format.Description != "" {
			js["description"] = tf.Format.Description
		}
		if tf.Format.Schema != nil {
			var s interface{}
			json.Unmarshal(tf.Format.Schema, &s)
			js["schema"] = s
		}
		if tf.Format.Strict != nil {
			js["strict"] = *tf.Format.Strict
		}
		return result
	case "text":
		return map[string]interface{}{"type": "text"}
	default:
		return map[string]interface{}{"type": tf.Format.Type}
	}
}

// ==================== Helpers ====================

func convertID(id, prefix string) string {
	if strings.HasPrefix(id, prefix) {
		return id
	}
	for _, p := range []string{"chatcmpl-", "resp_", "cmpl-"} {
		if strings.HasPrefix(id, p) {
			return prefix + id[len(p):]
		}
	}
	return prefix + id
}

var idCounter uint64

func generateID(prefix string) string {
	count := atomic.AddUint64(&idCounter, 1)
	return fmt.Sprintf("%s%d_%d", prefix, time.Now().UnixNano(), count)
}

func mustMarshal(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}
