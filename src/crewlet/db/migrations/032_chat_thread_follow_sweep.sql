-- chat_thread_follows — the index its retention sweep needs.
--
-- The table had no purge at all until now: the repository exposed only
-- `upsert` and `is_following`, and the MaintenanceWorker never named it,
-- so follow rows accumulated for the life of the deployment on a table
-- read on the hot path of every inbound chat message.
--
-- Retention is a range delete over `updated_at`, which every re-assert
-- (mention, collective address, the agent's own post) refreshes — so it
-- means last-activity, not creation. Without this index that delete
-- degrades to a full scan, which is the same shape 028_turn_completions
-- ships `turn_completions_completed_at_idx` for.

CREATE INDEX IF NOT EXISTS chat_thread_follows_updated_at_idx
    ON chat_thread_follows (updated_at);
