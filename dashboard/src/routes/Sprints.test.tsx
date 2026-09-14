/**
 * The renderings on the sprint panels whose WRONG form reads as a different
 * fact rather than as a missing one.
 *
 * Every case here is a number a lead acts on. A zero where the honest answer
 * is "nobody has said" is the failure mode a screen has and a log does not:
 * a velocity of 0 reads as a team that delivers nothing, and a capacity of 0
 * puts everybody permanently over.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { Burndown, SprintPanel, Velocity } from "./Sprints.tsx";
import { describeChange } from "~/lib/work.ts";
import { ProjectHead } from "./Work.tsx";
import type { WorkBurndown, WorkProjectDetail, WorkSprintRow } from "~/protocol/index.ts";

afterEach(cleanup);

const sprint = (over: Partial<WorkSprintRow> = {}): WorkSprintRow => ({
  project: "ENG",
  number: 4,
  name: "Kickoff",
  state: "active",
  start_at: "2031-04-01T00:00:00Z",
  end_at: "2031-04-15T00:00:00Z",
  version: 1,
  figures: {
    measure: "points",
    committed: 20,
    added: 5,
    removed: 2,
    done: 8,
    remaining: 15,
    open_after_close: 0,
    tasks: 10,
    unestimated: 0,
  },
  ...over,
});

// AN UNDECLARED CAPACITY IS A DASH, never a zero and never a bar. A policy
// that names nobody would otherwise render every assignee as over capacity,
// which is a finding a lead reorganises a sprint around — and a bar with no
// maximum is a claim about a limit nobody set.
test("an assignee with no declared capacity renders an em-dash", () => {
  render(
    <SprintPanel
      sprint={sprint({
        by_assignee: [{ handle: "ada", committed: 8, done: 3, remaining: 5, total: 8, tasks: 4 }],
      })}
    />,
  );
  expect(screen.getByText("—")).toBeTruthy();
  expect(document.querySelector(".table .meter-fill")).toBeNull();
});

test("a declared capacity somebody is over is marked as such", () => {
  const { container } = render(
    <SprintPanel
      sprint={sprint({
        by_assignee: [
          {
            handle: "ada",
            committed: 8,
            done: 3,
            remaining: 5,
            total: 8,
            tasks: 4,
            capacity: 21,
            over_capacity: true,
          },
        ],
      })}
    />,
  );
  // THE BAR'S TONE IS THE SIGNAL. The number alone says nothing about whether
  // it has been exceeded, and a capacity column that renders the same either
  // way is a column nobody reads. It is the SERVER's verdict rather than a
  // comparison of our own, for the reason the overdue flag is: two places
  // deriving one predicate is how one screen says over and another does not.
  // INSIDE THE TABLE, because the panel now carries a delivery bar of its
  // own: a selector that took the first meter on the screen would be
  // asserting the sprint's own progress and calling it a capacity.
  const fill = container.querySelector(".table .meter-fill");
  expect(fill).toBeTruthy();
  expect(fill?.getAttribute("data-tone")).toBe("critical");
  expect(screen.getByText("8 / 21")).toBeTruthy();
  expect(screen.queryByText("—")).toBeNull();
});

// THE MEASURE IS RENDERED, because a bare "20" is points to one team and
// minutes to another — and a sprint report read in the wrong unit is worse
// than no report.
test("a sprint in minutes says minutes, not points", () => {
  render(
    <SprintPanel
      sprint={sprint({
        figures: { ...sprint().figures, measure: "estimate_min" },
      })}
    />,
  );
  expect(screen.getAllByText(/minutes/).length).toBeGreaterThan(0);
  expect(screen.queryByText(/points/)).toBeNull();
});

// UNESTIMATED IS SURFACED. A sprint reporting 8 of 25 points done where half
// its tasks carry no estimate is reporting on half a sprint, and a reader who
// cannot see that quotes the number.
test("unestimated tasks are called out rather than folded into the totals", () => {
  render(<SprintPanel sprint={sprint({ figures: { ...sprint().figures, unestimated: 6 } })} />);
  expect(screen.getByText(/6 of 10 tasks carry no points/)).toBeTruthy();
});

test("a sprint with everything estimated says nothing about it", () => {
  render(<SprintPanel sprint={sprint()} />);
  expect(screen.queryByText(/carry no points/)).toBeNull();
});

// A PENDING SPILLOVER IS THE ONE THING ON THIS SCREEN A LEAD HAS TO ACT ON,
// and a closed sprint that does not say so looks finished.
test("a closed sprint whose spillover nobody settled is marked", () => {
  render(<SprintPanel sprint={sprint({ state: "closed", rollover_pending: true })} />);
  expect(screen.getByText("spillover pending")).toBeTruthy();
});

const project = (over: Partial<WorkProjectDetail> = {}): WorkProjectDetail => ({
  key: "ENG",
  name: "Engineering",
  unit: { key: "platform", name: "Platform", resolved: true },
  lead: { handle: "ada", kind: "agent" },
  task_counts: { open: 12, done: 40, closed: 3 },
  version: 1,
  statuses: [],
  types: [],
  fields: [],
  policy_stamp: 1,
  complete: true,
  ...over,
});

// A UNIT THE CHART NO LONGER HAS IS A FINDING, not a blank. It is what leaves
// a project's work routed to nobody, and a screen that renders it as an
// ordinary unit hides exactly that.
test("a project orphaned from the chart says so", () => {
  render(<ProjectHead detail={project({ unit: { key: "dissolved", resolved: false } })} />);
  expect(screen.getByText(/does not have/)).toBeTruthy();
});

test("a project whose unit resolves says nothing about it", () => {
  render(<ProjectHead detail={project()} />);
  expect(screen.queryByText(/does not have/)).toBeNull();
});

// AND THE OVERVIEW NAMES ITS MEASURE TOO. The strip shows "8 / 25" beside a
// project, and read in the wrong unit that is a team half as productive or
// twice as slow as it is.
test("the overview's sprint figure says which unit it is in", () => {
  render(
    <ProjectHead
      detail={project({
        sprints: {
          active: {
            number: 4,
            name: "Kickoff",
            state: "active",
            end_at: "2031-04-15T00:00:00Z",
            days_remaining: 3,
            figures: { ...sprint().figures, measure: "estimate_min" },
          },
        },
      })}
    />,
  );
  expect(screen.getByText(/minutes/)).toBeTruthy();
  expect(screen.queryByText(/points/)).toBeNull();
});

// A PROJECT THAT RUNS NO SPRINTS SAYS SO rather than rendering an empty one,
// AND THE TWO REASONS READ DIFFERENTLY: "this team does not work in sprints"
// and "this team is between sprints" send a reader to different places, and
// one sentence over both told them nothing. The answer distinguishes them —
// a project that runs sprints carries a POLICY whether or not one is open.
test("a project with no sprints says which kind of none it is", () => {
  render(<ProjectHead detail={project()} />);
  expect(screen.getByText("not used")).toBeTruthy();
  cleanup();
  render(<ProjectHead detail={project({ sprint_policy: { length_days: 14 } })} />);
  expect(screen.getByText("none running")).toBeTruthy();
});

// AND NOTHING AT ALL WHILE THE READ IS IN FLIGHT. An overview drawn with
// zeroes is a project that looks unstaffed, unled and out of sprint.
test("no overview is drawn before the answer arrives", () => {
  const { container } = render(<ProjectHead detail={null} />);
  expect(container.textContent).toBe("");
});

// A CHANGE IS RENDERED FROM ITS DELTAS, because "todo → in_progress" is what
// every reader of one wants and the kind alone does not say it.
test("a change with deltas renders them", () => {
  expect(
    describeChange({
      id: "r",
      log_seq: 1,
      log_stream: "s",
      log_generation: 1,
      at: "2031-04-16T00:00:00Z",
      effective_at: "2031-04-16T00:00:00Z",
      kind: "status",
      subject_kind: "task",
      subject_id: "t",
      notified: true,
      fields: { status: { from: "todo", to: "in_progress" } },
    }),
  ).toBe("status: todo → in_progress");
});

// AN EMPTY SIDE RENDERS AS AN EM-DASH rather than as nothing: "assignee:  →
// ada" reads as a rendering bug, where "assignee: — → ada" reads as an
// assignment.
test("an empty side of a delta renders as a dash", () => {
  expect(
    describeChange({
      id: "r",
      log_seq: 1,
      log_stream: "s",
      log_generation: 1,
      at: "2031-04-16T00:00:00Z",
      effective_at: "2031-04-16T00:00:00Z",
      kind: "assignee",
      subject_kind: "task",
      subject_id: "t",
      notified: true,
      fields: { assignee: { from: "", to: "ada" } },
    }),
  ).toBe("assignee: — → ada");
});

// A REMOVAL RENDERS AS A DASH ON THE OTHER SIDE TOO — "assignee: ada → " is
// the same rendering bug read from the other end, where "ada → —" is somebody
// being taken off a task.
test("a cleared value renders as a dash", () => {
  expect(
    describeChange({
      id: "r",
      log_seq: 1,
      log_stream: "s",
      log_generation: 1,
      at: "2031-04-16T00:00:00Z",
      effective_at: "2031-04-16T00:00:00Z",
      kind: "assignee",
      subject_kind: "task",
      subject_id: "t",
      notified: true,
      fields: { assignee: { from: "ada", to: "" } },
    }),
  ).toBe("assignee: ada → —");
});

// A COMMENT HAS NO DELTAS AND IS THE THING MOST WORTH READING, so the excerpt
// is what a feed shows for it — the kind alone says only that somebody spoke.
test("a change with no deltas renders its excerpt", () => {
  expect(
    describeChange({
      id: "r",
      log_seq: 1,
      log_stream: "s",
      log_generation: 1,
      at: "2031-04-16T00:00:00Z",
      effective_at: "2031-04-16T00:00:00Z",
      kind: "comment",
      subject_kind: "task",
      subject_id: "t",
      notified: true,
      excerpt: "rolled back, the migration was the cause",
    }),
  ).toBe("rolled back, the migration was the cause");
});

// AND A CHANGE WITH NEITHER DELTAS NOR AN EXCERPT IS STILL A CHANGE. Rendered
// blank it would look like a rendering bug where it is a real commit — a
// watcher added, a rank moved — so the kind is the last resort.
test("a change with nothing to show falls back to its kind", () => {
  expect(
    describeChange({
      id: "r",
      log_seq: 1,
      log_stream: "s",
      log_generation: 1,
      at: "2031-04-16T00:00:00Z",
      effective_at: "2031-04-16T00:00:00Z",
      kind: "comment_resolved",
      subject_kind: "task",
      subject_id: "t",
      notified: false,
    }),
  ).toBe("comment resolved");
});

// ---------------------------------------------------------------------------
// The burndown
// ---------------------------------------------------------------------------

const burndown = (over: Partial<WorkBurndown> = {}): WorkBurndown => ({
  project: "ENG",
  sprint: 4,
  name: "Kickoff",
  measure: "points",
  start_at: "2031-04-01T00:00:00Z",
  end_at: "2031-04-15T00:00:00Z",
  points: [
    { at: "2031-04-01T00:00:00Z", scope: 20, remaining: 20, delivered: 0 },
    { at: "2031-04-02T00:00:00Z", scope: 20, remaining: 17, delivered: 3 },
  ],
  ideal: 20,
  tasks: 10,
  unestimated: 0,
  complete: true,
  ...over,
});

// A SPRINT WITH NO LIVED DAY KEEPS ITS PANEL AND SAYS WHY. Its series is one
// point, and a chart of one point invites a conclusion from nothing — but
// drawing NOTHING is the other mistake: a burndown simply absent from an
// active sprint's report reads as a chart that failed to load, which is what
// every empty state in this product is written to avoid.
test("a sprint with no lived day says so rather than vanishing", () => {
  const { container } = render(
    <Burndown
      data={burndown({
        points: [{ at: "2031-04-01T00:00:00Z", scope: 20, remaining: 20, delivered: 0 }],
      })}
      sprint={sprint()}
    />,
  );
  expect(screen.getByText("Nothing to draw yet")).toBeTruthy();
  // And it names the day the first point arrives with, so the reader knows
  // this is a schedule rather than a fault.
  expect(container.textContent).toContain("2031");
  // And no series is drawn from one point — the panel's own icon is the
  // only vector in it.
  expect(screen.queryByText("Remaining")).toBeNull();
  expect(screen.queryByText("Ideal")).toBeNull();
  expect(container.querySelector("svg[role='img']")).toBeNull();
});

// A LIVED SPRINT DRAWS ALL THREE SERIES, and the ideal is a REFERENCE rather
// than a fourth measurement: dashed, so nobody reads it as a line that
// happened to be straight.
test("a running sprint draws remaining, scope and a dashed ideal", () => {
  render(<Burndown data={burndown()} sprint={sprint()} />);
  for (const name of ["Remaining", "Scope", "Ideal"]) {
    expect(screen.getAllByText(name).length).toBeGreaterThan(0);
  }
});

// UNESTIMATED WORK IS NAMED ABOVE THE CHART, never folded in: a series over a
// sprint half of whose tasks carry no value is a series about half a sprint,
// and a reader who cannot see that quotes the number.
test("a partly unestimated sprint says what its chart leaves out", () => {
  render(<Burndown data={burndown({ unestimated: 4, tasks: 10 })} sprint={sprint()} />);
  expect(screen.getByText(/4 of 10 tasks carry no points/)).toBeTruthy();
});

// AND A REFUSED SERIES KEEPS ITS PANEL TOO, which is the same rule as the
// unlived sprint above and was the one case that did not follow it: the panel
// took `data` and `loading` and never `error`, so `!data` swallowed a refusal
// and a failure rendered as the panel not existing. It is drawn ONLY for a
// running sprint, so its absence reads as a statement about the work — "this
// sprint has no burndown" — rather than as a question that failed.
test("a refused series keeps its panel and says why", () => {
  render(<Burndown data={null} error="bad_params" sprint={sprint()} />);
  // The panel is there...
  expect(screen.getByText(/burndown/i)).toBeTruthy();
  // ...and it carries the refusal rather than an empty chart.
  expect(screen.queryByText("Remaining")).toBeNull();
  expect(document.querySelector(".banner")).toBeTruthy();
});

// AND AN ERROR OUTRANKS A STALE SERIES, because the reading on screen would
// otherwise be presented as current while the poll behind it is failing.
test("a refused poll says so even when a series was drawn before", () => {
  render(<Burndown data={burndown()} error="query_failed" sprint={sprint()} />);
  expect(document.querySelector(".banner")).toBeTruthy();
});

// ---------------------------------------------------------------------------
// Delivery by sprint
// ---------------------------------------------------------------------------

// A SPRINT THAT HAS NOT STARTED HAS NO DELIVERY. The cadence duty mints future
// sprints ahead of time, and every row was drawn — so a project with three
// planned sprints ahead of it drew three bars reading "0 of 0 points" in a
// chart whose whole subject is a trend, and three-fifths of that trend was
// a measurement of sprints that had not happened.
test("delivery by sprint draws only sprints that have run", () => {
  const { container } = render(
    <Velocity
      measure="points"
      sprints={[
        sprint({
          number: 1,
          name: "One",
          state: "closed",
          figures: { ...sprint().figures, done: 12 },
        }),
        sprint({
          number: 2,
          name: "Two",
          state: "active",
          figures: { ...sprint().figures, done: 8 },
        }),
        sprint({
          number: 3,
          name: "Three",
          state: "future",
          figures: { ...sprint().figures, committed: 0, added: 0, done: 0 },
        }),
      ]}
    />,
  );
  const text = container.textContent ?? "";
  expect(text).toContain("1 · One");
  expect(text).toContain("2 · Two");
  expect(text).not.toContain("3 · Three");
});

// AND THE CHART DOES NOT DRAW ITSELF FOR ONE MEASUREMENT. A trend needs two
// points; with one closed sprint and four planned ones the old filter-free
// version had five bars and this has none.
test("delivery by sprint needs two sprints that have run", () => {
  const { container } = render(
    <Velocity
      measure="points"
      sprints={[
        sprint({ number: 1, state: "closed" }),
        sprint({ number: 2, state: "future" }),
        sprint({ number: 3, state: "future" }),
      ]}
    />,
  );
  // Nothing is drawn at all: one measurement is not a trend.
  expect(container.textContent).toBe("");
});
