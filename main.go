package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	nvidiaBase    = "https://integrate.api.nvidia.com/v1"
	cooldown429   = 1 * time.Minute
	modelLockout  = 30 * time.Second
	burstCooldown = 5 * time.Second
	bodyLimit     = 10 << 20
	checkInterval = 5 * time.Second
	versionStr    = "2.3.0"
)

type Key struct {
	Name           string
	Key            string
	CooldownUntil  time.Time
	CooldownReason string
	LastUsed       time.Time
	FailCount      int
}

type ModelLock struct {
	mu   sync.Mutex
	data map[string]map[string]time.Time
}

func (m *ModelLock) isLocked(key, model string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data == nil {
		return false
	}
	if perKey, ok := m.data[key]; ok {
		return time.Now().Before(perKey[model])
	}
	return false
}

func (m *ModelLock) lock(key, model string, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data == nil {
		m.data = make(map[string]map[string]time.Time)
	}
	if m.data[key] == nil {
		m.data[key] = make(map[string]time.Time)
	}
	m.data[key][model] = time.Now().Add(d)
}

func (m *ModelLock) lockedModels(key string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	var out []string
	if perKey, ok := m.data[key]; ok {
		for mdl, until := range perKey {
			if now.Before(until) {
				out = append(out, mdl)
			}
		}
	}
	sort.Strings(out)
	return out
}

type Pool struct {
	keys       []*Key
	mu         sync.RWMutex
	start      time.Time
	locks      ModelLock
	sem        chan struct{}
	concurrent atomic.Int64
}

func newPool(entries map[string]string) *Pool {
	p := &Pool{start: time.Now()}
	for name, k := range entries {
		p.keys = append(p.keys, &Key{Name: name, Key: k})
	}
	limit := (len(entries)*3 + 3) / 4 // ceil(75%)
	p.sem = make(chan struct{}, limit)
	log.Printf("  Concurrency limit: %d (75%% of %d keys)", limit, len(entries))
	sort.Slice(p.keys, func(i, j int) bool { return p.keys[i].Name < p.keys[j].Name })
	return p
}

func (p *Pool) Reload(entries map[string]string) (added, removed int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	old := make(map[string]*Key, len(p.keys))
	for _, k := range p.keys {
		old[k.Name] = k
	}
	seen := make(map[string]bool, len(entries))
	fresh := make([]*Key, 0, len(entries))
	for name, k := range entries {
		seen[name] = true
		if existing, ok := old[name]; ok {
			existing.Key = k
			fresh = append(fresh, existing)
		} else {
			fresh = append(fresh, &Key{Name: name, Key: k})
			added++
		}
	}
	for name := range old {
		if !seen[name] {
			removed++
		}
	}
	sort.Slice(fresh, func(i, j int) bool { return fresh[i].Name < fresh[j].Name })
	p.keys = fresh
	return
}

func (p *Pool) Pick(exclude map[string]bool, model string) *Key {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	var avail []*Key
	for _, k := range p.keys {
		if exclude[k.Name] {
			continue
		}
		if now.Before(k.CooldownUntil) {
			continue
		}
		if p.locks.isLocked(k.Name, model) {
			continue
		}
		avail = append(avail, k)
	}
	if len(avail) == 0 {
		return nil
	}
	pick := avail[rand.Intn(len(avail))]
	pick.LastUsed = now
	return pick
}

func (p *Pool) Cooldown(k *Key, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	k.CooldownUntil = time.Now().Add(d)
	k.CooldownReason = "rate_limited_429"
	k.FailCount++
}

func (p *Pool) Postpone(k *Key, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	k.CooldownUntil = time.Now().Add(d)
	k.CooldownReason = "post_request"
}

type KeyStatus struct {
	Name           string `json:"name"`
	Suffix         string `json:"suffix"`
	Cooldown       string `json:"cooldown,omitempty"`
	CooldownReason string `json:"cooldown_reason,omitempty"`
	LastUsed       string `json:"last_used,omitempty"`
	FailCount      int    `json:"fail_count"`
	Available      bool   `json:"available"`
}

type StatusResponse struct {
	OK         bool        `json:"ok"`
	Version    string      `json:"version"`
	Uptime     string      `json:"uptime"`
	Total      int         `json:"total"`
	Available  int         `json:"available"`
	Concurrent int         `json:"concurrent"`
	SemLimit   int         `json:"sem_limit"`
	Keys       []KeyStatus `json:"keys"`
	Locks      interface{} `json:"model_locks,omitempty"`
}

func (p *Pool) Status() StatusResponse {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	sr := StatusResponse{
		OK:         true,
		Version:    versionStr,
		Uptime:     now.Sub(p.start).Round(time.Second).String(),
		Total:      len(p.keys),
		Concurrent: int(p.concurrent.Load()),
		SemLimit:   cap(p.sem),
		Keys:       make([]KeyStatus, len(p.keys)),
	}
	locks := make(map[string][]string)
	for i, k := range p.keys {
		sk := k.Key
		if len(sk) > 8 {
			sk = sk[len(sk)-8:]
		}
		avail := true
		ks := KeyStatus{Name: k.Name, Suffix: sk, FailCount: k.FailCount}
		if now.Before(k.CooldownUntil) {
			ks.Cooldown = k.CooldownUntil.Sub(now).Round(time.Second).String()
			ks.CooldownReason = k.CooldownReason
			avail = false
		}
		if !k.LastUsed.IsZero() {
			ks.LastUsed = now.Sub(k.LastUsed).Round(time.Second).String()
		}
		ks.Available = avail
		if avail {
			sr.Available++
		}
		sr.Keys[i] = ks
		if lms := p.locks.lockedModels(k.Name); len(lms) > 0 {
			locks[k.Name] = lms
		}
	}
	if len(locks) > 0 {
		sr.Locks = locks
	}
	return sr
}

var (
	acclog    *log.Logger
	dbglog    *log.Logger
	debugMode bool
)

func initLogging() {
	acclog = log.New(os.Stdout, "", log.LstdFlags)
	df := "nim-proxy-debug.log"
	if e := os.Getenv("DEBUG_FILE"); e != "" {
		df = e
	}
	f, err := os.OpenFile(df, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		acclog.Printf("WARN: cannot open debug log %s: %v", df, err)
		dbglog = log.New(io.Discard, "", 0)
	} else {
		dbglog = log.New(f, "", log.LstdFlags)
	}
	debugMode = os.Getenv("DEBUG") == "1" || os.Getenv("DEBUG") == "true"
}

func short(s string) string {
	if len(s) > 8 {
		return s[len(s)-8:]
	}
	return s
}

func stripComments(raw []byte) []byte {
	res := make([]byte, 0, len(raw))
	inStr := false
	escape := false
	for i := 0; i < len(raw); i++ {
		b := raw[i]
		if escape {
			escape = false
			res = append(res, b)
			continue
		}
		if b == '\\' && inStr {
			escape = true
			res = append(res, b)
			continue
		}
		if b == '"' {
			inStr = !inStr
			res = append(res, b)
			continue
		}
		if inStr {
			res = append(res, b)
			continue
		}
		if b == '/' && i+1 < len(raw) && raw[i+1] == '/' {
			for i < len(raw) && raw[i] != '\n' {
				i++
			}
			res = append(res, '\n')
			continue
		}
		if b == '/' && i+1 < len(raw) && raw[i+1] == '*' {
			i += 2
			for i < len(raw) {
				if raw[i] == '*' && i+1 < len(raw) && raw[i+1] == '/' {
					i += 2
					break
				}
				i++
			}
			continue
		}
		res = append(res, b)
	}
	return res
}

// usage tracking

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

func logUsage(r UsageRecord) {
	usageMu.Lock()
	defer usageMu.Unlock()
	if usageOut == nil {
		return
	}
	usageOut.Encode(r)
}

type modelParamEntry struct {
	pattern string
	params  map[string]any
}

var (
	modelParams []*modelParamEntry
	modelMu     sync.RWMutex
)

func loadModelParams(path string) {
	modelMu.Lock()
	defer modelMu.Unlock()
	modelParams = nil
	raw, err := os.ReadFile(path)
	if err != nil {
		log.Printf("WARN: no %s, skipping default params", path)
		return
	}
	for _, line := range strings.Split(string(raw), "\n") {
		tr := strings.TrimSpace(line)
		if tr == "" {
			continue
		}
		if strings.HasPrefix(tr, "##") {
			pat := strings.TrimSpace(strings.TrimPrefix(tr, "##"))
			if pat != "" {
				modelParams = append(modelParams, &modelParamEntry{pattern: strings.ToLower(pat), params: make(map[string]any)})
			}
			continue
		}
		if strings.HasPrefix(tr, "#") {
			continue
		}
		if len(modelParams) > 0 && strings.Contains(tr, ":") {
			kv := strings.SplitN(tr, ":", 2)
			k := strings.TrimSpace(kv[0])
			v := strings.TrimSpace(kv[1])
			if k != "" && v != "" {
				modelParams[len(modelParams)-1].params[k] = parseParamVal(v)
			}
		}
	}
	log.Printf("  Loaded %d model param entries from %s", len(modelParams), path)
}

func parseParamVal(s string) any {
	if s == "true" || s == "false" {
		return s == "true"
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		if f == float64(int(f)) {
			return int(f)
		}
		return f
	}
	return s
}

func matchModelParams(model string) map[string]any {
	modelMu.RLock()
	defer modelMu.RUnlock()
	ml := strings.ToLower(model)
	for _, e := range modelParams {
		if globMatch(e.pattern, ml) {
			return e.params
		}
	}
	return nil
}

func globMatch(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	for _, part := range parts {
		if part == "" {
			continue
		}
		idx := strings.Index(s, part)
		if idx < 0 {
			return false
		}
		s = s[idx+len(part):]
	}
	return true
}

func matchesModelParams(model string) *modelParamEntry {
	modelMu.RLock()
	defer modelMu.RUnlock()
	ml := strings.ToLower(model)
	for _, e := range modelParams {
		if globMatch(e.pattern, ml) {
			return e
		}
	}
	return nil
}

type openRouterModel struct {
	ID               string                 `json:"id"`
	Name             string                 `json:"name"`
	Description      string                 `json:"description,omitempty"`
	ContextLength    int                    `json:"context_length"`
	Pricing          map[string]string      `json:"pricing"`
	Architecture     map[string]any         `json:"architecture"`
	TopProvider      map[string]any         `json:"top_provider"`
	PerRequestLimits any                    `json:"per_request_limits"`
}

func (p *Pool) handleModels(w http.ResponseWriter, r *http.Request) {
	var key *Key
	p.mu.RLock()
	for _, k := range p.keys {
		if time.Now().After(k.CooldownUntil) {
			key = k
			break
		}
	}
	if key == nil && len(p.keys) > 0 {
		key = p.keys[0]
	}
	p.mu.RUnlock()

	var nvModels []string
	if key != nil {
		req, _ := http.NewRequest("GET", nvidiaBase+"/models", nil)
		req.Header.Set("Authorization", "Bearer "+key.Key)
		resp, err := http.DefaultClient.Do(req)
		if err == nil && resp.StatusCode == 200 {
			var nv struct {
				Data []struct {
					ID string `json:"id"`
				} `json:"data"`
			}
			if json.NewDecoder(resp.Body).Decode(&nv) == nil {
				for _, m := range nv.Data {
					nvModels = append(nvModels, m.ID)
				}
			}
			resp.Body.Close()
		}
	}

	var out struct {
		Data []openRouterModel `json:"data"`
	}
	seen := make(map[string]bool)
	for _, mid := range nvModels {
		if seen[mid] {
			continue
		}
		ent := matchesModelParams(mid)
		if ent == nil {
			continue
		}
		seen[mid] = true
		cl := 131072
		desc := ""
		if v, ok := ent.params["context_length"]; ok {
			switch n := v.(type) {
			case float64:
				cl = int(n)
			case int:
				cl = n
			}
		}
		if v, ok := ent.params["description"]; ok {
			desc, _ = v.(string)
		}
		name := modelDisplayName(mid)
		out.Data = append(out.Data, openRouterModel{
			ID:            mid,
			Name:          name,
			Description:   desc,
			ContextLength: cl,
			Pricing:       map[string]string{"prompt": "0", "completion": "0", "request": "0"},
			Architecture: map[string]any{
				"modality":     "text->text",
				"tokenizer":    "Other",
				"instruct_type": nil,
			},
			TopProvider: map[string]any{
				"context_length":       cl,
				"max_completion_tokens": nil,
				"is_moderated":         false,
			},
			PerRequestLimits: nil,
		})
	}
	if len(out.Data) == 0 {
		modelMu.RLock()
		for _, e := range modelParams {
			cl := 131072
			desc := ""
			if v, ok := e.params["context_length"]; ok {
				switch n := v.(type) {
				case float64:
					cl = int(n)
				case int:
					cl = n
				}
			}
			if v, ok := e.params["description"]; ok {
				desc, _ = v.(string)
			}
			p := strings.ReplaceAll(e.pattern, "*", "")
			out.Data = append(out.Data, openRouterModel{
				ID:              p,
				Name:            modelDisplayName(p),
				Description:     desc,
				ContextLength:   cl,
				Pricing:         map[string]string{"prompt": "0", "completion": "0", "request": "0"},
				Architecture:    map[string]any{"modality": "text->text", "tokenizer": "Other", "instruct_type": nil},
				TopProvider:     map[string]any{"context_length": cl, "max_completion_tokens": nil, "is_moderated": false},
				PerRequestLimits: nil,
			})
		}
		modelMu.RUnlock()
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func modelDisplayName(id string) string {
	parts := strings.Split(id, "/")
	short := parts[len(parts)-1]
	short = strings.ReplaceAll(short, "-", " ")
	short = strings.ReplaceAll(short, "_", " ")
	if len(short) > 0 {
		short = strings.ToUpper(short[:1]) + short[1:]
	}
	return short
}

var camelToSnake = map[string]string{
	"topP": "top_p", "topK": "top_k",
	"maxTokens": "max_tokens", "minP": "min_p",
	"frequencyPenalty": "frequency_penalty",
	"presencePenalty":  "presence_penalty",
	"repetitionPenalty": "repetition_penalty",
	"stopSequences": "stop",
	"reasoningEffort":  "reasoning_effort",
	"reasoningBudget": "reasoning_budget",
}

func injectParams(body *[]byte) {
	if len(*body) == 0 {
		return
	}
	var m map[string]any
	if err := json.Unmarshal(*body, &m); err != nil {
		return
	}

	for camel, snake := range camelToSnake {
		if v, ok := m[camel]; ok {
			if _, exists := m[snake]; !exists {
				m[snake] = v
			}
			delete(m, camel)
		}
	}

	nimVal, hasNim := m["_nim"]
	delete(m, "_nim")

	inject := true
	if hasNim {
		if b, ok := nimVal.(bool); ok && b {
			inject = false
		}
	}

	if inject {
		model, _ := m["model"].(string)
		if model != "" {
			if p := matchModelParams(model); p != nil {
				for k, v := range p {
					if k == "context_length" || k == "description" {
						continue
					}
					if _, exists := m[k]; !exists {
						m[k] = v
					}
				}
			}
		}
	}

	out, err := json.Marshal(m)
	if err != nil {
		return
	}
	*body = out
}

func reqModel(body []byte) string {
	var j struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &j) == nil && j.Model != "" {
		return j.Model
	}
	return ""
}

func respTokens(body []byte) (prompt, completion, total int) {
	var j struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &j) == nil && j.Usage.TotalTokens > 0 {
		return j.Usage.PromptTokens, j.Usage.CompletionTokens, j.Usage.TotalTokens
	}
	return
}

func rlHeaders(h http.Header) map[string]string {
	m := make(map[string]string)
	for k := range h {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-ratelimit") || lk == "retry-after" {
			m[k] = h.Get(k)
		}
	}
	return m
}

func (p *Pool) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "*")
	w.Header().Set("Access-Control-Allow-Methods", "*")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	switch r.URL.Path {
	case "/status", "/health":
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(p.Status())
		return
	case "/v1/models":
		p.handleModels(w, r)
		return
	}

	if !strings.HasPrefix(r.URL.Path, "/v1") {
		http.Error(w, `{"error":"use /v1/... paths"}`, http.StatusBadRequest)
		return
	}

	p.sem <- struct{}{}
	p.concurrent.Add(1)
	defer func() {
		p.concurrent.Add(-1)
		<-p.sem
	}()

	body, err := io.ReadAll(io.LimitReader(r.Body, bodyLimit))
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), http.StatusBadRequest)
		return
	}
	r.Body.Close()

	injectParams(&body)

	isStream := contains(r.Header.Get("Accept"), "text/event-stream")
	if !isStream && len(body) > 0 {
		var j struct{ Stream bool `json:"stream"` }
		if json.Unmarshal(body, &j) == nil {
			isStream = j.Stream
		}
	}

	model := reqModel(body)
	acclog.Printf("-> %s %s model=%s stream=%v bytes=%d", r.Method, r.URL.Path, model, isStream, len(body))

	if debugMode {
		dbglog.Printf("=== REQUEST %s %s ===\nHeaders: %+v\nBody: %s", r.Method, r.URL.String(), r.Header, string(body))
	}

	target := nvidiaBase + strings.TrimPrefix(r.URL.Path, "/v1")
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	fwd := http.Header{}
	for k, v := range r.Header {
		lk := strings.ToLower(k)
		if lk == "authorization" || lk == "host" || lk == "origin" {
			continue
		}
		fwd[k] = v
	}
	if r.Method == http.MethodPost && fwd.Get("Content-Type") == "" {
		fwd.Set("Content-Type", "application/json")
	}

	exclude := make(map[string]bool)
	cl := &http.Client{Timeout: 300 * time.Second}
	var lastResp *http.Response
	var used *Key
	var retries int
	var rateLimited bool

	for attempt := 0; attempt < max(len(p.keys)*2+2, 4); attempt++ {
		key := p.Pick(exclude, model)
		if key == nil {
			if attempt < 3 {
				time.Sleep(200 * time.Millisecond)
				continue
			}
			break
		}
		used = key

		req, err := http.NewRequest(r.Method, target, bytes.NewReader(body))
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), http.StatusInternalServerError)
			return
		}
		req.Header.Set("Authorization", "Bearer "+key.Key)
		req.Header.Set("Host", "integrate.api.nvidia.com")
		req.Header.Set("X-Forwarded-For", r.RemoteAddr)
		if r.Header.Get("X-Forwarded-Host") != "" {
			req.Header.Set("X-Forwarded-Host", r.Header.Get("X-Forwarded-Host"))
		}
		for k, v := range fwd {
			req.Header[k] = v
		}

		resp, err := cl.Do(req)
		if err != nil {
			acclog.Printf("!! upstream error: %v", err)
			http.Error(w, fmt.Sprintf(`{"error":"upstream: %s"}`, err), http.StatusBadGateway)
			return
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			rateLimited = true
			p.Cooldown(key, cooldown429)
			p.locks.lock(key.Name, model, modelLockout)
			resp.Body.Close()
			rh := rlHeaders(resp.Header)
			acclog.Printf("!! 429 [%s] model=%s key-cooldown=%v model-lockout=%v headers=%v", key.Name, model, cooldown429, modelLockout, rh)
			exclude[key.Name] = true
			if attempt > 0 {
				retries++
			}
			continue
		}

		rb, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		bodyStr := string(rb)
		if resp.StatusCode == http.StatusForbidden || strings.Contains(bodyStr, "ResourceExhausted") {
			p.locks.lock(key.Name, model, burstCooldown)
			acclog.Printf("!! 403/ResourceExhausted [%s] model=%s burst-backoff=%v", key.Name, model, burstCooldown)
			exclude[key.Name] = true
			if attempt > 0 {
				retries++
			}
			continue
		}

		if attempt > 0 {
			retries++
		}
		lastResp = &http.Response{
			StatusCode: resp.StatusCode,
			Header:     resp.Header.Clone(),
			Body:       io.NopCloser(bytes.NewReader(rb)),
		}
		break
	}

	elapsed := time.Since(start)

	if lastResp == nil {
		record := UsageRecord{
			Ts:          time.Now().UTC().Format(time.RFC3339Nano),
			Model:       model,
			Method:      r.Method,
			Path:        r.URL.Path,
			StatusCode:  503,
			DurationMs:  elapsed.Milliseconds(),
			Stream:      isStream,
			Error:       "all keys exhausted or on cooldown",
			RateLimited: rateLimited,
		}
		if used != nil {
			record.KeyName = used.Name
		}
		logUsage(record)
		http.Error(w, `{"error":"all keys exhausted or on cooldown"}`, http.StatusServiceUnavailable)
		return
	}

	var prompT, compT, totalT int
	var recErr string
	if lastResp.StatusCode != http.StatusOK {
		rb2, _ := io.ReadAll(lastResp.Body)
		lastResp.Body.Close()
		lastResp.Body = io.NopCloser(bytes.NewReader(rb2))
		recErr = strings.TrimSpace(string(rb2))
		if len(recErr) > 200 {
			recErr = recErr[:200]
		}
	} else {
		bodyCopy, _ := io.ReadAll(lastResp.Body)
		lastResp.Body.Close()
		lastResp.Body = io.NopCloser(bytes.NewReader(bodyCopy))
		if !isStream {
			prompT, compT, totalT = respTokens(bodyCopy)
		}
	}

	respHeaders := rlHeaders(lastResp.Header)
	record := UsageRecord{
		Ts:               time.Now().UTC().Format(time.RFC3339Nano),
		Model:            model,
		KeyName:          used.Name,
		Method:           r.Method,
		Path:             r.URL.Path,
		StatusCode:       lastResp.StatusCode,
		DurationMs:       elapsed.Milliseconds(),
		PromptTokens:     prompT,
		CompletionTokens: compT,
		TotalTokens:      totalT,
		Stream:           isStream,
		RetryAttempt:     retries,
		RateLimited:      rateLimited,
		Error:            recErr,
		ContentBytes:     int64(len(body)),
		Headers:          respHeaders,
	}
	logUsage(record)

	for k, v := range lastResp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(lastResp.StatusCode)

	var written int64
	var respBodyBuf *bytes.Buffer
	if debugMode && isStream && lastResp.StatusCode == http.StatusOK {
		respBodyBuf = &bytes.Buffer{}
	}

	if isStream && lastResp.StatusCode == http.StatusOK {
		if fl, ok := w.(http.Flusher); ok {
			buf := make([]byte, 4096)
			for {
				n, err := lastResp.Body.Read(buf)
				if n > 0 {
					w.Write(buf[:n])
					if respBodyBuf != nil {
						respBodyBuf.Write(buf[:n])
					}
					fl.Flush()
					written += int64(n)
				}
				if err != nil {
					break
				}
			}
		} else {
			written, _ = io.Copy(w, lastResp.Body)
		}
	} else {
		written, _ = io.Copy(w, lastResp.Body)
	}
	lastResp.Body.Close()
	if lastResp.StatusCode == http.StatusOK {
		p.Postpone(used, 1500*time.Millisecond)
	}

	sk := ""
	if used != nil {
		sk = fmt.Sprintf(" [%s]", used.Name)
	}
	acclog.Printf("<- %d %s %s %v %d bytes%s", lastResp.StatusCode, r.Method, r.URL.Path, elapsed.Round(time.Millisecond), written, sk)

	if debugMode {
		respStr := ""
		if respBodyBuf != nil {
			respStr = respBodyBuf.String()
		}
		dbglog.Printf("=== RESPONSE %s ===\nStatus: %d Duration: %v Key: %s\nBody: %s", r.URL.Path, lastResp.StatusCode, time.Since(start), used.Name, respStr)
	}
}

func contains(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}

func loadKeys(raw []byte) (map[string]string, error) {
	cleaned := stripComments(raw)
	var obj map[string]string
	if err := json.Unmarshal(cleaned, &obj); err == nil {
		return obj, nil
	}
	var arr []string
	if err := json.Unmarshal(cleaned, &arr); err != nil {
		return nil, err
	}
	m := make(map[string]string, len(arr))
	for i, k := range arr {
		m[fmt.Sprintf("key-%d", i)] = k
	}
	return m, nil
}

func watchKeys(p *Pool, path string) {
	var lastMod time.Time
	for {
		fi, err := os.Stat(path)
		if err == nil {
			mod := fi.ModTime()
			if !mod.Equal(lastMod) && !lastMod.IsZero() {
				raw, err := os.ReadFile(path)
				if err == nil {
					entries, err := loadKeys(raw)
					if err == nil && len(entries) > 0 {
						added, removed := p.Reload(entries)
						if added > 0 || removed > 0 {
							stat := p.Status()
							acclog.Printf("!! keys.jsonc reloaded: +%d -%d = %d keys, %d available", added, removed, stat.Total, stat.Available)
						}
					}
				}
			}
			lastMod = mod
		}
		time.Sleep(checkInterval)
	}
}

func watchModelParams(path string) {
	var lastMod time.Time
	for {
		fi, err := os.Stat(path)
		if err == nil {
			mod := fi.ModTime()
			if !mod.Equal(lastMod) && !lastMod.IsZero() {
				log.Printf("  model_params.md changed, reloading")
				loadModelParams(path)
			}
			lastMod = mod
		}
		time.Sleep(checkInterval)
	}
}

func initUsageLog() {
	f, err := os.OpenFile("nim-usage.jsonl", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("WARN: cannot open nim-usage.jsonl: %v", err)
		return
	}
	usageFd = f
	usageOut = json.NewEncoder(f)
}

func serverMain() {
	initLogging()
	initUsageLog()
	loadModelParams("model_params.md")

	kf := "keys.jsonc"
	if e := os.Getenv("KEY_FILE"); e != "" {
		kf = e
	}

	raw, err := os.ReadFile(kf)
	if err != nil {
		log.Fatalf("Cannot read %s: %v\n\nCreate %s with:\n{\n  // comments are ok\n  \"main\": \"nvapi-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\",\n  \"backup-1\": \"nvapi-yyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyy\"\n}", kf, err, kf)
	}

	entries, err := loadKeys(raw)
	if err != nil {
		log.Fatalf("Invalid JSON in %s: %v", kf, err)
	}
	if len(entries) == 0 {
		log.Fatalf("%s is empty", kf)
	}

	pool := newPool(entries)
	go watchKeys(pool, kf)
	go watchModelParams("model_params.md")

	port := os.Getenv("PORT")
	if port == "" {
		port = "5419"
	}
	addr := ":" + port

	stat := pool.Status()
	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	sort.Strings(names)
	log.Printf("NVIDIA NIM Proxy v%s — %d keys: %s", versionStr, stat.Total, strings.Join(names, ", "))
	log.Printf("  429 cooldown=%v, model-lockout=%v, burst-backoff=%v", cooldown429, modelLockout, burstCooldown)
	log.Printf("  Effective ~%d RPM (40 RPM/key × %d keys)", 40*stat.Total, stat.Total)
	log.Printf("  Usage tracking -> nim-usage.jsonl")
	log.Printf("  Keys hot-reload enabled (JSONC)")
	log.Print()
	log.Printf("Listening on %s", addr)
	log.Printf("baseurl = http://localhost%s/v1", addr)
	log.Printf("api_key = dummy")
	log.Print()
	log.Printf("Status: http://localhost%s/status", addr)

	mux := http.NewServeMux()
	mux.Handle("/", pool)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      310 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Print("shutting down...")
		srv.Close()
		if usageFd != nil {
			usageFd.Close()
		}
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
}

// probe mode

var probeModels = []string{
	"deepseek-ai/deepseek-v4-flash",
	"deepseek-ai/deepseek-v4-pro",
	"minimaxai/minimax-m3",
	"moonshotai/kimi-k2.6",
	"nvidia/nemotron-3-ultra-550b-a55b",
	"poolside/laguna-xs-2.1",
	"stepfun-ai/step-3.7-flash",
	"z-ai/glm-5.2",
}

func runProbe() {
	initUsageLog()

	kf := "keys.jsonc"
	if e := os.Getenv("KEY_FILE"); e != "" {
		kf = e
	}
	raw, err := os.ReadFile(kf)
	if err != nil {
		log.Fatalf("Cannot read %s: %v", kf, err)
	}
	entries, err := loadKeys(raw)
	if err != nil {
		log.Fatalf("Invalid JSON in %s: %v", kf, err)
	}
	if len(entries) == 0 {
		log.Fatalf("%s is empty", kf)
	}

	keyList := make([]string, 0, len(entries))
	for _, v := range entries {
		keyList = append(keyList, v)
	}

	payload := `{"model":"%s","messages":[{"role":"user","content":"hi"}],"max_tokens":10,"stream":false}`

	fmt.Println()
	fmt.Println("NVIDIA NIM Rate Limit Probe")
	fmt.Println("============================")
	fmt.Printf("Keys: %d\n", len(keyList))
	fmt.Printf("Testing %d models, 3 requests each (1s apart)\n", len(probeModels))
	fmt.Println()

	type result struct {
		model    string
		attempts []int
		rl       []string
		tokens   []int
		ok       int
		errs     int
	}

	var results []result

	for _, m := range probeModels {
		r := result{model: m}
		fmt.Printf("  %-45s ", m)

		for i := 0; i < 3; i++ {
			if i > 0 {
				time.Sleep(1 * time.Second)
			}

			key := keyList[rand.Intn(len(keyList))]
			body := fmt.Sprintf(payload, m)

			req, _ := http.NewRequest("POST", nvidiaBase+"/chat/completions", bytes.NewReader([]byte(body)))
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("Content-Type", "application/json")

			cl := &http.Client{Timeout: 120 * time.Second}
			start := time.Now()
			resp, err := cl.Do(req)
			if err != nil {
				r.errs++
				fmt.Printf("!")
				r.attempts = append(r.attempts, 0)
				continue
			}
			elapsed := time.Since(start).Milliseconds()

			rb, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			rh := rlHeaders(resp.Header)
			prompT, compT, _ := respTokens(rb)

			r.attempts = append(r.attempts, int(elapsed))

			if resp.StatusCode == 429 {
				var cd string
				if v, ok := rh["Retry-After"]; ok {
					cd = v
				} else if v, ok := rh["retry-after"]; ok {
					cd = v
				}
				rlStr := fmt.Sprintf("429:%s", cd)
				r.rl = append(r.rl, rlStr)
				r.tokens = append(r.tokens, 0)
				fmt.Printf("R")
			} else if resp.StatusCode == 200 {
				r.ok++
				r.tokens = append(r.tokens, prompT+compT)
				fmt.Printf(".")
			} else {
				r.errs++
				bodyStr := strings.TrimSpace(string(rb))
				if len(bodyStr) > 200 {
					bodyStr = bodyStr[:200]
				}
				r.tokens = append(r.tokens, 0)
				fmt.Printf("%d", resp.StatusCode)
			}

			rec := UsageRecord{
				Ts:               time.Now().UTC().Format(time.RFC3339Nano),
				Model:            m,
				KeyName:          short(key),
				Method:           "POST",
				Path:             "/v1/chat/completions",
				StatusCode:       resp.StatusCode,
				DurationMs:       elapsed,
				PromptTokens:     prompT,
				CompletionTokens: compT,
				TotalTokens:      prompT + compT,
				RetryAttempt:     i,
				RateLimited:      resp.StatusCode == 429,
				ContentBytes:     int64(len(body)),
				Headers:          rh,
			}
			logUsage(rec)
		}
		results = append(results, r)
		fmt.Println()
	}

	fmt.Println()
	fmt.Println("Results")
	fmt.Println("=======")
	fmt.Printf("%-45s  %-12s  %-12s  %s\n", "Model", "Attempts(ms)", "RateLimited", "Tokens(in+out)")
	fmt.Println(strings.Repeat("-", 100))
	for _, r := range results {
		ats := ""
		for i, a := range r.attempts {
			if i > 0 {
				ats += ","
			}
			ats += fmt.Sprintf("%d", a)
		}
		rlStr := ""
		for i, rl := range r.rl {
			if i > 0 {
				rlStr += ","
			}
			rlStr += rl
		}
		if rlStr == "" {
			rlStr = "-"
		}
		tokStr := ""
		for i, t := range r.tokens {
			if i > 0 {
				tokStr += ","
			}
			tokStr += fmt.Sprintf("%d", t)
		}
		fmt.Printf("%-45s  %-12s  %-12s  %s\n", r.model, ats, rlStr, tokStr)
	}
	fmt.Println()

	if usageFd != nil {
		usageFd.Close()
	}
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "probe" {
		runProbe()
		return
	}
	serverMain()
}
