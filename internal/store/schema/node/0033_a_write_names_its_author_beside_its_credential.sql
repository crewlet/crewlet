-- A configuration revision and a stored secret name their AUTHOR beside the
-- CREDENTIAL that wrote them.
--
-- Both trails had room for one name, and it was the credential: `created_by`
-- and `updated_by` held a Tier A token's id, a signed-in person's login — and,
-- for a person writing through their own machine token, `pat:<credential id>`.
-- The token's row is the only thing that says whose it was, and the identity
-- sweep collects it a week after it expires or is revoked; a revision is kept
-- for ever. So a year on, `created_by` named a credential nobody could resolve
-- to anybody. Every other trail in the engine — a work item, a page, a chart
-- change, the identity log — records the two apart: the author whose authority
-- was exercised, and the credential it was exercised through.
--
-- So these two do as well. `created_by` / `updated_by` become the AUTHOR —
-- internal/iam's ActorFor name: a seat's handle for a person bound to one, a
-- login otherwise, `token:<id>` for a Tier A token, the node for the engine's
-- own writes — and beside each:
--
--   * `…_kind` — the author's KIND (agent, human, operator, system), which is
--     what tells a seat a person is bound to from an agent's seat with the
--     same handle, and which the dashboard's audit view used to guess as
--     `operator` for every row;
--   * `operator_id` — the credential the write was made through, EMPTY for a
--     write no credential made: the engine's own, and a command run on the
--     host while the engine was stopped.
--
-- EMPTY ON EVERY EARLIER ROW, which is the truth: nothing recorded either fact
-- for them, and a backfill could only guess.
ALTER TABLE company_config ADD COLUMN created_by_kind TEXT NOT NULL DEFAULT '';
ALTER TABLE company_config ADD COLUMN operator_id TEXT NOT NULL DEFAULT '';

-- secret_values is this node's own bootstrap half of the secret store, migrated
-- onto the fleet's at the next start — which carries every provenance column
-- across, so the three facts travel with the value rather than being
-- re-derived by the node that moved it.
ALTER TABLE secret_values ADD COLUMN updated_by_kind TEXT NOT NULL DEFAULT '';
ALTER TABLE secret_values ADD COLUMN operator_id TEXT NOT NULL DEFAULT '';

-- NO INDEX on any of the four. Nothing selects on them: each is read one row
-- at a time beside a revision or a secret somebody already named, and written
-- once per write.
