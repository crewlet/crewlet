-- The catalogue's RELATIONAL copy goes: three tables written on every
-- catalogue apply, on every node, and SELECTed by nothing.
--
-- # What was there
--
-- `tracker_types`, `tracker_fields` and `tracker_field_options` exploded the
-- type and field catalogues into rows the way every other collection this
-- domain carries is exploded — watchers, relations, tags. The difference is
-- that those have readers: a filter seeks them. A catalogue does not. Every
-- reader of a declaration goes through the DOCUMENT (`readFieldCatalogue`,
-- `readProject`), which is one row and one decode, because a company has tens
-- of fields rather than thousands and the whole set is wanted at once.
--
-- So the rows were a delete-and-rewrite of the entire catalogue per apply, per
-- node, with four indexes maintained over them, to answer no question.
--
-- # The column that gives it away
--
-- `tracker_fields.shadowed` was INSERTed as a literal `0` on every row, and
-- its own comment in 0002 says it is "what a reader sees meanwhile" during a
-- move between scopes. The reader that reports shadowing derives it in Go from
-- the two declaration documents (`groupFields`), so the column was a fact
-- about the data that nothing computed and nothing read — the clearest
-- possible statement that the rows were not being used.
--
-- # What stays
--
-- `tracker_field_values` stays: it is the per-task VALUE, which every `f.<ref>`
-- filter seeks and every total aggregates. Its `hidden` column is still
-- settled from the declaration on every catalogue apply — that is a fact about
-- a row a query reads, and it is the half of the explosion that was load-
-- bearing all along.
--
-- 0002 IS NOT EDITED. `schema_migrations` keys on the filename, so a file that
-- has already run never runs again: a fresh database and an upgraded one
-- converge here, by the same route.

-- The indexes go with their tables; naming them is belt-and-braces for an
-- engine that would keep an index over a dropped table.
DROP INDEX IF EXISTS tracker_fields_scope_idx;
DROP INDEX IF EXISTS tracker_fields_slug_idx;
DROP INDEX IF EXISTS tracker_fields_name_idx;
DROP INDEX IF EXISTS tracker_fields_id_idx;
DROP INDEX IF EXISTS tracker_field_options_slug_idx;
DROP INDEX IF EXISTS tracker_field_options_name_idx;

DROP TABLE IF EXISTS tracker_field_options;
DROP TABLE IF EXISTS tracker_fields;
DROP TABLE IF EXISTS tracker_types;
