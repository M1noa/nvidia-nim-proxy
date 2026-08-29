# Model Default Parameters
# Format: ## <glob-pattern> then indented key: value pairs
# Pattern is checked against the model string (case-insensitive)
# First match wins

## *kimi-k3*
  temperature: 1.0
  top_p: 0.95
  max_tokens: 131072
  context_length: 1048576
  description: Moonshot AI Kimi K3 — 2.8T-param MoE (16/896 experts, hybrid linear attention) with native vision and a 1M-token context for long-horizon coding, agentic tool use, and reasoning.

## *deepseek-v4-flash*
  temperature: 1.0
  top_p: 0.95
  max_tokens: 131072
  context_length: 1048576
  description: DeepSeek V4 Flash 0731 — 284B MoE (13B active) for fast coding, reasoning, tool use, and long-context agentic workflows.

## *deepseek-v4-pro*
  temperature: 1.0
  top_p: 0.95
  max_tokens: 131072
  context_length: 1048576
  description: DeepSeek V4 Pro 0813 — 1.6T MoE (49B active) for advanced coding, tool use, and long-horizon agentic work.

## *muse-glimmer*
  temperature: 1.0
  top_p: 0.95
  top_k: 64
  max_tokens: 4096
  context_length: 131072
  description: Meta Muse Glimmer 30B — ~29.6B dense multimodal (text+image) model with native tool-calling and reasoning, built for local agentic tasks.

## *llama*
  temperature: 0.7
  max_tokens: 2048
  context_length: 131072
  description: Meta Llama series model.

## *nemotron*3.5*lightning*
  temperature: 1.0
  top_p: 0.95
  max_tokens: 65536
  context_length: 1000000
  description: NVIDIA Nemotron 3.5 Lightning 30B A3B — fast lightweight model optimized for agentic workflows, tool use, and function calling.

## *nemotron*
  temperature: 0.5
  max_tokens: 4096
  context_length: 1048576
  description: NVIDIA Nemotron model optimized for agentic workflows, coding, and function calling.

## *mistral*
  temperature: 0.7
  max_tokens: 4096
  context_length: 262144
  description: Mistral series model.

## *deepseek*
  temperature: 1.0
  top_p: 0.95
  max_tokens: 131072
  context_length: 1048576
  description: DeepSeek model for coding, reasoning, and agentic workflows.
