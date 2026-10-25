package tanuki

import (
	"encoding/json"
	"strings"
	"testing"
)

// field parses an anonymized body and returns the value at path (object keys
// and array indexes), so a test can assert on fields rather than raw text.
func field(t *testing.T, out []byte, path ...any) any {
	t.Helper()

	var v any
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("anonymized body is not JSON: %v\n%s", err, out)
	}

	for _, step := range path {
		switch key := step.(type) {
		case string:
			obj, ok := v.(map[string]any)
			if !ok {
				t.Fatalf("no object at %v in %s", path, out)
			}

			v = obj[key]
		case int:
			arr, ok := v.([]any)
			if !ok || key >= len(arr) {
				t.Fatalf("no element at %v in %s", path, out)
			}

			v = arr[key]
		}
	}

	return v
}

// TestAnonymizeLeavesProtocolFieldsAlone: rewriting the raw body turned
// "role":"user" into "role":"DEVTARGET" once "user" was an org, and the model
// name into "DEVTARGET2-opus-5-5" once "claude" was, and the API rejected
// every request after that.
func TestAnonymizeLeavesProtocolFieldsAlone(t *testing.T) {
	proxyEngagement(t, "user.io", "claude.io")

	out, err := anonymizeUpstreamBody([]byte(`{"model":"claude-opus-5-5","messages":[` +
		`{"role":"user","content":[{"type":"text","text":"the user asked claude about user.io"}]}]}`))
	if err != nil {
		t.Fatalf("anonymizeUpstreamBody: %v", err)
	}

	if got := field(t, out, "model"); got != "claude-opus-5-5" {
		t.Errorf("model = %v, want it untouched", got)
	}

	if got := field(t, out, "messages", 0, "role"); got != "user" {
		t.Errorf("role = %v, want it untouched", got)
	}

	if got := field(t, out, "messages", 0, "content", 0, "type"); got != "text" {
		t.Errorf("type = %v, want it untouched", got)
	}

	if text, _ := field(t, out, "messages", 0, "content", 0, "text").(string); strings.Contains(text, "user.io") ||
		!strings.Contains(text, "localhost:") {
		t.Errorf("content was not anonymized: %q", text)
	}
}

// TestAnonymizeKeepsToolSchemasValid: every Claude Code tool schema names its
// dialect by URL. Mapping json-schema.org rewrote it to a localhost URL and the
// API rejected the request as an invalid schema. An engagement that already
// mapped it must not rewrite it either.
func TestAnonymizeKeepsToolSchemasValid(t *testing.T) {
	engDir := proxyEngagement(t, testBaseDomain)

	const dialect = "https://json-schema.org/draft/2020-12/schema"

	body := []byte(`{"tools":[{"name":"Bash","description":"Run a command, e.g. curl https://example.com",` +
		`"input_schema":{"$schema":"` + dialect + `","type":"object","properties":{"command":{"type":"string"}}}}],` +
		`"messages":[{"role":"user","content":"scan amazon.com"}]}`)

	out, err := anonymizeUpstreamBody(body)
	if err != nil {
		t.Fatalf("anonymizeUpstreamBody: %v", err)
	}

	if got := field(t, out, "tools", 0, "input_schema", "$schema"); got != dialect {
		t.Errorf("$schema = %v, want %s", got, dialect)
	}

	if mappingExists(engDir, "domain", "json-schema.org") {
		t.Error("json-schema.org was mapped as a target")
	}

	// An engagement from before the fix may already hold the mapping.
	if err := addDomain(engDir, "json-schema.org", ""); err != nil {
		t.Fatal(err)
	}

	proxyCache.invalidate()

	out, err = anonymizeUpstreamBody(body)
	if err != nil {
		t.Fatalf("anonymizeUpstreamBody with a stale mapping: %v", err)
	}

	if got := field(t, out, "tools", 0, "input_schema", "$schema"); got != dialect {
		t.Errorf("$schema with a stale mapping = %v, want %s", got, dialect)
	}
}

// TestAnonymizeReadsDecodedStrings: scanning the raw body read the escape in
// "\nsecret-target.io" as part of the name, mapped "nsecret-target.io", and
// let the real host through.
func TestAnonymizeReadsDecodedStrings(t *testing.T) {
	proxyEngagement(t, testBaseDomain)

	out, err := anonymizeUpstreamBody([]byte(`{"content":"first line\nsecret-target.io"}`))
	if err != nil {
		t.Fatalf("anonymizeUpstreamBody: %v", err)
	}

	if strings.Contains(string(out), "secret-target.io") {
		t.Errorf("host after an escape leaked: %s", out)
	}
}

// TestToolInputDataFieldIsAnonymized: "data" was treated as base64 whenever
// its object was not a text block, but a tool's arguments are an object of the
// tool's own making. A tool called with a "data" argument -- and Claude Code
// resends every tool_use in history -- went upstream unrewritten, and the
// fail-closed gate skipped it too, so nothing reported the leak.
func TestToolInputDataFieldIsAnonymized(t *testing.T) {
	proxyEngagement(t, testBaseDomain)

	out, err := anonymizeUpstreamBody([]byte(`{"messages":[{"role":"assistant","content":[` +
		`{"type":"tool_use","name":"Bash","id":"tu_1","input":` +
		`{"data":"curl https://bounty.amazon.com","command":"ping ` + testPublicIP + `"}}]}]}`))
	if err != nil {
		t.Fatalf("anonymizeUpstreamBody: %v", err)
	}

	for _, real := range []string{testSubDomain, testBaseDomain, testPublicIP} {
		if strings.Contains(string(out), real) {
			t.Errorf("real value %q reached the upstream body: %s", real, out)
		}
	}
}

// TestToolArgumentsAreContentNotProtocol: a tool's arguments follow the tool's
// own schema, so a tool taking "name", "id" or "type" carries prose there.
// Those were treated as protocol fields and left alone, and because Claude
// Code resends every tool_use in history the gate then refused every later
// turn of the conversation.
func TestToolArgumentsAreContentNotProtocol(t *testing.T) {
	engDir := proxyEngagement(t, testBaseDomain)

	if _, err := addCustomRule(engDir, "John Smith", "Jane Dev"); err != nil {
		t.Fatalf("addCustomRule: %v", err)
	}

	proxyCache.invalidate()

	out, err := anonymizeUpstreamBody([]byte(`{"messages":[{"role":"user","content":[` +
		`{"type":"tool_use","name":"Bash","id":"tu_1","input":` +
		`{"name":"John Smith at ` + testBaseDomain + `","id":"` + testPublicIP + `",` +
		`"nested":{"type":"` + testSubDomain + `"}}}]}]}`))
	if err != nil {
		t.Fatalf("anonymizeUpstreamBody: %v", err)
	}

	for _, real := range []string{"John Smith", testBaseDomain, testPublicIP, testSubDomain} {
		if strings.Contains(string(out), real) {
			t.Errorf("%q survived in tool arguments: %s", real, out)
		}
	}

	// The envelope around the arguments is still protocol.
	for _, protocol := range []string{`"type":"tool_use"`, `"name":"Bash"`, `"id":"tu_1"`, `"role":"user"`} {
		if !strings.Contains(string(out), protocol) {
			t.Errorf("protocol field %s was rewritten: %s", protocol, out)
		}
	}
}

// TestToolArgumentKeysAreRewritten: a key in a tool's arguments is the tool's
// own, so a target used as one must not be forwarded.
func TestToolArgumentKeysAreRewritten(t *testing.T) {
	proxyEngagement(t, testBaseDomain)

	out, err := anonymizeUpstreamBody([]byte(`{"messages":[{"role":"user","content":[` +
		`{"type":"tool_use","name":"Http","input":{"headers":{"host-` + testBaseDomain + `":"v"}}}]}]}`))
	if err != nil {
		t.Fatalf("anonymizeUpstreamBody: %v", err)
	}

	if strings.Contains(string(out), testBaseDomain) {
		t.Errorf("a target used as a tool argument key survived: %s", out)
	}
}

// TestRedactedThinkingPassesThrough: its "data" is an encrypted blob the API
// has to get back byte for byte.
func TestRedactedThinkingPassesThrough(t *testing.T) {
	proxyEngagement(t, testBaseDomain)

	const blob = "EvwBCkYIARgCKkB+amazon/redacted=="

	out, err := anonymizeUpstreamBody([]byte(`{"messages":[{"role":"assistant","content":[` +
		`{"type":"redacted_thinking","data":"` + blob + `"},` +
		`{"type":"text","text":"ok"}]}]}`))
	if err != nil {
		t.Fatalf("anonymizeUpstreamBody: %v", err)
	}

	if got := field(t, out, "messages", 0, "content", 0, "data"); got != blob {
		t.Errorf("redacted thinking changed: %q", got)
	}
}

// TestAnonymizeKeepsSignedAndBinaryData: a thinking block must go back exactly
// as the model wrote it or its signature fails, and base64 must not be hit by
// a word-bounded rule between "+" and "/".
func TestAnonymizeKeepsSignedAndBinaryData(t *testing.T) {
	proxyEngagement(t, testBaseDomain)

	const (
		thinking = "Plan: test for SQL injection on the login form"
		image    = "iVBORw0KGgo+amazon/AAAANSUhEUgAA"
	)

	out, err := anonymizeUpstreamBody([]byte(`{"messages":[{"role":"assistant","content":[` +
		`{"type":"thinking","thinking":"` + thinking + `","signature":"EqQBCkYIARgCKkA+amazon/sig"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + image + `"}},` +
		`{"type":"text","text":"SQL injection on amazon.com"}]}]}`))
	if err != nil {
		t.Fatalf("anonymizeUpstreamBody: %v", err)
	}

	content := func(path ...any) any { return field(t, out, append([]any{"messages", 0, "content"}, path...)...) }

	if got := content(0, "thinking"); got != thinking {
		t.Errorf("thinking changed: %q", got)
	}

	if got := content(0, "signature"); got != "EqQBCkYIARgCKkA+amazon/sig" {
		t.Errorf("signature changed: %q", got)
	}

	if got := content(1, "source", "data"); got != image {
		t.Errorf("base64 data changed: %q", got)
	}

	if got, _ := content(2, "text").(string); strings.Contains(got, testBaseDomain) ||
		strings.Contains(got, "SQL injection") {
		t.Errorf("text block was not anonymized: %q", got)
	}
}

// TestReverseSSEEventKeepsThinkingAndFraming: thinking deltas stay as the
// model sent them (the client returns them, signed); text deltas are reversed;
// the event framing is untouched.
func TestReverseSSEEventKeepsThinkingAndFraming(t *testing.T) {
	engDir := proxyEngagement(t, testBaseDomain)
	rw := newRewriter(loadMappings(engDir), directionF2R)

	thinking := "event: content_block_delta\r\ndata: {\"delta\":{\"thinking\":\"probe localhost:9000\"," +
		"\"type\":\"thinking_delta\"},\"index\":0,\"type\":\"content_block_delta\"}\r\n\r\n"
	if got := reverseSSEEvent(thinking, rw); got != thinking {
		t.Errorf("thinking delta changed:\n got %q\nwant %q", got, thinking)
	}

	text := "event: content_block_delta\ndata: {\"delta\":{\"text\":\"DEVTARGET runs on localhost:9000\"," +
		"\"type\":\"text_delta\"}}\n\n"

	got := reverseSSEEvent(text, rw)
	if !strings.Contains(got, `"text":"amazon runs on amazon.com"`) {
		t.Errorf("text delta not reversed: %q", got)
	}

	if !strings.HasPrefix(got, "event: content_block_delta\ndata: {") || !strings.HasSuffix(got, "}\n\n") {
		t.Errorf("framing changed: %q", got)
	}
}

// TestCollidingToolArgumentKeysKeepBothValues: two keys that rewrite to the
// same name used to overwrite one another, so an argument vanished and which
// one survived depended on map iteration order.
func TestCollidingToolArgumentKeysKeepBothValues(t *testing.T) {
	proxyEngagement(t, testBaseDomain)

	const body = `{"messages":[{"role":"user","content":[` +
		`{"type":"tool_use","id":"t1","name":"X","input":{"amazon":1,"AMAZON":2}}]}]}`

	first, err := anonymizeUpstreamBody([]byte(body))
	if err != nil {
		t.Fatalf("anonymizeUpstreamBody: %v", err)
	}

	args, _ := field(t, first, "messages", 0, "content", 0, "input").(map[string]any)
	if len(args) != 2 {
		t.Errorf("tool arguments lost a key: %v", args)
	}

	second, err := anonymizeUpstreamBody([]byte(body))
	if err != nil {
		t.Fatalf("anonymizeUpstreamBody: %v", err)
	}

	if string(first) != string(second) {
		t.Errorf("the same body produced two results:\n %s\n %s", first, second)
	}
}
