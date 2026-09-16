/**
 * One project's sprints: what was committed, what arrived, what shipped.
 *
 * # Every figure here is derived from the STAYS, and none of them is stored
 *
 * A sprint's numbers are predicates over one table of memberships: committed
 * is what was in the sprint when it STARTED, added is what arrived after,
 * removed is what was pulled out, and remaining is what is still open at the
 * end. A counter maintained forward could not say any of it, because
 * "committed" is a statement about a past instant and a counter is a statement
 * about now.
 *
 * # Done is a delivery, not a membership
 *
 * A task can sit in a sprint the whole way through and never be finished, so
 * `done` counts the tasks that ENTERED a delivered status inside the sprint's
 * own window, which is also why a sprint that closed last month does not keep
 * gaining velocity as its leftovers land.
 *
 * # Read-only, like every other work screen
 *
 * A sprint is started, closed and rolled over by the engine's own duty or by
 * a lead's tool call. A button here would write as "the dashboard", which is
 * not a person and cannot be asked why.
 */

import { useParam } from "~/app/router.tsx";
import { QueryState, SeatChip } from "~/components/common.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtDate } from "~/lib/format.ts";
import {
  Callout,
  Card,
  EmptyState,
  EmptyValue,
  PageHeader,
  Select,
  Stack,
  StatCard,
  StatGroup,
  Table,
  Tag,
  Toolbar,
} from "@crewlethq/ui";
import type { Tone } from "@crewlethq/ui";
import { CalendarClockGlyph, FolderGlyph, TimelineGlyph } from "@crewlethq/icons/glyphs";
import type { WorkSprintRow } from "~/protocol/index.ts";

/** The measure's own word, because a bare number is points to one team and
 *  minutes to another.
 *
 *  EXPORTED AS THE ONE SPELLING OF THE RULE. The board's project overview
 *  draws the same figure and spells it again inline, defaulting the other way,
 *  so a measure neither of them recognises is named "points" on this screen and
 *  "minutes" on that one for the same sprint. A figure read in the wrong unit
 *  is the failure this screen's suite exists to catch, so there is one rule to
 *  read rather than two to keep in step. */
export function measureLabel(measure: string): string {
  return measure === "estimate_min" ? "minutes" : "points";
}

const STATE_TONE: Record<string, Tone> = {
  future: "neutral",
  active: "info",
  closed: "success",
};

export function Sprints() {
  const [project, setProject] = useParam("project", "");
  // THE PROJECT LIST IS THE COMPANY'S, so this screen can be reached with no
  // project chosen and still offer one: a sprint report needs a project, and a
  // screen that could not name one would be a dead end.
  const projects = useQuery("work_projects", undefined, { pollMs: 60_000 });
  const keys = (projects.data?.projects ?? []).map((p) => p.key);
  const chosen = project || keys[0] || "";

  // NOT UNTIL ONE IS CHOSEN, see the same guard on the board's project
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

  return (
    <>
      <PageHeader
        title="Sprints"
        description="What each sprint took on, what arrived after it started, and what actually shipped inside its own window."
        badges={chosen ? <Tag appearance="outline">{chosen}</Tag> : undefined}
      />

      {/* WHICH PROJECT, not a filter. This control names the screen's subject
          rather than narrowing a list, which is also why it carries no `active`
          mark: the accent says "this is narrowing what you are reading", and a
          picker that holds a project from the first read onwards would wear it
          for the life of the screen and mean nothing by it. */}
      <Toolbar label="Which project" mode="group" sticky>
        <Select
          ariaLabel="Project"
          value={chosen}
          width="auto"
          placeholder="Pick a project"
          // THE SAME PICKER THE BOARD DRAWS, over the same catalogue, so it
          // carries the same search rather than growing one at some count. The
          // project list grows with the work, and a control that behaved
          // differently on two companies would do so for a reason no reader of
          // either can see.
          searchable
          options={keys.map((key) => ({ value: key, label: key }))}
          onChange={(value) => setProject(String(value))}
        />
      </Toolbar>

      {!chosen && projects.data && (
        <EmptyState
          icon={<FolderGlyph />}
          title="No projects"
          description="A sprint belongs to a project, and this company has none yet."
        />
      )}

      {chosen && (
        <QueryState error={report.error} loading={report.loading}>
          {sprints.length === 0 ? (
            <EmptyState
              icon={<TimelineGlyph />}
              title="No sprints"
              description={`${chosen} does not run sprints. A lead turns them on with the project's sprint policy.`}
            />
          ) : (
            <>
              {/* VELOCITY IS ABSENT UNTIL A SPRINT HAS CLOSED, and the screen
                  says so rather than drawing a zero, which would read as a
                  team that delivers nothing. */}
              <StatGroup columns={3}>
                <StatCard
                  label="Velocity"
                  // THE MARK NAMES THE ABSENCE AND THE LINE UNDER IT GIVES THE
                  // REASON, rather than both saying the same sentence: the
                  // label is read in place of the dash, so a reader who is told
                  // it would otherwise hear "no sprint has closed yet" twice in
                  // a row and learn nothing the second time.
                  value={report.data?.velocity_avg ?? <EmptyValue label="Not measured" />}
                  sub={
                    report.data?.velocity_avg === undefined
                      ? "no sprint has closed yet"
                      : `mean ${measure} per closed sprint`
                  }
                />
                <StatCard
                  label="Sprints shown"
                  value={sprints.length}
                  sub="the window this answer covers"
                />
                <StatCard
                  label="Earlier"
                  value={report.data?.earlier_sprints_dropped ?? 0}
                  sub="closed sprints below this window"
                />
              </StatGroup>

              {sprints.some((s) => s.rollover_pending) && (
                <Callout variant="warning">
                  A closed sprint's spillover is still undecided: its unfinished work is neither
                  carried forward nor dropped until a lead says which.
                </Callout>
              )}

              {[...sprints].reverse().map((s) => (
                <SprintPanel key={s.number} sprint={s} />
              ))}
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
    <Card as="section">
      <Card.Header
        icon={<CalendarClockGlyph size="sm" />}
        actions={
          <>
            <Tag variant={STATE_TONE[sprint.state] ?? "neutral"}>{sprint.state}</Tag>
            {sprint.rollover_pending && <Tag variant="warning">spillover pending</Tag>}
            {sprint.archived && <Tag appearance="outline">archived</Tag>}
          </>
        }
      >
        <Card.Title>{`${sprint.number} · ${sprint.name}`}</Card.Title>
      </Card.Header>

      <Stack gap={3}>
        <p className="t-caption">
          {fmtDate(sprint.start_at)} →{" "}
          {sprint.closed_at ? fmtDate(sprint.closed_at) : fmtDate(sprint.end_at)}
          {sprint.days_remaining !== undefined && ` · ${sprint.days_remaining}d left`}
          {sprint.goal ? ` · ${sprint.goal}` : ""}
        </p>

        {/* FIVE FIGURES, TWO ROWS, and the split is the sentence this screen
            makes: what the sprint took on, then what came of it.

            It is two groups rather than one of five because a stat row is
            SINGLE-ROW and draws its hairlines BETWEEN COLUMNS only, and five
            is not one of the counts it takes. The fourth tile of a
            three-across group starts a second row with no rule above it and a
            stray rule down the group's own left edge, so the numbers would run
            together exactly where a reader needs them separated. Only the
            narrow layout, which wraps the row on purpose, draws the rules a
            wrapped row needs. */}
        <StatGroup columns={3}>
          <StatCard label="Committed" value={f.committed} sub={measure} />
          <StatCard label="Added" value={f.added} sub="arrived after the start" />
          <StatCard label="Removed" value={f.removed} sub="pulled out, not carried" />
        </StatGroup>
        <StatGroup columns={2}>
          <StatCard label="Done" value={f.done} sub="delivered inside the window" />
          <StatCard label="Remaining" value={f.remaining} sub={`${f.tasks} tasks`} />
        </StatGroup>

        {/* UNESTIMATED IS THE HONESTY COLUMN. A sprint reporting 8 of 34 points
            done where half its tasks carry no estimate is reporting on half a
            sprint. */}
        {f.unestimated > 0 && (
          <Callout variant="info">
            {f.unestimated} of {f.tasks} tasks carry no {measure}, so these figures describe only
            the rest.
          </Callout>
        )}

        {sprint.by_assignee && sprint.by_assignee.length > 0 && (
          // THE PACKAGE'S PLAIN TABLE, not the record table every list screen
          // draws. A record table holds the reader's page, sort and column
          // choices in the URL under one name, and this screen draws one of
          // these per sprint: five panels would read and write one set of
          // parameters between them, so paging one would page all five. A
          // handful of rows with no sort and no pager is what Table is for.
          <Table
            caption={`Who is holding ${sprint.name}, in ${measure}`}
            captionHidden
            headers={["Who", "Holding", "Done", "Remaining", "Capacity"]}
            data={sprint.by_assignee.map((a) => [
              <SeatChip name={a.handle} handle={a.handle} />,
              a.total,
              a.done,
              a.remaining,
              // AN UNDECLARED CAPACITY IS A MARKED ABSENCE, never a zero:
              // rendering zero would put every assignee permanently over.
              a.capacity === undefined ? (
                <EmptyValue label="Not declared" />
              ) : (
                <Tag variant={a.over_capacity ? "warning" : "neutral"}>{a.capacity}</Tag>
              ),
            ])}
          />
        )}
      </Stack>
    </Card>
  );
}
