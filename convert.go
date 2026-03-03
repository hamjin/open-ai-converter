package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ==================== Chat Completions → Responses API ====================

func ConvertChatToResponsesRequest(chatReq *ChatCompletionsRequest) ([]byte, error) {
	respReq := make(map[string]interface{})
	respReq["model"] = chatReq.Model
	respReq["stream"] = chatReq.Stream

	// Convert messages → input
	var inputMessages []map[string]interface{}
	var instructions *string

	for _, msg := range chatReq.Messages {
		switch msg.Role {
		case "system", "developer":
			text := contentToString(msg.Content)
			instructions = &text

		case "user":
			m := map[string]interface{}{
				"role": "user",
			}
			if msg.Content != nil {
				m["content"] = convertChatContentToResponses(msg.Content)
			}
			inputMessages = append(inputMessages, m)

		case "assistant":
			if len(msg.ToolCalls) > 0 {
				text := contentToString(msg.Content)
				if text != "" {
					// Assistant message with text content → Responses API "message" item
					inputMessages = append(inputMessages, map[string]interface{}{
						"type":   "message",
						"role":   "assistant",
						"status": "completed",
						"content": []map[string]interface{}{
							{"type": "output_text", "text": text, "annotations": []interface{}{}},
						},
					})
				}
				for _, tc := range msg.ToolCalls {
					inputMessages = append(inputMessages, map[string]interface{}{
						"type":      "function_call",
						"id":        tc.ID,
						"call_id":   tc.ID,
						"name":      tc.Function.Name,
						"arguments": tc.Function.Arguments,
						"status":    "completed",
					})
				}
			} else {
				// Assistant message without tool_calls → Responses API "message" item
				text := contentToString(msg.Content)
				m := map[string]interface{}{
					"type":   "message",
					"role":   "assistant",
					"status": "completed",
					"content": []map[string]interface{}{
						{"type": "output_text", "text": text, "annotations": []interface{}{}},
					},
				}
				inputMessages = append(inputMessages, m)
			}

		case "tool":
			inputMessages = append(inputMessages, map[string]interface{}{
				"type":    "function_call_output",
				"call_id": msg.ToolCallID,
				"output":  contentToString(msg.Content),
			})
		}
	}

	respReq["input"] = inputMessages

	if instructions != nil {
		respReq["instructions"] = *instructions
	}

	// ---- Parameter mapping ----

	// max_tokens / max_completion_tokens → max_output_tokens
	if chatReq.MaxCompletionTokens != nil {
		respReq["max_output_tokens"] = *chatReq.MaxCompletionTokens
	} else if chatReq.MaxTokens != nil {
		respReq["max_output_tokens"] = *chatReq.MaxTokens
	}

	if chatReq.Temperature != nil {
		respReq["temperature"] = *chatReq.Temperature
	}
	if chatReq.TopP != nil {
		respReq["top_p"] = *chatReq.TopP
	}
	if chatReq.FrequencyPenalty != nil {
		respReq["frequency_penalty"] = *chatReq.FrequencyPenalty
	}
	if chatReq.PresencePenalty != nil {
		respReq["presence_penalty"] = *chatReq.PresencePenalty
	}

	// stop → (no direct equivalent, but some implementations accept it)
	if chatReq.Stop != nil {
		respReq["stop"] = json.RawMessage(chatReq.Stop)
	}

	// seed → (pass through, some implementations support it)
	if chatReq.Seed != nil {
		respReq["seed"] = *chatReq.Seed
	}

	// store
	if chatReq.Store != nil {
		respReq["store"] = *chatReq.Store
	}

	// metadata
	if chatReq.Metadata != nil {
		var md interface{}
		json.Unmarshal(chatReq.Metadata, &md)
		respReq["metadata"] = md
	}

	// service_tier
	if chatReq.ServiceTier != nil {
		respReq["service_tier"] = *chatReq.ServiceTier
	}

	// logprobs → top_logprobs
	if chatReq.TopLogprobs != nil {
		respReq["top_logprobs"] = *chatReq.TopLogprobs
	} else if chatReq.Logprobs != nil && *chatReq.Logprobs {
		respReq["top_logprobs"] = 1
	}

	// reasoning_effort → reasoning.effort
	if chatReq.ReasoningEffort != nil {
		respReq["reasoning"] = map[string]interface{}{
			"effort": *chatReq.ReasoningEffort,
		}
	}

	// response_format → text.format
	if chatReq.ResponseFormat != nil {
		respReq["text"] = convertResponseFormatToText(chatReq.ResponseFormat)
	}

	// parallel_tool_calls
	if chatReq.ParallelToolCalls != nil {
		respReq["parallel_tool_calls"] = *chatReq.ParallelToolCalls
	}

	// Convert tools
	if len(chatReq.Tools) > 0 {
		var respTools []map[string]interface{}
		for _, t := range chatReq.Tools {
			rt := map[string]interface{}{
				"type": "function",
				"name": t.Function.Name,
			}
			if t.Function.Description != "" {
				rt["description"] = t.Function.Description
			}
			if t.Function.Parameters != nil {
				var params interface{}
				json.Unmarshal(t.Function.Parameters, &params)
				rt["parameters"] = params
			}
			if t.Function.Strict != nil {
				rt["strict"] = *t.Function.Strict
			}
			respTools = append(respTools, rt)
		}
		respReq["tools"] = respTools
	}

	if chatReq.ToolChoice != nil {
		var tc interface{}
		json.Unmarshal(chatReq.ToolChoice, &tc)
		respReq["tool_choice"] = tc
	}

	if chatReq.User != nil {
		respReq["user"] = *chatReq.User
	}

	return json.Marshal(respReq)
}

// ConvertResponsesRespToChatResp converts Responses API response → Chat Completions response
func ConvertResponsesRespToChatResp(respResp *ResponsesResponse) (*ChatCompletionsResponse, error) {
	chatResp := &ChatCompletionsResponse{
		ID:          convertID(respResp.ID, "chatcmpl-"),
		Object:      "chat.completion",
		Created:     respResp.CreatedAt,
		Model:       respResp.Model,
		ServiceTier: respResp.ServiceTier,
	}

	var textParts []string
	var toolCalls []ToolCall
	var refusal *string
	finishReason := "stop"

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
			finishReason = "tool_calls"
		}
	}

	// Check incomplete_details for length finish reason
	if respResp.Status == "incomplete" {
		finishReason = "length"
	}

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

	chatResp.Choices = []ChatChoice{
		{
			Index:        0,
			Message:      &msg,
			FinishReason: &finishReason,
		},
	}

	// Convert usage with details
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

// ==================== Responses API → Chat Completions ====================

func ConvertResponsesToChatRequest(respReq *ResponsesRequest) ([]byte, error) {
	chatReq := make(map[string]interface{})
	chatReq["model"] = respReq.Model
	chatReq["stream"] = respReq.Stream

	var messages []map[string]interface{}

	// Add instructions as system message
	if respReq.Instructions != nil && *respReq.Instructions != "" {
		messages = append(messages, map[string]interface{}{
			"role":    "system",
			"content": *respReq.Instructions,
		})
	}

	// Parse input
	if respReq.Input != nil {
		var inputStr string
		if err := json.Unmarshal(respReq.Input, &inputStr); err == nil {
			messages = append(messages, map[string]interface{}{
				"role":    "user",
				"content": inputStr,
			})
		} else {
			var inputMsgs []json.RawMessage
			if err := json.Unmarshal(respReq.Input, &inputMsgs); err == nil {
				// Process each input message with type awareness
				for _, rawMsg := range inputMsgs {
					var im ResponsesInputMessage
					json.Unmarshal(rawMsg, &im)

					switch {
					case im.Type == "function_call_output":
						messages = append(messages, map[string]interface{}{
							"role":         "tool",
							"content":      im.Output,
							"tool_call_id": im.CallID,
						})

					case im.Type == "function_call":
						m := map[string]interface{}{
							"role": "assistant",
							"tool_calls": []map[string]interface{}{
								{
									"id":   im.CallID,
									"type": "function",
									"function": map[string]interface{}{
										"name":      im.Name,
										"arguments": im.Arguments,
									},
								},
							},
						}
						messages = append(messages, m)

					default:
						m := map[string]interface{}{
							"role": im.Role,
						}
						if im.Content != nil {
							m["content"] = convertResponsesContentToChat(im.Content)
						}
						messages = append(messages, m)
					}
				}
			}
		}
	}

	chatReq["messages"] = messages

	// ---- Parameter mapping ----

	// max_output_tokens → max_completion_tokens (prefer newer field)
	if respReq.MaxOutputTokens != nil {
		chatReq["max_completion_tokens"] = *respReq.MaxOutputTokens
	}
	if respReq.Temperature != nil {
		chatReq["temperature"] = *respReq.Temperature
	}
	if respReq.TopP != nil {
		chatReq["top_p"] = *respReq.TopP
	}
	if respReq.FrequencyPenalty != nil {
		chatReq["frequency_penalty"] = *respReq.FrequencyPenalty
	}
	if respReq.PresencePenalty != nil {
		chatReq["presence_penalty"] = *respReq.PresencePenalty
	}

	// store
	if respReq.Store != nil {
		chatReq["store"] = *respReq.Store
	}

	// metadata
	if respReq.Metadata != nil {
		var md interface{}
		json.Unmarshal(respReq.Metadata, &md)
		chatReq["metadata"] = md
	}

	// service_tier
	if respReq.ServiceTier != nil {
		chatReq["service_tier"] = *respReq.ServiceTier
	}

	// top_logprobs → logprobs + top_logprobs
	if respReq.TopLogprobs != nil && *respReq.TopLogprobs > 0 {
		chatReq["logprobs"] = true
		chatReq["top_logprobs"] = *respReq.TopLogprobs
	}

	// reasoning.effort → reasoning_effort
	if respReq.Reasoning != nil {
		var rc ReasoningConfig
		if err := json.Unmarshal(respReq.Reasoning, &rc); err == nil && rc.Effort != "" {
			chatReq["reasoning_effort"] = rc.Effort
		}
	}

	// text.format → response_format
	if respReq.Text != nil {
		chatReq["response_format"] = convertTextToResponseFormat(respReq.Text)
	}

	// parallel_tool_calls
	if respReq.ParallelToolCalls != nil {
		chatReq["parallel_tool_calls"] = *respReq.ParallelToolCalls
	}

	// Convert tools (handle function + skip unsupported types with warning)
	if respReq.Tools != nil {
		var respTools []map[string]interface{}
		if err := json.Unmarshal(respReq.Tools, &respTools); err == nil {
			var chatTools []map[string]interface{}
			for _, rt := range respTools {
				toolType, _ := rt["type"].(string)
				switch toolType {
				case "function":
					ct := map[string]interface{}{
						"type": "function",
						"function": map[string]interface{}{
							"name": rt["name"],
						},
					}
					fn := ct["function"].(map[string]interface{})
					if desc, ok := rt["description"]; ok {
						fn["description"] = desc
					}
					if params, ok := rt["parameters"]; ok {
						fn["parameters"] = params
					}
					if strict, ok := rt["strict"]; ok {
						fn["strict"] = strict
					}
					chatTools = append(chatTools, ct)
					// web_search, file_search, code_interpreter, computer_use
					// cannot be mapped to Chat Completions — silently skip
				}
			}
			if len(chatTools) > 0 {
				chatReq["tools"] = chatTools
			}
		}
	}

	if respReq.ToolChoice != nil {
		var tc interface{}
		json.Unmarshal(respReq.ToolChoice, &tc)
		chatReq["tool_choice"] = tc
	}

	if respReq.User != nil {
		chatReq["user"] = *respReq.User
	}

	if respReq.Stream {
		chatReq["stream_options"] = map[string]interface{}{
			"include_usage": true,
		}
	}

	return json.Marshal(chatReq)
}

// ConvertChatRespToResponsesResp converts Chat Completions response → Responses API response
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

		// Check finish reason for incomplete
		if choice.FinishReason != nil && *choice.FinishReason == "length" {
			respResp.Status = "incomplete"
			respResp.IncompleteDetails = json.RawMessage(`{"reason":"max_output_tokens"}`)
		}

		// Add message output item
		if msg.Content != nil || len(msg.ToolCalls) == 0 {
			text := contentToString(msg.Content)
			outputItem := OutputItem{
				ID:     fmt.Sprintf("msg_%d", time.Now().UnixNano()),
				Type:   "message",
				Status: "completed",
				Role:   "assistant",
			}

			if msg.Refusal != nil && *msg.Refusal != "" {
				outputItem.Content = []ContentPart{
					{
						Type:    "refusal",
						Refusal: *msg.Refusal,
					},
				}
			} else {
				outputItem.Content = []ContentPart{
					{
						Type:        "output_text",
						Text:        text,
						Annotations: json.RawMessage("[]"),
					},
				}
			}

			respResp.Output = append(respResp.Output, outputItem)
		}

		// Add function_call output items for tool calls
		for _, tc := range msg.ToolCalls {
			outputItem := OutputItem{
				ID:        tc.ID,
				Type:      "function_call",
				Status:    "completed",
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
				CallID:    tc.ID,
			}
			respResp.Output = append(respResp.Output, outputItem)
		}
	}

	// Convert usage with details
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

// ==================== Vision Content Conversion ====================

// convertChatContentToResponses converts Chat Completions content (string or multipart array)
// to Responses API input format, handling image_url → input_image conversion
func convertChatContentToResponses(raw json.RawMessage) interface{} {
	if raw == nil {
		return nil
	}

	// Try as string
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}

	// Try as multipart array
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
				if p.ImageURL != nil {
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
				// Pass through unknown types
				var raw map[string]interface{}
				b, _ := json.Marshal(p)
				json.Unmarshal(b, &raw)
				result = append(result, raw)
			}
		}
		return result
	}

	// Fallback
	var raw2 interface{}
	json.Unmarshal(raw, &raw2)
	return raw2
}

// convertResponsesContentToChat converts Responses API content (string or multipart array)
// to Chat Completions format, handling input_image → image_url conversion
func convertResponsesContentToChat(raw json.RawMessage) interface{} {
	if raw == nil {
		return nil
	}

	// Try as string
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}

	// Try as multipart array
	var parts []ResponsesContentPart
	if err := json.Unmarshal(raw, &parts); err == nil {
		var result []map[string]interface{}
		for _, p := range parts {
			switch p.Type {
			case "input_text":
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
			case "text":
				result = append(result, map[string]interface{}{
					"type": "text",
					"text": p.Text,
				})
			default:
				var raw2 map[string]interface{}
				b, _ := json.Marshal(p)
				json.Unmarshal(b, &raw2)
				result = append(result, raw2)
			}
		}
		return result
	}

	var raw2 interface{}
	json.Unmarshal(raw, &raw2)
	return raw2
}

// ==================== Structured Output Conversion ====================

// convertResponseFormatToText converts Chat Completions response_format → Responses text.format
func convertResponseFormatToText(rf json.RawMessage) map[string]interface{} {
	var rfObj ResponseFormatObj
	if err := json.Unmarshal(rf, &rfObj); err != nil {
		return nil
	}

	result := map[string]interface{}{}

	switch rfObj.Type {
	case "json_object":
		result["format"] = map[string]interface{}{
			"type": "json_object",
		}
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
		result["format"] = map[string]interface{}{
			"type": "text",
		}
	default:
		// Pass through
		var raw interface{}
		json.Unmarshal(rf, &raw)
		result["format"] = raw
	}

	return result
}

// convertTextToResponseFormat converts Responses text.format → Chat Completions response_format
func convertTextToResponseFormat(text json.RawMessage) interface{} {
	var tf ResponsesTextFormat
	if err := json.Unmarshal(text, &tf); err != nil {
		return nil
	}

	switch tf.Format.Type {
	case "json_object":
		return map[string]interface{}{
			"type": "json_object",
		}
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
		return map[string]interface{}{
			"type": "text",
		}
	default:
		return map[string]interface{}{
			"type": tf.Format.Type,
		}
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

func generateID(prefix string) string {
	return fmt.Sprintf("%s%d", prefix, time.Now().UnixNano())
}
