/**
 * The event log.
 *
 * Two things this screen gets right that its predecessor did not:
 *
 *  1. **The filter vocabulary is FIXED.** Category chips came from the live
 *     400-event ring, so chips appeared and vanished as it evicted — including
 *     the one you were reaching for — and an actor who had gone quiet could not
 *     be filtered to AT ALL, because no chip existed for them. The categories
 *     are a closed set the engine defines; the actor filter is a text box over
 *     the roster.
 *  2. **A row says WHO.** The old feed rendered a nub, a colour dot, a summary
 *     and a relative time, and offered actor filter chips for a field it never
 *     displayed.
 *
 * History paging asks the engine for older rows past the live ring. The cursor
 * is `before_time`+`before_id`, which is what the server actually reads — the
 * previous client sent `before`, so `time.Parse("")` failed and EVERY cursored
 * page was rejected, then read the rejection as "that is the beginning of the
 * retained history".
 */

import { useCallback, useMemo, useState } from "react";
import { ScreenHead } from "~/app/Shell.tsx";
import { useParam } from "~/app/router.tsx";
import { EventRow, QueryState } from "~/components/common.tsx";
import { Badge, Button, Chip, Panel, SearchInput, Skeleton } from "~/ui/primitives.tsx";
import { useClient, useEvents } from "~/lib/store-hooks.ts";
import { newestFirst, plural } from "~/lib/format.ts";
import type { FeedRow } from "~/protocol/index.ts";

/**
 * The categories the engine assigns, as a CLOSED set.
 *
 * Mirrors `internal/events/category.go`. A chip for a category with nothing in
 * it is still useful — it says the category exists and is quiet — which is the
 * opposite of a chip that vanishes because the ring evicted its last row.
 */
const CATEGORIES = [
  "lifecycle",
  "task",
  "communication",
  "decision",
  "knowledge",
  "learning",
  "a2a",
  "notification",
  "webhook",
  "system",
] as const;

const PAGE = 100;

export function Activity() {
  const { socket } = useClient();
  const liveEvents = useEvents();
  const [category, setCategory] = useParam("category", "");
  const [actor, setActor] = useParam("actor", "");
  const [q, setQ] = useParam("q", "");
  const [onlyFailed, setOnlyFailed] = useParam("failed", "");

  const [older, setOlder] = useState<FeedRow[]>([]);
  const [cursor, setCursor] = useState<{ before_time: string; before_id: string } | null>(null);
  const [exhausted, setExhausted] = useState(false);
  const [paging, setPaging] = useState(false);
  const [pageError, setPageError] = useState<string | null>(null);

  const rows = useMemo(() => {
    const seen = new Set<string>();
    const all = [...liveEvents, ...older].filter((e) => {
      if (seen.has(e.id)) return false;
      seen.add(e.id);
      return true;
    });
    const needle = q.trim().toLowerCase();
    return all
      .filter((e) => !category || e.category === category)
      .filter((e) => !actor || (e.actor ?? "").toLowerCase().includes(actor.toLowerCase()))
      .filter((e) => !onlyFailed || e.failed)
      .filter(
        (e) =>
          !needle ||
          (e.summary ?? "").toLowerCase().includes(needle) ||
          (e.type ?? "").toLowerCase().includes(needle) ||
          (e.source ?? "").toLowerCase().includes(needle),
      )
      .sort(newestFirst);
  }, [liveEvents, older, category, actor, q, onlyFailed]);

  const counts = useMemo(() => {
    const map = new Map<string, number>();
    for (const e of [...liveEvents, ...older]) {
      map.set(e.category, (map.get(e.category) ?? 0) + 1);
    }
    return map;
  }, [liveEvents, older]);

  const loadOlder = useCallback(async () => {
    setPaging(true);
    setPageError(null);
    try {
      // The cursor names BOTH halves. The engine reads `before_time` and
      // `before_id`; a client sending one bare `before` gets every page
      // rejected with `query_failed`.
      const params: Record<string, unknown> = { limit: PAGE };
      if (category) params.category = category;
      if (actor) params.actor = actor;
      if (cursor) {
        params.before_time = cursor.before_time;
        params.before_id = cursor.before_id;
      } else {
        const last = rows[rows.length - 1];
        if (last) {
          params.before_time = last.timestamp;
          params.before_id = last.id;
        }
      }
      // The answer is an OBJECT — `{events, next, exhausted}` — not a bare
      // array. Reading it as an array yielded [] every time, which the caller
      // then read as "the beginning of the retained history".
      const page = await socket.query("events", params);
      setOlder((prev) => [...prev, ...(page.events ?? [])]);
      setCursor(page.next ?? null);
      setExhausted(page.exhausted || !page.next);
    } catch (err) {
      setPageError(err instanceof Error ? err.message : "query_failed");
    } finally {
      setPaging(false);
    }
  }, [socket, cursor, rows, category, actor]);

  const filtered = !!(category || actor || q || onlyFailed);

  return (
    <>
      <ScreenHead
        title="Event log"
        sub="Everything the engine published, live and then paged out of the store. This tab holds the last 400 in memory; older rows are fetched."
        badges={<Badge outline>{plural(rows.length, "event")} shown</Badge>}
        actions={
          filtered ? (
            <Button
              icon="x"
              size="sm"
              onClick={() => {
                setCategory("");
                setActor("");
                setQ("");
                setOnlyFailed("");
              }}
            >
              Clear filters
            </Button>
          ) : undefined
        }
      />

      <div className="toolbar">
        <div style={{ maxWidth: 300, flex: 1 }}>
          <SearchInput
            value={q}
            onChange={setQ}
            ariaLabel="Search events"
            placeholder="Search summary, type or source"
          />
        </div>
        <div style={{ maxWidth: 180 }}>
          <SearchInput
            value={actor}
            onChange={setActor}
            ariaLabel="Filter by actor"
            placeholder="Actor"
          />
        </div>
        <Chip on={!!onlyFailed} onClick={() => setOnlyFailed(onlyFailed ? "" : "1")}>
          Failures only
        </Chip>
        <span className="spacer" />
      </div>

      <div className="row wrap gap-1">
        <Chip on={!category} onClick={() => setCategory("")}>
          All
        </Chip>
        {CATEGORIES.map((c) => (
          <Chip
            key={c}
            on={category === c}
            count={counts.get(c) ?? 0}
            onClick={() => setCategory(category === c ? "" : c)}
            title={
              counts.get(c)
                ? undefined
                : "No events of this category are in the loaded window — the category still exists."
            }
          >
            {c}
          </Chip>
        ))}
      </div>

      <Panel padding="none">
        {rows.length ? (
          <div className="list">
            {rows.map((ev) => (
              <EventRow key={ev.id} event={ev} />
            ))}
          </div>
        ) : (
          <QueryState
            error={null}
            loading={false}
            empty={{
              title: filtered ? "Nothing matches these filters" : "Nothing has been published yet",
              hint: filtered
                ? "Older rows may still match — load more history below."
                : "The log fills as the engine works. A company with no integrations and no schedules has nothing to react to.",
            }}
          />
        )}
        <footer className="panel-foot">
          {pageError ? (
            <QueryState error={pageError} loading={false} />
          ) : exhausted ? (
            <span>That is the beginning of the retained history.</span>
          ) : (
            <>
              <Button size="sm" onClick={() => void loadOlder()} disabled={paging}>
                {paging ? "Loading…" : `Load ${PAGE} older`}
              </Button>
              <span className="spacer" />
              <span>
                {older.length > 0 && `${older.length} older rows fetched · `}
                the store keeps 30 days
              </span>
            </>
          )}
        </footer>
      </Panel>
      {paging && <Skeleton rows={3} />}
    </>
  );
}
