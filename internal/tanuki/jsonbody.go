package tanuki

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// structuralKeys hold protocol values, not prose. Rewriting them corrupts the
// request: an org rule for "user" turned "role":"user" into "role":"DEVTARGET",
// one for "claude" broke the model name, and rewriting the "$schema" URL every
// Claude Code tool schema carries made the API reject the request. They are
// still scanned by the residual check, so a real value placed in one is
// refused, not forwarded.
var structuralKeys = map[string]bool{
	"$id":         true,
	"$ref":        true,
	"$schema":     true,
	"id":          true,
	"media_type":  true,
	"model":       true,
	"name":        true,
	"role":        true,
	"stop_reason": true,
	"tool_use_id": true,
	"type":        true,
	"user_id":     true,
}

// opaqueKeys hold signed model output that has to go back byte for byte: a
// thinking block whose text changed fails the API's signature check. They are
// neither rewritten nor scanned. The model wrote them, so upstream has already
// seen every value in them.
var opaqueKeys = map[string]bool{
	"signature": true,
	"thinking":  true,
}

// isBinary reports whether obj[key] is base64 (an image, a PDF, a redacted
// thinking block). A word-bounded org rule can hit it between "+" and "/", and
// it cannot hold a domain or IP as text, so it is neither rewritten nor
// scanned. Only a plain-text document source carries prose in "data".
func isBinary(obj map[string]any, key string) bool {
	return key == "data" && obj["type"] != "text"
}

// keepAsIs reports whether the string at obj[key] must pass through untouched.
func keepAsIs(obj map[string]any, key string) bool {
	return structuralKeys[key] || opaqueKeys[key] || isBinary(obj, key)
}

// rewriteJSONStrings applies fn to every content string in v, in place.
func rewriteJSONStrings(v any, fn func(string) string) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if s, ok := val.(string); ok {
				if !keepAsIs(t, k) {
					t[k] = fn(s)
				}

				continue
			}

			if !opaqueKeys[k] {
				t[k] = rewriteJSONStrings(val, fn)
			}
		}
	case []any:
		for i, val := range t {
			t[i] = rewriteJSONStrings(val, fn)
		}
	case string:
		return fn(t)
	}

	return v
}

// jsonText returns every key and string in v except opaque and binary ones,
// one per line: the decoded text that detection and the residual check scan.
// Scanning the raw body instead reads escapes as text ("\nacme.com" as
// "nacme.com").
func jsonText(v any) string {
	var b strings.Builder

	var walk func(any)

	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, val := range t {
				if opaqueKeys[k] || isBinary(t, k) {
					continue
				}

				b.WriteString(k)
				b.WriteByte('\n')
				walk(val)
			}
		case []any:
			for _, val := range t {
				walk(val)
			}
		case string:
			b.WriteString(t)
			b.WriteByte('\n')
		}
	}

	walk(v)

	return b.String()
}

// decodeJSON parses a whole body, keeping numbers exact. It reports false for
// anything that is not a single JSON value.
func decodeJSON(data []byte) (any, bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}

	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, false
	}

	return v, true
}

func encodeJSON(v any) ([]byte, error) {
	var buf bytes.Buffer

	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)

	if err := enc.Encode(v); err != nil {
		return nil, err
	}

	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// reverseBody rewrites fiction back to real in a response body, leaving
// protocol fields and signed thinking alone. A body that is not JSON is
// rewritten as plain text. One with nothing to reverse, which is most stream
// events, is returned as it arrived rather than re-encoded.
func reverseBody(data string, rw *rewriter) string {
	if rw == nil {
		return data
	}

	doc, ok := decodeJSON([]byte(data))
	if !ok {
		return rw.rewrite(data)
	}

	changed := false
	doc = rewriteJSONStrings(doc, func(s string) string {
		out := rw.rewrite(s)
		changed = changed || out != s

		return out
	})

	if !changed {
		return data
	}

	out, err := encodeJSON(doc)
	if err != nil {
		return rw.rewrite(data)
	}

	return string(out)
}

// reverseSSEEvent reverses the JSON payload of each "data:" line in one SSE
// event, leaving the framing ("event:", line endings) exactly as it arrived.
func reverseSSEEvent(event string, rw *rewriter) string {
	lines := strings.SplitAfter(event, "\n")

	for i, line := range lines {
		content := strings.TrimRight(line, "\r\n")

		payload, ok := strings.CutPrefix(content, "data:")
		if !ok {
			continue
		}

		lead := ""
		if strings.HasPrefix(payload, " ") {
			lead, payload = " ", payload[1:]
		}

		lines[i] = "data:" + lead + reverseBody(payload, rw) + line[len(content):]
	}

	return strings.Join(lines, "")
}
