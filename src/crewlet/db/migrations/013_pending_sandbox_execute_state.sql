-- Code-runtime sandbox-as-a-tool: persist the suspended Execute-loop state.
--
-- The sandbox is now a TOOL the Execute LLM calls (run_sandbox), not a
-- whole-phase backend. When the executor calls it, the Execute tool-loop
-- SUSPENDS: the conversation (the message list, incl. the assistant turn
-- whose run_sandbox tool_use is still unanswered) plus a little surface
-- bookkeeping is persisted here, the turn ends, and when the detached
-- coding run completes the engine RESUMES the loop — appending the run's
-- result as that tool call's reply and re-invoking the executor with full
-- context.
--
-- Stored as one JSONB blob (extensible, no per-field columns):
--   messages          -- [Message.model_dump(mode="json")]
--   pending_tool_call_id / pending_tool_name -- the dangling run_sandbox call
--   active_tool_names  -- surface.names at suspend (replayed on resume)
--   loaded_skill_keys  -- skill-guard loaded set (replayed on resume)
--   iteration          -- turn.iteration at suspend (label continuity)
--   input_tokens / output_tokens -- Execute partials (observability)
--
-- Empty ('{}') for the legacy execution_backend path, which dispatches a
-- fresh completion turn instead of resuming the loop.

ALTER TABLE pending_sandbox_run
    ADD COLUMN execute_state JSONB NOT NULL DEFAULT '{}'::jsonb;
