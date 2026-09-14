/**
 * One project's sprints — what was committed, what arrived, what shipped.
 *
 * # Every figure here is derived from the STAYS, and none of them is stored
 *
 * A sprint's numbers are predicates over one table of memberships: committed
 * is what was in the sprint when it STARTED, added is what arrived after,
 * removed is what was pulled out, and remaining is what is still open at the
 * end. A counter maintained forward could not say any of it — "committed" is
 * a statement about a past instant, and a counter is a statement about now.
 *
 * # Done is a delivery, not a membership
 *
 * A task can sit in a sprint the whole way through and never be finished, so
 * `done` counts the tasks that ENTERED a delivered status inside the sprint's
 * own window — which is also why a sprint that closed last month does not
 * keep gaining velocity as its leftovers land.
 *
 * # The two charts answer the two questions a sprint has
 *
 * ACROSS sprints: is this team's delivery steady — a ranked comparison, in
 * sprint order, because the order is the content. WITHIN one: is this sprint
 * going to land — a series, drawn to today rather than to the sprint's end so
 * a running sprint is not a flat line into its own future.
 *
 * # Read-only, like every other work screen
 *
 * A sprint is started, closed and rolled over by the engine's own duty or by
 * a lead's tool call. A button here would write as "the dashboard", which is
 * not a person and cannot be asked why.
 */

import { useMemo } from "react";
import { ScreenHead } from "~/app/Shell.tsx";
import { href, useParam } from "~/app/router.tsx";
import { QueryState, SeatChip } from "~/components/common.tsx";
import { Coverage } from "~/components/work.tsx";
import { Badge, Banner, Empty, Meter, Panel, Stat, StatRow } from "~/ui/primitives.tsx";
import { Select } from "~/ui/primitives.tsx";
import { BarList, Legend, TimeSeries } from "~/ui/charts.tsx";
import { DataTable } from "~/ui/DataTable.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg } from "~/lib/seats.ts";
import { fmtDate, tsKey } from "~/lib/format.ts";
import type { WorkBurndown, WorkSprintRow } from "~/protocol/index.ts";

/** The measure's own word, because a bare number is points to one team and
 *  minutes to another. */
function measureLabel(measure: string): string {
  return measure === "estimate_min" ? "minutes" : "points";
}

const STATE_TONE: Record<string, "positive" | "caution" | "info" | "neutral"> = {
  future: "neutral",
  active: "info",
  closed: "positive",
};

export function Sprints() {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const [project, setProject] = useParam("project", "");
  // THE PROJECT LIST IS THE COMPANY'S, so this screen can be reached with no
  // project chosen and still offer one — a sprint report needs a project and
  // a screen that could not name one would be a dead end.
  const projects = useQuery("work_projects", undefined, { pollMs: 60_000 });
  const keys = (projects.data?.projects ?? []).map((p) => p.key);
  const chosen = project || keys[0] || "";

  // NOT UNTIL ONE IS CHOSEN — see the same guard on the board's project
  // overview. `chosen` is empty while the catalogue is still loading and stays
  // empty for a company with no projects, and the engine refuses the question
  // without one, so the screen's own empty state is the honest rendering
  // rather than a `bad_params` banner for a project nobody named.
  const report = useQuery("work_sprints", chosen ? { project: chosen } : undefined, {
    enabled: chosen !== "",
    pollMs: 60_000,
  });
  const sprints = report.data?.sprints ?? [];
  const measure = measureLabel(report.data?.measure ?? "points");

  // THE ONE SPRINT WORTH A SERIES is the running one: a closed sprint's shape
  // is history and its outcome is already the velocity bar above, where a
  // running one is the question somebody opened this screen to ask.
  const active = sprints.find((s) => s.state === "active");
  const burndown = useQuery(
    "work_burndown",
    // BOTH HALVES FROM ONE ANSWER. A sprint is numbered PER PROJECT, so a
    // number paired with a key from somewhere else names a sprint that need
    // not exist: `chosen` changes the instant somebody picks a project and
    // `active` is still the previous project's report until the new one
    // lands, so every switch asked for the old number under the new key. The
    // row carries its own project, which makes the pair consistent by
    // construction — the panel shows the report it belongs to until the
    // replacement arrives, exactly as the rest of this screen does.
    active ? { project: active.project, sprint: active.number } : undefined,
    // FIVE MINUTES: a burndown moves by the day, and its own points are day
    // boundaries — a faster poll would redraw an identical series.
    { enabled: Boolean(active), pollMs: 300_000 },
  );

  const seatName = (handle: string) => index.byHandle.get(handle)?.name ?? handle;

  return (
    <>
      <ScreenHead
        title="Sprints"
        sub="What each sprint took on, what arrived after it started, and what actually shipped inside its own window."
        actions={
          <a className="t-link" href={href(["work"], chosen ? { project: chosen } : undefined)}>
            Tracker →
          </a>
        }
      />

      <div className="toolbar">
        <Select
          value={chosen}
          onChange={setProject}
          ariaLabel="Project"
          anyLabel="Pick a project"
          options={keys}
        />
        <span className="spacer" />
        <Coverage answer={report.data} />
      </div>

      {!chosen && projects.data && (
        <Empty
          title="No projects"
          hint="A sprint belongs to a project, and this company has none yet."
        />
      )}

      {chosen && (
        <QueryState error={report.error} loading={report.loading}>
          {sprints.length === 0 ? (
            <Empty
              title="No sprints"
              hint={`${chosen} does not run sprints — a lead turns them on with the project's sprint policy.`}
            />
          ) : (
            <>
              {/* VELOCITY IS ABSENT UNTIL A SPRINT HAS CLOSED, and the screen
                  says so rather than drawing a zero — which would read as a
                  team that delivers nothing. */}
              <StatRow cols={3}>
                <Stat
                  label="Velocity"
                  value={report.data?.velocity_avg ?? "—"}
                  sub={
                    report.data?.velocity_avg === undefined
                      ? "no sprint has closed yet"
                      : `mean ${measure} per closed sprint`
                  }
                />
                <Stat label="Sprints shown" value={sprints.length} />
                <Stat
                  label="Earlier"
                  value={report.data?.earlier_sprints_dropped ?? 0}
                  sub="closed sprints below this window"
                />
              </StatRow>

              {sprints.some((s) => s.rollover_pending) && (
                <Banner tone="caution">
                  A closed sprint's spillover is still undecided — its unfinished work is neither
                  carried forward nor dropped until a lead says which.
                </Banner>
              )}

              <Velocity sprints={sprints} measure={measure} />

              {active && (
                <Burndown
                  data={burndown.data}
                  loading={burndown.loading}
                  error={burndown.error}
                  sprint={active}
                />
              )}

              {[...sprints].reverse().map((s) => (
                <SprintPanel key={s.number} sprint={s} seatName={seatName} />
              ))}
            </>
          )}
        </QueryState>
      )}
    </>
  );
}

/**
 * Delivery across the recent sprints.
 *
 * IN SPRINT ORDER, oldest first, and NOT sorted by size: this is a sequence
 * rather than a ranking, and re-ordering it by how much each sprint delivered
 * would destroy the one thing a reader is looking for — whether the team is
 * steady, climbing or falling.
 *
 * ONE SERIES WITH ONE MEMBER HIGHLIGHTED, which is the emphasis form: the
 * running sprint is a partial total and every closed one is final, so a single
 * hue across all of them would invite the reader to compare an unfinished
 * sprint against finished ones as though they were the same measurement. No
 * legend, because a legend for one series is a box saying what the title says.
 */
export function Velocity({ sprints, measure }: { sprints: WorkSprintRow[]; measure: string }) {
  if (sprints.length < 2) return null;
  // THE SCALE IS THE LARGEST SPRINT'S WHOLE COMMITMENT, so a bar reads as a
  // fraction of what that sprint took on rather than of what the best sprint
  // delivered — the second makes an ordinary sprint beside an exceptional one
  // look like a failure.
  const max = Math.max(1, ...sprints.map((s) => s.figures.committed + s.figures.added));
  return (
    <Panel
      title="Delivery by sprint"
      icon="activity"
      subtitle="the running sprint in colour; its total is still partial"
    >
      <BarList
        max={max}
        data={sprints.map((s) => ({
          label: `${s.number} · ${s.name}`,
          value: s.figures.done,
          display: `${s.figures.done} of ${s.figures.committed + s.figures.added} ${measure}`,
          color: s.state === "active" ? "var(--viz-1)" : "var(--viz-other)",
          sub:
            s.state === "closed"
              ? `${fmtDate(s.start_at)} — ${fmtDate(s.closed_at ?? s.end_at)}${
                  s.figures.remaining ? ` · ${s.figures.remaining} ${measure} carried out` : ""
                }`
              : `${fmtDate(s.start_at)} — ${fmtDate(s.end_at)}`,
        }))}
        emptyLabel="No sprint in this window has delivered anything yet."
      />
    </Panel>
  );
}

/**
 * The running sprint, day by day.
 *
 * THREE LINES AND ONLY ONE OF THEM IS A MEASUREMENT OF PROGRESS. Remaining is
 * the question; scope is what makes it answerable, because a falling line over
 * a rising scope is a team keeping up rather than a team finishing; and the
 * ideal is a dashed REFERENCE, drawn so it cannot be mistaken for a series
 * that happened to be linear.
 *
 * A SPRINT WITH NO LIVED DAY DRAWS NO CURVE AND SAYS WHY: its series is one
 * point, and a chart of one point invites a conclusion from nothing. It keeps
 * its PANEL, because a burndown simply absent from an active sprint's report
 * reads as a chart that failed to load.
 */
export function Burndown({
  data,
  loading,
  error,
  sprint,
}: {
  data?: WorkBurndown | null;
  loading?: boolean;
  error?: string | null;
  sprint: WorkSprintRow;
}) {
  // A REFUSED SERIES KEEPS ITS PANEL, for the same reason a sprint with no
  // lived day does: this panel is drawn only for a sprint that is RUNNING, so
  // its absence reads as "this sprint has no burndown" — a statement about the
  // work — when what happened is that the question failed. `QueryState` is
  // where every code is turned into a sentence, so the refusal says which.
  if (error) {
    return (
      <Panel title={`Sprint ${sprint.number} burndown`} icon="activity" subtitle={sprint.name}>
        <QueryState error={error} loading={false} />
      </Panel>
    );
  }
  if (loading && !data) return null;
  if (!data) return null;
  const measure = measureLabel(data.measure);
  // A SPRINT WITH NO LIVED DAY SAYS SO rather than vanishing. The series
  // stops at the reader's own instant, so one point means the clock is at or
  // before the sprint's start — and a chart of one point invites a
  // conclusion from nothing. Drawing NOTHING was the other mistake: a
  // burndown panel that is simply absent from an active sprint's report
  // reads as a chart that failed to load, which is what every empty state in
  // this product is written to avoid.
  if (data.points.length < 2) {
    return (
      <Panel title={`Sprint ${data.sprint} burndown`} icon="activity" subtitle={sprint.name}>
        <Empty
          title="Nothing to draw yet"
          hint={`This sprint's window opens on ${fmtDate(data.start_at)}. A burndown needs a day that has been lived through, so the first point arrives with it.`}
        />
      </Panel>
    );
  }
  const from = tsKey(data.start_at);
  const to = tsKey(data.end_at);
  const last = data.points[data.points.length - 1];

  return (
    <Panel
      title={`Sprint ${data.sprint} burndown`}
      icon="activity"
      subtitle={`${sprint.name} · in ${measure}`}
      actions={
        sprint.days_remaining !== undefined && <Badge outline>{sprint.days_remaining}d left</Badge>
      }
    >
      {/* UNESTIMATED WORK IS NAMED ABOVE THE CHART, never folded in: a series
          over a sprint half of whose tasks carry no value is a series about
          half a sprint, and a reader who cannot see that quotes the number. */}
      {data.unestimated > 0 && (
        <Banner tone="info">
          {data.unestimated} of {data.tasks} tasks carry no {measure}, so this chart describes only
          the rest.
        </Banner>
      )}
      <TimeSeries
        from={from}
        to={to}
        height={160}
        label={`Sprint ${data.sprint} burndown`}
        format={(n) => `${n} ${measure}`}
        series={[
          {
            name: "Scope",
            color: "var(--viz-other)",
            points: data.points.map((p) => ({ t: tsKey(p.at), v: p.scope })),
          },
          {
            name: "Ideal",
            color: "var(--border-strong)",
            dashed: true,
            points: [
              { t: from, v: data.ideal },
              { t: to, v: 0 },
            ],
          },
          {
            name: "Remaining",
            color: "var(--viz-1)",
            // THE ONE FILLED SERIES, because it is the one the chart is
            // about: an area under every line would blend three translucent
            // washes into a fourth colour nobody chose.
            fill: true,
            points: data.points.map((p) => ({ t: tsKey(p.at), v: p.remaining })),
          },
        ]}
      />
      <Legend
        items={[
          { label: "Remaining", color: "var(--viz-1)" },
          { label: "Scope", color: "var(--viz-other)" },
          { label: "Ideal", color: "var(--border-strong)" },
        ]}
      />
      {last && (
        <p className="t-caption">
          {last.remaining} {measure} still to do of {last.scope} in the sprint; {last.delivered}{" "}
          delivered.
          {/* THE GAP IS ABANDONED WORK, and it is the one quantity two lines
              cannot show: `cancelled` is finished and undelivered, so it
              leaves the remaining line without joining the delivered one. */}
          {last.scope - last.remaining - last.delivered > 0
            ? ` ${(last.scope - last.remaining - last.delivered).toLocaleString()} ${measure} were cancelled rather than delivered.`
            : ""}
        </p>
      )}
    </Panel>
  );
}

/** One sprint's figures, its breakdown and the two things a lead acts on.
 *
 *  Exported for its own tests: every number here has a wrong form that reads
 *  as a different fact rather than as a missing one. */
export function SprintPanel({
  sprint,
  seatName,
}: {
  sprint: WorkSprintRow;
  seatName?: (handle: string) => string;
}) {
  const f = sprint.figures;
  const measure = measureLabel(f.measure);
  const committed = f.committed + f.added;
  return (
    <Panel
      title={`${sprint.number} · ${sprint.name}`}
      actions={
        <>
          <Badge tone={STATE_TONE[sprint.state] ?? "neutral"}>{sprint.state}</Badge>
          {sprint.rollover_pending && <Badge tone="caution">spillover pending</Badge>}
          {sprint.archived && <Badge tone="neutral">archived</Badge>}
        </>
      }
    >
      <p className="muted">
        {fmtDate(sprint.start_at)} →{" "}
        {sprint.closed_at ? fmtDate(sprint.closed_at) : fmtDate(sprint.end_at)}
        {sprint.days_remaining !== undefined && ` · ${sprint.days_remaining}d left`}
        {sprint.goal ? ` · ${sprint.goal}` : ""}
      </p>
      {/* THE SPRINT'S OWN PROGRESS AS A SHAPE, beside the five numbers that
          say what it is made of. The measure is on the bar's own right, so a
          reader never has to find it in a column heading. */}
      {committed > 0 && (
        <Meter
          used={f.done}
          max={committed}
          ariaLabel={`Sprint ${sprint.number} — delivered`}
          label="Delivered"
          right={`${f.done} of ${committed} ${measure}`}
          fullMeans="achieved"
        />
      )}
      <StatRow cols={5}>
        <Stat label="Committed" value={f.committed} sub={measure} />
        <Stat label="Added" value={f.added} sub="arrived after the start" />
        <Stat label="Removed" value={f.removed} sub="pulled out, not carried" />
        <Stat label="Done" value={f.done} sub="delivered inside the window" />
        <Stat label="Remaining" value={f.remaining} sub={`${f.tasks} tasks`} />
      </StatRow>
      {/* UNESTIMATED IS THE HONESTY COLUMN. A sprint reporting 8 of 34 points
          done where half its tasks carry no estimate is reporting on half a
          sprint. */}
      {f.unestimated > 0 && (
        <Banner tone="info">
          {f.unestimated} of {f.tasks} tasks carry no {measure}, so these figures describe only the
          rest.
        </Banner>
      )}
      {sprint.by_assignee && sprint.by_assignee.length > 0 && (
        <DataTable
          rows={sprint.by_assignee}
          rowKey={(a) => a.handle}
          columns={[
            {
              key: "handle",
              header: "Who",
              cell: (a) => <SeatChip name={seatName?.(a.handle) ?? a.handle} handle={a.handle} />,
            },
            { key: "total", header: "Holding", cell: (a) => a.total },
            { key: "done", header: "Done", cell: (a) => a.done },
            { key: "remaining", header: "Remaining", cell: (a) => a.remaining },
            {
              key: "capacity",
              header: "Capacity",
              // AN UNDECLARED CAPACITY IS A DASH, never a zero or a bar:
              // rendering zero would put every assignee permanently over, and
              // a bar with no maximum is a claim about a limit nobody set.
              cell: (a) =>
                a.capacity === undefined ? (
                  "—"
                ) : (
                  <span style={{ minWidth: 120, display: "block" }}>
                    <Meter
                      used={a.total}
                      max={a.capacity}
                      ariaLabel={`${a.handle} — committed against capacity`}
                      right={`${a.total} / ${a.capacity}`}
                      fullMeans="spent"
                      {...(a.over_capacity ? { tone: "critical" as const } : {})}
                    />
                  </span>
                ),
            },
          ]}
        />
      )}
    </Panel>
  );
}
