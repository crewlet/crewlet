/**
 * What an item's properties and links CLAIM.
 *
 * The two panels here are where a task's stored values meet the words a person
 * uses for them, and every one of these cases is a value whose raw form is
 * meaningless on screen: an option's uuid under a field heading, a dependency
 * labelled from the wrong end, a zero where nobody has written anything.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { ItemLinks, ItemProps, Subtasks, linkHeading } from "./WorkItem.tsx";
import type { WorkItem, WorkItemDetail, WorkProjectDetail } from "~/protocol/index.ts";

afterEach(cleanup);

const NOW = Date.parse("2031-04-16T12:00:00Z");

const task = (over: Partial<WorkItem> = {}): WorkItem => ({
  id: "t-1",
  key: "ENG-42",
  project: "ENG",
  title: "Fix the login race",
  status: "in_progress",
  version: 1,
  ...over,
});

const detail = (over: Partial<WorkItemDetail> = {}): WorkItemDetail => ({
  task: task(),
  complete: true,
  ...over,
});

const project = (over: Partial<WorkProjectDetail> = {}): WorkProjectDetail => ({
  key: "ENG",
  name: "Engineering",
  unit: { resolved: true },
  lead: {},
  task_counts: { open: 1, done: 0, closed: 0 },
  version: 1,
  statuses: [],
  types: [],
  fields: [
    {
      fields: [
        {
          id: "f-sev",
          slug: "severity",
          name: "Severity",
          type: "dropdown",
          config: { options: [{ id: "o-1", slug: "high", name: "High" }] },
        },
      ],
    },
  ],
  policy_stamp: 1,
  complete: true,
  ...over,
});

// A CHOICE STORES THE OPTION'S ID — which is what lets a company rename an
// option without orphaning every task that chose it — so a panel that showed
// the stored value would print a uuid under a field heading.
test("a custom field renders by its name, with its option's own word", () => {
  render(
    <ItemProps
      detail={detail({
        fields: [
          { id: "f-sev", slug: "severity", name: "Severity", type: "dropdown", value: "o-1" },
        ],
      })}
      chrome={{}}
      project={project()}
    />,
  );
  expect(screen.getByText("Severity")).toBeTruthy();
  expect(screen.getByText("High")).toBeTruthy();
  expect(screen.queryByText("o-1")).toBeNull();
});

// A VALUE A FILTER CANNOT REACH SAYS SO IN A WORD. "Archived", "from another
// tracker" and "undeclared" are three different facts, and dimming all three
// the same would invite somebody to filter on a field that cannot be filtered.
test("a value whose declaration was archived is marked as such", () => {
  render(
    <ItemProps
      detail={detail({
        fields: [
          {
            id: "f-sev",
            slug: "severity",
            name: "Severity",
            type: "dropdown",
            value: "o-1",
            hidden: true,
          },
        ],
      })}
      chrome={{}}
      project={project()}
    />,
  );
  expect(screen.getByText("archived")).toBeTruthy();
});

// AN ABSENT VALUE IS AN EM DASH, never a zero: zero is a measurement, and an
// unestimated task is one nobody has sized.
test("an unestimated task shows a dash rather than a zero", () => {
  const { container } = render(<ItemProps detail={detail()} chrome={{}} project={project()} />);
  expect(container.textContent).toContain("—");
  expect(container.textContent).not.toContain("0m");
});

// A HAND-OFF COUNT IS A BUDGET, not trivia: an item handed on too many times
// has stopped being work and started being a hot potato, and the engine
// refuses the next hand-off rather than letting it circle.
test("a task near its hand-off cap is flagged", () => {
  const { container } = render(
    <ItemProps
      detail={detail({ task: task({ reassignments: 7 }) })}
      chrome={{}}
      project={project()}
    />,
  );
  expect(screen.getByText("Hand-offs")).toBeTruthy();
  expect(container.querySelector("dd svg")).toBeTruthy();
});

// WATCHING AND MUTED ARE DIFFERENT FACTS AND BOTH TRAVEL: a set carrying only
// the difference would silently re-add everybody on the next mention.
test("a muted watcher is listed and said to be muted", () => {
  render(
    <ItemProps
      detail={detail({ task: task({ watchers: ["ada", "bo"], muted: ["bo"] }) })}
      chrome={{ seatName: (h) => h }}
      project={project()}
    />,
  );
  expect(screen.getByText("Watching")).toBeTruthy();
  expect(screen.getByText("(muted)")).toBeTruthy();
});

// ROUTING IS SHOWN ONLY WHERE IT HAS MOVED. The pair being equal is the
// ordinary case, and repeating it in two rows is noise that buries the one
// task whose routing actually changed.
test("a task routed where it was filed says so once", () => {
  const { rerender } = render(
    <ItemProps
      detail={detail({ task: task({ filed_unit: "platform", routing_unit: "platform" }) })}
      chrome={{}}
      project={project()}
    />,
  );
  expect(screen.queryByText("Routes to")).toBeNull();
  rerender(
    <ItemProps
      detail={detail({ task: task({ filed_unit: "platform", routing_unit: "payments" }) })}
      chrome={{}}
      project={project()}
    />,
  );
  expect(screen.getByText("Routes to")).toBeTruthy();
});

// ---------------------------------------------------------------------------
// Links
// ---------------------------------------------------------------------------

// THE DERIVED HALF OF A DEPENDENCY IS THE ONE NOBODY AUTHORED, so calling both
// ends "waiting on" would tell a reader their task is blocked by the very task
// it is blocking.
test("a dependency is named from the end it is read at", () => {
  expect(linkHeading({ kind: "waiting_on", other: "x" })).toBe("Waiting on");
  expect(linkHeading({ kind: "waiting_on", other: "x", derived: true })).toBe("Blocks");
  expect(linkHeading({ kind: "duplicates", other: "x" })).toBe("Duplicates");
  expect(linkHeading({ kind: "duplicates", other: "x", derived: true })).toBe("Duplicated by");
});

test("links are grouped under the word each end earns", () => {
  render(
    <ItemLinks
      detail={detail({
        links: [
          { kind: "waiting_on", other: "t-2", key: "ENG-2", title: "the blocker" },
          { kind: "waiting_on", other: "t-3", key: "ENG-3", title: "the dependent", derived: true },
        ],
      })}
      chrome={{}}
    />,
  );
  expect(screen.getByText("Waiting on")).toBeTruthy();
  expect(screen.getByText("Blocks")).toBeTruthy();
});

// THE REPAIR AND THE FINDING ARE DIFFERENT STATES: the first is a commit the
// tracker duty writes 30 seconds later, the second is an edge whose mirror was
// refused for good and which a person has to resolve.
test("a pending mirror and a permanent one-sided edge read differently", () => {
  render(
    <ItemLinks
      detail={detail({
        links: [
          { kind: "waiting_on", other: "t-2", key: "ENG-2", one_sided: true },
          {
            kind: "waiting_on",
            other: "t-4",
            key: "ENG-4",
            one_sided: true,
            one_sided_final: true,
          },
        ],
      })}
      chrome={{}}
    />,
  );
  expect(screen.getByText("mirror pending")).toBeTruthy();
  expect(screen.getByText("one-sided")).toBeTruthy();
});

// A PAGE LINK LEAVES THE TRACKER, and it has to go to the knowledge base
// rather than to a task key that does not exist.
test("a linked page points at the page rather than at a task", () => {
  const { container } = render(
    <ItemLinks
      detail={detail({ links: [{ kind: "page", other: "p-1", title: "The runbook" }] })}
      chrome={{}}
    />,
  );
  expect(screen.getByText("Pages")).toBeTruthy();
  expect(container.querySelector("a.mono")?.getAttribute("href")).toBe("#/pages/p-1");
});

// NO LINKS IS NO PANEL. A panel headed "Links" over nothing reads as a task
// whose links failed to load.
test("a task with no links draws no panel at all", () => {
  const { container } = render(<ItemLinks detail={detail()} chrome={{}} />);
  expect(container.textContent).toBe("");
});

// A FAILED SUBTREE READ IS NOT AN EMPTY ONE.
//
// The subtasks read is the only thing on this screen that can see a task's
// children, so nothing contradicts it: a refusal that draws no panel states
// that the task is a leaf. The panel took the rows and never the error, so
// every failure rendered as the fact that a task has no subtasks.
test("a refused subtree read says so rather than looking like a leaf", () => {
  render(<Subtasks rows={[]} error="bad_params" now={NOW} chrome={{}} />);
  expect(screen.getByText("Subtasks")).toBeTruthy();
  expect(document.querySelector(".banner")).toBeTruthy();
});

// AND A READ THAT ANSWERED IS ALLOWED TO CONCLUDE IT. An empty answer is a
// fact about the task, so the panel stays away and the screen does not carry
// a heading over nothing.
test("a task that answered with no children draws no panel", () => {
  const { container } = render(<Subtasks rows={[]} error={null} now={NOW} chrome={{}} />);
  expect(container.textContent).toBe("");
});

// AND AN ERROR OUTRANKS ROWS ALREADY ON SCREEN, so a failing poll is never
// presented as the current shape of the tree.
test("a refused poll says so even when children were listed before", () => {
  render(
    <Subtasks
      rows={[
        {
          id: "t-2",
          key: "ENG-43",
          project: "ENG",
          title: "A child",
          type: "task",
          status: "todo",
          updated: "2031-04-16T09:00:00Z",
          version: 1,
        },
      ]}
      error="query_failed"
      now={NOW}
      chrome={{}}
    />,
  );
  expect(document.querySelector(".banner")).toBeTruthy();
  expect(screen.queryByText("A child")).toBeNull();
});
