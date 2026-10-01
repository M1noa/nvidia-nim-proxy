package main

import (
	"encoding/json"
	"log"
	"os"
	"sync"
)

// usage tracking: per-request jsonl + in-memory totals for /status.
// disabled when usage.enabled is false (zero file, zero counters).

var (
	usageMu  sync.Mutex
	usageOut *json.Encoder
	usageFd  *os.File
)

type UsageRecord struct {
	Ts               string            `json:"ts"`
	Model            string            `json:"model"`
	KeyName          string            `json:"key"`
	Method           string            `json:"method"`
	Path             string            `json:"path"`
	StatusCode       int               `json:"status"`
	DurationMs       int64             `json:"duration_ms"`
	PromptTokens     int               `json:"prompt_tokens,omitempty"`
	CompletionTokens int               `json:"completion_tokens,omitempty"`
	TotalTokens      int               `json:"total_tokens,omitempty"`
	Stream           bool              `json:"stream"`
	RetryAttempt     int               `json:"retry_attempt"`
	RateLimited      bool              `json:"rate_limited"`
	CooldownSecs     int               `json:"cooldown_secs,omitempty"`
	Error            string            `json:"error,omitempty"`
	ContentBytes     int64             `json:"content_bytes"`
	Headers          map[string]string `json:"headers,omitempty"`
}

type usageTotals struct {
	Requests   int `json:"requests"`
	Prompt     int `json:"prompt_tokens"`
	Completion int `json:"completion_tokens"`
	Errors     int `json:"errors"`
}

var (
	totalsMu sync.Mutex
	totals   = map[string]*usageTotals{}
)

func logUsage(r UsageRecord) {
	if !cfg().Usage.Enabled {
		return
	}
	usageMu.Lock()
	if usageOut != nil {
		usageOut.Encode(r)
	}
	usageMu.Unlock()
	totalsMu.Lock()
	t := totals[r.Model]
	if t == nil {
		t = &usageTotals{}
		totals[r.Model] = t
	}
	t.Requests++
	t.Prompt += r.PromptTokens
	t.Completion += r.CompletionTokens
	if r.StatusCode >= 400 {
		t.Errors++
	}
	totalsMu.Unlock()
}

func usageSnapshot() (reqs, prompt, comp int, byModel map[string]usageTotals) {
	totalsMu.Lock()
	defer totalsMu.Unlock()
	byModel = make(map[string]usageTotals, len(totals))
	for m, t := range totals {
		byModel[m] = *t
		reqs += t.Requests
		prompt += t.Prompt
		comp += t.Completion
	}
	return
}

func initUsageLog() {
	if !cfg().Usage.Enabled {
		return
	}
	path := cfg().Usage.Path
	if path == "" {
		path = "nim-usage.jsonl"
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("WARN: cannot open %s: %v", path, err)
		return
	}
	usageMu.Lock()
	usageFd = f
	usageOut = json.NewEncoder(f)
	usageMu.Unlock()
}

// sseUsageTee scans chat sse bytes for a usage chunk, returns prompt/comp.
// nvidia/zen chat streams carry {"usage":{"prompt_tokens":..}} on the tail.
func sseUsageTee(b []byte) (prompt, comp int) {
	var start int
	for i := 0; i < len(b); i++ {
		if b[i] != '\n' {
			continue
		}
		line := b[start:i]
		start = i + 1
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		if len(line) < 6 || string(line[:6]) != "data: " {
			continue
		}
		data := line[6:]
		if len(data) == 0 || data[0] != '{' {
			continue
		}
		var ch struct {
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(data, &ch) != nil || ch.Usage == nil {
			continue
		}
		if ch.Usage.PromptTokens > 0 {
			prompt = ch.Usage.PromptTokens
		}
		if ch.Usage.CompletionTokens > 0 {
			comp = ch.Usage.CompletionTokens
		}
	}
	return
}

func closeUsageLog() {
	usageMu.Lock()
	defer usageMu.Unlock()
	if usageFd != nil {
		usageFd.Close()
		usageFd = nil
		usageOut = nil
	}
}
