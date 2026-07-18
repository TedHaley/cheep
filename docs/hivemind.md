# /hivemind — decentralized peer findings

A `/hivemind` mode where cheep instances share **distilled findings** peer-to-peer,
so ideas and discoveries cross-pollinate across people working on similar things.
Findings live as a **side context you pull from** — never auto-injected.

## Principles

1. **Fully decentralized.** No central hub. Peer-to-peer over libp2p (gossipsub +
   Kademlia DHT + mDNS + NAT traversal). Bootstrap is a default-but-replaceable
   seed list; mDNS gives zero-infra discovery on a LAN.
2. **Pull, don't inject.** Peer findings are a browsable side pool. Nothing enters
   your agent's context unless *you* pull a specific finding in. This is the whole
   safety model — it removes the prompt-injection/poisoning attack surface.
3. **Distilled findings only.** The unit of sharing is an insight/lesson/decision
   (what cheep already produces via `record_lesson` and `/stow`), plus tags — never
   raw transcripts, code, or secrets.
4. **Signed.** Every finding is signed by its author's keypair, so you can trust,
   filter, or mute by author even in the open swarm.
5. **Two context types, one transport.** Private group (encrypted, membership-gated)
   and open swarm (public) are just different gossipsub topics.

## Architecture

`internal/hivemind` (new package):

- **Identity**: a libp2p keypair per user, stored in `~/.cheep/hivemind/`. Also the
  signing key for findings.
- **Transport**: libp2p host — gossipsub for propagation, Kademlia DHT for discovery,
  mDNS for LAN peers, AutoNAT + DCUtR hole-punching + circuit-relay fallback for NAT.
- **Finding record** (signed): `{id, author_pubkey, created, tags[], title, body,
  project_hint?, sig}`. Small; insight-only.
- **Local pool**: received findings persisted to `~/.cheep/hivemind/pool/`, deduped by
  id, filterable by tag/author. This is the "side context."
- **Matching**: v1 by overlapping tags + project hint. Later: embeddings for semantic
  "similar work" surfacing.

## Trust models

- **Private group**: created/joined via an invite (a shared group key). Messages on the
  group topic are encrypted with it; only key-holders can read. Membership = key
  possession (optionally tightened to an allowlist of pubkeys).
- **Open swarm**: a well-known public topic. Anyone can publish/subscribe. Findings are
  plaintext but signed; you filter by author reputation/mutes.

## UX

- `/hivemind` — open the **side-context browser**: peer findings relevant to your work
  (tag/project matched), searchable, with author + age. Pull one to add it into your
  session as an attributed reference (clearly framed as external data).
- `/hivemind join <invite>` / `/hivemind leave` — private group membership.
- `/hivemind swarm on|off` — subscribe to / leave the open swarm.
- `/hivemind share` — publish a finding (opt-in, explicit). Offer to share the
  session's recorded lessons on `/stow`.
- `/hivemind mute <author>` — hide an author's findings.

Nothing is shared automatically; nothing is ingested automatically.

## Security

- No auto-injection. Pulled findings are wrapped as untrusted reference data the agent
  is instructed never to execute as commands.
- Findings-only payload; a redaction pass strips anything that looks like a secret/path
  before a finding leaves your machine.
- Private groups are end-to-end encrypted; bootstrap/relay nodes are blind.
- Sharing is always explicit opt-in.

## Phased plan

- **Phase 1 — local proof (zero infra).** Identity + signed finding record + local pool.
  libp2p host with **mDNS only** + gossipsub open topic. Two cheep instances on one LAN
  share findings. TUI side-context browser + pull-to-reference + `/hivemind share`.
- **Phase 2 — internet swarm + private groups.** Kademlia DHT + default bootstrap list
  (user-replaceable). Encrypted private-group topics via invite keys. Author mute/filter.
- **Phase 3 — NAT hardening + semantic matching.** DCUtR/relay fallback; embeddings so
  relevant findings surface in the side pane automatically (still pull-only to ingest).

## Open questions

1. Redaction: rule-based (regex for keys/paths) to start, or also an LLM redaction pass?
2. Invite format for private groups — short shareable code vs key file?
3. Should `/stow` auto-*offer* to publish its lessons, or stay fully manual?
