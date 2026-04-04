# Runbook: switch LLM provider for the `llm-client` template

The `llm-client` template ships a single `/usr/local/bin/llm-call`
dispatcher that speaks five providers out of the box and falls back to
any OpenAI-compatible endpoint via override. Switch at **call time** —
no template rebuild, no snapshot regen.

## Supported providers (built-in defaults)

| `$LLM_PROVIDER` | Endpoint | Default model | Auth header | Body shape |
|---|---|---|---|---|
| `openai` *(default)* | `https://api.openai.com/v1/chat/completions` | `gpt-4o-mini` | `Authorization: Bearer` | OpenAI |
| `anthropic` | `https://api.anthropic.com/v1/messages` | `claude-3-5-haiku-latest` | `x-api-key` + `anthropic-version` | Anthropic |
| `openrouter` | `https://openrouter.ai/api/v1/chat/completions` | `meta-llama/llama-3.2-3b-instruct` | `Authorization: Bearer` | OpenAI |
| `kilo` | `https://kilo.ai/api/openrouter/v1/chat/completions` | `anthropic/claude-3-haiku` | `Authorization: Bearer` | OpenAI |
| `ollama` | `http://127.0.0.1:11434/v1/chat/completions` | `llama3.2:1b` | none | OpenAI |

## Per-call overrides

| Env var | Purpose | Example |
|---|---|---|
| `LLM_PROVIDER` | Pick a provider profile | `openrouter` |
| `LLM_ENDPOINT` | Override the URL (any OpenAI-compatible host) | `http://vllm.internal:8000/v1/chat/completions` |
| `LLM_MODEL` | Override the model | `gpt-4o`, `claude-3-opus-latest` |
| `LLM_API_KEY` | Auth token (header style applied automatically) | `sk-...`, `sk-ant-...`, `sk-or-...` |
| `LLM_MAX_TOKENS` | Response cap (default `64`) | `128` |
| `LLM_SYSTEM` | Optional system prompt | `"You are a concise assistant."` |

The dispatcher also falls back to provider-specific env vars
(`OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `OPENROUTER_API_KEY`,
`KILO_API_KEY`) when `LLM_API_KEY` is unset.

## Examples

### From inside a fork (over vsock-exec)

```python
# Host-side, via firefork-server /v1/exec or workload.Call:
agent.Call({
  "cmd": "exec",
  "argv": ["sh", "-c",
    'LLM_PROVIDER=anthropic LLM_API_KEY="$KEY" '
    '/usr/local/bin/llm-call "Summarize the firefork pitch in one sentence."'],
})
```

### From the host shell (debug-style)

After `bin/fork --template llm-client/v1 --hold 60s`, ssh into the
guest (if you've wired networking) or send via the agent. The script
also runs standalone on any Alpine box with `curl` and `jq`:

```sh
LLM_PROVIDER=openrouter \
LLM_API_KEY=sk-or-... \
LLM_MODEL='deepseek/deepseek-r1' \
llm-call "What's 2+2?"
```

### Kilo (worked example)

Kilo's gateway is an OpenAI-compatible proxy that fronts OpenRouter's
catalog plus its own routing. The dispatcher's `kilo` profile points
at `https://kilo.ai/api/openrouter/v1/chat/completions` with
`anthropic/claude-3-haiku` as a low-cost default model.

**One-shot call inside a fork (host side, via the agent):**

```python
# host -> guest, with API key passed via env (not argv, to keep it
# out of /proc/<pid>/cmdline).
agent.Call({
  "cmd": "exec",
  "argv": ["env", "LLM_PROVIDER=kilo",
                  "LLM_API_KEY=" + kilo_jwt,
                  "/usr/local/bin/llm-call",
                  "Reply with exactly five words about microVMs."],
})
# -> {"ok": true, "rc": 0, "stdout": "Lightweight, secure, isolated, ..."}
```

**Fan-out across N parallel forks (host-side probe):**

```sh
export LLM_API_KEY=<your Kilo JWT>

sudo -E bin/probe-fanout \
  --template llm-client/v1 \
  --jailer /usr/local/bin/jailer \
  --n 16 \
  --endpoint https://kilo.ai/api/openrouter/v1/chat/completions \
  --model anthropic/claude-3-haiku \
  --prompt "In one short sentence what is firefork?"
```

Expected output shape:

```
=== firefork probe-fanout: N=16 ===
template: llm-client/v1
endpoint: https://kilo.ai/api/openrouter/v1/chat/completions
model:    anthropic/claude-3-haiku
prompt:   "In one short sentence what is firefork?"

[1/2] spawn: 16/16 forks in 56.3 ms wall (per-fork median 30.7 ms)
[2/2] call:  16/16 ok in 3.73 s wall (per-call median 2.59 s)

--- per-fork outputs ---
  [ 0] 2453ms  "firefork is a ..."
  [ 1] 2411ms  "..."
  ...

=== headline ===
  firefork:        3.78 s wall  (56.3 ms spawn + 3.73 s parallel call)
  serial-spawn baseline: ~10.59 s  (16*500ms + 2.59s call)
  speedup vs serial: 2.8x
```

The spawn cost (~3.5 ms per fork) is dominated by the API latency
(~2-3 s p50 for Claude Haiku via Kilo). That's the point of the
demo: spawn falls below the noise floor of the inference call.

**Choosing a model.** Kilo proxies OpenRouter's catalog, so the same
`<vendor>/<model>` identifiers work. The `anthropic/claude-3-haiku`
default is cheap and fast. Higher-quality options:

| Model | Approx p50 | Notes |
|---|---|---|
| `anthropic/claude-3-haiku` | ~2-3 s | default; fast and cheap |
| `anthropic/claude-3.5-sonnet` | ~3-5 s | better quality; needs paid tier on the Kilo account |
| `openai/gpt-4o-mini` | ~1-2 s | competitive cheap option |
| `meta-llama/llama-3.3-70b-instruct` | varies | open-weights routed via OpenRouter |

**Failure to anticipate:**

- A bare `anthropic/claude-3.5-sonnet` call against a free-tier Kilo
  account returns `401 PAID_MODEL_AUTH_REQUIRED`. Fall back to
  `anthropic/claude-3-haiku` or upgrade the account.
- `https://api.kilocode.ai/...` (the old hostname) currently returns a
  308 redirect to `https://kilo.ai/...` -- curl with `-L` follows it,
  but some clients (the Go `net/http` default for non-GET) don't replay
  the body. Use the canonical `kilo.ai` host directly.

### Self-hosted vLLM / Ollama / TGI

Any OpenAI-compatible service works without a profile addition:

```sh
LLM_PROVIDER=openai \
LLM_ENDPOINT=http://vllm.internal:8000/v1/chat/completions \
LLM_API_KEY=local-dev-key \
LLM_MODEL=meta-llama/Llama-3.2-1B-Instruct \
llm-call "ping"
```

For Ollama on the host:

```sh
LLM_PROVIDER=ollama \
LLM_ENDPOINT=http://<host-ip>:11434/v1/chat/completions \
LLM_MODEL=llama3.2:1b \
llm-call "hello"
```

(Needs Firecracker networking configured so the guest can reach
`<host-ip>`. Vsock-only forks can't make outbound HTTP — see
[`docs/architecture/0002-jailer-rollout.md`](../architecture/0002-jailer-rollout.md)
for the network-stack discussion.)

## Adding a new provider

Two paths:

### 1. OpenAI-compatible — zero code changes

If the new provider speaks the OpenAI chat-completions wire format
(Together AI, Fireworks, Groq, DeepInfra, Replicate's OpenAI-compat
endpoints, etc.):

```sh
LLM_PROVIDER=openai \
LLM_ENDPOINT=https://api.together.xyz/v1/chat/completions \
LLM_API_KEY=$TOGETHER_KEY \
LLM_MODEL='meta-llama/Llama-3.3-70B-Instruct-Turbo' \
llm-call "..."
```

Done. No template rebuild needed.

### 2. Non-standard wire format — edit the dispatcher

Add a new `case "$PROVIDER")` arm to the dispatcher inside
`configs/template_llm_client.yaml`. Each arm sets four variables:

```sh
new-provider)
  D_ENDPOINT="https://api.newco.example.com/v1/chat"
  D_MODEL="newco-1"
  AUTH_TYPE="bearer"          # bearer | x-api-key | none
  BODY_SHAPE="openai"         # openai | anthropic
  ;;
```

If the wire format is truly novel (not OpenAI- or Anthropic-shaped),
extend the `BODY_SHAPE` cases below the profile lookup. Then:

```sh
bin/seed-template --config configs/template_llm_client.yaml --jailer /usr/local/bin/jailer
```

…to rebuild the snapshot with the new dispatcher baked in.

## Secret handling

API keys are **never baked into the snapshot**. They're passed at
exec time via env vars on the agent's subprocess. The current agent
inherits the orchestrator's env on each subprocess; to inject keys
per-call, prefix the command:

```python
# Production-safe form: API key never logged in /proc/<pid>/cmdline
# (use env, not argv):
agent.Call({
  "cmd": "exec",
  "argv": ["env", "LLM_PROVIDER=openai",
                  "LLM_API_KEY=" + secret,
                  "/usr/local/bin/llm-call", prompt]
})
```

A planned agent extension will accept `env: {KEY: VAL}` directly in
the exec command so keys don't appear in argv at all.

## Verifying the dispatcher

After any change, smoke-test against a free provider:

```sh
# OpenRouter has free-tier models, no card required.
LLM_PROVIDER=openrouter \
LLM_API_KEY=$OPENROUTER_KEY \
LLM_MODEL='meta-llama/llama-3.2-1b-instruct:free' \
llm-call "Say hi in 5 words."
```

Expected: a one-line response on stdout, exit 0.

Common failure modes:

| Symptom | Cause | Fix |
|---|---|---|
| `usage: llm-call <prompt>` | No prompt argument | Quote the prompt: `llm-call "hello"` |
| `unknown LLM_PROVIDER` | Typo in `$LLM_PROVIDER` | Check the supported list above |
| `curl: (6) Could not resolve host` | DNS not warmed for non-default provider | Add to template `warmup:` list and rebuild |
| `{"error":{"message":"Incorrect API key..."}}` | Wrong key for the provider | Check `LLM_API_KEY` matches provider |
| Empty output | Provider returned a different JSON shape | Add a `BODY_SHAPE` case + matching `EXTRACT` jq filter |
