-- The company's own chat: the read side of the chat log.
--
-- Every table here is REBUILT FROM THE LOG rather than written by a caller.
-- The applier is the only writer, its transaction carries the checkpoint, and
-- an identity claim compares these rows byte for byte across the fleet — so
-- anything a node decides for itself belongs in the node's own estate instead.
-- There is no Divergent table in this domain at all: the applier reads no
-- epoch key, so every node writes identical rows and the claim covers the
-- whole of it.
--
-- # What this domain is
--
-- The framework's FOURTH domain and its THIRD strictly-ordered one, and the
-- first whose hot path and arbitration path share a log. Channel state — a
-- topic, a membership set, an archive, a prune — arbitrates on the channel's
-- own subject. New messages ride a SEPARATE subject kind whose id is that same
-- channel's, additively: two people talking in one room never contend, no
-- expectation is formed, no compare-and-set round is spent, and the broker's
-- per-subject index is bounded by the number of channels rather than by the
-- number of things anybody ever said.
--
-- It is also the highest-volume domain the engine has. Every sizing decision
-- below — the columns a message carries, which indexes exist, which do not,
-- and what the retention prune ranges on — is made against a declared census
-- of 20,000 messages a day with 50,000 a day as the supported ceiling.
--
-- # Why a message's row carries a per-channel sequence
--
-- `channel_seq` is minted by the APPLIER from log order, not by the writer.
-- A writer cannot mint it: posts are additive, so two writers in one room form
-- no expectation and would compute the same number. The applier can, because
-- every node applies the same records in the same order — which is exactly why
-- a message record's declared SCOPE is its CHANNEL and never the message.
-- Under a rolling upgrade the deferral cascade blocks only records whose scope
-- INTERSECTS a deferred one; a message-scoped post would let a node step over
-- a deferred sibling and mint the sequence that record should have had, and
-- the rows would differ on that node for ever on an identity-claiming domain.
-- The contiguity is what lets a browser detect a dropped live frame and refetch
-- exactly the hole rather than the channel.
--
-- # The three rules the schema enforces, and one it deliberately does not
--
-- NO FOREIGN KEY ANYWHERE. A cascade is a delete nobody committed: the applier
-- removes a channel's children in the same statement list, where the deletion
-- is part of the record's own effect and therefore identical on every node.
--
-- NO `UNIQUE` OUTSIDE A PRIMARY KEY. A constraint violation inside the apply
-- transaction aborts it deterministically on every node, which turns a rare
-- cosmetic anomaly into a fleet-wide stalled log. A channel-name collision is
-- the worked example: it cannot happen, because the NAME is the arbitration
-- subject — but a restored `chat_channel_names` beside newer channels could
-- produce one, and a unique index would wedge every node at once rather than
-- leaving one odd row for the operator surface to report.
--
-- NO `COLLATE` ANYWHERE. The default TEXT collation is BINARY, so an ORDER BY
-- reproduces the Go comparison exactly, and a name's normalisation is done in
-- Go once. A `COLLATE NOCASE` here would be a second answer to what one
-- address is.
--
-- What it does NOT enforce is the object versions' monotonicity: that is the
-- applier's `WHERE excluded.version > version` guard, because a redelivery
-- must SKIP rather than abort the transaction it arrived in.
--
-- # What the retention prune ranges on, and why children carry a parent's time
--
-- Chat is the first domain that DELETES CONTENT on a horizon. The prune is a
-- record carrying a cutoff INSTANT, and the applier range-deletes below it:
-- deterministic on every node because `created_at` is the broker's own stored
-- time rather than a clock each node reads for itself, and constant-size on
-- the wire where an explicit id list would be proportional to what it removes.
--
-- A range delete drives on an index or it is a scan of the corpus, so every
-- table the prune touches carries the two columns the range is over —
-- `channel_id` and the parent message's own `created_at` — rather than being
-- reached through a join the planner would have to walk. That denormalisation
-- is the price of a bounded delete, and it is stated here because the columns
-- look redundant to a reader who has not seen the statement list.

-- ---------------------------------------------------------------------------
-- The channel, its address, and who is in it
-- ---------------------------------------------------------------------------

-- chat_channels — one row per channel.
--
-- `message_seq` is the channel's own high-water mark, which is what the
-- applier mints the next message's `channel_seq` from. It lives on the channel
-- row rather than being derived with MAX() per post because the derivation
-- would read the largest index in the domain on the hot path of every message.
CREATE TABLE chat_channels (
    id             TEXT    NOT NULL PRIMARY KEY,
    -- The DISPLAYED name, keeping the author's own capitalisation, and
    -- name_norm the address it was arbitrated on. Both, because a mention is
    -- resolved by the second and rendered from the first. A DM and a group
    -- carry neither: their identity is derived from the sorted participant
    -- handles, so there is no name for anybody to contend for.
    name           TEXT    NOT NULL DEFAULT '',
    name_norm      TEXT    NOT NULL DEFAULT '',
    kind           TEXT    NOT NULL,
    private        INTEGER NOT NULL DEFAULT 0,
    topic          TEXT    NOT NULL DEFAULT '',
    purpose        TEXT    NOT NULL DEFAULT '',
    -- The unit whose room this is, empty for a channel nobody's org chart
    -- names. It is IDENTITY, never a read scope and never a credential — the
    -- same rule a unit's project key and space key already carry.
    unit           TEXT    NOT NULL DEFAULT '',
    -- NULL means "inherit the company default"; 0 means FOR EVER; a number is
    -- a horizon in days. Three settings, so a nullable column rather than an
    -- integer whose zero would have to mean two of them.
    retention_days INTEGER,
    message_seq    INTEGER NOT NULL DEFAULT 0,
    created_at     INTEGER NOT NULL,
    created_by     TEXT    NOT NULL DEFAULT '',
    created_by_kind TEXT   NOT NULL DEFAULT '',
    archived_at    INTEGER,
    version        INTEGER NOT NULL,
    -- The highest record that wrote this row FROM ANOTHER SUBJECT — a rename,
    -- which arbitrates on the new name rather than on the channel. Stamped
    -- instead of `version` so the row's broker expectation still matches its
    -- own subject's last message and cannot be poisoned into permanent
    -- unwritability; a read barrier compares MAX of the two.
    scoped_through INTEGER NOT NULL DEFAULT 0,
    document       BLOB    NOT NULL
);

-- The channel list a person's rail renders, newest activity first is computed
-- from the members' side, so this one is the company-wide listing.
CREATE INDEX chat_channels_kind_idx ON chat_channels (kind, name_norm);                    -- the channel directory
-- Resolve a name to a channel. On the NORMALISED name: a LOWER(name) predicate
-- could not use the index above, so every mention would be a scan.
CREATE INDEX chat_channels_address_idx ON chat_channels (name_norm);                       -- resolve a room by name
-- A unit's own room, for the epoch reconcile that creates and maintains it.
CREATE INDEX chat_channels_unit_idx ON chat_channels (unit) WHERE unit <> '';              -- the org-derived rooms
-- Archived rooms, which every default listing excludes. Partial, because the
-- column is NULL on almost every row.
CREATE INDEX chat_channels_archived_idx ON chat_channels (archived_at) WHERE archived_at IS NOT NULL; -- the archive filter

-- chat_channel_names — the address claim, and the row `PatternCreate`
-- arbitrates against.
--
-- A SEPARATE TABLE from the channel, for the reason the wiki's titles are: the
-- name is what two writers contend for and the channel id is what everything
-- else references, so a rename moves a row here and leaves every message's
-- channel_id alone. The token is the first 16 bytes of SHA-256 over the
-- normalised name, because a name is prose — it carries spaces, dots and
-- wildcards — and a subject is a broker path.
CREATE TABLE chat_channel_names (
    name_token TEXT    NOT NULL PRIMARY KEY,
    name_norm  TEXT    NOT NULL,
    channel_id TEXT    NOT NULL,
    claimed_at INTEGER NOT NULL,
    version    INTEGER NOT NULL
);
CREATE INDEX chat_channel_names_channel_idx ON chat_channel_names (channel_id);            -- a channel's own claims

-- chat_members — who is in a channel.
--
-- `source` is `org` for a member the unit reconcile added and `explicit` for
-- one a person invited. A unit room's membership is the ORG CHART'S ALONE --
-- join, leave and set-members are all refused there, because any of them would
-- be undone by the next apply with nothing to say so -- so every member of one
-- is `org` and every member of any other room is `explicit`, and the column is
-- derived from the room's kind rather than carried per member. It becomes a
-- value a writer must state on the day a guest can be invited into a unit
-- room; nothing in this build expresses that, and the refusals are what keep
-- it true.
--
-- `follow_all` is the whole of the fan-out policy. A member with it set is
-- woken by every message in the room; a member without it is woken only when
-- the message names them or their thread. The engine sets it for a unit's own
-- agent seats in that unit's room and for nobody else, because a delivery is a
-- two-phase turn against a node's whole concurrency budget and a busy room of
-- passive agents is a company that does nothing but read chat.
CREATE TABLE chat_members (
    channel_id TEXT    NOT NULL,
    handle     TEXT    NOT NULL,
    role       TEXT    NOT NULL DEFAULT '',
    source     TEXT    NOT NULL DEFAULT '',
    follow_all INTEGER NOT NULL DEFAULT 0,
    joined_at  INTEGER NOT NULL,
    version    INTEGER NOT NULL,
    PRIMARY KEY (channel_id, handle)
);
-- One person's rooms — the rail's own query, and the visibility filter every
-- read of a private channel is gated by.
CREATE INDEX chat_members_handle_idx ON chat_members (handle, channel_id);                 -- one person's channels

-- ---------------------------------------------------------------------------
-- The transcript
-- ---------------------------------------------------------------------------

-- chat_messages — one row per message.
--
-- `body` is a column rather than only a document field because every read of a
-- channel reads it, and decoding a document per row to render a transcript is
-- a full scan wearing a column's name.
--
-- A DELETE IS A TOMBSTONE: the row survives with `deleted_at` stamped and the
-- body blanked, so a reply still resolves its root and a thread does not
-- silently lose its first message. The one destructive gesture is a compliance
-- ERASE, which removes the row and leaves a marker in `chat_deletions`.
CREATE TABLE chat_messages (
    id              TEXT    NOT NULL PRIMARY KEY,
    channel_id      TEXT    NOT NULL,
    -- Empty for a top-level message; otherwise the id of the message that
    -- started the thread. A thread is one level deep by construction: a reply
    -- to a reply carries the same root, which is what makes "this thread" a
    -- range scan rather than a recursive walk.
    thread_root     TEXT    NOT NULL DEFAULT '',
    author_handle   TEXT    NOT NULL,
    author_kind     TEXT    NOT NULL,
    body            TEXT    NOT NULL DEFAULT '',
    -- The external URLs the body referenced, as JSON, capped by the writer.
    -- The engine stores no file bytes at all, so this is the whole of what a
    -- message can carry besides its own prose.
    links           TEXT    NOT NULL DEFAULT '',
    -- CONTIGUOUS PER CHANNEL, minted by the applier from log order. See the
    -- header: this is why a message record is scoped to its channel.
    channel_seq     INTEGER NOT NULL,
    -- The BROKER'S OWN stored instant, which is what makes the retention
    -- prune's time range decidable identically on every node.
    created_at      INTEGER NOT NULL,
    edited_at       INTEGER,
    deleted_at      INTEGER,
    -- The position triple, so a cursor survives a reanchor: a composed version
    -- alone cannot say which stream and generation it came from.
    log_stream      TEXT    NOT NULL DEFAULT '',
    log_generation  INTEGER NOT NULL DEFAULT 0,
    log_seq         INTEGER NOT NULL DEFAULT 0,
    -- The COMPOSED (generation << 40) | sequence of the last record that
    -- changed this row. It is the row's version guard AND the keyword index's
    -- forward watermark: an edit and a tombstone both move it, which is what
    -- makes the index a strictly forward walk that never re-reads history.
    version         INTEGER NOT NULL,
    -- The search bucket, a stable hash of the message's own identity into 64.
    -- DELIBERATELY NOT INDEXED, which is the finding the lexical index's own
    -- migration recorded: a bucket range is a scan predicate, and an index
    -- over a table whose rows are all being read is a second copy of the row
    -- order plus a random access per row.
    shard           INTEGER NOT NULL DEFAULT 0,
    document        BLOB    NOT NULL
);

-- The transcript itself, newest first. On `channel_seq` rather than on
-- `created_at`: the sequence is contiguous and the instant is not unique, and
-- a browser pages backwards through exactly this order.
CREATE INDEX chat_messages_channel_idx ON chat_messages (channel_id, channel_seq DESC);    -- one channel's transcript
-- One thread, in order.
CREATE INDEX chat_messages_thread_idx ON chat_messages (channel_id, thread_root, channel_seq); -- one thread
-- "What have I said", which is the question an agent's own recall asks.
CREATE INDEX chat_messages_author_idx ON chat_messages (author_handle, version DESC);      -- one seat's own messages
-- The retention prune's range delete. Without it the delete is a scan of the
-- whole corpus inside the transaction holding this store's only writer.
CREATE INDEX chat_messages_prune_idx ON chat_messages (channel_id, created_at);            -- the retention prune
-- The keyword index's forward watermark. A global order rather than a
-- per-channel one, because the indexer walks the whole corpus once and then
-- only ever forward.
CREATE INDEX chat_messages_watermark_idx ON chat_messages (version);                       -- the chat index's walk

-- chat_thread_participants — who has spoken in a thread.
--
-- DERIVED BY THE APPLIER from the messages themselves, not authored by the
-- writer: participation is a fact about rows rather than a claim a record can
-- make, and deriving it is what removes the vendor-shaped machinery where a
-- backend's echo of the bot's own post was the only signal that a seat had
-- joined a conversation.
--
-- It is NOT a follow. A participant is woken by a reply as `reply`, which does
-- not oblige an answer; a follower is woken as `follow`. Two tables because
-- they answer two questions and collapsing them would make every remark in a
-- thread a standing subscription to it.
CREATE TABLE chat_thread_participants (
    channel_id      TEXT    NOT NULL,
    thread_root     TEXT    NOT NULL,
    handle          TEXT    NOT NULL,
    first_spoken_at INTEGER NOT NULL,
    -- The root message's own `created_at`, carried so the prune's range delete
    -- reaches these rows on an index rather than through a join. See the
    -- header.
    root_at         INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (channel_id, thread_root, handle)
);
CREATE INDEX chat_thread_participants_prune_idx ON chat_thread_participants (channel_id, root_at); -- the retention prune

-- chat_follows — a deliberate subscription to one thread.
--
-- REPLICATED, which is what makes a native follow survive a seat moving node.
-- The vendor backends' follows live in coordination for the same reason from
-- the other direction: the node that wins an inbound delivery is rarely the
-- node that wrote the follow.
CREATE TABLE chat_follows (
    channel_id  TEXT    NOT NULL,
    thread_root TEXT    NOT NULL,
    handle      TEXT    NOT NULL,
    reason      TEXT    NOT NULL DEFAULT '',
    at          INTEGER NOT NULL,
    root_at     INTEGER NOT NULL DEFAULT 0,
    version     INTEGER NOT NULL,
    PRIMARY KEY (channel_id, thread_root, handle)
);
CREATE INDEX chat_follows_handle_idx ON chat_follows (handle, channel_id);                 -- one seat's followed threads
CREATE INDEX chat_follows_prune_idx ON chat_follows (channel_id, root_at);                 -- the retention prune

-- chat_reactions — an emoji on a message, by one handle.
--
-- A REACTION NEVER WAKES ANYBODY and never discharges a reply obligation. It
-- carries no text a triage prompt could classify, and if it counted as a
-- delivery a seat could answer every addressed message with a thumb.
CREATE TABLE chat_reactions (
    message_id TEXT    NOT NULL,
    emoji      TEXT    NOT NULL,
    handle     TEXT    NOT NULL,
    at         INTEGER NOT NULL,
    -- The message's own channel and instant, for the prune's range delete.
    channel_id TEXT    NOT NULL DEFAULT '',
    message_at INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (message_id, emoji, handle)
);
CREATE INDEX chat_reactions_message_idx ON chat_reactions (message_id);                    -- a message's reactions
CREATE INDEX chat_reactions_prune_idx ON chat_reactions (channel_id, message_at);          -- the retention prune

-- chat_mentions — one row per (message, mentioned handle).
--
-- THIS IS A PERSON'S @-MENTION FEED. There is no second mailbox table and no
-- per-viewer notification row: a mention is a fact about the message, the
-- feed is a range over this index, and an unread count is a counted range
-- above a cursor that lives in coordination rather than here.
CREATE TABLE chat_mentions (
    message_id TEXT    NOT NULL,
    handle     TEXT    NOT NULL,
    -- The composed position of the message, so the feed orders and pages the
    -- same way the transcript does.
    version    INTEGER NOT NULL,
    channel_id TEXT    NOT NULL DEFAULT '',
    message_at INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (message_id, handle)
);
CREATE INDEX chat_mentions_handle_idx ON chat_mentions (handle, version DESC);             -- one person's mention feed
CREATE INDEX chat_mentions_prune_idx ON chat_mentions (channel_id, message_at);            -- the retention prune

-- chat_history — what changed in a channel, for the activity surfaces.
--
-- Channel operations and message MUTATIONS only. A plain post writes no
-- history row: the message row IS the post's own record, and a second row per
-- message would double the highest-volume table in the engine to say a thing
-- the first one already says.
CREATE TABLE chat_history (
    id             TEXT    NOT NULL PRIMARY KEY,
    channel_id     TEXT    NOT NULL,
    message_id     TEXT    NOT NULL DEFAULT '',
    kind           TEXT    NOT NULL,
    actor          TEXT    NOT NULL DEFAULT '',
    actor_kind     TEXT    NOT NULL DEFAULT '',
    excerpt        TEXT    NOT NULL DEFAULT '',
    -- The broker's own instant, so two nodes order one channel's history
    -- identically.
    broker_at      INTEGER NOT NULL,
    log_stream     TEXT    NOT NULL DEFAULT '',
    log_generation INTEGER NOT NULL DEFAULT 0,
    log_seq        INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX chat_history_channel_idx ON chat_history (channel_id, broker_at DESC);        -- one channel's activity
CREATE INDEX chat_history_broker_idx ON chat_history (broker_at);                          -- the company-wide digest and the prune

-- chat_deletions — the compliance erase's marker, and the applier's own gate.
--
-- An erase is the one gesture that destroys a row. The marker is what stops a
-- redelivered or replayed post from RESURRECTING what the company destroyed:
-- the applier consults it before writing a message and refuses every record
-- but the one that wrote the marker, identified by its operation id.
CREATE TABLE chat_deletions (
    message_id TEXT    NOT NULL PRIMARY KEY,
    channel_id TEXT    NOT NULL DEFAULT '',
    erased_at  INTEGER NOT NULL,
    op_id      TEXT    NOT NULL DEFAULT '',
    by         TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX chat_deletions_channel_idx ON chat_deletions (channel_id, erased_at);         -- an operator's own audit

-- ---------------------------------------------------------------------------
-- The framework's per-domain tables
-- ---------------------------------------------------------------------------

-- chat_evictions — a node's removal from THIS log.
--
-- Its own table rather than another domain's, because an eviction fences
-- records above a COMPOSED position and positions on different streams name
-- different number spaces.
CREATE TABLE chat_evictions (
    node_id             TEXT    NOT NULL PRIMARY KEY,
    at                  INTEGER NOT NULL,
    by                  TEXT    NOT NULL DEFAULT '',
    from_position       INTEGER NOT NULL,
    -- NULL until a readmission, which is an INVERSE COMMIT rather than a
    -- delete: the whole history survives a replay, and a node evicted,
    -- readmitted and evicted again reads correctly rather than as one long
    -- absence.
    readmitted_position INTEGER,
    version             INTEGER NOT NULL
);

-- chat_log_generations — every reanchor's audit row.
CREATE TABLE chat_log_generations (
    generation            INTEGER NOT NULL PRIMARY KEY,
    at                    INTEGER NOT NULL,
    by                    TEXT    NOT NULL DEFAULT '',
    new_stream_created_at INTEGER NOT NULL DEFAULT 0,
    prev_last_seq_seen    INTEGER NOT NULL DEFAULT 0,
    record_id             TEXT    NOT NULL DEFAULT ''
);

-- ---------------------------------------------------------------------------
-- The machinery, in the exact shape 0001's header documents
-- ---------------------------------------------------------------------------

-- chat_ops — which operations this node has already applied.
--
-- A DONOR'S OPERATION LEDGER IS THE SHARPEST THING A SNAPSHOT MUST SCRUB: an
-- adopted peer's ops table would let this node resolve its own ambiguous
-- publish against somebody else's history, which is a write reported as landed
-- that never happened.
--
-- Swept on a horizon this domain declares for itself rather than on the
-- framework's default, because the ledger's size is a function of the domain's
-- own commit rate and chat's is an order of magnitude above the tracker's.
CREATE TABLE chat_ops (
    op_id      TEXT    NOT NULL PRIMARY KEY,
    subject    TEXT    NOT NULL,
    position   INTEGER NOT NULL,
    applied_at INTEGER NOT NULL
);
CREATE INDEX chat_ops_swept_idx ON chat_ops (applied_at);                                  -- the ops retention sweep

-- chat_log_deferred — a record this build could not decode, retained byte for
-- byte at its original position.
CREATE TABLE chat_log_deferred (
    position     INTEGER NOT NULL PRIMARY KEY,
    subject      TEXT    NOT NULL,
    subject_kind TEXT    NOT NULL,
    subject_id   TEXT    NOT NULL,
    version      INTEGER NOT NULL,
    payload      BLOB    NOT NULL,
    stored_at    INTEGER NOT NULL
);
CREATE INDEX chat_log_deferred_subject_idx ON chat_log_deferred (subject_id, subject_kind); -- the applier's per-object deferral probe

-- One row per scope PATH of the deferred record, written in the SAME
-- transaction as its parent and the checkpoint advance.
--
-- A PLAIN ROWID TABLE: `WITHOUT ROWID` is refused by the pinned driver as an
-- experimental feature, so a migration carrying it would fail on every node.
CREATE TABLE chat_log_deferred_scope (
    position INTEGER NOT NULL,
    path     TEXT    NOT NULL,
    PRIMARY KEY (position, path)
);
CREATE INDEX chat_log_deferred_scope_idx ON chat_log_deferred_scope (path, position);      -- the read barrier's coverage probe
