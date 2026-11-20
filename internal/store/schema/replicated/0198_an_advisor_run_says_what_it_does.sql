-- An Advisor run says what it does: a run of its rules, or an investigation on its model (ADR-0166).
-- WHO HAS TO AGREE ON IT: every data node, identically; written by the advisor applier from a run's request and its record, and only a record at version 2 (advisordomain.RecordVersion) carries it, so a build before it retains such a record rather than applies it.
-- '' is a run whose records carried no kind: every run before this column, which ran its rules.
ALTER TABLE advisor_runs ADD COLUMN kind TEXT NOT NULL DEFAULT '';
