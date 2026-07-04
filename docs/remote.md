# Using cheep from your phone (or any other machine)

cheep is a plain terminal app, so the easiest portable setup needs **zero new
code**: leave cheep running inside **tmux** on your main machine and attach to
that tmux session from your phone over SSH. You get the full TUI (tabs, agent
switching, everything), the session survives disconnects and app switches, and
a long-running delegation keeps working while your phone is in your pocket.

## One-time setup

1. **Tailscale** on both devices — <https://tailscale.com> (free for personal
   use). Install on the Mac and the phone, sign into the same tailnet. This
   gives the phone a private, encrypted path to the Mac from anywhere, no port
   forwarding or public IP.
2. **Enable SSH on the Mac**: System Settings → General → Sharing → **Remote
   Login** (or `sudo systemsetup -setremotelogin on`).
3. **An SSH client on the phone**:
   - iOS: [Blink Shell](https://blink.sh) (best keyboard for TUIs, mosh
     built in) or [Termius](https://termius.com)
   - Android: Termius or JuiceSSH

## Daily use

On the Mac, start (or keep) cheep inside tmux:

```sh
tmux new -As cheep    # attaches if the session already exists
cheep
```

From the phone:

```sh
ssh <mac-tailscale-name>      # e.g. ssh teds-macbook
tmux attach -t cheep
```

You are now in the exact same cheep session — same conversation, same running
task, same tabs. Detach with `ctrl+b d` (or just close the app; tmux doesn't
care). Anything you started from the phone is still there when you sit back
down at the Mac.

## Tips

- **Flaky networks**: use [mosh](https://mosh.org) instead of ssh (`mosh
  <mac-name> -- tmux attach -t cheep`). It roams across wifi/cellular and
  survives long sleeps. Blink has it built in; `brew install mosh` on the Mac.
- **Keys on a phone**: cheep leans on Tab / Shift+Tab / Ctrl+W / Esc. Blink
  and Termius both have a key toolbar above the keyboard; slash commands
  (`/mode`, `/close`, `/history`) cover the same actions if a chord is
  awkward.
- **Kick off, walk away**: start a big delegation, detach, and check back in
  from the phone — `/tokens` and the tab glyphs (● ✓ ⚠ ✗) give the status at
  a glance. `~/.cheep/history/notes.md` has the durable run notes.
- **Don't want tmux?** [Zellij](https://zellij.dev) works the same way
  (`zellij attach`). A browser-based alternative is
  [ttyd](https://github.com/tsl0922/ttyd) (`ttyd -W tmux attach -t cheep`
  bound to the tailnet address), but SSH + tmux is less to secure.

## Why this instead of a built-in server?

A future `cheep serve` could host the shell over the web, but tmux gives
attach/detach resilience, multi-device access, and scrollback for free, with
Tailscale handling auth and encryption. That's hard to beat with new code —
and it works today.
