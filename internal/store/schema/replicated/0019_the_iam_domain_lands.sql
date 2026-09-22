-- The identity estate's durable state: the read side of the iam log, and the
-- state log's FIFTH domain.
--
-- Every table here is REBUILT FROM THE LOG rather than written by a caller.
-- The applier is the only writer, its transaction carries the checkpoint, and
-- an identity claim compares these rows byte for byte across the fleet — so
-- anything a node decides for itself belongs in the node's own estate instead.
--
-- # What this domain is, and what it replaces
--
-- Until now the engine knew an OPERATOR TOKEN rather than a person: a request
-- resolved to an id and a bool, and every write a human made landed in the
-- audit trail under whatever name the token happened to carry. There were no
-- people, no sessions, no credentials and no way to take access away from one
-- person without rotating a secret every other person also held.
--
-- # THE ONE RULE THIS SCHEMA IS BUILT AROUND: uniqueness is not an index
--
-- The replicated estate forbids UNIQUE outside a primary key, because a
-- constraint violation inside the apply transaction aborts it
-- DETERMINISTICALLY on every node at once — which turns a rare, cosmetic
-- anomaly into a fleet-wide stalled log with no partial-failure arm to recover
-- through. In this domain that stalled log is every login in the company.
--
-- So NOTHING HERE IS UNIQUE, and the uniqueness an identity estate obviously
-- needs is enforced somewhere else entirely: at the BROKER, by the subject a
-- claim arbitrates on. An address, a login and a seat binding each get a
-- subject of their own, claimed create-only at an expectation of zero, so two
-- administrators enrolling one address contend and exactly one wins. The
-- subject grammar IS the uniqueness check; internal/iamdomain argues it in
-- full.
--
-- What is left for SQL to do is REPORT. The three partial indexes at the foot
-- of iam_people are NON-UNIQUE on purpose and are read by a duty that says "two
-- people hold this address" — a state that cannot arise from ordinary traffic
-- and can arise from a restore or a reanchor. A reviewer who reads that duty as
-- a backstop and reinstates a unique index converts a rare anomaly somebody can
-- repair into an outage nobody can.
--
-- # The three rules the schema enforces, and one it deliberately does not
--
-- NO FOREIGN KEY ANYWHERE. A cascade is a delete nobody committed: the applier
-- removes a person's credentials and sessions in the same statement list, where
-- the deletion is part of the record's own effect and therefore identical on
-- every node.
--
-- NO `UNIQUE` OUTSIDE A PRIMARY KEY, as above.
--
-- NO `COLLATE` ANYWHERE. The default TEXT collation is BINARY, so an ORDER BY
-- reproduces the Go comparison exactly. The pinned driver ACCEPTS
-- `COLLATE NOCASE`, which would silently collapse 'A' and 'a' — and a login's
-- folding is done in Go, once, on the way into the subject, so a collation here
-- would be a second answer to what one login is.
--
-- What it does NOT enforce is the rows' version monotonicity: that is the
-- applier's `WHERE excluded.version > version` guard, because it has to be a
-- SKIP rather than an error — a redelivered record is ordinary traffic.
--
-- # What is in the clear here, and what is not
--
-- A person's NAME and their EMAIL ADDRESS are sealed under that person's own
-- data encryption key, with their id as the additional authenticated data,
-- BEFORE the record is published. So they are ciphertext on the broker, in
-- every node's deferred table, in every snapshot and in the columns below —
-- and destroying the key is what a removal actually does, which is why the row
-- and the id outlive it.
--
-- Sealing at the WRITER rather than per node is what keeps the identity claim a
-- byte comparison: every node writes the same ciphertext, so a determinism
-- check compares content rather than counting rows it cannot read.
--
-- A LOGIN is in the clear, and the asymmetry with the address is deliberate: a
-- login is a name the company chose, printed beside every change an operator
-- reads, in a grammar internal/iam defines. Blinding a value the dashboard
-- renders on every row would cost an operator the ability to read their own
-- audit trail for nothing.
--
-- A BLIND is a keyed hash of a normalised address: a node holding the key can
-- compute it from an address, and nobody else can go the other way. It is what
-- the request path looks a person up by, so signing in opens nothing.

-- ---------------------------------------------------------------------------
-- The bucket column, which every table here carries.
--
-- The identity estate is divided into 64 buckets by an FNV-1a of the PERSON's
-- id — the id nothing renames — and the column appears on every table for two
-- readers that cannot see each other: the per-bucket retention sweep, whose
-- range delete needs it to be a seek rather than a scan, and the
-- duplicate-claim duty, whose cost is then measurable per bucket rather than
-- per company.
--
-- NOTHING ROUTES ON IT. Every node holds every bucket and reads every bucket;
-- it is a partition of WORK, not of storage, which is the same thing
-- internal/search says about its own unrelated 64.
-- ---------------------------------------------------------------------------

-- iam_people — one person or machine.
CREATE TABLE iam_people (
    -- The uuid7 minted at enrolment. NOTHING RENAMES IT: a person changes
    -- their address, their login, their name and their seat, and every audit
    -- row they ever authored still resolves through this.
    id                 TEXT    NOT NULL PRIMARY KEY,
    -- person or machine, in internal/iam's vocabulary. A machine is an
    -- ORDINARY state rather than a misconfiguration — CI, a pipeline — which
    -- is why it is a value here and not a person row with fields left blank.
    kind               TEXT    NOT NULL,
    -- invited / enrolling / active / suspended / retired. Only `active` may
    -- act, which is an allowlist of one: a stage this build does not know
    -- answers false, and a denylist would have admitted it.
    stage              TEXT    NOT NULL DEFAULT '',
    -- THE THREE CLAIMS, denormalised onto the row they belong to.
    --
    -- Columns and not a claims table, because a claim's uniqueness is
    -- enforced at the broker by the subject it arbitrates on, so a table would
    -- add no constraint. What the columns buy is the duplicate scan at the
    -- foot of this file.
    --
    -- THEY MUST BE HERE, in the migration that creates the table:
    -- schema_migrations keys on the FILENAME, so a column added by editing
    -- this file later would silently never run on any database that had
    -- already applied it.
    login              TEXT    NOT NULL DEFAULT '',
    email_blind        TEXT    NOT NULL DEFAULT '',
    -- The seat this person is bound to, by its DERIVED id rather than its
    -- handle: a rename moves a handle and this binding survives one.
    seat_id            TEXT    NOT NULL DEFAULT '',
    -- Sealed under this person's own key with their id as AAD. Ciphertext on
    -- every node, and unreadable on every node once a removal has destroyed
    -- the key.
    name_sealed        BLOB    NOT NULL DEFAULT x'',
    email_sealed       BLOB    NOT NULL DEFAULT x'',
    -- Whether this person's key still exists. A removal destroys the key and
    -- leaves the row, so a reader needs to be able to tell "sealed, and I can
    -- open it" from "sealed, and nobody ever will again" without attempting a
    -- decrypt and reading a failure as a key-store outage.
    shredded           INTEGER NOT NULL DEFAULT 0,
    bucket             INTEGER NOT NULL DEFAULT 0,
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL,
    version            INTEGER NOT NULL,
    -- The highest record that wrote this row FROM ANOTHER SUBJECT — a claim,
    -- which arbitrates on the address rather than on the person. It is stamped
    -- instead of `version` so the row's broker expectation still matches its
    -- own subject's last message and cannot be poisoned into permanent
    -- unwritability; a read barrier compares MAX of the two.
    scoped_through     INTEGER NOT NULL DEFAULT 0,
    document           BLOB    NOT NULL
);
-- The directory listing, and the sweep's own walk.
CREATE INDEX iam_people_bucket_idx ON iam_people (bucket, id);                              -- one bucket's people
CREATE INDEX iam_people_stage_idx ON iam_people (stage, id);                                -- the people who may act, and the abandoned enrolments
-- The seat binding read from the seat's end: which person holds this seat.
CREATE INDEX iam_people_seat_lookup_idx ON iam_people (seat_id);                            -- resolve a seat to the person bound to it

-- THE THREE DUPLICATE-CLAIM INDEXES, and they are NON-UNIQUE ON PURPOSE.
--
-- A unique index here would be the obvious thing to write and is the one thing
-- this estate may not have: a duplicate cannot arise from ordinary traffic,
-- because the broker refuses the second claim, and it CAN arise from a restore
-- or a reanchor — at which point a unique index aborts the apply transaction on
-- every node simultaneously and nobody in the company can sign in.
--
-- So they are non-unique, and what reads them is a DUTY that reports: two
-- people hold this address, here are their ids, an operator decides. PARTIAL,
-- because an unclaimed column is the empty string on most rows and an index
-- over thousands of identical empty keys is a scan wearing an index's name.
CREATE INDEX iam_people_email_claim_idx ON iam_people (email_blind, id) WHERE email_blind != '';  -- the duplicate-address report
CREATE INDEX iam_people_login_claim_idx ON iam_people (login, id) WHERE login != '';              -- the duplicate-login report
CREATE INDEX iam_people_seat_claim_idx ON iam_people (seat_id, id) WHERE seat_id != '';           -- the duplicate-seat-binding report

-- iam_credentials — what a person proves themselves with.
--
-- ONE ROW PER CREDENTIAL and not one per person, because a person holds a
-- password and an identity-provider binding at once, and a machine holds
-- several tokens with different expiries.
CREATE TABLE iam_credentials (
    id            TEXT    NOT NULL PRIMARY KEY,
    person_id     TEXT    NOT NULL,
    -- password / oidc / token.
    method        TEXT    NOT NULL,
    -- THE VERIFIER, never the secret. A password's is an argon2id digest with
    -- its own parameters beside it; a machine token's is a hash of the token;
    -- an identity provider's is the issuer and the subject, which are not
    -- secret at all. Nothing in this column can be presented to anything.
    verifier      BLOB    NOT NULL DEFAULT x'',
    -- The identity provider's own subject, for an oidc row. Blinded for the
    -- address's reason: it identifies a person at a third party.
    subject_blind TEXT    NOT NULL DEFAULT '',
    expires_at    INTEGER NOT NULL DEFAULT 0,
    -- When this credential stopped being usable, and why, so a revoked token
    -- is a row somebody can read rather than a row that vanished.
    revoked_at    INTEGER NOT NULL DEFAULT 0,
    bucket        INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL,
    version       INTEGER NOT NULL,
    document      BLOB    NOT NULL
);
CREATE INDEX iam_credentials_person_idx ON iam_credentials (person_id, method);             -- one person's credentials
-- The identity-provider sign-in lookup: an issuer's subject to a person.
CREATE INDEX iam_credentials_subject_idx ON iam_credentials (subject_blind) WHERE subject_blind != ''; -- resolve an IdP subject to a credential
CREATE INDEX iam_credentials_bucket_idx ON iam_credentials (bucket, id);                    -- the per-bucket sweep

-- iam_invites — an address spoken for by somebody who has no person yet.
CREATE TABLE iam_invites (
    id           TEXT    NOT NULL PRIMARY KEY,
    -- The address the invitation is for, blinded like every other address
    -- here. It is the subject the invitation arbitrated on, which is what
    -- makes an invitation and an enrolment for one address contend.
    email_blind  TEXT    NOT NULL,
    -- Sealed, so an invitation that is never redeemed still does not leave a
    -- cleartext address in every node's database for ever.
    email_sealed BLOB    NOT NULL DEFAULT x'',
    invited_by   TEXT    NOT NULL DEFAULT '',
    -- The grants this invitation confers when it is redeemed, so the decision
    -- is made once by the person who made it rather than again by whoever
    -- happens to process the redemption.
    grants_json  TEXT    NOT NULL DEFAULT '[]',
    expires_at   INTEGER NOT NULL DEFAULT 0,
    redeemed_at  INTEGER NOT NULL DEFAULT 0,
    -- The person the redemption created, empty until it happens.
    person_id    TEXT    NOT NULL DEFAULT '',
    bucket       INTEGER NOT NULL DEFAULT 0,
    created_at   INTEGER NOT NULL,
    version      INTEGER NOT NULL,
    document     BLOB    NOT NULL
);
CREATE INDEX iam_invites_email_idx ON iam_invites (email_blind, created_at DESC);           -- the invitations for one address
-- PARTIAL AND BUCKET-LEADING, matching the sweep's own predicate. A redeemed
-- invitation is never collected by age, so it has no business in the index the
-- collection seeks on.
CREATE INDEX iam_invites_open_idx ON iam_invites (bucket, expires_at) WHERE redeemed_at = 0; -- the invitations still outstanding, per bucket

-- iam_bootstrap_codes — how a company with nobody in it acquires its first
-- administrator.
--
-- ONE SUBJECT FOR THE WHOLE DOMAIN arbitrates these, so two nodes minting a
-- code contend and exactly one wins: two live bootstrap codes is two ways into
-- an engine that has no other way in.
CREATE TABLE iam_bootstrap_codes (
    id          TEXT    NOT NULL PRIMARY KEY,
    -- The VERIFIER again, never the code. A code that could be read out of a
    -- replicated database by anyone who can read a replicated database is not
    -- a credential.
    verifier    BLOB    NOT NULL,
    expires_at  INTEGER NOT NULL DEFAULT 0,
    redeemed_at INTEGER NOT NULL DEFAULT 0,
    -- The person the redemption created.
    person_id   TEXT    NOT NULL DEFAULT '',
    -- The node that minted it, which is what an operator reading a code they
    -- did not expect needs first.
    minted_by   TEXT    NOT NULL DEFAULT '',
    bucket      INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL,
    version     INTEGER NOT NULL,
    document    BLOB    NOT NULL
);
-- Every bootstrap code falls in ONE bucket, because the bootstrap has no person
-- and takes the bucket of its own subject — so leading with it costs nothing
-- and keeps this index the same shape as every other collection predicate,
-- which is what stops the sweep's SQL having a special case for one table.
CREATE INDEX iam_bootstrap_codes_open_idx ON iam_bootstrap_codes (bucket, expires_at) WHERE redeemed_at = 0; -- is there a live way in right now

-- iam_sessions — one row per session LINEAGE.
--
-- ROTATIONS ARE NOT ROWS. A rotation id is an HMAC over the lineage and the
-- session's age in rotate_after units, so the busiest thing a signed-in person
-- does writes nothing here at all — which is why this table grows with
-- sign-ins rather than with requests, and why the log's ceiling is sized from
-- sign-ins.
--
-- NO IP ADDRESS, NO USER AGENT, NO DEVICE FINGERPRINT. Where a session was
-- opened from is a request-scoped observation; putting one on a replicated,
-- retained table would make every node's database a location history of
-- everybody who works here.
CREATE TABLE iam_sessions (
    lineage             TEXT    NOT NULL PRIMARY KEY,
    person_id           TEXT    NOT NULL,
    -- The revocation epoch this session was opened at. A bearer presenting an
    -- epoch below the person's current one is over, which is what makes
    -- "sign out everywhere" one write rather than N deletes.
    epoch               INTEGER NOT NULL DEFAULT 0,
    -- The composed (generation << 40) | sequence this session began at, which
    -- is what a bearer carries so a node can tell a session opened before its
    -- own checkpoint from one it has simply not seen yet.
    start_position      INTEGER NOT NULL DEFAULT 0,
    absolute_expires_at INTEGER NOT NULL DEFAULT 0,
    -- When it ended, and why: signed out, revoked, expired, reuse detected.
    -- The row is kept until the sweep collects it, because "this session was
    -- ended by reuse detection" is the sentence an investigation is looking
    -- for.
    ended_at            INTEGER NOT NULL DEFAULT 0,
    ended_reason        TEXT    NOT NULL DEFAULT '',
    bucket              INTEGER NOT NULL DEFAULT 0,
    created_at          INTEGER NOT NULL,
    version             INTEGER NOT NULL,
    document            BLOB    NOT NULL
);
CREATE INDEX iam_sessions_person_idx ON iam_sessions (person_id, created_at DESC);          -- one person's sessions, newest first
-- ONE INDEX FOR BOTH HALVES OF THE SWEEP, and it leads with the bucket because
-- the sweep does: a session is collected either because it ENDED long enough
-- ago (bucket, a range over ended_at) or because it EXPIRED without ending
-- (bucket, ended_at = 0, a range over absolute_expires_at). An index leading
-- with the expiry would serve the second and leave the first scanning the
-- whole table, which is the shape that looks like an index and is not.
--
-- The company-wide "which sessions are live" read then costs 64 seeks rather
-- than one. That is the right way round: the sweep runs on every tick against
-- the largest table here, and the live listing is an operator opening a screen.
CREATE INDEX iam_sessions_sweep_idx ON iam_sessions (bucket, ended_at, absolute_expires_at); -- both halves of the per-bucket collection

-- iam_revocation_epochs — one person's REVOCATION EPOCH.
--
-- A TABLE OF ITS OWN rather than a column on iam_people, and the reason is the
-- READ: every request carrying a session bearer compares against this, so it
-- is the hottest row in the domain. Beside a person's sealed document it would
-- mean reading and decoding a blob to learn one integer, on every request, for
-- ever.
--
-- NAMED FOR WHAT IT HOLDS and not for what reads it. It is one number PER
-- PERSON, and the fleet-wide number a bearer also carries is the singleton
-- below — two counters with two writers, two meanings and two reasons to move,
-- which is why the one that ends one person's sessions and the one that ends
-- everybody's do not share a word.
CREATE TABLE iam_revocation_epochs (
    person_id  TEXT    NOT NULL PRIMARY KEY,
    epoch      INTEGER NOT NULL DEFAULT 0,
    -- Why the epoch last moved: signing out everywhere, a password change, or
    -- detected token reuse. The three produce an IDENTICAL bump and which one
    -- fired is the first question anybody investigating a compromise asks.
    reason     TEXT    NOT NULL DEFAULT '',
    bumped_at  INTEGER NOT NULL DEFAULT 0,
    bucket     INTEGER NOT NULL DEFAULT 0,
    version    INTEGER NOT NULL
);
CREATE INDEX iam_revocation_epochs_bucket_idx ON iam_revocation_epochs (bucket, person_id); -- the per-bucket sweep

-- iam_session_generation — the FLEET-WIDE generation every session bearer
-- carries, and the one row in this estate that is about nobody.
--
-- ONE ROW FOR THE WHOLE COMPANY, keyed on a constant, because what it answers
-- is a single question: is every cookie issued before this moment still good?
-- A bearer carries the generation it was minted under and a node compares it
-- against this row, so bumping it ends every session of every person as each
-- node applies the record — with no per-person write, no enumeration, and
-- nothing to miss.
--
-- WHY IT IS NOT THE SUM OF THE PER-PERSON EPOCHS ABOVE. A restore rolls this
-- estate back to an artefact's own instant, and a revocation epoch bumped
-- after that artefact was taken is rolled back with it — so a session somebody
-- revoked comes back. The restore runbook's last step (`crewlet iam
-- invalidate-all`) bumps THIS counter, which every surviving cookie is below
-- whatever the per-person rows were rolled back to. It is the only number in
-- the estate that can be moved forward without knowing who was affected.
--
-- WRITTEN BY ONE OP, `invalidate`, which INSTALLS A GATE: a node that deferred
-- it would go on honouring every bearer the company had just invalidated,
-- which is the exact shape of failure a deferred gate always is.
CREATE TABLE iam_session_generation (
    -- Always 0. A singleton table's key is a constant rather than a magic
    -- string, so the row cannot be duplicated by a writer that spelled its own
    -- name differently.
    singleton  INTEGER NOT NULL PRIMARY KEY,
    generation INTEGER NOT NULL DEFAULT 0,
    -- Why every session in the company was ended: a restore, a suspected
    -- compromise, a key rotation somebody cut short. Nobody bumps this
    -- casually and everybody asks why afterwards.
    reason     TEXT    NOT NULL DEFAULT '',
    bumped_at  INTEGER NOT NULL DEFAULT 0,
    by         TEXT    NOT NULL DEFAULT '',
    version    INTEGER NOT NULL
);

-- iam_history — the authentication trail.
--
-- THE ONLY TABLE HERE WITH TWO RETENTION HORIZONS, and the `class` column is
-- what separates them: a CHANGE ("who suspended this person") is an audit
-- somebody asks a year later and in several jurisdictions is one they must be
-- able to ask; a SESSION ("who signed in on Tuesday") answers an investigation
-- that is days old and is a record of a person's working hours for as long as
-- it is kept. Keeping the second as long as the first would be storing more
-- about people than there is a reason to.
--
-- The class is DERIVED FROM THE OP by internal/iamdomain and never carried on
-- the record: a writer that stated its own class could file a suspension in the
-- short horizon, and the row would simply be gone when somebody went looking.
CREATE TABLE iam_history (
    id          TEXT    NOT NULL PRIMARY KEY,
    -- change or session.
    class       TEXT    NOT NULL,
    -- The object the entry is about, as the pair every scope term and every
    -- surface uses: a kind and an id, never a composed string that would be
    -- parsed apart again at each of the places that read it.
    object_kind TEXT    NOT NULL,
    object_id   TEXT    NOT NULL,
    -- The person the entry concerns, which is what "show me everything about
    -- this person" reads — and it is NOT object_id, because a claim's object
    -- is an address and a session's is a lineage.
    person_id   TEXT    NOT NULL DEFAULT '',
    op          TEXT    NOT NULL,
    actor       TEXT    NOT NULL DEFAULT '',
    -- The actor's KIND as a column, never a prefix on the name: a prefixed
    -- operator showed one person as two people three rows apart in the audit
    -- feed, which is the mistake internal/iam exists to make unrepeatable.
    actor_kind  TEXT    NOT NULL DEFAULT '',
    reason      TEXT    NOT NULL DEFAULT '',
    summary     TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    -- broker_at is the RAW broker instant this entry was committed at, which is
    -- what makes the row byte-identical on every node rather than a clock each
    -- one read for itself.
    broker_at   INTEGER NOT NULL DEFAULT 0,
    bucket      INTEGER NOT NULL DEFAULT 0,
    version     INTEGER NOT NULL,
    document    BLOB    NOT NULL
);
CREATE INDEX iam_history_person_idx ON iam_history (person_id, broker_at DESC);             -- everything that happened to one person
CREATE INDEX iam_history_object_idx ON iam_history (object_kind, object_id, broker_at DESC); -- one address's or one session's own trail
-- NON-UNIQUE, and it is the index the two horizons are resolved through: a
-- sweep turns "ninety days" into a POSITION by reading the newest row of that
-- class below the age, once, at the publisher — so two nodes with skewed clocks
-- delete identical rows.
CREATE INDEX iam_history_class_idx ON iam_history (class, broker_at);                       -- resolve a retention horizon to a position
-- And the range delete itself, which ships its index for the ops sweep's
-- reason: without it a node returning from a month away scans the whole table
-- on every tick.
CREATE INDEX iam_history_sweep_idx ON iam_history (bucket, class, version);                 -- the per-bucket range delete

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

-- iam_ops — contract 3's first layer: what this node's applier has written.
--
-- A DONOR'S OPERATION LEDGER IS THE SHARPEST THING A SNAPSHOT MUST SCRUB: an
-- adopted peer's ops table would let this node resolve its own ambiguous
-- publish against somebody else's history, which is a write reported as landed
-- that never happened.
--
-- THE FRAMEWORK'S FOUR COLUMNS AND NOTHING ELSE. A `kind` column and an index
-- over (kind, applied_at) would be a SECOND HORIZON — a claim that a session's
-- op id may be swept on a different schedule from an enrolment's — and this
-- domain declares ONE ops horizon. Two horizons on one ledger is a retry that
-- resolves against a history half of which has been deleted.
--
-- The authentication trail above has two horizons and this has one, which is
-- not a contradiction: they answer different questions and are measured against
-- different things — an audit obligation, and the longest a client will retry.
CREATE TABLE iam_ops (
    op_id      TEXT    NOT NULL PRIMARY KEY,
    subject    TEXT    NOT NULL,
    -- The COMPOSED (generation << 40) | sequence, so a position from before a
    -- reanchor is comparable and safely stale rather than plausible.
    position   INTEGER NOT NULL,
    applied_at INTEGER NOT NULL
);
-- The sweep is a range delete over the age, and a range delete ships its index.
CREATE INDEX iam_ops_swept_idx ON iam_ops (applied_at);                                     -- the ops retention sweep

-- iam_log_deferred — a record this build could not decode, retained byte for
-- byte at its original position.
--
-- NOTHING PERSONAL REACHES THIS TABLE, and that is a property of the record
-- format rather than of this file: every envelope field is readable by every
-- build for ever and is therefore rendered by every operator surface that
-- reports a stalled log, so the subject is a blind or an opaque id and the
-- scope is a bucket number. The payload is bytes, and the values inside it are
-- sealed.
CREATE TABLE iam_log_deferred (
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
-- subject_id LEADS, because the per-object probe reads it ALONE: an address
-- claim and an invitation share a blind under different kinds, and the
-- dependency is on the OBJECT rather than on the kind.
CREATE INDEX iam_log_deferred_subject_idx ON iam_log_deferred (subject_id, subject_kind);   -- the applier's per-object deferral probe

-- One row per scope PATH of the deferred record, written in the SAME
-- transaction as its parent and the checkpoint advance — so there is no window
-- in which the position moved and the scope is unindexed.
--
-- A PATH RATHER THAN A TUPLE OF COORDINATES, because the framework's
-- containment model is a path hierarchy: it knows one thing about a scope, that
-- levels are separated left to right, and it computes coverage from that alone.
--
-- THIS DOMAIN'S PATHS ARE FLAT — `i` and `i/b/NN`, and nothing below — so the
-- containment the index serves collapses to equality plus the root. That is
-- deliberate: a person is not inside another person, and a per-person path
-- could not be computed by the one node that needs it, because a claim's
-- subject is a blind and the person it is about is inside a payload that node
-- could not decode.
--
-- A PLAIN ROWID TABLE: `WITHOUT ROWID` is refused by the pinned driver as an
-- experimental feature, so a migration carrying it would fail on every node and
-- every store would refuse to open.
--
-- NO FOREIGN KEY: the parent's own delete removes these rows in the same
-- statement list, because a cascade is a delete nobody committed.
CREATE TABLE iam_log_deferred_scope (
    position INTEGER NOT NULL,
    path     TEXT    NOT NULL,
    PRIMARY KEY (position, path)
);
CREATE INDEX iam_log_deferred_scope_idx ON iam_log_deferred_scope (path, position);         -- the read barrier's coverage probe and the writer's step 0

-- ---------------------------------------------------------------------------
-- The three GATE tables: not the objects a record writes, but the state an
-- apply reads BEFORE it writes anything.
--
-- Each is REPRODUCIBLE for the same reason its gate is trustworthy: the record
-- that installs it is on this log, in this order, and every node reaches the
-- same verdict from it with no clock and no coordination read. And they OUTLIVE
-- the records that wrote them — a removal below the trim floor has no record
-- left on the log to prove it happened, and its row is what still says so.
-- ---------------------------------------------------------------------------

-- iam_evictions — a node's removal from THIS log, or its readmission.
--
-- The gate is "records this node wrote ABOVE this position", and the position
-- is the log's own — so every node reaches the same verdict about every record
-- with no clock, no coordination read and no agreement beyond the order they
-- all already have. That is what makes it the fence that still holds when
-- coordination cannot be reached at all, which is the only state in which an
-- eviction is permitted at all.
CREATE TABLE iam_evictions (
    node_id             TEXT    NOT NULL PRIMARY KEY,
    at                  INTEGER NOT NULL,
    by                  TEXT    NOT NULL DEFAULT '',
    from_position       INTEGER NOT NULL,
    -- NULL until a readmission, which is an INVERSE COMMIT rather than a
    -- delete: an eviction's whole history survives a replay, and a node that
    -- was evicted, readmitted and evicted again reads correctly rather than as
    -- one long absence.
    readmitted_position INTEGER,
    version             INTEGER NOT NULL
);

-- iam_log_generations — every reanchor's audit row.
--
-- A reanchor is a committed record and therefore reproducible from the log;
-- this is what a recovery replay writes and what the operator surface reads.
CREATE TABLE iam_log_generations (
    generation            INTEGER NOT NULL PRIMARY KEY,
    at                    INTEGER NOT NULL,
    by                    TEXT    NOT NULL DEFAULT '',
    new_stream_created_at INTEGER NOT NULL DEFAULT 0,
    prev_last_seq_seen    INTEGER NOT NULL DEFAULT 0,
    record_id             TEXT    NOT NULL DEFAULT ''
);

-- iam_removed — who the company no longer has.
--
-- THE ONE OPERATION HERE WITH NO INVERSE, which is why it leaves a row behind
-- rather than only deleting one. Every other record is a full post-state under
-- a monotone guard, so a node that applied a stale one is repaired by the next
-- record about that person; nothing ever names a removed person again. Without
-- this row a redelivery of any earlier record about them would write the person
-- back, on one node, for ever — and in THIS domain that is not a stale row, it
-- is somebody the company off-boarded still signing in.
--
-- IT IS ALSO WHY THE ID OUTLIVES THE PERSON. What a removal destroys is their
-- KEY, which makes their name and address unrecoverable from every artefact at
-- once; the row and the id survive, because the authentication trail names them
-- and a history whose authors evaporate is not an audit trail.
--
-- THE CLAIMS ARE COLUMNS HERE TOO, and they are what stops a removed person's
-- address being silently re-enrolled as a different person while the claim's
-- own subject still carries an anchor nobody can see the object of. A claim
-- whose holder is removed is RELEASED by the same apply; what this row keeps is
-- the record of which claims that was.
CREATE TABLE iam_removed (
    person_id   TEXT    NOT NULL PRIMARY KEY,
    at          INTEGER NOT NULL,
    -- The record that removed them, by its OPERATION ID rather than its
    -- position: the apply that writes this row is the one that must be able to
    -- run twice, and a position changes under a republish where an op id does
    -- not.
    record_id   TEXT    NOT NULL DEFAULT '',
    actor       TEXT    NOT NULL DEFAULT '',
    actor_kind  TEXT    NOT NULL DEFAULT '',
    reason      TEXT    NOT NULL DEFAULT '',
    -- The claims this person held when they were removed, as a JSON object,
    -- so an operator asking "who held this address" after the fact has an
    -- answer that is not a decryption of a key that no longer exists.
    --
    -- THE BLINDS RATHER THAN THE VALUES. A removal's whole promise is that the
    -- address is unrecoverable; writing it here in the clear, in the one row
    -- designed to outlive the person, would be the promise broken by the
    -- mechanism that makes it.
    claims_json TEXT    NOT NULL DEFAULT '{}',
    bucket      INTEGER NOT NULL DEFAULT 0,
    version     INTEGER NOT NULL
);
-- The gate's own read is by person_id and is served by the primary key. This
-- index is for the OTHER reader: the operator surface asking who left recently,
-- and the per-bucket sweep that has a horizon to range over.
CREATE INDEX iam_removed_sweep_idx ON iam_removed (bucket, at);                             -- who left, per bucket, by age
