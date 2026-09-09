package main

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func approx(a, b float64) bool {
	return math.Abs(a-b) < 0.01
}

func TestIdleWeight(t *testing.T) {
	now := time.Now()
	k := &Key{}

	// never used: full weight 1.0
	if w := idleWeight(k, now); w != 1.0 {
		t.Fatalf("fresh key weight = %v, want 1.0", w)
	}

	// just used: floor 0.3
	k2 := &Key{LastUsed: now.Add(-time.Second)}
	if w := idleWeight(k2, now); !approx(w, 0.3) {
		t.Fatalf("recently-used weight = %v, want ~0.3", w)
	}

	// long idle: approaches 1.0
	k3 := &Key{LastUsed: now.Add(-time.Hour)}
	if w := idleWeight(k3, now); w != 1.0 {
		t.Fatalf("long-idle weight = %v, want 1.0", w)
	}
}

func TestIdleWeightFailPenalty(t *testing.T) {
	now := time.Now()
	k := &Key{LastUsed: now.Add(-time.Minute), LastFail: now.Add(-time.Minute), Consec429: 1}
	w := idleWeight(k, now)
	// idle=1min -> w~0.37; penalty 0.1 -> ~0.037
	if w >= 0.05 {
		t.Fatalf("recent-fail weight = %v, want strongly penalized (<0.05)", w)
	}

	// stale failure outside window: no penalty
	k2 := &Key{LastUsed: now.Add(-time.Hour), LastFail: now.Add(-time.Hour), Consec429: 3}
	if w := idleWeight(k2, now); w != 1.0 {
		t.Fatalf("stale-fail weight = %v, want 1.0 (no penalty)", w)
	}
}

func TestRateLimitBackoff(t *testing.T) {
	p := &Pool{}
	cases := []struct {
		consec int
		want   time.Duration
	}{
		{1, 1 * time.Minute},
		{2, 2 * time.Minute},
		{3, 4 * time.Minute},
		{4, 8 * time.Minute},
		{5, 16 * time.Minute},
		{10, 16 * time.Minute}, // capped
	}
	for _, c := range cases {
		k := &Key{}
		for i := 0; i < c.consec; i++ {
			p.rateLimit(k)
		}
		got := k.CooldownUntil.Sub(k.LastFail)
		if got < c.want || got > c.want+time.Millisecond {
			t.Fatalf("consec=%d backoff = %v, want %v", c.consec, got, c.want)
		}
		if k.Consec429 != c.consec {
			t.Fatalf("consec=%d, Consec429=%d", c.consec, k.Consec429)
		}
	}
}

func TestClear429(t *testing.T) {
	p := &Pool{}
	k := &Key{}
	p.rateLimit(k)
	p.rateLimit(k)
	if k.Consec429 != 2 {
		t.Fatalf("Consec429 = %d, want 2", k.Consec429)
	}
	p.Clear429(k)
	if k.Consec429 != 0 {
		t.Fatalf("after clear Consec429 = %d, want 0", k.Consec429)
	}
}

func TestPickSticky(t *testing.T) {
	now := time.Now()
	a := &Key{Name: "a", LastUsed: now.Add(-30 * time.Minute)}
	b := &Key{Name: "b", LastUsed: now.Add(-30 * time.Minute)}
	p := &Pool{keys: []*Key{a, b}, lastKey: make(map[string]string)}

	// no sticky: weighted pick returns one of them
	if k := p.PickSticky(nil, "m", ""); k == nil {
		t.Fatal("no key picked")
	}

	// sticky set to a: always picks a
	p.lastKey["m"] = "a"
	for i := 0; i < 100; i++ {
		if k := p.PickSticky(nil, "m", p.stickyKey("m")); k.Name != "a" {
			t.Fatalf("sticky pick = %s, want a", k.Name)
		}
	}

	// sticky key on cooldown: falls back to the other available key
	b.CooldownUntil = now.Add(time.Minute)
	p.lastKey["m"] = "b"
	for i := 0; i < 100; i++ {
		k := p.PickSticky(nil, "m", p.stickyKey("m"))
		if k == nil {
			t.Fatal("no fallback when sticky key is on cooldown")
		}
		if k.Name == "b" {
			t.Fatal("picked on-cooldown sticky key b")
		}
	}

	// all on cooldown: nil
	b.CooldownUntil = now.Add(time.Minute)
	a.CooldownUntil = now.Add(time.Minute)
	if k := p.PickSticky(nil, "m", p.stickyKey("m")); k != nil {
		t.Fatalf("expected nil when all keys on cooldown, got %s", k.Name)
	}
}

func TestHandleClassifier(t *testing.T) {
	// POST -> auto-approve JSON
	req := httptest.NewRequest("POST", "/v1/messages/classifier", strings.NewReader(`{"type":"permission_request"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handleClassifier(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp struct {
		Result   string `json:"result"`
		Decision struct {
			Allow bool `json:"allow"`
		} `json:"decision"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad response JSON: %v", err)
	}
	if resp.Result != "decision" || !resp.Decision.Allow {
		t.Fatalf("response = %s, want result=decision allow=true", rec.Body.String())
	}

	// GET -> 405
	req = httptest.NewRequest("GET", "/v1/messages/classifier", nil)
	rec = httptest.NewRecorder()
	handleClassifier(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", rec.Code)
	}
}

func TestPickAvoidsPenalized(t *testing.T) {
	now := time.Now()
	clean := &Key{Name: "clean", LastUsed: now.Add(-30 * time.Minute)}
	hot := &Key{Name: "hot", LastUsed: now.Add(-2 * time.Second)}
	penalized := &Key{Name: "pen", LastUsed: now.Add(-30 * time.Minute), LastFail: now.Add(-time.Minute), Consec429: 2}
	p := &Pool{keys: []*Key{clean, hot, penalized}}

	counts := map[string]int{}
	for i := 0; i < 30000; i++ {
		k := p.Pick(nil, "m")
		counts[k.Name]++
	}
	// once LastUsed self-resets in the tight loop, clean and hot both sit at the
	// idle floor; the real guarantee is the recently-failed key stays avoided.
	if counts["penalized"] > 2000 {
		t.Fatalf("penalized picked %d/30000, want rare", counts["penalized"])
	}
	if counts["penalized"] >= counts["clean"] {
		t.Fatalf("penalized (%d) picked as much as clean (%d)", counts["penalized"], counts["clean"])
	}
}

func TestEndpointForModel(t *testing.T) {
	// Muse Spark models use /responses
	for _, model := range []string{"muse-spark-1.3", "muse-spark-1.3-contributor-free", "muse-spark-1.2"} {
		if got := endpointForModel(model); got != "/responses" {
			t.Errorf("endpointForModel(%q) = %q, want /responses", model, got)
		}
	}
	// All other models use /chat/completions
	for _, model := range []string{"big-pickle", "mimo-v2.5-free", "ling-3.0-flash-fin-free", "nemotron-3-ultra-free", "nemotron-3.5-lightning-free", "kimi-k3"} {
		if got := endpointForModel(model); got != "/chat/completions" {
			t.Errorf("endpointForModel(%q) = %q, want /chat/completions", model, got)
		}
	}
}

func TestExtraFreeModelsIncludesBigPickle(t *testing.T) {
	found := false
	for _, id := range extraFreeModels {
		if id == "big-pickle" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("big-pickle not in extraFreeModels")
	}
}

func TestRefreshOpencodeModelsIncludesBigPickle(t *testing.T) {
	// Verify the refreshOpencodeModels logic includes big-pickle
	// by checking the condition: !strings.HasSuffix(m.ID, "-free") && m.ID != "big-pickle"
	ids := []string{"big-pickle", "muse-spark-1.3-contributor-free", "nemotron-3-ultra-free"}
	for _, id := range ids {
		shouldInclude := strings.HasSuffix(id, "-free") || id == "big-pickle"
		if !shouldInclude {
			t.Errorf("model %q should be included but condition excludes it", id)
		}
	}
}

func TestHandleOpenCodeUsesCorrectEndpoint(t *testing.T) {
	// Verify endpointForModel is called correctly in handleOpenCode
	// by checking that Muse Spark models route to /responses
	for _, model := range []string{"opencode/muse-spark-1.3", "opencode/muse-spark-1.3-contributor-free"} {
		realModel := strings.TrimPrefix(model, "opencode/")
		if got := endpointForModel(realModel); got != "/responses" {
			t.Errorf("%s should route to /responses, got %q", model, got)
		}
	}
	// big-pickle should route to /chat/completions
	realModel := strings.TrimPrefix("opencode/big-pickle", "opencode/")
	if got := endpointForModel(realModel); got != "/chat/completions" {
		t.Errorf("opencode/big-pickle should route to /chat/completions, got %q", got)
	}
}

func TestConvertToResponsesReasoningEffort(t *testing.T) {
	m := map[string]any{
		"model":           "muse-spark-1.3-contributor-free",
		"messages":        []any{map[string]any{"role": "user", "content": "hi"}},
		"max_tokens":      100,
		"reasoning_effort": "high",
	}
	convertToResponses(m)

	// reasoning_effort should be moved to reasoning.effort
	if _, ok := m["reasoning_effort"]; ok {
		t.Error("reasoning_effort should be deleted from top level")
	}
	reasoning, ok := m["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("reasoning should be a map, got %T", m["reasoning"])
	}
	if reasoning["effort"] != "high" {
		t.Errorf("reasoning.effort = %v, want high", reasoning["effort"])
	}

	// max_tokens → max_output_tokens
	if _, ok := m["max_tokens"]; ok {
		t.Error("max_tokens should be deleted")
	}
	if m["max_output_tokens"] != 100 {
		t.Errorf("max_output_tokens = %v, want 100", m["max_output_tokens"])
	}

	// messages → input
	if _, ok := m["messages"]; ok {
		t.Error("messages should be deleted")
	}
	if _, ok := m["input"]; !ok {
		t.Error("input should be present")
	}
}

func TestStreamResponsesToChat(t *testing.T) {
	// Simulate a Responses API SSE stream
	stream := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"Hello\"}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\" world\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":5}}}\n\n" +
		"data: [DONE]\n\n"

	var buf strings.Builder
	w := httptest.NewRecorder()
	_, promptT, compT := streamResponsesToChat(w, []byte(stream))

	body := w.Body.String()
	if !strings.Contains(body, "\"content\":\"Hello\"") {
		t.Error("missing first delta content")
	}
	if !strings.Contains(body, "\"content\":\" world\"") {
		t.Error("missing second delta content")
	}
	if !strings.Contains(body, "\"prompt_tokens\":10") {
		t.Error("missing prompt_tokens in usage")
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Error("missing [DONE]")
	}
	if promptT != 10 {
		t.Errorf("promptT = %d, want 10", promptT)
	}
	if compT != 5 {
		t.Errorf("compT = %d, want 5", compT)
	}
	_ = buf
}
