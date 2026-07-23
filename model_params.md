# Model Default Parameters
# Format: ## <glob-pattern> then indented key: value pairs
# Pattern is checked against the model string (case-insensitive)
# First match wins

## *deepseek*flash*
  temperature: 0.7
  top_p: 0.95
  max_tokens: 8192
  context_length: 1048576
  description: DeepSeek V4 Flash is a 284B MoE model with 1M-token context optimized for fast coding and agents.

## *deepseek*pro*
  temperature: 0.7
  top_p: 0.95
  max_tokens: 8192
  context_length: 1048576
  description: DeepSeek V4 scales to 1M-token context windows with efficient MoE architecture for coding tasks.

## *deepseek*v4*
  temperature: 0.7
  top_p: 0.95
  max_tokens: 8192
  context_length: 1048576
  description: DeepSeek V4 series model with MoE architecture.

## *deepseek*
  temperature: 0.7
  max_tokens: 4096
  context_length: 131072
  description: DeepSeek series model.

## *kimi*
  temperature: 0.3
  max_tokens: 4096
  context_length: 1048576
  description: 1T multimodal MoE for long-horizon coding, agentic tool use, and image/video understanding.

## *glm*
  temperature: 0.3
  max_tokens: 4096
  context_length: 131072
  description: GLM flagship LLM for agentic workflows, coding, and long-horizon reasoning tasks.

## *llama*
  temperature: 0.7
  max_tokens: 2048
  context_length: 131072
  description: Meta Llama series model.

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

## *inkling*
  temperature: 0.7
  top_p: 0.9
  max_tokens: 8192
  context_length: 262144
  description: Inkling model series.
