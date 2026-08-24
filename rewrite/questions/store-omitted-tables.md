# store — two tables in the Python inventory that no list claims

Raised by: Phase 2 `internal/store` build · Status: **open**

The store brief enumerates the tables to define, and separately enumerates the
coordination tables that must NOT be here because they belong to the KV layer
(D8 / d-201). Two tables in the Python migration inventory appear in **neither**
list, so I built the enumerated set exactly and left these out rather than
guessing:

| Table | Python migration | What it holds |
|---|---|---|
| `conversation_sessions` | `034_conversation_sessions.sql` | What a seat already did in ONE conversation, rendered back into the next turn of the same thread / issue / PR. Keyed `(agent_handle, conversation_key, work_key)`. |
| `a2a_channels` | `029_a2a_channels.sql` | The participants and open/closed state of an ephemeral agent-to-agent channel. |

## Why each is a real question, not an oversight to wave through

**`conversation_sessions` looks like it belongs here.** It is per-seat durable
memory with the same shape as `episodes`, and `src/crewlet/db/conversation_sessions.py`
argues at length that it is NOT episodes: episode recall is agent-scoped cosine
similarity over two summaries, with no conversation filter and no reply text,
and it is gated OFF for exactly the thin pointer-shaped triggers that continue a
conversation. If it is dropped, a seat re-reads a Slack thread it already
answered with no memory of its own reply.

It would also land cleanly in the dialect: its Python migration notes that it is
a REGULAR table (not a hypertable), so its dedupe is already "a plain unique
index and an ordinary `ON CONFLICT DO NOTHING`" — no NULL-mapping needed, and no
advisory-lock idiom to delete.

**`a2a_channels` looks like it belongs in the KV layer.** Its rows are read by
every authorization decision on an A2A channel and swept on an idle timeout —
that is coordination state with a TTL, the same shape as the tables the brief
sends to KV. But it is not in the KV exclusion list either, and its Python home
is `db/`, so it may simply have been missed on both sides.

## What I need

One line each: `conversation_sessions` here or elsewhere; `a2a_channels` here or
in KV. Adding either to `internal/store/schema/` afterwards is one file and
its contract test — the schema is consolidated and forward-only, so a new table
is a new numbered file, not an edit to an existing one.

Nothing is blocked in the meantime: no Go code in this phase writes to either.
