package tanuki

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailers":            true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

const (
	maxBodySize = 32 << 20 // 32 MB request body, the Messages API's own limit
	maxRespSize = 64 << 20 // 64 MB buffered response / SSE event
)

var (
	nnBoundary   = []byte("\n\n")
	rnrnBoundary = []byte("\r\n\r\n")
)

func runProxy() {
	port := proxyPort()
	upstream := envOr("TANUKI_UPSTREAM", "https://api.anthropic.com")

	eng, err := ensureEngagement()
	if err != nil {
		fatal(err.Error())
	}

	engDir := engagementDir(eng)

	if err := loadEnvConfig(engDir); err != nil {
		fatal(err.Error())
	}

	// Bind before writing any Claude Code config. configureHooks points this
	// directory at the proxy, and a start that fails after that -- a port
	// already in use, a second "compose up" -- left the redirect behind with
	// nothing ever listening, which is the ECONNREFUSED state. fatal() exits
	// past the cleanup at the end, so the only safe order is to fail first.
	var lc net.ListenConfig

	listener, err := lc.Listen(context.Background(), "tcp", "0.0.0.0:"+port)
	if err != nil {
		fatal("proxy listen failed: " + err.Error())
	}

	if err := configureHooks(); err != nil {
		fatal(err.Error())
	}

	if err := generateClaudeMD(engDir); err != nil {
		restoreClaudeCodeConfig()
		fatal(err.Error())
	}

	mappings := loadMappings(engDir)
	logger.Info("proxy starting",
		"version", versionString(),
		"engagement", eng,
		"mappings", len(mappings),
	)

	logger.Info("proxy listening",
		"upstream", upstream,
		"addr", "0.0.0.0:"+port,
	)

	server := &http.Server{
		Addr:              "0.0.0.0:" + port,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		// No write deadline: a long streaming /v1/messages response can run
		// past any fixed timeout. Client disconnect is covered by r.Context().
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
		Handler:      &proxyHandler{upstream: upstream},
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)

	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutting down proxy")
	case err := <-errCh:
		restoreClaudeCodeConfig()
		fatal("proxy failed: " + err.Error())
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown error", "error", err)
	}

	restoreClaudeCodeConfig()
}

// restoreClaudeCodeConfig undoes what configureHooks and generateClaudeMD
// wrote. Left behind, ANTHROPIC_BASE_URL sends the next session in this
// directory to a port nothing is listening on (ECONNREFUSED) and every tool
// call tries to docker exec a container that is gone, while the cover story
// tells that session the project is called DEVTARGET. A restart re-applies
// both, so they are in effect only while the proxy is.
func restoreClaudeCodeConfig() {
	settingsPath := filepath.Join(".claude", "settings.json")
	if fileExists(settingsPath) {
		removeTanukiSettings(settingsPath)
	}

	claudeMDPath := filepath.Join(".claude", "CLAUDE.md")
	if fileExists(claudeMDPath) {
		removeTanukiClaudeMD(claudeMDPath)
	}
}

var httpClient = &http.Client{
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

type proxyHandler struct {
	upstream string
}

// writeAPIError answers in the Anthropic error format, so Claude Code shows
// the message. The status decides whether the client retries: a 4xx is
// final, a 5xx is retried with backoff. A refusal is deterministic, so it is a
// 4xx; retrying it would only make the operator wait to see it.
func writeAPIError(w http.ResponseWriter, status int, msg string) {
	errType := "api_error"

	switch status {
	case http.StatusBadRequest:
		errType = "invalid_request_error"
	case http.StatusRequestEntityTooLarge:
		errType = "request_too_large"
	}

	type apiErrorDetail struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}

	body, _ := json.Marshal(struct {
		Type  string         `json:"type"`
		Error apiErrorDetail `json:"error"`
	}{"error", apiErrorDetail{errType, "tanuki: " + msg}})

	w.Header().Set("Content-Type", "application/json")
	w.Header().Del("Content-Length")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (h *proxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
			writeAPIError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("request body exceeds %d MB", maxBodySize>>20))
			return
		}

		writeAPIError(w, http.StatusBadRequest, "failed to read request body")

		return
	}

	_ = r.Body.Close()

	isMessagesPath := isMessagesRequest(r.URL.Path)

	if isMessagesPath {
		if err := checkRequestEnvelope(r); err != nil {
			logger.Warn("refusing to forward request", "path", r.URL.Path, "reason", err.Error())
			writeAPIError(w, http.StatusBadRequest, "refused to forward request: "+err.Error())

			return
		}
	}

	if isMessagesPath && len(body) > 0 {
		anonymized, err := anonymizeUpstreamBody(body)
		if err != nil {
			// Fail closed: never forward a prompt body we could not anonymize.
			logger.Warn("refusing to forward request unanonymized", "path", r.URL.Path, "reason", err.Error())
			writeAPIError(w, http.StatusBadRequest, "refused to forward request: "+err.Error())

			return
		}

		body = anonymized
	}

	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, h.upstream+r.URL.RequestURI(), bytes.NewReader(body))
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, "failed to create upstream request")
		return
	}

	copyProxyHeaders(proxyReq.Header, r.Header, false)

	if isMessagesPath {
		proxyReq.Header.Del("Accept-Encoding")
	}

	proxyReq.ContentLength = int64(len(body))

	resp, err := httpClient.Do(proxyReq)
	if err != nil {
		// A 5xx, so the client retries: a dropped connection is transient.
		writeAPIError(w, http.StatusBadGateway, "upstream error: "+err.Error())
		return
	}

	defer func() { _ = resp.Body.Close() }()

	isSSE := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")

	logger.Info("proxied request",
		"method", r.Method,
		"path", r.URL.Path,
		"status", resp.StatusCode,
		"anonymized", isMessagesPath,
		"sse", isSSE,
	)

	copyProxyHeaders(w.Header(), resp.Header, isMessagesPath)

	switch {
	case !isMessagesPath:
		w.WriteHeader(resp.StatusCode)
		forwardRaw(w, resp.Body)
	case isSSE:
		w.WriteHeader(resp.StatusCode)
		streamSSEWithRewrite(w, resp.Body, responseRewriter(), jsonResponseRewriter())
	default:
		forwardWithRewrite(w, resp, responseRewriter())
	}
}

// checkRequestEnvelope refuses a prompt-bearing request whose envelope the
// rewriter never reaches: a compressed body, which it cannot read and would
// forward whole, and a real target in the query string or a header, neither of
// which is part of the body the gate scans.
//
// Refusing rather than rewriting is deliberate. Claude Code compresses no
// request and puts no target in a header, so either means something else is
// driving the proxy and the body's guarantees do not hold.
func checkRequestEnvelope(r *http.Request) error {
	if enc := r.Header.Get("Content-Encoding"); enc != "" && !strings.EqualFold(enc, "identity") {
		return fmt.Errorf("request body is %s-encoded, which cannot be anonymized", enc)
	}

	if found := residualTargets(nil, r.URL.RawQuery); len(found) > 0 {
		logger.Warn("real values in the request query", "count", len(found), "values", found)

		return fmt.Errorf("%d real value(s) in the request query; see the tanuki log", len(found))
	}

	for name, values := range r.Header {
		if hopByHopHeaders[http.CanonicalHeaderKey(name)] {
			continue
		}

		if found := residualTargets(nil, values...); len(found) > 0 {
			logger.Warn("real values in a request header",
				"header", name, "count", len(found), "values", found)

			return fmt.Errorf("%d real value(s) in the %s header; see the tanuki log", len(found), name)
		}
	}

	return nil
}

// isMessagesRequest reports whether path addresses the Messages API, the
// traffic carrying prompts. The path is cleaned and compared without case:
// "//v1/messages", "/v1/../v1/messages" and "/V1/messages" all reach the same
// endpoint upstream, and each one used to skip anonymization.
func isMessagesRequest(p string) bool {
	clean := path.Clean("/" + strings.TrimLeft(p, "/"))

	return strings.HasPrefix(strings.ToLower(clean), "/v1/messages")
}

// copyProxyHeaders copies non-hop-by-hop headers from src to dst, dropping
// Content-Length when the body will be rewritten (its length changes).
func copyProxyHeaders(dst, src http.Header, dropContentLength bool) {
	for key, vals := range src {
		canonical := http.CanonicalHeaderKey(key)
		if hopByHopHeaders[canonical] {
			continue
		}

		if dropContentLength && canonical == "Content-Length" {
			continue
		}

		for _, val := range vals {
			dst.Add(key, val)
		}
	}
}

// sseSink is the destination for rewritten SSE events. A nil flusher means
// the underlying ResponseWriter does not support flushing.
//
// Text and tool-input deltas are reversed across events, not one at a time:
// the model streams a few characters per delta, so "localhost:9000" can arrive
// as "local", "host:90", "00". The tail of a block that could be the start of a
// fiction value is held back until the next delta shows whether it is one, and
// released when the block or the stream ends.
type sseSink struct {
	w       io.Writer
	flusher http.Flusher
	rw      *rewriter
	// rwJSON reverses into a JSON fragment, escaping each real value as it is
	// inserted: input_json_delta carries a slice of the tool's argument JSON,
	// where an unescaped quote or backslash ends the string early.
	rwJSON *rewriter
	held   map[string]*heldDelta // by content block index
}

// heldDelta is the unsent tail of one content block.
type heldDelta struct {
	index any    // the block index, sent back as it arrived
	kind  string // the delta type: a key of deltaFields
	text  string
}

// deltaFields maps each delta type that carries reversible text to its field.
var deltaFields = map[string]string{
	"text_delta":       "text",
	"input_json_delta": "partial_json",
}

func newSSESink(w http.ResponseWriter, rw, rwJSON *rewriter) *sseSink {
	flusher, _ := w.(http.Flusher)

	return &sseSink{w: w, flusher: flusher, rw: rw, rwJSON: rwJSON, held: map[string]*heldDelta{}}
}

// write handles one complete event.
func (s *sseSink) write(event string) {
	if doc := s.eventData(event); doc != nil {
		switch doc[typeKey] {
		case "content_block_start":
			if s.holdStart(doc) {
				return
			}
		case "content_block_delta":
			if s.holdDelta(doc) {
				return
			}
		case "content_block_stop":
			s.release(fmt.Sprint(doc["index"]))
		}
	}

	s.send(reverseSSEEvent(event, s.rw))
}

// eventData decodes the JSON payload of an event's "data:" line, or returns
// nil when there is nothing to reverse or no JSON object to read.
func (s *sseSink) eventData(event string) map[string]any {
	if s.rw == nil {
		return nil
	}

	for line := range strings.Lines(event) {
		payload, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "data:")
		if !ok {
			continue
		}

		doc, _ := decodeJSON([]byte(strings.TrimPrefix(payload, " ")))
		obj, _ := doc.(map[string]any)

		return obj
	}

	return nil
}

// holdStart does for a block's prefilled text what holdDelta does for the
// deltas after it. A block can open with text already in it, and a fiction
// value straddling that text and the first delta was never reassembled: the
// operator read "localhost:9000" where the target should have been.
func holdStartField(block map[string]any) (string, bool) {
	text, ok := block["text"].(string)

	return text, ok && text != ""
}

func (s *sseSink) holdStart(doc map[string]any) bool {
	block, _ := doc["content_block"].(map[string]any)
	if block == nil {
		return false
	}

	text, ok := holdStartField(block)
	if !ok {
		return false
	}

	send, hold := s.rw.splitHeld(text)
	block["text"] = s.rw.rewrite(send)

	key := fmt.Sprint(doc["index"])
	s.held[key] = &heldDelta{index: doc["index"], kind: "text_delta", text: hold}

	data, err := encodeJSON(doc)
	if err != nil {
		return false
	}

	s.send("event: content_block_start\ndata: " + string(data) + "\n\n")

	return true
}

// holdDelta sends the part of a text delta that is settled, keeps the tail
// that may still turn into a fiction value, and reports whether the event was
// a text delta at all.
func (s *sseSink) holdDelta(doc map[string]any) bool {
	delta, _ := doc["delta"].(map[string]any)
	kind, _ := delta[typeKey].(string)
	field, isText := deltaFields[kind]
	text, isString := delta[field].(string)

	if !isText || !isString {
		return false
	}

	key := fmt.Sprint(doc["index"])

	h := s.held[key]
	if h == nil {
		h = &heldDelta{index: doc["index"], kind: kind}
		s.held[key] = h
	}

	send, hold := s.rw.splitHeld(h.text + text)
	h.text = hold

	if send != "" {
		s.sendDelta(h, send)
	}

	return true
}

// release sends what is held for a block, before the event that ends it.
func (s *sseSink) release(key string) {
	h := s.held[key]
	delete(s.held, key)

	if h != nil && h.text != "" {
		s.sendDelta(h, h.text)
	}
}

// releaseAll sends whatever a stream that ended early left held.
func (s *sseSink) releaseAll() {
	for _, key := range slices.Sorted(maps.Keys(s.held)) {
		s.release(key)
	}
}

func (s *sseSink) sendDelta(h *heldDelta, text string) {
	rw := s.rw
	if h.kind == "input_json_delta" && s.rwJSON != nil {
		rw = s.rwJSON
	}

	data, err := encodeJSON(map[string]any{
		typeKey: "content_block_delta",
		"index": h.index,
		"delta": map[string]any{typeKey: h.kind, deltaFields[h.kind]: rw.rewrite(text)},
	})
	if err != nil {
		return
	}

	s.send("event: content_block_delta\ndata: " + string(data) + "\n\n")
}

// send writes an event downstream, flushing so the client sees each event as
// it arrives rather than at end of stream.
func (s *sseSink) send(event string) {
	_, _ = io.WriteString(s.w, event)

	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// streamSSEWithRewrite reverses fiction to real in a stream of SSE events.
func streamSSEWithRewrite(w http.ResponseWriter, body io.Reader, rw, rwJSON *rewriter) {
	sink := newSSESink(w, rw, rwJSON)

	var lineBuf bytes.Buffer

	buf := make([]byte, 4096)

	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			lineBuf.Write(buf[:n])
			flushCompleteEvents(&lineBuf, sink)

			// Bound memory if an event never terminates.
			if lineBuf.Len() > maxRespSize {
				flushRemainder(&lineBuf, sink)
				lineBuf.Reset()
			}
		}

		if readErr != nil {
			flushRemainder(&lineBuf, sink)
			sink.releaseAll()

			break
		}
	}
}

// flushRemainder writes whatever is left in buf after the stream ends, so a
// partial trailing event still gets rewritten rather than dropped.
func flushRemainder(buf *bytes.Buffer, sink *sseSink) {
	if buf.Len() == 0 {
		return
	}

	sink.write(buf.String())
}

func findEventBoundary(data []byte) (int, int) {
	nn := bytes.Index(data, nnBoundary)
	rn := bytes.Index(data, rnrnBoundary)

	switch {
	case nn >= 0 && (rn < 0 || nn <= rn):
		return nn, 2
	case rn >= 0:
		return rn, 4
	default:
		return -1, 0
	}
}

// flushCompleteEvents drains every whole SSE event currently in buf,
// leaving any trailing partial event for the next read.
func flushCompleteEvents(buf *bytes.Buffer, sink *sseSink) {
	for {
		data := buf.Bytes()

		idx, bLen := findEventBoundary(data)
		if idx < 0 {
			break
		}

		sink.write(string(data[:idx+bLen]))
		buf.Next(idx + bLen)
	}
}

// forwardWithRewrite reads the whole response before sending the status, so a
// failed or oversized read can still become an error the client sees and
// retries, instead of a 200 with an empty or truncated JSON body.
func forwardWithRewrite(w http.ResponseWriter, resp *http.Response, rw *rewriter) {
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxRespSize+1))
	if err != nil {
		logger.Error("reading upstream response failed", "error", err)
		writeAPIError(w, http.StatusBadGateway, "reading upstream response failed")

		return
	}

	if len(respBody) > maxRespSize {
		logger.Error("upstream response exceeded rewrite limit", "limit", maxRespSize)
		writeAPIError(w, http.StatusBadGateway, "upstream response exceeds rewrite limit")

		return
	}

	w.WriteHeader(resp.StatusCode)
	_, _ = io.WriteString(w, reverseBody(string(respBody), rw))
}

func forwardRaw(w http.ResponseWriter, body io.Reader) {
	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 4096)

	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				break
			}

			if canFlush {
				flusher.Flush()
			}
		}

		if readErr != nil {
			break
		}
	}
}

func responseRewriter() *rewriter {
	eng := currentEngagement()
	if eng == "" {
		return nil
	}

	return proxyCache.getRewriter(engagementDir(eng), directionF2R)
}

// jsonResponseRewriter reverses text that sits inside a JSON string.
func jsonResponseRewriter() *rewriter {
	eng := currentEngagement()
	if eng == "" {
		return nil
	}

	return proxyCache.jsonRewriter(engagementDir(eng))
}

// anonymizeUpstreamBody rewrites a body's real values to fiction before it
// leaves. It fails closed: no engagement, unreadable or empty mappings, and a
// real value still present after the rewrite all return an error rather than
// forward the body.
//
// A JSON body is rewritten string by string, never as raw text: see
// structuralKeys for what raw rewriting did to the request itself.
func anonymizeUpstreamBody(body []byte) ([]byte, error) {
	eng := currentEngagement()
	if eng == "" {
		return nil, errors.New("no active engagement (run: tanuki add <domain>)")
	}

	engDir := engagementDir(eng)

	doc, isJSON := decodeJSON(body)

	texts := []string{string(body)}

	// Ignores good for this request only: the operator's own email domain,
	// read from the context Claude Code injects. Computed before the rewrite
	// and reused by the residual gate so detection and the gate agree. Derived
	// from the decoded doc so it can skip target-controlled tool_result blocks;
	// a non-JSON body has no such structure and gets no per-request ignores.
	var extraIgnored []string

	if isJSON {
		texts = jsonStrings(doc)
		extraIgnored = operatorIgnoredDomains(doc)
	}

	if err := mapBodyTargets(engDir, texts, extraIgnored); err != nil {
		return nil, err
	}

	rw, err := proxyCache.rewriterOrError(engDir, directionR2F)
	if err != nil {
		return nil, fmt.Errorf("cannot read mappings: %w", err)
	}

	if rw == nil {
		return nil, errors.New("no target mappings configured (run: tanuki add <domain>)")
	}

	if !isJSON {
		out := rw.rewrite(string(body))

		if err := checkResidual(rw, rw.survivors(out), extraIgnored, out); err != nil {
			return nil, err
		}

		return []byte(out), nil
	}

	// Only the strings the rewrite touched are checked for org and custom
	// values: "role":"user" is never rewritten, so an org named "user" sitting
	// there is not a leak.
	var survived []string

	doc = rewriteJSONStrings(doc, func(s string) string {
		out := rw.rewrite(s)
		survived = append(survived, rw.survivors(out)...)

		return out
	})

	if err := checkResidual(rw, survived, extraIgnored, jsonStrings(doc)...); err != nil {
		return nil, err
	}

	return encodeJSON(doc)
}

// mapBodyTargets maps anything in the body that has no fiction yet. Every byte
// bound for the API passes through here, so this covers a target wherever it
// came from: the typed prompt, tool output, an @-mentioned file, a paste,
// CLAUDE.md, a session resumed from elsewhere, or a client other than Claude
// Code.
func mapBodyTargets(engDir string, texts, extraIgnored []string) error {
	mapped, err := mapTargets(engDir, extraIgnored, texts...)
	if err != nil {
		return fmt.Errorf("cannot map new targets: %w", err)
	}

	if mapped == 0 {
		return nil
	}

	logger.Info("auto-mapped new targets", "count", mapped)
	proxyCache.invalidate()

	return nil
}

// checkResidual is the fail-closed gate. Detection runs before the rewrite, so
// in a working pipeline nothing real is left; if something is, the rewriter
// missed it and the body must not go upstream. survived holds the mapped org
// names and custom-rule values the rewrite left behind (see survivors); texts
// are scanned for real-looking domains and IPs.
//
// The error names counts, never values. It is returned to the local client,
// which may fold the text into its next prompt, so the values go to the log
// the operator can read with "docker logs tanuki" instead.
func checkResidual(rw *rewriter, survived, extraIgnored []string, texts ...string) error {
	slices.Sort(survived)

	found := slices.DeleteFunc(residualTargets(extraIgnored, texts...), rw.isOwnFiction)

	residual := slices.Concat(found, slices.Compact(survived))
	if len(residual) == 0 {
		return nil
	}

	logger.Warn("real values survived anonymization",
		"count", len(residual),
		"values", residual,
	)

	if onLeakPolicy() == onLeakWarn {
		return nil
	}

	return fmt.Errorf(
		"%d real value(s) would have been sent unanonymized; see the tanuki log "+
			"(set TANUKI_ON_LEAK=warn to forward anyway)",
		len(residual),
	)
}
