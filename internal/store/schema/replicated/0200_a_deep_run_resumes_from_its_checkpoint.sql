-- A deep run's checkpoint, the newest it wrote: its agenda with each verdict and the reads it asked, which a holder taking the run over resumes from (ADR-0166).
-- WHO HAS TO AGREE ON IT: every data node, identically; written by the advisor applier from a run's progress records that carry a checkpoint, which only a build at record version 3 (advisordomain.RecordVersion) applies, so a build before it retains them. Removed with the runs past their retention.
CREATE TABLE advisor_run_checkpoint (
    run_id   TEXT    PRIMARY KEY,
    at       INTEGER NOT NULL,
    body     TEXT    NOT NULL,
    position INTEGER NOT NULL
);
CREATE INDEX advisor_run_checkpoint_at_idx ON advisor_run_checkpoint (at);
