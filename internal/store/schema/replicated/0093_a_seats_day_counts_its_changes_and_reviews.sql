-- A seat's day counts its changes and reviews.
--
-- A seat's day on a node (usage_turns, the head row 0023 made) gains what the
-- seat did on the code host: the changes it opened, the reviews it gave, and
-- the findings the reviews of its own changes raised — counted ON THE OWNER,
-- since a finding is work the change's owner received. Each is counted by the
-- node that landed the record, off the event its landing announced
-- (`change_opened`, `review_submitted`), into the seat record it publishes
-- (internal/usage, Record.Code, record version 3); a person's changes and
-- reviews are counted on no seat, since a person has no seat day.
--
-- ON THE HEAD ROW, never a table of their own: they are three figures of the
-- seat's day, replaced whole with it under the row's monotone version, and a
-- seat's activity answer reads them in the same seek as its turns. A record
-- that carries none writes zeros, which is what a day with none of them is.
--
-- Class: usage_turns stays DIVERGENT (0023): written only by applying a
-- committed record, and two nodes legitimately hold different days, so it is
-- in no identity claim.
ALTER TABLE usage_turns ADD COLUMN changes_opened    INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage_turns ADD COLUMN reviews_given     INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage_turns ADD COLUMN findings_received INTEGER NOT NULL DEFAULT 0;
