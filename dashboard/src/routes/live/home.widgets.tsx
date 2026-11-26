/**
 * What the Live workspace offers for Home: the executors in flight, the
 * latest engine events, and the turns that failed today.
 *
 * NOT A SECOND "LIVE NOW". Home's own card already says which seats are on a
 * turn; these answer what Live adds to it: which executors are waiting on a
 * person, what the engine just did, and what went wrong.
 *
 * THE COMPANY-WIDE TRAIL IS ADMIN'S, and each widget names the Live page it
 * comes from, so the event log and the turns are offered to an admin alone.
 */

import { useMemo } from "react";
import { ListItem } from "@crewlethq/ui";
import { ActivityGlyph, BrainGlyph, SquareTerminalGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { EventRow } from "~/components/common.tsx";
import { useNow } from "~/lib/clock.ts";
import { endOfBucket } from "~/lib/range.ts";
import { fmtDateTime, fmtDuration, plural, relTime } from "~/lib/format.ts";
import { RUNS_POLL_MS } from "~/lib/runs.ts";
import { indexOrg, nameOfIn } from "~/lib/seats.ts";
import { useEngineHealth, useEvents, useOrg, useExecutors } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { RunStatus, runTitle } from "~/routes/live/Executors.tsx";
import { mergeRuns } from "~/lib/runs.ts";
import {
  WIDGET_PAGE,
  WidgetCard,
  WidgetList,
  WidgetQuery,
  defineWidgets,
  gapsOf,
  useHomeRange,
} from "~/routes/home/board/kit.tsx";
import { rangeSince } from "~/routes/home/model.ts";
import { useCompanyZone } from "~/lib/companyDay.ts";

const ROWS = 5;

function Executors() {
  const now = useNow();
  const org = useOrg();
  const nameOf = useMemo(() => nameOfIn(indexOrg(org)), [org]);
  const live = useExecutors();
  // The Executors screen's own read: the durable rows have no push behind them.
  const { data, loading, error, refusal } = useQuery("executors", undefined, {
    pollMs: RUNS_POLL_MS,
  });
  const rows = useMemo(() => mergeRuns(data?.runs ?? [], live), [data, live]);
  return (
    <WidgetCard title="Executors" from="live-executors">
      <WidgetQuery
        error={error}
        refusal={refusal}
        loading={loading && rows.length === 0}
        // NOTHING TO READ A GAP FROM: neither the executors' answer nor the push
        // says what it could not account for.
        gaps={[]}
        empty={
          rows.length === 0
            ? {
                title: "No executor is in flight",
                hint: "What a finished one did is on its turn's trace.",
              }
            : undefined
        }
      >
        <WidgetList rows={ROWS}>
          {rows.map((run) => (
            <ListItem
              key={run.turn_id}
              href={href(["live", "executors", run.turn_id])}
              trailing={<RunStatus status={run.status} ending={run.ending} />}
            >
              <span className="home-widget-line">
                <span className="truncate">{runTitle(run)}</span>
                <span className="t-caption truncate">
                  {[
                    nameOf(run.agent_handle) || run.role,
                    relTime(run.updated_at || run.started_at, now),
                  ]
                    .filter(Boolean)
                    .join(" · ")}
                </span>
              </span>
            </ListItem>
          ))}
        </WidgetList>
      </WidgetQuery>
    </WidgetCard>
  );
}

const EVENT_ROWS = 6;
const SHOW = [
  { value: "all", label: "Every event" },
  { value: "failed", label: "Failures only" },
] as const;

function LatestEvents({ params }: { params: Readonly<Record<string, string>> }) {
  // The push the event log and Live now draw from, newest first — seeded at
  // this node's boot from every node it could reach, which `seeded_from`
  // says, so a node the seed missed is a gap in what this card lists.
  const pushed = useEvents();
  const seeded = useEngineHealth()?.seeded_from;
  const failedOnly = params.show === "failed";
  const all = failedOnly ? pushed.filter((e) => e.failed) : pushed;
  const rows = all.slice(0, WIDGET_PAGE);
  return (
    <WidgetCard title={failedOnly ? "Failed events" : "Latest events"} from="live-events">
      <WidgetQuery
        error={null}
        loading={false}
        gaps={gapsOf([{ coverage: seeded }])}
        empty={
          rows.length === 0
            ? {
                title: failedOnly ? "Nothing has failed" : "Nothing has happened yet",
                hint: failedOnly
                  ? "Failed events appear here as they happen."
                  : "Events appear here as agents work.",
              }
            : undefined
        }
      >
        <WidgetList variant="plain" className="live-feed" rows={EVENT_ROWS} total={all.length}>
          {rows.map((ev) => (
            <li key={ev.id}>
              <EventRow event={ev} />
            </li>
          ))}
        </WidgetList>
      </WidgetQuery>
    </WidgetCard>
  );
}

const HOUR = 3_600_000;

function FailedTurns() {
  const now = useNow();
  // HOME'S WINDOW, from the company's midnight its range opens on; its end
  // moves by the hour, so the question changes once an hour rather than with
  // every tick of the clock.
  const range = useHomeRange();
  const zone = useCompanyZone();
  const edge = endOfBucket(now, HOUR);
  const since = rangeSince(range, now, zone);
  const params = useMemo(
    () => ({
      since: new Date(since).toISOString(),
      until: new Date(edge).toISOString(),
      limit: WIDGET_PAGE,
      sort: "-started",
      failed: "true",
    }),
    [since, edge],
  );
  const { data, loading, error, refusal } = useQuery("turns", params, { pollMs: 60_000 });
  const rows = data?.turns ?? [];
  return (
    <WidgetCard title="Failed turns" from="live-turns">
      <WidgetQuery
        error={error}
        refusal={refusal}
        loading={loading && !data}
        gaps={gapsOf([data])}
        empty={
          data && rows.length === 0
            ? {
                title: "No failed turns",
                hint:
                  range.days === 1
                    ? "No turn failed today."
                    : `No turn failed in the last ${range.words}.`,
              }
            : undefined
        }
      >
        <WidgetList rows={ROWS} total={rows.length} capped={Boolean(data?.next)}>
          {rows.map((t) => (
            <ListItem
              key={t.turn_id}
              href={href(["live", "turns", t.turn_id])}
              tone="danger"
              trailing={
                <span className="t-caption" title={fmtDateTime(t.started_at)}>
                  {relTime(t.started_at, now)}
                </span>
              }
            >
              <span className="home-widget-line">
                <span className="truncate">{t.role || "Unknown seat"}</span>
                <span className="t-caption truncate">
                  {[
                    t.work_key,
                    plural(t.phases, "phase"),
                    t.duration_ms > 0 ? fmtDuration(t.duration_ms) : "",
                  ]
                    .filter(Boolean)
                    .join(" · ")}
                </span>
              </span>
            </ListItem>
          ))}
        </WidgetList>
      </WidgetQuery>
    </WidgetCard>
  );
}

export const widgets = defineWidgets([
  {
    key: "live.executors",
    from: "live-executors",
    label: "Executors",
    description: "Executors in flight and any waiting on an answer",
    icon: SquareTerminalGlyph,
    hue: "cyan",
    size: { w: 6, minW: 4, minH: 8, vertical: true },
    once: true,
    Component: Executors,
  },
  {
    key: "live.events",
    from: "live-events",
    label: "Latest events",
    description: "The engine's latest events",
    icon: ActivityGlyph,
    hue: "blue",
    size: { w: 6, minW: 4, minH: 8, vertical: true },
    params: [
      {
        key: "show",
        label: "Show",
        useChoices: () => ({ loading: false, options: SHOW }),
      },
    ],
    nameOf: (p) => (p.show === "failed" ? "Failed events" : undefined),
    Component: LatestEvents,
  },
  {
    key: "live.failed-turns",
    from: "live-turns",
    label: "Failed turns",
    description: "Turns that failed over Home's range",
    icon: BrainGlyph,
    hue: "rose",
    size: { w: 4, minW: 3, minH: 8, vertical: true },
    once: true,
    Component: FailedTurns,
  },
]);
