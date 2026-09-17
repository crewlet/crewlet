/**
 * Recurring work: what fires, when it next fires, and how it last went.
 *
 * THE NAMES HERE ARE THE SERVER'S. This screen used to read `scope`,
 * `scope_name`, `last_run` and `last_outcome` off a schedule and `name`,
 * `scope_name` and `detail` off a run; `schedule.Row` serialises `scope_type`
 * and `scope_id` and has no last-run field at all, and a ledger row carries
 * `schedule_name`, `fire_label`, `target_handle`, `scheduled_at` and
 * `trace_id`. Seven cells rendered `undefined` on every row, and the sort and
 * row keys they were built from collapsed every row onto one key.
 *
 * The last fire is JOINED from the ledger rather than read off the row, on the
 * identity the ledger keys on — (scope_type, scope_id, name) — because that is
 * where the fact actually lives.
 *
 * AND THE LEDGER HAS TWO OUTCOMES. `schedule.Outcome` is `fired` or
 * `skipped_catchup`: it is a DISPATCH ledger, not a turn-outcome one, so
 * "recent failures" counted a value the engine cannot emit and read 0 for ever
 * on every company. What it can honestly say is how many ticks were skipped,
 * which is the catchup cap doing its job or a node that was down too long.
 */

import { useMemo, type ReactNode } from "react";
import { QueryState, SeatChip } from "~/components/common.tsx";
import { Card, EmptyState, Skeleton, StatCard, StatGroup, Tag } from "@crewlethq/ui";
import {
  CalendarTodayGlyph,
  DescriptionGlyph,
  ErrorGlyph,
  ScheduleGlyph,
} from "@crewlethq/icons/glyphs";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { Dash, DateCell, SeatCell, TextCell } from "~/app/frame/cells.tsx";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { PropertiesRail } from "~/app/frame/PropertiesRail.tsx";
import { peekHref, rowPeekHandler, usePeekControls } from "~/app/frame/DetailRail.tsx";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import type { ObjectRef } from "~/app/frame/objects.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { describe as describeCron, nextFires } from "~/lib/cron.ts";
import { fmtDateTime, fmtDuration, inTime, relTime, tsKey, plural } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import type { ScheduleRow, ScheduleRunRow } from "~/protocol/index.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { usePageLabels } from "~/app/Shell.tsx";
import { href } from "~/app/router.tsx";

/** The ledger's two outcomes, and nothing else — see the note above. */
const OUTCOME_TONE: Record<string, "success" | "warning" | "neutral"> = {
  fired: "success",
  skipped_catchup: "warning",
};

/** The identity a ledger row and a schedule row share. */
function identity(scopeType: string, scopeID: string, name: string): string {
  return `${scopeType}:${scopeID}:${name}`;
}

const rowID = (s: ScheduleRow) => identity(s.scope_type, s.scope_id, s.name);
const runID = (r: ScheduleRunRow) => identity(r.scope_type, r.scope_id, r.schedule_name);

/**
 * A schedule's address, as the FRAME spells it.
 *
 * THREE SEGMENTS JOINED BY A SLASH, which is `objects.ts`'s own grammar for
 * this kind and not a second spelling of [identity]: that one is a map key
 * inside this screen and may use whatever separator it likes, this one is
 * carried in a URL and read back by `KINDS.schedule.pathOf`. Both are all
 * three segments for the same reason — two units may each declare a
 * "standup", and a role and a unit may both — so an address that carried the
 * name alone would open whichever one the rail happened to find first.
 */
function scheduleRef(scopeType: string, scopeId: string, name: string): ObjectRef {
  return { kind: "schedule", id: [scopeType, scopeId, name].join("/") };
}

/** Where a scope points: a seat for a role schedule, a unit for a unit one. */
function scopePath(scopeType: string, scopeId: string): string[] {
  return scopeType === "role" ? ["company", "people", scopeId] : ["company", "units", scopeId];
}

/**
 * What a schedule IS — the four facts, in one order, for the page and the rail.
 *
 * ONE BUILDER, for the reason `ObjectHeader` exists: a schedule is recognised
 * by WHEN it fires, and "when" is two facts that are easy to mistake for each
 * other — the EXPRESSION, which is what its author wrote, and NEXT, which is
 * what the engine worked out from it in the row's own timezone. A page that
 * led with one and a rail that led with the other would make a reader
 * re-derive the schedule every time it changed frame.
 */
function scheduleFacts(row: ScheduleRow, now: number): Fact[] {
  const said = describeCron(row.cron);
  return [
    {
      label: "Cron",
      // THE EXPRESSION AND WHAT IT MEANS, side by side. A reader who knows
      // cron reads `0 9 * * 1-5`; a founder reads five numbers and a dash on
      // the one screen that says when the company wakes itself up. An
      // expression this build cannot read shows as itself with nothing
      // claimed about it.
      value: (
        <>
          <code className="inline nowrap">{row.cron}</code>
          {said && <span className="faint"> {said}</span>}
        </>
      ),
    },
    { label: "Timezone", value: row.timezone || "UTC" },
    {
      label: "Next",
      // THE ENGINE'S ANSWER, and the REASON where it has none. A schedule
      // whose timezone was renamed has an empty `next_run` and would read as
      // merely idle — the row says so in its own `problem` field and this is
      // the only place a reader sees it.
      value: row.next_run ? (
        <span title={fmtDateTime(row.next_run)}>{inTime(row.next_run, now)}</span>
      ) : row.problem ? (
        <Tag variant="danger" title={row.problem}>
          {row.problem}
        </Tag>
      ) : (
        <span className="faint">
          {row.enabled ? "never — the calendar does not reach it" : "disabled"}
        </span>
      ),
    },
    {
      // WHO A FIRE REACHES, resolved by the engine from the scope and the
      // target rather than worked out here: a unit schedule targeting `each`
      // wakes every member, one targeting `lead` wakes one seat, and a role
      // schedule's target is meaningless rather than defaulted.
      label: "Wakes",
      value: row.runners?.length ? row.runners.join(", ") : <span className="faint">nobody</span>,
    },
  ];
}

/**
 * The one thing about a schedule that is a STATE rather than identity.
 *
 * Three badges in the page bar once — which is where a screen's CONTROLS
 * live, so a schedule's own condition was drawn in the one strip that is not
 * about the object. Only one of them can be true at a time and they are not
 * equally bad: a DISABLED schedule is somebody's decision and a schedule that
 * CANNOT FIRE is a defect, so only the second carries a tone.
 */
function scheduleStatus(row: ScheduleRow | undefined): ReactNode {
  if (!row) {
    return (
      <Tag appearance="outline" title="the ledger outlives the configuration">
        no longer declared
      </Tag>
    );
  }
  if (row.problem) {
    return (
      <Tag variant="danger" title={row.problem}>
        cannot fire
      </Tag>
    );
  }
  if (!row.enabled) return <Tag appearance="outline">disabled</Tag>;
  return undefined;
}

/**
 * The definition itself, on the page and in the rail alike.
 *
 * THREE OF THESE ROWS HAD NO SURFACE AT ALL. `target`, `catchup` and
 * `timeout_seconds` are on every schedule the engine serialises and were
 * rendered nowhere: whether a missed tick fires late and how long a fire may
 * run before the engine stops it are the two questions asked of a schedule
 * that misbehaved, and the only way to answer either was to read the company
 * configuration.
 */
function ScheduleDefinition({ row }: { row: ScheduleRow }) {
  const said = describeCron(row.cron);
  return (
    <Card>
      <Card.Header icon={<DescriptionGlyph size="sm" />}>
        <Card.Title>Definition</Card.Title>
      </Card.Header>
      <PropertiesRail
        groups={[
          {
            properties: [
              { label: "Task", value: row.task },
              {
                label: "Means",
                value: said ?? (
                  <span className="faint">this build cannot read this expression</span>
                ),
                title: "the cron expression in words",
              },
              {
                label: "Scope",
                value: `${row.scope_type} · ${row.scope_id}`,
                path: scopePath(row.scope_type, row.scope_id),
              },
              {
                label: "Target",
                // A ROLE SCHEDULE'S TARGET IS MEANINGLESS rather than
                // defaulted, which is `schedule.Row`'s own rule — so the row
                // says which of the two an empty value is instead of drawing
                // the dash that means "set to nothing".
                value:
                  row.target ||
                  (row.scope_type === "role" ? (
                    <span className="faint">not used by a role schedule</span>
                  ) : (
                    ""
                  )),
                title: "which members of the unit a fire reaches",
              },
              {
                label: "Catchup",
                value: row.catchup
                  ? "a tick missed while the node was down fires late"
                  : "a missed tick is dropped",
              },
              {
                label: "Cap",
                // ABSENT IS NOT ZERO here either: a schedule with no cap
                // declared runs to the engine's own default rather than for
                // no time at all.
                value: row.timeout_seconds > 0 ? fmtDuration(row.timeout_seconds * 1000) : "",
                title: "wall clock one fire may take before the engine stops it",
              },
            ],
          },
        ]}
      />
    </Card>
  );
}

/**
 * How many fires ahead the page works out.
 *
 * Five: enough that a weekly schedule shows a month and a daily one shows a
 * working week, and few enough that the panel is read rather than scrolled.
 */
const UPCOMING_FIRES = 5;

/**
 * And how many the rail does.
 *
 * Three, for the question the rail asks rather than the one the page does:
 * "every 4 hours" and "at 4am" have the same NEXT fire for most of the day
 * and differ on the one after, so two is the smallest number that can answer
 * "is this the schedule I meant" and the third is what tells a weekly from a
 * fortnightly. More than that is a list scrolled inside a 420 px column, and
 * `Open ↗` is one click from the page that shows five.
 */
const PEEK_FIRES = 3;

/** And how many of its last fires — see [PEEK_FIRES]; the page shows the page. */
const PEEK_RUNS = 3;

/**
 * The fires after the next one.
 *
 * THE ENGINE IS THE AUTHORITY on the next fire and the facts above say so.
 * What it does NOT say is the one after — and the fires after the next are
 * what tell a reader whether an expression means what its author thought.
 *
 * Exported for its own suite, in this tree's idiom (`Runs.BridgeLog`,
 * `Seat.CounterpartyRow`): the claim worth pinning is which ZONE the
 * expression is worked out in, and the screen around it needs an org, a
 * roster and three queries before this panel draws at all.
 */
export function NextFires({ row, now, count }: { row: ScheduleRow; now: number; count: number }) {
  // ANCHORED TO THE MINUTE, not to the ticking clock: the list changes when a
  // fire passes, and recomputing it every second would rebuild the panel
  // sixty times for an answer that moves once.
  const minute = Math.floor(now / 60_000);
  // IN THE ROW'S OWN ZONE, which `nextFires` requires and refuses to default —
  // the engine resolves the row's timezone and evaluates the expression there,
  // so a list worked out in UTC was wrong by the zone's STANDING offset on
  // every row of every company that does not run in UTC, not merely across a
  // daylight-saving change. An empty `timezone` is the engine's own default
  // rather than an unreadable one; a zone this runtime cannot read yields no
  // fires at all, which is the same refusal an unparseable expression gets and
  // is what `problem` on the row names.
  const upcoming = useMemo(
    () =>
      row.cron ? nextFires(row.cron, new Date(minute * 60_000), row.timezone || "UTC", count) : [],
    [row.cron, row.timezone, minute, count],
  );
  // ONE FIRE IS NOT A SERIES. The next fire is already a fact in the header,
  // so a panel repeating it alone would be the same value twice.
  if (upcoming.length <= 1) return null;
  return (
    <Card>
      <Card.Header
        icon={<CalendarTodayGlyph size="sm" />}
        subtitle={`the next ${upcoming.length} fires this expression works out to, evaluated in ${row.timezone || "UTC"}`}
      >
        <Card.Title>And after that</Card.Title>
      </Card.Header>
      <ol className="fires">
        {upcoming.map((at) => (
          <li key={at.toISOString()} className="row gap-2">
            <span className="mono t-caption">{fmtDateTime(at.toISOString())}</span>
            <span className="spacer" />
            <span className="t-caption faint">{inTime(at.toISOString(), now)}</span>
          </li>
        ))}
      </ol>
      {row.timezone && row.timezone.toUpperCase() !== "UTC" && (
        // TWO ZONES ARE IN PLAY AND THEY ARE NOT THE SAME ONE. The expression
        // is worked out in the SCHEDULE's zone — the engine's own, so the list
        // no longer drifts from it — and each instant is then rendered in the
        // READER's, like every other timestamp on this screen. Saying which is
        // which beats a reader working out why `0 9 * * *` in Asia/Tokyo is
        // listed at midnight.
        <p className="t-caption" style={{ marginTop: "var(--space-2)" }}>
          This schedule is evaluated in <code className="inline">{row.timezone}</code>; the instants
          above are the same fires shown in your own timezone.
        </p>
      )}
    </Card>
  );
}

/** What an address that names no schedule and no fire means. */
const NO_SCHEDULE_HINT =
  "A schedule is addressed by all three of its segments — scope type, scope, name. A company revision may have removed this one, and the retention sweep may have taken its fires.";

/** And what the ledger outliving the configuration means, wherever it shows. */
const UNDECLARED_HINT =
  "The company configuration no longer declares this schedule. Its history is kept until the retention sweep takes it.";

export function Schedules({ scope = [] }: { scope?: string[] }) {
  const now = useNow();
  const { open: openPeek } = usePeekControls();
  // `#/activity/schedules/{scope_type}/{scope_id}/{name}` names ONE schedule.
  // Three segments because a schedule's identity is all three: two units may
  // each declare a "standup", and a role and a unit may both.
  const [scopeType, scopeId, scheduleName] = scope;
  const detail = Boolean(scopeType && scopeId && scheduleName);
  // ONE SCHEDULE'S OWN HISTORY, which the company-wide ledger below cannot
  // be: `schedules.recent_runs` is fifty rows across every schedule, so
  // twenty hourly ones fill it in two and a half hours and "did the standup
  // fire this week" is unanswerable. The three segments were destructured
  // here and never read, so this address rendered the whole list.
  const one = useQuery(
    "schedule_runs",
    { scope_type: scopeType, scope_id: scopeId, name: scheduleName },
    { enabled: detail, pollMs: 30_000 },
  );
  // Schedules are pushed on a config apply, and the RUNS are not pushed at
  // all — so this polls, slowly, because a cron's next fire moves in minutes.
  const { data, loading, error } = useQuery("schedules", undefined, { pollMs: 30_000 });

  const schedules = data?.schedules ?? [];
  const runs = data?.recent_runs ?? [];
  const due = schedules.filter((s) => tsKey(s.next_run) > 0 && tsKey(s.next_run) - now < 3_600_000);
  // A schedule that cannot fire at all, which is a defect rather than a
  // choice: a cron nobody can parse, a timezone that no longer exists. The
  // row says so in its own `problem` field and a blank Next cell was the
  // only symptom.
  const broken = schedules.filter((s) => s.problem);

  // THE ORDER `[` AND `]` WALK, published only from the LIST. On the detail
  // route this same component renders one schedule, and a stepper walking
  // every schedule in the company would step a rail opened from a screen its
  // rows are not on.
  usePeekNeighbours(
    useMemo(
      () => (detail ? [] : schedules.map((s) => scheduleRef(s.scope_type, s.scope_id, s.name))),
      [detail, schedules],
    ),
  );

  // The last fire per schedule, from the ledger. `Recent` returns newest
  // first, so the first row per identity is the latest one.
  const lastFire = new Map<string, ScheduleRunRow>();
  for (const run of runs) if (!lastFire.has(runID(run))) lastFire.set(runID(run), run);

  const chosen = detail
    ? schedules.find(
        (s) => s.scope_type === scopeType && s.scope_id === scopeId && s.name === scheduleName,
      )
    : undefined;

  /**
   * A row's click, for the two grids whose rows point at a schedule.
   *
   * THE GRID HANDS THIS BOTH EVENTS. `rowPeekHandler` is the frame's one copy
   * of "which clicks mean elsewhere" and reads a mouse event — ⌘, ctrl,
   * shift, alt and the middle button belong to the browser, which is what
   * keeps the row a real link to the schedule's page. The `enter` chord
   * carries no button at all and is never "open elsewhere".
   */
  function openSchedule(ref: ObjectRef, e: React.MouseEvent | React.KeyboardEvent): void {
    const go = () => openPeek(ref);
    if (!("button" in e)) {
      go();
      return;
    }
    rowPeekHandler(go)?.(e);
  }

  // THE THREE SEGMENTS SPELLED OUT rather than `detail` reused, because this
  // is the one place their being present has to NARROW them: a boolean says
  // the same thing to a reader and nothing at all to the compiler.
  if (scopeType && scopeId && scheduleName) {
    return (
      <OneSchedule
        scopeType={scopeType}
        scopeId={scopeId}
        name={scheduleName}
        row={chosen}
        // WHETHER AN ABSENT ROW MEANS ANYTHING YET. `chosen` is undefined both
        // while the company's schedules are in flight and when none of them
        // is this one, and those say opposite things about the fires below.
        known={Boolean(data)}
        runs={one.data?.runs ?? []}
        truncated={Boolean(one.data?.truncated)}
        loading={one.loading}
        error={one.error}
        now={now}
      />
    );
  }

  return (
    <>
      <PageActions>
        {<Tag appearance="outline">{plural(schedules.length, "schedule")} defined</Tag>}
      </PageActions>
      <PageNote>
        Role- and unit-scoped recurring work. Delivery is at-most-once, a missed tick is caught up,
        and a run is capped on wall clock.
      </PageNote>

      {/* The flush Panel is gone: StatGroup draws that surface itself. */}
      <StatGroup columns={3}>
        <StatCard
          icon={<CalendarTodayGlyph size="xs" />}
          label="Schedules"
          value={schedules.length}
          sub="across every seat and unit"
        />
        <StatCard
          icon={<ScheduleGlyph size="xs" />}
          label="Firing within the hour"
          value={due.length}
          sub={due.length ? due.map((s) => s.name).join(", ") : "nothing due soon"}
        />
        <StatCard
          icon={<ErrorGlyph size="xs" />}
          label="Cannot fire"
          tone={broken.length ? "danger" : undefined}
          value={broken.length}
          sub={
            broken.length
              ? broken.map((s) => s.name).join(", ")
              : "every expression parses and every timezone resolves"
          }
        />
      </StatGroup>

      {loading && !schedules.length && (
        <Skeleton variant="text" rows={4} label="Loading schedules" />
      )}
      <QueryState
        error={error}
        loading={loading}
        empty={
          schedules.length
            ? undefined
            : {
                title: "No recurring work is defined",
                hint: "Add a schedules block to a role or a unit in the company configuration — a standup, a nightly audit, a weekly report.",
              }
        }
      >
        <Card padding="none">
          <Card.Header icon={<CalendarTodayGlyph size="sm" />} count={schedules.length}>
            <Card.Title>Defined</Card.Title>
          </Card.Header>
          <DataGrid
            rows={schedules}
            rowKey={rowID}
            // A SCHEDULE HAS AN ADDRESS, and it is its whole identity: two
            // units may each declare a "standup", and a role and a unit may
            // both, so the trail is all three segments. THE FRAME'S OWN
            // ANSWER to where that address lives, rather than a second copy
            // of the route — the rail's `Open ↗` is built from the same
            // reference, so the link a row carries and the way out of the
            // panel it opens can never name different pages.
            rowHref={(s) => peekHref(scheduleRef(s.scope_type, s.scope_id, s.name))}
            onRowActivate={(s, e) => openSchedule(scheduleRef(s.scope_type, s.scope_id, s.name), e)}
            defaultSort="next"
            columns={[
              {
                key: "name",
                header: "Name",
                sortValue: (s) => s.name,
                cell: (s) => (
                  <span className="row gap-1">
                    <span className="truncate">{s.name}</span>
                    {!s.enabled && <Tag appearance="outline">disabled</Tag>}
                  </span>
                ),
              },
              {
                key: "scope",
                header: "Scope",
                shrink: true,
                sortValue: (s) => `${s.scope_type}:${s.scope_id}`,
                // A ROLE SCOPE IS A SEAT and a unit scope is not, which is
                // why only half of this column is a cell: `SeatCell` is the
                // one rendering of a person in a grid, and a unit is a name
                // with no avatar, no state and no page of people behind it.
                cell: (s) =>
                  s.scope_type === "role" ? (
                    <SeatCell handle={s.scope_id} />
                  ) : (
                    <Tag appearance="outline">{s.scope_id}</Tag>
                  ),
              },
              {
                key: "cron",
                header: "Cron",
                shrink: true,
                sortValue: (s) => s.cron,
                cell: (s) => {
                  // THE EXPRESSION AND WHAT IT MEANS. A reader who knows cron
                  // reads `0 9 * * 1-5`; a founder reads five numbers and a
                  // dash on the one screen that says when the company wakes
                  // itself up. An expression this build cannot read shows as
                  // itself with nothing claimed about it.
                  const said = describeCron(s.cron);
                  return (
                    <span className="col" style={{ gap: 1 }}>
                      <code
                        className="inline nowrap"
                        title={s.timezone ? `timezone: ${s.timezone}` : undefined}
                      >
                        {s.cron}
                      </code>
                      {said && <span className="t-caption faint">{said}</span>}
                    </span>
                  );
                },
              },
              {
                key: "task",
                header: "Task",
                cell: (s) => <TextCell>{s.task}</TextCell>,
              },
              {
                // WHO A FIRE REACHES, resolved by the engine from the scope
                // and the target — a unit schedule targeting `each` wakes
                // every member, one targeting `lead` wakes one seat, and a
                // role schedule's target is meaningless rather than defaulted.
                // A CHIP LIST RATHER THAN `SeatCell`: this cell holds up to
                // three people and a remainder, which is a composite the
                // one-seat cell cannot be.
                key: "runners",
                header: "Wakes",
                shrink: true,
                cell: (s) =>
                  s.runners?.length ? (
                    <span className="row gap-1">
                      {s.runners.slice(0, 3).map((handle) => (
                        <SeatChip key={handle} name={handle} handle={handle} />
                      ))}
                      {s.runners.length > 3 && (
                        <span className="t-caption">+{s.runners.length - 3}</span>
                      )}
                    </span>
                  ) : (
                    <Dash title="nobody — this schedule wakes no seat" />
                  ),
              },
              {
                key: "next",
                header: "Next",
                shrink: true,
                sortValue: (s) => tsKey(s.next_run) || Number.MAX_SAFE_INTEGER,
                // NOT `DateCell`: this is the only column in the product that
                // reads FORWARD, and `relTime` spells a future instant as an
                // elapsed one.
                cell: (s) =>
                  s.next_run ? (
                    <span className="t-caption" title={fmtDateTime(s.next_run)}>
                      {inTime(s.next_run, now)}
                    </span>
                  ) : s.problem ? (
                    // THE REASON, not a blank. A schedule whose timezone was
                    // renamed shows nothing under Next and looks merely idle.
                    <Tag variant="danger" title={s.problem}>
                      {s.problem}
                    </Tag>
                  ) : (
                    <Dash title="disabled, or the calendar never reaches it" />
                  ),
              },
              {
                key: "last",
                header: "Last",
                shrink: true,
                sortValue: (s) => tsKey(lastFire.get(rowID(s))?.fired_at ?? ""),
                cell: (s) => {
                  const run = lastFire.get(rowID(s));
                  if (!run) return <Dash title="this schedule has never fired" />;
                  return (
                    <span className="row gap-1">
                      <DateCell at={run.fired_at} now={now} />
                      {run.outcome && (
                        <Tag variant={OUTCOME_TONE[run.outcome] ?? "neutral"}>{run.outcome}</Tag>
                      )}
                    </span>
                  );
                },
              },
            ]}
          />
        </Card>

        <Card padding="none">
          <Card.Header icon={<ScheduleGlyph size="sm" />} count={runs.length}>
            <Card.Title>Recent runs</Card.Title>
          </Card.Header>
          <DataGrid
            name="runs"
            rows={runs}
            rowKey={(r) => `${r.fired_at}:${runID(r)}:${r.fire_label}`}
            // A FIRE IS NOT AN OBJECT — the ledger row has no page of its own
            // — so a row here opens the SCHEDULE it is a fire of. That is the
            // question this table raises: a reader scanning the company's
            // fires stops at one and asks what it was.
            rowHref={(r) => peekHref(scheduleRef(r.scope_type, r.scope_id, r.schedule_name))}
            onRowActivate={(r, e) =>
              openSchedule(scheduleRef(r.scope_type, r.scope_id, r.schedule_name), e)
            }
            defaultSort="-fired"
            empty={{
              title: "No runs recorded",
              hint: "A run is recorded when a schedule fires. Nothing has fired since this node started keeping the record.",
            }}
            columns={[
              {
                key: "fired",
                header: "Fired",
                shrink: true,
                sortValue: (r) => tsKey(r.fired_at),
                cell: (r) => <DateCell at={r.fired_at} now={now} />,
              },
              {
                key: "name",
                header: "Schedule",
                sortValue: (r) => r.schedule_name,
                cell: (r) => <TextCell>{r.schedule_name}</TextCell>,
              },
              {
                key: "scope",
                header: "Scope",
                shrink: true,
                sortValue: (r) => `${r.scope_type}:${r.scope_id}`,
                cell: (r) => <Tag appearance="outline">{r.scope_id}</Tag>,
              },
              {
                // WHICH SEAT this fire woke. A unit schedule targeting `each`
                // writes one ledger row per member, so without this column
                // three rows read as one fire repeated.
                key: "target",
                header: "Woke",
                shrink: true,
                cell: (r) => <SeatCell handle={r.target_handle} />,
              },
              {
                key: "outcome",
                header: "Outcome",
                shrink: true,
                sortValue: (r) => r.outcome,
                // A BADGE, NOT `StatusCell`: the ledger's two words are a
                // dispatch vocabulary rather than a lifecycle, and this is
                // the pill the Last column above draws for the same value.
                // AN ABSENT OUTCOME IS NOT A WORD, though — a badge reading
                // "—" claims the ledger recorded something it did not.
                cell: (r) =>
                  r.outcome ? (
                    <Tag
                      variant={OUTCOME_TONE[r.outcome] ?? "neutral"}
                      title={
                        r.outcome === "skipped_catchup"
                          ? "the tick was missed and fell outside the catchup window"
                          : undefined
                      }
                    >
                      {r.outcome}
                    </Tag>
                  ) : (
                    <Dash title="the ledger recorded no outcome for this fire" />
                  ),
              },
              {
                // THE TICK, which is not the instant it ran: a catchup run
                // fires now for a tick that was due earlier, and the pair is
                // the only way to see that. ABSOLUTE rather than `DateCell`
                // for exactly that reason — two relative times an hour apart
                // read as one fire, and the tick's own label is the ledger's
                // at-most-once key.
                key: "due",
                header: "For the tick",
                shrink: true,
                sortValue: (r) => tsKey(r.scheduled_at),
                cell: (r) =>
                  r.scheduled_at ? (
                    <span className="t-caption" title={r.fire_label}>
                      {fmtDateTime(r.scheduled_at)}
                    </span>
                  ) : (
                    <Dash title="the ledger recorded no tick for this fire" />
                  ),
              },
            ]}
          />
        </Card>
      </QueryState>
    </>
  );
}

/**
 * One schedule: what it is, and every fire this node's ledger still holds.
 *
 * # Why this is its own read
 *
 * `schedules.recent_runs` is the COMPANY's fifty most recent fires. A company
 * with twenty hourly schedules fills that page in two and a half hours, so a
 * screen wanting one schedule's history had to page the company's and filter —
 * which is exactly the shape that makes a reader conclude a schedule stopped
 * running when it simply fell off the end.
 *
 * # A schedule the config no longer declares still has a history
 *
 * The ledger outlives the configuration: a schedule somebody removed has rows
 * until the retention sweep takes them, and this page shows them. So the
 * header degrades to the identity rather than refusing — "there is no such
 * schedule" would be wrong about the rows underneath it.
 */
function OneSchedule({
  scopeType,
  scopeId,
  name,
  row,
  known,
  runs,
  truncated,
  loading,
  error,
  now,
}: {
  scopeType: string;
  scopeId: string;
  name: string;
  row?: ScheduleRow;
  /** Whether the company's schedules have answered — see the caller. */
  known: boolean;
  runs: ScheduleRunRow[];
  truncated: boolean;
  loading: boolean;
  error: string | null;
  now: number;
}) {
  usePageLabels({ [[scopeType, scopeId, name].join("/")]: name });
  return (
    <>
      <PageActions>
        {
          <a className="t-link" href={href(["activity", "schedules"])}>
            All schedules →
          </a>
        }
      </PageActions>

      {/* THE SCOPE, THE CONDITION AND THE THREE TILES ARE THE HEADER'S NOW.
          The scope was a badge in the page bar and the condition was two more
          beside it — a schedule's identity drawn in the strip that holds a
          screen's controls — and the tiles under them repeated the three
          facts the header already carries. Nothing is lost: the expression's
          description rides beside the expression, the reason a schedule
          cannot fire is still the Next fact, and the runners are still named
          in full. */}
      <ObjectHeader
        kind="Schedule"
        icon="calendar_today"
        identifier={`${scopeType}:${scopeId}`}
        title={name}
        // NEITHER THE BADGE NOR THE NOTE MAKES THE CLAIM BEFORE THE ANSWER
        // DOES. "No longer declared" is a statement about the company
        // configuration, and a read still in flight has not seen one.
        status={known ? scheduleStatus(row) : undefined}
        facts={row ? scheduleFacts(row, now) : undefined}
      />
      <PageNote>
        {row ? row.task : known ? UNDECLARED_HINT : "One schedule, and every fire it has left."}
      </PageNote>

      {row && <ScheduleDefinition row={row} />}
      {row && <NextFires row={row} now={now} count={UPCOMING_FIRES} />}

      <QueryState
        error={error}
        loading={loading}
        empty={
          runs.length
            ? undefined
            : {
                title: "This schedule has not fired",
                hint: "A run is recorded when a schedule fires. Nothing has fired since this node started keeping the record.",
              }
        }
      >
        <Card padding="none">
          <Card.Header
            icon={<ScheduleGlyph size="sm" />}
            count={runs.length}
            // SAYS WHEN IT CUT: a page that filled is indistinguishable from a
            // schedule that has fired exactly that many times.
            subtitle={
              truncated ? `the newest ${runs.length} — older fires are past this page` : undefined
            }
          >
            <Card.Title>Fires</Card.Title>
          </Card.Header>
          <DataGrid
            name="fires"
            rows={runs}
            rowKey={(r) => `${r.fired_at}:${r.fire_label}:${r.target_handle}`}
            defaultSort="-fired"
            columns={[
              {
                key: "fired",
                header: "Fired",
                shrink: true,
                sortValue: (r) => tsKey(r.fired_at),
                cell: (r) => <DateCell at={r.fired_at} now={now} />,
              },
              {
                key: "target",
                header: "Woke",
                shrink: true,
                cell: (r) => <SeatCell handle={r.target_handle} />,
              },
              {
                key: "outcome",
                header: "Outcome",
                shrink: true,
                sortValue: (r) => r.outcome,
                cell: (r) =>
                  r.outcome ? (
                    <Tag variant={OUTCOME_TONE[r.outcome] ?? "neutral"}>{r.outcome}</Tag>
                  ) : (
                    <Dash title="the ledger recorded no outcome for this fire" />
                  ),
              },
              {
                // THE TICK, which is not the instant it ran: a catchup fire
                // runs now for a tick that was due earlier, and the pair is
                // the only way to see that — so it stays absolute where the
                // column beside it is relative.
                key: "due",
                header: "For the tick",
                shrink: true,
                sortValue: (r) => tsKey(r.scheduled_at),
                cell: (r) =>
                  r.scheduled_at ? (
                    <span className="t-caption" title={r.fire_label}>
                      {fmtDateTime(r.scheduled_at)}
                    </span>
                  ) : (
                    <Dash title="the ledger recorded no tick for this fire" />
                  ),
              },
            ]}
          />
        </Card>
      </QueryState>
    </>
  );
}

/**
 * One schedule, beside the list it was found in.
 *
 * # Two reads, because a schedule's definition and its history are two facts
 *
 * The DEFINITION is pushed on a config apply and arrives in the company-wide
 * `schedules` answer; the HISTORY is the dispatch ledger and is not pushed at
 * all. The rail asks for both rather than filtering the company's fifty most
 * recent fires, for the reason the page does: twenty hourly schedules fill
 * that page in two and a half hours, and a schedule whose fires fell off the
 * end is indistinguishable from one that stopped running.
 *
 * # An address with no definition behind it is not an error
 *
 * The ledger outlives the configuration. A schedule somebody removed keeps
 * its fires until the retention sweep takes them, so the rail degrades to the
 * identity and says which of the two absences it is — and only an address
 * with neither a definition nor a fire is "no such schedule".
 */
export function SchedulePeek({ scope }: { scope: string }) {
  const now = useNow();
  // THE ADDRESS IS ALL THREE SEGMENTS — see [scheduleRef]. A plain split is
  // right because the engine SLUGS a schedule's name, so the only separators
  // in the token are the two this screen put there.
  const [scopeType = "", scopeId = "", name = ""] = scope.split("/");
  const addressed = Boolean(scopeType && scopeId && name);

  const defined = useQuery("schedules", undefined, { enabled: addressed, pollMs: 30_000 });
  const history = useQuery(
    "schedule_runs",
    { scope_type: scopeType, scope_id: scopeId, name },
    { enabled: addressed, pollMs: 30_000 },
  );

  const row = defined.data?.schedules?.find(
    (s) => s.scope_type === scopeType && s.scope_id === scopeId && s.name === name,
  );
  const runs = history.data?.runs ?? [];
  const loading = defined.loading || history.loading;
  const answered = Boolean(defined.data) && Boolean(history.data);

  return (
    <>
      <ObjectHeader
        size="peek"
        kind="Schedule"
        icon="calendar_today"
        identifier={`${scopeType}:${scopeId}`}
        // THE NAME OUT OF THE ADDRESS, not out of the row: a schedule the
        // configuration has dropped still has a name, and a rail that waited
        // for a row to title itself would draw an empty header over a list of
        // real fires.
        title={name || scope}
        status={answered ? scheduleStatus(row) : undefined}
        facts={row ? scheduleFacts(row, now) : undefined}
      />
      <div className="col gap-3">
        {loading && !answered && <Skeleton variant="text" rows={6} label="Loading the schedule" />}
        <QueryState
          error={defined.error || history.error}
          loading={loading}
          empty={
            answered && !row && runs.length === 0
              ? { title: `No schedule at “${scope}”`, hint: NO_SCHEDULE_HINT }
              : undefined
          }
        >
          {answered && (
            <>
              {/* WHICH ABSENCE THIS IS, before anything below it. A rail
                  showing fires under no definition reads as a rail that
                  failed to load one. */}
              {!row && runs.length > 0 && <p className="t-caption">{UNDECLARED_HINT}</p>}

              {row && <ScheduleDefinition row={row} />}
              {row && <NextFires row={row} now={now} count={PEEK_FIRES} />}

              <Card padding="none">
                <Card.Header
                  icon={<ScheduleGlyph size="sm" />}
                  count={runs.length}
                  // WHICH OF THE COUNT IS DRAWN. The chip is the ledger's own
                  // total and the rows are the newest few of it — a panel that
                  // said 12 over three rows and nothing else would read as a
                  // list that failed to finish rendering.
                  subtitle={runs.length > PEEK_RUNS ? `the newest ${PEEK_RUNS} of them` : undefined}
                >
                  <Card.Title>Last fires</Card.Title>
                </Card.Header>
                {runs.length > 0 ? (
                  <div className="list">
                    {runs.slice(0, PEEK_RUNS).map((r) => (
                      <div
                        key={`${r.fired_at}:${r.fire_label}:${r.target_handle}`}
                        className="thread-entry"
                      >
                        <div className="row gap-1">
                          {r.outcome ? (
                            <Tag variant={OUTCOME_TONE[r.outcome] ?? "neutral"}>{r.outcome}</Tag>
                          ) : (
                            <Dash title="the ledger recorded no outcome for this fire" />
                          )}
                          <span className="spacer" />
                          <span className="t-caption" title={fmtDateTime(r.fired_at)}>
                            {relTime(r.fired_at, now)}
                          </span>
                        </div>
                        <div className="row gap-1">
                          <SeatCell handle={r.target_handle} />
                          <span className="spacer" />
                          {/* THE TICK BESIDE THE FIRE, which is the only way
                              to see a catchup: this ran now for something
                              that was due earlier. */}
                          <span className="t-caption faint" title={r.fire_label}>
                            {r.scheduled_at ? `for ${fmtDateTime(r.scheduled_at)}` : "no tick"}
                          </span>
                        </div>
                      </div>
                    ))}
                  </div>
                ) : (
                  <EmptyState
                    size="compact"
                    icon={<ScheduleGlyph size={32} />}
                    title="This schedule has not fired"
                    description="A run is recorded when a schedule fires. Nothing has fired since this node started keeping the record."
                  />
                )}
              </Card>
            </>
          )}
        </QueryState>
      </div>
    </>
  );
}
