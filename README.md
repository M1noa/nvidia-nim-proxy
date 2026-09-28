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
./nim-proxy          # listens on :5419, creates config.yml on first run
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

Edit `config.yml` (created from `config.yml.example` on first run):

```yaml
nvidia_keys:
  main: "nvapi-xxx"
  backup-1: "nvapi-yyy"
```

Edits hot-reload, no restart. Without keys, NIM models return 503 with a hint; `opencode/*` keeps working.

## Locking it down with auth (optional)

```yaml
auth:
  tokens:
    - "sk-my-secret-token"
```

Empty `tokens` (default) means open proxy, any key works. With tokens set, every model endpoint (`/v1/chat/*`, `/v1/messages*`, `/v1/responses*`) needs `Authorization: Bearer <token>` or `x-api-key: <token>`. `/v1/models` and `/status` stay open, but `/status` hides keys, locks, zen session and proxy unless the request carries a valid token. `./nim status` forwards `NIM_AUTH` as the token when set.

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

`models.claude_map` in `config.yml` maps `claude-*` names to backends (glob patterns, first match wins, hot-reloaded). Defaults route to free `opencode/*` models, so Claude Code works keyless too.

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

Everything lives in `config.yml` (auto-created from `config.yml.example`, hot-reloaded, heavily commented). Knobs:

| section | purpose |
|---|---|
| `server.port` | listen port (env `PORT` wins) |
| `auth.tokens` | tokens gating model endpoints; empty = open proxy |
| `nvidia_keys` | NVIDIA keys, optional (keyless serves `opencode/*` only) |
| `guardrails` | strip baked-in refusals (`enabled`, big `file` list, inline `extra`) |
| `inject` | request tweaks (`params`, `helpful_line` + custom `helpful_text`) |
| `anonymize` | mask `entities` (name + `variations`, per-type) and legacy `terms` per request (`enabled`, `mode: realistic`/`variable`, `fuzzy_threshold`, `disclose`); model sees masks only, responses swap back |
| `nudge` | master switch for the spark no-tools continue (`enabled`, default on; per-model `nudge_no_tools` still applies) |
| `zen` | `always_proxy`, custom `proxies` / `proxy_file` (auth in URL ok), `blocked_countries`, pool caps |
| `status` | toggle `show_keys`, `show_opencode_models`, `show_zen`, `show_locks` |
| `models.params` | per-model default params, glob patterns, first match wins |
| `models.claude_map` | `claude-*` → backend mapping for `/v1/messages` |

Env vars: `PORT`, `CONFIG_FILE` (default `config.yml`), `NIM_AUTH` (token for `./nim status`), `DEBUG=1` (verbose body logging to `nim-proxy-debug.log`).

Other endpoints: `/status` (pool health, sensitive fields hidden without a token when auth is on), `/v1/models` (whitelisted NIM + live zen free models), `./nim-proxy probe` (NIM rate-limit probe, needs keys).

Requests are logged to `nim-usage.jsonl` (tokens, latency, retries, rate-limit headers).

## Dev

```sh
go test ./...
go build -o nim-proxy .
```

## License

MIT — see [LICENSE](LICENSE).
