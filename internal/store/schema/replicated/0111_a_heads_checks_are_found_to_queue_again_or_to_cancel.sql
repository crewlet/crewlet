-- A head's checks are found to queue again or to cancel.
--
-- A repository's home git node owes a live change's head more than its
-- dispatch (internal/code's checksdispatch.go): the NEXT attempt of a check
-- whose latest attempt at the head, for the change's target, its
-- infrastructure ended — the runner lost, the box or its setup failed
-- (code.CheckReason.Requeued) — while that attempt is below
-- code.AutoRequeueAttempts; and, when it dispatches a head, the CANCELLATION
-- of the change's attempts still queued or running at a head it moved on
-- from, or for a target it was retargeted from, which nobody merges.
--
-- code_checks_requeue_idx is a repository's attempts at a commit, for a
-- target, that ended errored for their infrastructure, PARTIAL on those rows
-- alone: the home's `checks_due` listing asks of every live change's head
-- whether its latest attempt of a check is one it queues again, and this is
-- one seek of an index that holds only those, where code_checks' own key
-- would read every attempt of every check of the head.
--
-- code_checks_live_idx is a change's attempts of its head's checks that have
-- not ended, by the change, PARTIAL on those rows alone: a dispatch reads the
-- attempts its change left behind, one seek of an index holding only what
-- still waits for or holds a runner. An attempt carries the change whose
-- dispatch queued it (code_checks.change_id, a head check's alone), which no
-- other index leads with: replicated 0108 dropped code_checks_change_idx,
-- every attempt any change ever queued, which no read of a change's checks
-- uses — a change's checks are read on code_checks' key, by its head and its
-- target.
--
-- Both are created on the table 0108 made, and keyed in its key's order.
--
-- Class: code_checks stays reproducible; the indexes are the applier's rows.
CREATE INDEX code_checks_requeue_idx ON code_checks (repo_id, commit_oid, target_ref, name, attempt)
    WHERE state = 'errored' AND reason IN ('runner_lost', 'box_failed', 'setup_failed');    -- a head's attempts its infrastructure ended: the home's requeue

CREATE INDEX code_checks_live_idx ON code_checks (change_id, commit_oid, target_ref, name, attempt)
    WHERE state IN ('queued', 'running') AND purpose = 'head';    -- a change's head attempts not yet ended: the home's cancellation of a head its change moved on from
