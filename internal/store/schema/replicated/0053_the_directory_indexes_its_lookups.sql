-- `iam_people`'s three duplicate-report indexes go, and the two lookups the
-- directory makes get plain ones.
--
-- 0034 shipped three PARTIAL, non-unique indexes — over the address blind, the
-- login and the seat id, each `WHERE <column> != ''` — for a duty that walked
-- them hourly to REPORT a value two people held. Every address, login and seat
-- is now decided on the directory's one subject against every row, so ordinary
-- traffic cannot put one value on two rows, and the report and its duty are
-- gone: a lookup that meets two rows answers that this node cannot say, and
-- that needs no walk.
--
-- WHAT IS LEFT IS LOOKUP. A sign-in resolves a login or an address blind to
-- its row, and a directory decision asks whether anybody else holds the one it
-- is about to give — `WHERE login = ?` and `WHERE email_blind = ?` with a bound
-- value. A partial index is usable for a query only where the query's own
-- WHERE implies the index's, and a bound parameter implies nothing about being
-- non-empty, so the partial indexes are replaced by plain ones over each
-- column. The seat already has one (0034's `iam_people_seat_lookup_idx`).
--
-- STILL NOT UNIQUE, for 0034's reason: a violation inside an apply aborts it
-- on every node at once, and two rows holding one value — a restore's, or a
-- retained record's — is an anomaly an operator repairs, never an outage.
DROP INDEX iam_people_email_claim_idx;
DROP INDEX iam_people_login_claim_idx;
DROP INDEX iam_people_seat_claim_idx;
CREATE INDEX iam_people_login_idx ON iam_people (login);                 -- a login to its holder
CREATE INDEX iam_people_email_idx ON iam_people (email_blind);           -- an address blind to its holder
