# cheep UI/UX Requirements

Source of truth for the interface design. Edit freely — anything I build gets checked against this.

## Goal

A coding-agent shell whose interface feels like a first-class app: a persistent
frame (agent banner + input) with a scrollable conversation, where scrolling and
copying "just work" with the mouse.

## Requirements

Set the Priority column yourself (MUST / SHOULD / NICE). The "Feasible in a
terminal?" column is my honest assessment — see the Platform Decision below.

| # | Requirement | Priority | Feasible in a terminal? |
|---|-------------|----------|--------------------------|
| R1 | Full-screen layout (not inline scrollback) | ? | ✅ |
| R2 | Orchestrator/executor banner pinned at the **top**, stays put while the chat scrolls | ? | ✅ (alt-screen) |
| R3 | Input box pinned at the **bottom**, always visible | ? | ✅ (alt-screen) |
| R4 | Scroll the conversation with the **mouse wheel / trackpad** | ? | ✅ only if the app captures the mouse |
| R5 | Select & copy text with a **plain mouse drag** (no modifier key) | ? | ✅ only if the app does NOT capture the mouse |
| R6 | Clean start — no leftover terminal artifacts on launch | ? | ✅ (done) |
| R7 | Conversation spacing: blank line between turns, no separator rules | ? | ✅ (done) |
| R8 | Ghost-text next-step suggestions, accept with Tab | ? | ✅ (done) |
| R9 | ↑/↓ input recall within the current session only (NOT persisted across sessions) | ? | ✅ (done) |
| R10 | Keyboard editing: ⌥←/→ word, ⌥⌫ word-delete, Ctrl+A/E, Ctrl+C clear, Esc stop | ? | ✅ (done) |
| R11 | Self-update: `/upgrade` + launch notification | ? | ✅ (done) |

## The core conflict (why this keeps going in circles)

R4 (wheel scroll) and R5 (plain-drag copy) use the **same mouse channel** in a
terminal, and R2/R3 (pinned frame) require the app to own the screen. Pick any
two of these three — a terminal cannot do all three at once:

- **A — Full-screen, mouse ON:** R1 R2 R3 R4 ✅ · R5 needs ⌥-drag to copy.
- **B — Full-screen, mouse OFF:** R1 R2 R3 R5 ✅ · R4 gone — scroll with PgUp/PgDn.
- **C — Inline (Claude Code style):** R4 R5 ✅ (both native) · R2/R3 gone — banner/input sit in scrollback and scroll away.

This is a property of terminals, not of Go or Bubble Tea. Every terminal TUI
(vim, k9s, lazygit, htop) lands in row A: wheel scrolls, and you hold a modifier
to select. Claude Code lands in row C (it is not full-screen).

## Platform Decision (the real fork)

**Only a GUI can satisfy R2 + R3 + R4 + R5 simultaneously** — a pinned frame with
OS-native scroll and OS-native copy. If all four are MUST, the honest path is a
desktop/web app (Tauri/Electron/web), which is a separate, larger build — not a
terminal rewrite in another language (Rust/TS would hit the identical wall).

- [ ] **Stay in the terminal** → then rank the trade-off: is wheel-scroll (A) or
      plain-copy (B) the one you can't live without? I set that mode and we're done.
- [ ] **Go GUI** → we scope a real app where all of R1–R5 hold. Bigger effort,
      but it's the only thing that delivers the full set.

## Open questions for you

1. Of R2, R4, R5 — if you could only keep two in the terminal, which two?
2. Is the pinned frame (R2/R3) a hard MUST, or is "visible, scrolls with history"
   (Claude Code style) acceptable?
3. Terminal or GUI?
