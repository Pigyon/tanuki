package tanuki

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"slices"
	"strings"
)

// typeKey is the field every API object names its kind with.
const typeKey = "type"

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
	typeKey:       true,
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

// binaryDataTypes are the objects whose "data" field holds base64, not prose.
// A word-bounded org rule can hit base64 between "+" and "/", and it cannot
// carry a domain or IP as text, so those are neither rewritten nor scanned.
//
// Matching the list, rather than "anything but text", is what keeps a tool's
// own "data" argument in scope: that is prose the model reads.
var binaryDataTypes = map[string]bool{
	"base64":            true,
	"redacted_thinking": true,
}

// isBinary reports whether obj[key] is base64.
func isBinary(obj map[string]any, key string) bool {
	if key != "data" {
		return false
	}

	t, _ := obj[typeKey].(string)

	return binaryDataTypes[t]
}

// keepAsIs reports whether the string at obj[key] must pass through untouched.
func keepAsIs(obj map[string]any, key string) bool {
	return structuralKeys[key] || opaqueKeys[key] || isBinary(obj, key)
}

// rewriteJSONStrings applies fn to every content string in v, in place.
func rewriteJSONStrings(v any, fn func(string) string) any {
	return rewriteJSONValue(v, fn, false)
}

// rewriteJSONValue walks v. toolArgs marks a subtree as a tool's arguments,
// where every string and every key is content: those names come from the
// tool's own schema, not the API, so a tool taking "name", "id" or "type"
// carries prose in them.
func rewriteJSONValue(v any, fn func(string) string, toolArgs bool) any {
	switch t := v.(type) {
	case map[string]any:
		if toolArgs {
			return rewriteToolArgs(t, fn)
		}

		isToolUse := t[typeKey] == "tool_use"

		for k, val := range t {
			if s, ok := val.(string); ok {
				if !keepAsIs(t, k) {
					t[k] = fn(s)
				}

				continue
			}

			if !opaqueKeys[k] && !keepAsIs(t, k) {
				t[k] = rewriteJSONValue(val, fn, isToolUse && k == "input")
			}
		}
	case []any:
		for i, val := range t {
			t[i] = rewriteJSONValue(val, fn, toolArgs)
		}
	case string:
		return fn(t)
	case json.Number:
		return rewriteJSONNumber(t, fn)
	}

	return v
}

// rewriteJSONNumber rewrites a number's text. The result goes back as a number
// when it is still a valid JSON literal, and as a string otherwise: an account
// fiction is zero-padded, which no number literal may be. Changing the type is
// better than leaving a real value in place.
func rewriteJSONNumber(n json.Number, fn func(string) string) any {
	out := fn(n.String())
	if out == n.String() {
		return n
	}

	if json.Valid([]byte(out)) {
		return json.Number(out)
	}

	return out
}

// rewriteToolArgs rewrites every key and value of a tool's arguments. It
// builds a new map because a key cannot be renamed while ranging over one.
//
// Keys are taken in order so the result does not depend on map iteration, and
// a key whose rewrite collides with one already placed keeps its original
// name: dropping an argument silently is worse than leaving one to the gate.
func rewriteToolArgs(t map[string]any, fn func(string) string) map[string]any {
	out := make(map[string]any, len(t))

	for _, k := range slices.Sorted(maps.Keys(t)) {
		key := fn(k)
		if _, taken := out[key]; taken {
			key = k
		}

		if s, ok := t[k].(string); ok {
			out[key] = fn(s)
			continue
		}

		out[key] = rewriteJSONValue(t[k], fn, true)
	}

	return out
}

// jsonStrings returns every key and string in v except opaque and binary
// ones: the decoded text that detection and the residual check scan. Scanning
// the raw body instead reads escapes as text ("\nacme.com" as "nacme.com").
// Each string is scanned on its own, so one seen in an earlier request is not
// scanned again (see memoCache).
func jsonStrings(v any) []string {
	var out []string

	var walk func(any)

	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, val := range t {
				if opaqueKeys[k] || isBinary(t, k) {
					continue
				}

				out = append(out, k)

				walk(val)
			}
		case []any:
			for _, val := range t {
				walk(val)
			}
		case string:
			out = append(out, t)
		case json.Number:
			// decodeJSON keeps numbers exact, so a numeric target identifier
			// (an AWS account, a tenant or ticket number) arrives as a Number
			// and matched neither case: unscanned by the gate and unrewritten.
			out = append(out, t.String())
		}
	}

	walk(v)

	return out
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

// jsonStringInterior returns s as it appears between the quotes of a JSON
// string, keeping a fragment valid when a value is inserted into it.
func jsonStringInterior(s string) string {
	quoted, err := encodeJSON(s)
	if err != nil || len(quoted) < 2 {
		return s
	}

	return string(quoted[1 : len(quoted)-1])
}
