-- A deep run's progress line says what step it is (a read, a verdict, a question, a press's checks, a note), whether it failed, what it asked and why it failed, so a run reads as its steps (ADR-0166).
-- WHO HAS TO AGREE ON IT: every data node, identically; written by the advisor applier from a run's progress records, and only a record at version 4 (advisordomain.RecordVersion) carries them, so a build before it retains such a record rather than applies it.
-- '' and 0 are a line whose record carried none: every line before these columns, which reads as a note that did not fail.
ALTER TABLE advisor_run_progress ADD COLUMN step TEXT NOT NULL DEFAULT '';
ALTER TABLE advisor_run_progress ADD COLUMN failed INTEGER NOT NULL DEFAULT 0;
ALTER TABLE advisor_run_progress ADD COLUMN detail TEXT NOT NULL DEFAULT '';
ALTER TABLE advisor_run_progress ADD COLUMN why TEXT NOT NULL DEFAULT '';
