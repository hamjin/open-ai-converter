package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// ==================== Request-scoped logger ====================
//
// Non-invasive logger: middleware attaches a *RequestLogger to the request
// context, captures the raw request body, tees the response body, and writes
// everything to a per-request file. Handlers may call helpers (LoggerFromReq,
// Filef, SetModelMapping, SetEffortMapping) to enrich the record without
// changing handler signatures.

type reqLoggerKey struct{}

// RequestLogger accumulates everything that should be persisted for a single
// HTTP request/response pair. Only a tiny summary is mirrored to stdout — the
// rest lives in the per-request log file.
type RequestLogger struct {
	SessionID string
	Path      string
	Method    string
	Direction string // "convert" or "transparent"

	file    *os.File
	mu      sync.Mutex
	started time.Time

	streamSet  bool
	stream     bool
	modelPre   string
	modelPost  string
	effortPre  string
	effortPost string
	reqBytes   int
	respBytes  int
	status     int
	closedOnce sync.Once
}

var reqCounter uint64

func newSessionID() string {
	n := atomic.AddUint64(&reqCounter, 1)
	return fmt.Sprintf("req_%d_%d", time.Now().UnixNano(), n)
}

// LoggerFromCtx fetches the RequestLogger from a context (may be nil).
func LoggerFromCtx(ctx context.Context) *RequestLogger {
	if ctx == nil {
		return nil
	}
	if v, ok := ctx.Value(reqLoggerKey{}).(*RequestLogger); ok {
		return v
	}
	return nil
}

// LoggerFromReq fetches the RequestLogger from an *http.Request.
func LoggerFromReq(r *http.Request) *RequestLogger {
	if r == nil {
		return nil
	}
	return LoggerFromCtx(r.Context())
}

// Filef writes a timestamped diagnostic line to the request log file only.
func (rl *RequestLogger) Filef(format string, args ...interface{}) {
	if rl == nil || rl.file == nil {
		return
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	line := fmt.Sprintf(format, args...)
	fmt.Fprintf(rl.file, "[%s] %s\n", time.Now().Format("15:04:05.000"), line)
}

// WriteSection writes a delimited section followed by a raw payload.
// SetStream records the streaming flag for the console summary.
func (rl *RequestLogger) SetStream(s bool) {
	if rl == nil {
		return
	}
	rl.streamSet = true
	rl.stream = s
}

// SetModelMapping records the pre/post mapping model names.
func (rl *RequestLogger) SetModelMapping(pre, post string) {
	if rl == nil {
		return
	}
	if rl.modelPre == "" {
		rl.modelPre = pre
	}
	rl.modelPost = post
}

// SetEffortMapping records the pre/post mapping reasoning effort values.
func (rl *RequestLogger) SetEffortMapping(pre, post string) {
	if rl == nil {
		return
	}
	if rl.effortPre == "" {
		rl.effortPre = pre
	}
	rl.effortPost = post
}

// WriteSection writes a delimited section followed by a raw payload. Direction
// argument controls the marker prefix: true → ">>>" (client/upstream-bound),
// false → "<<<" (response/client-bound).
func (rl *RequestLogger) WriteSection(title string, body []byte) {
	rl.writeSectionDir(title, body, true)
}

// WriteResponseSection mirrors WriteSection but uses the "<<<" prefix.
func (rl *RequestLogger) WriteResponseSection(title string, body []byte) {
	rl.writeSectionDir(title, body, false)
}

func (rl *RequestLogger) writeSectionDir(title string, body []byte, outbound bool) {
	if rl == nil || rl.file == nil {
		return
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	prefix := ">>>"
	if !outbound {
		prefix = "<<<"
	}
	fmt.Fprintf(rl.file, "\n────────────────────────────────────────\n")
	fmt.Fprintf(rl.file, "%s %s\n", prefix, title)
	fmt.Fprintf(rl.file, "────────────────────────────────────────\n")
	if len(body) > 0 {
		rl.file.Write(body)
		if body[len(body)-1] != '\n' {
			rl.file.Write([]byte{'\n'})
		}
	}
}

func prettyJSON(body []byte) []byte {
	if !isLikelyJSON(body) {
		return body
	}
	var v interface{}
	if json.Unmarshal(body, &v) != nil {
		return body
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return body
	}
	return out
}

// WriteConvertedRequest records the JSON body that will be sent upstream.
func (rl *RequestLogger) WriteConvertedRequest(body []byte) {
	if rl == nil {
		return
	}
	rl.WriteSection("CONVERTED REQUEST (sent to upstream)", prettyJSON(body))
}

// WriteUpstreamResponse records the full upstream response body for non-SSE replies.
func (rl *RequestLogger) WriteUpstreamResponse(status int, body []byte) {
	if rl == nil {
		return
	}
	rl.WriteResponseSection(fmt.Sprintf("UPSTREAM RESPONSE (status %d)", status), prettyJSON(body))
}

// WriteFinalResponse records the converted response body the proxy sends back.
func (rl *RequestLogger) WriteFinalResponse(body []byte) {
	if rl == nil {
		return
	}
	rl.WriteResponseSection("FINAL RESPONSE (converted, sent to client)", prettyJSON(body))
}

// WriteResponseHeaders records the Response Headers section.
func (rl *RequestLogger) WriteResponseHeaders(h http.Header, status int) {
	if rl == nil || rl.file == nil {
		return
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	fmt.Fprintf(rl.file, "\n────────────────────────────────────────\n")
	fmt.Fprintf(rl.file, "<<< Response Headers (status %d)\n", status)
	fmt.Fprintf(rl.file, "────────────────────────────────────────\n")
	for k, vs := range h {
		for _, v := range vs {
			fmt.Fprintf(rl.file, "  %s: %s\n", k, v)
		}
	}
}

// NewUpstreamBodyTeeCloser wraps an upstream ReadCloser so that every byte
// streamed back is mirrored to the request log under the right section.
// Streaming requests use "RAW UPSTREAM SSE"; non-streaming use
// "UPSTREAM RESPONSE (status N)".
func (rl *RequestLogger) NewUpstreamBodyTeeCloser(rc io.ReadCloser, status int, streaming bool) io.ReadCloser {
	if rl == nil || rl.file == nil {
		return rc
	}
	title := fmt.Sprintf("UPSTREAM RESPONSE (status %d)", status)
	if streaming {
		title = "RAW UPSTREAM SSE"
	}
	return &teeReadCloser{
		ReadCloser: rc,
		sec:        newResponseSectionWriter(rl, title),
		rl:         rl,
		status:     status,
		streaming:  streaming,
	}
}

type teeReadCloser struct {
	io.ReadCloser
	sec       *sectionWriter
	rl        *RequestLogger
	status    int
	streaming bool
}

func (t *teeReadCloser) Read(p []byte) (int, error) {
	n, err := t.ReadCloser.Read(p)
	if n > 0 && t.sec != nil {
		t.sec.Write(p[:n])
	}
	return n, err
}

func (t *teeReadCloser) Close() error {
	if t.streaming && t.rl != nil {
		t.rl.WriteUpstreamResponse(t.status, nil)
	}
	return t.ReadCloser.Close()
}

func (rl *RequestLogger) finish() {
	if rl == nil {
		return
	}
	rl.closedOnce.Do(func() {
		if rl.file != nil {
			rl.mu.Lock()
			fmt.Fprintf(rl.file, "\n=== End: %s status=%d elapsed=%.3fs reqBytes=%d respBytes=%d ===\n",
				rl.SessionID, rl.status, time.Since(rl.started).Seconds(), rl.reqBytes, rl.respBytes)
			rl.mu.Unlock()
			rl.file.Close()
		}
		rl.printConsoleSummary()
	})
}

func emptyDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func (rl *RequestLogger) printConsoleSummary() {
	post := rl.modelPost
	if post == "" {
		post = rl.modelPre
	}
	ePost := rl.effortPost
	if ePost == "" {
		ePost = rl.effortPre
	}

	var parts []string
	parts = append(parts, fmt.Sprintf("%s %s", rl.Method, rl.Path))
	if rl.modelPre != "" || post != "" {
		parts = append(parts, fmt.Sprintf("model=%s→%s", emptyDash(rl.modelPre), emptyDash(post)))
	}
	if rl.effortPre != "" || ePost != "" {
		parts = append(parts, fmt.Sprintf("effort=%s→%s", emptyDash(rl.effortPre), emptyDash(ePost)))
	}
	if rl.streamSet {
		parts = append(parts, fmt.Sprintf("stream=%v", rl.stream))
	}
	joined := ""
	for i, p := range parts {
		if i > 0 {
			joined += " "
		}
		joined += p
	}
	log.Printf("[%s] %s", rl.SessionID, joined)
}

// ==================== Tee response writer ====================

type teeResponseWriter struct {
	http.ResponseWriter
	rl    *RequestLogger
	bodyW *sectionWriter
}

func (t *teeResponseWriter) WriteHeader(code int) {
	if t.rl != nil {
		t.rl.status = code
		t.rl.Filef("response status: %d", code)
		t.rl.WriteResponseHeaders(t.ResponseWriter.Header(), code)
	}
	t.ResponseWriter.WriteHeader(code)
}

func (t *teeResponseWriter) Write(b []byte) (int, error) {
	n, err := t.ResponseWriter.Write(b)
	if n > 0 && t.bodyW != nil {
		t.bodyW.Write(b[:n])
		if t.rl != nil {
			t.rl.respBytes += n
		}
	}
	return n, err
}

func (t *teeResponseWriter) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (t *teeResponseWriter) Header() http.Header { return t.ResponseWriter.Header() }

// sectionWriter is an io.Writer that lazily emits a section header on first
// write and streams subsequent bytes directly to the log file.
// When deferred=true, writes are buffered in memory until FlushDeferred is
// called. This prevents interleaving when multiple sectionWriters write to the
// same file concurrently (upstream body vs downstream body in streaming mode).
type sectionWriter struct {
	rl            *RequestLogger
	title         string
	outbound      bool
	flushedHeader bool
	deferred      bool
	buf           bytes.Buffer
}

func newSectionWriter(rl *RequestLogger, title string) *sectionWriter {
	return &sectionWriter{rl: rl, title: title, outbound: true}
}

func newResponseSectionWriter(rl *RequestLogger, title string) *sectionWriter {
	return &sectionWriter{rl: rl, title: title, outbound: false}
}

func (s *sectionWriter) Write(b []byte) (int, error) {
	if s.rl == nil || s.rl.file == nil {
		return len(b), nil
	}
	if s.deferred {
		s.buf.Write(b)
		return len(b), nil
	}
	s.rl.mu.Lock()
	defer s.rl.mu.Unlock()
	if !s.flushedHeader {
		s.flushedHeader = true
		prefix := ">>>"
		if !s.outbound {
			prefix = "<<<"
		}
		fmt.Fprintf(s.rl.file, "\n────────────────────────────────────────\n")
		fmt.Fprintf(s.rl.file, "%s %s\n", prefix, s.title)
		fmt.Fprintf(s.rl.file, "────────────────────────────────────────\n")
	}
	s.rl.file.Write(b)
	return len(b), nil
}

// FlushDeferred writes the buffered content to the log file. Must be called
// after the handler returns, before the file is closed.
func (s *sectionWriter) FlushDeferred() {
	if s.rl == nil || s.rl.file == nil || !s.deferred {
		return
	}
	s.rl.mu.Lock()
	defer s.rl.mu.Unlock()
	if !s.flushedHeader {
		s.flushedHeader = true
		prefix := ">>>"
		if !s.outbound {
			prefix = "<<<"
		}
		fmt.Fprintf(s.rl.file, "\n────────────────────────────────────────\n")
		fmt.Fprintf(s.rl.file, "%s %s\n", prefix, s.title)
		fmt.Fprintf(s.rl.file, "────────────────────────────────────────\n")
	}
	if s.buf.Len() > 0 {
		s.rl.file.Write(s.buf.Bytes())
		s.buf.Reset()
	}
}

// ==================== Helpers ====================

func isLikelyJSON(b []byte) bool {
	for _, c := range b {
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			continue
		}
		return c == '{' || c == '['
	}
	return false
}

func maskAuth(v string) string {
	if len(v) <= 12 {
		return v
	}
	return v[:12] + "...(masked)"
}

func (rl *RequestLogger) detectFromBody(body []byte) {
	if !isLikelyJSON(body) {
		return
	}
	var p struct {
		Model           string `json:"model"`
		Stream          bool   `json:"stream"`
		ReasoningEffort string `json:"reasoning_effort"`
		Reasoning       struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return
	}
	if p.Model != "" {
		rl.modelPre = p.Model
	}
	rl.streamSet = true
	rl.stream = p.Stream
	if p.ReasoningEffort != "" {
		rl.effortPre = p.ReasoningEffort
	} else if p.Reasoning.Effort != "" {
		rl.effortPre = p.Reasoning.Effort
	}
}

func (rl *RequestLogger) writeHeader(r *http.Request, body []byte) {
	if rl == nil || rl.file == nil {
		return
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	fmt.Fprintf(rl.file, "=== Request Log: %s ===\n", rl.SessionID)
	fmt.Fprintf(rl.file, "Time:       %s\n", rl.started.Format(time.RFC3339Nano))
	fmt.Fprintf(rl.file, "Direction:  %s\n", rl.Direction)
	fmt.Fprintf(rl.file, "Method:     %s\n", r.Method)
	fmt.Fprintf(rl.file, "Path:       %s\n", r.URL.Path)
	if r.URL.RawQuery != "" {
		fmt.Fprintf(rl.file, "Query:      %s\n", r.URL.RawQuery)
	}
	fmt.Fprintf(rl.file, "RemoteAddr: %s\n", r.RemoteAddr)
	fmt.Fprintf(rl.file, "Stream:     %v\n", rl.stream)
	if rl.modelPre != "" {
		fmt.Fprintf(rl.file, "Model:      %s\n", rl.modelPre)
	}
	if rl.effortPre != "" {
		fmt.Fprintf(rl.file, "Effort:     %s\n", rl.effortPre)
	}

	fmt.Fprintf(rl.file, "\n────────────────────────────────────────\n")
	fmt.Fprintf(rl.file, ">>> Request Headers\n")
	fmt.Fprintf(rl.file, "────────────────────────────────────────\n")
	for k, vs := range r.Header {
		for _, v := range vs {
			if k == "Authorization" {
				v = maskAuth(v)
			}
			fmt.Fprintf(rl.file, "  %s: %s\n", k, v)
		}
	}

	fmt.Fprintf(rl.file, "\n────────────────────────────────────────\n")
	fmt.Fprintf(rl.file, ">>> RAW REQUEST (from client) (%d bytes)\n", len(body))
	fmt.Fprintf(rl.file, "────────────────────────────────────────\n")
	if isLikelyJSON(body) {
		var v interface{}
		if json.Unmarshal(body, &v) == nil {
			pretty, _ := json.MarshalIndent(v, "", "  ")
			rl.file.Write(pretty)
			rl.file.Write([]byte{'\n'})
		} else {
			rl.file.Write(body)
			rl.file.Write([]byte{'\n'})
		}
	} else if len(body) > 0 {
		rl.file.Write(body)
		if body[len(body)-1] != '\n' {
			rl.file.Write([]byte{'\n'})
		}
	}
}

// ==================== Middleware ====================

// conversationLoggingMiddleware wraps the mux so that every meaningful request
// is recorded to a per-session file under cfg.ConvertLogDir or
// cfg.TransparentLogDir, while the console gets only the summary line.
func conversationLoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" || r.URL.Path == "/" {
			next.ServeHTTP(w, r)
			return
		}

		if !cfg.LogEnabled {
			log.Printf("[%s] %s %s", r.Method, r.URL.Path, r.RemoteAddr)
			next.ServeHTTP(w, r)
			return
		}

		sessionID := newSessionID()

		dir := cfg.ConvertLogDir
		direction := "convert"
		if transparentEnabledForPath(r.URL.Path) {
			dir = cfg.TransparentLogDir
			direction = "transparent"
		}
		if dir == "" {
			dir = "conversations"
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Printf("[%s] mkdir %s failed: %v", sessionID, dir, err)
		}
		fpath := filepath.Join(dir, sessionID+".log")
		f, err := os.Create(fpath)
		if err != nil {
			log.Printf("[%s] create log file failed: %v", sessionID, err)
		}

		rl := &RequestLogger{
			SessionID: sessionID,
			Path:      r.URL.Path,
			Method:    r.Method,
			Direction: direction,
			file:      f,
			started:   time.Now(),
		}

		// Read body, restore so handler can re-read.
		var body []byte
		if r.Body != nil {
			body, _ = io.ReadAll(r.Body)
			r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		rl.reqBytes = len(body)
		rl.detectFromBody(body)
		rl.writeHeader(r, body)

		downstreamTitle := "FINAL RESPONSE (converted, sent to client)"
		bodyW := newResponseSectionWriter(rl, downstreamTitle)
		if rl.stream {
			// Buffer downstream body in memory during the handler so that
			// upstream SSE and UPSTREAM RESPONSE sections are flushed to the
			// log file first. Without this, the two concurrent sectionWriters
			// (upstream tee vs downstream tee) interleave in the output.
			bodyW.deferred = true
		}
		tw := &teeResponseWriter{
			ResponseWriter: w,
			rl:             rl,
			bodyW:          bodyW,
		}

		ctx := context.WithValue(r.Context(), reqLoggerKey{}, rl)
		r = r.WithContext(ctx)

		defer func() {
			bodyW.FlushDeferred()
			rl.finish()
		}()
		next.ServeHTTP(tw, r)
	})
}
