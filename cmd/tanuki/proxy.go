package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
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

func runProxy() {
	port := getProxyPort()
	upstream := envOr("TANUKI_UPSTREAM", "https://api.anthropic.com")

	eng := getEngagement()
	if eng != "" {
		mappings := loadMappings(engagementDir(eng))
		fmt.Fprintf(os.Stderr, "[tanuki] Engagement: %s (%d mappings, reloaded per-request)\n", eng, len(mappings))
	} else {
		fmt.Fprintln(os.Stderr, "[tanuki] No active engagement yet (mappings auto-created on first use)")
	}

	fmt.Fprintf(os.Stderr, "[tanuki] Upstream: %s\n", upstream)
	fmt.Fprintf(os.Stderr, "[tanuki] Proxy listening on 0.0.0.0:%s\n", port)

	server := &http.Server{
		Addr:              "0.0.0.0:" + port, // binds all interfaces - required for Docker container accessibility
		ReadHeaderTimeout: 10 * time.Second,
		Handler:           &proxyHandler{upstream: upstream},
	}
	if err := server.ListenAndServe(); err != nil {
		fmt.Fprintf(os.Stderr, "[tanuki] Proxy error: %v\n", err)
		os.Exit(1)
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

//nolint:funlen // ServeHTTP handles complex proxying logic
func (h *proxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read request", http.StatusBadGateway)
		return
	}

	r.Body.Close()

	if strings.HasPrefix(r.URL.Path, "/v1/messages") && len(body) > 0 {
		eng := getEngagement()
		if eng != "" {
			mappings := loadMappings(engagementDir(eng))
			if len(mappings) > 0 {
				body = []byte(newRewriter(mappings, "r2f").rewrite(string(body)))
			}
		}
	}

	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, h.upstream+r.URL.RequestURI(), bytes.NewReader(body))
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

	proxyReq.ContentLength = int64(len(body))

	resp, err := httpClient.Do(proxyReq)
	if err != nil {
		http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for key, vals := range resp.Header {
		if hopByHopHeaders[http.CanonicalHeaderKey(key)] {
			continue
		}

		for _, val := range vals {
			w.Header().Add(key, val)
		}
	}

	w.WriteHeader(resp.StatusCode)

	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 4096)

	for {
		n, readErr := resp.Body.Read(buf)
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
