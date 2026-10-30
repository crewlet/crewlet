-- A field's declaration settles its value rows through an index of each side.
--
-- # tracker_field_values_hidden_idx
--
-- Every catalogue apply makes every field's value rows agree with its
-- declaration — hidden when the field is archived or does not apply to the
-- task's type, shown otherwise — and it did so with one statement per field
-- whose WHERE was `hidden <> <what the declaration says>`. No index serves
-- that, since a row on either side may be the one that moves, so each
-- catalogue change read every value row in the company from the heap, once
-- per field the company declares, inside the applier's transaction.
--
-- The settle now writes each side on its own (`settleStatements` in
-- internal/worklet/apply_task.go). The visible side already has four indexes
-- that hold it whole — 0002's value indexes are partial on `hidden = 0` and
-- nothing else — and this is the hidden side's: partial on `hidden = 1`, so it
-- is the size of what the company's declarations hide, and a visible value,
-- which is nearly every value written, writes nothing to it.
CREATE INDEX tracker_field_values_hidden_idx ON tracker_field_values (field_id)
    WHERE hidden = 1;                                                                                         -- the settle's hidden side: what a declaration shows again
