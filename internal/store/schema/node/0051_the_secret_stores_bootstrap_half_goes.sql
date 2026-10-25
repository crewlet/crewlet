-- The secret store's bootstrap half goes.
--
-- `secret_values` held what `crewlet secrets set`, `crewlet llm login
-- -capture-token` and a provisioner's `-secret-store` sink wrote while the
-- engine was stopped, and the engine carried those rows onto the coordination
-- store at its next start. The company's live values were never here, and that
-- was the defect: with the engine stopped a command READ this table too, so a
-- value stored through a running node — every rotation on a live fleet — read
-- as unset, which a provisioner takes as the signal to mint over a working
-- credential.
--
-- A command now reaches the fleet's store itself while the engine is stopped
-- (cmd/crewlet's fleetstore.go, over internal/engine's OpenFleetStore), so
-- nothing writes this table, the boot migration that read it is gone, and the
-- table is dropped rather than kept as an invitation to wire something to it.
-- Whatever an earlier build left in it goes with it: no release ever wrote a
-- row here.

DROP INDEX IF EXISTS secret_values_updated_at_idx;
DROP TABLE IF EXISTS secret_values;
