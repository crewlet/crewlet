/**
 * "Recent activity" — what the company did, newest first: work delivered,
 * filed and handed on, pages published, schedules run — one merged feed from
 * the engine (`company_feed`) with one cursor.
 *
 * EVERY PHRASE IS A FACT ON THE ROW: "approved on first review" is the
 * engine's `first_pass`, "1 turn · 38.2k tokens" its `spend` (tokens, never
 * money), "from Slack" the create's own `origin`, "hand-off 1 of 8" the task's
 * counter against its budget, "ran 12 times" the engine's fold of a schedule's
 * consecutive runs. Nothing is inferred from a title, and every verb is one
 * table's (`FEED_PHRASES`, `scheduleVerb` in `model.ts`).
 *
 * THE HEAD FREEZES WHEN AN OLDER PAGE IS ASKED FOR, as the work history's
 * does: the cursor was minted from the first page's last row, so a poll that
 * then brought newer rows would push rows out of the first page that belong
 * to neither — a hole in the middle of a feed. A filter change starts a live
 * one again.
 */

import { useCallback, useMemo, useState } from "react";
import { Button, Card, SegmentedControl } from "@crewlethq/ui";
import {
  CircleCheckGlyph,
  CalendarClockGlyph,
  FileTextGlyph,
  PlusGlyph,
  UserGlyph,
} from "@crewlethq/icons/glyphs";
import { href, useParam } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useMediaQuery } from "~/lib/media.ts";
import { PHONE_BREAKPOINT } from "~/app/layout.ts";
import { useClient, useOrg } from "~/lib/store-hooks.ts";
import { indexOrg, type OrgIndex } from "~/lib/seats.ts";
import { fmtCount, fmtDateCompact, parseUTC, plural, readerDay } from "~/lib/format.ts";
import { zone } from "~/lib/prefs.ts";
import type { CompanyFeedAnswer, FeedEntry, FeedWorkRow } from "~/protocol/index.ts";
import {
  FEED_FILTERS,
  FEED_PHRASES,
  feedFilterOf,
  mergePages,
  scheduleVerb,
  surfaceName,
} from "./model.ts";
import { itemPath } from "~/lib/work.ts";

/** One screen of the feed before "Load older". */
export const FEED_PAGE = 20;

export function Feed({ now }: { now: number }) {
  const [filterValue, setFilter] = useParam("feed", "all");
  const filter = feedFilterOf(filterValue);
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const { socket } = useClient();
  const params = useMemo(
    () => ({ limit: FEED_PAGE, ...(filter.kinds ? { kinds: filter.kinds } : {}) }),
    [filter.kinds],
  );
  const live = useQuery("company_feed", params, { pollMs: 30_000 });
  const phone = useMediaQuery(`(width < ${PHONE_BREAKPOINT}px)`);

  // The older pages, the head they were paged from, and where they stopped —
  // all reset by a filter change, which is a different feed.
  const [older, setOlder] = useState<{
    key: string;
    head: CompanyFeedAnswer;
    pages: CompanyFeedAnswer[];
  } | null>(null);
  const [paging, setPaging] = useState(false);
  const [pageError, setPageError] = useState<string | null>(null);
  const key = filter.value;
  const held = older && older.key === key ? older : null;
  const head = held?.head ?? live.data;
  const last = held ? held.pages[held.pages.length - 1] : live.data;
  const next = last?.next_cursor ?? "";

  const loadOlder = useCallback(async () => {
    if (!next || !head) return;
    setPaging(true);
    setPageError(null);
    try {
      const page = await socket.query("company_feed", { ...params, cursor: next });
      setOlder((prev) =>
        prev && prev.key === key
          ? { ...prev, pages: [...prev.pages, page] }
          : { key, head, pages: [page] },
      );
    } catch (err) {
      setPageError(err instanceof Error ? err.message : "query_failed");
    } finally {
      setPaging(false);
    }
  }, [socket, params, next, head, key]);

  const rows = useMemo(
    () => (head ? mergePages([head, ...(held?.pages ?? [])]) : []),
    [head, held],
  );

  return (
    <Card padding="none" className="home-card home-feed">
      <Card.Header
        actions={
          <a className="t-link" href={href(["live", "events"])}>
            Full event log
          </a>
        }
      >
        <span className="home-feed-title">
          <Card.Title as="h3">Recent activity</Card.Title>
          {/* THE DESIGN'S SIZE, the range control's own — and the small one on
              a phone, where the four at full size are wider than the card and
              the last would sit behind a scroll nobody knows is there. */}
          <SegmentedControl
            label="Show"
            semantics="radio"
            size={phone ? "sm" : "md"}
            options={FEED_FILTERS.map((f) => ({ value: f.value, label: f.label }))}
            value={filter.value}
            onValueChange={(v) => setFilter(v === "all" ? "" : v)}
          />
        </span>
      </Card.Header>
      <QueryState
        error={live.error}
        refusal={live.refusal}
        loading={live.loading && !live.data}
        empty={
          head && rows.length === 0
            ? {
                title:
                  filter.value === "all"
                    ? "Nothing has happened yet"
                    : `No ${filter.label.toLowerCase()} yet`,
                hint: "A row appears here as the company's agents file, finish and hand on work.",
              }
            : undefined
        }
      >
        <ul className="home-feed-list">
          {rows.map((row, i) => (
            <FeedRow
              key={`${row.kind}:${row.work?.id ?? row.page?.id ?? i}`}
              row={row}
              index={index}
              now={now}
            />
          ))}
        </ul>
        {(next || pageError) && (
          <div className="home-feed-more">
            {pageError && <span className="t-caption">Could not read older activity.</span>}
            <Button size="small" variant="ghost" loading={paging} onClick={() => void loadOlder()}>
              Load older
            </Button>
          </div>
        )}
      </QueryState>
    </Card>
  );
}

function FeedRow({ row, index, now }: { row: FeedEntry; index: OrgIndex; now: number }) {
  const { icon, text, aside } = phrase(row, index, now);
  return (
    <li className="home-feed-row">
      <time className="home-feed-time mono" dateTime={row.at}>
        {whenLabel(row.at, now)}
      </time>
      <span className="home-feed-mark" aria-hidden="true">
        {icon}
      </span>
      <span className="home-feed-text">{text}</span>
      {aside && <span className="home-feed-aside">{aside}</span>}
    </li>
  );
}

/** "09:42" for a row from today, "Sep 22" for an older one — the reader's clock. */
function whenLabel(at: string, now: number): string {
  const d = parseUTC(at);
  if (!d) return "";
  if (readerDay(d) === readerDay(now)) {
    return d.toLocaleTimeString(undefined, {
      hour: "2-digit",
      minute: "2-digit",
      hour12: false,
      timeZone: zone(),
    });
  }
  return fmtDateCompact(at, now);
}

function nameOf(index: OrgIndex, handle: string | undefined): string {
  if (!handle) return "Somebody";
  return index.byHandle.get(handle)?.name ?? handle;
}

/** The task a row is about: its key as a reader reads it, linked by its address. */
function Key({ row }: { row: FeedWorkRow }) {
  if (!row.key && !row.task) return null;
  return (
    <a
      className="home-feed-key mono"
      href={href(itemPath({ id: row.task, key: row.key, key_collision: row.key_collision }))}
    >
      {row.key || row.task}
    </a>
  );
}

/** A row in words: its mark, its sentence and the fact at its end. Every verb
 *  is `FEED_PHRASES`' or `scheduleVerb`'s — see `model.ts`. */
function phrase(row: FeedEntry, index: OrgIndex, now: number) {
  const w = row.work;
  if (w) {
    // WHOEVER THE RECORD NAMES (`iam.ActorFor`): a person the directory binds
    // to a seat writes AS the seat, and anybody bound to none under their login.
    const who = <strong>{nameOf(index, w.actor)}</strong>;
    switch (w.kind) {
      case "completed": {
        const review =
          w.first_pass === true
            ? " — approved on first review"
            : w.first_pass === false
              ? " — sent back in review"
              : "";
        return {
          icon: <CircleCheckGlyph size="sm" />,
          text: (
            <>
              {who} {FEED_PHRASES.completed} <Key row={w} /> {w.title}
              {review}
            </>
          ),
          aside: w.spend
            ? `${plural(w.spend.turns, "turn")} · ${fmtCount(w.spend.tokens)} tokens`
            : "",
        };
      }
      case "created":
        return {
          icon: <PlusGlyph size="sm" />,
          text: (
            <>
              {who} {FEED_PHRASES.created} <Key row={w} /> {w.title}
            </>
          ),
          aside: w.origin ? `from ${surfaceName(w.origin.surface)}` : "",
        };
      case "handoff":
        return {
          icon: <UserGlyph size="sm" />,
          text: (
            <>
              {who} {FEED_PHRASES.handoff} <Key row={w} /> from {nameOf(index, w.from)} to{" "}
              {nameOf(index, w.to)}
            </>
          ),
          aside:
            w.reassignments != null && w.reassignment_budget
              ? `hand-off ${w.reassignments} of ${w.reassignment_budget}`
              : "",
        };
    }
  }
  if (row.page) {
    const p = row.page;
    return {
      icon: <FileTextGlyph size="sm" />,
      text: (
        <>
          <strong>{nameOf(index, p.actor)}</strong>{" "}
          {p.change === "created" ? FEED_PHRASES.page_created : FEED_PHRASES.page_saved}{" "}
          <a className="prose-link" href={href(["knowledge", "pages", p.page_id])}>
            {p.title ? `“${p.title}”` : "a page"}
          </a>
        </>
      ),
      aside: "Knowledge",
    };
  }
  const s = row.schedule;
  if (!s) {
    // A ROW WITH NO BODY IT CAN READ — a kind a newer engine added — says so
    // rather than inventing a sentence for it.
    return {
      icon: <CalendarClockGlyph size="sm" />,
      text: <>An entry this dashboard cannot read ({row.kind})</>,
      aside: "",
    };
  }
  // SINCE IS THE OLDEST RUN THE ROW FOLDS, on the same clock as every time in
  // the column: "since 02:40" beside "ran 12 times" says over what span.
  return {
    icon: <CalendarClockGlyph size="sm" />,
    text: (
      <>
        Schedule <strong>{s.name}</strong> {scheduleVerb(s)}
        {s.target ? ` for ${nameOf(index, s.target)}` : ""}
      </>
    ),
    aside: s.since && (s.runs ?? 1) > 1 ? `since ${whenLabel(s.since, now)}` : "",
  };
}
