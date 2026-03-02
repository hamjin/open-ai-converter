package main

import (
	"encoding/json"
	"time"
)

// ==================== Chat Completions API Types ====================

type ChatCompletionsRequest struct {
	Model               string          `json:"model"`
	Messages            []ChatMessage   `json:"messages"`
	Stream              bool            `json:"stream,omitempty"`
	MaxTokens           *int            `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int            `json:"max_completion_tokens,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	FrequencyPenalty    *float64        `json:"frequency_penalty,omitempty"`
	PresencePenalty     *float64        `json:"presence_penalty,omitempty"`
	Tools               []ChatTool      `json:"tools,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool           `json:"parallel_tool_calls,omitempty"`
	Stop                json.RawMessage `json:"stop,omitempty"`
	N                   *int            `json:"n,omitempty"`
	Seed                *int            `json:"seed,omitempty"`
	StreamOptions       *StreamOptions  `json:"stream_options,omitempty"`
	User                *string         `json:"user,omitempty"`
	ResponseFormat      json.RawMessage `json:"response_format,omitempty"`
	Logprobs            *bool           `json:"logprobs,omitempty"`
	TopLogprobs         *int            `json:"top_logprobs,omitempty"`
	ReasoningEffort     *string         `json:"reasoning_effort,omitempty"`
	ServiceTier         *string         `json:"service_tier,omitempty"`
	Store               *bool           `json:"store,omitempty"`
	Metadata            json.RawMessage `json:"metadata,omitempty"`
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

type ChatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	Name       string          `json:"name,omitempty"`
	ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Refusal    *string         `json:"refusal,omitempty"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Index    *int         `json:"index,omitempty"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ChatTool struct {
	Type     string       `json:"type"`
	Function ChatFunction `json:"function"`
}

type ChatFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

type ChatCompletionsResponse struct {
	ID                string       `json:"id"`
	Object            string       `json:"object"`
	Created           int64        `json:"created"`
	Model             string       `json:"model"`
	Choices           []ChatChoice `json:"choices"`
	Usage             *ChatUsage   `json:"usage,omitempty"`
	SystemFingerprint string       `json:"system_fingerprint,omitempty"`
	ServiceTier       string       `json:"service_tier,omitempty"`
}

type ChatChoice struct {
	Index        int             `json:"index"`
	Message      *ChatMessage    `json:"message,omitempty"`
	Delta        *ChatDelta      `json:"delta,omitempty"`
	FinishReason *string         `json:"finish_reason"`
	Logprobs     *ChoiceLogprobs `json:"logprobs,omitempty"`
}

type ChatDelta struct {
	Role      string     `json:"role,omitempty"`
	Content   *string    `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	Refusal   *string    `json:"refusal,omitempty"`
}

type ChatUsage struct {
	PromptTokens            int                      `json:"prompt_tokens"`
	CompletionTokens        int                      `json:"completion_tokens"`
	TotalTokens             int                      `json:"total_tokens"`
	CompletionTokensDetails *CompletionTokensDetails `json:"completion_tokens_details,omitempty"`
	PromptTokensDetails     *PromptTokensDetails     `json:"prompt_tokens_details,omitempty"`
}

type CompletionTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

type PromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens,omitempty"`
}

type ChoiceLogprobs struct {
	Content []LogprobContent `json:"content,omitempty"`
	Refusal []LogprobContent `json:"refusal,omitempty"`
}

type LogprobContent struct {
	Token       string       `json:"token"`
	Logprob     float64      `json:"logprob"`
	Bytes       []int        `json:"bytes,omitempty"`
	TopLogprobs []TopLogprob `json:"top_logprobs,omitempty"`
}

type TopLogprob struct {
	Token   string  `json:"token"`
	Logprob float64 `json:"logprob"`
	Bytes   []int   `json:"bytes,omitempty"`
}

// ==================== Chat Completions Vision ====================

type ChatContentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *ChatImageURL `json:"image_url,omitempty"`
}

type ChatImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// ==================== Responses API Types ====================

type ResponsesRequest struct {
	Model              string          `json:"model"`
	Input              json.RawMessage `json:"input"`
	Instructions       *string         `json:"instructions,omitempty"`
	Stream             bool            `json:"stream,omitempty"`
	MaxOutputTokens    *int            `json:"max_output_tokens,omitempty"`
	Temperature        *float64        `json:"temperature,omitempty"`
	TopP               *float64        `json:"top_p,omitempty"`
	Tools              json.RawMessage `json:"tools,omitempty"`
	ToolChoice         json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls  *bool           `json:"parallel_tool_calls,omitempty"`
	User               *string         `json:"user,omitempty"`
	Reasoning          json.RawMessage `json:"reasoning,omitempty"`
	Text               json.RawMessage `json:"text,omitempty"`
	Truncation         json.RawMessage `json:"truncation,omitempty"`
	Store              *bool           `json:"store,omitempty"`
	Metadata           json.RawMessage `json:"metadata,omitempty"`
	FrequencyPenalty   *float64        `json:"frequency_penalty,omitempty"`
	PresencePenalty    *float64        `json:"presence_penalty,omitempty"`
	PreviousResponseID *string         `json:"previous_response_id,omitempty"`
	ServiceTier        *string         `json:"service_tier,omitempty"`
	TopLogprobs        *int            `json:"top_logprobs,omitempty"`
}

type ResponsesInputMessage struct {
	Role      string          `json:"role,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	Type      string          `json:"type,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Output    string          `json:"output,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
	Status    string          `json:"status,omitempty"`
}

type ResponsesResponse struct {
	ID                string          `json:"id"`
	Object            string          `json:"object"`
	CreatedAt         int64           `json:"created_at"`
	Status            string          `json:"status"`
	Model             string          `json:"model"`
	Output            []OutputItem    `json:"output"`
	Usage             *ResponsesUsage `json:"usage,omitempty"`
	Error             json.RawMessage `json:"error,omitempty"`
	Instructions      *string         `json:"instructions,omitempty"`
	Temperature       *float64        `json:"temperature,omitempty"`
	TopP              *float64        `json:"top_p,omitempty"`
	MaxOutputTokens   *int            `json:"max_output_tokens,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"`
	Tools             json.RawMessage `json:"tools,omitempty"`
	Metadata          json.RawMessage `json:"metadata,omitempty"`
	ServiceTier       string          `json:"service_tier,omitempty"`
	IncompleteDetails json.RawMessage `json:"incomplete_details,omitempty"`
	Reasoning         json.RawMessage `json:"reasoning,omitempty"`
	Text              json.RawMessage `json:"text,omitempty"`
}

type OutputItem struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Status    string          `json:"status,omitempty"`
	Role      string          `json:"role,omitempty"`
	Content   []ContentPart   `json:"content,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Summary   json.RawMessage `json:"summary,omitempty"`
}

type ContentPart struct {
	Type        string          `json:"type"`
	Text        string          `json:"text,omitempty"`
	Annotations json.RawMessage `json:"annotations,omitempty"`
	Refusal     string          `json:"refusal,omitempty"`
}

type ResponsesUsage struct {
	InputTokens         int                  `json:"input_tokens"`
	OutputTokens        int                  `json:"output_tokens"`
	TotalTokens         int                  `json:"total_tokens"`
	InputTokensDetails  *InputTokensDetails  `json:"input_tokens_details,omitempty"`
	OutputTokensDetails *OutputTokensDetails `json:"output_tokens_details,omitempty"`
}

type InputTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type OutputTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// ==================== Responses Vision ====================

type ResponsesContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	Detail   string `json:"detail,omitempty"`
	FileID   string `json:"file_id,omitempty"`
	Filename string `json:"filename,omitempty"`
}

// ==================== Structured Output Types ====================

type ResponseFormatObj struct {
	Type       string         `json:"type"`
	JSONSchema *JSONSchemaObj `json:"json_schema,omitempty"`
}

type JSONSchemaObj struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

type ResponsesTextFormat struct {
	Format TextFormatSpec `json:"format,omitempty"`
}

type TextFormatSpec struct {
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// ==================== Reasoning Types ====================

type ReasoningConfig struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// ==================== Streaming Event Types ====================

type ResponsesTextDelta struct {
	Type         string `json:"type"`
	ContentIndex int    `json:"content_index"`
	OutputIndex  int    `json:"output_index"`
	Delta        string `json:"delta"`
	ItemID       string `json:"item_id"`
}

type ResponsesOutputItemAdded struct {
	Type        string     `json:"type"`
	OutputIndex int        `json:"output_index"`
	Item        OutputItem `json:"item"`
}

type ResponsesCompleted struct {
	Type     string            `json:"type"`
	Response ResponsesResponse `json:"response"`
}

type ResponsesFunctionCallArgsDelta struct {
	Type        string `json:"type"`
	ItemID      string `json:"item_id"`
	OutputIndex int    `json:"output_index"`
	Delta       string `json:"delta"`
}

// ==================== Helpers ====================

func strPtr(s string) *string       { return &s }
func intPtr(i int) *int             { return &i }
func boolPtr(b bool) *bool          { return &b }
func float64Ptr(f float64) *float64 { return &f }
func nowUnix() int64                { return time.Now().Unix() }

func jsonString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

func contentToString(raw json.RawMessage) string {
	if raw == nil || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var result string
		for _, p := range parts {
			if p.Type == "text" || p.Type == "output_text" || p.Type == "input_text" {
				result += p.Text
			}
		}
		return result
	}
	return string(raw)
}
