-- A change remembers whether it was ready.
--
-- Whether a live change may merge is a judgment of the rows one snapshot holds
-- and the chart one epoch holds (code.ReadinessOf): its draft flag, its head
-- revision's facts, a counted approval, its standing blockers and its head's
-- mergeability. Every read of a change judges it again from its own snapshot,
-- so no read ever trusts a stored answer. But two things read the answer
-- WITHOUT judging every live change again: a wake that a change became ready
-- to merge, which a record must carry, and the listing of the changes waiting
-- on a person, which must seek them rather than judge every live change of
-- the company on every page. So the answer is STORED on the record that
-- judges it — a `mergeability` record on the change's own subject (0100),
-- whose decide judges the readiness from its own snapshot with the merge the
-- record carries and stamps it on the record, which the applier writes here.
--
-- code_mergeability gains that answer and what it was judged at:
--   - ready is whether every precondition was met, and waits_person whether
--     the first unmet one waits on persons alone — a person's approval, a
--     blocker a person raised and an author answered — which the listing of
--     the changes waiting on a person seeks (needs=person).
--   - change_version and readiness_version are the versions of the change's
--     own row (code_changes.version, moved by every record on the
--     repository's subject that moves the change: a revision, an edit, a
--     draft flag, a retarget, a close, a reopen) and of its conversation's
--     readiness inputs (code_conversations.readiness_version, below) the
--     answer was judged at. A change whose row or conversation has moved past
--     them is DUE A REFRESH: its home records a `mergeability` record that
--     merges nothing, judging the readiness again over the merge the row
--     holds (code.ReadChangesDue's third term), so the stored answer follows
--     a review a minute later at the most, the home's backstop.
--
-- code_conversations gains readiness_version: a count of the records on a
-- change's subject that move what its readiness reads — an ask for review
-- (whom the approval waits on), a review (its verdicts and blockers), an
-- answer to findings (its blockers) and a revision's facts (the patch ids an
-- approval carries by). A comment, a thread's resolution and a mergeability
-- move version and not it: none is a readiness input, and a mergeability that
-- moved it would make its own change due a refresh again, for ever.
--
-- CODE_MERGEABILITY IS CREATED AGAIN, EMPTY, RATHER THAN ALTERED: its
-- conflicted paths are its wide column, declared last so a row read for a
-- column before it stops at its leaf page (0089's measurement), and every
-- column ALTER TABLE adds goes after it — where the due listing, which reads
-- change_version and readiness_version of every live change on every home
-- pass, would read each row's whole overflow chain to reach them. No release
-- carries 0100: the code domain is on no build of main and in no image or tag
-- (the domain's stated condition, code.RecordVersion), so no store anywhere
-- holds a row of it outside a development branch, and a change whose row is
-- gone is due its merge again at its home's next pass — a function of two
-- commits, recorded again. 0100 IS NOT EDITED: `schema_migrations` keys on the
-- filename, so a store that applied it would never see the change. NO DEFAULTS
-- on the answer's columns: a writer states every one, so no row reads as
-- unready, or as waiting on nobody, by omission.
--
-- # The indexes
--
-- code_mergeability_waits_idx is the changes whose stored answer waits on a
-- person, newest change first (the change's id is a UUIDv7 minted at its
-- open): the listing of what waits on a person (needs=person). PARTIAL on
-- waits_person = 1, so it holds only those — the few a company's persons have
-- in front of them — and a write of a row that waits on nobody costs it
-- nothing.
--
-- code_findings_blockers_idx is a change's STANDING BLOCKERS, oldest raised
-- first: the blockers a change's readiness names with who may clear each.
-- PARTIAL on the blockers that stand — open, resolved, wont_fix or disputed,
-- since only a dismissal and a supersession clear one (S40) — so a readiness
-- seeks exactly them, never the change's every finding of every revision,
-- and a finding of another severity, or a blocker cleared, costs it nothing.
-- code_findings_raiser_idx (0096) leads with the raiser, so it reads every
-- raiser's findings to find one change's blockers.
--
-- Class: code_mergeability stays reproducible: written only by the applier,
-- from the records on a change's own subject, and removed only by its
-- repository's purge, a change's mergeability before its row. code_findings
-- and code_conversations stay reproducible: the new index is the applier's
-- rows, and the new column is written only by the applier, from the same
-- records.
DROP TABLE code_mergeability;

CREATE TABLE code_mergeability (
    change_id         TEXT    NOT NULL PRIMARY KEY,
    revision          INTEGER NOT NULL,
    target_ref        TEXT    NOT NULL,
    target_oid        TEXT    NOT NULL,
    target_position   INTEGER NOT NULL,
    state             TEXT    NOT NULL,
    conflicts_count   INTEGER NOT NULL DEFAULT 0,
    conflicts_omitted INTEGER NOT NULL DEFAULT 0,
    node              TEXT    NOT NULL,
    bytes             INTEGER NOT NULL DEFAULT 0,
    position          INTEGER NOT NULL,
    at                INTEGER NOT NULL,
    change_version    INTEGER NOT NULL,
    readiness_version INTEGER NOT NULL,
    ready             INTEGER NOT NULL,
    waits_person      INTEGER NOT NULL,
    conflicts         TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX code_mergeability_waits_idx ON code_mergeability (waits_person, change_id) WHERE waits_person = 1;    -- the changes waiting on a person, newest first: needs=person

ALTER TABLE code_conversations ADD COLUMN readiness_version INTEGER NOT NULL DEFAULT 0;

CREATE INDEX code_findings_blockers_idx ON code_findings (change_id, position, id) WHERE severity = 'blocker' AND state IN ('open', 'resolved', 'wont_fix', 'disputed');    -- a change's standing blockers, oldest raised first: its readiness
