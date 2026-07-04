# Agentic workflow upgrade — design & phase plan

Goal: fold the practices from Kun Chen's agentic engineering workflow (and the best
of his firstmate project) into cheep, and make cheep the launchpad for configuring
any project — usable either as the harness itself or as a toolbelt other harnesses
(Claude Code, Codex, OpenCode) borrow via open-standard files and headless
subcommands.

Decisions locked in:
- BYO harness = open-standard files (`AGENTS.md`, SKILL.md dirs) + CLI toolbelt
  (`cheep validate --json`, `cheep worktree ... --json`).
- Validation checks live in a parsed `## Validation` section of the project
  AGENTS.md (fenced ```check blocks), one human+machine source of truth.
- Approval system = Goose-style modes (yolo/auto/approve) + inline diff preview
  with accept/reject.

## Phases

| # | Content | Packages |
|---|---------|----------|
| 0 | `Build()` Options refactor; `core.Event` json tags | orchestrator, core |
| 1 | AGENTS.md loading (global `~/AGENTS.md` + project, walk-up, CLAUDE.md fallback), prompt injection for all roles incl. executors (closure reads repo root, not worktree); `## Validation` checks parser; `record_lesson` tool → `## Lessons`; skills multi-path (`.agents/skills`, `.claude/skills`, `~/.claude/skills`, `~/.cheep/skills`) + SKILL.md dir support, project-first dedup | project (new), skills, orchestrator |
| 2 | Worktree pool: `~/.cheep/worktrees/<key>/` slots, flock locking, fail-closed landed check (state file AND `merge-base --is-ancestor`), recycle = detach + reset --hard + `clean -fd` (no `-x` — cached deps survive), quarantine on doubt, ephemeral fallback; `cheep worktree acquire/release/list --json` | worktree |
| 3 | Validation pipeline: checks in worktree → fresh-context reviewer (read-only tools, branch diff, JSON verdict) → bounded fix rounds → merge only if passed, else branch kept. Checks fail-closed; reviewer parse fail-open unless `Strict`. Config `Validation{...}`, optional `Reviewer` role. Run notes via history. `cheep validate` CLI (exit 0/1/2) | validate (new), orchestrator, config, history |
| 4 | `cheep init`: git init, AGENTS.md template w/ detected build/test/lint commands, CLAUDE.md symlink, toolbelt SKILL.md; agent-assisted rewrite pass (solo agent) unless `--no-assist` | project, main |
| 5 | Approval gate: modes, `Wrap` on shared-workdir tools only (worktree executors ungated — the merge boundary is their gate), Myers diff + lipgloss colors, TUI overlay y/n/a/esc, `/approval` | approve (new), tui, orchestrator |
| 6 | TUI polish: slash autocomplete dropdown, `@`-file fuzzy mentions, `/model` (preserves session), window/tmux titles, `cheep run --json` (JSONL events, exit 0/1/2) | tui, main |
| 7 | NL dispatch rules (`.cheep/dispatch.json`: rules[]{when,use{executor,model},why}; LLM matches, delegate() enforces explicit executor when rules exist); liaison prompt rules (no internal vocab, full URLs, plain failure reporting); exit confirmation while running | dispatch (new), orchestrator, tui |

## Key invariants

- All git mutations stay under `gitMu`; validation checks/review/fix run outside it.
- Pool recycling: `clean -fd` without `-x` is load-bearing (pinned by test).
- Never destroy unlanded work: quarantine, don't recycle; keep branch on failure.
- Executors read project instructions from the repo root (uncommitted AGENTS.md
  edits and fresh lessons reach new sessions), not their worktree copy.
- Prompt bloat: project block clipped ~16KB; dispatch/liaison blocks terse.

Full research and rationale: session notes; firstmate reference clone in scratchpad.

## Addendum — pi.dev + firstmate round 2 (shipped 2026-07-04)

Second wave, folding in pi's best ideas and firstmate's remaining ones:

| # | Content | Packages |
|---|---------|----------|
| 8 | Thinking-level shorthand: `model:low\|medium\|high` suffix → Anthropic extended thinking (blocks preserved/replayed on tool turns) / OpenAI `reasoning_effort`; pricing strips the suffix; Ollama tags (`qwen3:8b`) untouched | core, provider, pricing |
| 9 | Prompt templates: `.cheep/prompts` + `~/.cheep/prompts` markdown, `/name args`, `$ARGUMENTS`/`$1..$9`, front-matter description, autocomplete, project-shadows-global | prompts (new), tui |
| 10 | Session tree: `Parent`/`ForkAt` on history records, `/fork` (branch from an earlier user turn), `/tree` (nested navigation), collision-safe `UniqueID` | history, tui |
| 11 | Scout tasks: `delegate` `kind:"scout"` — worktree changes discarded, findings returned + saved to `~/.cheep/reports/` | orchestrator, worktree |
| 12 | Delivery modes: `"delivery":"pr"` pushes validated branches + `gh pr create` instead of local merge; `/delivery` | config, worktree, orchestrator, tui |
| 13 | Crash recovery: per-delegation inflight markers (PID-guarded), stale ones surfaced at launch; `/stow` = record_lesson sweep + structured handoff run-note | inflight (new), orchestrator, tui |
| 14 | Pi extension bridge: embedded Node MCP-stdio bridge loads pi.dev extensions (npm or path, jiti for TS, `pi.extensions` manifest); tools-only surface; `cheep pi add/remove/list`, `"pi_extensions"` config | piext (new), mcp, config, main |
| 15 | Remote use: tmux + Tailscale SSH recipe (docs/remote.md) — no server code | docs |

Deliberately NOT taken: pi's no-subagents stance (tabs already answer the observability
objection), firstmate secondmates (roles config covers it), session backends (TUI tabs),
X mode.
