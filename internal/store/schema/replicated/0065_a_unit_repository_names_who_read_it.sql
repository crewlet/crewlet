-- A unit repository names who read it.
--
-- usage_repo_read_days and usage_repo_reads are who read a `visibility: unit`
-- repository's contents, by node and company day: a git fetch through the code
-- host whose answer carried a pack, or a server-side read through a code tool
-- (internal/usage, kind `repo_read`, record version 2; ADR-0020's shape). The
-- serving node derives its day of them from its own event log
-- (`code_fetched`) and publishes ONE RECORD PER (node, day, repository)
-- carrying every reader, so the compacted stream's subjects — an index every
-- broker member holds in memory — are counted in repositories × nodes × days,
-- never in readers. An apply REPLACES the day's readers under the head row's
-- monotone version, so a republished day is harmless and a replayed older one
-- changes nothing. Kept usage.History (181 days) by the applier's own horizon:
-- DAY FIRST in both keys, so the horizon's `DELETE … WHERE day < ?` seeks them.
--
-- A READER IS ITS PRINCIPAL KIND, ITS PRINCIPAL, THE WAY IT READ AND THE SEAT
-- IT ACTED AS: a person reads through several credentials of theirs, each its
-- own reader naming their one seat, and a token the identity directory binds
-- to one seat and later to another is two readers — never one whose earlier
-- reads are filed under the seat it holds now.
--
-- Class: both tables DIVERGENT, like every row table of the usage domain:
-- written only by applying a committed record, they travel inside a snapshot,
-- and two nodes legitimately hold different days — one that joined last month
-- holds fewer — so neither is in the identity claim.
CREATE TABLE usage_repo_read_days (
    day            TEXT    NOT NULL,   -- period's day label on the company clock
    node           TEXT    NOT NULL,   -- the node that served the reads, and the record's one writer
    repo_id        TEXT    NOT NULL,
    repo_name      TEXT    NOT NULL,   -- as the day's newest read named it
    readers_elided INTEGER NOT NULL,   -- readers past usage.ReadersPerRepoDay
    version        INTEGER NOT NULL,   -- the packed position of the record that wrote the day
    PRIMARY KEY (day, node, repo_id)
);

CREATE TABLE usage_repo_reads (
    day            TEXT    NOT NULL,
    node           TEXT    NOT NULL,
    repo_id        TEXT    NOT NULL,
    principal_kind TEXT    NOT NULL,   -- operator | agent
    principal      TEXT    NOT NULL,   -- the credential (iam.ActorFor's name for it), or an agent's handle
    via            TEXT    NOT NULL,   -- git | tool
    seat           TEXT    NOT NULL,   -- the bound person, or the agent; '' for a token bound to nobody
    reads          INTEGER NOT NULL,
    bytes          INTEGER NOT NULL,
    last_at        INTEGER NOT NULL,
    PRIMARY KEY (day, node, repo_id, principal_kind, principal, via, seat)
);

-- "Who read it" over a range of days: grouped by reader in index order, so a
-- page of readers seeks where the page before it ended and stops at its own
-- end; covering, so the read never visits the table.
-- TestEveryPagedStatementSeeksItsKeyAndStreams holds the plan.
CREATE INDEX usage_repo_reads_repo_idx ON usage_repo_reads
    (repo_id, principal_kind, principal, via, seat, day, reads, bytes, last_at);

-- And how many readers the per-day cap dropped over that range: a seek of the
-- repository's own days on every node, rather than of every repository's.
CREATE INDEX usage_repo_read_days_repo_idx ON usage_repo_read_days (repo_id, day, readers_elided);
