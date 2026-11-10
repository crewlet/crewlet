-- A memory note says what it is about, and which engine build wrote it.
--
-- `provenance` is a closed class: `platform` (how the engine itself works),
-- `capability` (what the seat can reach or run), `company` (how the company
-- works) or `preference` (how a person likes to work). `engine_build` is the
-- build that wrote the note. The turn-start prefetch holds back a platform
-- note another build wrote, and a capability note written before the seat's
-- latest told access change, until a turn keeps the fact again. A note written
-- before this migration reads as `company`, which nothing holds back.
--
-- WHO HAS TO AGREE ON IT: whichever node holds the seat, as every diary column;
-- memsync carries both columns with the row.
ALTER TABLE agent_diary ADD COLUMN provenance TEXT NOT NULL DEFAULT 'company';
ALTER TABLE agent_diary ADD COLUMN engine_build TEXT NOT NULL DEFAULT '';
