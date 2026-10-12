package tanuki

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The fixtures in testdata mirror the shape of a current Claude Code request
// and streamed response: cached system blocks, tool schemas with "$schema",
// signed thinking, tool_use and tool_result blocks, an image, metadata. The
// values are invented. Unit tests built from hand-written bodies missed that
// rewriting "$schema" broke every request, so these run the proxy over what
// Claude Code actually sends.

// fixtureRealValues are the targets the request fixture names.
var fixtureRealValues = []string{
	"acme-corp.com", "bounty.acme-corp.com", "portal.acme-corp.com",
	"sso.acme-corp.com", "security@acme-corp.com", "52.94.236.248",
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}

	return data
}

// assertPreserved walks the original and anonymized documents together and
// fails on any protocol, signed or binary value that changed. Prose may change;
// nothing else may.
func assertPreserved(t *testing.T, path string, orig, out any) {
	t.Helper()

	switch o := orig.(type) {
	case map[string]any:
		assertObjectPreserved(t, path, o, out)
	case []any:
		got, ok := out.([]any)
		if !ok || len(got) != len(o) {
			t.Errorf("%s: array shape changed", path)
			return
		}

		for i := range o {
			assertPreserved(t, path, o[i], got[i])
		}
	case string:
		if _, ok := out.(string); !ok {
			t.Errorf("%s: string became %T", path, out)
		}
	default:
		if !reflect.DeepEqual(orig, out) {
			t.Errorf("%s changed: %v -> %v", path, orig, out)
		}
	}
}

func assertObjectPreserved(t *testing.T, path string, orig map[string]any, out any) {
	t.Helper()

	got, ok := out.(map[string]any)
	if !ok || len(got) != len(orig) {
		t.Errorf("%s: object shape changed", path)
		return
	}

	for k, v := range orig {
		if keepAsIs(orig, k) || opaqueKeys[k] {
			if !reflect.DeepEqual(v, got[k]) {
				t.Errorf("%s.%s changed: %v -> %v", path, k, v, got[k])
			}

			continue
		}

		assertPreserved(t, path+"."+k, v, got[k])
	}
}

// TestClaudeCodeRequest runs a Claude Code request through anonymization on a
// fresh engagement, as the first request of a session does.
//
//nolint:paralleltest // t.Setenv forbids t.Parallel.
func TestClaudeCodeRequest(t *testing.T) {
	engDir := proxyEngagement(t)
	body := readFixture(t, "claude_code_request.json")

	out, err := anonymizeUpstreamBody(body)
	if err != nil {
		t.Fatalf("anonymizeUpstreamBody: %v", err)
	}

	var orig, got any
	if err := json.Unmarshal(body, &orig); err != nil {
		t.Fatal(err)
	}

	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("anonymized body is not JSON: %v", err)
	}

	assertPreserved(t, "$", orig, got)

	lower := strings.ToLower(string(out))
	for _, real := range fixtureRealValues {
		if strings.Contains(lower, real) {
			t.Errorf("real value %q reached the upstream body", real)
		}
	}

	for _, d := range []string{"claude.com", "json-schema.org"} {
		if mappingExists(engDir, "domain", d) {
			t.Errorf("%s was mapped as a target", d)
		}
	}

	// Claude Code resends the whole conversation every turn. The same history
	// must anonymize to the same bytes, or prompt caching misses every time.
	again, err := anonymizeUpstreamBody(body)
	if err != nil {
		t.Fatalf("second anonymizeUpstreamBody: %v", err)
	}

	if string(again) != string(out) {
		t.Error("the same request anonymized differently the second time")
	}
}

// sseClient replays a stream as Claude Code assembles it: the concatenated
// text of each content block, keyed by block index, plus every thinking and
// signature delta exactly as it arrived.
type sseClient struct {
	blocks map[float64]string
	signed []string
}

func readSSE(t *testing.T, stream string) sseClient {
	t.Helper()

	client := sseClient{blocks: map[float64]string{}}

	for line := range strings.Lines(stream) {
		payload, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "data: ")
		if !ok {
			continue
		}

		var ev struct {
			Type  string         `json:"type"`
			Index float64        `json:"index"`
			Delta map[string]any `json:"delta"`
		}

		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			t.Fatalf("event is not JSON: %q", payload)
		}

		if ev.Type != "content_block_delta" {
			continue
		}

		text, _ := ev.Delta["text"].(string)
		partial, _ := ev.Delta["partial_json"].(string)

		switch ev.Delta["type"] {
		case "text_delta", "input_json_delta":
			client.blocks[ev.Index] += text + partial
		default:
			client.signed = append(client.signed, payload)
		}
	}

	return client
}

// TestClaudeCodeResponse streams a Claude Code response back through the
// proxy's SSE path.
//
//nolint:paralleltest // t.Setenv forbids t.Parallel.
func TestClaudeCodeResponse(t *testing.T) {
	engDir := proxyEngagement(t, "acme-corp.com", "sso.acme-corp.com")
	stream := string(readFixture(t, "claude_code_response.sse"))

	rec := httptest.NewRecorder()
	streamSSEWithRewrite(rec, strings.NewReader(stream), newRewriter(loadMappings(engDir), directionF2R))

	sent, got := readSSE(t, stream), readSSE(t, rec.Body.String())

	// Thinking is signed: every thinking and signature delta arrives as sent.
	if !reflect.DeepEqual(got.signed, sent.signed) {
		t.Errorf("thinking or signature deltas changed:\n got %q\nwant %q", got.signed, sent.signed)
	}

	if want := "The login on acme-corp.com redirects to "; !strings.HasPrefix(got.blocks[1], want) {
		t.Errorf("text block = %q, want it to start %q", got.blocks[1], want)
	}

	if !strings.HasSuffix(rec.Body.String(), "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n") {
		t.Error("stream did not end with message_stop")
	}
}
