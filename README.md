# cheep

> A smart lead agent orchestrates. Cheap local models do the work. You pay almost nothing.

**cheep** is a tiny, single-binary, interactive multi-agent coding shell. A lead
**orchestrator** agent breaks a task into pieces, hands each to one or more **executor**
agents (in parallel), verifies the work, and recovers any executor that gets stuck — loops,
runs out of context, or errors out. Every role can point at an Anthropic or any
OpenAI-compatible endpoint, so you can mix an expensive planner with free local workers — or
run entirely local.

Website & docs: **https://tedhaley.ca/cheep/**

## Install

**Homebrew (macOS)**

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
cheep            # first run walks you through setup, then drops you in the shell
```

On first launch cheep asks you to configure an **orchestrator** and, optionally, one or
more **executors**. Then you get an interactive prompt:

```
› build a small CLI todo app with tests
```

Type a task and the orchestrator plans, delegates, verifies, and reports back.

## Configuration

cheep keeps everything in its home directory, **`~/.cheep/`** (override with `CHEEP_HOME`):

```
~/.cheep/
├── config.json   # your agents (orchestrator + executors)
└── keys.env      # your API keys, loaded automatically on startup
```

**Set it up** — run the wizard any time:

```sh
cheep config              # interactive setup / reconfigure
cheep config show         # print the current setup
cheep config path         # print the config file location
```

The wizard asks for an **orchestrator** (blank endpoint = Anthropic/Claude; or paste any
OpenAI-compatible endpoint), then optionally lets you add **executors**. For executors you
give only an **endpoint + access key** — cheep detects the model behind it automatically.
If you configure no executors, the orchestrator runs **solo** and does the work itself.

**Keys** — store them once in `~/.cheep/keys.env` instead of exporting them:

```sh
cheep keys                # creates the file and prints its path
```

```ini
# ~/.cheep/keys.env  (one KEY=value per line, loaded on startup)
ANTHROPIC_API_KEY=sk-ant-...
```

Anything exported in your shell takes precedence over `keys.env`. Executor keys can also be
saved inline during `cheep config`.

**Check connectivity** at any time:

```sh
cheep check               # pings the orchestrator and every executor
```

### Example: Claude orchestrator + local Qwen executor

1. `cheep config` → leave the orchestrator endpoint blank (Claude), choose a model.
2. Add an executor → endpoint `http://127.0.0.1:1234`, blank key → cheep detects the model.
3. Put `ANTHROPIC_API_KEY=sk-ant-...` in `~/.cheep/keys.env`.
4. `cheep` → start working.

## Commands

| Command | What it does |
|---|---|
| `cheep` | Start the interactive shell (slash commands: `/config`, `/status`, `/clear`, `/help`, `/exit`) |
| `cheep run "<task>" [--workdir DIR]` | Run a single task non-interactively |
| `cheep check` | Ping the orchestrator and every executor |
| `cheep config [show\|path]` | Set up or inspect your agents |
| `cheep keys` | Show/create the key store |
| `cheep version` | Print the version |

## How it works

```
            ┌──────────────┐   delegate (parallel)   ┌──────────────┐
   task ──▶ │ orchestrator │ ──────────────────────▶ │  executor(s) │
            │  (planner)   │ ◀────────────────────── │  (workers)   │
            └──────────────┘   status + summary       └──────────────┘
                  │  verifies with read_file / run_bash
                  ▼
              final answer
```

The orchestrator and executors share one generic agent loop (`internal/agent`) over two
provider backends (`internal/provider`: Anthropic + any OpenAI-compatible endpoint). The
orchestrator's `delegate` tool fans subtasks out across executors **in parallel**; when the
workspace is a git repo, each runs in its own **worktree** and its changes are merged back
automatically (`internal/worktree`). Executor file access is **confined to the workspace**.
Each run returns a status — `completed`, `max_turns`, `looping`, `context_exhausted`, or
`error` — which the orchestrator uses to accept the work or recover it.

## Build from source

```sh
go build -o cheep .
go test ./...
```

## License

MIT
