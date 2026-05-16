package main

import (
	"encoding/json"
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

	LogEnabled        bool
	ConvertLogDir     string
	TransparentLogDir string

	TransparentEnabled bool

	ModelMap            map[string]string
	ModelMapTransparent bool
	ModelMapConvert     bool

	ReasoningEffortMap            map[string]string
	ReasoningEffortMapTransparent bool
	ReasoningEffortMapConvert     bool

	// When true, forward the client's Authorization header to upstream.
	// When false, always replace it with the configured upstream key.
	// Effective passthrough = mode flag AND upstream-API flag (both must be true).
	APIKeyPassthroughTransparent    bool
	APIKeyPassthroughConvert        bool
	APIKeyPassthroughResponsesAPI   bool
	APIKeyPassthroughCompletionsAPI bool
}

var cfg Config

func loadConfig() {
	// Command line flags (override env vars)
	flag.StringVar(&cfg.ResponsesAPIBaseURL, "responses-url", envOrDefault("RESPONSES_API_BASE_URL", "https://api.openai.com"), "Upstream Responses API base URL")
	flag.StringVar(&cfg.ResponsesAPIKey, "responses-key", envOrDefault("RESPONSES_API_KEY", ""), "Upstream Responses API key")
	flag.StringVar(&cfg.CompletionsAPIBaseURL, "completions-url", envOrDefault("COMPLETIONS_API_BASE_URL", "https://api.openai.com"), "Upstream Chat Completions API base URL")
	flag.StringVar(&cfg.CompletionsAPIKey, "completions-key", envOrDefault("COMPLETIONS_API_KEY", ""), "Upstream Chat Completions API key")
	flag.StringVar(&cfg.Host, "host", envOrDefault("HOST", "0.0.0.0"), "Server host")
	flag.IntVar(&cfg.Port, "port", envIntOrDefault("PORT", 9090), "Server port")
	flag.StringVar(&cfg.CacheDir, "cache-dir", envOrDefault("CACHE_DIR", "cache"), "Directory for caching reasoning results")
	flag.BoolVar(&cfg.LogEnabled, "log-enabled", envOrDefault("LOG_ENABLED", "false") == "true", "Enable request/response logging to files (false = console summary only)")
	flag.StringVar(&cfg.ConvertLogDir, "convert-log-dir", envOrDefault("CONVERT_LOG_DIR", "conversations"), "Directory for conversion-mode request/response logs")
	flag.StringVar(&cfg.TransparentLogDir, "transparent-log-dir", envOrDefault("TRANSPARENT_LOG_DIR", "conversations_t"), "Directory for transparent-mode request/response logs")
	flag.BoolVar(&cfg.TransparentEnabled, "transparent", envOrDefault("TRANSPARENT_ENABLED", "false") == "true", "Enable transparent pass-through mode (no conversion)")
	flag.BoolVar(&cfg.ModelMapTransparent, "model-map-transparent", envOrDefault("MODEL_MAP_TRANSPARENT_ENABLED", "true") == "true", "Enable MODEL_MAP in transparent pass-through mode")
	flag.BoolVar(&cfg.ModelMapConvert, "model-map-convert", envOrDefault("MODEL_MAP_CONVERT_ENABLED", "true") == "true", "Enable MODEL_MAP in conversion mode")
	flag.BoolVar(&cfg.ReasoningEffortMapTransparent, "reasoning-effort-map-transparent", envOrDefault("REASONING_EFFORT_MAP_TRANSPARENT_ENABLED", "true") == "true", "Enable REASONING_EFFORT_MAP in transparent pass-through mode")
	flag.BoolVar(&cfg.ReasoningEffortMapConvert, "reasoning-effort-map-convert", envOrDefault("REASONING_EFFORT_MAP_CONVERT_ENABLED", "true") == "true", "Enable REASONING_EFFORT_MAP in conversion mode")
	flag.BoolVar(&cfg.APIKeyPassthroughTransparent, "api-key-passthrough-transparent", envOrDefault("API_KEY_PASSTHROUGH_TRANSPARENT_ENABLED", "true") == "true", "Pass client Authorization through to upstream in transparent mode (false = always use configured key)")
	flag.BoolVar(&cfg.APIKeyPassthroughConvert, "api-key-passthrough-convert", envOrDefault("API_KEY_PASSTHROUGH_CONVERT_ENABLED", "true") == "true", "Pass client Authorization through to upstream in conversion mode (false = always use configured key)")
	flag.BoolVar(&cfg.APIKeyPassthroughResponsesAPI, "api-key-passthrough-responses", envOrDefault("API_KEY_PASSTHROUGH_RESPONSES_ENABLED", "true") == "true", "Pass client Authorization through to upstream Responses API (false = always use configured key)")
	flag.BoolVar(&cfg.APIKeyPassthroughCompletionsAPI, "api-key-passthrough-completions", envOrDefault("API_KEY_PASSTHROUGH_COMPLETIONS_ENABLED", "true") == "true", "Pass client Authorization through to upstream Chat Completions API (false = always use configured key)")
	flag.Parse()

	// Parse model mapping from env (supports multi-line JSON)
	cfg.ModelMap = make(map[string]string)
	if mm := envOrDefault("MODEL_MAP", ""); mm != "" {
		if err := json.Unmarshal([]byte(mm), &cfg.ModelMap); err != nil {
			log.Printf("warning: MODEL_MAP parse error: %v", err)
		}
	}

	// Parse reasoning_effort mapping from env (supports multi-line JSON)
	cfg.ReasoningEffortMap = make(map[string]string)
	if rm := envOrDefault("REASONING_EFFORT_MAP", ""); rm != "" {
		if err := json.Unmarshal([]byte(rm), &cfg.ReasoningEffortMap); err != nil {
			log.Printf("warning: REASONING_EFFORT_MAP parse error: %v", err)
		}
	}
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
	if cfg.LogEnabled {
		log.Printf("  Request logging: ENABLED")
		log.Printf("  Convert log dir: %s", cfg.ConvertLogDir)
		log.Printf("  Transparent log dir: %s", cfg.TransparentLogDir)
	} else {
		log.Println("  Request logging: DISABLED (set LOG_ENABLED=true to enable)")
	}
	if cfg.TransparentEnabled {
		log.Printf("  Transparent mode: ENABLED")
	} else {
		log.Println("  Transparent mode: DISABLED (set TRANSPARENT_ENABLED=true to enable)")
	}
	if len(cfg.ModelMap) > 0 {
		log.Printf("  Model map: %d entries (transparent=%v convert=%v)", len(cfg.ModelMap), cfg.ModelMapTransparent, cfg.ModelMapConvert)
	} else {
		log.Println("  Model map: DISABLED (set MODEL_MAP={...} to enable)")
	}
	if len(cfg.ReasoningEffortMap) > 0 {
		log.Printf("  Reasoning effort map: %d entries (transparent=%v convert=%v)", len(cfg.ReasoningEffortMap), cfg.ReasoningEffortMapTransparent, cfg.ReasoningEffortMapConvert)
	} else {
		log.Println("  Reasoning effort map: DISABLED (set REASONING_EFFORT_MAP={...} to enable)")
	}
	log.Printf("  API key passthrough: transparent=%v convert=%v responses=%v completions=%v (effective = mode AND api)", cfg.APIKeyPassthroughTransparent, cfg.APIKeyPassthroughConvert, cfg.APIKeyPassthroughResponsesAPI, cfg.APIKeyPassthroughCompletionsAPI)
	log.Println("")
	log.Println("  /v1/chat/completions → upstream Responses API")
	log.Println("  /v1/responses        → upstream Chat Completions API")
	log.Println("========================================")

	if err := http.ListenAndServe(addr, corsMiddleware(conversationLoggingMiddleware(mux))); err != nil {
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

// Load .env file (simple implementation, no external deps).
// Supports multi-line JSON object/array values when value starts with `{` or `[`.
func loadDotEnv(filename string) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return
	}
	src := string(data)
	i := 0
	for i < len(src) {
		// skip whitespace, blank lines, comment lines
		for i < len(src) && (src[i] == ' ' || src[i] == '\t' || src[i] == '\r' || src[i] == '\n') {
			i++
		}
		if i >= len(src) {
			break
		}
		if src[i] == '#' {
			for i < len(src) && src[i] != '\n' {
				i++
			}
			continue
		}
		// parse KEY up to '=' or newline
		keyStart := i
		for i < len(src) && src[i] != '=' && src[i] != '\n' {
			i++
		}
		if i >= len(src) || src[i] != '=' {
			// no '=' on this line — skip
			for i < len(src) && src[i] != '\n' {
				i++
			}
			continue
		}
		key := strings.TrimSpace(src[keyStart:i])
		i++ // consume '='
		// skip leading spaces/tabs on value (but NOT newlines)
		for i < len(src) && (src[i] == ' ' || src[i] == '\t') {
			i++
		}
		valStart := i
		var val string
		if i < len(src) && (src[i] == '{' || src[i] == '[') {
			// Multi-line JSON: scan until matching outer brace/bracket closes.
			open := src[i]
			closeCh := byte('}')
			if open == '[' {
				closeCh = ']'
			}
			depth := 0
			inStr := false
			esc := false
			for i < len(src) {
				c := src[i]
				if inStr {
					switch {
					case esc:
						esc = false
					case c == '\\':
						esc = true
					case c == '"':
						inStr = false
					}
				} else {
					switch c {
					case '"':
						inStr = true
					case open:
						depth++
					case closeCh:
						depth--
						if depth == 0 {
							i++
							goto done
						}
					}
				}
				i++
			}
		done:
			val = strings.TrimSpace(src[valStart:i])
			// consume rest of line (trailing comments/whitespace)
			for i < len(src) && src[i] != '\n' {
				i++
			}
		} else {
			// single-line value
			for i < len(src) && src[i] != '\n' {
				i++
			}
			val = strings.TrimSpace(src[valStart:i])
			// strip surrounding quotes
			if len(val) >= 2 {
				if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
					val = val[1 : len(val)-1]
				}
			}
		}
		if key != "" && os.Getenv(key) == "" {
			os.Setenv(key, val)
		}
	}
}
