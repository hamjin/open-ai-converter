package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func captureUpstreamRequest(t *testing.T) <-chan *http.Request {
	t.Helper()

	oldClient := httpClient
	captured := make(chan *http.Request, 1)

	httpClient = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			clone := req.Clone(req.Context())
			clone.Header = req.Header.Clone()
			clone.Host = req.Host
			captured <- clone

			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
				Request:    req,
			}, nil
		}),
	}

	t.Cleanup(func() {
		httpClient = oldClient
	})

	return captured
}

func TestDoUpstreamRequestUsesUpstreamHostRejectsCompressionAndForwardsHeaders(t *testing.T) {
	captured := captureUpstreamRequest(t)

	origReq := httptest.NewRequest(http.MethodPost, "http://client.example/v1/chat/completions", strings.NewReader(`{}`))
	origReq.RemoteAddr = "127.0.0.1:12345"
	origReq.Host = "client.example"
	origReq.Header.Set("Accept-Encoding", "gzip, br")
	origReq.Header.Set("User-Agent", "open-ai-converter-test/1.0")
	origReq.Header.Set("X-Client-Trace", "trace-123")
	origReq.Header.Add("OpenAI-Beta", "assistants=v2")
	origReq.Header.Add("OpenAI-Beta", "responses=v1")

	resp, err := doUpstreamRequest(origReq, "https://upstream.example/v1/responses", "upstream-key", []byte(`{}`), false)
	if err != nil {
		t.Fatalf("doUpstreamRequest() error = %v", err)
	}
	defer resp.Body.Close()

	req := <-captured
	if req.Host != "upstream.example" {
		t.Fatalf("upstream Host = %q, want upstream.example", req.Host)
	}
	if got := req.Header.Get("Accept-Encoding"); got != "identity" {
		t.Fatalf("Accept-Encoding = %q, want identity", got)
	}
	if got := req.Header.Get("X-Client-Trace"); got != "trace-123" {
		t.Fatalf("X-Client-Trace = %q, want trace-123", got)
	}
	if got := req.Header.Get("User-Agent"); got != "open-ai-converter-test/1.0" {
		t.Fatalf("User-Agent = %q, want open-ai-converter-test/1.0", got)
	}
	if got, want := req.Header.Values("OpenAI-Beta"), []string{"assistants=v2", "responses=v1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("OpenAI-Beta = %v, want %v", got, want)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer upstream-key" {
		t.Fatalf("Authorization = %q, want Bearer upstream-key", got)
	}
	if got := req.Header.Get("Accept"); got != "application/json" {
		t.Fatalf("Accept = %q, want application/json", got)
	}
}

func TestHandleTransparentUsesUpstreamHostRejectsCompressionAndForwardsHeaders(t *testing.T) {
	oldCfg := cfg
	cfg = Config{
		CompletionsAPIBaseURL:           "https://chat-upstream.example",
		CompletionsAPIKey:               "configured-chat-key",
		APIKeyPassthroughTransparent:    true,
		APIKeyPassthroughCompletionsAPI: true,
	}
	t.Cleanup(func() {
		cfg = oldCfg
	})

	captured := captureUpstreamRequest(t)

	req := httptest.NewRequest(http.MethodPost, "http://client.example/v1/responses?debug=true", strings.NewReader(`{"model":"gpt-4o"}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Host = "client.example"
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Authorization", "Bearer client-token")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Client-Trace", "trace-456")

	rr := httptest.NewRecorder()
	handleTransparent(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	upstreamReq := <-captured
	if upstreamReq.URL.String() != "https://chat-upstream.example/v1/responses?debug=true" {
		t.Fatalf("upstream URL = %q, want chat upstream responses URL", upstreamReq.URL.String())
	}
	if upstreamReq.Host != "chat-upstream.example" {
		t.Fatalf("upstream Host = %q, want chat-upstream.example", upstreamReq.Host)
	}
	if got := upstreamReq.Header.Get("Accept-Encoding"); got != "identity" {
		t.Fatalf("Accept-Encoding = %q, want identity", got)
	}
	if got := upstreamReq.Header.Get("Authorization"); got != "Bearer client-token" {
		t.Fatalf("Authorization = %q, want Bearer client-token", got)
	}
	if got := upstreamReq.Header.Get("Accept"); got != "application/json" {
		t.Fatalf("Accept = %q, want application/json", got)
	}
	if got := upstreamReq.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if got := upstreamReq.Header.Get("X-Client-Trace"); got != "trace-456" {
		t.Fatalf("X-Client-Trace = %q, want trace-456", got)
	}
}
