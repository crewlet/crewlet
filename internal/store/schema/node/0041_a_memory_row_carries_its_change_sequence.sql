-- A seat's append-only memory rows carry a CHANGE SEQUENCE, and the memory
-- changelog's watermark is over it rather than over the rowid.
--
-- WHAT WAS WRONG. internal/learning/memsync carries a seat's diary, episodes
-- and skill versions incrementally: each cycle exports the rows past the
-- highest one it published last time, and that watermark was the ROWID. These
-- three tables are keyed on TEXT with no AUTOINCREMENT, so a new row takes
-- max(rowid) + 1 — and once the newest row is deleted (the diary's expiry and
-- its 500-note trim, the lifecycle's episode sweeps, a skill's pruned
-- history), the next insert takes that rowid AGAIN (measured on the pinned
-- driver: insert a, b; delete b; the next insert is rowid 2). The watermark
-- had already passed it, so the row was never published while this node held
-- the seat, the release's final flush skipped it too, and the next holder
-- hydrated without it: a note, a turn or a skill version lost to the fleet.
-- The diary's vector fill made it likelier — it re-filed a filled note to
-- max(rowid) + 1, newest-first, so the last note it moved to the top was the
-- seat's oldest unretrieved one, exactly the note the trim evicts first.
--
-- WHAT REPLACES IT. One counter for the node, `memory_change_sequence`, that
-- only ever increments and is never derived from the rows that are left, and a
-- `change_seq` column on each of the three tables stamped from it. The stamp
-- is a TRIGGER, not a step each writer performs, for the reason memsync's
-- registry gives for reading the tables at all: memory is written from many
-- places, and a scheme every writer has to remember is one a writer forgets.
-- An insert takes a fresh value whoever wrote it — a turn, the lifecycle, a
-- hydration replaying a peer's rows — and so does the one in-place change an
-- append-only row carries, a vector set on a row written without one (the
-- holder's fill of the diary and of the episodes, and an import that takes a
-- carried vector over a stored one). Nothing else an append-only row takes in
-- place travels — a diary note's retrieval counter, an episode's
-- consolidation marker — so nothing else stamps it. Each trigger is one row's
-- statement and runs inside it, so the stamp commits or rolls back with the
-- change it describes. The store is single-writer (internal/store, begin.go),
-- so stamps are taken in commit order and a reader that sees a value has seen
-- every smaller one.
--
-- conversation_sessions, the fourth table carried by watermark, is not here:
-- its rowid is its `id INTEGER PRIMARY KEY AUTOINCREMENT`, which is never
-- reused (measured: delete the newest of three and the next insert is 4), so
-- the watermark over it was already sound.
--
-- EXISTING ROWS KEEP THEIR ROWID AS THEIR SEQUENCE, and the counter starts at
-- the largest rowid any of the three holds, so every stamp taken after this
-- migration is larger than every value before it. A watermark is a value of
-- the column it is over, so a mark taken over the rowids before this file ran
-- is still a valid mark over change_seq after it: everything at or below it
-- was carried, and every change after it is above it. (Marks live in memory
-- and this runs at Open, before any exists — but the numbering is what makes
-- that true, not the timing.)
--
-- WHO HAS TO AGREE ON IT: this node alone. The sequence orders this node's own
-- writes for this node's own exports and means nothing on a peer, so memsync
-- does not carry the column: a hydrated row is stamped by the receiving node's
-- trigger, from its own counter. The wire is therefore unchanged in both
-- directions — an older peer on the same changelog publishes and reads exactly
-- the rows it always did — and an older build opening this file ignores the
-- column, its inserts and its fills still stamped by the triggers.
CREATE TABLE memory_change_sequence (
    singleton INTEGER NOT NULL PRIMARY KEY CHECK (singleton = 1),
    last      INTEGER NOT NULL
);

ALTER TABLE agent_diary ADD COLUMN change_seq INTEGER NOT NULL DEFAULT 0;
ALTER TABLE episodes ADD COLUMN change_seq INTEGER NOT NULL DEFAULT 0;
ALTER TABLE synthesized_skill_versions ADD COLUMN change_seq INTEGER NOT NULL DEFAULT 0;

UPDATE agent_diary SET change_seq = rowid;
UPDATE episodes SET change_seq = rowid;
UPDATE synthesized_skill_versions SET change_seq = rowid;

INSERT INTO memory_change_sequence (singleton, last) VALUES (1, max(
    (SELECT COALESCE(max(rowid), 0) FROM agent_diary),
    (SELECT COALESCE(max(rowid), 0) FROM episodes),
    (SELECT COALESCE(max(rowid), 0) FROM synthesized_skill_versions)));

CREATE TRIGGER agent_diary_change_on_insert AFTER INSERT ON agent_diary
BEGIN
    UPDATE memory_change_sequence SET last = last + 1;
    UPDATE agent_diary SET change_seq = (SELECT last FROM memory_change_sequence)
     WHERE rowid = NEW.rowid;
END;

CREATE TRIGGER agent_diary_change_on_vector
AFTER UPDATE OF embedding, embedding_model ON agent_diary
BEGIN
    UPDATE memory_change_sequence SET last = last + 1;
    UPDATE agent_diary SET change_seq = (SELECT last FROM memory_change_sequence)
     WHERE rowid = NEW.rowid;
END;

CREATE TRIGGER episodes_change_on_insert AFTER INSERT ON episodes
BEGIN
    UPDATE memory_change_sequence SET last = last + 1;
    UPDATE episodes SET change_seq = (SELECT last FROM memory_change_sequence)
     WHERE rowid = NEW.rowid;
END;

CREATE TRIGGER episodes_change_on_vector
AFTER UPDATE OF embedding, embedding_model ON episodes
BEGIN
    UPDATE memory_change_sequence SET last = last + 1;
    UPDATE episodes SET change_seq = (SELECT last FROM memory_change_sequence)
     WHERE rowid = NEW.rowid;
END;

CREATE TRIGGER synthesized_skill_versions_change_on_insert
AFTER INSERT ON synthesized_skill_versions
BEGIN
    UPDATE memory_change_sequence SET last = last + 1;
    UPDATE synthesized_skill_versions
       SET change_seq = (SELECT last FROM memory_change_sequence)
     WHERE rowid = NEW.rowid;
END;

-- THE EXPORT IS A RANGE SEEK: one seat's rows past its mark, in sequence
-- order, every memory sync cycle for every seat this node holds.
CREATE INDEX agent_diary_agent_change_idx ON agent_diary (agent_id, change_seq);
CREATE INDEX episodes_agent_change_idx ON episodes (agent_handle, change_seq);
CREATE INDEX synthesized_skill_versions_agent_change_idx
    ON synthesized_skill_versions (agent_handle, change_seq);
