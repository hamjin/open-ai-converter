package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
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

	respReq, err := ConvertChatToResponsesRequest(&chatReq)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "conversion error: "+err.Error())
		return
	}

	respBody, err := json.Marshal(respReq)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "marshal error: "+err.Error())
		return
	}

	log.Printf("[chat→resp] converted body: %s", truncateLog(string(respBody), 2000))

	upstreamURL := cfg.ResponsesAPIBaseURL + "/v1/responses"

	if chatReq.Stream {
		handleChatStreamViaResponses(r, w, upstreamURL, apiKey, respBody, chatReq.Model)
	} else {
		handleChatNonStream(r, w, upstreamURL, apiKey, respBody)
	}
}

func handleChatNonStream(r *http.Request, w http.ResponseWriter, url, apiKey string, reqBody []byte) {
	resp, err := doUpstreamRequest(r, url, apiKey, reqBody, false)
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

func handleChatStreamViaResponses(r *http.Request, w http.ResponseWriter, url, apiKey string, reqBody []byte, model string) {
	resp, err := doUpstreamRequest(r, url, apiKey, reqBody, true)
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
						cc.Delta = ChatDelta{
							Role:      "assistant",
							ToolCalls: choice.Message.ToolCalls,
							Refusal:   choice.Message.Refusal,
						}
						text := contentToString(choice.Message.Content)
						if text != "" {
							cc.Delta.Content = &text
						}
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

	chatReq, err := ConvertResponsesToChatRequest(&respReq)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "conversion error: "+err.Error())
		return
	}

	chatBody, err := json.Marshal(chatReq)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "marshal error: "+err.Error())
		return
	}

	log.Printf("[resp→chat] converted body: %s", truncateLog(string(chatBody), 500))

	upstreamURL := cfg.CompletionsAPIBaseURL + "/v1/chat/completions"

	if respReq.Stream {
		handleResponsesStreamViaChat(r, w, upstreamURL, apiKey, chatBody, respReq.Model)
	} else {
		handleResponsesNonStream(r, w, upstreamURL, apiKey, chatBody)
	}
}

func handleResponsesNonStream(r *http.Request, w http.ResponseWriter, url, apiKey string, reqBody []byte) {
	resp, err := doUpstreamRequest(r, url, apiKey, reqBody, false)
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

func handleResponsesStreamViaChat(r *http.Request, w http.ResponseWriter, url, apiKey string, reqBody []byte, model string) {
	resp, err := doUpstreamRequest(r, url, apiKey, reqBody, true)
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

	// Fallback: if upstream returned JSON instead of SSE
	upstreamContentType := resp.Header.Get("Content-Type")
	if strings.HasPrefix(upstreamContentType, "application/json") {
		respBody, _ := io.ReadAll(resp.Body)
		var chatResp ChatCompletionsResponse
		if err := json.Unmarshal(respBody, &chatResp); err == nil {
			responsesResp, err := ConvertChatRespToResponsesResp(&chatResp)
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
	var chatUsage *ChatUsage
	var toolCalls []ToolCall
	toolCallMap := make(map[int]*ToolCall)
	var finishReason string
	var finishReasonSeen bool
	var contentPartAdded bool
	var contentType string // "output_text" or "refusal"

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
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
			// Text content
			if choice.Delta.Content != nil && *choice.Delta.Content != "" {
				delta := *choice.Delta.Content
				fullText.WriteString(delta)

				if !contentPartAdded {
					contentPartAdded = true
					contentType = "output_text"
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
						"item_id": msgID, "output_index": 0,
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
					"item_id": msgID, "output_index": 0,
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
						"item_id": msgID, "output_index": 0,
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
					"item_id": msgID, "output_index": 0,
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

	// Finalize message content
	if contentPartAdded {
		if contentType == "refusal" {
			writeResponsesSSE(w, "response.refusal.done", map[string]interface{}{
				"type": "response.refusal.done", "content_index": 0,
				"item_id": msgID, "output_index": 0,
				"refusal": fullRefusal.String(), "sequence_number": seqNum,
			})
			flusher.Flush()
			seqNum++

			writeResponsesSSE(w, "response.content_part.done", map[string]interface{}{
				"type": "response.content_part.done", "content_index": 0,
				"item_id": msgID, "output_index": 0,
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
				"item_id": msgID, "output_index": 0,
				"text": fullText.String(), "sequence_number": seqNum,
			})
			flusher.Flush()
			seqNum++

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

	log.Printf("[upstream] POST %s (%d bytes) streaming=%v", url, len(body), streaming)
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

func truncateLog(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
