package main

// rtkCompressBody compresses tool_result content in an Anthropic request body.
// returns the body unchanged when disabled, nothing matched, or parsing failed.
//
// the body is walked as map[string]any, not the anthropicRequest struct:
// re-marshaling the struct would drop every field it does not model.
// runs before the PII guard masks, so compression sees original bytes and
// vault placeholders are never chopped by a filter.

import (
	"encoding/json"
	"strings"
)

func rtkCompressBody(body []byte) []byte {
	if !cfg().Rtk.Enabled {
		return body
	}
	blind := cfg().Rtk.BlindTruncate
	st := &rtkStats{}

	var root map[string]any
	if json.Unmarshal(body, &root) != nil {
		return body
	}
	msgs, _ := root["messages"].([]any)
	if len(msgs) == 0 {
		return body
	}

	changed := false
	for _, m := range msgs {
		msg, _ := m.(map[string]any)
		blocks, _ := msg["content"].([]any)
		if msg == nil || len(blocks) == 0 {
			continue
		}
		hit := false
		for _, bl := range blocks {
			b, _ := bl.(map[string]any)
			if b["type"] != "tool_result" || b["is_error"] == true {
				continue
			}
			text, kind := rtkToolResultText(b["content"])
			if text == "" {
				continue
			}
			// the client prefixes failures with "Error: " on the way out
			// (convertUserMessage), so a result starting that way is an error
			// even without is_error. leave it whole.
			if strings.HasPrefix(strings.TrimSpace(text), "Error:") {
				continue
			}
			out := rtkCompressText(text, st, blind)
			if out == text {
				continue
			}
			if kind == toolResultString {
				b["content"] = out
			} else {
				if !setTextParts(b["content"], out) {
					continue
				}
			}
			hit = true
		}
		changed = changed || hit
	}
	if !changed {
		return body
	}

	nb, err := json.Marshal(root)
	if err != nil {
		return body
	}
	if line := st.log(); line != "" {
		acclog.Printf("%s", line)
	}
	return nb
}

const (
	toolResultString = iota
	toolResultBlocks
)

// rtkToolResultText pulls the text out of a tool_result content field, which
// is either a bare string or an array of text/image blocks.
func rtkToolResultText(raw any) (string, int) {
	switch v := raw.(type) {
	case string:
		return v, toolResultString
	case []any:
		var parts []string
		for _, p := range v {
			if pm, ok := p.(map[string]any); ok && pm["type"] == "text" {
				if s, ok := pm["text"].(string); ok {
					parts = append(parts, s)
				}
			}
		}
		return strings.Join(parts, "\n"), toolResultBlocks
	}
	return "", toolResultString
}

// setTextParts writes joined text back across the block array, leaving
// non-text parts (images) in place. false when the shape is not a text array.
func setTextParts(raw any, text string) bool {
	arr, ok := raw.([]any)
	if !ok {
		return false
	}
	var parts []string
	for _, p := range arr {
		pm, ok := p.(map[string]any)
		if !ok || pm["type"] != "text" {
			return false
		}
		parts = append(parts, pm["text"].(string))
	}
	if len(parts) == 0 {
		return false
	}
	// single text block: keep it a block, carry the joined text
	if len(parts) == 1 {
		arr[0].(map[string]any)["text"] = text
		return true
	}
	// multiple: first holds everything, rest blanked so the model does not
	// read the same text twice
	arr[0].(map[string]any)["text"] = text
	for _, p := range arr[1:] {
		p.(map[string]any)["text"] = ""
	}
	return true
}
