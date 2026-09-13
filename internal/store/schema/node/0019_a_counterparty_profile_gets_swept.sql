-- counterparty_profiles has a retention now, so it needs the index a range
-- delete reads.
--
-- The table was `wholeEachCycle` in memsync with no cap, no TTL and no entry
-- in the maintenance sweep: one row per distinct human or seat a seat has
-- ever messaged, republished for every held seat every 30 s, growing for the
-- life of the deployment. Every other table with a documented retention ships
-- the index its sweep needs; this one had no sweep to need one.
--
-- last_updated_at rather than last_corroborated_at: the sweep drops a profile
-- nobody has INTERACTED with, and last_updated_at moves on every upsert
-- including the no-ops, so it is the true last-contact stamp. Corroboration
-- measures trait CHANGE, which a long relationship can go without.
CREATE INDEX IF NOT EXISTS counterparty_profiles_last_updated_idx
    ON counterparty_profiles (last_updated_at);
