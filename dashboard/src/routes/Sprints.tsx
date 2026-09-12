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
 * # Read-only, like every other work screen
 *
 * A sprint is started, closed and rolled over by the engine's own duty or by
 * a lead's tool call. A button here would write as "the dashboard", which is
 * not a person and cannot be asked why.
 */

import { ScreenHead } from "~/app/Shell.tsx";
import { useParam } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { Badge, Banner, Empty, Panel, Stat, StatRow } from "~/ui/primitives.tsx";
import { Select } from "~/ui/primitives.tsx";
import { DataTable } from "~/ui/DataTable.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtDate } from "~/lib/format.ts";
import type { WorkSprintRow } from "~/protocol/index.ts";

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
  const [project, setProject] = useParam("project", "");
  // THE PROJECT LIST IS THE COMPANY'S, so this screen can be reached with no
  // project chosen and still offer one — a sprint report needs a project and
  // a screen that could not name one would be a dead end.
  const projects = useQuery("work_projects", undefined, { pollMs: 60_000 });
  const keys = (projects.data?.projects ?? []).map((p) => p.key);
  const chosen = project || keys[0] || "";

  const report = useQuery(
    "work_sprints",
    chosen ? { project: chosen } : undefined,
    { pollMs: 60_000 },
  );
  const sprints = report.data?.sprints ?? [];
  const measure = measureLabel(report.data?.measure ?? "points");

  return (
    <>
      <ScreenHead
        title="Sprints"
        sub="What each sprint took on, what arrived after it started, and what actually shipped inside its own window."
      />

      <div className="toolbar">
        <Select
          value={chosen}
          onChange={setProject}
          ariaLabel="Project"
          anyLabel="Pick a project"
          options={keys}
        />
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

              {[...sprints].reverse().map((s) => <SprintPanel key={s.number} sprint={s} />)}
            </>
          )}
        </QueryState>
      )}
    </>
  );
}

/** One sprint's figures, its breakdown and the two things a lead acts on.
 *
 *  Exported for its own tests: every number here has a wrong form that reads
 *  as a different fact rather than as a missing one. */
export function SprintPanel({ sprint }: { sprint: WorkSprintRow }) {
  const f = sprint.figures;
  const measure = measureLabel(f.measure);
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
        {fmtDate(sprint.start_at)} → {sprint.closed_at ? fmtDate(sprint.closed_at) : fmtDate(sprint.end_at)}
        {sprint.days_remaining !== undefined && ` · ${sprint.days_remaining}d left`}
        {sprint.goal ? ` · ${sprint.goal}` : ""}
      </p>
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
            { key: "handle", header: "Who", cell: (a) => a.handle },
            { key: "total", header: "Holding", cell: (a) => a.total },
            { key: "done", header: "Done", cell: (a) => a.done },
            { key: "remaining", header: "Remaining", cell: (a) => a.remaining },
            {
              key: "capacity",
              header: "Capacity",
              // AN UNDECLARED CAPACITY IS A DASH, never a zero: rendering
              // zero would put every assignee permanently over.
              cell: (a) =>
                a.capacity === undefined ? (
                  "—"
                ) : (
                  <Badge tone={a.over_capacity ? "caution" : "neutral"}>{a.capacity}</Badge>
                ),
            },
          ]}
        />
      )}
    </Panel>
  );
}
