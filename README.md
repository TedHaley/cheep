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

## Project launchpad & validation

cheep is also the entry point for setting up any project for agentic work:

```sh
cheep init                # AGENTS.md (+ CLAUDE.md link), detected validation checks,
                          # git init, and a toolbelt skill for other harnesses
cheep validate [--review] # run the AGENTS.md '## Validation' checks (+ optional
                          # fresh-context AI review of your branch diff); exit 0 = pass
cheep worktree …          # pooled, artifact-warm git worktrees for parallel work
```

Every agent's prompt carries your global `~/AGENTS.md` and the project's
`AGENTS.md`; corrections you give are written back to its `## Lessons` section
via the `record_lesson` tool. Delegated subtasks are validated (checks + a
fresh-context reviewer with bounded fix rounds) **before** their worktree
merges; failed work stays on its branch, never lost. These files use the open
AGENTS.md / Agent Skills standards, so Claude Code, Codex, and friends read
the same setup — and can shell out to `cheep validate` / `cheep worktree` via
the bundled skill. Skills load project-first from `.agents/skills` and
`.claude/skills`, then `~/.claude/skills` and `~/.cheep/skills` (SKILL.md
directories or flat `.md` files). Steer which executor gets which kind of
work with natural-language rules in `.cheep/dispatch.json`, and gate risky
tool calls with `/approval yolo|auto|approve` (file writes preview as diffs).

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
how much you saved by not running everything on a premium model. Per-token prices are fetched
from the maintained [LiteLLM dataset](https://github.com/BerriAI/litellm) (cached in
`~/.cheep/prices.json`, refreshed weekly), falling back to a built-in table offline; override
any agent with `"price_in"` / `"price_out"` (USD per 1M tokens) in `config.json`.

**Budget-aware routing** — executors are presented to the orchestrator cheapest-first with
cost tiers (`free · local`, `$ cheap`, `$$ mid`, `$$$ premium`) and the project's budget, and
it's told to prefer the cheapest executor that can do each subtask — reserving pricier models
for hard work (a failed cheap attempt auto-escalates anyway).

**Cheap-first escalation** — when a delegated subtask ends in a non-`completed` status
(loops, times out, runs out of context, or errors), cheep automatically retries it on a more
capable, pricier executor before giving up — so premium models are used only when the cheaper
ones can't cope. Executors are ranked by estimated cost (local first); each result includes an
`"escalation"` trail (e.g. `qwen:looping → deepseek:completed`) when it happened. Turn it off
with `"disable_escalate": true` in `config.json`.

**Prompt caching** — on Anthropic roles, cheep caches the system prompt, tool definitions, and
the conversation prefix, so the repeated context across an agent's many turns is billed at a
fraction of the input price. OpenAI-compatible endpoints (DeepSeek, etc.) cache automatically
server-side. The cost meter reflects the savings.

**Budget cap** — set an optional **per-project** ceiling with `/budget 5` (stored per working
directory; or a global `"budget_usd": 5` / per-project `"budgets"` map in `config.json`). cheep
warns at 80% and stops the running task at 100%; `/budget` shows the current spend, `/budget
off` clears it for this project.

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
| `cheep pi <add\|remove\|list>` | Run [pi](https://pi.dev) coding-agent extensions inside cheep (see below) |
| `cheep version` | Print the version |

### Slash commands (in the shell)

`/config` (discovery configurator) · `/setup` (configure by chatting) · `/history` (browse
and resume past conversations) · `/fork` (branch the conversation from an earlier turn) ·
`/tree` (navigate the session tree) · `/prompts` (list `/name` prompt templates) · `/stow`
(sweep lessons + a handoff note to disk before a reset) · `/delivery` (how validated work
lands: `merge` or `pr`) · `/tokens` (tokens **and estimated $** per model, with local
savings) · `/status` (current setup) · `/keeptabs` (toggle auto-closing finished executor
tabs) · `/copy` (copy the last reply to the clipboard) · `/mouse` (release the mouse so
text selects normally) · `/close` or `Ctrl+W` (close the focused executor tab) · `/clear` ·
`/help` · `/exit`.

### Modes

The interactive shell has three modes, switchable mid-conversation (your history carries over):

- **chat** — talk only, no tools, no changes.
- **plan** — read-only investigation; produces a step-by-step plan for you to approve.
- **auto** — full autonomy: plan, delegate to executors, edit, and verify (the default).
- **loop** — auto with a goal, iterating until it's met or progress **plateaus** (two rounds
  without improvement — the goal decides when to stop, not vibes). The goal can be **numeric**
  (a shell command whose output is a number — coverage %, benchmark time, lint count — plus a
  direction and target; powered by `iterate_metric`) or a **coverage** goal ("do X for every
  item in a set that isn't fully known up front" — e.g. replicate every capability of a site,
  port every endpoint). For coverage, cheep maintains a growing checklist and works one narrow
  chunk per round — sized to each executor's context window — until the list is empty.

Press **Shift+Tab** to cycle modes live (the prompt shows the current one: `⏵⏵ auto`,
`⏸ plan`, `⏵ chat`, `∞ loop`). Or use `/chat` `/plan` `/auto` `/loop` / `/mode`.

### Full-screen agent tabs (default) — or inline

cheep runs as a full-screen shell with a **tab per agent**: the orchestrator plus one for
each executor it spawns, with live status glyphs (`●` `✓` `⚠` `✗`). **Tab**/`Ctrl+←/→`
switch agents, the wheel or `PgUp/PgDn` scrolls, `/keeptabs` and `Ctrl+W`//`/close` manage
tabs, and a persistent status line shows the mode, context gauge, and **token counter**
(orchestrator vs executor usage, local tokens flagged free). Copying text: `/copy` grabs the
last reply, `/mouse` releases the wheel so drag-select works (sticky; cheep disables the
terminal's wheel→arrow-key translation so a released wheel never types junk), or hold
Option/Shift while dragging.

Prefer Claude Code-style rendering? `"inline": true` in config.json prints the conversation
into your terminal's own scrollback — native scrolling, selection, and ⌘F search, executor
output interleaved with a dim `⟨name#1⟩` prefix. When stdin isn't a terminal (pipes/CI),
cheep falls back to a simple line-based mode.

### Chat history & the session tree

Every conversation is saved to `~/.cheep/history/` — a JSON record (used to resume into the
agent's context) plus a human-readable Markdown transcript. Press `/history` (or `/resume`)
to browse past sessions and reopen one where you left off.

Sessions form a **tree**: `/fork` branches the current conversation from any earlier user
turn — everything before it is kept, the abandoned tail stays on the old branch — so you can
try an alternative approach without re-paying for the shared context. `/tree` shows every
session with forks nested under their parent; pick one to switch.

### Prompt templates

Reusable prompts are markdown files in `.cheep/prompts/*.md` (project) or
`~/.cheep/prompts/*.md` (global; a project file shadows a global one of the same name),
invoked as `/name args...`. `$ARGUMENTS` expands to everything after the name, `$1..$9` to
individual arguments; optional front-matter (`description:`) labels the autocomplete entry.
`/prompts` lists what's available.

### Scout vs ship subtasks

The orchestrator marks each delegated subtask `"ship"` (delivers file changes through the
usual validate-and-merge machinery) or `"scout"` (investigation, audit, research, planning).
A scout's file changes are **discarded**; its findings come back directly and are saved as a
report under `~/.cheep/reports/` — no merge machinery runs, so cheap-model research gets
even cheaper.

### No-mistakes mode

`/nomistakes on` (firstmate's strictest safety posture) makes cheep incapable of changing
your checkout without an explicit yes: every shared-workspace write and shell command asks
first, and a validated subtask branch is merged **only after you approve its diff** in the
approval overlay (`y`/`n`, scrollable preview). Declined or unattended work is never lost —
it stays on its branch, and headless runs (`cheep run`, CI) hold everything on branches
because there is no approver. `/nomistakes off` returns to your configured `/approval` mode
and automatic merges.

### Delivery: local merge or pull requests

By default validated worktree changes merge into your local checkout. `/delivery pr` (or
`"delivery": "pr"` in config.json) instead pushes each validated subtask branch and opens a
**pull request** with the `gh` CLI — the local checkout is never modified, which makes cheep
safe to point at shared repos.

### Interrupted work is never lost

Every delegated subtask writes a crash marker while it runs; if cheep dies mid-delegation,
the next launch lists what was interrupted and where partial work survived (worktree
branches are quarantined, never recycled, until their work provably lands). `/stow` is the
graceful version: before a `/clear` or a walk-away it records durable lessons via
`record_lesson` and appends a structured handoff note (done / in flight / next steps) to
`~/.cheep/history/notes.md`.

### Working across directories

cheep's file tools are scoped to the workspace (that's what makes worktree isolation real),
so if you launch in one repo and ask it to work on another, `read_file`/`write_file` can't
reach it. `/cd <path>` moves the whole session — tools, worktree pool, and project
instructions — to another directory, keeping the conversation. (Or just launch cheep from
the target repo.)

### Per-model context windows

For cloud models, cheep reads the context window straight from the LiteLLM dataset it
already caches for pricing (`~/.cheep/prices.json`) — no configuration needed. For local
models (which rarely appear there, since the window is a load-time choice) set
`"context_window"` explicitly. Either way, cheep sizes everything to fit: it self-compacts
at ~75% of the window and hard-stops just under it, so a small local model **compresses and
continues** mid-task instead of dying with `context_exhausted`. Each
compaction is saved to `~/.cheep/history/notes.md`, and the orchestrator sees each
executor's window in its roster — so it splits big jobs ("copy every capability of this
site") into chunks that fit rather than overflowing one session.

```json
{ "name": "local-qwen", "provider": "openai",
  "endpoint": "http://127.0.0.1:1234/v1", "model": "qwen/qwen3.6-35b-a3b",
  "context_window": 8000 }
```

### Context bar, auto-compression, and saved memories

The status bar shows a live **context gauge** (`ctx ▰▰▰▱▱▱▱▱ 38%`) — how full the
orchestrator's conversation is relative to its compaction budget (`context_budget`,
default 120k est. tokens; green → yellow at 60% → red at 85%). At 100% cheep
**auto-compresses**: older history is summarized in place, recent turns stay verbatim, and
the squeezed-out summary is appended to `~/.cheep/history/notes.md` — compression never
silently discards memory. If a model's real window is smaller than the budget (common with
local models) and the server rejects a request as too long, cheep compacts aggressively in
bounded chunks and retries automatically instead of failing the run.

### Reasoning effort per role

Append `:low`, `:medium`, or `:high` to any model name (`"claude-sonnet-4-6:high"`,
`"gpt-5:low"`) to set that role's thinking budget — extended thinking on Anthropic,
`reasoning_effort` on OpenAI-compatible endpoints. An orchestrator at `:high` with executors
at `:low` is the cost thesis expressed in one suffix. Ollama-style tags (`qwen3:8b`) are
untouched.

### Use it from your phone

cheep in tmux + Tailscale SSH = the same live session from anywhere, surviving disconnects.
See [docs/remote.md](docs/remote.md) for the 5-minute setup.

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

**Pi extensions** — cheep can run [pi coding agent](https://pi.dev) extensions published to
npm (or local ones). `cheep pi add <npm-package>` installs the package into `~/.cheep/pi`
and registers it; on the next start a bundled Node bridge loads the extension (TypeScript
included, via jiti), honors its `pi.extensions` package manifest, and serves every tool it
registers over MCP stdio — so pi tools join your agents' tool set like any MCP server's,
named `pi__<tool>`. Only the **tool surface** crosses the bridge: pi event hooks, commands,
custom renderers, and providers need pi's own runtime and are skipped (reported once on
startup). Review third-party extensions before installing — they run with full system
access. Requires `node` on PATH.

**Skills** — drop markdown knowledge files in `~/.cheep/skills/*.md` (optional `name` /
`description` front-matter). The planner calls `list_skills` to see them and `use_skill(name)`
to load one into context **only when relevant** — keeping prompts small (and cheap) instead of
stuffing all knowledge into every request.

**Loops** — for work with an objective check, the orchestrator can call
`iterate_until{ "subtask", "check", "executor" }`: it re-runs the subtask on a cheap executor
and re-runs the shell `check` (e.g. `go test ./...`) until it exits 0, feeding each failure
back in — bounded by `max_rounds` and the budget. Iterating on cheap/local executors is nearly
free, so it's the preferred way to "fix until green."

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
