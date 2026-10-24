package tanuki

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain initializes the package logger once so handlers that log (the proxy
// logs every request) do not panic on a nil logger during tests.
func TestMain(m *testing.M) {
	initLogger()
	os.Exit(m.Run())
}

// setupEngagement points TANUKI_DATA at a fresh temp dir, creates an active
// engagement named "eng", seeds it with the given domains, and returns its
// directory. Tests using it cannot run in parallel because t.Setenv forbids it.
func setupEngagement(t *testing.T, domains ...string) string {
	t.Helper()

	dataRoot := t.TempDir()
	t.Setenv("TANUKI_DATA", dataRoot)

	engDir := filepath.Join(dataRoot, "eng")
	if err := seedEngagement(engDir, defaultFictionOrg); err != nil {
		t.Fatalf("seedEngagement: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dataRoot, "current_engagement"), []byte("eng"), 0o600); err != nil {
		t.Fatalf("writing current_engagement: %v", err)
	}

	for _, d := range domains {
		if err := addDomain(engDir, d, "DEVTARGET"); err != nil {
			t.Fatalf("addDomain(%q): %v", d, err)
		}
	}

	return engDir
}

func newUpstream(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	return srv
}

func newProxy(t *testing.T, upstream string) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(&proxyHandler{upstream: upstream})
	t.Cleanup(srv.Close)

	return srv
}

// post sends a POST and returns the status and full response body, closing the
// body itself so callers deal only with the values they assert on.
func post(t *testing.T, url, path, body string) (status int, respBody string) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}

	defer func() { _ = resp.Body.Close() }()

	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response: %v", err)
	}

	return resp.StatusCode, string(out)
}

func TestProxyRewritesRequestToFiction(t *testing.T) {
	setupEngagement(t, testBaseDomain)

	received := make(chan string, 1)
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		received <- string(b)

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	proxy := newProxy(t, up.URL)

	status, _ := post(t, proxy.URL, "/v1/messages",
		`{"messages":[{"role":"user","content":"scan amazon.com"}]}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}

	got := <-received
	if strings.Contains(got, testBaseDomain) {
		t.Errorf("real domain leaked to upstream: %s", got)
	}

	if !strings.Contains(got, testFictionBase) {
		t.Errorf("expected fiction in upstream body, got: %s", got)
	}
}

// This test calls t.Setenv directly, which paralleltest recognizes, so it
// needs no suppression (unlike the tests that reach t.Setenv via a helper).
func TestProxyFailsClosedWithoutEngagement(t *testing.T) {
	t.Setenv("TANUKI_DATA", t.TempDir()) // no current_engagement, no mappings

	hit := make(chan struct{}, 1)
	up := newUpstream(t, func(_ http.ResponseWriter, _ *http.Request) {
		hit <- struct{}{}
	})

	proxy := newProxy(t, up.URL)

	status, _ := post(t, proxy.URL, "/v1/messages", `{"content":"`+testBaseDomain+`"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}

	select {
	case <-hit:
		t.Error("upstream was contacted with no engagement (fail-open leak)")
	default:
	}
}

func TestProxyFailsClosedOnEmptyMappings(t *testing.T) {
	setupEngagement(t) // active engagement, but zero mappings

	hit := make(chan struct{}, 1)
	up := newUpstream(t, func(_ http.ResponseWriter, _ *http.Request) {
		hit <- struct{}{}
	})

	proxy := newProxy(t, up.URL)

	// Nothing detectable in the body, so the empty mapping table is the only
	// reason to refuse; a body naming a target is covered by
	// TestProxyMapsOnFirstSightInsteadOfRefusing.
	status, _ := post(t, proxy.URL, "/v1/messages", `{"content":"nothing to rewrite here"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}

	select {
	case <-hit:
		t.Error("upstream was contacted with empty mappings (fail-open leak)")
	default:
	}
}

func TestProxyPassesThroughNonMessagesPath(t *testing.T) {
	setupEngagement(t, testBaseDomain)

	received := make(chan string, 1)
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		received <- string(b)

		w.WriteHeader(http.StatusOK)
	})

	proxy := newProxy(t, up.URL)

	// A non-/v1/messages path is a passthrough: neither rewritten nor
	// fail-closed, since it carries no prompt content.
	post(t, proxy.URL, "/v1/models", "amazon.com stays")

	if got := <-received; got != "amazon.com stays" {
		t.Errorf("non-messages path was altered: %q", got)
	}
}

func TestProxyReversesJSONResponse(t *testing.T) {
	setupEngagement(t, testBaseDomain)

	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"go to localhost:9000"}`)
	})

	proxy := newProxy(t, up.URL)

	_, body := post(t, proxy.URL, "/v1/messages", `{"m":1}`)
	if !strings.Contains(body, testBaseDomain) {
		t.Errorf("fiction not reversed to real in response: %s", body)
	}

	if strings.Contains(body, testFictionBase) {
		t.Errorf("fiction leaked to client: %s", body)
	}
}

func TestProxyReversesSSEResponse(t *testing.T) {
	setupEngagement(t, testBaseDomain)

	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		// A whole fiction value inside a single event round-trips.
		_, _ = io.WriteString(w, "event: delta\ndata: {\"text\":\"see localhost:9000 now\"}\n\n")

		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	})

	proxy := newProxy(t, up.URL)

	_, body := post(t, proxy.URL, "/v1/messages", `{"m":1}`)
	if !strings.Contains(body, testBaseDomain) {
		t.Errorf("SSE fiction not reversed to real: %s", body)
	}

	if strings.Contains(body, testFictionBase) {
		t.Errorf("fiction leaked to client in SSE: %s", body)
	}
}

// TestProxyRejectsOversizedBodyAsFinal: over the limit was a 502, which the
// client retries; it is a 413 now, and the limit matches the API's 32 MB.
func TestProxyRejectsOversizedBodyAsFinal(t *testing.T) {
	setupEngagement(t, testBaseDomain)

	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	proxy := newProxy(t, up.URL)

	// Screenshots are what make a body large: 11 MB of base64 is under the
	// limit, and is neither rewritten nor scanned.
	under := `{"content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` +
		strings.Repeat("A", 11<<20) + `"}}]}`
	if status, body := post(t, proxy.URL, "/v1/messages", under); status != http.StatusOK {
		t.Fatalf("11 MB body: status %d (%s), want 200", status, body)
	}

	status, body := post(t, proxy.URL, "/v1/messages", strings.Repeat("a", maxBodySize+1))
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", status)
	}

	if !strings.Contains(body, `"request_too_large"`) {
		t.Errorf("error is not in the Anthropic format: %s", body)
	}
}

// TestProxyRefusalIsAnAPIError: Claude Code shows the message of an error in
// the Anthropic format; a plain-text 503 was retried and then shown as noise.
func TestProxyRefusalIsAnAPIError(t *testing.T) {
	setupEngagement(t)

	proxy := newProxy(t, newUpstream(t, func(http.ResponseWriter, *http.Request) {}).URL)

	status, body := post(t, proxy.URL, "/v1/messages", `{"content":"nothing to rewrite here"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}

	var apiErr struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := json.Unmarshal([]byte(body), &apiErr); err != nil {
		t.Fatalf("refusal is not JSON: %q", body)
	}

	if apiErr.Type != "error" || apiErr.Error.Type != "invalid_request_error" ||
		!strings.HasPrefix(apiErr.Error.Message, "tanuki: ") {
		t.Errorf("unexpected error shape: %+v", apiErr)
	}
}

// TestProxyReportsBrokenUpstreamBody: the status used to go out before the
// body was read, so a dropped upstream became a 200 with an empty body.
func TestProxyReportsBrokenUpstreamBody(t *testing.T) {
	setupEngagement(t, testBaseDomain)

	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "1000")
		_, _ = io.WriteString(w, `{"partial":`)
	})
	proxy := newProxy(t, up.URL)

	if status, _ := post(t, proxy.URL, "/v1/messages", `{"content":"hi"}`); status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", status)
	}
}

// TestPathConfusionCannotSkipAnonymization: the Messages API was recognised by
// a raw prefix match, so "//v1/messages", "/v1/../v1/messages" and
// "/V1/messages" reached the same endpoint upstream while skipping the rewrite
// entirely. The prompt was forwarded verbatim.
func TestPathConfusionCannotSkipAnonymization(t *testing.T) {
	proxyEngagement(t, testBaseDomain)

	var received string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received = string(body)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	handler := &proxyHandler{upstream: upstream.URL}

	for _, p := range []string{"/v1/messages", "//v1/messages", "/v1/../v1/messages", "/V1/messages"} {
		received = ""
		req := httptest.NewRequestWithContext(
			t.Context(), http.MethodPost, p,
			strings.NewReader(`{"content":"scan `+testBaseDomain+`"}`),
		)
		handler.ServeHTTP(httptest.NewRecorder(), req)

		if strings.Contains(received, testBaseDomain) {
			t.Errorf("POST %s forwarded the real target: %s", p, received)
		}
	}
}

func TestIsMessagesRequest(t *testing.T) {
	t.Parallel()

	tests := map[string]bool{
		"/v1/messages":              true,
		"//v1/messages":             true,
		"/v1/../v1/messages":        true,
		"/V1/Messages":              true,
		"/v1/messages/count_tokens": true,
		"/v1/models":                false,
		"/v1/files":                 false,
		"/":                         false,
	}

	for p, want := range tests {
		if got := isMessagesRequest(p); got != want {
			t.Errorf("isMessagesRequest(%q) = %v, want %v", p, got, want)
		}
	}
}
