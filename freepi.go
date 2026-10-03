package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// freepi backend: ad-supported free models via https://api.freepi.ai.
// mimics the official free-pi-cli: bearer jwt, one stable x-session-id
// per account, x-client-version, silent ad cycle (next + impression),
// meter polls. per-account lanes, never shared with zen lanes.

const freePiBase = "https://api.freepi.ai"

// leaseGrace is how long a locally-claimed lease is treated as live before
// a 409 is attributed to a crashed run instead of an in-flight request.
const leaseGrace = 3 * time.Second

func freepiEnabled() bool {
	c := cfg().Freepi
	return c.Enabled && len(c.Accounts) > 0
}

func freepiVersion() string {
	if v := cfg().Freepi.ClientVersion; v != "" {
		return v
	}
	return "0.2.19"
}

func freepiInlineEvery() int {
	if n := cfg().Freepi.AdInlineEvery; n > 0 {
		return n
	}
	return 5
}

// freepiQueueWait is how long a request waits for a busy account to free up
// (one live stream per jwt). 0 = 60s.
func freepiQueueWait() time.Duration {
	if s := cfg().Freepi.QueueWaitSecs; s > 0 {
		return time.Duration(s) * time.Second
	}
	return 60 * time.Second
}

// freepiAcct wraps one configured account with runtime state.
type freepiAcct struct {
	name      string
	jwt       string
	sessionID string // stable x-session-id, minted once (server 409s a 2nd stream)
	mu        sync.Mutex
	inflight  int
	turns     int
	lastAd    time.Time
	claimedAt time.Time // when inflight went 0->1
	models    []string
}

var (
	freepiMu    sync.Mutex
	freepiAccts = map[string]*freepiAcct{}
)

// freepiAccounts returns live account states, creating lane state on
// first use. jwt comes from config (never logged, never in /status).
func freepiAccounts() []*freepiAcct {
	c := cfg().Freepi
	freepiMu.Lock()
	defer freepiMu.Unlock()
	var out []*freepiAcct
	seen := map[string]bool{}
	for _, a := range c.Accounts {
		if a.Name == "" || a.JWT == "" {
			continue
		}
		seen[a.Name] = true
		fa, ok := freepiAccts[a.Name]
		if !ok {
			fa = &freepiAcct{name: a.Name, sessionID: "fp-" + randHex(12)}
			freepiAccts[a.Name] = fa
		}
		fa.jwt = a.JWT
		out = append(out, fa)
	}
	for name := range freepiAccts {
		if !seen[name] {
			delete(freepiAccts, name)
		}
	}
	return out
}

// freepiHeaders sets the exact official-client headers. no User-Agent:
// fetch default is what the cli sends.
func freepiHeaders(req *http.Request, fa *freepiAcct) {
	req.Header.Set("Authorization", "Bearer "+fa.jwt)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-session-id", fa.sessionID)
	req.Header.Set("x-client-version", freepiVersion())
}

func freepiGet(fa *freepiAcct, path string, timeout time.Duration) (int, []byte) {
	req, err := http.NewRequest("GET", freePiBase+path, nil)
	if err != nil {
		return 0, nil
	}
	freepiHeaders(req, fa)
	cl := &http.Client{Timeout: timeout}
	resp, err := cl.Do(req)
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, b
}

// freepiAdCycle runs the silent ad support: banner at lane start + 10min,
// inline every Nth turn, impression once per fresh click_token, meter per
// turn. failures swallowed. nothing is displayed anywhere.
func freepiAdCycle(fa *freepiAcct, forceBanner bool) {
	now := time.Now()
	if forceBanner || now.Sub(fa.lastAd) > 10*time.Minute {
		if code, body := freepiGet(fa, "/ads/next?slot=banner", 3*time.Second); code == 200 {
			freepiImpress(fa, body)
		}
		fa.lastAd = now
	}
	fa.turns++
	if fa.turns%freepiInlineEvery() == 0 {
		if code, body := freepiGet(fa, "/ads/next?slot=inline", 3*time.Second); code == 200 {
			freepiImpress(fa, body)
		}
	}
	// meter poll every turn, ignored (server notice handled client-side).
	freepiGet(fa, "/me", 3*time.Second)
}

func freepiImpress(fa *freepiAcct, adBody []byte) {
	var ad struct {
		AdID       string `json:"ad_id"`
		ClickToken string `json:"click_token"`
	}
	if json.Unmarshal(adBody, &ad) != nil || ad.AdID == "" || ad.ClickToken == "" {
		return
	}
	payload, _ := json.Marshal(map[string]string{"ad_id": ad.AdID, "click_token": ad.ClickToken})
	req, err := http.NewRequest("POST", freePiBase+"/ads/impression", bytes.NewReader(payload))
	if err != nil {
		return
	}
	freepiHeaders(req, fa)
	cl := &http.Client{Timeout: 3 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

// freepiErr classifies the server error envelope.
type freepiErr struct {
	Status int
	Code   string
	Msg    string
	Retry  string // retry-after header when present
}

func parseFreepiErr(status int, body []byte, hdr http.Header) freepiErr {
	fe := freepiErr{Status: status, Retry: hdr.Get("Retry-After")}
	var env struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &env) == nil {
		fe.Code, fe.Msg = env.Code, env.Message
	}
	if fe.Msg == "" {
		fe.Msg = strings.TrimSpace(string(body))
		if len(fe.Msg) > 200 {
			fe.Msg = fe.Msg[:200]
		}
	}
	return fe
}

// freepiModels returns catalog ids via /client-version, fallback builtin.
func freepiModels(fa *freepiAcct) []string {
	if len(fa.models) > 0 {
		return fa.models
	}
	_, body := freepiGet(fa, "/client-version", 5*time.Second)
	var cv struct {
		Models []struct {
			ID string `json:"id"`
		} `json:"models"`
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &cv) == nil {
		for _, m := range cv.Models {
			if m.ID != "" {
				fa.models = append(fa.models, m.ID)
			}
		}
		if len(fa.models) == 0 && cv.Model != "" {
			fa.models = []string{cv.Model}
		}
	}
	if len(fa.models) == 0 {
		fa.models = []string{"deepseek/deepseek-v4-flash"}
	}
	return fa.models
}

// freepiSessionReset releases a stale session lease (client's
// /close-other-session). only on 409 concurrent_session.
func freepiSessionReset(fa *freepiAcct) {
	req, err := http.NewRequest("POST", freePiBase+"/session/reset", nil)
	if err != nil {
		return
	}
	freepiHeaders(req, fa)
	cl := &http.Client{Timeout: 5 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
}

// freepiSnapshot builds the /status freepi section: per-account models,
// request counts (from usage totals), inflight state. jwt never leaves.
func freepiSnapshot() *freepiSummary {
	sum := &freepiSummary{Enabled: true}
	for _, fa := range freepiAccounts() {
		st := freepiAcctStatus{Name: fa.name, Models: freepiModels(fa)}
		fa.mu.Lock()
		st.Inflight = fa.inflight > 0
		fa.mu.Unlock()
		ku := keyUsage("freepi:" + fa.name)
		st.Requests = ku.Requests
		st.Prompt = ku.Prompt
		st.Completion = ku.Completion
		sum.Accounts = append(sum.Accounts, st)
	}
	return sum
}

// handleFreepiAnthropic serves /v1/messages for freeepi/<model>: same
// account rotation, then converts the openai answer back to the
// Anthropic Messages shape (stream or not).
func (p *Pool) handleFreepiAnthropic(w http.ResponseWriter, r *http.Request, oaiBody []byte, clientModel, upstreamModel string, tnames toolNames, isStream bool, start time.Time, reqID string) {
	realModel := strings.TrimPrefix(upstreamModel, "freeepi/")
	var m map[string]any
	if json.Unmarshal(oaiBody, &m) == nil {
		m["model"] = realModel
		stripCacheFields(m)
		m["stream"] = false // fetch the whole answer, convert here
		if b, err := json.Marshal(m); err == nil {
			oaiBody = b
		}
	}
	acclog.Printf("%s routed to freepi model=%s", reqID, realModel)

	_, rb, acct, ok := freepiFetch(r.Method, oaiBody, realModel)
	if !ok {
		acclog.Printf("!! freepi failed model=%s err=%q", realModel, acct)
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "freepi: "+acct)
		return
	}
	prompT, compT, totalT := respTokens(rb)
	logReq := func(stream bool, contentBytes int64) {
		logUsage(UsageRecord{
			Ts:    time.Now().UTC().Format(time.RFC3339Nano),
			Model: upstreamModel, KeyName: "freepi:" + acct, Method: r.Method,
			Path: r.URL.Path, StatusCode: 200,
			DurationMs:   time.Since(start).Milliseconds(),
			PromptTokens: prompT, CompletionTokens: compT, TotalTokens: totalT,
			Stream: stream, ContentBytes: contentBytes,
		})
	}

	if isStream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		written, _, _ := streamAnthropic(w, rb, clientModel, tnames)
		logReq(true, written)
		acclog.Printf("<- 200 POST /v1/messages %v %d bytes [freepi:%s] model=%s %s",
			time.Since(start).Round(time.Millisecond), written, acct, realModel, reqID)
		return
	}
	out, errMsg, _ := openAIToAnthropic(rb, clientModel, tnames)
	if errMsg != "" {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", errMsg)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(out)
	logReq(false, int64(len(out)))
	acclog.Printf("<- 200 POST /v1/messages %v %d bytes [freepi:%s] model=%s %s",
		time.Since(start).Round(time.Millisecond), len(out), acct, realModel, reqID)
}

// freepiFetch performs one inference against a free account. the server
// allows one live stream per jwt, so callers claim a slot (queueing when
// every account is mid-request) and rotate accounts on failure. a 409 on a
// freshly claimed slot means a stale server-side lease (a crashed run):
// this process serializes locally, so no live local request owns it and
// the documented /session/reset is safe.
func freepiFetch(method string, body []byte, model string) (*http.Response, []byte, string, bool) {
	accts := freepiAccounts()
	if len(accts) == 0 {
		return nil, nil, "no freepi accounts configured", false
	}
	target := freePiBase + "/v1/chat/completions"
	deadline := time.Now().Add(freepiQueueWait())
	var lastErr string
	tried := map[string]time.Time{} // account -> when we last used it

	for {
		// claim a slot: free-and-untried first, else the coldest tried one.
		var fa *freepiAcct
		for _, c := range accts {
			c.mu.Lock()
			free := c.inflight == 0
			c.mu.Unlock()
			if !free {
				continue
			}
			last, done := tried[c.name]
			if !done {
				fa = c
				break
			}
			if fa == nil {
				fa = c
				break
			}
			if faLast, fd := tried[fa.name]; fd && last.Before(faLast) {
				fa = c
			}
		}
		if fa != nil {
			fa.mu.Lock()
			if fa.inflight != 0 {
				fa.mu.Unlock()
				fa = nil
			} else {
				fa.inflight++
				fa.mu.Unlock()
			}
		}
		if fa == nil {
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(150 * time.Millisecond)
			continue
		}

		resp, rb, acct, errMsg, retry := freepiTry(method, target, body, fa, model)
		fa.mu.Lock()
		fa.inflight--
		fa.mu.Unlock()
		if errMsg == "" {
			return resp, rb, acct, true
		}
		lastErr = errMsg
		tried[fa.name] = time.Now()
		if !retry && time.Now().After(deadline) {
			break
		}
		if !retry && len(tried) >= len(accts) && time.Now().After(deadline) {
			break
		}
	}
	if lastErr == "" {
		lastErr = "all accounts busy (queue wait exceeded)"
	}
	return nil, nil, lastErr, false
}

// freepiTry makes one upstream attempt with lease recovery. retry=true
// asks the caller to loop again (lease was released, or account failed).
func freepiTry(method, target string, body []byte, fa *freepiAcct, model string) (*http.Response, []byte, string, string, bool) {
	go freepiAdCycle(fa, fa.turns == 0)
	var lastErr string
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequest(method, target, bytes.NewReader(body))
		if err != nil {
			return nil, nil, "", err.Error(), false
		}
		freepiHeaders(req, fa)
		resp, err := (&http.Client{Timeout: 300 * time.Second}).Do(req)
		if err != nil {
			acclog.Printf("!! freepi net error acct=%s model=%s: %v", fa.name, model, err)
			return nil, nil, "", err.Error(), false
		}
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, upstreamBodyLimit))
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return resp, rb, fa.name, "", false
		}
		fe := parseFreepiErr(resp.StatusCode, rb, resp.Header)
		if resp.StatusCode == http.StatusConflict && fe.Code == "concurrent_session" && attempt == 0 {
			// this process serializes per account, so a 409 on our own slot
			// is a stale lease from a crashed run. release it.
			acclog.Printf("  freepi acct=%s stale session lease, releasing", fa.name)
			freepiSessionReset(fa)
			time.Sleep(leaseGrace)
			lastErr = fe.Msg
			continue
		}
		acclog.Printf("!! freepi %d [%s] acct=%s model=%s code=%s err=%q",
			resp.StatusCode, fe.Code, fa.name, model, fe.Code, fe.Msg)
		if resp.StatusCode == http.StatusForbidden && fe.Code == "ads_required" {
			go freepiAdCycle(fa, true)
		}
		return nil, nil, "", fe.Msg, true
	}
	return nil, nil, "", lastErr, true
}

// handleFreepi routes freeepi/<model> to api.freepi.ai with account
// rotation: one concurrent stream per account (server 409s a second),
// failover across accounts on 429/409/502/503.
func (p *Pool) handleFreepi(w http.ResponseWriter, r *http.Request, body []byte, model string, isStream bool, start time.Time, reqID string) {
	realModel := strings.TrimPrefix(model, "freeepi/")
	var m map[string]any
	if json.Unmarshal(body, &m) == nil {
		m["model"] = realModel
		stripCacheFields(m)
		if b, err := json.Marshal(m); err == nil {
			body = b
		}
	}
	acclog.Printf("%s routed to freepi model=%s", reqID, realModel)

	resp, rb, acct, ok := freepiFetch(r.Method, body, realModel)
	if !ok {
		acclog.Printf("!! freepi failed model=%s err=%q", realModel, acct)
		http.Error(w, fmt.Sprintf(`{"error":"freepi: %s"}`, acct), http.StatusBadGateway)
		return
	}
	prompT, compT, totalT := respTokens(rb)
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	if !isStream {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(http.StatusOK)
	w.Write(rb)
	logUsage(UsageRecord{
		Ts:    time.Now().UTC().Format(time.RFC3339Nano),
		Model: model, KeyName: "freepi:" + acct, Method: r.Method,
		Path: r.URL.Path, StatusCode: 200,
		DurationMs:   time.Since(start).Milliseconds(),
		PromptTokens: prompT, CompletionTokens: compT, TotalTokens: totalT,
		Stream: isStream, ContentBytes: int64(len(body)),
	})
	acclog.Printf("<- 200 %s %s %v [freepi:%s] model=%s %s",
		r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond), acct, realModel, reqID)
}
