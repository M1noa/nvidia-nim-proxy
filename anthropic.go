package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// --- Types ---

type anthropicRequest struct {
	Model         string           `json:"model"`
	System        json.RawMessage  `json:"system,omitempty"`
	Messages      []anthropicMsg   `json:"messages"`
	Tools         []anthropicTool  `json:"tools,omitempty"`
	ToolChoice    any              `json:"tool_choice,omitempty"`
	MaxTokens     int              `json:"max_tokens"`
	Temperature   *float64         `json:"temperature,omitempty"`
	TopP          *float64         `json:"top_p,omitempty"`
	TopK          *int             `json:"top_k,omitempty"`
	StopSequences []string         `json:"stop_sequences,omitempty"`
	Stream        bool             `json:"stream,omitempty"`
}

type anthropicMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
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
	} `json:"source,omitempty"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
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

func loadClaudeModels(path string) {
	claudeModelsMu.Lock()
	defer claudeModelsMu.Unlock()
	claudeModels = nil
	raw, err := os.ReadFile(path)
	if err != nil {
		log.Printf("  No %s, Claude model passthrough enabled", path)
		return
	}
	clean := stripComments(raw)
	var entries []claudeModelEntry
	if err := json.Unmarshal(clean, &entries); err != nil {
		log.Printf("  WARN: %s: %v", path, err)
		return
	}
	for _, e := range entries {
		if e.Pattern == "" || e.Model == "" {
			continue
		}
		claudeModels = append(claudeModels, &claudeModelEntry{
			Pattern: strings.ToLower(e.Pattern),
			Model:   e.Model,
		})
	}
	log.Printf("  Loaded %d Claude model mappings from %s", len(claudeModels), path)
}

func watchClaudeModels(path string) {
	var lastMod time.Time
	for {
		fi, err := os.Stat(path)
		if err == nil {
			mod := fi.ModTime()
			if !mod.Equal(lastMod) && !lastMod.IsZero() {
				log.Printf("  %s changed, reloading", path)
				loadClaudeModels(path)
			}
			lastMod = mod
		}
		time.Sleep(checkInterval)
	}
}

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

// --- Request Conversion ---

func anthropicRequestToOpenAI(body []byte) (oai []byte, clientModel, upstreamModel string, isStream bool, err error) {
	var req anthropicRequest
	if err = json.Unmarshal(body, &req); err != nil {
		return nil, "", "", false, fmt.Errorf("invalid JSON: %w", err)
	}

	clientModel = strings.TrimSuffix(req.Model, "[1m]")
	upstreamModel = mapClaudeModel(clientModel)
	isStream = req.Stream

	messages := make([]map[string]any, 0)

	if sysText := extractSystemText(req.System); sysText != "" {
		messages = append(messages, map[string]any{"role": "system", "content": sysText})
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
		tools := make([]map[string]any, len(req.Tools))
		for i, t := range req.Tools {
			tools[i] = map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  json.RawMessage(t.InputSchema),
				},
			}
		}
		out["tools"] = tools
	}

	if req.ToolChoice != nil {
		out["tool_choice"] = convertToolChoice(req.ToolChoice)
	}

	if isStream {
		out["stream"] = true
		out["stream_options"] = map[string]any{"include_usage": true}
	}

	ob, _ := json.Marshal(out)
	injectParams(&ob)

	return ob, clientModel, upstreamModel, isStream, nil
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

	for _, b := range blocks {
		switch b.Type {
		case "text":
			textParts = append(textParts, b.Text)
		case "tool_result":
			content := toolResultText(b.Content)
			if b.IsError {
				content = "Error: " + content
			}
			result = append(result, map[string]any{
				"role":         "tool",
				"tool_call_id": b.ToolUseID,
				"content":      content,
			})
		case "image":
			if b.Source != nil {
				textParts = append(textParts, fmt.Sprintf("[Image: %s]", b.Source.MediaType))
			}
		}
	}

	if len(textParts) > 0 {
		userMsg := map[string]any{"role": "user", "content": strings.Join(textParts, "\n")}
		result = append([]map[string]any{userMsg}, result...)
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

	for _, b := range blocks {
		switch b.Type {
		case "text":
			textParts = append(textParts, b.Text)
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
	return []map[string]any{msg}
}

func toolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []anthropicBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return string(raw)
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, "\n")
	}
	return string(raw)
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

func openAIToAnthropic(body []byte, clientModel string) (out []byte, errMsg string, msgID string) {
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
				"name":  tc.Function.Name,
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

// --- Streaming Conversion ---

func streamAnthropic(w http.ResponseWriter, body []byte, clientModel string) (written int64, promptT, compT int) {
	fl, _ := w.(http.Flusher)

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
					ToolCalls []struct {
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
			written += writeSSE(w, "content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%s}}`, jsonStr(choice.Delta.Content)))
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
					written += writeSSE(w, "content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":"%s","name":"%s","input":{}}}`, lastToolIdx, toolID, tc.Function.Name))
				}
				if tc.Function.Arguments != "" {
					written += writeSSE(w, "content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%s}}`, lastToolIdx, jsonStr(tc.Function.Arguments)))
				}
			}
		}

		// finish reason
		if choice.FinishReason != nil && !hasSentStopReason {
			hasSentStopReason = true

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

// streamResponses converts an OpenAI /responses SSE stream to Anthropic messages SSE.
func streamResponses(w http.ResponseWriter, body []byte, clientModel string) (written int64, promptT, compT int) {
	fl, _ := w.(http.Flusher)

	msgID := "msg_" + randHex(24)
	preamble := fmt.Sprintf(`{"type":"message_start","message":{"id":"%s","type":"message","role":"assistant","model":"%s","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0}}}`, msgID, clientModel)
	written += writeSSE(w, "message_start", preamble)
	written += writeSSE(w, "ping", `{"type":"ping"}`)
	if fl != nil {
		fl.Flush()
	}

	blockIdx := 0
	textIdx := -1
	textOpen := false
	toolIdx := map[string]int{}
	hasToolUse := false
	hasSentStopReason := false

	openText := func() {
		if textOpen {
			return
		}
		textIdx = blockIdx
		blockIdx++
		textOpen = true
		written += writeSSE(w, "content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`, textIdx))
	}
	closeText := func() {
		if !textOpen {
			return
		}
		textOpen = false
		written += writeSSE(w, "content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, textIdx))
	}
	openTool := func(id, callID, name string) {
		if _, ok := toolIdx[id]; ok {
			return
		}
		toolIdx[id] = blockIdx
		blockIdx++
		hasToolUse = true
		written += writeSSE(w, "content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%s,"name":%s,"input":{}}}`, toolIdx[id], jsonStr(callID), jsonStr(name)))
	}
	closeTool := func(id string) {
		if _, ok := toolIdx[id]; !ok {
			return
		}
		written += writeSSE(w, "content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, toolIdx[id]))
		delete(toolIdx, id)
	}
	finish := func() {
		if hasSentStopReason {
			return
		}
		hasSentStopReason = true
		closeText()
		for id, idx := range toolIdx {
			written += writeSSE(w, "content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, idx))
			delete(toolIdx, id)
		}
		stopReason := "end_turn"
		if hasToolUse {
			stopReason = "tool_use"
		}
		written += writeSSE(w, "message_delta", fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%s,"stop_sequence":null},"usage":{"output_tokens":%d}}`, jsonStr(stopReason), compT))
		written += writeSSE(w, "message_stop", `{"type":"message_stop"}`)
		n, _ := w.Write([]byte("data: [DONE]\n\n"))
		written += int64(n)
		if fl != nil {
			fl.Flush()
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
				openTool(id, evt.Item.CallID, evt.Item.Name)
			}
		case "response.function_call_arguments.delta":
			if _, ok := toolIdx[evt.ItemID]; ok && evt.Delta != "" {
				written += writeSSE(w, "content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%s}}`, toolIdx[evt.ItemID], jsonStr(evt.Delta)))
			}
		case "response.output_item.done":
			if evt.Item != nil && evt.Item.Type == "function_call" {
				closeTool(evt.ItemID)
			}
		case "response.output_text.delta":
			openText()
			if evt.Delta != "" {
				written += writeSSE(w, "content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%s}}`, textIdx, jsonStr(evt.Delta)))
			}
		case "response.output_text.done", "response.content_part.done":
			closeText()
		case "response.completed":
			if evt.Response != nil && evt.Response.Usage != nil {
				compT = evt.Response.Usage.OutputTokens
				promptT = evt.Response.Usage.InputTokens
			}
			finish()
			return
		}
	}

	finish()
	return
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

func (p *Pool) handleAnthropic(w http.ResponseWriter, r *http.Request, start time.Time) {
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

	if r.URL.Path == "/v1/messages/count_tokens" {
		handleCountTokens(w, body)
		return
	}

	p.sem <- struct{}{}
	p.concurrent.Add(1)
	defer func() {
		p.concurrent.Add(-1)
		<-p.sem
	}()

	oaiBody, clientModel, upstreamModel, isStream, err := anthropicRequestToOpenAI(body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	acclog.Printf("-> POST /v1/messages model=%s upstream=%s stream=%v bytes=%d", clientModel, upstreamModel, isStream, len(oaiBody))

	// Route opencode/* models to zen endpoint
	if strings.HasPrefix(upstreamModel, "opencode/") {
		p.handleOpenCodeAnthropic(w, r, body, oaiBody, clientModel, upstreamModel, isStream, start)
		return
	}

	target := nvidiaBase + "/chat/completions"
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
		written, promptT, compT := streamAnthropic(w, u.Body, clientModel)

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
		out, errMsg, _ := openAIToAnthropic(u.Body, clientModel)
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
func (p *Pool) handleOpenCodeAnthropic(w http.ResponseWriter, r *http.Request, origBody, oaiBody []byte, clientModel, upstreamModel string, isStream bool, start time.Time) {
	zenModel := strings.TrimPrefix(upstreamModel, "opencode/")
	isResponses := endpointForModel(zenModel) == "/responses"

	var m map[string]any
	if json.Unmarshal(oaiBody, &m) == nil {
		m["model"] = zenModel
		stripCacheFields(m)
		if isResponses {
			convertToResponses(m)
		}
		if b, err := json.Marshal(m); err == nil {
			oaiBody = b
		}
	}

	target := OpencodeBase + endpointForModel(zenModel)
	cl := &http.Client{Timeout: 300 * time.Second}
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

	sessionID := ""
	if s := r.Header.Get("x-session-id"); s != "" {
		sessionID = s
	} else {
		sessionID = "ses_" + randHex(20)
	}

	for zenRetries := 0; zenRetries < 5; zenRetries++ {
		proxy := ""
		if zenRetries == 0 {
			cl = &http.Client{Timeout: 300 * time.Second}
		} else {
			proxy = pickFastProxy()
			if proxy == "" {
				continue
			}
			cl = zenClient(proxy)
			sessionID = "ses_" + randHex(20)
		}
		req, err := http.NewRequest(r.Method, target, bytes.NewReader(oaiBody))
		if err != nil {
			writeAnthropicError(w, http.StatusInternalServerError, "api_error", err.Error())
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer public")
		req.Header.Set("x-opencode-client", "desktop")
		req.Header.Set("x-opencode-session", sessionID)
		if zenRetries > 0 {
			acclog.Printf("  opencode retry %d/4 session=%s proxy=%s", zenRetries, sessionID, proxy)
		}
		req.Header.Set("User-Agent", "opencode/1.18.25")
		if isStream {
			req.Header.Set("Accept", "text/event-stream")
		} else {
			req.Header.Set("Accept", "application/json")
		}

		resp, err = cl.Do(req)
		if err != nil {
			acclog.Printf("!! opencode zen error (retry %d/4) %s: %v", zenRetries, target, err)
			if zenRetries < 4 {
				time.Sleep(time.Duration(zenRetries+1) * time.Second)
				continue
			}
			writeAnthropicError(w, http.StatusBadGateway, "api_error", "opencode zen: "+err.Error())
			return
		}

		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == 529 {
			resp.Body.Close()
			time.Sleep(time.Duration(zenRetries+1) * time.Second)
			continue
		}
		break
	}
	defer resp.Body.Close()

	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusOK {
		rb, _ := io.ReadAll(resp.Body)
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
		rb, _ := io.ReadAll(resp.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		var written int64
		var promptT, compT int
		if isResponses {
			written, promptT, compT = streamResponses(w, rb, clientModel)
		} else {
			written, promptT, compT = streamAnthropic(w, rb, clientModel)
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
		acclog.Printf("<- 200 POST /v1/messages %v %d bytes [opencode]", elapsed.Round(time.Millisecond), written)
	} else {
		rb, _ := io.ReadAll(resp.Body)
		if isResponses {
			rb = responsesToChat(rb)
		}
		out, errMsg, _ := openAIToAnthropic(rb, clientModel)
		if errMsg != "" {
			writeAnthropicError(w, http.StatusInternalServerError, "api_error", errMsg)
			return
		}
		prompT, compT, _ := respTokens(rb)

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
		acclog.Printf("<- 200 POST /v1/messages %v %d bytes [opencode]", elapsed.Round(time.Millisecond), len(out))
	}
}
