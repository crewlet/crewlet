-- What this node keeps about WHO: a write names its author beside the
-- credential it went through, an event's parties are seats by their agent id,
-- and an adoption stops recording a fold nothing performs.
--
-- # A configuration revision and a stored secret name their author and their
--   credential apart
--
-- Both trails had room for one name, and it was the credential: `created_by`
-- and `updated_by` held a Tier A token's id, a signed-in person's login — and,
-- for a person writing through their own machine token, `pat:<credential id>`.
-- The token's row is the only thing that says whose it was, and the identity
-- sweep collects it a week after it expires or is revoked; a revision is kept
-- for ever. So a year on, `created_by` would name a credential nobody could
-- resolve to anybody. Every other trail in the engine — a work item, a page,
-- the identity log — records the two apart: the author whose authority was
-- exercised, and the credential it was exercised through.
--
-- So these two do as well. `created_by` / `updated_by` are the AUTHOR —
-- internal/iam's ActorFor name: a seat's handle for a person bound to one, a
-- login otherwise, `token:<id>` for a Tier A token, the node for the engine's
-- own writes — and beside each:
--
--   * `…_kind` — the author's KIND, an `iam.ActorKind` (agent, human,
--     operator, system), which tells a seat a person is bound to from an
--     agent's seat with the same handle. `company_config.created_by_kind` is
--     0035's column, under the same name and the same `NOT NULL DEFAULT ''`;
--     what this build writes into it is the ActorFor kind, a wider vocabulary
--     than the `operator`/`node` that 0035's header describes;
--   * `operator_id` — the credential the write was made through, EMPTY for a
--     write no credential made: the engine's own, and a command run on the
--     host while the engine was stopped.
--
-- The new columns are EMPTY ON EVERY EARLIER ROW, which is the truth: nothing
-- recorded those facts for them, and a backfill could only guess.
--
-- secret_values is this node's own bootstrap half of the secret store, migrated
-- onto the fleet's at the next start — which carries every provenance column
-- across, so the three facts travel with the value rather than being
-- re-derived by the node that moved it.
--
-- NO INDEX on any of them, 0035's column included. Nothing selects on them:
-- each is read one row at a time beside a revision or a secret somebody
-- already named, and written once per write.
ALTER TABLE company_config ADD COLUMN operator_id TEXT NOT NULL DEFAULT '';
ALTER TABLE secret_values ADD COLUMN updated_by_kind TEXT NOT NULL DEFAULT '';
ALTER TABLE secret_values ADD COLUMN operator_id TEXT NOT NULL DEFAULT '';

-- # The related-agent index is keyed on agent ids, never names
--
-- 0016 made "every event involving this agent" an index seek by writing one
-- `crewlet_event_parties` row per (party, event). What it wrote as a party was
-- a NAME: the event's actor, its `agent_role` tag — a seat's display name —
-- and the `target`, `recipient` and `sender` tags as they came. A seat's name
-- is prose two seats may share, so `GET /events?agent=` listed every
-- namesake's work as one seat's; and an external sender who happened to share
-- a seat's handle was filed under that seat.
--
-- A party is now an AGENT ID, derived from the seat's handle (ADR-0013): the
-- event's own `agent_id`, and each seat a participant tag names, resolved
-- through the organisation by the process that writes the event — see
-- internal/store's `partiesOf`. The filter's `agent` parameter is resolved to
-- an agent id the same way before it is compared.
--
-- Every row this table held is a name, and no id will ever equal one: kept,
-- they would be dead weight on the index for the rest of the retention
-- window, answering nothing. They are replaced from the one party the log
-- carries AS an id — each event's own `agent_id` — so an agent's own history
-- stays in the filter. The participant tags cannot be carried over: resolving
-- a handle to an agent id needs the company's name and the seat each handle
-- named when the event was written, and neither is in this database. An A2A
-- counterpart or a delivery's recipient named before this migration drops out
-- of the related view for the rest of the thirty-day window; everything
-- written after it is filed under both ends.
DELETE FROM crewlet_event_parties;

INSERT INTO crewlet_event_parties (party, event_time, event_id)
SELECT agent_id, event_time, event_id FROM crewlet_events WHERE agent_id <> ''
ON CONFLICT (party, event_time, event_id) DO NOTHING;

-- # `statelog_adoption.ledger_folded` goes
--
-- 0030 added it so the engine could fold, once, every adoption recorded by a
-- build from before the operation ledger travelled into the ledger's
-- watermark: such an adoption installed a file whose ledger its donor had
-- scrubbed. No build that scrubbed a ledger was ever released, so there is no
-- such adoption to fold, and the fold and the column that marked its progress
-- go together. There is no data to migrate — the row is this node's history,
-- and the column said nothing a reader uses.
ALTER TABLE statelog_adoption DROP COLUMN ledger_folded;
