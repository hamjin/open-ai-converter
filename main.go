package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
)

type Config struct {
	// Direction 1: Chat Completions client → upstream Responses API
	ResponsesAPIBaseURL string
	ResponsesAPIKey     string

	// Direction 2: Responses client → upstream Chat Completions API
	CompletionsAPIBaseURL string
	CompletionsAPIKey     string

	Host string
	Port int

	CacheDir string

	TransparentEnabled bool
}

var cfg Config

func loadConfig() {
	// Command line flags (override env vars)
	flag.StringVar(&cfg.ResponsesAPIBaseURL, "responses-url", envOrDefault("RESPONSES_API_BASE_URL", "https://codex.viloze.com"), "Upstream Responses API base URL")
	flag.StringVar(&cfg.ResponsesAPIKey, "responses-key", envOrDefault("RESPONSES_API_KEY", ""), "Upstream Responses API key")
	flag.StringVar(&cfg.CompletionsAPIBaseURL, "completions-url", envOrDefault("COMPLETIONS_API_BASE_URL", "https://api.openai.com"), "Upstream Chat Completions API base URL")
	flag.StringVar(&cfg.CompletionsAPIKey, "completions-key", envOrDefault("COMPLETIONS_API_KEY", ""), "Upstream Chat Completions API key")
	flag.StringVar(&cfg.Host, "host", envOrDefault("HOST", "0.0.0.0"), "Server host")
	flag.IntVar(&cfg.Port, "port", envIntOrDefault("PORT", 9090), "Server port")
	flag.StringVar(&cfg.CacheDir, "cache-dir", envOrDefault("CACHE_DIR", "cache"), "Directory for caching reasoning results")
	flag.BoolVar(&cfg.TransparentEnabled, "transparent", envOrDefault("TRANSPARENT_ENABLED", "false") == "true", "Enable transparent pass-through mode (no conversion)")
	flag.Parse()
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envIntOrDefault(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		var n int
		fmt.Sscanf(v, "%d", &n)
		if n > 0 {
			return n
		}
	}
	return def
}

func main() {
	// Load .env file if present
	loadDotEnv(".env")
	loadConfig()

	mux := http.NewServeMux()

	// Direction 1: Client speaks Chat Completions, proxy converts to Responses API
	if cfg.TransparentEnabled {
		mux.HandleFunc("/v1/chat/completions", handleTransparent)
	} else {
		mux.HandleFunc("/v1/chat/completions", handleChatCompletions)
	}

	// Direction 2: Client speaks Responses, proxy converts to Chat Completions API
	if cfg.TransparentEnabled {
		mux.HandleFunc("/v1/responses", handleTransparent)
	} else {
		mux.HandleFunc("/v1/responses", handleResponses)
	}

	// Pass-through for models and other endpoints
	mux.HandleFunc("/v1/models", handlePassthrough)

	// Health check
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})

	// Root handler with info
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			// Catch-all pass-through for other /v1/ paths
			if strings.HasPrefix(r.URL.Path, "/v1/") {
				handlePassthrough(w, r)
				return
			}
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
  "service": "OpenAI API Converter Proxy",
  "endpoints": {
    "/v1/chat/completions": "Chat Completions API (converts to upstream Responses API)",
    "/v1/responses": "Responses API (converts to upstream Chat Completions API)",
    "/v1/models": "Pass-through to upstream",
    "/health": "Health check"
  }
}`))
	})

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)

	log.Println("========================================")
	log.Println("  OpenAI API Converter Proxy")
	log.Println("========================================")
	log.Printf("  Listening on: http://%s", addr)
	log.Printf("  Responses upstream: %s", cfg.ResponsesAPIBaseURL)
	log.Printf("  Completions upstream: %s", cfg.CompletionsAPIBaseURL)
	if cfg.TransparentEnabled {
		log.Printf("  Transparent mode: ENABLED")
	} else {
		log.Println("  Transparent mode: DISABLED (set TRANSPARENT_ENABLED=true to enable)")
	}
	log.Println("")
	log.Println("  /v1/chat/completions → upstream Responses API")
	log.Println("  /v1/responses        → upstream Chat Completions API")
	log.Println("========================================")

	if err := http.ListenAndServe(addr, corsMiddleware(loggingMiddleware(mux))); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}

// Simple CORS middleware
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// Logging middleware
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("[%s] %s %s", r.Method, r.URL.Path, r.RemoteAddr)
		next.ServeHTTP(w, r)
	})
}

// Load .env file (simple implementation, no external deps)
func loadDotEnv(filename string) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		// Don't override existing env vars
		if os.Getenv(key) == "" {
			os.Setenv(key, val)
		}
	}
}
