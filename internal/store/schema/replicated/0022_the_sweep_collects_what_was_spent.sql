-- The indexes a version-2 identity sweep record seeks on.
--
-- Version 1 of the sweep collected what LAPSED — an invitation or a bootstrap
-- code past its expiry and never redeemed, a session ended or past its deadline
-- — and kept what was SPENT for ever: every redeemed invitation, with the
-- sealed address it was for, every redeemed bootstrap code, and every revoked
-- or expired credential, verifier and all. Version 2 collects those a week past
-- the moment they stopped being presentable (`internal/iamdomain`,
-- `SweepRecordVersion`), and each of its statements is a seek into one bucket's
-- range on an index shaped like its own predicate, for the reason 0019 gives
-- for the open ones: a sweep that scanned would hold this store's only writer
-- for the length of a table.
--
-- PARTIAL, like 0019's: a row that is not spent has no business in the index
-- the collection of spent rows seeks on, and an index over thousands of zeros
-- is a scan wearing an index's name. The two credential indexes are separate
-- because a credential is collected for EITHER reason and one index over both
-- columns serves only the one it leads with.
CREATE INDEX iam_invites_spent_idx ON iam_invites (bucket, redeemed_at) WHERE redeemed_at > 0;                  -- the redeemed invitations, per bucket
CREATE INDEX iam_bootstrap_codes_spent_idx ON iam_bootstrap_codes (bucket, redeemed_at) WHERE redeemed_at > 0;  -- the redeemed bootstrap codes, per bucket
CREATE INDEX iam_credentials_revoked_idx ON iam_credentials (bucket, revoked_at) WHERE revoked_at > 0;          -- the revoked credentials, per bucket
CREATE INDEX iam_credentials_expiring_idx ON iam_credentials (bucket, expires_at) WHERE expires_at > 0;         -- the credentials with an expiry, per bucket
