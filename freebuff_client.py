#!/usr/bin/env python3
# freebuff exact-behavior client replica (stdlib only).
# mirrors cli/src + sdk/src + agent-runtime behavior field-for-field.
# anti-ban rules baked in (learned 2026-09-28, 1 account lost):
#  - admit at most once per model per account per hour; heartbeat, never re-admit
#  - chat turns: FINISH every run, human pacing >=20s between turns
#  - failures: exp backoff 20s->300s cap, honor retry-after, stop on 4xx
#  - NEVER sweep models; NEVER admit without intent to chat within 60s
# usage: from freebuff_client import FreebuffClient
import json, random, time, uuid, urllib.request, urllib.error

WWW = "https://www.codebuff.com"
LOGIN = "https://freebuff.com"  # agentic/offer host
CLI_VER = "0.0.1"
UA_PRODUCT = f"Freebuff-CLI/{CLI_VER}"
UA_SDK = "ai-sdk/openai-compatible/1.0.0/codebuff"
CHROME_UA = ("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) "
             "AppleWebKit/537.36 (KHTML, like Gecko) "
             "Chrome/151.0.0.0 Safari/537.36")
# base2 root agent per model (FREEBUFF_ROOT_AGENT_ID_BY_MODEL, 62 rows).
# full table with premium/mm/price in FREEBUFF-MODEL-TABLE.md
AGENT_BY_MODEL = {
    "mimo/mimo-v2.5": "base2-free-mimo",
    "mimo/mimo-v2.6-pro": "base2-free-mimo-2-6-pro",
    "minimax/minimax-m3": "base2-free-minimax-m3",
    "openai/gpt-5.6-luna": "base2-free-luna",
    "openai/gpt-6-luna": "base2-free-luna-6",
    "upstage/solar-pro4": "base2-free-solar-pro4",
    "upstage/solar-mini4": "base2-free-solar-mini4",
    "stealth/space-bunny-alpha": "base2-free-space-bunny-alpha",
    "deepseek/deepseek-v4-pro": "base2-free-deepseek",
    "deepseek/deepseek-v4-flash": "base2-free-deepseek-flash",
    "z-ai/glm-5.2": "base2-free-glm",
    "z-ai/glm-5.3-flash": "base2-free-glm-5-3-flash",
    "crof/kimi-k3-eco": "base2-free-kimi-k3-eco",
    "openai/gpt-5.6-luna-es": "base2-free-luna-es",
    "anthropic/claude-fable-5.1": "base2-free-fable",
    "meta/muse-spark-1.2-contributor": "base2-free-muse-spark",
    "meta/muse-spark-1.3-contributor": "base2-free-muse-spark-1-3",
    "stealth/ox-alpha": "base2-free-ox-alpha",
    "google/gemini-3.8-flash": "base2-free-gemini-3-8-flash",
    "deepseek/deepseek-v4.1-flash": "base2-free-deepseek-v4-1-flash",
    "z-ai/glm-5.3": "base2-free-glm-5-3",
    "openai/gpt-6-sol": "base2-free-gpt-6-sol",
    "openai/gpt-6-sol-pro": "base2-free-gpt-6-sol-pro",
    "openai/gpt-6-luna-pro": "base2-free-gpt-6-luna-pro",
    "openai/gpt-6-astra": "base2-free-gpt-6-astra",
    "openai/gpt-6-astra-pro": "base2-free-gpt-6-astra-pro",
    "openai/gpt-5.6-sol": "base2-free-gpt-5-6-sol",
    "openai/gpt-5.6-sol-pro": "base2-free-gpt-5-6-sol-pro",
    "openai/gpt-5.6-terra": "base2-free-gpt-5-6-terra",
    "openai/gpt-5.6-terra-pro": "base2-free-gpt-5-6-terra-pro",
    "openai/gpt-5.6-luna-pro": "base2-free-gpt-5-6-luna-pro",
    "openai/gpt-5.5": "base2-free-gpt-5-5",
    "openai/gpt-5.5-pro": "base2-free-gpt-5-5-pro",
    "openai/gpt-5.4-pro": "base2-free-gpt-5-4-pro",
    "openai/o3-pro": "base2-free-o3-pro",
    "anthropic/claude-opus-5.5": "base2-free-claude-opus-5-5",
    "anthropic/claude-opus-5": "base2-free-claude-opus-5",
    "anthropic/claude-sonnet-5": "base2-free-claude-sonnet-5",
    "anthropic/claude-opus-4.8": "base2-free-claude-opus-4-8",
    "anthropic/claude-sonnet-4.6": "base2-free-claude-sonnet-4-6",
    "qwen/qwen3.8-max-prime": "base2-free-qwen3-8-max-prime",
    "qwen/qwen3.8-max-0902": "base2-free-qwen3-8-max-0902",
    "qwen/qwen3.8-flash": "base2-free-qwen3-8-flash",
    "qwen/qwen3.8-27b": "base2-free-qwen3-8-27b",
    "qwen/qwen3.7-max": "base2-free-qwen3-7-max",
    "qwen/qwen3.7-plus": "base2-free-qwen3-7-plus",
    "qwen/qwen3.6-max-preview": "base2-free-qwen3-6-max-preview",
    "qwen/qwen3.6-plus": "base2-free-qwen3-6-plus",
    "x-ai/grok-4.7": "base2-free-grok-4-7",
    "x-ai/grok-4.6": "base2-free-grok-4-6",
    "x-ai/grok-4.5": "base2-free-grok-4-5",
    "x-ai/grok-4.20": "base2-free-grok-4-20",
    "google/gemini-3.7-flash": "base2-free-gemini-3-7-flash",
    "google/gemini-3.6-flash": "base2-free-gemini-3-6-flash",
    "google/gemini-3.5-flash": "base2-free-gemini-3-5-flash",
    "moonshotai/kimi-k3": "base2-free-kimi-k3",
    "z-ai/glm-5.3-prime": "base2-free-glm-5-3-prime",
    "z-ai/glm-5.3-flashx": "base2-free-glm-5-3-flashx",
    "z-ai/glm-5-turbo": "base2-free-glm-5-turbo",
    "mistralai/mistral-large": "base2-free-mistral-large",
    "mistralai/codestral-2508": "base2-free-codestral-2508",
    "meta-llama/llama-4-maverick": "base2-free-llama-4-maverick",
}
BUFFY_OPEN = "You are Buffy, the strategic coding assistant."
# chat gate: match error+status pair, never message alone
GATE_ENDS_SESSION = {"waiting_room_required": 428, "session_expired": 410,
                     "session_superseded": 409, "session_model_mismatch": 409}
GATE_KEEP_SESSION = {"session_limit_reached": 409, "waiting_room_queued": 429,
                     "model_unavailable": 410}
FAIL_BASE_MS, FAIL_MAX_MS = 20_000, 300_000
MIN_TURN_GAP_S = 20  # human pacing between served turns


class FreebuffError(Exception):
    def __init__(self, code, body):
        self.code, self.body = code, body
        super().__init__(f"freebuff {code}: {str(body)[:200]}")


def _backoff(failures, retry_after_ms=None):
    cap = min(FAIL_MAX_MS, FAIL_BASE_MS * 2 ** max(0, failures - 1))
    ms = max(1, round(cap / 2 + cap / 2 * random.random()))
    if retry_after_ms is not None:
        ms = max(ms, min(FAIL_MAX_MS, max(0, retry_after_ms)))
    return min(FAIL_MAX_MS, ms) / 1000


class FreebuffClient:
    # wire tools = agent toolNames + spawnableAgents (NOT the cli display
    # list). captured 22 for base2: propose_* variants + subagent spawns,
    # no read_url/skill/list_directory/glob/render_ui/gravity_index.
    # caller sets c.tools = compiled list (see /tmp/glm_tools.json pattern).
    tools = []

    def __init__(self, token, model, timeout=90):
        self.token = token
        self.model = model
        self.timeout = timeout
        self.instance_id = None  # server UUID, set by admit()
        self.session = None
        self._failures = 0
        self._last_turn = 0.0

    def _req(self, method, path, body=None, headers=None, host=WWW,
             ua=UA_SDK):
        req = urllib.request.Request(
            host + path, method=method,
            data=json.dumps(body).encode() if body is not None else None,
            headers={"Content-Type": "application/json",
                     "Authorization": f"Bearer {self.token}",
                     "User-Agent": ua,
                     **(headers or {})})
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as r:
                retry = r.headers.get("retry-after")
                ra = int(retry) * 1000 if retry and retry.isdigit() else None
                try:
                    return r.status, json.loads(r.read() or b"null"), ra
                except Exception:
                    return r.status, {}, ra
        except urllib.error.HTTPError as e:
            try:
                return e.code, json.loads(e.read() or b"null"), None
            except Exception:
                return e.code, {}, None

    # --- session (poll cadence mirrors use-freebuff-session: GET heartbeat,
    # compact when active, jittered 30s) ---
    def get_session(self, compact=True):
        st, b, _ = self._req("GET", "/api/v1/freebuff/session", headers={
            **({"x-freebuff-instance-id": self.instance_id}
               if self.instance_id else {}),
            **({"x-freebuff-compact-session": "1"} if compact and
               self.instance_id else {}),
            "x-fb-timezone": "America/New_York"})
        if st != 200:
            raise FreebuffError(st, b)
        self.session = b
        if b.get("status") == "active":
            self.instance_id = b["instanceId"]
        return b

    def admit(self):
        claim = "cli:" + str(uuid.uuid4())
        st, b, _ = self._req(
            "POST", "/api/v1/freebuff/session/admission", {},
            {"x-freebuff-model": self.model,
             "x-freebuff-wallet-spend-limit": "0",
             "x-freebuff-instance-id": claim,
             "x-fb-timezone": "America/New_York",
             "x-freebuff-first-tab-discount": "0"}, ua=UA_SDK)
        if st == 200 and b.get("status") == "active":
            self.instance_id = b["instanceId"]  # server UUID, NOT claim
            self.session = b
            self._failures = 0
            return b
        if st in (408, 429, 503):
            raise FreebuffError(st, b)  # retryable
        raise FreebuffError(st, b)  # 4xx: stop, do not retry

    def ensure_session(self):
        try:
            s = self.get_session()
        except FreebuffError as e:
            if e.code in (401, 403):
                raise  # banned/bad token: stop immediately
            s = {"status": "none"}
        if s.get("status") == "active" and s.get("model") == self.model:
            return s
        if s.get("status") == "active":
            self.release()  # one model per account; avoid model_locked
            time.sleep(3 + random.random() * 2)  # DELETE->POST race guard
        # exp backoff on repeated admission failures
        while True:
            try:
                return self.admit()
            except FreebuffError as e:
                if e.code not in (408, 429, 503):
                    raise
                self._failures += 1
                time.sleep(_backoff(self._failures))

    def release(self):
        iid = self.instance_id or (self.session or {}).get("instanceId")
        if iid:
            self._req("DELETE", "/api/v1/freebuff/session",
                      headers={"x-freebuff-instance-id": iid})
        self.instance_id, self.session = None, None

    # --- agent runs (START + FINISH every turn, like agent-runtime) ---
    def start_run(self):
        agent = AGENT_BY_MODEL.get(self.model)
        if not agent:
            raise FreebuffError(0, f"no agent mapping for {self.model}")
        st, b, _ = self._req("POST", "/api/v1/agent-runs",
                              {"action": "START", "agentId": agent})
        if st == 200 and b.get("runId"):
            return b["runId"]
        raise FreebuffError(st, b)

    def finish_run(self, run_id, status="completed", steps=1):
        self._req("POST", "/api/v1/agent-runs",
                  {"action": "FINISH", "runId": run_id, "status": status,
                   "totalSteps": steps, "directCredits": 0,
                   "totalCredits": 0})

    # --- chat ---
    def chat(self, messages, stream=True, max_tokens=32000):
        """messages: caller-built exact shape (system full prompt +
        orchestrator followups, multipart user parts). Buffy marker
        auto-prepended if no system message (gate)."""
        gap = MIN_TURN_GAP_S - (time.time() - self._last_turn)
        if gap > 0:
            time.sleep(gap)
        self.ensure_session()
        if not (messages and messages[0].get("role") == "system"):
            messages = [{"role": "system", "content": BUFFY_OPEN +
                         " You help users with software engineering tasks."}
                        ] + messages
        # exact captured shape (tap3 2026-09-29, sdk run through relay):
        # - NO max_tokens/stop/temperature/top_p/user/seed/n
        # - provider TOP-LEVEL {allow_fallbacks:true} (+ data_collection deny
        #   merged from agent def for free roots)
        # - tool_choice:"auto", stream:true
        # - messages: [system(full bundled prompt), user(prompt multipart),
        #   user(orchestrator instruction), user(system_reminder)]
        #   multipart = [{"type":"text","text":...}]
        # - metadata += trace_session_id, repo_snapshot (json str),
        #   llm_step_number:"1"; client_id = 10-char promptId
        provider = {"allow_fallbacks": True, "data_collection": "deny"}
        if self.model.startswith("anthropic/"):
            provider = {"only": ["amazon-bedrock"],
                        "data_collection": "deny"}
        run_id = self.start_run()
        body = {"model": self.model,
                "codebuff_metadata": {
                    "freebuff_instance_id": self.instance_id,
                    "surface": "cli",
                    "trace_session_id": str(uuid.uuid4()),
                    "repo_snapshot": json.dumps({
                        "gitAvailable": False,
                        "repositoryVisibility": "unknown", "fileCount": 0,
                        "fileCountIsLowerBound": False, "testFileCount": 0,
                        "changedFileCount": 0,
                        "changedFileScanTruncated": False}),
                    "llm_step_number": "1",
                    "run_id": run_id,
                    "client_id": uuid.uuid4().hex[:10],
                    "cost_mode": "free"},
                "provider": provider,
                "messages": messages, "tools": self.tools,
                "tool_choice": "auto", "stream": True}
        st, b, _ = self._req("POST", "/api/v1/chat/completions", body)
        ok = st == 200
        try:
            if not ok:
                err, code = b.get("error"), None
                if isinstance(err, dict):
                    code = err.get("code")
                elif isinstance(err, str):
                    code, err = err, {"message": b.get("message")}
                if code in GATE_ENDS_SESSION:
                    self.instance_id, self.session = None, None
                    self.ensure_session()
                    body["codebuff_metadata"]["freebuff_instance_id"] = \
                        self.instance_id
                    body["codebuff_metadata"]["run_id"] = self.start_run()
                    st2, b2, _ = self._req("POST", "/api/v1/chat/completions",
                                           body)
                    if st2 == 200:
                        b, ok = b2, True
            if ok:
                self._failures = 0
                self._last_turn = time.time()
                return b
            raise FreebuffError(st, b)
        finally:
            self.finish_run(run_id, "completed" if ok else "failed")

    # --- ads (same UA pairing as cli: product header, browser body) ---
    def ad_policy(self):
        st, b, _ = self._req("GET", "/api/v1/ads/policy")
        if st == 200:
            return b
        raise FreebuffError(st, b)

    def ad_auction(self, messages, session_id="proxy-1", provider=None):
        slim = [{"role": m["role"], "content": m["content"][:2000]}
                for m in messages[-6:]
                if m.get("role") in ("user", "assistant")]
        body = {"messages": slim, "sessionId": session_id,
                "device": {"os": "macos", "timezone": "America/New_York",
                           "locale": "en-US"},
                "userAgent": CHROME_UA, "surface": "cli_chat"}
        if provider:
            body["provider"] = provider
        st, b, _ = self._req("POST", "/api/v1/ads", body, ua=UA_PRODUCT)
        if st == 200:
            return b
        raise FreebuffError(st, b)

    def ad_impression(self, imp_url, render_delay_ms=0):
        eid = str(uuid.uuid4())
        st, b, _ = self._req(
            "POST", "/api/v1/ads/impression",
            {"impUrl": imp_url, "mode": "FREE", "userAgent": CHROME_UA,
             "os": "macos", "clientEventId": eid,
             **({"renderDelayMs": render_delay_ms} if render_delay_ms else {})},
            {"x-freebuff-event-id": eid}, ua=UA_PRODUCT)
        if st == 200:
            return b
        raise FreebuffError(st, b)

    def agentic_offer(self, messages, conversation_id="proxy-1"):
        slim = [{"role": m["role"], "content": m["content"][:2000]}
                for m in messages[-6:]
                if m.get("role") in ("user", "assistant")]
        st, b, _ = self._req(
            "POST", "/api/v1/ads/agentic/offer",
            {"v": 1, "conversationId": conversation_id, "messages": slim,
             "inPlaceExecutionVersion": 1,
             "device": {"os": "macos", "timezone": "America/New_York",
                        "locale": "en-US"}},
            ua=UA_PRODUCT, host=LOGIN)
        if st == 200:
            return b
        raise FreebuffError(st, b)
