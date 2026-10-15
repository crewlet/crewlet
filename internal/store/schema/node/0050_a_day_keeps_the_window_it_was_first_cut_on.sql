-- usage_day_windows — the instants each of this node's company days is cut on,
-- recorded the first time its usage publisher cut it.
--
-- WHO HAS TO AGREE ON IT: this node alone — the cut its own publisher put its
-- days on. Only this node derives and publishes its days (internal/usage,
-- ADR-0020); what the fleet agrees on is the record it publishes under each
-- day's LABEL, which is the usage domain's.
--
-- # Why a day's window is recorded rather than recomputed
--
-- A record names its day by label alone (`2026-09-23`), and the label is cut
-- into instants on the company's clock — which an apply can move. Before this
-- table a day older than yesterday was never derived again, so a cut that
-- changed with the clock touched only today and yesterday, once. Now any older
-- day inside the event log's read floor is derived again — a kept custody
-- settle marks it, a restart's watermark names it — and recut on the clock in
-- force NOW, each label replaced the record its old cut published while the
-- objects the new cut no longer reached kept theirs: a seat's row two hours
-- either side of midnight was then counted in two published days, or in none.
--
-- So the first cut of a label is recorded, and every later derivation of that
-- label — the two days a tick fingerprints and every older one — uses it. A
-- label cut for the first time ABUTS its adjacent labels' recorded cuts: it
-- starts where the day before it ended and ends where the day after it starts,
-- so the day a clock moved on is that much longer or shorter. And it is CLAMPED
-- by a farther recorded cut that reaches into it — a move of more than a day
-- leaves the label between uncut — while one that ends before it starts is an
-- outage's gap and leaves it alone. So no instant falls in two days or in none.
-- The publisher is the only writer, one tick at a time.
--
-- Swept with the event log (`EventLog.Purge`), on its retention: no day older
-- than the read floor is ever derived again, so its cut answers nothing.
CREATE TABLE usage_day_windows (
    label    TEXT    NOT NULL PRIMARY KEY,
    start_at INTEGER NOT NULL,
    end_at   INTEGER NOT NULL,
    CHECK (start_at < end_at)
);

-- The day an instant falls in, sought by its start. The table holds about a
-- month of rows, so the sweep's range needs no index of its own.
CREATE INDEX usage_day_windows_start_idx ON usage_day_windows (start_at, end_at);
