-- The identity estate lands as the state log's FOURTH domain, a ledger learns
-- to record a loss per subject kind, and the tracker's rows follow the build
-- that reads them.
--
-- Three parts, in this order: the identity estate's tables, the framework's
-- per-kind ledger watermark it needs, and the tracker's own changes.

-- ===========================================================================
-- # THE IDENTITY ESTATE
--
-- People, the addresses and logins they are known by, the credentials they
-- hold, the invitations that enrolled them and the sessions they are signed in
-- with: the read side of the iam log. internal/iamdomain is the domain, and
-- internal/iam the vocabulary it is written in.
--
-- Every table here is REBUILT FROM THE LOG rather than written by a caller.
-- The applier is the only writer, its transaction carries the checkpoint, and
-- an identity claim compares these rows byte for byte across the fleet — so
-- anything a node decides for itself belongs in the node's own estate instead.
--
-- ## What it replaces
--
-- The engine used to know an OPERATOR TOKEN rather than a person: a request
-- resolved to an id and a bool, and every write a human made landed in the
-- audit trail under whatever name the token happened to carry. There were no
-- people, no sessions, no credentials and no way to take access away from one
-- person without rotating a secret every other person also held.
--
-- ## THE ONE RULE THIS SCHEMA IS BUILT AROUND: uniqueness is not an index
--
-- The replicated estate forbids UNIQUE outside a primary key, because a
-- constraint violation inside the apply transaction aborts it
-- DETERMINISTICALLY on every node at once — which turns a rare, cosmetic
-- anomaly into a fleet-wide stalled log with no partial-failure arm to recover
-- through. In this domain that stalled log is every login in the company.
--
-- So NOTHING HERE IS UNIQUE, and the uniqueness an identity estate obviously
-- needs is decided somewhere else entirely: on the DIRECTORY's one subject.
-- Every record that sets or frees an address, a login or a seat publishes
-- there, decided in a snapshot of every row, so a value somebody else holds is
-- refused naming them and the broker accepts the record only if no other
-- directory record landed since. internal/iamdomain argues it in full.
--
-- What is left for SQL to do is LOOK UP. The indexes over the address blind,
-- the login and the seat are plain and NON-UNIQUE on purpose: two rows holding
-- one value cannot arise from ordinary traffic and CAN arise from a restore or
-- a retained record, and a lookup that meets two answers that this node cannot
-- say. A reviewer who reinstates a unique index converts a rare anomaly
-- somebody can repair into an outage nobody can.
--
-- ## The three rules the schema enforces, and one it deliberately does not
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
-- grammar is decided in Go, once, so a collation here would be a second answer
-- to what one login is.
--
-- What it does NOT enforce is the rows' version monotonicity: that is the
-- applier's `WHERE excluded.version > version` guard, because it has to be a
-- SKIP rather than an error — a redelivered record is ordinary traffic.
--
-- ## What is in the clear here, and what is not
--
-- A person's NAME and ADDRESS, an invitation's ADDRESS and a second factor's
-- SEED are sealed under the FLEET KEYRING by the WRITER, before the record is
-- published, each bound by its associated data to whose value it is and which
-- of their values. So they are ciphertext on the broker, in every node's
-- deferred table, in every snapshot and in the columns below — and sealing at
-- the writer rather than per node is what keeps the identity claim a byte
-- comparison: every node writes the same ciphertext. The applier never opens
-- one.
--
-- A LOGIN is in the clear, and the asymmetry with the address is deliberate: a
-- login is a name the company chose, printed beside every change an operator
-- reads, in a grammar internal/iam defines. Blinding a value the dashboard
-- renders on every row would cost an operator the ability to read their own
-- audit trail for nothing.
--
-- A BLIND is a keyed hash of a normalised address: a node holding the
-- company's key can compute it from an address, and nobody can go the other
-- way. It is what the request path looks a person up by, so signing in opens
-- nothing.
--
-- A REMOVAL ERASES: its apply deletes the person's rows and clears every
-- sealed value of theirs that rows not their own still hold — an invitation
-- addressed to them, the trail rows whose records carried their values — so
-- afterwards no row on any node holds anything of theirs that opens.
--
-- ## The bucket column, which every table here carries
--
-- The identity estate is divided into 64 buckets by an FNV-1a of the PERSON's
-- id — the id nothing renames — and the column appears on every table for the
-- per-bucket retention sweep, whose range delete needs it to be a seek rather
-- than a scan.
--
-- NOTHING ROUTES ON IT. Every node holds every bucket and reads every bucket;
-- it is a partition of WORK, not of storage, which is the same thing
-- internal/search says about its own unrelated 64.
-- ===========================================================================

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
    -- THE THREE DIRECTORY VALUES, on the row they belong to.
    --
    -- Columns and not a table of their own, because their uniqueness is
    -- decided on the directory's subject, so a table would add no
    -- constraint — only a join.
    login              TEXT    NOT NULL DEFAULT '',
    email_blind        TEXT    NOT NULL DEFAULT '',
    -- The seat this person is bound to, by its HANDLE — the identity every
    -- other subsystem already addresses a seat by, and immutable (ADR-0013),
    -- so a binding names its seat for as long as the seat exists.
    seat_id            TEXT    NOT NULL DEFAULT '',
    -- Sealed under the fleet keyring, bound to this person and this field.
    name_sealed        BLOB    NOT NULL DEFAULT x'',
    email_sealed       BLOB    NOT NULL DEFAULT x'',
    bucket             INTEGER NOT NULL DEFAULT 0,
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL,
    -- The ONE monotone guard the row needs. The directory's records create,
    -- re-identify and remove the row; a person's own subject owns everything
    -- else on it; and every record about one person is applied in log order,
    -- so whichever wrote the row last stamps this.
    version            INTEGER NOT NULL,
    document           BLOB    NOT NULL
);
-- The directory listing, and the sweep's own walk.
CREATE INDEX iam_people_bucket_idx ON iam_people (bucket, id);                              -- one bucket's people
CREATE INDEX iam_people_stage_idx ON iam_people (stage, id);                                -- the people who may act, and the abandoned enrolments
-- THE THREE LOOKUPS, plain and non-unique. A sign-in resolves a login or an
-- address blind to its row, and a directory decision asks whether anybody else
-- holds the value it is about to give — each `WHERE <column> = ?` with a bound
-- value, which a PARTIAL index cannot serve: a partial index is usable only
-- where the query's own WHERE implies the index's, and a bound parameter
-- implies nothing about being non-empty.
CREATE INDEX iam_people_seat_lookup_idx ON iam_people (seat_id);                            -- a seat to the person bound to it
CREATE INDEX iam_people_login_idx ON iam_people (login);                                    -- a login to its holder
CREATE INDEX iam_people_email_idx ON iam_people (email_blind);                              -- an address blind to its holder
-- AND ONE PARTIAL, for the reads that list every binding (`SeatBindings`,
-- `SeatHolders`): `WHERE seat_id != '' ORDER BY seat_id, id` is this index's
-- predicate and its order. Measured on this driver, they plan as a scan of the
-- index with it and as a scan of every person plus a sort without it — and
-- they run on every alarm heartbeat and every party-registry rebuild.
CREATE INDEX iam_people_seat_claim_idx ON iam_people (seat_id, id) WHERE seat_id != '';     -- every binding, in seat order

-- iam_credentials — what a person proves themselves with.
--
-- ONE ROW PER CREDENTIAL and not one per person, because a person holds a
-- password and a second factor at once, and a machine holds several tokens
-- with different expiries.
CREATE TABLE iam_credentials (
    id            TEXT    NOT NULL PRIMARY KEY,
    person_id     TEXT    NOT NULL,
    -- password / totp / recovery / token.
    method        TEXT    NOT NULL,
    -- THE VERIFIER, and for one method the sealed secret. A password's is an
    -- argon2id digest with its parameters beside it; a machine token's and a
    -- recovery code's a hash. A second factor's SEED is the one value that
    -- cannot be a verifier — both ends derive the code from it — so it is
    -- sealed under the fleet keyring before it reaches a record. Nothing in
    -- this column can be presented to anything.
    verifier      BLOB    NOT NULL DEFAULT x'',
    expires_at    INTEGER NOT NULL DEFAULT 0,
    -- When this credential stopped being usable, so a revoked token is a row
    -- somebody can read rather than a row that vanished.
    revoked_at    INTEGER NOT NULL DEFAULT 0,
    bucket        INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL,
    version       INTEGER NOT NULL,
    document      BLOB    NOT NULL
);
CREATE INDEX iam_credentials_person_idx ON iam_credentials (person_id, method);             -- one person's credentials
CREATE INDEX iam_credentials_bucket_idx ON iam_credentials (bucket, id);                    -- the per-bucket sweep
-- The sweep collects a credential a week past the moment it stopped being
-- presentable, for EITHER reason, and each of its statements is a seek into
-- one bucket's range on an index shaped like its own predicate: a sweep that
-- scanned would hold this store's only writer for the length of a table.
-- PARTIAL, because a row that is neither revoked nor expiring has no business
-- in the index the collection seeks on, and an index over thousands of zeros
-- is a scan wearing an index's name; TWO, because one index over both columns
-- serves only the one it leads with.
CREATE INDEX iam_credentials_revoked_idx ON iam_credentials (bucket, revoked_at) WHERE revoked_at > 0;   -- the revoked credentials, per bucket
CREATE INDEX iam_credentials_expiring_idx ON iam_credentials (bucket, expires_at) WHERE expires_at > 0;  -- the credentials with an expiry, per bucket

-- iam_invites — an address spoken for by somebody who has no person yet.
CREATE TABLE iam_invites (
    id           TEXT    NOT NULL PRIMARY KEY,
    -- The address the invitation is for, blinded like every other address
    -- here. A directory decision reads it beside `iam_people`, because an
    -- address an open invitation holds is held.
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
-- PARTIAL AND BUCKET-LEADING, each matching one of the sweep's own predicates:
-- an invitation is collected a week after it stopped being presentable —
-- expired unredeemed, or redeemed — and a row in one state has no business in
-- the other's index.
CREATE INDEX iam_invites_open_idx ON iam_invites (bucket, expires_at) WHERE redeemed_at = 0;   -- the invitations still outstanding, per bucket
CREATE INDEX iam_invites_spent_idx ON iam_invites (bucket, redeemed_at) WHERE redeemed_at > 0; -- the redeemed invitations, per bucket

-- iam_sessions — one row per session LINEAGE.
--
-- A RE-ISSUE IS NOT A ROW. The idle deadline a re-issue moves is inside the
-- signed bearer, so the busiest thing a signed-in person does writes nothing
-- here at all — which is why this table grows with sign-ins rather than with
-- requests, and why the log's ceiling is sized from sign-ins.
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
    -- is what a bearer carries so a node can tell a session that ended from
    -- one it has simply not applied yet.
    start_position      INTEGER NOT NULL DEFAULT 0,
    absolute_expires_at INTEGER NOT NULL DEFAULT 0,
    -- When it ended, and why: signed out, revoked or expired. The row is kept
    -- until the sweep collects it, because "this session was revoked, and
    -- why" is the sentence an investigation is looking for.
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
-- is the hottest row in the domain. Beside a person's document it would mean
-- reading and decoding a blob to learn one integer, on every request, for
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
    -- Why the epoch last moved, as the record that moved it said: the
    -- bumps are IDENTICAL whatever caused them, and which one fired is the
    -- first question anybody investigating a compromise asks.
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
    -- What the entry is about, as the pair every scope term and every surface
    -- uses: a kind and an id, never a composed string that would be parsed
    -- apart again at each of the places that read it.
    object_kind TEXT    NOT NULL,
    object_id   TEXT    NOT NULL,
    -- The person the entry concerns, which is what "show me everything about
    -- this person" reads — and it is NOT object_id, because a directory
    -- record's object may be an address and a session's is a lineage.
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
    document    BLOB    NOT NULL,
    -- The CREDENTIAL the actor acted through — `pat:<id>` for a machine
    -- token, `session:<lineage>` for one of a person's browser sessions —
    -- the column every other trail has beside its author, since a token acts
    -- AS its owner and the owner is the right actor. EMPTY where the record
    -- named none: the node's own writer, which acts through no credential
    -- (every sign-in, sign-out and enrolment the sign-in surface writes, and
    -- every duty), and a gate — a removal or an invalidation — whose
    -- announcing event carries the credential instead.
    operator_id TEXT    NOT NULL DEFAULT ''
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
-- The log's own machinery: excluded from the identity claim and from the
-- completeness audit.
--
-- THE FRAMEWORK GENERATES THE STATEMENTS THAT WRITE THESE, so their shape is
-- the framework's — `0001_the_state_log_lands.sql` and, for the ledger's
-- fifth column, `0021_an_operation_names_the_record_that_applied_it.sql` — and
-- not this domain's to choose. Changing a column here compiles, migrates,
-- opens and serves every read, and fails the first time a record this build
-- cannot decode arrives — the rarest path in the system and the one whose
-- failure is a stalled log.
-- ---------------------------------------------------------------------------

-- iam_ops — contract 3's first layer: what this node's applier has written.
--
-- IT TRAVELS in every donated snapshot and stays out of the identity claim:
-- every node's applier writes these rows from the same records, only
-- `applied_at` differs, and an adopter without them could not tell a first
-- attempt from a retry of anything the donor applied.
--
-- THE FRAMEWORK'S FIVE COLUMNS AND NO `kind`, and TWO HORIZONS anyway: a
-- SESSION subject's rows go after an hour, every other kind's after the
-- framework's month (`internal/iamdomain`, `SessionOpsRetention`). A sign-in
-- is answered inside its request and never re-asked, and every close is
-- re-DECIDED rather than re-asked, so a month of them is a row per sign-in for
-- a question nobody asks after an hour. The hour's sweep is a range over the
-- stored SUBJECT, which already carries the kind, and the loss it leaves is
-- recorded per kind (`statelog_ops_lost_kind`, below) — so a `kind` column
-- would be a second copy of what the subject says, and one nothing populates.
CREATE TABLE iam_ops (
    op_id      TEXT    NOT NULL PRIMARY KEY,
    subject    TEXT    NOT NULL,
    -- The COMPOSED (generation << 40) | sequence, so a position from before a
    -- reanchor is comparable and safely stale rather than plausible.
    position   INTEGER NOT NULL,
    applied_at INTEGER NOT NULL,
    -- The applying record's broker instant, in micros; 0021 says why.
    stored_at  INTEGER NOT NULL DEFAULT 0
);
-- Each sweep is a range delete over the age, and a range delete ships its
-- index: without it a node returning from a month away scans the whole table
-- on every tick, and deletes its whole month in one statement holding the only
-- writer. (subject, applied_at) rather than subject alone for the hour's: the
-- range is one kind's subjects, and the second column is what lets the age
-- filter be read off the index rather than off each row.
CREATE INDEX iam_ops_swept_idx ON iam_ops (applied_at);                                     -- the month's sweep
CREATE INDEX iam_ops_subject_swept_idx ON iam_ops (subject, applied_at);                    -- the session ops' own sweep

-- iam_log_deferred — a record this build could not decode, retained byte for
-- byte at its original position. This node's own verdict, scrubbed out of
-- every donated snapshot.
--
-- NOTHING PERSONAL REACHES THIS TABLE, and that is a property of the record
-- format rather than of this file: every envelope field is readable by every
-- build for ever and is therefore rendered by every operator surface that
-- reports a stalled log, so the subject is the directory, a person's opaque id
-- or a lineage, and the scope is a bucket number. The payload is bytes, and
-- the values inside it are sealed.
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
-- subject_id LEADS, because the per-object probe reads it ALONE: the
-- dependency is on the OBJECT rather than on the kind.
CREATE INDEX iam_log_deferred_subject_idx ON iam_log_deferred (subject_id, subject_kind);   -- the applier's per-object deferral probe

-- One row per scope PATH of the deferred record, written in the SAME
-- transaction as its parent and the checkpoint advance — so there is no window
-- in which the position moved and the scope is unindexed.
--
-- THIS DOMAIN'S PATHS ARE FLAT — `i` and `i/b/NN`, and nothing below — so the
-- containment the index serves collapses to equality plus the root. That is
-- deliberate: a person is not inside another person, and a per-person path
-- could not be computed by the one node that needs it, because a directory
-- record's subject names nobody and the person it is about is inside a
-- payload that node could not decode.
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
-- THE ROW AND THE ID OUTLIVE THE PERSON, and nothing else of theirs does: the
-- removal deletes their rows and erases every sealed value of theirs the
-- estate still holds, while the authentication trail goes on naming the id,
-- because a history whose authors evaporate is not an audit trail.
CREATE TABLE iam_removed (
    person_id       TEXT    NOT NULL PRIMARY KEY,
    at              INTEGER NOT NULL,
    -- The record that removed them, by its OPERATION ID rather than its
    -- position: the apply that writes this row is the one that must be able
    -- to run twice, and a position changes under a republish where an
    -- operation id does not.
    record_id       TEXT    NOT NULL DEFAULT '',
    actor           TEXT    NOT NULL DEFAULT '',
    actor_kind      TEXT    NOT NULL DEFAULT '',
    reason          TEXT    NOT NULL DEFAULT '',
    -- The directory values the removal RELEASED — the address blind, the
    -- login and the seat — as a JSON object, so an operator asking "who held
    -- this" after the fact has an answer. BLINDS AND NEVER ADDRESSES: writing
    -- an address in the clear into the one row designed to outlive the
    -- person would break the removal's promise by the mechanism that makes it.
    claims_json     TEXT    NOT NULL DEFAULT '{}',
    bucket          INTEGER NOT NULL DEFAULT 0,
    version         INTEGER NOT NULL,
    -- WHAT STOPS THE TOMBSTONE SPEAKING FOR ITS SEAT. A removal frees the
    -- seat the person held, which then reads as held by nobody — and routes
    -- by the chart's contact map, which still names the leaver's own
    -- accounts — so the tombstone is what withholds the seat. Its evidence is
    -- about the binding it RELEASED, and the FIRST bind of that seat after
    -- the removal is a later binding with a standing of its own: that bind
    -- stamps its operation id here, and nothing after it does, so every node
    -- holds the same id. Empty is the ordinary state — a seat nobody has
    -- taken since — rather than a missing value.
    seat_rebound_by TEXT    NOT NULL DEFAULT ''
);
-- The gate's own read is by person_id and is served by the primary key. This
-- index is for the OTHER reader: the operator surface asking who left recently,
-- and the per-bucket sweep that has a horizon to range over.
CREATE INDEX iam_removed_sweep_idx ON iam_removed (bucket, at);                             -- who left, per bucket, by age

-- ===========================================================================
-- # A LEDGER RECORDS A LOSS PER SUBJECT KIND
--
-- 0017 gave every operation ledger ONE watermark: the instant before which it
-- may have lost rows. The publisher reads it before it decides anything, and
-- an operation minted before it is vouched for only by its own row — absence
-- there may be a row the sweep took, so the write is answered `unknown`
-- without publishing rather than decided a second time.
--
-- The identity estate's ledger sweeps a session subject's rows after an hour
-- and everything else's after the month. Moving 0017's one watermark on that
-- hourly sweep would say the WHOLE ledger lost everything older than an hour
-- — and every identity operation minted before that (an invitation redeemed
-- under an id derived from its issue, a create retried under its operation
-- key) would be answered `unknown` without ever being published. Not moving it
-- would let a session operation whose row the hour took be decided again,
-- which is the double apply the watermark exists to prevent.
--
-- So one row per (ledger, subject kind) that a kind-specific sweep has lost
-- rows of, written in the very transaction that deletes them, and only ever
-- moved forward. What the publisher weighs for an operation is the later of
-- the table's watermark and its subject kind's — so a session operation is held
-- to the hour and every other kind to the month. IT TRAVELS AS 0017'S DOES: a
-- snapshot carries the replicated file, so an adopter inherits how far back
-- each kind of the donor's ledger may have lost rows along with the rows
-- themselves. An absent row means that kind lost nothing beyond the table-wide
-- watermark.
-- ===========================================================================
CREATE TABLE statelog_ops_lost_kind (
    ops_table    TEXT    NOT NULL,
    subject_kind TEXT    NOT NULL,
    lost_before  INTEGER NOT NULL,
    PRIMARY KEY (ops_table, subject_kind)
);

-- ===========================================================================
-- # THE TRACKER
--
-- 0002 IS NOT EDITED. `schema_migrations` keys on the filename, so a file
-- that has already run never runs again: a fresh database and an upgraded one
-- converge here, by the same route.
--
-- Every column dropped below is dropped in the change that stops the applier
-- naming it: an insert naming a column that is gone fails the apply on every
-- node at once, which in a derived estate is a stalled log rather than one bad
-- row. No index names any of them.
-- ===========================================================================

-- ## A project is stamped with the ACTIVATION its chart-owned fields came from
--
-- `tracker_projects.chart_epoch` held a WALL-CLOCK READING in seconds, taken
-- on whichever node was applying. It now holds the instant the revision the
-- fields were derived from was ACTIVATED, in Unix milliseconds
-- (`configplane.ActivationStamp`) — the instant on the fleet's activation
-- pointer, which every node reads and which a later activation always carries
-- later (`coord.ActivationAt`) — so an older configuration cannot walk a newer
-- one's fields back, during a rollout as much as anywhere.
--
-- DROPPED AND ADDED AGAIN rather than kept, because the values do not carry: a
-- reading in seconds would sit in a column that means milliseconds. Every row
-- starts at 0, which every activation outranks, so the first apply after this
-- lands re-stamps each project once.
ALTER TABLE tracker_projects DROP COLUMN chart_epoch;
ALTER TABLE tracker_projects ADD COLUMN chart_epoch INTEGER NOT NULL DEFAULT 0;

-- ## A checklist item keeps no promotion
--
-- `tracker_checklist_items.promoted_to` kept an item that had been turned into
-- a subtask, pointing at the subtask it became. Its only writer, the
-- tracker's item promotion, never had a caller — no tool, no route, no duty,
-- no CLI — so every row anybody could write held NULL here, and it went with
-- the promotion. THE TABLE STAYS, and so do its two indexes: they are what the
-- checklist's own readers read, and none of them ever named this column.
ALTER TABLE tracker_checklist_items DROP COLUMN promoted_to;

-- ## A history row names its author once
--
-- `tracker_history.actor_seat` (0026) carried the seat a Tier A token was
-- bound to through `contact.crewlet_operator_id`, so a card could show the
-- person and the wake could leave them out of their own change. That binding
-- does not exist in this build: a principal's author is `iam.ActorFor`'s
-- answer, and for a person bound to a seat — through the identity directory,
-- whichever credential they presented — the author IS the seat: `actor` is
-- the seat, `actor_kind` is `human`, and `operator_id` is the credential the
-- write went through. A second copy of the author beside the first is a value
-- two readers could each take as the one that counts.
ALTER TABLE tracker_history DROP COLUMN actor_seat;

-- ## A notice names the TASK it is about, beside the subject it was written on
--
-- A notice stores `subject_id` (the object its record changed) and
-- `subject_key` (the key a reader follows). For a task commit the two describe
-- one task. For the one routable wake that is not a task commit — a lead
-- putting a task at the top of somebody's priorities — the subject is the
-- PERSON and the key is the TASK's, so the row held the task's key and no
-- task id at all. That matters the moment two tasks hold one key: the key
-- opens the task that claimed it first, so a notice must be able to say "this
-- key does not open the task I mean, and here is the id that does".
--
-- `task_id` is the wake's own `Snapshot.TaskID`, written by the tracker's
-- applier: the subject for a task commit, the task the snapshot names for a
-- person's priorities, and empty for a wake that names no task — "names no
-- task" is a state rather than a missing value, so NOT NULL with a default. A
-- row written before this column holds that empty string, which reads as "the
-- key is the only address", exactly what every notice was before it. No
-- index: the column is read only beside the row the inbox range already
-- reaches.
ALTER TABLE tracker_notifications ADD COLUMN task_id TEXT NOT NULL DEFAULT '';

-- ## A task filed behind a cross-project move that has finished is found
--
-- A move re-keys its root, marks it mid-move and walks the subtree it read
-- before its first append; its last append takes the mark down only in a
-- snapshot where nothing under the root is outside the root's project. That
-- covers every create a node decides once it has applied the root's move, and
-- every create whose record lands before the walk's last append. It does not
-- cover a create DECIDED on a node that had not applied the root's move yet,
-- whose record the broker accepted AFTER the mark came down: the applier files
-- it under its parent, now in the new project, flags it `inconsistent_project`
-- and moves on — a committed record is never refused there, since refusing
-- would stall every node on it — and with the mark down, nothing would look at
-- that root again.
--
-- The tracker duty's `tracker_move_stragglers` job is what finds it: its gate
-- asks whether any live task is flagged, and its walk selects the ROOTS those
-- tasks sit under, in root order, and carries each root's stragglers into its
-- project under the move's own claim. Partial on exactly the predicate both
-- statements spell out — this engine's planner does not infer a partial
-- index's WHERE from a query that omits it — so a company where every subtree
-- lives in one project pays one probe that touches no row, and the walk reads
-- the flagged rows already in root order rather than sorting them. A task in
-- the TRASH is left out: it takes no write until somebody restores it, and its
-- restore clears `removed_at`, which brings it into the index.
CREATE INDEX tracker_tasks_stranded_idx ON tracker_tasks (root_id)
    WHERE inconsistent_project = 1 AND removed_at IS NULL;
