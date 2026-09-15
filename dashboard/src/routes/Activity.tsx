/**
 * The event log.
 *
 * It is a LIST SCREEN, drawn the way every list screen in this product is
 * drawn: a head, a toolbar carrying the search box and the Filter and Sort
 * menus, a chip for each axis a reader narrowed, the rows, and a footer saying
 * how many of how many are on screen. The row of hand-built pills this
 * replaces was a fourth idea of what a filter looks like.
 *
 * Two things this screen gets right that its predecessor did not:
 *
 *  1. **The filter vocabulary is FIXED.** Category chips came from the live
 *     400-event ring, so chips appeared and vanished as it evicted, including
 *     the one you were reaching for, and an actor who had gone quiet could not
 *     be filtered to AT ALL, because no chip existed for them. The categories
 *     are a closed set the engine defines; the actor filter is a text box over
 *     the roster.
 *  2. **A row says WHO.** The old feed rendered a nub, a colour dot, a summary
 *     and a relative time, and offered actor filter chips for a field it never
 *     displayed.
 *
 * The narrowing itself stays the SCREEN's: every axis is a URL parameter, so a
 * narrowed log is a link somebody can send, and the rows handed down are the
 * ones that survived. History paging asks the engine for older rows past the
 * live ring. The cursor is `before_time`+`before_id`, which is what the server
 * actually reads: the previous client sent `before`, so `time.Parse("")`
 * failed and EVERY cursored page was rejected, then read the rejection as
 * "that is the beginning of the retained history".
 */

import { useCallback, useMemo, useState } from "react";
import { useParam } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { useClient, useEvents } from "~/lib/store-hooks.ts";
import { href } from "~/app/router.tsx";
import { fmtDateTime, humanize, newestFirst, plural } from "~/lib/format.ts";
import type { FeedRow } from "~/protocol/index.ts";
import { ErrorGlyph } from "@crewlethq/icons/glyphs";
import {
  Button,
  DataView,
  EmptyState,
  RelativeTime,
  Tag,
  useNow,
  type DataViewColumn,
  type FilterDef,
  type FilterValues,
} from "@crewlethq/ui";

/**
 * The categories the engine assigns, as a CLOSED set.
 *
 * Mirrors `internal/events/category.go`. An answer for a category with nothing
 * in it is still useful: it says the category exists and is quiet, which is
 * the opposite of a chip that vanishes because the ring evicted its last row.
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
  const now = useNow();
  const [category, setCategory] = useParam("category", "");
  const [actor, setActor] = useParam("actor", "");
  const [q, setQ] = useParam("q", "");
  const [onlyFailed, setOnlyFailed] = useParam("failed", "");

  const [older, setOlder] = useState<FeedRow[]>([]);
  const [cursor, setCursor] = useState<{ before_time: string; before_id: string } | null>(null);
  const [exhausted, setExhausted] = useState(false);
  const [paging, setPaging] = useState(false);
  const [pageError, setPageError] = useState<string | null>(null);

  const all = useMemo(() => {
    const seen = new Set<string>();
    return [...liveEvents, ...older].filter((e) => {
      if (seen.has(e.id)) return false;
      seen.add(e.id);
      return true;
    });
  }, [liveEvents, older]);

  const rows = useMemo(() => {
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
  }, [all, category, actor, q, onlyFailed]);

  const counts = useMemo(() => {
    const map = new Map<string, number>();
    for (const e of all) map.set(e.category, (map.get(e.category) ?? 0) + 1);
    return map;
  }, [all]);

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
      // The answer is an OBJECT, `{events, next, exhausted}`, not a bare
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

  /*
   * THE SCREEN OWNS THE NARROWING, so every axis is declared here rather than
   * on a column: each one is a URL parameter, two of them are sent to the
   * engine when older rows are fetched, and a browser applying them a second
   * time over a field a row does not carry would narrow the list twice.
   */
  const filters = useMemo<FilterDef<FeedRow>[]>(
    () => [
      {
        name: "q",
        label: "Search events",
        role: "search",
        placeholder: "Search summary, type or source",
      },
      { name: "actor", label: "Actor", placeholder: "A seat's handle, or the engine" },
      {
        name: "category",
        label: "Category",
        kind: "select",
        options: [
          { value: "", label: "Any category" },
          ...CATEGORIES.map((c) => ({
            value: c,
            // The count says whether a category is quiet rather than absent.
            label: `${c} (${counts.get(c) ?? 0})`,
          })),
        ],
      },
      {
        name: "failed",
        label: "Outcome",
        kind: "select",
        options: [
          { value: "", label: "Any outcome" },
          { value: "1", label: "Failures only" },
        ],
      },
    ],
    [counts],
  );

  const values: FilterValues = { q, actor, category, failed: onlyFailed };

  const onValuesChange = useCallback(
    (next: FilterValues) => {
      setQ(String(next.q ?? ""));
      setActor(String(next.actor ?? ""));
      setCategory(String(next.category ?? ""));
      setOnlyFailed(String(next.failed ?? ""));
    },
    [setQ, setActor, setCategory, setOnlyFailed],
  );

  const columns = useMemo<DataViewColumn<FeedRow>[]>(
    () => [
      {
        key: "timestamp",
        header: "When",
        shrink: true,
        sortable: true,
        firstDirection: "desc",
        sortValue: (e) => Date.parse(e.timestamp) || 0,
        render: (e) => (
          <RelativeTime value={e.timestamp} now={now} title={fmtDateTime(e.timestamp)} />
        ),
      },
      {
        key: "actor",
        header: "Actor",
        shrink: true,
        sortable: true,
        sortValue: (e) => e.actor || "engine",
        render: (e) => e.actor || "engine",
      },
      {
        key: "summary",
        header: "What happened",
        sortable: true,
        sortValue: (e) => e.summary || e.type,
        render: (e) => (
          <span className="row gap-1">
            {/* The word is what carries it; the glyph only repeats the row's
                own tone, which is why it is hidden rather than named twice. */}
            {e.failed && <ErrorGlyph size="xs" aria-hidden />}
            <span className="truncate">{e.summary || e.type}</span>
          </span>
        ),
      },
      {
        key: "source",
        header: "Source",
        shrink: true,
        sortable: true,
        sortValue: (e) => e.source,
        render: (e) => e.source,
      },
      {
        key: "category",
        header: "Category",
        shrink: true,
        sortable: true,
        sortValue: (e) => e.category,
        render: (e) => <Tag appearance="outline">{humanize(e.category) || "system"}</Tag>,
      },
    ],
    [now],
  );

  return (
    <DataView<FeedRow>
      title="Event log"
      description="Everything the engine published, live and then paged out of the store. This tab holds the last 400 in memory; older rows are fetched."
      badges={<Tag appearance="outline">{plural(rows.length, "event")} shown</Tag>}
      framed
      columns={columns}
      rows={rows}
      totalCount={all.length}
      rowKey="id"
      getRowHref={(e) => href(["events", e.id])}
      rowTone={(e) => (e.failed ? "danger" : null)}
      defaultSort={{ key: "timestamp", direction: "desc" }}
      filters={filters}
      filterValues={values}
      onFilterValuesChange={onValuesChange}
      emptyMessage={
        <EmptyState
          size="compact"
          title={filtered ? "Nothing matches these filters" : "Nothing has been published yet"}
          description={
            filtered
              ? "Older rows may still match. Load more history from the footer below."
              : "The log fills as the engine works. A company with no integrations and no schedules has nothing to react to."
          }
        />
      }
      pagination={
        pageError ? (
          <QueryState error={pageError} loading={false} />
        ) : exhausted ? (
          <span>That is the beginning of the retained history.</span>
        ) : (
          <>
            <Button
              variant="secondary"
              size="small"
              onClick={() => void loadOlder()}
              disabled={paging}
            >
              {paging ? "Loading older events" : `Load ${PAGE} older`}
            </Button>
            <span>
              {older.length > 0 && `${plural(older.length, "older row")} fetched · `}
              the store keeps 30 days
            </span>
          </>
        )
      }
    />
  );
}
