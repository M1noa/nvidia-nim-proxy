# nvidia-nim-proxy

> THIS PROJECT WAS MADE PARTIALLY USING AGENTIC AI CODING TOOLS

![build](https://github.com/M1noa/nvidia-nim-proxy/actions/workflows/build.yml/badge.svg)
![go](https://img.shields.io/badge/go-1.22-00ADD8)
![release](https://img.shields.io/github/v/release/M1noa/nvidia-nim-proxy)

A single-file Go reverse proxy for the NVIDIA NIM API (`integrate.api.nvidia.com`). It holds a pool of API keys, rotates between them, and backs off on 429s so you can push past the per-key rate limit. It also exposes opencode zen free models under `opencode/<model>` with no auth.

OpenAI-compatible. Point any client at it with `base_url=http://localhost:5419/v1` and any API key.

## How it works

- Keys live in `keys.jsonc` (JSON with comments). Edits hot-reload; no restart needed.
- Key picking is weighted: idle keys score higher, keys that failed a 429 in the last 50 minutes get crushed. On a 429 the key gets an exponential backoff (1m → 16m cap) and the (key, model) pair is locked out for 30s.
- `model_params.jsonc` sets per-model default parameters (temperature, top_p, top_k, min_p, reasoning_effort, ...) with glob patterns. First match wins; a client-sent value always wins over the default. Edits hot-reload.
- Every request is logged to `nim-usage.jsonl` (tokens, latency, retries, rate-limit headers).
- `/status` returns pool health as JSON. `/v1/models` lists whitelisted models.

## Install

Download a binary from [Releases](../../releases) (macOS, Linux, Windows; amd64 and arm64), or build it:

```sh
go build -o nim-proxy .
```

## Configure

```sh
cp keys.json.example keys.jsonc
# edit keys.jsonc: {"main": "nvapi-xxx", "backup": "nvapi-yyy"}
```

## Run

```sh
./nim-proxy          # listens on :5419
```

macOS users can use the `nim` helper script, which manages the proxy through launchd:

```sh
./nim start|stop|restart|status|logs|tail
```

## Use

```sh
curl http://localhost:5419/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model": "moonshotai/kimi-k3", "messages": [{"role":"user","content":"hi"}]}'
```

`./nim-proxy probe` hits a few models with test requests and reports per-model rate limits.
