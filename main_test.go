package main

import (
	"flag"
	"os"
	"testing"
)

func withIsolatedConfig(t *testing.T) {
	t.Helper()

	oldArgs := os.Args
	oldCommandLine := flag.CommandLine
	oldCfg := cfg

	os.Args = []string{"openai-converter.test"}
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)

	t.Cleanup(func() {
		os.Args = oldArgs
		flag.CommandLine = oldCommandLine
		cfg = oldCfg
	})
}

func TestLoadConfigSplitsTransparentModeByEndpoint(t *testing.T) {
	withIsolatedConfig(t)

	t.Setenv("TRANSPARENT_ENABLED", "false")
	t.Setenv("TRANSPARENT_CHAT_COMPLETIONS_ENABLED", "true")
	t.Setenv("TRANSPARENT_RESPONSES_ENABLED", "false")

	loadConfig()

	if !cfg.TransparentChatCompletionsEnabled {
		t.Fatal("TransparentChatCompletionsEnabled = false, want true")
	}
	if cfg.TransparentResponsesEnabled {
		t.Fatal("TransparentResponsesEnabled = true, want false")
	}
}

func TestLoadConfigUsesLegacyTransparentEnabledAsDefault(t *testing.T) {
	withIsolatedConfig(t)

	t.Setenv("TRANSPARENT_ENABLED", "true")

	loadConfig()

	if !cfg.TransparentChatCompletionsEnabled {
		t.Fatal("TransparentChatCompletionsEnabled = false, want true from TRANSPARENT_ENABLED")
	}
	if !cfg.TransparentResponsesEnabled {
		t.Fatal("TransparentResponsesEnabled = false, want true from TRANSPARENT_ENABLED")
	}
}

func TestLoadConfigSpecificTransparentEnvOverridesLegacyDefault(t *testing.T) {
	withIsolatedConfig(t)

	t.Setenv("TRANSPARENT_ENABLED", "true")
	t.Setenv("TRANSPARENT_CHAT_COMPLETIONS_ENABLED", "false")
	t.Setenv("TRANSPARENT_RESPONSES_ENABLED", "true")

	loadConfig()

	if cfg.TransparentChatCompletionsEnabled {
		t.Fatal("TransparentChatCompletionsEnabled = true, want false from specific env")
	}
	if !cfg.TransparentResponsesEnabled {
		t.Fatal("TransparentResponsesEnabled = false, want true from specific env")
	}
}

func TestLoadConfigSupportsLegacyTransparentFlagWithSpecificOverride(t *testing.T) {
	withIsolatedConfig(t)

	os.Args = []string{
		"openai-converter.test",
		"-transparent",
		"-transparent-responses=false",
	}

	loadConfig()

	if !cfg.TransparentChatCompletionsEnabled {
		t.Fatal("TransparentChatCompletionsEnabled = false, want true from -transparent")
	}
	if cfg.TransparentResponsesEnabled {
		t.Fatal("TransparentResponsesEnabled = true, want false from -transparent-responses=false")
	}
}

func TestTransparentEnabledForPath(t *testing.T) {
	oldCfg := cfg
	cfg = Config{
		TransparentChatCompletionsEnabled: true,
		TransparentResponsesEnabled:       false,
	}
	t.Cleanup(func() {
		cfg = oldCfg
	})

	if !transparentEnabledForPath("/v1/chat/completions") {
		t.Fatal("chat completions path transparent = false, want true")
	}
	if transparentEnabledForPath("/v1/responses") {
		t.Fatal("responses path transparent = true, want false")
	}
	if transparentEnabledForPath("/v1/models") {
		t.Fatal("models path transparent = true, want false")
	}
}

func TestTransparentUpstreamConfigForPathUsesDirectionUpstream(t *testing.T) {
	oldCfg := cfg
	cfg = Config{
		ResponsesAPIBaseURL:             "https://responses.example",
		ResponsesAPIKey:                 "responses-key",
		CompletionsAPIBaseURL:           "https://completions.example",
		CompletionsAPIKey:               "completions-key",
		APIKeyPassthroughTransparent:    true,
		APIKeyPassthroughResponsesAPI:   false,
		APIKeyPassthroughCompletionsAPI: true,
	}
	t.Cleanup(func() {
		cfg = oldCfg
	})

	baseURL, apiKey, passthrough := transparentUpstreamConfigForPath("/v1/chat/completions")
	if baseURL != "https://responses.example" || apiKey != "responses-key" || passthrough {
		t.Fatalf("chat upstream = (%q, %q, %v), want direction 1 upstream with passthrough=false", baseURL, apiKey, passthrough)
	}

	baseURL, apiKey, passthrough = transparentUpstreamConfigForPath("/v1/responses")
	if baseURL != "https://completions.example" || apiKey != "completions-key" || !passthrough {
		t.Fatalf("responses upstream = (%q, %q, %v), want direction 2 upstream with passthrough=true", baseURL, apiKey, passthrough)
	}
}
