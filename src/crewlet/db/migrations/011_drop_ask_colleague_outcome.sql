-- Drop the ``ask_colleague`` review outcome.
--
-- The engine no longer has an ``ask_colleague`` Review decision (nor
-- the Plan ``delegate`` decision or its handoff dispatcher).  Agents
-- now hand off / escalate by calling the colleague-surface tools
-- (Slack, Jira, Confluence, GitHub, A2A) directly during Execute, so a
-- turn that asks for help simply ends ``done``.
--
-- ``review_outcome`` is a plain TEXT column (no DB CHECK constraint);
-- the allowed values are enforced at the application layer by
-- ``ReviewOutcomeStr``.  Existing episode rows that recorded the
-- ``ask_colleague`` outcome are remapped to ``self_iterate`` -- the
-- bucket ``ask_colleague`` always shared in the learning subsystem
-- (both are non-terminal mid-states: excluded from skill synthesis by
-- the reflect-engine terminal-outcome gate and swept by the
-- non-terminal cleanup).  Recall and lifecycle behaviour are therefore
-- unchanged, and the tightened ``ReviewOutcomeStr`` Literal no longer
-- raises when ``_row_to_episode`` reads a legacy row.

UPDATE episodes SET review_outcome = 'self_iterate' WHERE review_outcome = 'ask_colleague';
