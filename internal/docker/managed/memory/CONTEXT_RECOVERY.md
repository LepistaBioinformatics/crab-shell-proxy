# Recovering full conversation history

Your live session context can be reset when this workspace's container restarts
(idle scale-down, a settings change, a redeploy). When that happens you may lose
the earlier turns of the current conversation from your working context — but the
**complete history is preserved for you** and you can read it back.

## Where the full history lives

Append-only, next to your live session files:

- `workspace/sessions/durable/<session-key>.jsonl` — one per conversation. It
  **only ever grows and is never overwritten**, even across restarts. (Your live
  session file can be rewritten with only recent turns; this one is not.)

Each line is a JSON object with `role`, `content`, and `created_at`. Read it like
any workspace file — it is maintained for you and is read-only.

## When and how to use it

If you seem to be missing earlier messages from the current conversation — the
user refers to something you don't recall, asks about the start of the chat, or
asks how many messages were exchanged — **read the matching
`*.jsonl` in `workspace/sessions/durable/`** (the one whose recent lines match
the current thread) to recover the full transcript before answering.

Prefer recovering from this file over guessing or saying you don't remember.
