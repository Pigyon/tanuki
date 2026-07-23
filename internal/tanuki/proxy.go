package tanuki

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
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
	maxBodySize = 10 << 20 // 10 MB request body
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

	if err := configureHooks(); err != nil {
		fatal(err.Error())
	}

	if err := generateClaudeMD(engDir); err != nil {
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
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutting down proxy")
	case err := <-errCh:
		fatal("proxy listen failed: " + err.Error())
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown error", "error", err)
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

func (h *proxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadGateway)
		return
	}

	_ = r.Body.Close()

	isMessagesPath := strings.HasPrefix(r.URL.Path, "/v1/messages")

	if isMessagesPath && len(body) > 0 {
		anonymized, err := anonymizeUpstreamBody(body)
		if err != nil {
			// Fail closed: never forward a prompt body we could not anonymize.
			logger.Warn("refusing to forward request unanonymized", "path", r.URL.Path, "reason", err.Error())
			http.Error(w, "tanuki refused to forward request: "+err.Error(), http.StatusServiceUnavailable)

			return
		}

		body = anonymized
	}

	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, h.upstream+r.URL.RequestURI(), bytes.NewReader(body))
	if err != nil {
		http.Error(w, "failed to create request", http.StatusBadGateway)
		return
	}

	copyProxyHeaders(proxyReq.Header, r.Header, false)

	if isMessagesPath {
		proxyReq.Header.Del("Accept-Encoding")
	}

	proxyReq.ContentLength = int64(len(body))

	resp, err := httpClient.Do(proxyReq)
	if err != nil {
		http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}

	defer func() { _ = resp.Body.Close() }()

	isSSE := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
	willRewrite := isMessagesPath && (isSSE || resp.ContentLength != 0)

	logger.Info("proxied request",
		"method", r.Method,
		"path", r.URL.Path,
		"status", resp.StatusCode,
		"anonymized", isMessagesPath,
		"sse", isSSE,
	)

	copyProxyHeaders(w.Header(), resp.Header, willRewrite)
	w.WriteHeader(resp.StatusCode)
	h.forwardResponse(w, resp, isMessagesPath, isSSE)
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

func (h *proxyHandler) forwardResponse(w http.ResponseWriter, resp *http.Response, messagesPath, isSSE bool) {
	if !messagesPath {
		h.forwardRaw(w, resp.Body)
		return
	}

	rw := responseRewriter()
	if isSSE {
		h.streamSSEWithRewrite(w, resp.Body, rw)
		return
	}

	h.forwardWithRewrite(w, resp.Body, rw)
}

// sseSink is the destination for rewritten SSE events. A nil flusher means
// the underlying ResponseWriter does not support flushing.
type sseSink struct {
	w       io.Writer
	flusher http.Flusher
	rw      *rewriter
}

func newSSESink(w http.ResponseWriter, rw *rewriter) sseSink {
	flusher, _ := w.(http.Flusher)

	return sseSink{w: w, flusher: flusher, rw: rw}
}

// write rewrites text and sends it downstream, flushing so the client sees
// each event as it arrives rather than at end of stream.
func (s sseSink) write(text string) {
	if s.rw != nil {
		text = s.rw.rewrite(text)
	}

	_, _ = io.WriteString(s.w, text)

	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// streamSSEWithRewrite reverses fiction to real one SSE event at a time. A value
// split across separate events is not rejoined (response direction, not a leak).
func (h *proxyHandler) streamSSEWithRewrite(w http.ResponseWriter, body io.Reader, rw *rewriter) {
	sink := newSSESink(w, rw)

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

			break
		}
	}
}

// flushRemainder writes whatever is left in buf after the stream ends, so a
// partial trailing event still gets rewritten rather than dropped.
func flushRemainder(buf *bytes.Buffer, sink sseSink) {
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
func flushCompleteEvents(buf *bytes.Buffer, sink sseSink) {
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

func (h *proxyHandler) forwardWithRewrite(w http.ResponseWriter, body io.Reader, rw *rewriter) {
	respBody, err := io.ReadAll(io.LimitReader(body, maxRespSize+1))
	if err != nil {
		return
	}

	if len(respBody) > maxRespSize {
		logger.Warn("response exceeded rewrite limit; truncating", "limit", maxRespSize)
		respBody = respBody[:maxRespSize]
	}

	text := string(respBody)
	if rw != nil {
		text = rw.rewrite(text)
	}

	_, _ = io.WriteString(w, text)
}

func (h *proxyHandler) forwardRaw(w http.ResponseWriter, body io.Reader) {
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

// anonymizeUpstreamBody rewrites a body's real values to fiction before it leaves.
// It fails closed: no engagement / unreadable / empty mappings return an error.
func anonymizeUpstreamBody(body []byte) ([]byte, error) {
	eng := currentEngagement()
	if eng == "" {
		return nil, errors.New("no active engagement (run: tanuki add <domain>)")
	}

	rw, err := proxyCache.rewriterOrError(engagementDir(eng), directionR2F)
	if err != nil {
		return nil, fmt.Errorf("cannot read mappings: %w", err)
	}

	if rw == nil {
		return nil, errors.New("no target mappings configured (run: tanuki add <domain>)")
	}

	return []byte(rw.rewrite(string(body))), nil
}
