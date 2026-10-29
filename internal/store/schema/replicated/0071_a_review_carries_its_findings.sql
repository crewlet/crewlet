-- A review carries its findings.
--
-- A review judges one revision of a change: a verdict — approve, request
-- changes or comment — a summary, and the FINDINGS it raises, each a thing it
-- says about lines of the change, with how much it weighs. It is written by ONE
-- record on the change's own subject (0070), the review and all its findings
-- together, by the repository's committing data node; an answer to findings is
-- a record of its own there too. A review is a code-domain object and never a
-- decision of the tracker's: what a merge will count is its verdict and its
-- findings, read from these rows.
--
-- code_reviews is one row a review. revision is the change's revision it
-- judges, reviewer and reviewer_kind who reviewed — a seat, or a person with
-- the credential they used (operator_id) — verdict what it says, findings how
-- many it raised, turn_id and model the turn and the model that reviewed, and
-- position and at its record's packed position and broker instant.
-- summary_bytes is its summary's width, which a purge part's cut reads off the
-- rows it deletes. THE SUMMARY IS DECLARED LAST, as a comment's body is (0070).
--
-- code_findings is one row a finding, NARROW: where it points — the path, the
-- side of the revision's diff, its lines, the commit and the blob they were
-- read in — the review and the revision it was raised on, its severity and
-- category, and its STATE: open; resolved, wont_fix or disputed, an author's
-- answers; dismissed, by its raiser, a maintainer or a person; superseded, by
-- its raiser's review of a later revision that does not carry it. The first
-- four STAND: a standing blocker holds its change back whatever its authors
-- answered (S40), and only a dismissal or that later review clears it.
-- resolution_note, resolved_by, resolved_by_kind, resolved_commit and
-- resolved_position are its last answer — a supersession among them, which
-- is its raiser's and names no note and no commit. Its TEXT is a row of its
-- own: a finding's state is rewritten by every answer and by every later
-- review of its raiser's, which supersedes up to a thousand of them in one
-- record, and a row is read and rewritten whole, overflow pages and all
-- (0066), so a title, a body of up to 4 KiB and a suggestion of up to 16 KiB
-- kept on it would be rewritten with every one.
--
-- code_finding_texts is a finding's title, body and suggestion (NULL for none),
-- keyed by its change and its id, so a purge reaches a change's texts by the
-- change alone and a change's findings are read in the order of their ids by
-- this key; bytes is what they weigh together.
--
-- code_conversations gains a review's counters: reviews; open_findings, the
-- findings none of whose authors has answered; open_blockers, the STANDING
-- blockers; and changes_requested, how many reviewers' standing verdict asks
-- for changes. Every record that moves one moves it, and its text_bytes counts
-- a review's summary and its findings' texts beside the comments.
--
-- code_participants gains each reviewer's STANDING VERDICT on the change:
-- verdict, the latest of their reviews that judges — approve or
-- request_changes; '' for a participant who has given none — and
-- verdict_revision and verdict_review, the revision it judged and the review
-- that gave it. A review that comments judges nothing and leaves it as it
-- was. Kept on the one narrow row a reviewer has on the change, so a review's
-- apply moves changes_requested by what the reviewer's verdict was before it
-- in one seek of the key, never by a walk of their reviews past every one
-- that only commented.
--
-- code_comments gains finding_id: a comment that replies to a finding, whose
-- thread the finding names — read beside the body in a timeline, never on a
-- decide's path, so it is added after it rather than the table made again.
--
-- # The indexes
--
-- code_reviews_change_idx is a change's reviews in the order they were given:
-- what its timeline reads, and the order a purge part's cut takes them in.
-- code_findings_raiser_idx is a change's findings by raiser and state: a
-- raiser's standing findings, which a review supersedes and its decide counts,
-- and a purge part's cut of a change's findings — which ends in the finding's
-- id because a cut deletes the first n of an order every node must agree on,
-- and a raiser's findings of one revision share every other column of it.
-- code_finding_texts needs none of its own: its key leads with the change.
--
-- Class: code_reviews, code_findings and code_finding_texts are reproducible:
-- written only by the applier, from the records on a change's own subject, and
-- removed only by its repository's purge, before the change's row.
-- code_conversations, code_participants and code_comments stay reproducible:
-- their new columns are written only by the applier, from the same records.
CREATE TABLE code_reviews (
    id            TEXT    NOT NULL PRIMARY KEY,
    change_id     TEXT    NOT NULL,
    revision      INTEGER NOT NULL,
    reviewer      TEXT    NOT NULL,
    reviewer_kind TEXT    NOT NULL,
    operator_id   TEXT    NOT NULL DEFAULT '',
    verdict       TEXT    NOT NULL,
    findings      INTEGER NOT NULL,
    turn_id       TEXT    NOT NULL DEFAULT '',
    model         TEXT    NOT NULL DEFAULT '',
    summary_bytes INTEGER NOT NULL,
    position      INTEGER NOT NULL,
    at            INTEGER NOT NULL,
    summary       TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX code_reviews_change_idx ON code_reviews (change_id, position);    -- a change's reviews in the order given: its timeline, and a purge part's cut of them

CREATE TABLE code_findings (
    id                TEXT    NOT NULL PRIMARY KEY,
    change_id         TEXT    NOT NULL,
    review_id         TEXT    NOT NULL,
    raiser            TEXT    NOT NULL,
    raiser_kind       TEXT    NOT NULL,
    revision          INTEGER NOT NULL,
    commit_oid        TEXT    NOT NULL,
    path              TEXT    NOT NULL,
    blob_oid          TEXT    NOT NULL,
    side              TEXT    NOT NULL,
    line_start        INTEGER NOT NULL,
    line_end          INTEGER NOT NULL,
    severity          TEXT    NOT NULL,
    category          TEXT    NOT NULL,
    state             TEXT    NOT NULL,
    resolution_note   TEXT    NOT NULL DEFAULT '',
    resolved_by       TEXT    NOT NULL DEFAULT '',
    resolved_by_kind  TEXT    NOT NULL DEFAULT '',
    resolved_commit   TEXT    NOT NULL DEFAULT '',
    resolved_position INTEGER NOT NULL DEFAULT 0,
    position          INTEGER NOT NULL
);

CREATE INDEX code_findings_raiser_idx ON code_findings (change_id, raiser, state, revision, id);    -- a raiser's standing findings on a change, which a review supersedes, and a purge part's cut of a change's findings

CREATE TABLE code_finding_texts (
    change_id  TEXT    NOT NULL,
    finding_id TEXT    NOT NULL,
    bytes      INTEGER NOT NULL,
    title      TEXT    NOT NULL,
    body       TEXT    NOT NULL DEFAULT '',
    suggestion TEXT,
    PRIMARY KEY (change_id, finding_id)
);

ALTER TABLE code_conversations ADD COLUMN reviews INTEGER NOT NULL DEFAULT 0;
ALTER TABLE code_conversations ADD COLUMN open_findings INTEGER NOT NULL DEFAULT 0;
ALTER TABLE code_conversations ADD COLUMN open_blockers INTEGER NOT NULL DEFAULT 0;
ALTER TABLE code_conversations ADD COLUMN changes_requested INTEGER NOT NULL DEFAULT 0;
ALTER TABLE code_participants ADD COLUMN verdict TEXT NOT NULL DEFAULT '';
ALTER TABLE code_participants ADD COLUMN verdict_revision INTEGER NOT NULL DEFAULT 0;
ALTER TABLE code_participants ADD COLUMN verdict_review TEXT NOT NULL DEFAULT '';
ALTER TABLE code_comments ADD COLUMN finding_id TEXT NOT NULL DEFAULT '';
