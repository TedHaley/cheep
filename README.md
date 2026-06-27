# cheep

> Claude orchestrates. Local Qwen does the work. You pay almost nothing.

**cheep** is a tiny, single-binary CLI for multi-agent coding. One expensive, capable
**orchestrator** (Claude) breaks your task into pieces, hands each to a free, local
**executor** (Qwen via Ollama), verifies the work, and recovers any executor that gets
stuck — loops, runs out of context, or errors out.

Website & docs: **https://tedhaley.github.io/cheep/**

## Install

**Homebrew (macOS & Linux)**

```sh
brew tap TedHaley/homebrew-tap
brew install cheep
```

**Go**

```sh
go install github.com/TedHaley/cheep@latest
```

**Direct download** — grab a binary from the [latest release](https://github.com/TedHaley/cheep/releases/latest).

## Quick start

```sh
ollama serve &
ollama pull qwen2.5-coder
export ANTHROPIC_API_KEY=sk-ant-...

cheep check                                  # ping both endpoints
cheep run "add a /health endpoint" --workdir .
```

## Configuration

All via environment variables (defaults shown):

| Variable | Default |
|---|---|
| `ANTHROPIC_API_KEY` | — (required) |
| `CHEEP_ORCHESTRATOR_MODEL` | `claude-sonnet-4-6` |
| `CHEEP_EXECUTOR_MODEL` | `qwen2.5-coder` |
| `CHEEP_EXECUTOR_BASE_URL` | `http://localhost:11434/v1` |
| `CHEEP_EXECUTOR_API_KEY` | `ollama` |
| `CHEEP_EXECUTOR_TOKEN_BUDGET` | `100000` |

## How it works

```
            ┌──────────────┐   delegate_to_executor   ┌──────────────┐
   task ──▶ │ orchestrator │ ───────────────────────▶ │  executor    │
            │   (Claude)   │ ◀─────────────────────── │   (Qwen)     │
            └──────────────┘   status + summary        └──────────────┘
                  │  verifies with read_file / run_bash
                  ▼
              final answer
```

The orchestrator and executors share one generic agent loop (`internal/agent`) over two
provider backends (`internal/provider`: Anthropic + any OpenAI-compatible endpoint). Each
executor run returns a status — `completed`, `max_turns`, `looping`, `context_exhausted`,
or `error` — which the orchestrator uses to decide whether to accept the work or recover it.

## Build from source

```sh
go build -o cheep .
go test ./...
```

## License

MIT
