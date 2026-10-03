-- The org chart's durable state: the read side of the chart log, and the state
-- log's FOURTH domain.
--
-- Every table here is REBUILT FROM THE LOG rather than written by a caller.
-- The applier is the only writer, its transaction carries the checkpoint, and
-- an identity claim compares these rows byte for byte across the fleet — so
-- anything a node decides for itself belongs in the node's own estate instead.
--
-- # What this domain is, and what it replaces
--
-- Until now the company's org chart was a nested object inside the Tier B
-- company document, rewritten whole by whoever wrote the document last. That
-- settles one question badly and three not at all: a write is the WHOLE
-- document, so two people editing two teams collide on a revision neither
-- touched and the loser's change is overwritten rather than refused; there is
-- no per-object arbitration, so the unit of contention is the company rather
-- than the seat; and a change leaves NO RECORD, because a document revision
-- says what the chart became and never what happened — which is exactly what a
-- reorganisation most needs an audit of.
--
-- # The one shape here that neither of the other strict domains has
--
-- STRUCTURE IS AN OBJECT IN ITS OWN RIGHT, and it is a column rather than a
-- table. A task belongs to a project and a page to a container, and in both
-- cases containment is a field on the thing contained. An org chart's
-- containment IS the thing — so `chart_units.parent_key` and
-- `chart_seats.unit_key` are written by a record on ONE subject for the whole
-- structure (`crewlet.chart.log.tree`), while each object's own content is
-- written by a record on its own. Two writers editing two seats never contend;
-- two writers reorganising anything always do, which is what makes a cycle
-- unreachable rather than merely detectable. internal/chart argues it in full.
--
-- # The three rules the schema enforces, and one it deliberately does not
--
-- NO FOREIGN KEY ANYWHERE. A cascade is a delete nobody committed: the applier
-- removes an object's edges in the same statement list, where the deletion is
-- part of the record's own effect and therefore identical on every node.
--
-- NO `UNIQUE` OUTSIDE A PRIMARY KEY. A constraint violation inside the apply
-- transaction aborts it deterministically on every node, which turns a rare
-- cosmetic anomaly into a fleet-wide stalled log. The key collision is the
-- worked example: it cannot happen, because a key IS the arbitration subject of
-- the record that claims it — but a restored `chart_units` beside newer rows
-- could produce one, and a unique index would wedge every node at once rather
-- than flagging it.
--
-- NO `COLLATE` ANYWHERE. The default TEXT collation is BINARY, so an ORDER BY
-- reproduces the Go comparison exactly. The pinned driver ACCEPTS
-- `COLLATE NOCASE`, which would silently collapse 'A' and 'a' — and a key's
-- folding is done in Go, once, by chart.NormalizeKey, so a collation here would
-- be a second answer to what one address is.
--
-- What it does NOT enforce is the object versions' monotonicity: that is the
-- applier's `WHERE excluded.version > version` guard, because it has to be a
-- SKIP rather than an error — a redelivered record is ordinary traffic.

-- ---------------------------------------------------------------------------
-- The object tables.
--
-- Each carries `version INTEGER NOT NULL` — the COMPOSED
-- (generation << 40) | stream sequence of the commit that last arbitrated this
-- object, in the same integer space as the domain's own checkpoint — and
-- `document`, the object's full encoded state INCLUDING fields this build does
-- not know, so a rolling upgrade's newer fields round-trip through a durable
-- row.
-- ---------------------------------------------------------------------------

-- chart_units — one unit: a department, a team, a squad, a pod.
CREATE TABLE chart_units (
    -- The ADDRESS, in chart.NormalizeKey's folded form: what a `manages:`
    -- entry, a root seat's `unit:` and every scope path this unit's records are
    -- filed under all name. The spelling a founder typed lives in the company
    -- document; `name` is what a screen renders.
    key              TEXT    NOT NULL PRIMARY KEY,
    -- The keys this unit USED to answer to, newest first, as a JSON array.
    --
    -- A COLUMN RATHER THAN A KEY DIRECTORY TABLE, and the reason is the size of
    -- the corpus: a company has tens or hundreds of units, not the hundreds of
    -- thousands of work items `tracker_task_keys` was built for. A former-key
    -- lookup here is a scan of a table a page of memory holds, so a child table
    -- and its index would be machinery bought for a read that is already free —
    -- and a list read WHOLE beside the row it belongs to is one fewer thing an
    -- apply has to keep in step.
    --
    -- IT MUST BE HERE, in the migration that creates the table. schema_migrations
    -- keys on the FILENAME, so a column added by editing this file later would
    -- silently never run on any database that had already applied it.
    former_keys_json TEXT    NOT NULL DEFAULT '[]',
    name             TEXT    NOT NULL DEFAULT '',
    type             TEXT    NOT NULL DEFAULT '',
    purpose          TEXT    NOT NULL DEFAULT '',
    channel          TEXT    NOT NULL DEFAULT '',
    -- The unit's tracker and knowledge identities. VENDOR-NEUTRAL: each names a
    -- native container or a vendor's, whichever backend the company runs.
    project          TEXT    NOT NULL DEFAULT '',
    space            TEXT    NOT NULL DEFAULT '',
    -- STRUCTURE, written only by a record on the tree's own subject. Empty is
    -- the org root, which is a real placement and not a missing one.
    parent_key       TEXT    NOT NULL DEFAULT '',
    -- The AUTHORED lead's handle, empty where the unit inherits one. The
    -- EFFECTIVE lead is a walk up parent_key and is deliberately not stored: a
    -- derived value written down is a second answer that goes stale the moment
    -- an ancestor's lead moves, recomputed by nothing, because the record that
    -- changed the ancestor never named this unit.
    lead             TEXT    NOT NULL DEFAULT '',
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL,
    version          INTEGER NOT NULL,
    -- The highest record that wrote this row FROM ANOTHER SUBJECT — a
    -- structural record that reparented it, which touches this unit's own path.
    -- It is stamped instead of `version` so the row's broker expectation still
    -- matches its subject's last message and cannot be poisoned into permanent
    -- unwritability; a read barrier compares MAX of the two.
    scoped_through   INTEGER NOT NULL DEFAULT 0,
    document         BLOB    NOT NULL
);
-- The tree walk: one unit's children, which every derivation over the chart
-- starts from (lead inheritance, unit-key expansion, the roster).
CREATE INDEX chart_units_parent_idx ON chart_units (parent_key, key);                      -- one unit's children
-- The inverse of `lead`: which units a seat leads. Authored on the unit and
-- read from the other end by the roster, the routing fallback and IsUnitLead.
CREATE INDEX chart_units_lead_idx ON chart_units (lead);                                   -- the units one seat leads

-- chart_seats — one seat, agent or human.
CREATE TABLE chart_seats (
    -- The handle, in folded form: the identity every other subsystem already
    -- addresses a seat by — the mailbox, the derived agent id, the external
    -- accounts, and every `lead:` and `manages:` entry in the document.
    handle               TEXT    NOT NULL PRIMARY KEY,
    -- The handles this seat used to answer to. See chart_units.former_keys_json
    -- for why this is a column. What a former handle keeps working is the
    -- REFERENCES somebody wrote down -- a `manages:` entry, a `lead:` -- for as
    -- long as the capped list holds them. The seat's own durable state needs
    -- none of it: its mailbox, lease, diary and schedule ledger key on the id
    -- derived from `origin_handle` in the document, which no rename moves.
    -- (A COMMENT ONLY. Nothing about the shape this file creates has changed,
    -- and nothing may: an applied migration is history.)
    former_keys_json     TEXT    NOT NULL DEFAULT '[]',
    -- agent or human. An agent seat has an inbox, a turn loop and a model
    -- chain; a human seat participates in the same hierarchy and is
    -- addressable only.
    kind                 TEXT    NOT NULL,
    name                 TEXT    NOT NULL DEFAULT '',
    -- The address as AUTHORED, which is what a person reads back out of the
    -- config.
    email                TEXT    NOT NULL DEFAULT '',
    -- And the form it is MATCHED on: lower-cased with the plus tag stripped,
    -- derived once by chart.NormalizeEmail at apply time.
    --
    -- ITS OWN COLUMN because inbound Jira and GitHub payloads identify people
    -- by address, and a company routinely subscribes its seats with a
    -- plus-addressed form so vendor mail is filterable — so the address on the
    -- record and the address in the payload are routinely different strings
    -- naming one person. The alternative is a LOWER(...) predicate over every
    -- row on every inbound webhook, which cannot use an index and has to
    -- re-derive the plus rule in SQL, in a dialect where the Go answer beside
    -- it would then be a second opinion.
    --
    -- IT MUST BE HERE, for former_keys_json's reason: an applied migration is
    -- history, not source.
    email_index          TEXT    NOT NULL DEFAULT '',
    backstory            TEXT    NOT NULL DEFAULT '',
    goal                 TEXT    NOT NULL DEFAULT '',
    -- A root-level seat's own tracker and knowledge identities. A seat inside a
    -- unit takes the unit's.
    project              TEXT    NOT NULL DEFAULT '',
    space                TEXT    NOT NULL DEFAULT '',
    -- STRUCTURE, written only by a record on the tree's own subject. Empty is
    -- the org root: an org-wide seat above every team.
    unit_key             TEXT    NOT NULL DEFAULT '',
    created_at           INTEGER NOT NULL,
    updated_at           INTEGER NOT NULL,
    version              INTEGER NOT NULL,
    scoped_through       INTEGER NOT NULL DEFAULT 0,
    document             BLOB    NOT NULL
);
-- A unit's own members, which is what a roster and every unit-scoped read walk.
CREATE INDEX chart_seats_unit_idx ON chart_seats (unit_key, handle);                       -- one unit's direct members
-- The address lookup an inbound webhook resolves through. Not partial: a seat
-- with no address stores '' and the predicate would make the index unusable for
-- the equality it exists for.
CREATE INDEX chart_seats_email_idx ON chart_seats (email_index);                           -- resolve a vendor payload's address to a seat

-- ---------------------------------------------------------------------------
-- The two AUTHORED EDGE tables.
--
-- An edge in an org chart has two ends and the document authors exactly one of
-- them: a seat states what it MANAGES, and a unit states who LEADS it. The
-- other end — who manages me, which units do I lead — is DERIVED, and is read
-- as often as the authored end. That is why each is a table with an index in
-- the unauthored direction rather than a collection inside its owner's
-- document: a collection cannot be indexed, and a query that decodes every
-- row's document to answer "who manages alice" is a full scan wearing an
-- index's name.
--
-- BOTH STORE WHAT WAS AUTHORED, including an entry that resolves to nothing. A
-- `manages:` naming a seat nobody has added yet is kept as written, for the
-- reason the organisation model gives: a chart is built in pieces and every
-- intermediate revision is applied by every node, so refusing a partly wired
-- chart would make that sequence impossible. Resolution happens where the tree
-- is read, and a dangling reference is reported there.
-- ---------------------------------------------------------------------------

-- chart_manages — one authored `manages:` entry.
--
-- The target is stored UNEXPANDED. A `manages:` entry naming a unit reaches
-- every seat in its subtree, and that expansion is a function of the tree at
-- the moment it is read — so storing the expansion would be a derived value
-- written down, stale from the next structural record onwards and recomputed by
-- nothing.
CREATE TABLE chart_manages (
    manager TEXT NOT NULL,
    -- The entry exactly as written: a seat handle, a unit key, or neither.
    target  TEXT NOT NULL,
    version INTEGER NOT NULL,
    PRIMARY KEY (manager, target)
);
-- The unauthored direction: who manages this handle or this unit key. Read by
-- the identity prompt, by escalation routing and by the org model's Manager.
CREATE INDEX chart_manages_target_idx ON chart_manages (target, manager);                  -- who manages one seat or unit

-- chart_leads — one unit's authored lead, as an edge rather than only the
-- column above.
--
-- THE COLUMN AND THE ROW ARE NOT A DUPLICATE, and the difference is what each
-- is read for. `chart_units.lead` is part of the unit's own row, read with it
-- and rewritten with it. This table is the EDGE SET, and it is what a walk
-- reads: lead inheritance means the effective lead of a unit is an ancestor's
-- authored row, so answering "who leads this team" is a walk over edges rather
-- than a read of one column — and answering "what does this seat lead" is the
-- same walk from the other end.
CREATE TABLE chart_leads (
    unit_key TEXT    NOT NULL PRIMARY KEY,
    handle   TEXT    NOT NULL,
    version  INTEGER NOT NULL
);
CREATE INDEX chart_leads_handle_idx ON chart_leads (handle, unit_key);                     -- the units one seat is authored to lead

-- ---------------------------------------------------------------------------
-- The history, which is what a card, a digest and an operator asking "when did
-- this team change hands" all render from.
-- ---------------------------------------------------------------------------

CREATE TABLE chart_history (
    id          TEXT    NOT NULL PRIMARY KEY,
    -- The object the change is about, as the pair every scope term and every
    -- notification uses: a kind and an id, never a composed string that would
    -- be parsed apart again at each of the places that read it.
    object_kind TEXT    NOT NULL,
    object_id   TEXT    NOT NULL,
    kind        TEXT    NOT NULL,
    actor       TEXT    NOT NULL DEFAULT '',
    actor_kind  TEXT    NOT NULL DEFAULT '',
    operator_id TEXT    NOT NULL DEFAULT '',
    -- The company configuration revision this change came from, when it came
    -- from one. Its absence is the answer to "which edit did this": nobody's —
    -- somebody did it by hand.
    revision    TEXT    NOT NULL DEFAULT '',
    summary     TEXT    NOT NULL DEFAULT '',
    turn_id     TEXT    NOT NULL DEFAULT '',
    quiet       INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL,
    -- broker_at is the RAW broker instant this entry was committed at, which is
    -- what makes the row byte-identical on every node rather than a clock each
    -- one read for itself.
    broker_at   INTEGER NOT NULL DEFAULT 0,
    version     INTEGER NOT NULL,
    document    BLOB    NOT NULL
);
CREATE INDEX chart_history_object_idx ON chart_history (object_kind, object_id, created_at DESC); -- one unit's or one seat's own history
CREATE INDEX chart_history_created_idx ON chart_history (created_at);                      -- the company-wide reorganisation feed

-- ---------------------------------------------------------------------------
-- The import ledger.
-- ---------------------------------------------------------------------------

-- chart_import_ledger — which company configuration revision produced which
-- position on this log.
--
-- KEYED ON THE REVISION, because re-activating an UNCHANGED revision is the
-- credential-rotation gesture and is therefore routine — see the control plane.
-- Without this row that gesture would rewrite every object in the chart and
-- wake everybody a second time; with it, every node reaches the same no-op the
-- same way, from a row rather than from a comparison each node makes for
-- itself.
--
-- IT IS ALSO THE OPERATOR SURFACE for the question a chart nobody expected
-- prompts: which revision is this company's structure actually running, and
-- when did it land.
CREATE TABLE chart_import_ledger (
    revision   TEXT    NOT NULL PRIMARY KEY,
    -- The composed (generation << 40) | sequence of the import record, so a
    -- position from before a reanchor is comparable and safely stale rather
    -- than plausible.
    position   INTEGER NOT NULL,
    at         INTEGER NOT NULL,
    by         TEXT    NOT NULL DEFAULT '',
    -- How many objects the import's own record placed. An OPERATOR SURFACE: an
    -- import that placed three objects where the previous one placed three
    -- hundred is a config edit somebody should look at.
    objects    INTEGER NOT NULL DEFAULT 0,
    record_id  TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX chart_import_ledger_position_idx ON chart_import_ledger (position);           -- the newest import, and the ledger's own sweep

-- ---------------------------------------------------------------------------
-- The log's own machinery: excluded from the identity claim, from the
-- completeness audit, and scrubbed out of every donated snapshot.
--
-- THE FRAMEWORK GENERATES THE STATEMENTS THAT WRITE THESE, so their shape is
-- `0001_the_state_log_lands.sql`'s and not this domain's to choose. Changing a
-- column here compiles, migrates, opens and serves every read, and fails the
-- first time a record this build cannot decode arrives — the rarest path in the
-- system and the one whose failure is a stalled log.
-- ---------------------------------------------------------------------------

-- chart_ops — contract 3's first layer: what this node's applier has written.
--
-- A DONOR'S OPERATION LEDGER IS THE SHARPEST THING A SNAPSHOT MUST SCRUB: an
-- adopted peer's ops table would let this node resolve its own ambiguous
-- publish against somebody else's history, which is a write reported as landed
-- that never happened.
--
-- THE FRAMEWORK'S FOUR COLUMNS AND NOTHING ELSE. A `kind` column and an index
-- over (kind, applied_at) would be a SECOND HORIZON — a claim that a structural
-- record's op id may be swept on a different schedule from a content record's —
-- and this domain declares ONE ops horizon. Two horizons on one ledger is a
-- retry that resolves against a history half of which has been deleted.
CREATE TABLE chart_ops (
    op_id      TEXT    NOT NULL PRIMARY KEY,
    subject    TEXT    NOT NULL,
    -- The COMPOSED (generation << 40) | sequence, so a position from before a
    -- reanchor is comparable and safely stale rather than plausible.
    position   INTEGER NOT NULL,
    applied_at INTEGER NOT NULL
);
-- The sweep is a range delete over the age, and a range delete ships its index:
-- without it a node returning from a month away scans the whole table on every
-- tick, and deletes its whole month in one statement holding the only writer.
CREATE INDEX chart_ops_swept_idx ON chart_ops (applied_at);                                -- the ops retention sweep

-- chart_log_deferred — a record this build could not decode, retained byte for
-- byte at its original position.
CREATE TABLE chart_log_deferred (
    position     INTEGER NOT NULL PRIMARY KEY,
    subject      TEXT    NOT NULL,
    subject_kind TEXT    NOT NULL,
    subject_id   TEXT    NOT NULL,
    -- The record version this build could not decode, which is the number an
    -- operator needs to pick a build.
    version      INTEGER NOT NULL,
    -- THE WHOLE MESSAGE, BYTE FOR BYTE. Lossless means the bytes: a build that
    -- can read this record must get exactly what its writer published, not what
    -- an intermediate build understood of it.
    payload      BLOB    NOT NULL,
    -- The broker's own instant, because a record reprocessed after an upgrade
    -- must contribute the instant it was COMMITTED rather than the instant it
    -- was reprocessed — and this row is the only other durable copy of it.
    stored_at    INTEGER NOT NULL
);
-- subject_id LEADS, because the per-object probe reads it ALONE: a seat's own
-- content record and the key claim that renames it share an id under different
-- kinds, and the dependency is on the OBJECT rather than on the kind.
CREATE INDEX chart_log_deferred_subject_idx ON chart_log_deferred (subject_id, subject_kind); -- the applier's per-object deferral probe

-- One row per scope PATH of the deferred record, written in the SAME
-- transaction as its parent and the checkpoint advance — so there is no window
-- in which the position moved and the scope is unindexed.
--
-- A PATH RATHER THAN A TUPLE OF COORDINATES, because the framework's
-- containment model is a path hierarchy: it knows one thing about a scope, that
-- levels are separated left to right, and it computes coverage from that alone.
--
-- The probe is two clauses over this column and neither finds the other's case:
-- a stored path IN the query's closure is one that CONTAINS what the query is
-- about, and a stored path UNDER one of its roots is one INSIDE it.
--
-- A PLAIN ROWID TABLE: `WITHOUT ROWID` is refused by the pinned driver as an
-- experimental feature, so a migration carrying it would fail on every node and
-- every store would refuse to open.
--
-- NO FOREIGN KEY: the parent's own delete removes these rows in the same
-- statement list, because a cascade is a delete nobody committed.
CREATE TABLE chart_log_deferred_scope (
    position INTEGER NOT NULL,
    path     TEXT    NOT NULL,
    PRIMARY KEY (position, path)
);
CREATE INDEX chart_log_deferred_scope_idx ON chart_log_deferred_scope (path, position);    -- the read barrier's coverage probe and the writer's step 0

-- ---------------------------------------------------------------------------
-- And one ADDITIVE column on a table this domain does not own.
-- ---------------------------------------------------------------------------

-- tracker_projects.chart_position — the position on THIS log of the chart
-- record that last wrote a project's chart-owned fields.
--
-- It sits BESIDE `chart_epoch`, which stays and stays written. The two answer
-- the same question in two number spaces: `chart_epoch` is the config
-- revision's own epoch, which is what the tracker's ApplyChart reconcile
-- compares today, and this is the composed (generation << 40) | sequence a
-- state-log guard compares — the same integer space as every other `version`
-- column in this estate. A later change moves that writer onto the chart log
-- and drops `chart_epoch` in a migration of its own; until it does, a node runs
-- one writer and the other column is zero.
--
-- IT LANDS HERE AND NOT THERE because schema_migrations keys on the FILENAME:
-- adding a column by editing `0002_the_tracker_lands.sql` would silently never
-- run on any database that had already applied it, and every one of those would
-- keep the old shape while the code assumed the new one.
ALTER TABLE tracker_projects ADD COLUMN chart_position INTEGER NOT NULL DEFAULT 0;
