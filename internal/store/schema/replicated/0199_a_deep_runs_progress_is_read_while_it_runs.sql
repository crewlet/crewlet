-- A deep run's progress lines, readable from any node while the run is in flight (ADR-0166).
-- WHO HAS TO AGREE ON IT: every data node, identically; written by the advisor applier from a run's progress records, which only a build at record version 2 (advisordomain.RecordVersion) applies, so a build before it retains them. Removed with the runs past their retention.
CREATE TABLE advisor_run_progress (
    run_id   TEXT    NOT NULL,
    at       INTEGER NOT NULL,
    text     TEXT    NOT NULL,
    position INTEGER NOT NULL,
    PRIMARY KEY (run_id, position)
);
CREATE INDEX advisor_run_progress_at_idx ON advisor_run_progress (at, position);
