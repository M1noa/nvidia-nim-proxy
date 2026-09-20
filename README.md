# nvidia-nim-proxy
> THIS PROJECT WAS MADE PARTIALLY USING AGENTIC AI CODING TOOLS

![build](https://github.com/M1noa/nvidia-nim-proxy/actions/workflows/build.yml/badge.svg)
![go](https://img.shields.io/badge/go-1.22-00ADD8)
![release](https://img.shields.io/github/v/release/M1noa/nvidia-nim-proxy)
![license](https://img.shields.io/badge/license-MIT-green)

OpenAI-compatible proxy with two backends:

- **NVIDIA NIM** (`integrate.api.nvidia.com`) — pool of API keys, weighted rotation, exponential 429 backoff. Needs keys.
- **opencode zen free models** (`opencode/<model>`) — no auth, no keys needed. Works out of the box.

Point any OpenAI client at `base_url=http://localhost:5419/v1` with any API key.

## Quickstart (no keys)

```sh
go build -o nim-proxy .
./nim-proxy          # listens on :5419, creates keys.jsonc for later
```

This already serves every `opencode/<model>` free model. Check what's available:

```sh
curl -s http://localhost:5419/v1/models | python3 -m json.tool
curl -s http://localhost:5419/status | python3 -m json.tool
```

Try one:

```sh
curl http://localhost:5419/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model": "opencode/big-pickle", "messages": [{"role":"user","content":"hi"}]}'
```

## Adding NVIDIA keys (optional)

```sh
cp keys.jsonc.example keys.jsonc
# edit keys.jsonc: {"main": "nvapi-xxx", "backup": "nvapi-yyy"}
```

Edits hot-reload, no restart. Without keys, NIM models return 503 with a hint; `opencode/*` keeps working.

## Use with opencode

opencode works through any OpenAI-compatible provider. Point it at the proxy:

- base URL: `http://localhost:5419/v1`
- api key: anything (e.g. `dummy`)
- model: any `opencode/<model>` id from `/v1/models` — no NVIDIA keys required

## Use with Claude Code

The proxy speaks the Anthropic Messages API:

```sh
ANTHROPIC_BASE_URL=http://localhost:5419 claude
```

`claude_models.jsonc` maps `claude-*` names to backends (glob patterns, first match wins, hot-reloaded). Defaults route to free `opencode/*` models, so Claude Code works keyless too.

Endpoints: `POST /v1/messages` (stream + tools), `POST /v1/messages/count_tokens`, `POST /v1/messages/classifier` (always approves).

## Run as a service

`./nim` handles all three OSes (launchd on macOS, systemd user unit on Linux, background process elsewhere):

```sh
./nim run        # foreground
./nim start      # start service
./nim stop       # stop service
./nim restart    # restart service
./nim status     # pretty status (key pool hidden when keyless)
./nim logs       # status + last log lines
./nim tail       # follow access log
./nim install    # install + start, persists across reboots
./nim uninstall  # stop + remove service
```

System-wide installs: templates live in `deploy/` (replace `REPLACE_WITH_PATH` with the install dir):

- macOS: `deploy/com.user.nvidia-nim-proxy.plist` → `/Library/LaunchDaemons/`
- Linux: `deploy/nvidia-nim-proxy.service` → `/etc/systemd/system/`
- Windows: `deploy/nvidia-nim-proxy.xml` with [WinSW](https://github.com/winsw/winsw) next to `nim-proxy.exe`

## Configure

| file | purpose |
|---|---|
| `keys.jsonc` | NVIDIA keys, optional (auto-created empty on first run) |
| `model_params.jsonc` | per-model default params, glob patterns, first match wins |
| `claude_models.jsonc` | `claude-*` → backend mapping for `/v1/messages` |
| `guardrails.json` | system-prompt guardrail strings stripped from requests |

Env vars: `PORT` (default 5419), `KEY_FILE` (default `keys.jsonc`), `DEBUG=1` (verbose body logging to `nim-proxy-debug.log`).

Other endpoints: `/status` (pool health, `keys` omitted when keyless), `/v1/models` (whitelisted NIM + live zen free models), `./nim-proxy probe` (NIM rate-limit probe, needs keys).

Requests are logged to `nim-usage.jsonl` (tokens, latency, retries, rate-limit headers).

## Dev

```sh
go test ./...
go build -o nim-proxy .
```

## License

MIT — see [LICENSE](LICENSE).
