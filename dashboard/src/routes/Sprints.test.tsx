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
import { SprintPanel } from "./Sprints.tsx";
import { describeChange, ProjectOverview } from "./Work.tsx";
import type { WorkProjectDetail, WorkSprintRow } from "~/protocol/index.ts";

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

// AN UNDECLARED CAPACITY IS A DASH, never a zero. A policy that names nobody
// would otherwise render every assignee as over capacity, which is a finding
// a lead reorganises a sprint around.
test("an assignee with no declared capacity renders an em-dash", () => {
  render(
    <SprintPanel
      sprint={sprint({
        by_assignee: [
          { handle: "ada", committed: 8, done: 3, remaining: 5, total: 8, tasks: 4 },
        ],
      })}
    />,
  );
  expect(screen.getByText("—")).toBeTruthy();
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
  // THE BADGE'S TONE IS THE SIGNAL. The number alone says nothing about
  // whether it has been exceeded, and a capacity column that renders the
  // same either way is a column nobody reads.
  const badge = [...container.querySelectorAll(".badge")].find(
    (el) => el.textContent === "21",
  );
  expect(badge).toBeTruthy();
  expect(badge?.className).toMatch(/caution/);
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
  render(
    <SprintPanel sprint={sprint({ state: "closed", rollover_pending: true })} />,
  );
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
  render(<ProjectOverview detail={project({ unit: { key: "dissolved", resolved: false } })} />);
  expect(screen.getByText(/does not have/)).toBeTruthy();
});

test("a project whose unit resolves says nothing about it", () => {
  render(<ProjectOverview detail={project()} />);
  expect(screen.queryByText(/does not have/)).toBeNull();
});

// AND THE OVERVIEW NAMES ITS MEASURE TOO. The strip shows "8 / 25" beside a
// project, and read in the wrong unit that is a team half as productive or
// twice as slow as it is.
test("the overview's sprint figure says which unit it is in", () => {
  render(
    <ProjectOverview
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

// A PROJECT THAT RUNS NO SPRINTS SAYS SO rather than rendering an empty one:
// "this team does not work in sprints" and "this team has none right now" are
// different facts.
test("a project with no sprints renders none rather than a zeroed one", () => {
  render(<ProjectOverview detail={project()} />);
  expect(screen.getByText("this project runs none")).toBeTruthy();
});

// AND NOTHING AT ALL WHILE THE READ IS IN FLIGHT. An overview drawn with
// zeroes is a project that looks unstaffed, unled and out of sprint.
test("no overview is drawn before the answer arrives", () => {
  const { container } = render(<ProjectOverview detail={null} />);
  expect(container.textContent).toBe("");
});

// A CHANGE IS RENDERED FROM ITS DELTAS, because "todo → in_progress" is what
// every reader of one wants and the kind alone does not say it.
test("a change with deltas renders them", () => {
  expect(
    describeChange({
      id: "r", log_seq: 1, log_stream: "s", log_generation: 1,
      at: "2031-04-16T00:00:00Z", effective_at: "2031-04-16T00:00:00Z",
      kind: "status", subject_kind: "task", subject_id: "t", notified: true,
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
      id: "r", log_seq: 1, log_stream: "s", log_generation: 1,
      at: "2031-04-16T00:00:00Z", effective_at: "2031-04-16T00:00:00Z",
      kind: "assignee", subject_kind: "task", subject_id: "t", notified: true,
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
      id: "r", log_seq: 1, log_stream: "s", log_generation: 1,
      at: "2031-04-16T00:00:00Z", effective_at: "2031-04-16T00:00:00Z",
      kind: "assignee", subject_kind: "task", subject_id: "t", notified: true,
      fields: { assignee: { from: "ada", to: "" } },
    }),
  ).toBe("assignee: ada → —");
});

// A COMMENT HAS NO DELTAS AND IS THE THING MOST WORTH READING, so the excerpt
// is what a feed shows for it — the kind alone says only that somebody spoke.
test("a change with no deltas renders its excerpt", () => {
  expect(
    describeChange({
      id: "r", log_seq: 1, log_stream: "s", log_generation: 1,
      at: "2031-04-16T00:00:00Z", effective_at: "2031-04-16T00:00:00Z",
      kind: "comment", subject_kind: "task", subject_id: "t", notified: true,
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
      id: "r", log_seq: 1, log_stream: "s", log_generation: 1,
      at: "2031-04-16T00:00:00Z", effective_at: "2031-04-16T00:00:00Z",
      kind: "comment_resolved", subject_kind: "task", subject_id: "t",
      notified: false,
    }),
  ).toBe("comment resolved");
});
