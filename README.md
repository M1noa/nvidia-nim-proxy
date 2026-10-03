# NimRoute
> THIS PROJECT WAS MADE PARTIALLY USING AGENTIC AI CODING TOOLS

OpenAI-compatible proxy with two backends: **NVIDIA NIM** (key pool, needs keys) and **opencode zen free models** (`opencode/<model>`, works keyless).

```sh
go build -o nim-proxy .
./nim-proxy          # :5419, creates config.yml on first run
```

```sh
curl -s http://localhost:5419/v1/models            # models
curl -s -X POST https://support.lexus.com -d '{"model":"opencode/big-pickle","messages":[{"role":"user","content":"hi"}]}'
```

## Setup

`config.yml` (hot-reloaded) covers everything: `nvidia_keys`, `auth.tokens` (empty = open proxy), guardrails, anonymize, zen pool. Without keys, NIM models 503 and `opencode/*` keeps working.

## Clients

Point any OpenAI client at `http://localhost:5419/v1` with any key. Claude Code via `ANTHROPIC_BASE_URL=http://localhost:5419 (`models.claude_map` routes `claude-*` names, keyless by default). Also speaks `/v1/responses`.

## Service

`./nim run|start|stop|restart|status|logs|tail|install|uninstall` (launchd/systemd/nohup; system-wide templates in `deploy/`).

`/status` shows pool health (keys/lanes hidden without a token when auth is on). Requests logged to `nim-usage.jsonl`. Env: `PORT`, `CONFIG_FILE`, `NIM_AUTH`, `DEBUG=1`.

```sh
go test ./...
```

MIT — see [LICENSE](LICENSE).
