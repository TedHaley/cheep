# cheep-claude-mcp (experimental)

A tiny stdio MCP server that lets cheep **delegate tasks to your Claude Code
subscription — without an Anthropic API key**.

It exposes one tool, `ask_claude(task, [cwd])`, which shells out to headless
Claude Code (`claude -p "<task>" --output-format json`) and returns the result.
Because it uses the official `claude` CLI, it runs under whatever that CLI is
logged in with — including a Pro/Max membership.

```
cheep (local orchestrator, free)
   │  calls  claude__ask_claude("design the refactor")
   ▼
cheep-claude-mcp  ──shell──▶  claude -p …   ← your Claude Code login, no API key
```

## Build

```sh
go build -o ~/.local/bin/cheep-claude-mcp ./cmd/cheep-claude-mcp
```

(Not shipped in releases — build it yourself.) Requires the `claude` CLI
installed and logged in (`claude` once, interactively, to authenticate).

## Wire it into cheep

Add to `~/.cheep/config.json`:

```json
"mcp": {
  "claude": { "command": "cheep-claude-mcp", "roles": ["orchestrator"] }
}
```

The orchestrator then gets a `claude__ask_claude` tool. In auto mode it can hand
heavy reasoning or hard changes to Claude while the local/cheap agents do the
routine work. Scope it to `["executor"]` instead if you'd rather executors call it.

## Env knobs

| Variable | Default | Meaning |
|---|---|---|
| `CHEEP_CLAUDE_BIN` | `claude` | Path to the Claude Code CLI |
| `CHEEP_CLAUDE_ARGS` | — | Extra flags, e.g. `--model sonnet --permission-mode acceptEdits` |
| `CHEEP_CLAUDE_TIMEOUT` | `600` | Seconds before a task is aborted |

Tasks that need Claude to edit files or run commands will hit Claude Code's
permission prompts in headless mode — pass `--permission-mode acceptEdits` (or
`--allowedTools …`) via `CHEEP_CLAUDE_ARGS` for those.

## Caveats

- **Unsupported / gray area.** Programmatically driving a subscription via
  headless Claude Code is subject to Anthropic's usage policy and Max-plan rate
  limits. It may break. Check current terms before relying on it.
- **Coarse-grained.** Each call runs a fresh Claude Code session in `cwd`
  (default: the MCP server's working dir) with its own tools — there's no cheep
  worktree confinement around it, and no shared context between calls.
- **Fragile.** Depends on the `claude` CLI interface and login state.
