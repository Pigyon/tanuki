package tanuki

import (
	"bytes"
	"context"
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

const maxBodySize = 10 << 20 // 10 MB

var (
	nnBoundary   = []byte("\n\n")
	rnrnBoundary = []byte("\r\n\r\n")
)

func runProxy() {
	port := getProxyPort()
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
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       120 * time.Second,
		Handler:           &proxyHandler{upstream: upstream},
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("proxy listen failed", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down proxy")

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

//nolint:funlen,gocyclo // ServeHTTP handles complex proxying logic
func (h *proxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadGateway)
		return
	}

	r.Body.Close()

	isMessagesPath := strings.HasPrefix(r.URL.Path, "/v1/messages")

	if isMessagesPath && len(body) > 0 {
		eng := getEngagement()
		if eng != "" {
			rw := proxyCache.getRewriter(engagementDir(eng), "r2f")
			if rw != nil {
				body = []byte(rw.rewrite(string(body)))
			}
		}
	}

	proxyReq, err := http.NewRequestWithContext(
		r.Context(),
		r.Method,
		h.upstream+r.URL.RequestURI(),
		bytes.NewReader(body),
	)
	if err != nil {
		http.Error(w, "failed to create request", http.StatusBadGateway)
		return
	}

	for key, vals := range r.Header {
		if hopByHopHeaders[http.CanonicalHeaderKey(key)] {
			continue
		}

		for _, val := range vals {
			proxyReq.Header.Add(key, val)
		}
	}

	if isMessagesPath {
		proxyReq.Header.Del("Accept-Encoding")
	}

	proxyReq.ContentLength = int64(len(body))

	resp, err := httpClient.Do(proxyReq)
	if err != nil {
		http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	isSSE := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
	willRewrite := isMessagesPath && (isSSE || resp.ContentLength != 0)

	logger.Info("proxied request",
		"method", r.Method,
		"path", r.URL.Path,
		"status", resp.StatusCode,
		"anonymized", isMessagesPath,
		"sse", isSSE,
	)

	for key, vals := range resp.Header {
		if hopByHopHeaders[http.CanonicalHeaderKey(key)] {
			continue
		}

		if willRewrite && http.CanonicalHeaderKey(key) == "Content-Length" {
			continue
		}

		for _, val := range vals {
			w.Header().Add(key, val)
		}
	}

	w.WriteHeader(resp.StatusCode)

	if isMessagesPath {
		rw := responseRewriter()

		if isSSE {
			h.streamSSEWithRewrite(w, resp.Body, rw)
		} else {
			h.forwardWithRewrite(w, resp.Body, rw)
		}

		return
	}

	h.forwardRaw(w, resp.Body)
}

func (h *proxyHandler) streamSSEWithRewrite(w http.ResponseWriter, body io.Reader, rw *rewriter) {
	flusher, canFlush := w.(http.Flusher)
	var lineBuf bytes.Buffer
	buf := make([]byte, 4096)

	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			lineBuf.Write(buf[:n])
			h.flushCompleteEvents(&lineBuf, w, canFlush, flusher, rw)
		}

		if readErr != nil {
			if lineBuf.Len() > 0 {
				remaining := lineBuf.String()
				if rw != nil {
					remaining = rw.rewrite(remaining)
				}

				_, _ = io.WriteString(w, remaining)

				if canFlush {
					flusher.Flush()
				}
			}

			break
		}
	}
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

func (h *proxyHandler) flushCompleteEvents(
	buf *bytes.Buffer,
	w http.ResponseWriter,
	canFlush bool,
	flusher http.Flusher,
	rw *rewriter,
) {
	for {
		data := buf.Bytes()
		idx, bLen := findEventBoundary(data)

		if idx < 0 {
			break
		}

		event := string(data[:idx+bLen])
		if rw != nil {
			event = rw.rewrite(event)
		}

		_, _ = io.WriteString(w, event)

		if canFlush {
			flusher.Flush()
		}

		buf.Next(idx + bLen)
	}
}

func (h *proxyHandler) forwardWithRewrite(w http.ResponseWriter, body io.Reader, rw *rewriter) {
	respBody, err := io.ReadAll(body)
	if err != nil {
		return
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
	eng := getEngagement()
	if eng == "" {
		return nil
	}

	return proxyCache.getRewriter(engagementDir(eng), "f2r")
}
