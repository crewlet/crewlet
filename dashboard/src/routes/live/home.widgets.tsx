/**
 * What the Live workspace offers for Home: the coding runs in flight, the
 * latest engine events, and the turns that failed today.
 *
 * NOT A SECOND "LIVE NOW". Home's own card already says which seats are on a
 * turn; these answer what Live adds to it: which runs are waiting on a
 * person, what the engine just did, and what went wrong.
 *
 * THE COMPANY-WIDE TRAIL IS ADMIN'S, and each widget names the Live page it
 * comes from, so the event log and the turns are offered to an admin alone.
 */

import { useMemo } from "react";
import { List, ListItem } from "@crewlethq/ui";
import { ActivityGlyph, BrainGlyph, SquareTerminalGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { EventRow } from "~/components/common.tsx";
import { useNow } from "~/lib/clock.ts";
import { fmtDateTime, fmtDuration, plural, relTime } from "~/lib/format.ts";
import { RUNS_POLL_MS } from "~/lib/runs.ts";
import { indexOrg, nameOfIn } from "~/lib/seats.ts";
import { useEngineHealth, useEvents, useOrg, useExecutors } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { RunStatus, mergeRuns, runTitle } from "~/routes/live/Runs.tsx";
import { WidgetCard, WidgetQuery, defineWidgets, gapsOf } from "~/routes/home/board/kit.tsx";

const ROWS = 5;

function CodingRuns() {
  const now = useNow();
  const org = useOrg();
  const nameOf = useMemo(() => nameOfIn(indexOrg(org)), [org]);
  const live = useExecutors();
  // The Runs screen's own read: durable runs have no push behind them.
  const { data, loading, error, refusal } = useQuery("executors", undefined, {
    pollMs: RUNS_POLL_MS,
  });
  const rows = useMemo(() => mergeRuns(data?.runs ?? [], live), [data, live]);
  return (
    <WidgetCard title="Coding runs" from="live-runs">
      <WidgetQuery
        error={error}
        refusal={refusal}
        loading={loading && rows.length === 0}
        // NOTHING TO READ A GAP FROM: neither the runs' answer nor the push
        // says what it could not account for.
        gaps={[]}
        empty={
          rows.length === 0
            ? {
                title: "No coding run is in flight",
                hint: "Finished runs are on each turn's trace.",
              }
            : undefined
        }
      >
        <List>
          {rows.slice(0, ROWS).map((run) => (
            <ListItem
              key={run.turn_id}
              href={href(["live", "runs", run.turn_id])}
              trailing={<RunStatus status={run.status} />}
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
        </List>
        {rows.length > ROWS ? (
          <a className="home-card-more t-link" href={href(["live", "runs"])}>
            {`All ${plural(rows.length, "run")}`}
          </a>
        ) : null}
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
  const rows = (failedOnly ? pushed.filter((e) => e.failed) : pushed).slice(0, EVENT_ROWS);
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
        <div className="live-feed">
          {rows.map((ev) => (
            <EventRow key={ev.id} event={ev} compact />
          ))}
        </div>
      </WidgetQuery>
    </WidgetCard>
  );
}

const HOUR = 3_600_000;
const DAY = 24 * HOUR;

function FailedTurns() {
  const now = useNow();
  // THE WINDOW MOVES BY THE HOUR, so the question changes once an hour rather
  // than with every tick of the clock.
  const edge = Math.ceil(now / HOUR) * HOUR;
  const params = useMemo(
    () => ({
      since: new Date(edge - DAY).toISOString(),
      until: new Date(edge).toISOString(),
      limit: ROWS,
      sort: "-started",
      failed: "true",
    }),
    [edge],
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
            ? { title: "No failed turns", hint: "No turn failed in the last 24 hours." }
            : undefined
        }
      >
        <List>
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
        </List>
      </WidgetQuery>
    </WidgetCard>
  );
}

export const widgets = defineWidgets([
  {
    key: "live.runs",
    from: "live-runs",
    label: "Coding runs",
    description: "Runs in flight and any waiting on an answer",
    icon: SquareTerminalGlyph,
    hue: "cyan",
    size: { w: 6, minW: 4, minH: 8, vertical: true },
    once: true,
    Component: CodingRuns,
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
    description: "Turns that failed in the last 24 hours",
    icon: BrainGlyph,
    hue: "rose",
    size: { w: 4, minW: 3, minH: 8, vertical: true },
    once: true,
    Component: FailedTurns,
  },
]);
