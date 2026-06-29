# CHEEP!

> The most results per dollar of compute. A smart orchestrator plans and verifies; cheap or local executors do the work — and you watch every token and dollar.

**cheep** is a tiny, single-binary, interactive multi-agent coding shell built around one
idea: **get the best (or nearly the best) results for the least money.** A lead
**orchestrator** agent breaks a task into pieces, hands each to one or more **executor**
agents (in parallel), verifies the work, and recovers any executor that gets stuck — loops,
runs out of context, or errors out. Every role can point at an Anthropic or any
OpenAI-compatible endpoint, so you spend premium tokens only where judgment matters and run
the rest on cheap or local models — with a live **token-and-dollar meter** showing what each
task actually cost and what you saved. It's one static binary with zero runtime dependencies.
cheep always runs **both** roles (an orchestrator + at least one executor); the same model
can fill both if that's all you have.

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
cheep            # first run opens the setup configurator, then drops you in the shell
```

On first launch (and any time a role is missing) cheep opens a **setup configurator**: it
scans your machine for running model servers (Ollama, LM Studio, vLLM, llama.cpp, …) and API
keys, and you pick an **orchestrator** and at least one **executor** — or press `m` to enter
cloud credentials manually (Anthropic, OpenAI, Grok, DeepSeek, …). With a single local model
you can use it for both roles. Then you get an interactive prompt:

```
› build a small CLI todo app with tests
```

Type a task and the orchestrator plans, delegates, verifies, and reports back. Reopen the
configurator any time with `/config`.

## Configuration

cheep keeps everything in its home directory, **`~/.cheep/`** (override with `CHEEP_HOME`):

```
~/.cheep/
├── config.json   # your agents (orchestrator + executors)
├── keys.env      # your API keys, loaded automatically on startup
└── history/      # saved conversations (JSON records + Markdown transcripts)
```

**Set it up** — two ways, both interactive:

- **In the shell:** `/config` opens the **discovery configurator** — it lists discovered
  local servers (with their models) and any API keys it found; pick an orchestrator with `o`,
  tag executors with `e`, or press `m` to enter cloud credentials manually. `/setup` instead
  lets you configure by chatting with a working agent (see below).
- **From the CLI:**

  ```sh
  cheep config              # line-based setup / reconfigure
  cheep config show         # print the current setup
  cheep config path         # print the config file location
  ```

cheep needs **both** an orchestrator and at least one executor — there's no solo mode. If you
only have one model, use it for both roles (in the picker, tag the same row with `o` and `e`).
For executors you give only an **endpoint + access key** — cheep detects the model behind it.

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

**Cost estimates** — `/tokens` shows estimated spend per model (local models are free) and
how much you saved by not running everything on a premium model. Prices come from a built-in
table; tune any agent with `"price_in"` / `"price_out"` (USD per 1M tokens) in `config.json`.

**Cheap-first escalation** — when a delegated subtask ends in a non-`completed` status
(loops, times out, runs out of context, or errors), cheep automatically retries it on a more
capable, pricier executor before giving up — so premium models are used only when the cheaper
ones can't cope. Executors are ranked by estimated cost (local first); each result includes an
`"escalation"` trail (e.g. `qwen:looping → deepseek:completed`) when it happened. Turn it off
with `"disable_escalate": true` in `config.json`.

**Check connectivity** at any time:

```sh
cheep check               # pings the orchestrator and every executor
```

### Example: Claude orchestrator + local Qwen executor

1. Start your local model (e.g. LM Studio or Ollama) on an OpenAI-compatible endpoint.
2. `cheep` → in the configurator, press `m` → **Anthropic (Claude)** → enter a model and paste
   your `ANTHROPIC_API_KEY`. (Or set the orchestrator to your local model for a $0 setup.)
3. Back in the list, tag the discovered local Qwen as the executor with `e`, then save.
4. Start working.

## Commands

| Command | What it does |
|---|---|
| `cheep` | Start the interactive shell |
| `cheep run "<task>" [--workdir DIR]` | Run a single task non-interactively |
| `cheep check` | Ping the orchestrator and every executor |
| `cheep config [show\|path]` | Set up or inspect your agents (line-based CLI wizard) |
| `cheep keys` | Show/create the key store |
| `cheep version` | Print the version |

### Slash commands (in the shell)

`/config` (discovery configurator) · `/setup` (configure by chatting) · `/history` (browse
and resume past conversations) · `/tokens` (tokens **and estimated $** per model, with local savings) · `/status`
(current setup) · `/keeptabs` (toggle auto-closing finished executor tabs) · `/close` or
`Ctrl+W` (close the focused executor tab) · `/clear` · `/help` · `/exit`.

### Modes

The interactive shell has three modes, switchable mid-conversation (your history carries over):

- **chat** — talk only, no tools, no changes.
- **plan** — read-only investigation; produces a step-by-step plan for you to approve.
- **auto** — full autonomy: plan, delegate to executors, edit, and verify (the default).

Press **Shift+Tab** to cycle modes live (the prompt shows the current one: `⏵⏵ auto`,
`⏸ plan`, `⏵ chat`). Or use `/chat` `/plan` `/auto` / `/mode`.

### Agent tabs

In a terminal, cheep runs as a full-screen shell with a **tab per agent**: the orchestrator
plus one for each executor it spawns (with a live status — running `●`, done `✓`, stuck `⚠`,
error `✗`). Press **Tab** (or `Ctrl+←/→`) to switch agents and watch each one's output;
`PgUp/PgDn` (or the mouse wheel) scrolls. Finished executor tabs **auto-close at the end of a
turn** by default — toggle with `/keeptabs`, or close the focused one with `Ctrl+W` / `/close`.
A persistent **token counter** shows orchestrator vs executor usage and flags local tokens as
free. When stdin isn't a terminal (pipes/CI), cheep falls back to a simple line-based mode.

### Chat history

Every conversation is saved to `~/.cheep/history/` — a JSON record (used to resume into the
agent's context) plus a human-readable Markdown transcript. Press `/history` (or `/resume`)
to browse past sessions and reopen one where you left off.

### Configure by chatting

Once one agent is reachable, `cheep setup` (or `/setup`) lets you configure the rest in plain
language — the working agent probes endpoints, detects models, and writes the config for you:

```
setup › add an executor named local-2 at http://127.0.0.1:1234
```

## Tools & MCP

**Built-in tools** live in `internal/tool/tool.go` (`Make()`). A tool is a
`core.Tool{Name, Description, Parameters (JSON Schema), Func}`; append one there and it's
available to every agent — no other changes needed.

**MCP servers** plug in via the `mcp` section of `~/.cheep/config.json`. cheep launches each
server, lists its tools, and exposes them as `<server>__<tool>`. Both **stdio** (`command`)
and **HTTP** (`url`, Streamable HTTP / SSE) transports are supported, and each server can be
scoped to specific roles with `roles` (`"orchestrator"`, `"executor"`; default both):

```json
"mcp": {
  "fs":     { "command": "npx", "args": ["-y", "@modelcontextprotocol/server-filesystem", "/path"],
              "roles": ["executor"] },
  "github": { "url": "https://api.example.com/mcp", "headers": { "Authorization": "Bearer …" } }
}
```

On launch you'll see `mcp "fs": N tool(s)`. A failed server is reported and skipped — cheep
still runs.

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
