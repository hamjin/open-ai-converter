package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ==================== Direction 1: /v1/chat/completions → upstream /v1/responses ====================

// mapModelInJSON replaces the "model":"<old>" value in raw JSON with "model":"<new>".
// Only matches the top-level model field to avoid replacing model names in message content.
func mapModelInJSON(data []byte, old, new string) []byte {
	return bytes.Replace(data, []byte(`"model":"`+old+`"`), []byte(`"model":"`+new+`"`), 1)
}

// mapReasoningEffortInJSON rewrites both top-level `reasoning_effort` (Chat Completions
// shape) and nested `reasoning.effort` (Responses shape) when the value matches a key
// in the supplied map. Returns the original body untouched when nothing changed or the
// payload is not a JSON object.
func mapReasoningEffortInJSON(data []byte, m map[string]string, rl *RequestLogger, logPrefix string) []byte {
	if len(m) == 0 {
		return data
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(data, &doc); err != nil {
		return data
	}
	changed := false
	if v, ok := doc["reasoning_effort"].(string); ok {
		if mapped, ok2 := m[v]; ok2 {
			doc["reasoning_effort"] = mapped
			rl.Filef("%s reasoning_effort mapping: %s → %s", logPrefix, v, mapped)
			rl.SetEffortMapping(v, mapped)
			changed = true
		}
	}
	if rsn, ok := doc["reasoning"].(map[string]interface{}); ok {
		if v, ok := rsn["effort"].(string); ok {
			if mapped, ok2 := m[v]; ok2 {
				rsn["effort"] = mapped
				rl.Filef("%s reasoning.effort mapping: %s → %s", logPrefix, v, mapped)
				rl.SetEffortMapping(v, mapped)
				changed = true
			}
		}
	}
	if !changed {
		return data
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return data
	}
	return out
}

func handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	rl := LoggerFromReq(r)
	apiKey := resolveAPIKey(r, cfg.ResponsesAPIKey, cfg.APIKeyPassthroughConvert && cfg.APIKeyPassthroughResponsesAPI)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(r, w, http.StatusBadRequest, "failed to read request body")
		return
	}
	defer r.Body.Close()

	var chatReq ChatCompletionsRequest
	if err := json.Unmarshal(body, &chatReq); err != nil {
		writeError(r, w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	rl.Filef("[chat→resp] model=%s stream=%v messages=%d", chatReq.Model, chatReq.Stream, len(chatReq.Messages))
	rl.SetStream(chatReq.Stream)
	rl.SetModelMapping(chatReq.Model, chatReq.Model)
	if chatReq.ReasoningEffort != nil {
		rl.SetEffortMapping(*chatReq.ReasoningEffort, *chatReq.ReasoningEffort)
	}

	// Apply model mapping: client model → upstream model
	if cfg.ModelMapConvert {
		if mapped, ok := cfg.ModelMap[chatReq.Model]; ok {
			rl.Filef("[chat→resp] model mapping: %s → %s", chatReq.Model, mapped)
			rl.SetModelMapping(chatReq.Model, mapped)
			chatReq.Model = mapped
		}
	}

	// Apply reasoning_effort mapping (forces unsupported values down to allowed ones)
	if cfg.ReasoningEffortMapConvert && chatReq.ReasoningEffort != nil {
		if mapped, ok := cfg.ReasoningEffortMap[*chatReq.ReasoningEffort]; ok {
			rl.Filef("[chat→resp] reasoning_effort mapping: %s → %s", *chatReq.ReasoningEffort, mapped)
			rl.SetEffortMapping(*chatReq.ReasoningEffort, mapped)
			chatReq.ReasoningEffort = &mapped
		}
	}

	respReq, err := ConvertChatToResponsesRequest(&chatReq)
	if err != nil {
		writeError(r, w, http.StatusInternalServerError, "conversion error: "+err.Error())
		return
	}

	respBody, err := json.Marshal(respReq)
	if err != nil {
		writeError(r, w, http.StatusInternalServerError, "marshal error: "+err.Error())
		return
	}
	rl.WriteConvertedRequest(respBody)

	upstreamURL := cfg.ResponsesAPIBaseURL + "/v1/responses"

	if chatReq.Stream {
		handleChatStreamViaResponses(r, w, upstreamURL, apiKey, respBody, chatReq.Model, chatReq.Model)
	} else {
		handleChatNonStream(r, w, upstreamURL, apiKey, respBody, chatReq.Model)
	}
}

func handleChatNonStream(r *http.Request, w http.ResponseWriter, url, apiKey string, reqBody []byte, originalModel string) {
	resp, err := doUpstreamRequest(r, url, apiKey, reqBody, false)
	if err != nil {
		writeError(r, w, http.StatusBadGateway, "upstream error: "+err.Error())
		return
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		writeError(r, w, http.StatusBadGateway, "failed to read upstream response")
		return
	}

	if resp.StatusCode != http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		w.Write(respBody)
		return
	}

	var respResp ResponsesResponse
	if err := json.Unmarshal(respBody, &respResp); err != nil {
		writeError(r, w, http.StatusBadGateway, "failed to parse upstream response: "+err.Error())
		return
	}

	chatResp, err := ConvertResponsesRespToChatResp(&respResp)
	if err != nil {
		writeError(r, w, http.StatusInternalServerError, "conversion error: "+err.Error())
		return
	}

	chatResp.Model = originalModel // Reverse-map to client model

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(chatResp)
}

func handleChatStreamViaResponses(r *http.Request, w http.ResponseWriter, url, apiKey string, reqBody []byte, model string, originalModel string) {
	model = originalModel // Use client model in all responses
	resp, err := doUpstreamRequest(r, url, apiKey, reqBody, true)
	if err != nil {
		writeError(r, w, http.StatusBadGateway, "upstream error: "+err.Error())
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
		writeError(r, w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	// Fallback: if upstream returned JSON instead of SSE
	contentType := resp.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "application/json") {
		respBody, _ := io.ReadAll(resp.Body)
		var respResp ResponsesResponse
		if err := json.Unmarshal(respBody, &respResp); err == nil {
			chatResp, err := ConvertResponsesRespToChatResp(&respResp)
			if err == nil {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache")
				w.Header().Set("Connection", "keep-alive")
				w.Header().Set("X-Accel-Buffering", "no")
				w.WriteHeader(http.StatusOK)

				// Convert to single streaming chunk
				chunk := ChatCompletionsChunk{
					ID: chatResp.ID, Object: "chat.completion.chunk",
					Created: chatResp.Created, Model: chatResp.Model,
				}
				for _, choice := range chatResp.Choices {
					cc := ChatChunkChoice{Index: choice.Index}
					if choice.Message != nil {
						delta := ChatDelta{
							Role:      "assistant",
							ToolCalls: choice.Message.ToolCalls,
							Refusal:   choice.Message.Refusal,
						}
						if choice.Message.ReasoningContent != "" {
							delta.ReasoningContent = &choice.Message.ReasoningContent
						}
						text := contentToString(choice.Message.Content)
						if text != "" {
							delta.Content = &text
						}
						cc.Delta = delta
					}
					chunk.Choices = append(chunk.Choices, cc)
				}
				writeSSEChunk(w, chunk)
				flusher.Flush()
			}
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Use streaming state machine
	state := NewResponsesEventToChatState()
	state.Model = model

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "" || data == "[DONE]" {
			continue
		}

		var evt ResponsesStreamEvent
		if err := json.Unmarshal([]byte(data), &evt); err != nil {
			continue
		}

		chunks := ResponsesEventToChatChunks(&evt, state)
		for _, chunk := range chunks {
			writeSSEChunk(w, chunk)
			flusher.Flush()
		}

		if state.Finalized {
			break
		}
	}

	// Safety net: finalize if stream ended without completion event
	if chunks := FinalizeResponsesChatStream(state); chunks != nil {
		for _, chunk := range chunks {
			writeSSEChunk(w, chunk)
			flusher.Flush()
		}
	}

	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// ==================== Direction 2: /v1/responses → upstream /v1/chat/completions ====================

func handleResponses(w http.ResponseWriter, r *http.Request) {
	apiKey := resolveAPIKey(r, cfg.CompletionsAPIKey, cfg.APIKeyPassthroughConvert && cfg.APIKeyPassthroughCompletionsAPI)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(r, w, http.StatusBadRequest, "failed to read request body")
		return
	}
	defer r.Body.Close()

	var respReq ResponsesRequest
	if err := json.Unmarshal(body, &respReq); err != nil {
		writeError(r, w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	rl := LoggerFromReq(r)
	rl.Filef("[resp→chat] model=%s stream=%v", respReq.Model, respReq.Stream)
	rl.SetStream(respReq.Stream)
	rl.SetModelMapping(respReq.Model, respReq.Model)
	if respReq.Reasoning != nil && respReq.Reasoning.Effort != "" {
		rl.SetEffortMapping(respReq.Reasoning.Effort, respReq.Reasoning.Effort)
	}

	// Apply model mapping: client model → upstream model
	if cfg.ModelMapConvert {
		if mapped, ok := cfg.ModelMap[respReq.Model]; ok {
			rl.Filef("[resp→chat] model mapping: %s → %s", respReq.Model, mapped)
			rl.SetModelMapping(respReq.Model, mapped)
			respReq.Model = mapped
		}
	}

	// Apply reasoning_effort mapping on nested reasoning.effort
	if cfg.ReasoningEffortMapConvert && respReq.Reasoning != nil && respReq.Reasoning.Effort != "" {
		if mapped, ok := cfg.ReasoningEffortMap[respReq.Reasoning.Effort]; ok {
			rl.Filef("[resp→chat] reasoning.effort mapping: %s → %s", respReq.Reasoning.Effort, mapped)
			rl.SetEffortMapping(respReq.Reasoning.Effort, mapped)
			respReq.Reasoning.Effort = mapped
		}
	}

	chatReq, err := ConvertResponsesToChatRequest(&respReq)
	if err != nil {
		writeError(r, w, http.StatusInternalServerError, "conversion error: "+err.Error())
		return
	}

	chatBody, err := json.Marshal(chatReq)
	if err != nil {
		writeError(r, w, http.StatusInternalServerError, "marshal error: "+err.Error())
		return
	}
	rl.WriteConvertedRequest(chatBody)

	upstreamURL := cfg.CompletionsAPIBaseURL + "/v1/chat/completions"

	conversationID := responseConversationID(&respReq)
	if respReq.Stream {
		handleResponsesStreamViaChat(r, w, upstreamURL, apiKey, chatBody, respReq.Model, respReq.Model, conversationID)
	} else {
		handleResponsesNonStream(r, w, upstreamURL, apiKey, chatBody, respReq.Model, conversationID)
	}
}

func handleResponsesNonStream(r *http.Request, w http.ResponseWriter, url, apiKey string, reqBody []byte, originalModel string, conversationID string) {
	resp, err := doUpstreamRequest(r, url, apiKey, reqBody, false)
	if err != nil {
		writeError(r, w, http.StatusBadGateway, "upstream error: "+err.Error())
		return
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		writeError(r, w, http.StatusBadGateway, "failed to read upstream response")
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
		writeError(r, w, http.StatusBadGateway, "failed to parse upstream response: "+err.Error())
		return
	}

	// Cache reasoning_content from upstream for injection into next request
	var reasoningContent string
	for _, choice := range chatResp.Choices {
		if choice.Message != nil && choice.Message.ReasoningContent != "" {
			reasoningContent = choice.Message.ReasoningContent
			for _, tc := range choice.Message.ToolCalls {
				storeReasoningContentForConversation(conversationID, tc.ID, choice.Message.ReasoningContent)
			}
		}
	}

	// Extract reasoning effort from converted request
	var reasoningEffort string
	var partial struct {
		ReasoningEffort string `json:"reasoning_effort"`
	}
	json.Unmarshal(reqBody, &partial)
	reasoningEffort = partial.ReasoningEffort

	responsesResp, err := ConvertChatRespToResponsesResp(&chatResp, reasoningEffort, reasoningContent)
	if err != nil {
		writeError(r, w, http.StatusInternalServerError, "conversion error: "+err.Error())
		return
	}

	responsesResp.Model = originalModel // Reverse-map to client model

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(responsesResp)
}

func handleResponsesStreamViaChat(r *http.Request, w http.ResponseWriter, url, apiKey string, reqBody []byte, model string, originalModel string, conversationID string) {
	model = originalModel // Use client model in all responses

	resp, err := doUpstreamRequest(r, url, apiKey, reqBody, true)
	if err != nil {
		writeError(r, w, http.StatusBadGateway, "upstream error: "+err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		w.Write(reqBody)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(r, w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	// Fallback: if upstream returned JSON instead of SSE
	upstreamContentType := resp.Header.Get("Content-Type")
	if strings.HasPrefix(upstreamContentType, "application/json") {
		respBody, _ := io.ReadAll(resp.Body)
		var chatResp ChatCompletionsResponse
		if err := json.Unmarshal(respBody, &chatResp); err == nil {
			var reasoningContent string
			for _, choice := range chatResp.Choices {
				if choice.Message != nil && choice.Message.ReasoningContent != "" {
					reasoningContent = choice.Message.ReasoningContent
					for _, tc := range choice.Message.ToolCalls {
						storeReasoningContentForConversation(conversationID, tc.ID, choice.Message.ReasoningContent)
					}
				}
			}
			var partial struct {
				ReasoningEffort string `json:"reasoning_effort"`
			}
			json.Unmarshal(reqBody, &partial)
			responsesResp, err := ConvertChatRespToResponsesResp(&chatResp, partial.ReasoningEffort, reasoningContent)
			if err == nil {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache")
				w.Header().Set("Connection", "keep-alive")
				w.Header().Set("X-Accel-Buffering", "no")
				w.WriteHeader(http.StatusOK)

				seqNum := 0
				responseID := generateID("resp_")
				baseResponse := map[string]interface{}{
					"id": responseID, "object": "response", "created_at": responsesResp.CreatedAt,
					"status": responsesResp.Status, "model": model, "output": []interface{}{},
				}
				writeResponsesSSE(w, "response.created", map[string]interface{}{
					"type": "response.created", "response": baseResponse, "sequence_number": seqNum,
				})
				flusher.Flush()
				seqNum++
				writeResponsesSSE(w, "response.in_progress", map[string]interface{}{
					"type": "response.in_progress", "response": baseResponse, "sequence_number": seqNum,
				})
				flusher.Flush()
				seqNum++

				var outputItems []interface{}
				for _, item := range responsesResp.Output {
					outputItems = append(outputItems, item)
				}

				completedResponse := map[string]interface{}{
					"id": responseID, "object": "response", "created_at": responsesResp.CreatedAt,
					"status": responsesResp.Status, "completed_at": time.Now().Unix(),
					"model": model, "output": outputItems,
				}
				if responsesResp.Usage != nil {
					completedResponse["usage"] = map[string]interface{}{
						"input_tokens":  responsesResp.Usage.InputTokens,
						"output_tokens": responsesResp.Usage.OutputTokens,
						"total_tokens":  responsesResp.Usage.TotalTokens,
					}
				}
				// Emit reasoning output item events for Codex thinking display
				if reasoningContent != "" {
					reasoningID := generateID("reasoning_")
					writeResponsesSSE(w, "response.output_item.added", map[string]interface{}{
						"type": "response.output_item.added", "output_index": 0,
						"item": map[string]interface{}{
							"id": reasoningID, "type": "reasoning", "status": "in_progress",
						},
						"sequence_number": seqNum,
					})
					flusher.Flush()
					seqNum++

					writeResponsesSSE(w, "response.reasoning_summary_text.delta", map[string]interface{}{
						"type": "response.reasoning_summary_text.delta", "content_index": 0,
						"item_id": reasoningID, "output_index": 0,
						"delta": reasoningContent, "sequence_number": seqNum,
					})
					flusher.Flush()
					seqNum++

					writeResponsesSSE(w, "response.reasoning_summary_text.done", map[string]interface{}{
						"type": "response.reasoning_summary_text.done", "content_index": 0,
						"item_id": reasoningID, "output_index": 0,
						"summary": reasoningContent, "sequence_number": seqNum,
					})
					flusher.Flush()
					seqNum++

					writeResponsesSSE(w, "response.output_item.done", map[string]interface{}{
						"type": "response.output_item.done", "output_index": 0,
						"item": map[string]interface{}{
							"id": reasoningID, "type": "reasoning", "status": "completed",
							"summary": []map[string]interface{}{{"type": "summary_text", "text": reasoningContent}},
						},
						"sequence_number": seqNum,
					})
					flusher.Flush()
					seqNum++
				}

				if responsesResp.Reasoning != nil {
					completedResponse["reasoning"] = json.RawMessage(responsesResp.Reasoning)
				}

				// Include reasoning output item in response.completed output
				if reasoningContent != "" {
					reasoningOutputItem := map[string]interface{}{
						"type":    "reasoning",
						"status":  "completed",
						"id":      generateID("reasoning_"),
						"summary": []map[string]interface{}{{"type": "summary_text", "text": reasoningContent}},
					}
					// Prepend reasoning before text message in output
					outputItems = append([]interface{}{reasoningOutputItem}, outputItems...)
					completedResponse["output"] = outputItems
				}

				writeResponsesSSE(w, "response.completed", map[string]interface{}{
					"type": "response.completed", "response": completedResponse, "sequence_number": seqNum,
				})
				flusher.Flush()
			}
		}
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

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	var fullText strings.Builder
	var fullRefusal strings.Builder
	var fullReasoning strings.Builder
	var chatUsage *ChatUsage
	var toolCalls []ToolCall
	toolCallMap := make(map[int]*ToolCall)
	var finishReason string
	var finishReasonSeen bool
	var contentPartAdded bool
	var contentType string // "output_text" or "refusal"
	var sseParsed bool
	var reasoningEmitted bool
	var reasoningID string
	var textOutputIndex int // 0 normally, 1 if reasoning is emitted first

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		sseParsed = true
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk ChatCompletionsChunk
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

		if choice.FinishReason == nil && !finishReasonSeen {
			// Reasoning content — emit inline BEFORE text events
			if choice.Delta.ReasoningContent != nil && *choice.Delta.ReasoningContent != "" {
				rc := *choice.Delta.ReasoningContent
				fullReasoning.WriteString(rc)
				if !reasoningEmitted {
					reasoningEmitted = true
					textOutputIndex = 1
					reasoningID = generateID("reasoning_")
					writeResponsesSSE(w, "response.output_item.added", map[string]interface{}{
						"type": "response.output_item.added", "output_index": 0,
						"item": map[string]interface{}{
							"id": reasoningID, "type": "reasoning", "status": "in_progress",
						},
						"sequence_number": seqNum,
					})
					flusher.Flush()
					seqNum++
				}
				writeResponsesSSE(w, "response.reasoning_summary_text.delta", map[string]interface{}{
					"type": "response.reasoning_summary_text.delta", "content_index": 0,
					"item_id": reasoningID, "output_index": 0,
					"delta": rc, "sequence_number": seqNum,
				})
				flusher.Flush()
				seqNum++
			}
			// Text content
			if choice.Delta.Content != nil && *choice.Delta.Content != "" {
				delta := *choice.Delta.Content
				fullText.WriteString(delta)

				if !contentPartAdded {
					contentPartAdded = true
					contentType = "output_text"
					textOutputIndex := 0
					if reasoningEmitted {
						textOutputIndex = 1
						// Finalize reasoning BEFORE text events
						writeResponsesSSE(w, "response.reasoning_summary_text.done", map[string]interface{}{
							"type": "response.reasoning_summary_text.done", "content_index": 0,
							"item_id": reasoningID, "output_index": 0,
							"summary": fullReasoning.String(), "sequence_number": seqNum,
						})
						flusher.Flush()
						seqNum++
						writeResponsesSSE(w, "response.output_item.done", map[string]interface{}{
							"type": "response.output_item.done", "output_index": 0,
							"item": map[string]interface{}{
								"id": reasoningID, "type": "reasoning", "status": "completed",
								"summary": []map[string]interface{}{{"type": "summary_text", "text": fullReasoning.String()}},
							},
							"sequence_number": seqNum,
						})
						flusher.Flush()
						seqNum++
					}
					writeResponsesSSE(w, "response.output_item.added", map[string]interface{}{
						"type": "response.output_item.added", "output_index": textOutputIndex,
						"item": map[string]interface{}{
							"id": msgID, "type": "message", "status": "in_progress",
							"content": []interface{}{}, "role": "assistant",
						},
						"sequence_number": seqNum,
					})
					flusher.Flush()
					seqNum++
					writeResponsesSSE(w, "response.content_part.added", map[string]interface{}{
						"type": "response.content_part.added", "content_index": 0,
						"item_id": msgID, "output_index": textOutputIndex,
						"part": map[string]interface{}{
							"type": "output_text", "annotations": []interface{}{}, "text": "",
						},
						"sequence_number": seqNum,
					})
					flusher.Flush()
					seqNum++
				}

				writeResponsesSSE(w, "response.output_text.delta", map[string]interface{}{
					"type": "response.output_text.delta", "content_index": 0,
					"item_id": msgID, "output_index": textOutputIndex,
					"delta": delta, "sequence_number": seqNum,
				})
				flusher.Flush()
				seqNum++
			}

			// Refusal
			if choice.Delta.Refusal != nil && *choice.Delta.Refusal != "" {
				refusalDelta := *choice.Delta.Refusal
				fullRefusal.WriteString(refusalDelta)

				if !contentPartAdded {
					contentPartAdded = true
					contentType = "refusal"
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
					writeResponsesSSE(w, "response.content_part.added", map[string]interface{}{
						"type": "response.content_part.added", "content_index": 0,
						"item_id": msgID, "output_index": textOutputIndex,
						"part": map[string]interface{}{
							"type": "refusal", "refusal": "",
						},
						"sequence_number": seqNum,
					})
					flusher.Flush()
					seqNum++
				}

				writeResponsesSSE(w, "response.refusal.delta", map[string]interface{}{
					"type": "response.refusal.delta", "content_index": 0,
					"item_id": msgID, "output_index": textOutputIndex,
					"delta": refusalDelta, "sequence_number": seqNum,
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
			finishReasonSeen = true
			continue
		}
	}

	// If upstream returned JSON but Content-Type was text/event-stream,
	// fall back to JSON parsing
	if !sseParsed {
		var chatResp ChatCompletionsResponse
		respBody, _ := io.ReadAll(resp.Body)
		if err := json.Unmarshal(respBody, &chatResp); err == nil {
			var reasoningContent string
			for _, choice := range chatResp.Choices {
				if choice.Message != nil && choice.Message.ReasoningContent != "" {
					reasoningContent = choice.Message.ReasoningContent
					for _, tc := range choice.Message.ToolCalls {
						storeReasoningContentForConversation(conversationID, tc.ID, choice.Message.ReasoningContent)
					}
				}
			}
			var partial struct {
				ReasoningEffort string `json:"reasoning_effort"`
			}
			json.Unmarshal(reqBody, &partial)
			responsesResp, err := ConvertChatRespToResponsesResp(&chatResp, partial.ReasoningEffort, reasoningContent)
			if err == nil {
				var outputItems []interface{}
				for _, item := range responsesResp.Output {
					outputItems = append(outputItems, item)
				}
				completedResponse := map[string]interface{}{
					"id": responseID, "object": "response", "created_at": created,
					"status": responsesResp.Status, "completed_at": time.Now().Unix(),
					"model": model, "output": outputItems,
				}
				if responsesResp.Usage != nil {
					completedResponse["usage"] = map[string]interface{}{
						"input_tokens":  responsesResp.Usage.InputTokens,
						"output_tokens": responsesResp.Usage.OutputTokens,
						"total_tokens":  responsesResp.Usage.TotalTokens,
					}
				}
				if responsesResp.Reasoning != nil {
					completedResponse["reasoning"] = json.RawMessage(responsesResp.Reasoning)
				}
				if reasoningContent != "" {
					reasoningOutputItem := map[string]interface{}{
						"type":    "reasoning",
						"status":  "completed",
						"id":      generateID("reasoning_"),
						"summary": []map[string]interface{}{{"type": "summary_text", "text": reasoningContent}},
					}
					outputItems = append([]interface{}{reasoningOutputItem}, outputItems...)
					completedResponse["output"] = outputItems
				}
				writeResponsesSSE(w, "response.completed", map[string]interface{}{
					"type": "response.completed", "response": completedResponse, "sequence_number": seqNum,
				})
				flusher.Flush()
				seqNum++
				return
			}
		}
	}

	// Finalize tool calls in deterministic order
	toolCallIndices := make([]int, 0, len(toolCallMap))
	for idx := range toolCallMap {
		toolCallIndices = append(toolCallIndices, idx)
	}
	sort.Ints(toolCallIndices)

	for _, idx := range toolCallIndices {
		tc := toolCallMap[idx]
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

	// Cache reasoning content for injection into next request's assistant messages
	if fullReasoning.Len() > 0 {
		rc := fullReasoning.String()
		for _, tc := range toolCalls {
			storeReasoningContentForConversation(conversationID, tc.ID, rc)
		}

		// Finalize reasoning if NO text content was received (reasoning-only response)
		if reasoningEmitted && !contentPartAdded {
			writeResponsesSSE(w, "response.reasoning_summary_text.done", map[string]interface{}{
				"type": "response.reasoning_summary_text.done", "content_index": 0,
				"item_id": reasoningID, "output_index": 0,
				"summary": rc, "sequence_number": seqNum,
			})
			flusher.Flush()
			seqNum++

			writeResponsesSSE(w, "response.output_item.done", map[string]interface{}{
				"type": "response.output_item.done", "output_index": 0,
				"item": map[string]interface{}{
					"id": reasoningID, "type": "reasoning", "status": "completed",
					"summary": []map[string]interface{}{{"type": "summary_text", "text": rc}},
				},
				"sequence_number": seqNum,
			})
			flusher.Flush()
			seqNum++
		}
	}

	// Finalize message content
	if reasoningEmitted {
		textOutputIndex = 1
	}
	if contentPartAdded {
		if contentType == "refusal" {
			writeResponsesSSE(w, "response.refusal.done", map[string]interface{}{
				"type": "response.refusal.done", "content_index": 0,
				"item_id": msgID, "output_index": textOutputIndex,
				"refusal": fullRefusal.String(), "sequence_number": seqNum,
			})
			flusher.Flush()
			seqNum++

			writeResponsesSSE(w, "response.content_part.done", map[string]interface{}{
				"type": "response.content_part.done", "content_index": 0,
				"item_id": msgID, "output_index": textOutputIndex,
				"part": map[string]interface{}{
					"type": "refusal", "refusal": fullRefusal.String(),
				},
				"sequence_number": seqNum,
			})
			flusher.Flush()
			seqNum++
		} else {
			writeResponsesSSE(w, "response.output_text.done", map[string]interface{}{
				"type": "response.output_text.done", "content_index": 0,
				"item_id": msgID, "output_index": textOutputIndex,
				"text": fullText.String(), "sequence_number": seqNum,
			})
			flusher.Flush()
			seqNum++

			writeResponsesSSE(w, "response.content_part.done", map[string]interface{}{
				"type": "response.content_part.done", "content_index": 0,
				"item_id": msgID, "output_index": textOutputIndex,
				"part": map[string]interface{}{
					"type": "output_text", "annotations": []interface{}{}, "text": fullText.String(),
				},
				"sequence_number": seqNum,
			})
			flusher.Flush()
			seqNum++
		}

		var msgContent []map[string]interface{}
		if contentType == "refusal" {
			msgContent = []map[string]interface{}{
				{"type": "refusal", "refusal": fullRefusal.String()},
			}
		} else {
			msgContent = []map[string]interface{}{
				{"type": "output_text", "annotations": []interface{}{}, "text": fullText.String()},
			}
		}
		writeResponsesSSE(w, "response.output_item.done", map[string]interface{}{
			"type": "response.output_item.done", "output_index": 0,
			"item": map[string]interface{}{
				"id": msgID, "type": "message", "status": "completed", "role": "assistant",
				"content": msgContent,
			},
			"sequence_number": seqNum,
		})
		flusher.Flush()
		seqNum++
	}

	// Build final output
	var outputItems []interface{}
	if contentPartAdded {
		var msgContent []map[string]interface{}
		if contentType == "refusal" {
			msgContent = []map[string]interface{}{
				{"type": "refusal", "refusal": fullRefusal.String()},
			}
		} else {
			msgContent = []map[string]interface{}{
				{"type": "output_text", "annotations": []interface{}{}, "text": fullText.String()},
			}
		}
		outputItems = append(outputItems, map[string]interface{}{
			"id": msgID, "type": "message", "status": "completed", "role": "assistant",
			"content": msgContent,
		})
	}

	// Include reasoning output item if reasoning was collected
	if fullReasoning.Len() > 0 {
		reasoningOutputItem := map[string]interface{}{
			"type":    "reasoning",
			"status":  "completed",
			"id":      generateID("reasoning_"),
			"summary": []map[string]interface{}{{"type": "summary_text", "text": fullReasoning.String()}},
		}
		outputItems = append([]interface{}{reasoningOutputItem}, outputItems...)
	}

	for _, tc := range toolCalls {
		outputItems = append(outputItems, map[string]interface{}{
			"id": tc.ID, "type": "function_call", "status": "completed",
			"call_id": tc.ID, "name": tc.Function.Name, "arguments": tc.Function.Arguments,
		})
	}

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

	completedResponse := map[string]interface{}{
		"id": responseID, "object": "response", "created_at": created,
		"status": finalStatus, "completed_at": time.Now().Unix(),
		"model": model, "output": outputItems, "usage": usage,
	}
	if chatUsage != nil && chatUsage.CompletionTokensDetails != nil &&
		chatUsage.CompletionTokensDetails.ReasoningTokens > 0 {
		var re struct {
			ReasoningEffort string `json:"reasoning_effort"`
		}
		json.Unmarshal(reqBody, &re)
		effort := re.ReasoningEffort
		if effort == "" {
			effort = "high"
		}
		summary := fullReasoning.String()
		if summary != "" {
			summaryJSON, _ := json.Marshal([]map[string]interface{}{
				{"type": "summary_text", "text": summary},
			})
			completedResponse["reasoning"] = json.RawMessage(
				fmt.Sprintf(`{"effort":%q,"summary":%s}`, effort, summaryJSON))
		} else {
			completedResponse["reasoning"] = json.RawMessage(
				fmt.Sprintf(`{"effort":%q,"summary":null}`, effort))
		}
	}
	writeResponsesSSE(w, "response.completed", map[string]interface{}{
		"type": "response.completed", "response": completedResponse, "sequence_number": seqNum,
	})
	flusher.Flush()
}

// ==================== Pass-through ====================

func handlePassthrough(w http.ResponseWriter, r *http.Request) {
	apiKey := resolveAPIKey(r, cfg.ResponsesAPIKey, cfg.APIKeyPassthroughTransparent && cfg.APIKeyPassthroughResponsesAPI)

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
		writeError(r, w, http.StatusBadGateway, "failed to create request")
		return
	}

	req.Header.Set("Authorization", "Bearer "+apiKey)
	if ct := r.Header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}

	// Forward client IP
	clientIP := r.RemoteAddr
	if host, _, err := net.SplitHostPort(clientIP); err == nil {
		clientIP = host
	}
	if prior := r.Header.Get("X-Forwarded-For"); prior != "" {
		if !isPrivateOrLoopback(clientIP) {
			req.Header.Set("X-Forwarded-For", prior+", "+clientIP)
		} else {
			req.Header.Set("X-Forwarded-For", prior)
		}
	} else if !isPrivateOrLoopback(clientIP) {
		req.Header.Set("X-Forwarded-For", clientIP)
	}
	if realIP := r.Header.Get("X-Real-IP"); realIP != "" {
		req.Header.Set("X-Real-IP", realIP)
	} else if !isPrivateOrLoopback(clientIP) {
		req.Header.Set("X-Real-IP", clientIP)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		writeError(r, w, http.StatusBadGateway, "upstream error: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if rl := LoggerFromReq(r); rl != nil {
		resp.Body = rl.NewUpstreamBodyTeeCloser(resp.Body, resp.StatusCode, false)
	}

	for k, v := range resp.Header {
		for _, vv := range v {
			w.Header().Add(k, vv)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// ==================== Transparent Pass-through ====================

func handleTransparent(w http.ResponseWriter, r *http.Request) {
	upstreamBaseURL, upstreamKey, passthrough := transparentUpstreamConfigForPath(r.URL.Path)
	apiKey := resolveAPIKey(r, upstreamKey, passthrough)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(r, w, http.StatusBadRequest, "failed to read request body")
		return
	}
	defer r.Body.Close()

	upstreamURL := upstreamBaseURL + r.URL.Path
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}

	// Apply model mapping
	rl := LoggerFromReq(r)
	var clientModel, mappedModel string
	if cfg.ModelMapTransparent && len(cfg.ModelMap) > 0 {
		var partial struct {
			Model string `json:"model"`
		}
		json.Unmarshal(body, &partial)
		if m, ok := cfg.ModelMap[partial.Model]; ok {
			clientModel = partial.Model
			mappedModel = m
			body = mapModelInJSON(body, clientModel, mappedModel)
			rl.Filef("[transparent] model mapping: %s → %s", clientModel, mappedModel)
			rl.SetModelMapping(clientModel, mappedModel)
		}
	}

	// Apply reasoning_effort mapping (both Chat Completions and Responses shapes)
	if cfg.ReasoningEffortMapTransparent && len(cfg.ReasoningEffortMap) > 0 {
		body = mapReasoningEffortInJSON(body, cfg.ReasoningEffortMap, rl, "[transparent]")
	}
	rl.WriteConvertedRequest(body)

	rl.Filef("[transparent] path=%s (%d bytes)", r.URL.Path, len(body))

	req, err := http.NewRequest(r.Method, upstreamURL, bytes.NewReader(body))
	if err != nil {
		writeError(r, w, http.StatusBadGateway, "failed to create request")
		return
	}

	req.Header.Set("Authorization", "Bearer "+apiKey)
	if ct := r.Header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if r.Header.Get("Accept") != "" {
		req.Header.Set("Accept", r.Header.Get("Accept"))
	}

	clientIP := r.RemoteAddr
	if host, _, err := net.SplitHostPort(clientIP); err == nil {
		clientIP = host
	}
	if prior := r.Header.Get("X-Forwarded-For"); prior != "" {
		if !isPrivateOrLoopback(clientIP) {
			req.Header.Set("X-Forwarded-For", prior+", "+clientIP)
		} else {
			req.Header.Set("X-Forwarded-For", prior)
		}
	} else if !isPrivateOrLoopback(clientIP) {
		req.Header.Set("X-Forwarded-For", clientIP)
	}
	if realIP := r.Header.Get("X-Real-IP"); realIP != "" {
		req.Header.Set("X-Real-IP", realIP)
	} else if !isPrivateOrLoopback(clientIP) {
		req.Header.Set("X-Real-IP", clientIP)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		writeError(r, w, http.StatusBadGateway, "upstream error: "+err.Error())
		return
	}
	defer resp.Body.Close()
	transparentStreaming := strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
	if rl != nil {
		rl.SetStream(transparentStreaming)
		resp.Body = rl.NewUpstreamBodyTeeCloser(resp.Body, resp.StatusCode, transparentStreaming)
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		writeError(r, w, http.StatusBadGateway, "failed to read upstream response")
		return
	}

	// Reverse-map model in response so client sees original model
	if clientModel != "" && mappedModel != "" {
		respBody = mapModelInJSON(respBody, mappedModel, clientModel)
	}

	for k, v := range resp.Header {
		for _, vv := range v {
			w.Header().Add(k, vv)
		}
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
}

func transparentUpstreamConfigForPath(path string) (baseURL, apiKey string, passthrough bool) {
	switch path {
	case "/v1/responses":
		return cfg.CompletionsAPIBaseURL, cfg.CompletionsAPIKey, cfg.APIKeyPassthroughTransparent && cfg.APIKeyPassthroughCompletionsAPI
	default:
		return cfg.ResponsesAPIBaseURL, cfg.ResponsesAPIKey, cfg.APIKeyPassthroughTransparent && cfg.APIKeyPassthroughResponsesAPI
	}
}

// ==================== Utilities ====================

var httpClient = &http.Client{
	Timeout: 5 * time.Minute,
}

func isPrivateOrLoopback(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	return ip.IsPrivate() || ip.IsLoopback()
}

func doUpstreamRequest(origReq *http.Request, url, apiKey string, body []byte, streaming bool) (*http.Response, error) {
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	if streaming {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}

	// Forward client IP
	if origReq != nil {
		clientIP := origReq.RemoteAddr
		if host, _, err := net.SplitHostPort(clientIP); err == nil {
			clientIP = host
		}
		if prior := origReq.Header.Get("X-Forwarded-For"); prior != "" {
			if !isPrivateOrLoopback(clientIP) {
				req.Header.Set("X-Forwarded-For", prior+", "+clientIP)
			} else {
				req.Header.Set("X-Forwarded-For", prior)
			}
		} else if !isPrivateOrLoopback(clientIP) {
			req.Header.Set("X-Forwarded-For", clientIP)
		}
		if realIP := origReq.Header.Get("X-Real-IP"); realIP != "" {
			req.Header.Set("X-Real-IP", realIP)
		} else if !isPrivateOrLoopback(clientIP) {
			req.Header.Set("X-Real-IP", clientIP)
		}
	}

	LoggerFromReq(origReq).Filef("[upstream] POST %s (%d bytes) streaming=%v", url, len(body), streaming)
	resp, err := httpClient.Do(req)
	if err != nil || resp == nil {
		return resp, err
	}
	if rl := LoggerFromReq(origReq); rl != nil {
		rl.Filef("[upstream] status=%d content-type=%s", resp.StatusCode, resp.Header.Get("Content-Type"))
		resp.Body = rl.NewUpstreamBodyTeeCloser(resp.Body, resp.StatusCode, streaming)
	}
	return resp, err
}

func extractAPIKey(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}

// resolveAPIKey picks the API key forwarded upstream.
// passthrough=true  → prefer the client's Authorization (fallback to upstreamKey).
// passthrough=false → always use upstreamKey, ignoring the client header.
func resolveAPIKey(r *http.Request, upstreamKey string, passthrough bool) string {
	if passthrough {
		if k := extractAPIKey(r); k != "" {
			return k
		}
	}
	return upstreamKey
}

func writeError(r *http.Request, w http.ResponseWriter, code int, msg string) {
	LoggerFromReq(r).Filef("[error] %d: %s", code, msg)
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

func truncateLog(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
