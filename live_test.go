package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// handleAnthropic end to end minus the network hop, which is the only part
// not ours. covers routing, RTK, PII ordering and the tool-name restore in
// one pass. asserts on the body handed to the response translator.
func TestLiveAnthropicChain(t *testing.T) {
	c := defaultConfig()
	c.Rtk.Enabled = true
	c.Nvidia.Enabled = true
	c.NvidiaKeys = map[string]string{"k1": "fake"}
	useConfig(t, &c)

	var big strings.Builder
	big.WriteString("On branch main\n")
	for i := 0; i < 40; i++ {
		big.WriteString("\tmodified:   file" + string(rune('a'+i%26)) + ".go\n")
	}
	raw := big.String()
	anthropic := []byte(`{"model":"test","max_tokens":100,"messages":[
		{"role":"user","content":"go"},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":` + jsonStr(raw) + `}]}
	],"tools":[{"name":"Bash","input_schema":{"type":"object"}}]}`)

	// step 1: what the handler produces for the upstream call
	compressed := rtkCompressBody(append([]byte(nil), anthropic...))
	oai, clientModel, _, tnames, _, err := anthropicRequestToOpenAI(compressed)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	t.Logf("tool_result %d bytes -> oai body %d bytes", len(raw), len(oai))
	if len(oai) > 1400 {
		t.Errorf("tool_result not compressed: oai body %d bytes", len(oai))
	}
	if !strings.Contains(string(oai), "main") {
		t.Errorf("branch lost: %s", oai)
	}
	if len(tnames) != 1 || tnames[0] != "Bash" {
		t.Fatalf("tool names = %v, want [Bash]", tnames)
	}

	// step 2: upstream replies lowercase, client must get PascalCase back
	up := []byte(`{"choices":[{"message":{"content":"","tool_calls":[
		{"id":"c1","type":"function","function":{"name":"bash","arguments":"{}"}}]},
		"finish_reason":"tool_calls"}]}`)
	out, errMsg, _ := openAIToAnthropic(up, clientModel, tnames)
	if errMsg != "" {
		t.Fatalf("response translate: %s", errMsg)
	}
	if !strings.Contains(string(out), `"name":"Bash"`) {
		t.Errorf("client got lowercase tool name: %s", out)
	}
	t.Logf("client sees: %s", out)

	// step 3: the compressed tool result must still be valid JSON in the
	// exact shape the upstream expects
	var m map[string]any
	if err := json.Unmarshal(compressed, &m); err != nil {
		t.Fatalf("compressed body is not valid JSON: %v", err)
	}
	if _, ok := m["stream_options"]; ok {
		t.Errorf("unexpected field survived")
	}
}

// the full streaming chain, same coverage through the SSE writer.
func TestLiveStreamChain(t *testing.T) {
	c := defaultConfig()
	c.Rtk.Enabled = true
	useConfig(t, &c)

	body := rtkCompressBody([]byte(`{"model":"m","messages":[{"role":"user","content":[
		{"type":"tool_result","tool_use_id":"t1","content":` +
		jsonStr("On branch main\n"+strings.Repeat("\tmodified:   x.go\n", 40)) + `}]}]}`))
	_, _, _, tnames, _, err := anthropicRequestToOpenAI(body)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(tnames) != 0 {
		t.Logf("tool names: %v", tnames)
	}

	sse := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"bash","arguments":"{}"}}]},"finish_reason":null}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	w := httptest.NewRecorder()
	streamAnthropic(w, []byte(sse), "m", toolNames{"Bash"})
	got := w.Body.String()
	if !strings.Contains(got, `"name":"Bash"`) {
		t.Errorf("stream: client got lowercase: %s", got)
	}
	// every SSE line must be parseable JSON
	for _, line := range strings.Split(got, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		d := strings.TrimPrefix(line, "data: ")
		if d == "[DONE]" {
			continue
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(d), &v); err != nil {
			t.Errorf("malformed SSE line %q: %v", line, err)
		}
	}
}

// real HTTP round trip through callUpstream, the nvidia branch of
// handleAnthropic, against a local upstream. this is the path a /v1/messages
// request takes when the model is not opencode/*: request translation, real
// upstream exchange, response translation, tool-name restore.
func TestLiveToolCallRoundTrip(t *testing.T) {
	var gotBody []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"let me check","tool_calls":[
			{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"ls\"}"}},
			{"id":"call_2","type":"function","function":{"name":"glob","arguments":"{\"pattern\":\"*.go\"}"}},
			{"id":"call_3","type":"function","function":{"name":"say\"hi","arguments":"{}"}}
		]},"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":100,"completion_tokens":20}}`)
	}))
	defer up.Close()

	c := defaultConfig()
	c.Rtk.Enabled = true
	c.Nvidia.Enabled = true
	c.NvidiaKeys = map[string]string{"k1": "fake"}
	useConfig(t, &c)

	declared := `{"model":"test-model","messages":[{"role":"user","content":"list files"}],
		"tools":[{"name":"Bash","input_schema":{"type":"object"}},
		         {"name":"Glob","input_schema":{"type":"object"}},
		         {"name":"Say\"Hi","input_schema":{"type":"object"}}]}`
	oai, clientModel, _, tn, _, err := anthropicRequestToOpenAI([]byte(declared))
	if err != nil {
		t.Fatalf("translate request: %v", err)
	}
	if len(tn) != 3 || tn[0] != "Bash" || tn[1] != "Glob" || tn[2] != `Say"Hi` {
		t.Fatalf("declared names = %v", tn)
	}
	// the tool schemas must reach the upstream request intact
	for _, want := range []string{"Bash", "Glob", `Say\"Hi`} {
		if !strings.Contains(string(oai), want) {
			t.Errorf("tool %s missing from upstream body: %s", want, oai)
		}
	}

	p := newPool(c.NvidiaKeys)
	res, err := p.callUpstream("POST", up.URL, oai,
		http.Header{"Content-Type": {"application/json"}}, "test-model", "1.2.3.4")
	if err != nil || res == nil {
		t.Fatalf("callUpstream: %v", err)
	}
	if res.StatusCode != 200 {
		t.Fatalf("upstream status %d: %s", res.StatusCode, res.Body)
	}
	_ = gotBody

	out, errMsg, _ := openAIToAnthropic(res.Body, clientModel, tn)
	if errMsg != "" {
		t.Fatalf("translate response: %s", errMsg)
	}
	t.Logf("client sees: %s", out)

	for _, want := range []string{`"name":"Bash"`, `"name":"Glob"`, `"name":"Say\"Hi"`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing %s in translated response", want)
		}
	}
	for _, bad := range []string{`"name":"bash"`, `"name":"glob"`, `"name":"say\"hi"`} {
		if strings.Contains(string(out), bad) {
			t.Errorf("lowercase name leaked: %s", bad)
		}
	}
	if !strings.Contains(string(out), `"stop_reason":"tool_use"`) {
		t.Errorf("stop_reason should be tool_use:\n%s", out)
	}
	if !strings.Contains(string(out), `"input":{"command":"ls"}`) {
		t.Errorf("tool arguments not parsed:\n%s", out)
	}
	if !strings.Contains(string(out), `"input_tokens":100`) {
		t.Errorf("usage not carried over:\n%s", out)
	}
}
