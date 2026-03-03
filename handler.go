package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// ==================== Direction 1: /v1/chat/completions → upstream /v1/responses ====================

func handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	apiKey := extractAPIKey(r)
	if apiKey == "" {
		apiKey = cfg.ResponsesAPIKey
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	defer r.Body.Close()

	var chatReq ChatCompletionsRequest
	if err := json.Unmarshal(body, &chatReq); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	log.Printf("[chat→resp] model=%s stream=%v messages=%d", chatReq.Model, chatReq.Stream, len(chatReq.Messages))

	respBody, err := ConvertChatToResponsesRequest(&chatReq)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "conversion error: "+err.Error())
		return
	}

	log.Printf("[chat→resp] converted body: %s", truncateLog(string(respBody), 2000))

	upstreamURL := cfg.ResponsesAPIBaseURL + "/v1/responses"

	if chatReq.Stream {
		handleChatStreamViaResponses(w, upstreamURL, apiKey, respBody, chatReq.Model)
	} else {
		handleChatNonStream(w, upstreamURL, apiKey, respBody)
	}
}

func handleChatNonStream(w http.ResponseWriter, url, apiKey string, reqBody []byte) {
	resp, err := doUpstreamRequest(url, apiKey, reqBody)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream error: "+err.Error())
		return
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to read upstream response")
		return
	}

	if resp.StatusCode != http.StatusOK {
		log.Printf("[chat→resp] upstream error %d: %s", resp.StatusCode, truncateLog(string(respBody), 1000))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		w.Write(respBody)
		return
	}

	var respResp ResponsesResponse
	if err := json.Unmarshal(respBody, &respResp); err != nil {
		writeError(w, http.StatusBadGateway, "failed to parse upstream response: "+err.Error())
		return
	}

	chatResp, err := ConvertResponsesRespToChatResp(&respResp)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "conversion error: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(chatResp)
}

func handleChatStreamViaResponses(w http.ResponseWriter, url, apiKey string, reqBody []byte, model string) {
	resp, err := doUpstreamRequest(url, apiKey, reqBody)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream error: "+err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		w.Write(body)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	chatID := generateID("chatcmpl-")
	created := nowUnix()
	firstChunk := true
	var pendingToolCalls []ToolCall
	currentFuncName := ""
	currentFuncArgs := ""
	currentFuncCallID := ""
	currentFuncIndex := 0
	sentFirstToolDelta := make(map[int]bool) // track per-tool first delta

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "" {
			continue
		}

		var event struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}

		switch event.Type {
		case "response.output_item.added":
			var ev ResponsesOutputItemAdded
			json.Unmarshal([]byte(data), &ev)
			if ev.Item.Type == "function_call" {
				currentFuncCallID = ev.Item.CallID
				if currentFuncCallID == "" {
					currentFuncCallID = ev.Item.ID
				}
				currentFuncName = ev.Item.Name
				currentFuncArgs = ""
				currentFuncIndex = ev.OutputIndex
				sentFirstToolDelta[currentFuncIndex] = false
			}

		case "response.output_text.delta":
			var ev ResponsesTextDelta
			json.Unmarshal([]byte(data), &ev)

			chunk := makeChatChunk(chatID, created, model)
			if firstChunk {
				chunk.Choices[0].Delta.Role = "assistant"
				firstChunk = false
			}
			chunk.Choices[0].Delta.Content = &ev.Delta
			writeSSEChunk(w, chunk)
			flusher.Flush()

		case "response.refusal.delta":
			// Refusal streaming (Responses API)
			var ev struct {
				Delta string `json:"delta"`
			}
			json.Unmarshal([]byte(data), &ev)

			chunk := makeChatChunk(chatID, created, model)
			if firstChunk {
				chunk.Choices[0].Delta.Role = "assistant"
				firstChunk = false
			}
			chunk.Choices[0].Delta.Refusal = &ev.Delta
			writeSSEChunk(w, chunk)
			flusher.Flush()

		case "response.function_call_arguments.delta":
			var ev ResponsesFunctionCallArgsDelta
			json.Unmarshal([]byte(data), &ev)
			currentFuncArgs += ev.Delta

			chunk := makeChatChunk(chatID, created, model)
			if firstChunk {
				chunk.Choices[0].Delta.Role = "assistant"
				firstChunk = false
			}

			idx := currentFuncIndex
			tc := ToolCall{
				Index: &idx,
				Function: FunctionCall{
					Arguments: ev.Delta,
				},
			}

			// Only send ID, type, and name on the first delta for each tool call
			if !sentFirstToolDelta[currentFuncIndex] {
				tc.ID = currentFuncCallID
				tc.Type = "function"
				tc.Function.Name = currentFuncName
				sentFirstToolDelta[currentFuncIndex] = true
			}

			chunk.Choices[0].Delta.ToolCalls = []ToolCall{tc}
			writeSSEChunk(w, chunk)
			flusher.Flush()

		case "response.function_call_arguments.done":
			pendingToolCalls = append(pendingToolCalls, ToolCall{
				ID:   currentFuncCallID,
				Type: "function",
				Function: FunctionCall{
					Name:      currentFuncName,
					Arguments: currentFuncArgs,
				},
			})

		case "response.completed":
			var ev ResponsesCompleted
			json.Unmarshal([]byte(data), &ev)

			finishReason := "stop"
			if len(pendingToolCalls) > 0 {
				finishReason = "tool_calls"
			}
			if ev.Response.Status == "incomplete" {
				finishReason = "length"
			}

			finalChunk := makeChatChunk(chatID, created, model)
			finalChunk.Choices[0].FinishReason = &finishReason

			if ev.Response.Usage != nil {
				finalChunk.Usage = &ChatUsage{
					PromptTokens:     ev.Response.Usage.InputTokens,
					CompletionTokens: ev.Response.Usage.OutputTokens,
					TotalTokens:      ev.Response.Usage.TotalTokens,
				}
				if ev.Response.Usage.OutputTokensDetails != nil {
					finalChunk.Usage.CompletionTokensDetails = &CompletionTokensDetails{
						ReasoningTokens: ev.Response.Usage.OutputTokensDetails.ReasoningTokens,
					}
				}
				if ev.Response.Usage.InputTokensDetails != nil {
					finalChunk.Usage.PromptTokensDetails = &PromptTokensDetails{
						CachedTokens: ev.Response.Usage.InputTokensDetails.CachedTokens,
					}
				}
			}

			writeSSEChunk(w, finalChunk)
			flusher.Flush()
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
			return
		}
	}

	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// ==================== Direction 2: /v1/responses → upstream /v1/chat/completions ====================

func handleResponses(w http.ResponseWriter, r *http.Request) {
	apiKey := extractAPIKey(r)
	if apiKey == "" {
		apiKey = cfg.CompletionsAPIKey
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	defer r.Body.Close()

	var respReq ResponsesRequest
	if err := json.Unmarshal(body, &respReq); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	log.Printf("[resp→chat] model=%s stream=%v", respReq.Model, respReq.Stream)

	chatBody, err := ConvertResponsesToChatRequest(&respReq)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "conversion error: "+err.Error())
		return
	}

	log.Printf("[resp→chat] converted body: %s", truncateLog(string(chatBody), 500))

	upstreamURL := cfg.CompletionsAPIBaseURL + "/v1/chat/completions"

	if respReq.Stream {
		handleResponsesStreamViaChat(w, upstreamURL, apiKey, chatBody, respReq.Model)
	} else {
		handleResponsesNonStream(w, upstreamURL, apiKey, chatBody)
	}
}

func handleResponsesNonStream(w http.ResponseWriter, url, apiKey string, reqBody []byte) {
	resp, err := doUpstreamRequest(url, apiKey, reqBody)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream error: "+err.Error())
		return
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to read upstream response")
		return
	}

	if resp.StatusCode != http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		w.Write(respBody)
		return
	}

	var chatResp ChatCompletionsResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		writeError(w, http.StatusBadGateway, "failed to parse upstream response: "+err.Error())
		return
	}

	responsesResp, err := ConvertChatRespToResponsesResp(&chatResp)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "conversion error: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(responsesResp)
}

func handleResponsesStreamViaChat(w http.ResponseWriter, url, apiKey string, reqBody []byte, model string) {
	resp, err := doUpstreamRequest(url, apiKey, reqBody)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream error: "+err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		w.Write(body)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	responseID := generateID("resp_")
	msgID := generateID("msg_")
	created := nowUnix()
	seqNum := 0

	baseResponse := map[string]interface{}{
		"id":         responseID,
		"object":     "response",
		"created_at": created,
		"status":     "in_progress",
		"model":      model,
		"output":     []interface{}{},
	}

	// response.created
	writeResponsesSSE(w, "response.created", map[string]interface{}{
		"type": "response.created", "response": baseResponse, "sequence_number": seqNum,
	})
	flusher.Flush()
	seqNum++

	// response.in_progress
	writeResponsesSSE(w, "response.in_progress", map[string]interface{}{
		"type": "response.in_progress", "response": baseResponse, "sequence_number": seqNum,
	})
	flusher.Flush()
	seqNum++

	// output_item.added (message)
	writeResponsesSSE(w, "response.output_item.added", map[string]interface{}{
		"type": "response.output_item.added", "output_index": 0,
		"item": map[string]interface{}{
			"id": msgID, "type": "message", "status": "in_progress",
			"content": []interface{}{}, "role": "assistant",
		},
		"sequence_number": seqNum,
	})
	flusher.Flush()
	seqNum++

	// content_part.added
	writeResponsesSSE(w, "response.content_part.added", map[string]interface{}{
		"type": "response.content_part.added", "content_index": 0,
		"item_id": msgID, "output_index": 0,
		"part": map[string]interface{}{
			"type": "output_text", "annotations": []interface{}{}, "text": "",
		},
		"sequence_number": seqNum,
	})
	flusher.Flush()
	seqNum++

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	var fullText strings.Builder
	var chatUsage *ChatUsage
	var toolCalls []ToolCall
	toolCallMap := make(map[int]*ToolCall)
	var finishReason string

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk ChatCompletionsResponse
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}

		if chunk.Usage != nil {
			chatUsage = chunk.Usage
		}

		if len(chunk.Choices) == 0 {
			continue
		}

		choice := chunk.Choices[0]

		if choice.Delta != nil {
			// Text content
			if choice.Delta.Content != nil && *choice.Delta.Content != "" {
				delta := *choice.Delta.Content
				fullText.WriteString(delta)

				writeResponsesSSE(w, "response.output_text.delta", map[string]interface{}{
					"type": "response.output_text.delta", "content_index": 0,
					"item_id": msgID, "output_index": 0,
					"delta": delta, "sequence_number": seqNum,
				})
				flusher.Flush()
				seqNum++
			}

			// Refusal
			if choice.Delta.Refusal != nil && *choice.Delta.Refusal != "" {
				writeResponsesSSE(w, "response.refusal.delta", map[string]interface{}{
					"type": "response.refusal.delta", "content_index": 0,
					"item_id": msgID, "output_index": 0,
					"delta": *choice.Delta.Refusal, "sequence_number": seqNum,
				})
				flusher.Flush()
				seqNum++
			}

			// Tool calls
			for _, tc := range choice.Delta.ToolCalls {
				idx := 0
				if tc.Index != nil {
					idx = *tc.Index
				}
				if existing, ok := toolCallMap[idx]; ok {
					existing.Function.Arguments += tc.Function.Arguments

					writeResponsesSSE(w, "response.function_call_arguments.delta", map[string]interface{}{
						"type":    "response.function_call_arguments.delta",
						"item_id": existing.ID, "output_index": idx + 1,
						"delta": tc.Function.Arguments, "sequence_number": seqNum,
					})
					flusher.Flush()
					seqNum++
				} else {
					newTC := &ToolCall{
						ID:   tc.ID,
						Type: "function",
						Function: FunctionCall{
							Name:      tc.Function.Name,
							Arguments: tc.Function.Arguments,
						},
					}
					toolCallMap[idx] = newTC

					writeResponsesSSE(w, "response.output_item.added", map[string]interface{}{
						"type": "response.output_item.added", "output_index": idx + 1,
						"item": map[string]interface{}{
							"id": tc.ID, "type": "function_call", "status": "in_progress",
							"call_id": tc.ID, "name": tc.Function.Name, "arguments": "",
						},
						"sequence_number": seqNum,
					})
					flusher.Flush()
					seqNum++

					if tc.Function.Arguments != "" {
						writeResponsesSSE(w, "response.function_call_arguments.delta", map[string]interface{}{
							"type":    "response.function_call_arguments.delta",
							"item_id": tc.ID, "output_index": idx + 1,
							"delta": tc.Function.Arguments, "sequence_number": seqNum,
						})
						flusher.Flush()
						seqNum++
					}
				}
			}
		}

		if choice.FinishReason != nil {
			finishReason = *choice.FinishReason
			break
		}
	}

	// Finalize tool calls
	for idx, tc := range toolCallMap {
		toolCalls = append(toolCalls, *tc)

		writeResponsesSSE(w, "response.function_call_arguments.done", map[string]interface{}{
			"type":    "response.function_call_arguments.done",
			"item_id": tc.ID, "output_index": idx + 1,
			"arguments": tc.Function.Arguments, "sequence_number": seqNum,
		})
		flusher.Flush()
		seqNum++

		writeResponsesSSE(w, "response.output_item.done", map[string]interface{}{
			"type": "response.output_item.done", "output_index": idx + 1,
			"item": map[string]interface{}{
				"id": tc.ID, "type": "function_call", "status": "completed",
				"call_id": tc.ID, "name": tc.Function.Name, "arguments": tc.Function.Arguments,
			},
			"sequence_number": seqNum,
		})
		flusher.Flush()
		seqNum++
	}

	// output_text.done
	writeResponsesSSE(w, "response.output_text.done", map[string]interface{}{
		"type": "response.output_text.done", "content_index": 0,
		"item_id": msgID, "output_index": 0,
		"text": fullText.String(), "sequence_number": seqNum,
	})
	flusher.Flush()
	seqNum++

	// content_part.done
	writeResponsesSSE(w, "response.content_part.done", map[string]interface{}{
		"type": "response.content_part.done", "content_index": 0,
		"item_id": msgID, "output_index": 0,
		"part": map[string]interface{}{
			"type": "output_text", "annotations": []interface{}{}, "text": fullText.String(),
		},
		"sequence_number": seqNum,
	})
	flusher.Flush()
	seqNum++

	// output_item.done (message)
	writeResponsesSSE(w, "response.output_item.done", map[string]interface{}{
		"type": "response.output_item.done", "output_index": 0,
		"item": map[string]interface{}{
			"id": msgID, "type": "message", "status": "completed", "role": "assistant",
			"content": []map[string]interface{}{
				{"type": "output_text", "annotations": []interface{}{}, "text": fullText.String()},
			},
		},
		"sequence_number": seqNum,
	})
	flusher.Flush()
	seqNum++

	// Build final output
	outputItems := []interface{}{
		map[string]interface{}{
			"id": msgID, "type": "message", "status": "completed", "role": "assistant",
			"content": []map[string]interface{}{
				{"type": "output_text", "annotations": []interface{}{}, "text": fullText.String()},
			},
		},
	}
	for _, tc := range toolCalls {
		outputItems = append(outputItems, map[string]interface{}{
			"id": tc.ID, "type": "function_call", "status": "completed",
			"call_id": tc.ID, "name": tc.Function.Name, "arguments": tc.Function.Arguments,
		})
	}

	// Determine final status
	finalStatus := "completed"
	if finishReason == "length" {
		finalStatus = "incomplete"
	}

	var usage interface{}
	if chatUsage != nil {
		u := map[string]interface{}{
			"input_tokens": chatUsage.PromptTokens, "output_tokens": chatUsage.CompletionTokens,
			"total_tokens": chatUsage.TotalTokens,
		}
		if chatUsage.CompletionTokensDetails != nil {
			u["output_tokens_details"] = map[string]interface{}{
				"reasoning_tokens": chatUsage.CompletionTokensDetails.ReasoningTokens,
			}
		}
		if chatUsage.PromptTokensDetails != nil {
			u["input_tokens_details"] = map[string]interface{}{
				"cached_tokens": chatUsage.PromptTokensDetails.CachedTokens,
			}
		}
		usage = u
	}

	// response.completed
	completedResponse := map[string]interface{}{
		"id": responseID, "object": "response", "created_at": created,
		"status": finalStatus, "completed_at": time.Now().Unix(),
		"model": model, "output": outputItems, "usage": usage,
	}
	writeResponsesSSE(w, "response.completed", map[string]interface{}{
		"type": "response.completed", "response": completedResponse, "sequence_number": seqNum,
	})
	flusher.Flush()
}

// ==================== Pass-through ====================

func handlePassthrough(w http.ResponseWriter, r *http.Request) {
	apiKey := extractAPIKey(r)
	if apiKey == "" {
		apiKey = cfg.ResponsesAPIKey
	}

	upstreamURL := cfg.ResponsesAPIBaseURL + r.URL.Path
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}

	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
		defer r.Body.Close()
	}

	req, err := http.NewRequest(r.Method, upstreamURL, bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to create request")
		return
	}

	req.Header.Set("Authorization", "Bearer "+apiKey)
	if ct := r.Header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream error: "+err.Error())
		return
	}
	defer resp.Body.Close()

	for k, v := range resp.Header {
		for _, vv := range v {
			w.Header().Add(k, vv)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// ==================== Utilities ====================

var httpClient = &http.Client{
	Timeout: 5 * time.Minute,
}

func doUpstreamRequest(url, apiKey string, body []byte) (*http.Response, error) {
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "text/event-stream")

	log.Printf("[upstream] POST %s (%d bytes)", url, len(body))
	return httpClient.Do(req)
}

func extractAPIKey(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}

func writeError(w http.ResponseWriter, code int, msg string) {
	log.Printf("[error] %d: %s", code, msg)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"message": msg,
			"type":    "proxy_error",
			"code":    code,
		},
	})
}

func writeSSEChunk(w http.ResponseWriter, data interface{}) {
	b, _ := json.Marshal(data)
	fmt.Fprintf(w, "data: %s\n\n", b)
}

func writeResponsesSSE(w http.ResponseWriter, event string, data interface{}) {
	b, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
}

func makeChatChunk(id string, created int64, model string) ChatCompletionsResponse {
	return ChatCompletionsResponse{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []ChatChoice{
			{
				Index: 0,
				Delta: &ChatDelta{},
			},
		},
	}
}

func truncateLog(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
