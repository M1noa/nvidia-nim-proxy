package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"os"
	"strings"
	"sync"
	"time"
)

// --- Types ---

type anthropicRequest struct {
	Model         string             `json:"model"`
	System        json.RawMessage    `json:"system,omitempty"`
	Messages      []anthropicMsg     `json:"messages"`
	Tools         []anthropicTool    `json:"tools,omitempty"`
	ToolChoice    any                `json:"tool_choice,omitempty"`
	MaxTokens     int                `json:"max_tokens"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	TopK          *int               `json:"top_k,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	Thinking      *anthropicThinking `json:"thinking,omitempty"`
}

type anthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

// thinkingToEffort maps Claude Code's thinking field to a reasoning effort.
// Returns "" when the client did not send an explicit budget, so the model
// default (from model_params) applies.
func thinkingToEffort(t *anthropicThinking) string {
	if t == nil {
		return ""
	}
	switch t.Type {
	case "enabled":
		switch {
		case t.BudgetTokens <= 0:
			return "medium"
		case t.BudgetTokens <= 4000:
			return "low"
		case t.BudgetTokens <= 16000:
			return "medium"
		default:
			return "high"
		}
	default:
		return ""
	}
}

type anthropicMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Source    *struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	} `json:"source,omitempty"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
	Function    *struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// --- Config ---

type claudeModelEntry struct {
	Pattern string `json:"pattern"`
	Model   string `json:"model"`
}

var (
	claudeModels   []*claudeModelEntry
	claudeModelsMu sync.RWMutex
)

// claude mapping lives in config.yml models.claude_map now; applyConfig
// rebuilds it, so there is no separate loader anymore.

func mapClaudeModel(model string) string {
	claudeModelsMu.RLock()
	defer claudeModelsMu.RUnlock()
	ml := strings.ToLower(model)
	for _, e := range claudeModels {
		if globMatch(e.Pattern, ml) {
			return e.Model
		}
	}
	return model
}

// toolNames are the tool names the client declared, in the client's spelling.
// Upstream models (zen, spark, and other OpenAI-compatible gateways) frequently
// return tool call names lowercased ("bash" for "Bash"), which makes Claude Code
// reject the call with "No such tool available". Response emitters run the
// upstream name back through resolve so tool_use carries the declared spelling.
type toolNames []string

// resolve maps an upstream tool call name back to the client's spelling: exact
// match first, then case-insensitive. Unknown names pass through unchanged.
func (tn toolNames) resolve(name string) string {
	if !cfg().Translate.restoreToolNames() {
		return name
	}
	for _, c := range tn {
		if c == name {
			return c
		}
	}
	for _, c := range tn {
		if strings.EqualFold(c, name) {
			return c
		}
	}
	return name
}

// --- Request Conversion ---

func anthropicRequestToOpenAI(body []byte) (oai []byte, clientModel, upstreamModel string, tnames toolNames, isStream bool, err error) {
	var req anthropicRequest
	if err = json.Unmarshal(body, &req); err != nil {
		return nil, "", "", nil, false, fmt.Errorf("invalid JSON: %w", err)
	}

	clientModel = strings.TrimSuffix(req.Model, "[1m]")
	upstreamModel = mapClaudeModel(clientModel)
	isStream = req.Stream

	messages := make([]map[string]any, 0)

	if sysText := extractSystemText(req.System); sysText != "" {
		sysText, _ = stripSomeGuardrails(sysText)
		if line := cfg().Inject.HelpfulText; cfg().Inject.HelpfulLine && line != "" && !strings.Contains(sysText, line) {
			sysText = line + "\n" + sysText
		}
		if dl := discloseLine(); dl != "" && !strings.Contains(sysText, dl) {
			sysText += "\n" + dl
		}
		messages = append(messages, map[string]any{"role": "system", "content": sysText})
	} else if dl := discloseLine(); dl != "" {
		messages = append(messages, map[string]any{"role": "system", "content": dl})
	}

	for _, msg := range req.Messages {
		switch msg.Role {
		case "user":
			messages = append(messages, convertUserMessage(msg.Content)...)
		case "assistant":
			messages = append(messages, convertAssistantMessage(msg.Content)...)
		}
	}

	out := map[string]any{
		"model":      upstreamModel,
		"messages":   messages,
		"max_tokens": req.MaxTokens,
	}

	if req.MaxTokens == 0 {
		out["max_tokens"] = 4096
	}

	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if len(req.StopSequences) > 0 {
		out["stop"] = req.StopSequences
	}

	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			name := t.Name
			desc := t.Description
			params := json.RawMessage(t.InputSchema)
			// Also accept OpenAI-format tools sent to /v1/messages:
			// {"type":"function","function":{...}}
			if t.Function != nil {
				if t.Function.Name != "" {
					name = t.Function.Name
				}
				if t.Function.Description != "" {
					desc = t.Function.Description
				}
				if len(t.Function.Parameters) > 0 && len(params) == 0 {
					params = t.Function.Parameters
				}
			}
			if strings.TrimSpace(name) == "" {
				continue // zen rejects empty tool names
			}
			tnames = append(tnames, name)
			// zen 400s when parameters is null/missing/non-object.
			var obj map[string]any
			if err := json.Unmarshal(params, &obj); err != nil || obj == nil {
				params = json.RawMessage(`{"type":"object"}`)
			}
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        name,
					"description": desc,
					"parameters":  params,
				},
			})
		}
		if len(tools) > 0 {
			out["tools"] = tools
		}
	}

	if req.ToolChoice != nil {
		out["tool_choice"] = convertToolChoice(req.ToolChoice)
	}

	if eff := thinkingToEffort(req.Thinking); eff != "" {
		out["reasoning_effort"] = eff
	}

	if isStream {
		out["stream"] = true
		out["stream_options"] = map[string]any{"include_usage": true}
	}

	ob, _ := json.Marshal(out)
	injectParams(&ob)

	return ob, clientModel, upstreamModel, tnames, isStream, nil
}

func extractSystemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []anthropicBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func convertUserMessage(raw json.RawMessage) []map[string]any {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []map[string]any{{"role": "user", "content": s}}
	}
	var blocks []anthropicBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return []map[string]any{{"role": "user", "content": ""}}
	}

	var result []map[string]any
	var textParts []string
	var imgParts []map[string]any
	var docParts []map[string]any

	for _, b := range blocks {
		switch b.Type {
		case "text":
			textParts = append(textParts, b.Text)
		case "tool_result":
			text, imgs := toolResultParts(b.Content)
			if b.IsError {
				text = "Error: " + text
			}
			result = append(result, map[string]any{
				"role":         "tool",
				"tool_call_id": b.ToolUseID,
				"content":      text,
			})
			// a tool_result can carry images (screenshot tools). role:"tool"
			// is text-only upstream, so the images become a trailing
			// multimodal user message instead of being pasted into the tool
			// content as raw json, which zen /chat/completions 400s on.
			if len(imgs) > 0 {
				result = append(result, map[string]any{
					"role":    "user",
					"content": contentOf(nil, imgs),
				})
			}
		case "image":
			if u := imageURL(b.Source); u != "" {
				imgParts = append(imgParts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
			}
		case "document":
			// text/plain documents are just text: inline them. every other
			// document type (pdf, docx...) has no openai chat/completions
			// equivalent, so it rides as a file part and falls back to a
			// data url, which is what multimodal providers accept.
			if p := documentPart(b); p != nil {
				docParts = append(docParts, p)
			}
		}
	}
	imgParts = append(imgParts, docParts...)

	if len(textParts) > 0 || len(imgParts) > 0 {
		result = append([]map[string]any{{"role": "user", "content": contentOf(textParts, imgParts)}}, result...)
	}

	if len(result) == 0 {
		return []map[string]any{{"role": "user", "content": ""}}
	}
	return result
}

func convertAssistantMessage(raw json.RawMessage) []map[string]any {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []map[string]any{{"role": "assistant", "content": s}}
	}
	var blocks []anthropicBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return []map[string]any{{"role": "assistant", "content": ""}}
	}

	var textParts []string
	var toolCalls []map[string]any
	var thinkingParts []string

	for _, b := range blocks {
		switch b.Type {
		case "text":
			textParts = append(textParts, b.Text)
		case "thinking", "redacted_thinking":
			// zen's interleaved-reasoning models (space-bunny, big-pickle)
			// expect prior reasoning back on assistant messages as
			// reasoning_content, matching opencode's transform. drop the
			// signature; it is opaque and zen does not require it.
			if b.Thinking != "" {
				thinkingParts = append(thinkingParts, b.Thinking)
			}
		case "tool_use":
			input := "{}"
			if len(b.Input) > 0 && string(b.Input) != "null" {
				input = string(b.Input)
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   b.ID,
				"type": "function",
				"function": map[string]any{
					"name":      b.Name,
					"arguments": input,
				},
			})
		}
	}

	msg := map[string]any{"role": "assistant"}
	if len(textParts) > 0 {
		msg["content"] = strings.Join(textParts, "\n")
	} else {
		msg["content"] = nil
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	if len(thinkingParts) > 0 {
		msg["reasoning_content"] = strings.Join(thinkingParts, "\n")
	}
	return []map[string]any{msg}
}

// imageURL converts an anthropic image source block to a url usable as an
// openai image_url part. returns "" when the source carries no usable data.
func imageURL(src *struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	URL       string `json:"url"`
}) string {
	if src == nil {
		return ""
	}
	mt := src.MediaType
	if mt == "" {
		mt = "image/jpeg"
	}
	// claude code sends base64 without an explicit type sometimes, so prefer
	// whichever field is populated rather than trusting src.Type.
	if src.Data != "" {
		return "data:" + mt + ";base64," + src.Data
	}
	return src.URL
}

// documentPart converts an anthropic document block into an openai content
// part. text-ish documents are inlined as text (universally understood);
// binary ones become a file part carrying a data url, and a bare url source
// stays a plain url. returns nil when there is nothing usable.
func documentPart(b anthropicBlock) map[string]any {
	// anthropic ships some documents as {"type":"text","data":"..."} with no
	// source, and some as source.type=="text".
	if b.Source == nil {
		if b.Type == "text" && b.Text != "" {
			return map[string]any{"type": "text", "text": b.Text}
		}
		return nil
	}
	s := b.Source
	mt := s.MediaType
	if mt == "" {
		mt = "application/octet-stream"
	}
	if s.Type == "text" || strings.HasPrefix(mt, "text/") {
		if s.Data == "" {
			return nil
		}
		if dec, err := base64.StdEncoding.DecodeString(s.Data); err == nil {
			return map[string]any{"type": "text", "text": string(dec)}
		}
		// already plain text, not base64
		return map[string]any{"type": "text", "text": s.Data}
	}
	if s.Data != "" {
		return map[string]any{
			"type": "file",
			"file": map[string]any{"filename": docName(mt), "file_data": "data:" + mt + ";base64," + s.Data},
		}
	}
	if s.URL != "" {
		return map[string]any{"type": "file", "file": map[string]any{"filename": docName(mt), "file_url": s.URL}}
	}
	return nil
}

// docName is a filename hint from a media type; zen ignores it but some
// providers reject a file part without one.
func docName(mt string) string {
	if i := strings.IndexByte(mt, '/'); i > 0 {
		return "attachment" + mt[i:]
	}
	return "attachment"
}

// contentOf builds a user message body: a plain string when there are no
// parts, a parts array once there are.
func contentOf(text []string, imgs []map[string]any) any {
	if len(imgs) == 0 {
		return strings.Join(text, "\n")
	}
	arr := make([]any, 0, len(text)+len(imgs))
	for _, t := range text {
		arr = append(arr, map[string]any{"type": "text", "text": t})
	}
	for _, im := range imgs {
		arr = append(arr, im)
	}
	return arr
}

// toolResultText flattens a tool_result body to the string a role:"tool"
// message can carry.
func toolResultText(raw json.RawMessage) string {
	text, _ := toolResultParts(raw)
	return text
}

// toolResultParts splits a tool_result body into its text and its images.
// text-only callers use toolResultText; images need a separate multimodal
// user message because role:"tool" cannot carry them.
func toolResultParts(raw json.RawMessage) (string, []map[string]any) {
	if len(raw) == 0 {
		return "", nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var blocks []anthropicBlock
	if json.Unmarshal(raw, &blocks) != nil {
		// not a known shape: pass it through as text, never as a half-parsed
		// structure. upstream sees a plain string either way.
		return string(raw), nil
	}
	var parts []string
	var imgs []map[string]any
	for _, b := range blocks {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "image":
			if u := imageURL(b.Source); u != "" {
				imgs = append(imgs, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
			}
		case "document":
			if p := documentPart(b); p != nil {
				imgs = append(imgs, p)
			}
		}
	}
	return strings.Join(parts, "\n"), imgs
}

func convertToolChoice(tc any) any {
	switch v := tc.(type) {
	case string:
		switch v {
		case "auto", "none":
			return v
		case "any":
			return "required"
		default:
			return v
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		switch typ {
		case "auto", "none":
			return typ
		case "any":
			return "required"
		case "tool":
			name, _ := v["name"].(string)
			if name != "" {
				return map[string]any{
					"type":     "function",
					"function": map[string]any{"name": name},
				}
			}
			return "auto"
		}
	}
	return "auto"
}

// --- Response Conversion (non-stream) ---

func openAIToAnthropic(body []byte, clientModel string, tnames toolNames) (out []byte, errMsg string, msgID string) {
	msgID = "msg_" + randHex(24)

	var oaiResp struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content          *string `json:"content"`
				ReasoningContent *string `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}

	if err := json.Unmarshal(body, &oaiResp); err != nil {
		return nil, "failed to parse upstream response: " + err.Error(), msgID
	}

	if oaiResp.Error != nil {
		return nil, oaiResp.Error.Message, msgID
	}

	content := make([]map[string]any, 0)

	if oaiResp.Choices != nil && len(oaiResp.Choices) > 0 {
		msg := oaiResp.Choices[0].Message

		text := ""
		if msg.Content != nil && *msg.Content != "" {
			text = *msg.Content
		} else if msg.ReasoningContent != nil && *msg.ReasoningContent != "" {
			text = *msg.ReasoningContent
		}
		text = stripWaitSentinel(text)
		if text != "" {
			content = append(content, map[string]any{"type": "text", "text": text})
		}

		for _, tc := range msg.ToolCalls {
			input := map[string]any{}
			if tc.Function.Arguments != "" {
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &input); err != nil {
					input = map[string]any{"_raw": tc.Function.Arguments}
				}
			}
			content = append(content, map[string]any{
				"type":  "tool_use",
				"id":    tc.ID,
				"name":  tnames.resolve(tc.Function.Name),
				"input": input,
			})
		}
	}

	if len(content) == 0 {
		content = []map[string]any{{"type": "text", "text": ""}}
	}

	stopReason := "end_turn"
	if oaiResp.Choices != nil && len(oaiResp.Choices) > 0 && oaiResp.Choices[0].FinishReason != nil {
		switch *oaiResp.Choices[0].FinishReason {
		case "stop":
			stopReason = "end_turn"
		case "length":
			stopReason = "max_tokens"
		case "tool_calls":
			stopReason = "tool_use"
		}
	}

	inputTokens, outputTokens := 0, 0
	if oaiResp.Usage != nil {
		inputTokens = oaiResp.Usage.PromptTokens
		outputTokens = oaiResp.Usage.CompletionTokens
	}

	result := map[string]any{
		"id":            msgID,
		"type":          "message",
		"role":          "assistant",
		"model":         clientModel,
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  inputTokens,
			"output_tokens": outputTokens,
		},
	}

	out, err := json.Marshal(result)
	if err != nil {
		return nil, "failed to marshal response: " + err.Error(), msgID
	}
	return out, "", msgID
}

func streamAnthropic(w http.ResponseWriter, body []byte, clientModel string, tnames toolNames) (written int64, promptT, compT int) {
	fl, _ := w.(http.Flusher)

	// proxy-internal marker must never reach the client.
	body = stripSentinelSSE(body)

	msgID := "msg_" + randHex(24)
	preamble := fmt.Sprintf(`{"type":"message_start","message":{"id":"%s","type":"message","role":"assistant","model":"%s","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0}}}`, msgID, clientModel)
	written += writeSSE(w, "message_start", preamble)
	written += writeSSE(w, "content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	written += writeSSE(w, "ping", `{"type":"ping"}`)
	if fl != nil {
		fl.Flush()
	}

	textClosed := false
	hasToolCalls := false
	toolIndex := -1
	lastToolIdx := 0
	hasSentStopReason := false
	var strip sentinelStripper
	flushStrip := func() {
		if tail := strip.flush(); tail != "" && !hasToolCalls && !textClosed {
			written += writeSSE(w, "content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%s}}`, jsonStr(tail)))
		}
	}

	lines := strings.Split(string(body), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}

		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}

		if chunk.Usage != nil {
			if chunk.Usage.PromptTokens > 0 {
				promptT = chunk.Usage.PromptTokens
			}
			if chunk.Usage.CompletionTokens > 0 {
				compT = chunk.Usage.CompletionTokens
			}
		}

		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]

		// text delta
		if choice.Delta.Content != "" && !hasToolCalls && !textClosed {
			if clean := strip.push(choice.Delta.Content); clean != "" {
				written += writeSSE(w, "content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%s}}`, jsonStr(clean)))
			}
		}

		// tool call deltas
		if len(choice.Delta.ToolCalls) > 0 {
			if !hasToolCalls {
				hasToolCalls = true
				if !textClosed {
					written += writeSSE(w, "content_block_stop", `{"type":"content_block_stop","index":0}`)
					textClosed = true
				}
			}

			for _, tc := range choice.Delta.ToolCalls {
				if toolIndex < 0 || tc.Index != toolIndex {
					toolIndex = tc.Index
					lastToolIdx++
					toolID := tc.ID
					if toolID == "" {
						toolID = "toolu_" + randHex(24)
					}
					written += writeSSE(w, "content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%s,"name":%s,"input":{}}}`, lastToolIdx, jsonStr(toolID), jsonStr(tnames.resolve(tc.Function.Name))))
				}
				if tc.Function.Arguments != "" {
					written += writeSSE(w, "content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%s}}`, lastToolIdx, jsonStr(tc.Function.Arguments)))
				}
			}
		}

		// finish reason
		if choice.FinishReason != nil && !hasSentStopReason {
			hasSentStopReason = true
			flushStrip()

			for i := 1; i <= lastToolIdx; i++ {
				written += writeSSE(w, "content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, i))
			}

			if !textClosed {
				written += writeSSE(w, "content_block_stop", `{"type":"content_block_stop","index":0}`)
				textClosed = true
			}

			stopReason := "end_turn"
			switch *choice.FinishReason {
			case "length":
				stopReason = "max_tokens"
			case "tool_calls":
				stopReason = "tool_use"
			}

			written += writeSSE(w, "message_delta", fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":"%s","stop_sequence":null},"usage":{"output_tokens":%d}}`, stopReason, compT))
			written += writeSSE(w, "message_stop", `{"type":"message_stop"}`)
			n, _ := w.Write([]byte("data: [DONE]\n\n"))
			written += int64(n)
			if fl != nil {
				fl.Flush()
			}
			return
		}
	}

	// stream ended without finish_reason
	if !hasSentStopReason {
		flushStrip()
		for i := 1; i <= lastToolIdx; i++ {
			written += writeSSE(w, "content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, i))
		}
		if !textClosed {
			written += writeSSE(w, "content_block_stop", `{"type":"content_block_stop","index":0}`)
		}
		written += writeSSE(w, "message_delta", fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":%d}}`, compT))
		written += writeSSE(w, "message_stop", `{"type":"message_stop"}`)
		n, _ := w.Write([]byte("data: [DONE]\n\n"))
		written += int64(n)
		if fl != nil {
			fl.Flush()
		}
	}

	return
}

// respStreamer carries per-turn state so several buffered /responses SSE
// bodies can be emitted as one continuous Anthropic turn.
type respStreamer struct {
	w           http.ResponseWriter
	fl          http.Flusher
	hasFlusher  bool
	clientModel string
	tnames      toolNames
	written     int64
	promptT     int
	compT       int
	blockIdx    int
	textIdx     int
	textOpen    bool
	strip       sentinelStripper
	toolIdx     map[string]int
	hasToolUse  bool
	sentStop    bool
}

func (s *respStreamer) flush() {
	if s.hasFlusher {
		s.fl.Flush()
	}
}

func (s *respStreamer) openText() {
	if s.textOpen {
		return
	}
	s.textIdx = s.blockIdx
	s.blockIdx++
	s.textOpen = true
	s.written += writeSSE(s.w, "content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`, s.textIdx))
}

func (s *respStreamer) closeText() {
	if !s.textOpen {
		return
	}
	s.textOpen = false
	if tail := s.strip.flush(); tail != "" {
		s.written += writeSSE(s.w, "content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%s}}`, s.textIdx, jsonStr(tail)))
	}
	s.written += writeSSE(s.w, "content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, s.textIdx))
}

func (s *respStreamer) openTool(id, callID, name string) {
	if _, ok := s.toolIdx[id]; ok {
		return
	}
	s.toolIdx[id] = s.blockIdx
	s.blockIdx++
	s.hasToolUse = true
	s.written += writeSSE(s.w, "content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%s,"name":%s,"input":{}}}`, s.toolIdx[id], jsonStr(callID), jsonStr(s.tnames.resolve(name))))
}

func (s *respStreamer) closeTool(id string) {
	idx, ok := s.toolIdx[id]
	if !ok {
		return
	}
	s.written += writeSSE(s.w, "content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, idx))
	delete(s.toolIdx, id)
}

// closeBlocks ends open blocks without ending the turn, so another
// buffered body can continue on fresh indices.
func (s *respStreamer) closeBlocks() {
	s.closeText()
	for id := range s.toolIdx {
		s.closeTool(id)
	}
}

func (s *respStreamer) finish() {
	if s.sentStop {
		return
	}
	s.sentStop = true
	s.closeBlocks()
	stopReason := "end_turn"
	if s.hasToolUse {
		stopReason = "tool_use"
	}
	s.written += writeSSE(s.w, "message_delta", fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%s,"stop_sequence":null},"usage":{"output_tokens":%d}}`, jsonStr(stopReason), s.compT))
	s.written += writeSSE(s.w, "message_stop", `{"type":"message_stop"}`)
	n, _ := s.w.Write([]byte("data: [DONE]\n\n"))
	s.written += int64(n)
	s.flush()
}

func (s *respStreamer) feed(body []byte, last bool) {
	// proxy-internal marker must never reach the client.
	body = stripSentinelSSE(body)
	lines := strings.Split(string(body), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var evt struct {
			Type      string `json:"type"`
			Delta     string `json:"delta"`
			Arguments string `json:"arguments"`
			ItemID    string `json:"item_id"`
			Item      *struct {
				ID        string `json:"id"`
				Type      string `json:"type"`
				CallID    string `json:"call_id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"item"`
			Response *struct {
				Usage *struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
					TotalTokens  int `json:"total_tokens"`
				} `json:"usage"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(data), &evt); err != nil {
			continue
		}

		switch evt.Type {
		case "response.output_item.added":
			if evt.Item != nil && evt.Item.Type == "function_call" {
				id := evt.ItemID
				if id == "" {
					id = evt.Item.ID
				}
				s.openTool(id, evt.Item.CallID, evt.Item.Name)
			}
		case "response.function_call_arguments.delta":
			if _, ok := s.toolIdx[evt.ItemID]; ok && evt.Delta != "" {
				s.written += writeSSE(s.w, "content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%s}}`, s.toolIdx[evt.ItemID], jsonStr(evt.Delta)))
			}
		case "response.output_item.done":
			if evt.Item != nil && evt.Item.Type == "function_call" {
				s.closeTool(evt.ItemID)
			}
		case "response.output_text.delta":
			s.openText()
			if evt.Delta != "" {
				if clean := s.strip.push(evt.Delta); clean != "" {
					s.written += writeSSE(s.w, "content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%s}}`, s.textIdx, jsonStr(clean)))
				}
			}
		case "response.output_text.done", "response.content_part.done":
			s.closeText()
		case "response.completed":
			if evt.Response != nil && evt.Response.Usage != nil {
				s.compT += evt.Response.Usage.OutputTokens
				s.promptT = evt.Response.Usage.InputTokens
			}
			if last {
				s.finish()
				return
			}
			s.closeBlocks()
		}
	}
}

// streamResponsesMerged emits several buffered /responses SSE bodies as one
// Anthropic turn: one preamble, continuous block indices, one stop.
func streamResponsesMerged(w http.ResponseWriter, bodies [][]byte, clientModel string, tnames toolNames) (written int64, promptT, compT int) {
	fl, _ := w.(http.Flusher)
	s := &respStreamer{w: w, clientModel: clientModel, tnames: tnames, toolIdx: map[string]int{}, textIdx: -1}
	if fl != nil {
		s.fl = fl
		s.hasFlusher = true
	}

	msgID := "msg_" + randHex(24)
	preamble := fmt.Sprintf(`{"type":"message_start","message":{"id":"%s","type":"message","role":"assistant","model":"%s","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0}}}`, msgID, clientModel)
	s.written += writeSSE(w, "message_start", preamble)
	s.written += writeSSE(w, "ping", `{"type":"ping"}`)
	s.flush()

	for i, b := range bodies {
		s.feed(b, i == len(bodies)-1)
	}
	s.finish()
	return s.written, s.promptT, s.compT
}

// toolsOffered reports whether a /responses-shaped request body offers any
// non-empty-named function tools.
func toolsOffered(body []byte) bool {
	var m struct {
		Tools []struct {
			Type     string `json:"type"`
			Name     string `json:"name"`
			Function *struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return false
	}
	for _, t := range m.Tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		name := t.Name
		if t.Function != nil {
			name = t.Function.Name
		}
		if strings.TrimSpace(name) != "" {
			return true
		}
	}
	return false
}

// endsWithQuestion reports whether text ends with a question, so genuine
// user-directed questions are not nudged into assuming an answer.
func endsWithQuestion(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return false
	}
	// check the last sentence: walk back past trailing quotes/parens/space
	end := len(t)
	for end > 0 {
		c := t[end-1]
		if c == '"' || c == '\'' || c == ')' || c == ']' || c == ' ' || c == '\n' || c == '\t' {
			end--
			continue
		}
		break
	}
	if end > 0 && t[end-1] == '?' {
		return true
	}
	// also catch a question followed by a short tail ("Two options:") or an
	// option list ("Which one?\n\n1. foo\n2. bar"). err toward treating it
	// as a question: skipping a nudge is benign, nudging past a real
	// question fabricates user consent.
	tail := t[max(0, len(t)-200):]
	if qi := strings.LastIndex(tail, "?"); qi >= 0 {
		after := strings.TrimSpace(tail[qi+1:])
		if len(after) <= 120 {
			return true
		}
		for _, line := range strings.Split(after, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			c := line[0]
			if (c >= '0' && c <= '9') || c == '-' || c == '*' || c == '(' {
				return true
			}
		}
	}
	return false
}

// waitSentinel is what the model is told to emit when it is waiting on
// the user or done. A retry body holding only this is swallowed so the
// turn ends on the first body's output instead of a redundant summary.
const waitSentinel = "SPARK_WAITING_FOR_INPUT"

// nudgeContinuation builds a follow-up /responses body: the prior input plus
// the assistant's text and a nudge to use the offered tools. Tool calls
// always win; if the model is waiting on the user or done, it must emit
// only waitSentinel so the retry can be swallowed.
func nudgeContinuation(oaiBody []byte, priorText string) []byte {
	var m map[string]any
	if err := json.Unmarshal(oaiBody, &m); err != nil {
		return nil
	}
	in, _ := m["input"].([]any)
	in = append(in, map[string]any{"role": "assistant", "content": priorText})
	in = append(in, map[string]any{
		"role": "user",
		"content": "You have tools available in this conversation. If any tool applies to the task above, call it now instead of describing what you would do. " +
			"Do not narrate progress or restate what you already wrote. " +
			"If you are waiting on the user to answer, or the task is fully done, reply with exactly " + waitSentinel + " and nothing else. " +
			"Do not answer your own questions or assume the user's reply.",
	})
	m["input"] = in
	delete(m, "nudge_no_tools")
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return b
}

// isWaitOnly reports whether a retry body holds only the wait sentinel and
// no function calls, meaning it should be swallowed.
func isWaitOnly(body []byte) bool {
	text, hasCalls := scanResponsesSSE(body)
	if hasCalls {
		return false
	}
	return strings.TrimSpace(text) == waitSentinel
}

// stripWaitSentinel removes the internal wait sentinel so it never reaches
// the client. the model sometimes echoes it with extra words attached.
func stripWaitSentinel(s string) string {
	return strings.ReplaceAll(s, waitSentinel, "")
}

// stripSentinelSSE removes the internal wait sentinel from a buffered
// /responses SSE body. belt and suspenders behind the swallow-non-calls
// rule: the marker is proxy-internal and must never reach the client.
func stripSentinelSSE(body []byte) []byte {
	if !strings.Contains(string(body), waitSentinel) {
		return body
	}
	return []byte(strings.ReplaceAll(string(body), waitSentinel, ""))
}

// sentinelStripper removes waitSentinel from a stream of text deltas. only
// the trailing overlap with a sentinel prefix is held back, so ordinary
// text flows through unbuffered while a marker split across deltas is
// still caught; flush emits whatever is left at block close.
type sentinelStripper struct {
	pending string
}

func (s *sentinelStripper) push(d string) string {
	s.pending += d
	cleaned := strings.ReplaceAll(s.pending, waitSentinel, "")
	hold := 0
	max := len(cleaned)
	if max > len(waitSentinel)-1 {
		max = len(waitSentinel) - 1
	}
	for n := max; n > 0; n-- {
		if strings.HasPrefix(waitSentinel, cleaned[len(cleaned)-n:]) {
			hold = n
			break
		}
	}
	out := cleaned[:len(cleaned)-hold]
	s.pending = cleaned[len(cleaned)-hold:]
	return out
}

func (s *sentinelStripper) flush() string {
	out := strings.ReplaceAll(s.pending, waitSentinel, "")
	s.pending = ""
	return out
}

// scanResponsesSSE returns the concatenated output text and whether any
// function_call output item appeared in a buffered /responses SSE body.
// it covers delta events plus the done/completed shapes spark actually
// sends when text arrives without deltas or calls complete in one item.
func scanResponsesSSE(body []byte) (text string, hasCalls bool) {
	var sb strings.Builder
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		var evt struct {
			Type     string         `json:"type"`
			Delta    string         `json:"delta"`
			Text     string         `json:"text"`
			Item     map[string]any `json:"item"`
			Response *struct {
				Output []map[string]any `json:"output"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(data), &evt); err != nil {
			continue
		}
		switch evt.Type {
		case "response.output_text.delta":
			sb.WriteString(evt.Delta)
		case "response.output_text.done", "response.content_part.done":
			sb.WriteString(evt.Text)
		case "response.output_item.added", "response.output_item.done":
			t, _ := evt.Item["type"].(string)
			if t == "function_call" {
				hasCalls = true
			} else if t == "message" {
				sb.WriteString(responseItemText(evt.Item))
			}
		case "response.completed", "response.incomplete", "response.failed":
			if evt.Response != nil {
				for _, it := range evt.Response.Output {
					t, _ := it["type"].(string)
					if t == "function_call" {
						hasCalls = true
					} else if t == "message" {
						sb.WriteString(responseItemText(it))
					}
				}
			}
		}
	}
	return sb.String(), hasCalls
}

// responseItemText pulls output_text out of a /responses message item.
func responseItemText(item map[string]any) string {
	var sb strings.Builder
	content, _ := item["content"].([]any)
	for _, c := range content {
		cm, _ := c.(map[string]any)
		if cm == nil {
			continue
		}
		t, _ := cm["type"].(string)
		if t == "output_text" || t == "text" {
			s, _ := cm["text"].(string)
			sb.WriteString(s)
		}
	}
	return sb.String()
}

// tryNudgeResponses runs the no-tools continue: gates on the master switch,
// the per-model flag, offered tools, and response text, then posts the
// continuation via post. returns the retry body, or nil when the nudge
// doesn't fire (or the retry is wait-only) so callers keep the original.
// callers merge: stream sites append the retry as a second body,
// buffered sites fold mergeResponsesSSE([rb, retry]) before converting.
func tryNudgeResponses(clientModel string, posted, rb []byte, post func(nb []byte) []byte) []byte {
	if !nudgeEnabled(clientModel) {
		dbglog.Printf("  nudge skip model=%s reason=disabled", clientModel)
		return nil
	}
	if !toolsOffered(posted) {
		dbglog.Printf("  nudge skip model=%s reason=no-tools-offered", clientModel)
		return nil
	}
	text, hasCalls := scanResponsesSSE(rb)
	if hasCalls {
		return nil
	}
	if strings.Contains(text, waitSentinel) {
		return nil
	}
	if strings.TrimSpace(text) == "" {
		dbglog.Printf("  nudge skip model=%s reason=empty-text", clientModel)
		return nil
	}
	// no question exemption: the reprompt tells the model to emit
	// only waitSentinel when waiting, so nudging past a question
	// is safe. exempting questions let greetings loop forever
	// with no tool calls and no nudge ever firing.
	nb := nudgeContinuation(posted, text)
	if nb == nil {
		return nil
	}
	nb2 := post(nb)
	if nb2 == nil {
		return nil
	}
	text2, nHasCalls := scanResponsesSSE(nb2)
	if !nHasCalls {
		// retry called nothing: swallowing keeps the turn to one
		// assistant message so the agent acts instead of narrating
		// twice. covers wait-only plus any tool-less text (which may
		// carry the wait sentinel with extra words attached).
		if isWaitOnly(nb2) {
			acclog.Printf("  opencode nudge model=%s waiting, swallowing retry", clientModel)
		} else {
			acclog.Printf("  opencode nudge model=%s no-calls, swallowing retry text=%q", clientModel, errSnippet([]byte(text2), 120))
		}
		return nil
	}
	acclog.Printf("  opencode nudge model=%s calls=%v bytes=%d", clientModel, nHasCalls, len(nb2))
	return nb2
}

// --- Handlers ---

// handleClassifier answers Claude Code's permission-classifier endpoint.
// Claude Code POSTs a permission_request to {base}/v1/messages/classifier when a
// tool needs approval; the gateway returns a decision. We always approve so the
// session never blocks on an interactive prompt. The request body is ignored.
func handleClassifier(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only POST is supported")
		return
	}
	json.NewEncoder(w).Encode(map[string]any{
		"result": "decision",
		"decision": map[string]any{
			"allow":  true,
			"reason": "auto-approved by proxy",
		},
	})
}

func (p *Pool) handleAnthropic(w http.ResponseWriter, r *http.Request, start time.Time, reqID string) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "*")
	w.Header().Set("Access-Control-Allow-Methods", "*")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only POST is supported")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, bodyLimit))
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	r.Body.Close()

	// RTK: compress tool_result text before the PII guard masks, so filters
	// see original bytes and vault placeholders are never chopped.
	body = rtkCompressBody(body)

	// same deal as ServeHTTP: mask the inbound body, unmask on the way out.
	injectDiscloseLine(&body)
	pg := guardForRequest(r)
	body = pg.MaskBody(body)
	if dw := wrapPII(w, pg); dw != nil {
		w = dw
		defer dw.finish()
	}

	if r.URL.Path == "/v1/messages/count_tokens" {
		handleCountTokens(w, body)
		return
	}

	if used, limit := p.semStats(); used >= limit {
		acclog.Printf("%s ... all %d slots busy, queueing", reqID, limit)
	}
	p.semAcquire()
	p.concurrent.Add(1)
	defer func() {
		p.concurrent.Add(-1)
		p.semRelease()
	}()

	oaiBody, clientModel, upstreamModel, tnames, isStream, err := anthropicRequestToOpenAI(body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	acclog.Printf("%s -> POST /v1/messages model=%s upstream=%s stream=%v bytes=%d", reqID, clientModel, upstreamModel, isStream, len(oaiBody))

	// Route opencode/* models to zen endpoint
	if strings.HasPrefix(upstreamModel, "opencode/") {
		if !zenEnabled() {
			acclog.Printf("<- 503 POST /v1/messages model=%s (zen disabled)", clientModel)
			writeAnthropicError(w, http.StatusServiceUnavailable, "api_error", "opencode zen is disabled; set zen.enabled: true in config.yml")
			return
		}
		p.concurrentZen.Add(1)
		defer p.concurrentZen.Add(-1)
		acclog.Printf("%s routed to opencode model=%s", reqID, upstreamModel)
		p.handleOpenCodeAnthropic(w, r, body, oaiBody, clientModel, upstreamModel, tnames, isStream, start, reqID)
		return
	}

	// Route freeepi/* models to api.freepi.ai
	if strings.HasPrefix(upstreamModel, "freeepi/") {
		if !freepiEnabled() {
			acclog.Printf("<- 503 POST /v1/messages model=%s (freepi disabled)", clientModel)
			writeAnthropicError(w, http.StatusServiceUnavailable, "api_error", "freepi is disabled; set freepi.enabled: true with accounts in config.yml")
			return
		}
		acclog.Printf("%s routed to freepi model=%s", reqID, upstreamModel)
		p.handleFreepiAnthropic(w, r, oaiBody, clientModel, upstreamModel, tnames, isStream, start, reqID)
		return
	}

	// nvidia/ prefix is the listed form; bare ids stay nvidia for compat.
	if strings.HasPrefix(upstreamModel, "nvidia/") {
		upstreamModel = strings.TrimPrefix(upstreamModel, "nvidia/")
		if b, err := setReqModel(oaiBody, upstreamModel); err == nil {
			oaiBody = b
		}
	}

	// disabled or keyless mode serves opencode/* only; nim models need keys.
	if !cfg().Nvidia.Enabled {
		acclog.Printf("<- 503 POST /v1/messages model=%s (nvidia disabled)", clientModel)
		writeAnthropicError(w, http.StatusServiceUnavailable, "api_error", "nvidia backend is disabled; set nvidia.enabled: true in config.yml")
		return
	}
	if len(p.keys) == 0 {
		acclog.Printf("<- 503 POST /v1/messages model=%s (keyless: no nvidia keys)", clientModel)
		writeAnthropicError(w, http.StatusServiceUnavailable, "api_error", "no nvidia keys configured; use an opencode/<model> free model or add nvidia_keys to config.yml")
		return
	}

	target := nvidiaBase + "/chat/completions"
	acclog.Printf("%s routed to nvidia target=%s model=%s", reqID, target, upstreamModel)
	p.concurrentNV.Add(1)
	defer p.concurrentNV.Add(-1)
	fwd := http.Header{
		"Content-Type": {"application/json"},
		"Accept":       {"application/json"},
	}
	if isStream {
		fwd.Set("Accept", "text/event-stream")
	}

	u, upErr := p.callUpstream(r.Method, target, oaiBody, fwd, upstreamModel, r.RemoteAddr)
	elapsed := time.Since(start)

	if u == nil {
		errMsg := "all keys exhausted or on cooldown"
		if upErr != nil {
			errMsg = upErr.Error()
		}
		logUsage(UsageRecord{
			Ts:         time.Now().UTC().Format(time.RFC3339Nano),
			Model:      clientModel,
			Method:     "POST",
			Path:       "/v1/messages",
			StatusCode: 503,
			DurationMs: elapsed.Milliseconds(),
			Stream:     isStream,
			Error:      errMsg,
		})
		writeAnthropicError(w, http.StatusServiceUnavailable, "api_error", errMsg)
		return
	}

	if u.StatusCode != http.StatusOK {
		errMsg := extractErrMessage(string(u.Body))
		acclog.Printf("!! upstream %d [%s] model=%s err=%q", u.StatusCode, u.Key, clientModel, errMsg)

		logUsage(UsageRecord{
			Ts:          time.Now().UTC().Format(time.RFC3339Nano),
			Model:       clientModel,
			KeyName:     u.Key,
			Method:      "POST",
			Path:        "/v1/messages",
			StatusCode:  u.StatusCode,
			DurationMs:  elapsed.Milliseconds(),
			Stream:      isStream,
			Error:       errMsg,
			RateLimited: u.RateLimited,
		})

		writeAnthropicError(w, u.StatusCode, "api_error", errMsg)
		return
	}

	if isStream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		written, promptT, compT := streamAnthropic(w, u.Body, clientModel, tnames)

		p.Postpone(u.KeyObj, 1500*time.Millisecond)
		p.Clear429(u.KeyObj)

		logUsage(UsageRecord{
			Ts:               time.Now().UTC().Format(time.RFC3339Nano),
			Model:            clientModel,
			KeyName:          u.Key,
			Method:           "POST",
			Path:             "/v1/messages",
			StatusCode:       200,
			DurationMs:       elapsed.Milliseconds(),
			PromptTokens:     promptT,
			CompletionTokens: compT,
			TotalTokens:      promptT + compT,
			Stream:           true,
			RetryAttempt:     u.Retries,
			RateLimited:      u.RateLimited,
			ContentBytes:     int64(len(body)),
		})

		acclog.Printf("<- 200 POST /v1/messages %v %d bytes [%s]", elapsed.Round(time.Millisecond), written, u.Key)
	} else {
		out, errMsg, _ := openAIToAnthropic(u.Body, clientModel, tnames)
		if errMsg != "" {
			writeAnthropicError(w, http.StatusInternalServerError, "api_error", errMsg)
			return
		}

		prompT, compT, _ := respTokens(u.Body)

		p.Postpone(u.KeyObj, 1500*time.Millisecond)
		p.Clear429(u.KeyObj)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(out)

		logUsage(UsageRecord{
			Ts:               time.Now().UTC().Format(time.RFC3339Nano),
			Model:            clientModel,
			KeyName:          u.Key,
			Method:           "POST",
			Path:             "/v1/messages",
			StatusCode:       200,
			DurationMs:       elapsed.Milliseconds(),
			PromptTokens:     prompT,
			CompletionTokens: compT,
			TotalTokens:      prompT + compT,
			Stream:           false,
			RetryAttempt:     u.Retries,
			RateLimited:      u.RateLimited,
			ContentBytes:     int64(len(body)),
		})

		acclog.Printf("<- 200 POST /v1/messages %v %d bytes [%s]", elapsed.Round(time.Millisecond), len(out), u.Key)
	}
}

func handleCountTokens(w http.ResponseWriter, body []byte) {
	tokens := estimateTokens(body)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"input_tokens": tokens})
}

// --- Helpers ---

func writeSSE(w io.Writer, event, data string) int64 {
	var n int
	if event != "" {
		n, _ = fmt.Fprintf(w, "event: %s\n", event)
	}
	m, _ := fmt.Fprintf(w, "data: %s\n\n", data)
	return int64(n + m)
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func writeAnthropicError(w http.ResponseWriter, status int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]string{
			"type":    errType,
			"message": msg,
		},
	})
}

func extractErrMessage(body string) string {
	var errResp struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(body), &errResp) == nil && errResp.Error.Message != "" {
		return errResp.Error.Message
	}
	var errStr struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(body), &errStr) == nil && errStr.Error != "" {
		return errStr.Error
	}
	msg := strings.TrimSpace(body)
	if msg == "" {
		msg = "upstream error with empty body"
	}
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}

func randHex(n int) string {
	b := make([]byte, n/2+1)
	rand.Read(b)
	return fmt.Sprintf("%x", b)[:n]
}

const b62chars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

func randBase62(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	out := make([]byte, n)
	for i := range b {
		out[i] = b62chars[int(b[i])%62]
	}
	return string(out)
}

// zenID mirrors packages/schema/src/identifier.ts: timestamp ms * 0x1000 +
// per-ms counter, 6 bytes big-endian hex (complemented when descending),
// plus 14 base62 random chars from crypto bytes % 62.
var (
	zenIDMu      sync.Mutex
	zenIDLastTs  int64
	zenIDCounter int64
	zenIDMask    int64 = 0xFFFFFFFFFFFF
)

func zenID(descending bool) string {
	ts := time.Now().UnixMilli()
	zenIDMu.Lock()
	if ts != zenIDLastTs {
		zenIDLastTs = ts
		zenIDCounter = 0
	}
	zenIDCounter++
	current := ts*0x1000 + zenIDCounter
	masked := current & zenIDMask
	if descending {
		masked = (^masked) & zenIDMask
	}
	zenIDMu.Unlock()
	timeHex := fmt.Sprintf("%012x", masked)
	// crypto random, same charset/modulo as identifier.ts
	rb := make([]byte, 14)
	rand.Read(rb)
	rb62 := make([]byte, 14)
	for i := range rb {
		rb62[i] = b62chars[int(rb[i])%62]
	}
	return timeHex + string(rb62)
}

// zenSession mirrors opencode's session id: ses_ + descending().
// (packages/schema/src/session-id.ts)
func zenSession() string {
	return "ses_" + zenID(true)
}

// zenMsgID mirrors opencode's per-request message id: msg_ + ascending().
// (packages/schema/src/session-message.ts, sent as x-opencode-request)
func zenMsgID() string {
	return "msg_" + zenID(false)
}

// setZenHeaders applies the exact header set the real opencode client sends to
// zen so anonymous free-tier requests pass the "used from within OpenCode" check.
// real client (session/llm/request.ts): bare `opencode/${InstallationVersion}`
// with no ai-sdk/bun suffix, x-opencode-request = user message id.
func setZenHeaders(req *http.Request, sessionID string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer public")
	req.Header.Set("x-opencode-client", "cli")
	req.Header.Set("x-opencode-session", sessionID)
	req.Header.Set("x-opencode-request", zenMsgID())
	req.Header.Set("x-opencode-project", "global")
	req.Header.Set("User-Agent", "opencode/"+zenVersionStr())
	req.Header.Set("Accept", "*/*")
	// opencode's @ai-sdk/anthropic client sends this on every zen request.
	// the anonymous free tier fingerprints it, so omitting it can surface as
	// FreeTierError ("can only be used from within OpenCode").
	req.Header.Set("anthropic-version", "2023-06-01")
}

func estimateTokens(body []byte) int {
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return 1000
	}

	chars := 0
	if len(req.System) > 0 {
		chars += len(req.System)
	}
	for _, msg := range req.Messages {
		chars += len(msg.Content)
	}
	for _, tool := range req.Tools {
		chars += len(tool.Name) + len(tool.Description) + len(tool.InputSchema)
	}

	tokens := chars / 4
	if tokens < 10 {
		tokens = 10
	}
	return tokens
}

// handleOpenCodeAnthropic routes opencode/* models through the zen endpoint.
func (p *Pool) handleOpenCodeAnthropic(w http.ResponseWriter, r *http.Request, origBody, oaiBody []byte, clientModel, upstreamModel string, tnames toolNames, isStream bool, start time.Time, reqID string) {
	zenModel := strings.TrimPrefix(upstreamModel, "opencode/")
	endpoint := endpointForModel(zenModel)
	isResponses := endpoint == "/responses"
	acclog.Printf("%s routed to opencode model=%s endpoint=%s", reqID, zenModel, endpoint)

	var m map[string]any
	if json.Unmarshal(oaiBody, &m) == nil {
		m["model"] = zenModel
		stripCacheFields(m)
		ensureZenTools(m)
		if !isStream {
			m["stream"] = true
			m["stream_options"] = map[string]any{"include_usage": true}
		}
		if isResponses {
			convertToResponses(m)
		} else {
			sanitizeZenChatMessages(m)
		}
		if b, err := json.Marshal(m); err == nil {
			oaiBody = b
		}
	}

	target := OpencodeBase + endpointForModel(zenModel)
	var resp *http.Response

	if os.Getenv("ZEN_DUMP") != "" {
		t := time.Now().UnixNano()
		p := fmt.Sprintf("/tmp/zenreq_%d.json", t)
		os.WriteFile(p, oaiBody, 0o644)
		acclog.Printf("ZEN_DUMP request %d bytes -> %s", len(oaiBody), p)
		pi := fmt.Sprintf("/tmp/zenin_%d.json", t)
		os.WriteFile(pi, origBody, 0o644)
		acclog.Printf("ZEN_DUMP inbound %d bytes -> %s", len(origBody), pi)
	}

	lane := laneFor(r.Header.Get("x-session-id"), oaiBody)
	sessionID := laneSession(lane)

	// zenPost sends body to zen with proxy failover and returns the response.
	zenPost := func(body []byte, stream bool, tag string) *http.Response {
		var cl *http.Client
		lastProxy := ""
		rotating := true
		for zenRetries := 0; zenRetries < zenMaxRetries(); zenRetries++ {
			if !laneHealthy(lane) {
				if nl := laneFailover(lane); nl != nil {
					lane = nl
					sessionID = laneSession(lane)
				} else if zenRetries > 0 {
					if maybeRefreshStalePool() {
						sessionID = laneSession(lane)
					} else if wait := laneWait(); wait > 0 {
						acclog.Printf("  opencode all lanes cooling, waiting %v (retry %d/%d)", wait, zenRetries, zenRetryLabel())
						time.Sleep(wait)
					}
				}
			} else {
				laneGate(lane)
			}
			proxy := ""
			if zenRetries == 0 {
				proxy = laneProxyFor(lane, zenModel)
				if proxy == "" && cfg().Zen.AlwaysProxy {
					if nl := laneFailover(lane); nl != nil {
						lane = nl
						sessionID = laneSession(lane)
						proxy = laneProxyFor(lane, zenModel)
					}
					if proxy == "" {
						return nil
					}
				}
				cl = zenClient(proxy)
				lastProxy = proxy
			} else if rotating {
				proxy = laneProxyFor(lane, zenModel)
				if proxy == "" && cfg().Zen.AlwaysProxy {
					if nl := laneFailover(lane); nl != nil {
						lane = nl
						sessionID = laneSession(lane)
						proxy = laneProxyFor(lane, zenModel)
					}
				}
				if proxy == "" && cfg().Zen.AlwaysProxy {
					continue
				}
				cl = zenClient(proxy)
				lastProxy = proxy
			} else {
				// service overloaded: retry on the same lane proxy + session.
				proxy = lastProxy
				cl = zenClient(proxy)
			}
			req, err := http.NewRequest(r.Method, target, bytes.NewReader(body))
			if err != nil {
				return nil
			}
			setZenHeaders(req, sessionID)
			if zenRetries > 0 {
				acclog.Printf("  opencode retry %d/%d session=%s proxy=%s lane=%s", zenRetries, zenRetryLabel(), sessionID, redactProxyUserinfo(proxy), lane.id)
			}

			hedgeAfter := 2 * time.Second
			if ms := cfg().Zen.HedgeAfterMs; ms != 0 {
				if ms < 0 {
					hedgeAfter = 0
				} else {
					hedgeAfter = time.Duration(ms) * time.Millisecond
				}
			}
			var firstByteAt time.Time
			traced := req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
				GotFirstResponseByte: func() { firstByteAt = time.Now() },
			}))
			sendStart := time.Now()
			up, err, _ := hedgedDo(
				func() (*http.Response, error) { return cl.Do(traced) },
				func() (*http.Response, error) {
					hl := lane
					if nl := laneFailover(lane); nl != nil {
						hl = nl
					}
					hp := laneProxyFor(hl, zenModel)
					if hp == "" || hp == proxy {
						select {}
					}
					hcl := zenClient(hp)
					hreq, herr := http.NewRequest(r.Method, target, bytes.NewReader(body))
					if herr != nil {
						return nil, herr
					}
					setZenHeaders(hreq, laneSession(hl))
					hup, huerr := hcl.Do(hreq)
					if huerr == nil {
						noteExitOK(hp)
					}
					return hup, huerr
				}, hedgeAfter)
			if err == nil && !firstByteAt.IsZero() {
				if fb := firstByteAt.Sub(sendStart); fb > 6*time.Second {
					noteExitSlow(proxy)
				}
			}
			if err != nil {
				dropProxy(proxy)
				rotating = true
				logZenNetErr(zenRetries, clientModel, proxy, tag+" "+target, err)
				if noteZenNetworkError() {
					acclog.Printf("  %d+ consecutive network errors, refreshing proxy pool", zenErrorThreshold())
					refreshZenProxiesAsync()
					resetZenNetworkErrors()
				}
				if !zenLastRetry(zenRetries) {
					zenBackoff(zenRetries)
					continue
				}
				return nil
			}

			if up.StatusCode == http.StatusTooManyRequests || up.StatusCode == 529 {
				up.Body.Close()
				acclog.Printf("  opencode rate-limited %d (retry %d/%d) model=%s country=%s proxy=%s %s lane=%s, cooling lane",
					up.StatusCode, zenRetries, zenRetryLabel(), clientModel, proxyCountry(proxy), redactProxyUserinfo(proxy), tag, lane.id)
				if proxy == "" {
					laneEscalate(lane, zenModel) // direct got limited: give the lane an exit
				}
				noteExit429(proxy)
			laneCool(lane)
				if nl := laneFailover(lane); nl != nil {
					lane = nl
					sessionID = laneSession(lane)
				}
				rotating = true
				zenBackoff(zenRetries)
				continue
			}

			if up.StatusCode != http.StatusOK {
				eb, _ := io.ReadAll(io.LimitReader(up.Body, 16<<10))
				up.Body.Close()
				if zenServiceOverloaded(eb) {
					acclog.Printf("  opencode overloaded %d (retry %d/%d) model=%s country=%s proxy=%s %s retrying same proxy+session err=%q",
						up.StatusCode, zenRetries, zenRetryLabel(), clientModel, proxyCountry(proxy), redactProxyUserinfo(proxy), tag, errSnippet(eb, 160))
					rotating = false
					if !zenLastRetry(zenRetries) {
						zenBackoff(zenRetries)
						continue
					}
				}
				if zenGeoBlocked(eb) {
					logZenBlocked("geo", zenRetries, up.StatusCode, clientModel, proxy, tag, eb)
					dropProxy(proxy)
					laneCoolSoft(lane)
					if nl := laneFailover(lane); nl != nil {
						lane = nl
						sessionID = laneSession(lane)
					}
					rotating = true
					if noteZenGeoErr() {
						acclog.Printf("  %d+ geo-blocks, refreshing proxy pool", zenErrorThreshold())
						refreshZenProxiesAsync()
						resetZenGeoErrs()
					}
					if !zenLastRetry(zenRetries) {
						zenBackoff(zenRetries)
						continue
					}
				}
				if zenUserBlocked(eb) {
					logZenBlocked("user", zenRetries, up.StatusCode, clientModel, proxy, tag, eb)
					dropProxy(proxy)
					laneBlock(lane)
					if nl := laneFailover(lane); nl != nil {
						lane = nl
						sessionID = laneSession(lane)
					}
					rotating = true
					if noteZenBlockErr() {
						acclog.Printf("  %d+ user-blocks, refreshing proxy pool", zenErrorThreshold())
						refreshZenProxiesAsync()
						resetZenBlockErrs()
					}
					if !zenLastRetry(zenRetries) {
						zenBackoff(zenRetries)
						continue
					}
				}
				if zenFreeTierBlocked(eb) {
					acclog.Printf("  opencode free-tier-blocked %d (retry %d/%d) model=%s country=%s proxy=%s %s lane=%s, switching lane",
						up.StatusCode, zenRetries, zenRetryLabel(), clientModel, proxyCountry(proxy), redactProxyUserinfo(proxy), tag, lane.id)
					dropProxy(proxy)
					burnExit(proxy, zenModel)
					if proxy == "" {
						laneEscalate(lane, zenModel) // direct is flagged: give the lane an exit
					}
					laneBlock(lane)
					if nl := laneFailover(lane); nl != nil {
						lane = nl
						sessionID = laneSession(lane)
					}
					rotating = true
					if noteZenFreeTierErr() {
						acclog.Printf("  %d+ free-tier blocks, refreshing proxy pool", zenErrorThreshold())
						refreshZenProxiesAsync()
						resetZenFreeTierErrs()
					}
					if !zenLastRetry(zenRetries) {
						zenBackoff(zenRetries)
						continue
					}
				}
				acclog.Printf("!! opencode upstream %d (retry %d/%d) model=%s country=%s proxy=%s %s err=%q",
					up.StatusCode, zenRetries, zenRetryLabel(), clientModel, proxyCountry(proxy), redactProxyUserinfo(proxy), tag, errSnippet(eb, 160))
				up.Body = io.NopCloser(bytes.NewReader(eb))
			}
			resetZenNetworkErrors()
			if up.StatusCode == http.StatusOK {
				p.noteZenSuccess(sessionID, proxy)
				noteExitOK(proxy)
			laneTouch(lane, proxy)
			}
			return up
		}
		return nil
	}

	resp = zenPost(oaiBody, isStream, "")
	if resp == nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "opencode zen: upstream unreachable")
		return
	}
	defer resp.Body.Close()

	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusOK {
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, upstreamBodyLimit))
		if os.Getenv("ZEN_DUMP") != "" {
			p := fmt.Sprintf("/tmp/zenresp_%d.json", time.Now().UnixNano())
			os.WriteFile(p, rb, 0o644)
			acclog.Printf("ZEN_DUMP response %d bytes -> %s", len(rb), p)
		}
		errMsg := extractErrMessage(string(rb))
		acclog.Printf("!! opencode zen %d model=%s err=%q", resp.StatusCode, clientModel, errMsg)
		writeAnthropicError(w, resp.StatusCode, "api_error", errMsg)
		return
	}

	if isStream {
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, upstreamBodyLimit))
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		var written int64
		var promptT, compT int
		if isResponses {
			// nudge_no_tools (models.params in config.yml): when the
			// client offered tools but the model ended with none, refetch
			// once on the same session with a nudge and stream both bodies
			// as one turn. If the retry also calls nothing, the turn ends.
			bodies := [][]byte{rb}
			post := func(nb []byte) []byte {
				nresp := zenPost(nb, true, "nudge")
				if nresp == nil {
					return nil
				}
				defer nresp.Body.Close()
				if nresp.StatusCode != http.StatusOK {
					return nil
				}
				nb2, _ := io.ReadAll(io.LimitReader(nresp.Body, upstreamBodyLimit))
				return nb2
			}
			if retry := tryNudgeResponses(upstreamModel, oaiBody, rb, post); retry != nil {
				bodies = append(bodies, retry)
			}
			written, promptT, compT = streamResponsesMerged(w, bodies, clientModel, tnames)
		} else {
			written, promptT, compT = streamAnthropic(w, rb, clientModel, tnames)
		}

		logUsage(UsageRecord{
			Ts:               time.Now().UTC().Format(time.RFC3339Nano),
			Model:            clientModel,
			KeyName:          "opencode",
			Method:           "POST",
			Path:             "/v1/messages",
			StatusCode:       200,
			DurationMs:       elapsed.Milliseconds(),
			PromptTokens:     promptT,
			CompletionTokens: compT,
			TotalTokens:      promptT + compT,
			Stream:           true,
			ContentBytes:     int64(len(origBody)),
		})
		acclog.Printf("<- 200 POST /v1/messages %v %d bytes [opencode] model=%s %s", elapsed.Round(time.Millisecond), written, clientModel, reqID)
	} else {
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, upstreamBodyLimit))
		if os.Getenv("ZEN_DUMP") != "" {
			p := fmt.Sprintf("/tmp/zenraw_%d.bin", time.Now().UnixNano())
			os.WriteFile(p, rb, 0o644)
			acclog.Printf("ZEN_DUMP nonstream raw %d bytes -> %s", len(rb), p)
		}
		if isResponses && resp.StatusCode == http.StatusOK {
			// same no-tools continue as the stream branch, on the raw
			// SSE before folding. post replays non-stream too.
			post := func(nb []byte) []byte {
				nresp := zenPost(nb, false, "nudge")
				if nresp == nil {
					return nil
				}
				defer nresp.Body.Close()
				if nresp.StatusCode != http.StatusOK {
					return nil
				}
				nb2, _ := io.ReadAll(io.LimitReader(nresp.Body, upstreamBodyLimit))
				return nb2
			}
			if retry := tryNudgeResponses(upstreamModel, oaiBody, rb, post); retry != nil {
				rb = mergeResponsesSSE([][]byte{rb, retry})
			}
			rb = responsesToChat(responsesSSEToJSON(rb))
		} else {
			rb = sseToNonStream(rb, zenModel, tnames)
		}
		if os.Getenv("ZEN_DUMP") != "" {
			p := fmt.Sprintf("/tmp/zenfold_%d.json", time.Now().UnixNano())
			os.WriteFile(p, rb, 0o644)
			acclog.Printf("ZEN_DUMP nonstream folded %d bytes -> %s", len(rb), p)
		}
		out := rb
		prompT, compT := 0, 0
		var errMsg string
		out, errMsg, _ = openAIToAnthropic(rb, clientModel, tnames)
		if errMsg != "" {
			writeAnthropicError(w, http.StatusInternalServerError, "api_error", errMsg)
			return
		}
		prompT, compT, _ = respTokens(rb)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(out)

		logUsage(UsageRecord{
			Ts:               time.Now().UTC().Format(time.RFC3339Nano),
			Model:            clientModel,
			KeyName:          "opencode",
			Method:           "POST",
			Path:             "/v1/messages",
			StatusCode:       200,
			DurationMs:       elapsed.Milliseconds(),
			PromptTokens:     prompT,
			CompletionTokens: compT,
			TotalTokens:      prompT + compT,
			Stream:           false,
			ContentBytes:     int64(len(origBody)),
		})
		acclog.Printf("<- 200 POST /v1/messages %v %d bytes [opencode] model=%s %s", elapsed.Round(time.Millisecond), len(out), clientModel, reqID)
	}
}
