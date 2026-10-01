package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sseUsageTee pulls the usage chunk out of a chat sse stream.
func TestSseUsageTee(t *testing.T) {
	sse := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":20,\"total_tokens\":120}}\n\n" +
		"data: [DONE]\n\n"
	p, c := sseUsageTee([]byte(sse))
	if p != 100 || c != 20 {
		t.Fatalf("got prompt=%d comp=%d, want 100/20", p, c)
	}
	if p, c := sseUsageTee([]byte("data: [DONE]\n\n")); p != 0 || c != 0 {
		t.Fatalf("empty stream gave %d/%d, want 0/0", p, c)
	}
}

// logUsage disabled writes nothing and bumps no counters.
func TestLogUsageDisabled(t *testing.T) {
	c := testConfig()
	c.Usage.Enabled = false
	useConfig(t, c)
	before, _, _, _ := usageSnapshot()
	logUsage(UsageRecord{Model: "m", PromptTokens: 5, CompletionTokens: 3})
	after, _, _, _ := usageSnapshot()
	if before != after {
		t.Fatalf("disabled logUsage bumped counters: %d -> %d", before, after)
	}
}

// logUsage enabled appends one json line and aggregates per model.
func TestLogUsageFileAndTotals(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "u.jsonl")
	c := testConfig()
	c.Usage.Enabled = true
	c.Usage.Path = path
	useConfig(t, c)
	initUsageLog()
	t.Cleanup(closeUsageLog)
	totalsMu.Lock()
	totals = map[string]*usageTotals{}
	totalsMu.Unlock()

	logUsage(UsageRecord{
		Ts: "t", Model: "m1", Method: "POST", Path: "/v1/chat/completions",
		StatusCode: 200, PromptTokens: 10, CompletionTokens: 4, TotalTokens: 14,
	})
	logUsage(UsageRecord{
		Ts: "t", Model: "m1", Method: "POST", Path: "/v1/chat/completions",
		StatusCode: 500, Error: "boom",
	})

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 jsonl lines, got %d", len(lines))
	}
	var r UsageRecord
	if err := json.Unmarshal([]byte(lines[0]), &r); err != nil {
		t.Fatal(err)
	}
	if r.Model != "m1" || r.PromptTokens != 10 || r.TotalTokens != 14 {
		t.Fatalf("bad record: %+v", r)
	}

	reqs, prompt, comp, byModel := usageSnapshot()
	if reqs < 2 || prompt < 10 || comp < 4 {
		t.Fatalf("bad totals: reqs=%d prompt=%d comp=%d", reqs, prompt, comp)
	}
	if byModel["m1"].Requests < 2 || byModel["m1"].Errors < 1 {
		t.Fatalf("bad per-model: %+v", byModel["m1"])
	}
}

// /status carries usage totals when enabled, hides them when off.
func TestStatusUsage(t *testing.T) {
	c := testConfig()
	c.Usage.Enabled = true
	c.Status.ShowUsage = true
	useConfig(t, c)
	totalsMu.Lock()
	totals = map[string]*usageTotals{"m1": {Requests: 2, Prompt: 10, Completion: 4}}
	totalsMu.Unlock()
	p := newPool(nil)
	sr := p.StatusFor(true)
	if sr.Usage == nil || sr.Usage.Requests != 2 || sr.Usage.Prompt != 10 {
		t.Fatalf("bad status usage: %+v", sr.Usage)
	}

	off := testConfig()
	off.Usage.Enabled = false
	off.Status.ShowUsage = true
	useConfig(t, off)
	if sr := p.StatusFor(true); sr.Usage != nil {
		t.Fatalf("usage shown while disabled: %+v", sr.Usage)
	}
}

// usage defaults stay on for old configs without the section.
func TestUsageDefaultsOn(t *testing.T) {
	d := defaultConfig()
	if !d.Usage.Enabled || d.Usage.Path == "" || !d.Status.ShowUsage {
		t.Fatalf("bad defaults: %+v %+v", d.Usage, d.Status)
	}
	c, err := loadConfigFile("config.yml.example")
	if err != nil {
		t.Fatal(err)
	}
	if !c.Usage.Enabled || c.Usage.Path == "" {
		t.Fatalf("example must enable usage: %+v", c.Usage)
	}
}
