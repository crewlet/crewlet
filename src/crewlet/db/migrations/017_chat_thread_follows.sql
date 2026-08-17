-- chat_thread_follows — thread-follow state for EVERY chat backend.
--
-- Generalises `slack_thread_follows` (001_initial) now that Slack is one
-- chat backend among several rather than the chat backend.  The follow
-- model itself is unchanged and transfers 1:1: an agent follows a thread
-- identified by (channel, thread), and top-level messages always deliver.
-- What changes is that the thread key is no longer necessarily a Slack
-- `ts`, so the column is `thread_id` and the row carries the `backend`
-- that minted it.
--
-- `backend` is part of the PRIMARY KEY rather than a tag, because thread
-- ids are only unique *within* a backend: a Mattermost root post id and a
-- Slack `thread_ts` are drawn from different namespaces and could collide
-- for the same agent.  Without it in the key, one backend's follow row
-- could satisfy another backend's lookup.
--
-- Rename-in-place rather than create-and-copy: the table is small but the
-- state is load-bearing (losing it makes every agent go silent in threads
-- it was following until something re-triggers a follow), and a rename
-- carries the rows across with no window in which they are absent.
--
-- The backfill default is dropped immediately after it is applied.  A
-- lingering DEFAULT 'slack' would mean a future INSERT that forgets the
-- backend silently lands in Slack's namespace — the exact class of bug
-- putting `backend` in the key is meant to prevent.

ALTER TABLE slack_thread_follows RENAME TO chat_thread_follows;

ALTER TABLE chat_thread_follows RENAME COLUMN thread_ts TO thread_id;

ALTER TABLE chat_thread_follows
    ADD COLUMN backend TEXT NOT NULL DEFAULT 'slack';

ALTER TABLE chat_thread_follows
    ALTER COLUMN backend DROP DEFAULT;

ALTER TABLE chat_thread_follows
    DROP CONSTRAINT slack_thread_follows_pkey;

ALTER TABLE chat_thread_follows
    ADD PRIMARY KEY (backend, agent_handle, channel_id, thread_id);

DROP INDEX IF EXISTS idx_slack_thread_follows_thread;

CREATE INDEX idx_chat_thread_follows_thread
    ON chat_thread_follows(backend, channel_id, thread_id);
