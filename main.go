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
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	nvidiaBase   = "https://integrate.api.nvidia.com/v1"
	OpencodeBase = "https://opencode.ai/zen/v1"
	cooldown429   = 1 * time.Minute
	maxBackoff    = 16 * time.Minute
	failWindow    = 50 * time.Minute
	idleScale     = 10 * time.Minute
	modelLockout  = 30 * time.Second
	burstCooldown = 5 * time.Second
	bodyLimit     = 10 << 20
	checkInterval = 5 * time.Second
)

// versionStr is overridden at build time via -ldflags "-X main.versionStr=x.y.z".
var versionStr = "2.6.0"

type Key struct {
	Name           string
	Key            string
	CooldownUntil  time.Time
	CooldownReason string
	LastUsed       time.Time
	FailCount      int
	Consec429      int
	LastFail       time.Time
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
	// lastKey pins a model to the key that last served it, so a conversation
	// stays on one key and NIM's per-key prompt cache stays warm.
	lastKey map[string]string
}

func newPool(entries map[string]string) *Pool {
	p := &Pool{start: time.Now(), lastKey: make(map[string]string)}
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
	return p.PickSticky(exclude, model, "")
}

// PickSticky picks a key, preferring the last key that served model (sticky)
// when it is still available, so prompt caches stay warm. Sticky keys only win
// if usable; otherwise fall back to the normal weighted pick.
func (p *Pool) PickSticky(exclude map[string]bool, model, sticky string) *Key {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	var avail []*Key
	var stickyKey *Key
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
		if k.Name == sticky {
			stickyKey = k
		}
	}
	if len(avail) == 0 {
		return nil
	}
	if stickyKey != nil {
		stickyKey.LastUsed = now
		return stickyKey
	}
	// weighted pick: favor idle keys, sink recently-failed/hot keys
	total := 0.0
	weights := make([]float64, len(avail))
	for i, k := range avail {
		w := idleWeight(k, now)
		total += w
		weights[i] = w
	}
	r := rand.Float64() * total
	pick := avail[len(avail)-1]
	for i, w := range weights {
		r -= w
		if r <= 0 {
			pick = avail[i]
			break
		}
	}
	pick.LastUsed = now
	return pick
}

// noteKey records that model was last served by key name, pinning stickiness.
func (p *Pool) noteKey(model, name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastKey[model] = name
}

// stickyKey returns the name of the key that last served model, if any.
func (p *Pool) stickyKey(model string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lastKey[model]
}

// idleWeight scores a key 0..1: more idle -> higher, failed within window -> crushed.
func idleWeight(k *Key, now time.Time) float64 {
	w := 1.0
	if !k.LastUsed.IsZero() {
		idle := now.Sub(k.LastUsed)
		if idle < 0 {
			idle = 0
		}
		// 0 at 0 idle, approach 1 as idle grows past idleScale
		f := float64(idle) / float64(idleScale)
		if f > 1 {
			f = 1
		}
		// balanced: floor at 0.3 so never-idle keys still have a shot
		w = 0.3 + 0.7*f
	}
	if !k.LastFail.IsZero() && now.Sub(k.LastFail) < failWindow {
		// failed within the window: steep penalty, escalates with consecutive 429s
		pen := 0.2
		for i := 0; i < k.Consec429 && i < 4; i++ {
			pen *= 0.5
		}
		w *= pen
	}
	return w
}

func (p *Pool) rateLimit(k *Key) {
	p.mu.Lock()
	defer p.mu.Unlock()
	k.Consec429++
	k.FailCount++
	k.LastFail = time.Now()
	d := cooldown429 << (k.Consec429 - 1)
	if d > maxBackoff {
		d = maxBackoff
	}
	k.CooldownUntil = time.Now().Add(d)
	k.CooldownReason = "rate_limited_429"
}

func (p *Pool) Clear429(k *Key) {
	p.mu.Lock()
	defer p.mu.Unlock()
	k.Consec429 = 0
}

func (p *Pool) Postpone(k *Key, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	k.CooldownUntil = time.Now().Add(d)
	k.CooldownReason = "post_request"
}

// callUpstream runs the retry loop against NIM and returns the result.
// Returns (nil, nil) when all keys exhausted, (nil, error) on hard error.
func (p *Pool) callUpstream(method, target string, body []byte, fwd http.Header, model, remoteAddr string) (*upstreamResult, error) {
	exclude := make(map[string]bool)
	cl := &http.Client{Timeout: 300 * time.Second}
	var retries int
	var rateLimited bool
	var used *Key

	sticky := p.stickyKey(model)
	for attempt := 0; attempt < max(len(p.keys)*2+2, 4); attempt++ {
		key := p.PickSticky(exclude, model, sticky)
		if key == nil {
			if attempt < 3 {
				time.Sleep(200 * time.Millisecond)
				continue
			}
			dbglog.Printf("no keys available at attempt %d (excluded=%d, model=%s)", attempt, len(exclude), model)
			break
		}
		used = key

		req, err := http.NewRequest(method, target, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("NewRequest: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+key.Key)
		req.Header.Set("Host", "integrate.api.nvidia.com")
		req.Header.Set("X-Forwarded-For", remoteAddr)
		for k, v := range fwd {
			req.Header[k] = v
		}

		resp, err := cl.Do(req)
		if err != nil {
			return nil, fmt.Errorf("upstream: %w", err)
		}

		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == 529 {
			rateLimited = true
			p.rateLimit(key)
			p.locks.lock(key.Name, model, modelLockout)
			resp.Body.Close()
			rh := rlHeaders(resp.Header)
			cd := cooldown429 << (key.Consec429 - 1)
			if cd > maxBackoff {
				cd = maxBackoff
			}
			acclog.Printf("!! %d [%s] model=%s key-backoff=%v model-lockout=%v headers=%v", resp.StatusCode, key.Name, model, cd, modelLockout, rh)
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
		p.noteKey(model, used.Name)
		return &upstreamResult{
			StatusCode:  resp.StatusCode,
			Header:      resp.Header.Clone(),
			Body:        rb,
			Key:         used.Name,
			KeyObj:      used,
			Retries:     retries,
			RateLimited: rateLimited,
		}, nil
	}

	return nil, nil
}

type KeyStatus struct {
	Name           string `json:"name"`
	Suffix         string `json:"suffix"`
	Cooldown       string `json:"cooldown,omitempty"`
	CooldownReason string `json:"cooldown_reason,omitempty"`
	LastUsed       string `json:"last_used,omitempty"`
	FailCount      int    `json:"fail_count"`
	Consec429      int    `json:"consec_429"`
	Available      bool   `json:"available"`
}

type StatusResponse struct {
	OK         bool          `json:"ok"`
	Version    string        `json:"version"`
	Uptime     string        `json:"uptime"`
	Total      int           `json:"total"`
	Available  int           `json:"available"`
	Concurrent int           `json:"concurrent"`
	SemLimit   int           `json:"sem_limit"`
	Keys       []KeyStatus   `json:"keys"`
	Locks      interface{}   `json:"model_locks,omitempty"`
	Opencode   *OpencodeInfo `json:"opencode,omitempty"`
}

type OpencodeInfo struct {
	Enabled bool     `json:"enabled"`
	Base    string   `json:"base"`
	Models  []string `json:"models"`
}

// upstreamResult holds the outcome of a callUpstream attempt.
type upstreamResult struct {
	StatusCode  int
	Header      http.Header
	Body        []byte
	Key         string
	KeyObj      *Key
	Retries     int
	RateLimited bool
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
		ks := KeyStatus{Name: k.Name, Suffix: sk, FailCount: k.FailCount, Consec429: k.Consec429}
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
	sr.Opencode = ocInfo.Load()
	return sr
}

var (
	acclog    *log.Logger
	dbglog    *log.Logger
	debugMode bool
)

// ocInfo caches the opencode zen free-model list fetched at startup.
var ocInfo atomic.Pointer[OpencodeInfo]

// refreshOpencodeModels pulls the free model list from opencode zen and caches it.
func refreshOpencodeModels() {
	info := &OpencodeInfo{Enabled: true, Base: OpencodeBase}
	cl := &http.Client{Timeout: 10 * time.Second}
	resp, err := cl.Get(OpencodeBase + "/models")
	if err == nil {
		var oc struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if json.NewDecoder(resp.Body).Decode(&oc) == nil {
			for _, m := range oc.Data {
				if strings.HasSuffix(m.ID, "-free") {
					info.Models = append(info.Models, "opencode/"+m.ID)
				}
			}
		}
		resp.Body.Close()
	}
	sort.Strings(info.Models)
	ocInfo.Store(info)
	log.Printf("  Opencode zen: %d free models", len(info.Models))
}

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
	clean := stripComments(raw)
	var entries []struct {
		Pattern string         `json:"pattern"`
		Params  map[string]any `json:"params"`
	}
	if err := json.Unmarshal(clean, &entries); err != nil {
		log.Printf("WARN: %s: %v", path, err)
		return
	}
	for _, e := range entries {
		if e.Pattern == "" {
			continue
		}
		if e.Params == nil {
			e.Params = make(map[string]any)
		}
		modelParams = append(modelParams, &modelParamEntry{pattern: strings.ToLower(e.Pattern), params: e.Params})
	}
	log.Printf("  Loaded %d model param entries from %s", len(modelParams), path)
}

func matchModelParams(model string) map[string]any {
	if e := matchesModelParams(model); e != nil {
		return e.params
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
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	Description      string            `json:"description,omitempty"`
	ContextLength    int               `json:"context_length"`
	Pricing          map[string]string `json:"pricing"`
	Architecture     map[string]any    `json:"architecture"`
	TopProvider      map[string]any    `json:"top_provider"`
	PerRequestLimits any               `json:"per_request_limits"`
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
		mc := &http.Client{Timeout: 10 * time.Second}
		req, _ := http.NewRequest("GET", nvidiaBase+"/models", nil)
		req.Header.Set("Authorization", "Bearer "+key.Key)
		resp, err := mc.Do(req)
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
				"modality":      "text->text",
				"tokenizer":     "Other",
				"instruct_type": nil,
			},
			TopProvider: map[string]any{
				"context_length":        cl,
				"max_completion_tokens": nil,
				"is_moderated":          false,
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
				ID:               p,
				Name:             modelDisplayName(p),
				Description:      desc,
				ContextLength:    cl,
				Pricing:          map[string]string{"prompt": "0", "completion": "0", "request": "0"},
				Architecture:     map[string]any{"modality": "text->text", "tokenizer": "Other", "instruct_type": nil},
				TopProvider:      map[string]any{"context_length": cl, "max_completion_tokens": nil, "is_moderated": false},
				PerRequestLimits: nil,
			})
		}
		modelMu.RUnlock()
	}

	// opencode zen free models (no auth) -> opencode/<id>
	ocClient := &http.Client{Timeout: 10 * time.Second}
	if ocResp, err := ocClient.Get(OpencodeBase + "/models"); err == nil {
		var oc struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if json.NewDecoder(ocResp.Body).Decode(&oc) == nil {
			ocSeen := make(map[string]bool)
			for _, m := range oc.Data {
				if !strings.HasSuffix(m.ID, "-free") || ocSeen[m.ID] {
					continue
				}
				ocSeen[m.ID] = true
				cl := 131072
				desc := ""
				if e := matchesModelParams(m.ID); e != nil {
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
				}
				ocID := "opencode/" + m.ID
				out.Data = append(out.Data, openRouterModel{
					ID:            ocID,
					Name:          modelDisplayName(ocID),
					Description:   desc,
					ContextLength: cl,
					Pricing:       map[string]string{"prompt": "0", "completion": "0", "request": "0"},
					Architecture:  map[string]any{"modality": "text->text", "tokenizer": "Other", "instruct_type": nil},
					TopProvider:   map[string]any{"context_length": cl, "max_completion_tokens": nil, "is_moderated": false},
				})
			}
		}
		ocResp.Body.Close()
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
	"frequencyPenalty":  "frequency_penalty",
	"presencePenalty":   "presence_penalty",
	"repetitionPenalty": "repetition_penalty",
	"stopSequences":     "stop",
	"reasoningEffort":   "reasoning_effort",
	"reasoningBudget":   "reasoning_budget",
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

	// Anthropic Messages API — route to anthropic.go handler
	if r.URL.Path == "/v1/messages" || r.URL.Path == "/v1/messages/count_tokens" {
		p.handleAnthropic(w, r, start)
		return
	}
	// Permission classifier — Claude Code asks the gateway to classify tool-use
	// permission requests; always approve so sessions never stall on a prompt.
	if r.URL.Path == "/v1/messages/classifier" {
		handleClassifier(w, r)
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
		acclog.Printf("!! 400 bad-request-body %s %s: %v", r.Method, r.URL.Path, err)
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), http.StatusBadRequest)
		return
	}
	r.Body.Close()

	injectParams(&body)

	isStream := contains(r.Header.Get("Accept"), "text/event-stream")
	if !isStream && len(body) > 0 {
		var j struct {
			Stream bool `json:"stream"`
		}
		if json.Unmarshal(body, &j) == nil {
			isStream = j.Stream
		}
	}

	model := reqModel(body)
	acclog.Printf("-> %s %s model=%s stream=%v bytes=%d", r.Method, r.URL.Path, model, isStream, len(body))

	if strings.HasPrefix(model, "opencode/") {
		p.handleOpenCode(w, r, body, model, isStream, start)
		return
	}

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
	sticky := p.stickyKey(model)

	for attempt := 0; attempt < max(len(p.keys)*2+2, 4); attempt++ {
		key := p.PickSticky(exclude, model, sticky)
		if key == nil {
			if attempt < 3 {
				time.Sleep(200 * time.Millisecond)
				continue
			}
			dbglog.Printf("no keys available at attempt %d (excluded=%d, model=%s)", attempt, len(exclude), model)
			break
		}
		used = key

		req, err := http.NewRequest(r.Method, target, bytes.NewReader(body))
		if err != nil {
			acclog.Printf("!! 500 internal NewRequest-failed %s %s: %v", r.Method, target, err)
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
			acclog.Printf("!! 502 upstream-error [%s] %s %s: %v", key.Name, r.Method, target, err)
			http.Error(w, fmt.Sprintf(`{"error":"upstream: %s"}`, err), http.StatusBadGateway)
			return
		}

		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == 529 {
			rateLimited = true
			p.rateLimit(key)
			p.locks.lock(key.Name, model, modelLockout)
			resp.Body.Close()
			rh := rlHeaders(resp.Header)
			cd := cooldown429 << (key.Consec429 - 1)
			if cd > maxBackoff {
				cd = maxBackoff
			}
			acclog.Printf("!! %d [%s] model=%s key-backoff=%v model-lockout=%v headers=%v", resp.StatusCode, key.Name, model, cd, modelLockout, rh)
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
		if resp.StatusCode == http.StatusOK {
			p.noteKey(model, key.Name)
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
		acclog.Printf("<- 503 %s %s %v (all keys exhausted: excluded=%d, rate_limited=%v, model=%s)", r.Method, r.URL.Path, elapsed.Round(time.Millisecond), len(exclude), rateLimited, model)
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
		acclog.Printf("!! upstream %d [%s] model=%s err=%q", lastResp.StatusCode, used.Name, model, recErr)
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
		p.Clear429(used)
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

// handleOpenCode routes opencode/<model> requests to the opencode zen API
// (OpenAI-compatible, no auth for -free models).
func (p *Pool) handleOpenCode(w http.ResponseWriter, r *http.Request, body []byte, model string, isStream bool, start time.Time) {
	realModel := strings.TrimPrefix(model, "opencode/")
	var m map[string]any
	if json.Unmarshal(body, &m) == nil {
		m["model"] = realModel
		if b, err := json.Marshal(m); err == nil {
			body = b
		}
	}

	target := OpencodeBase + strings.TrimPrefix(r.URL.Path, "/v1")
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	cl := &http.Client{Timeout: 300 * time.Second}
	var resp *http.Response
	var prompT, compT, totalT int
	var recErr string

	for zenRetries := 0; zenRetries < 5; zenRetries++ {
		req, err := http.NewRequest(r.Method, target, bytes.NewReader(body))
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), http.StatusInternalServerError)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer public")
		req.Header.Set("x-opencode-client", "desktop")
		if zenRetries == 0 {
			if s := r.Header.Get("x-session-id"); s != "" {
				req.Header.Set("x-opencode-session", s)
			} else {
				req.Header.Set("x-opencode-session", "ses_"+randHex(20))
			}
		} else {
			req.Header.Set("x-opencode-session", "ses_"+randHex(20))
			acclog.Printf("  opencode retry %d/4 rotating session", zenRetries)
		}
		req.Header.Set("User-Agent", "opencode/1.18.25")
		req.Header.Set("Accept", "text/event-stream")
		for k, v := range r.Header {
			switch strings.ToLower(k) {
			case "authorization", "host", "content-type", "accept", "x-opencode-client":
				continue
			default:
				req.Header[k] = v
			}
		}

		resp, err = cl.Do(req)
		if err != nil {
			acclog.Printf("!! 502 opencode upstream-error %s: %v", target, err)
			http.Error(w, fmt.Sprintf(`{"error":"upstream: %s"}`, err), http.StatusBadGateway)
			return
		}

		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == 529 {
			resp.Body.Close()
			time.Sleep(time.Duration(zenRetries+1) * time.Second)
			continue
		}
		break
	}

	if isStream && resp.StatusCode == http.StatusOK {
		// true streaming — pipe through, no token capture
	} else {
		rb, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(rb))
		if resp.StatusCode != http.StatusOK {
			recErr = strings.TrimSpace(string(rb))
			if len(recErr) > 200 {
				recErr = recErr[:200]
			}
			acclog.Printf("!! opencode upstream %d model=%s err=%q", resp.StatusCode, model, recErr)
		} else {
			prompT, compT, totalT = respTokens(rb)
		}
	}

	logUsage(UsageRecord{
		Ts:               time.Now().UTC().Format(time.RFC3339Nano),
		Model:            model,
		KeyName:          "opencode",
		Method:           r.Method,
		Path:             r.URL.Path,
		StatusCode:       resp.StatusCode,
		DurationMs:       time.Since(start).Milliseconds(),
		PromptTokens:     prompT,
		CompletionTokens: compT,
		TotalTokens:      totalT,
		Stream:           isStream,
		Error:            recErr,
		ContentBytes:     int64(len(body)),
	})

	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)

	var written int64
	if isStream && resp.StatusCode == http.StatusOK {
		if fl, ok := w.(http.Flusher); ok {
			buf := make([]byte, 4096)
			for {
				n, err := resp.Body.Read(buf)
				if n > 0 {
					w.Write(buf[:n])
					fl.Flush()
					written += int64(n)
				}
				if err != nil {
					break
				}
			}
		} else {
			written, _ = io.Copy(w, resp.Body)
		}
	} else {
		written, _ = io.Copy(w, resp.Body)
	}
	resp.Body.Close()
	acclog.Printf("<- %d %s %s %v %d bytes [opencode]", resp.StatusCode, r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond), written)
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
				log.Printf("  model_params.jsonc changed, reloading")
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
	loadModelParams("model_params.jsonc")
	loadClaudeModels("claude_models.jsonc")
	refreshOpencodeModels()

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
	go watchModelParams("model_params.jsonc")
	go watchClaudeModels("claude_models.jsonc")

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
	log.Printf("  429 backoff=%v..%v (exp, reset on success), model-lockout=%v, burst-backoff=%v", cooldown429, maxBackoff, modelLockout, burstCooldown)
	log.Printf("  weighted key pick: idle-preference + 50m failure window")
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
	"minimaxai/minimax-m3",
	"moonshotai/kimi-k2.6",
	"nvidia/nemotron-3-ultra-550b-a55b",
	"nvidia/nemotron-3.5-lightning-30b-a3b",
	"poolside/laguna-xs-2.1",
	"stepfun-ai/step-3.7-flash",
	"deepseek-ai/deepseek-v4-flash-0731",
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
