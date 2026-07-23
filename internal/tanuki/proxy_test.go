package tanuki

import (
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

	// The active-engagement fallback is process-global; clear it so a prior
	// test cannot leak an engagement into a "no engagement" case.
	lastKnownEngagement.Store("")

	dataRoot := t.TempDir()
	t.Setenv("TANUKI_DATA", dataRoot)

	engDir := filepath.Join(dataRoot, "eng")
	if err := seedEngagement(engDir, "DEVTARGET"); err != nil {
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

//nolint:paralleltest // t.Setenv (via setupEngagement) forbids t.Parallel.
func TestProxyRewritesRequestToFiction(t *testing.T) {
	setupEngagement(t, "amazon.com")

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
	if strings.Contains(got, "amazon.com") {
		t.Errorf("real domain leaked to upstream: %s", got)
	}

	if !strings.Contains(got, "localhost:9000") {
		t.Errorf("expected fiction in upstream body, got: %s", got)
	}
}

// This test calls t.Setenv directly, which paralleltest recognizes, so it
// needs no suppression (unlike the tests that reach t.Setenv via a helper).
func TestProxyFailsClosedWithoutEngagement(t *testing.T) {
	lastKnownEngagement.Store("")
	t.Setenv("TANUKI_DATA", t.TempDir()) // no current_engagement, no mappings

	hit := make(chan struct{}, 1)
	up := newUpstream(t, func(_ http.ResponseWriter, _ *http.Request) {
		hit <- struct{}{}
	})

	proxy := newProxy(t, up.URL)

	status, _ := post(t, proxy.URL, "/v1/messages", `{"content":"amazon.com"}`)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", status)
	}

	select {
	case <-hit:
		t.Error("upstream was contacted with no engagement (fail-open leak)")
	default:
	}
}

//nolint:paralleltest // t.Setenv (via setupEngagement) forbids t.Parallel.
func TestProxyFailsClosedOnEmptyMappings(t *testing.T) {
	setupEngagement(t) // active engagement, but zero mappings

	hit := make(chan struct{}, 1)
	up := newUpstream(t, func(_ http.ResponseWriter, _ *http.Request) {
		hit <- struct{}{}
	})

	proxy := newProxy(t, up.URL)

	status, _ := post(t, proxy.URL, "/v1/messages", `{"content":"amazon.com"}`)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", status)
	}

	select {
	case <-hit:
		t.Error("upstream was contacted with empty mappings (fail-open leak)")
	default:
	}
}

//nolint:paralleltest // t.Setenv (via setupEngagement) forbids t.Parallel.
func TestProxyPassesThroughNonMessagesPath(t *testing.T) {
	setupEngagement(t, "amazon.com")

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

//nolint:paralleltest // t.Setenv (via setupEngagement) forbids t.Parallel.
func TestProxyReversesJSONResponse(t *testing.T) {
	setupEngagement(t, "amazon.com")

	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"go to localhost:9000"}`)
	})

	proxy := newProxy(t, up.URL)

	_, body := post(t, proxy.URL, "/v1/messages", `{"m":1}`)
	if !strings.Contains(body, "amazon.com") {
		t.Errorf("fiction not reversed to real in response: %s", body)
	}

	if strings.Contains(body, "localhost:9000") {
		t.Errorf("fiction leaked to client: %s", body)
	}
}

//nolint:paralleltest // t.Setenv (via setupEngagement) forbids t.Parallel.
func TestProxyReversesSSEResponse(t *testing.T) {
	setupEngagement(t, "amazon.com")

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
	if !strings.Contains(body, "amazon.com") {
		t.Errorf("SSE fiction not reversed to real: %s", body)
	}

	if strings.Contains(body, "localhost:9000") {
		t.Errorf("fiction leaked to client in SSE: %s", body)
	}
}
