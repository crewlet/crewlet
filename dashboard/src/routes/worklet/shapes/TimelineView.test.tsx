/**
 * A grouped timeline's bands: headed in the company's own words, and saying
 * where the rest of a band is.
 *
 * The failure modes: a band headed with the stored value (`in_progress`, a
 * seat's handle), because the answer labels only the axes whose value is not
 * a word already; and a band whose page stopped at its row cap while its count
 * said the whole column, with no way to the rows past it.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { TimelineView } from "./Timeline.tsx";
import type { WorkGroup, WorkSummary } from "~/protocol/index.ts";

afterEach(cleanup);

const NOW = Date.parse("2031-04-16T09:00:00Z");

const row = (id: string): WorkSummary => ({
  flow: null,
  id,
  key: id.toUpperCase(),
  project: "ENG",
  title: `Task ${id}`,
  type: "task",
  status: "in_progress",
  start: "2031-04-14T00:00:00Z",
  due: "2031-04-18T00:00:00Z",
  updated: "2031-04-16T00:00:00Z",
  version: 1,
});

const BAND: WorkGroup = { key: "in_progress", count: 3, rows: [row("eng-1"), row("eng-2")] };

function draw(onOverflow = vi.fn()) {
  render(
    <TimelineView
      rows={[]}
      groups={[BAND]}
      heading={(group) => (group.key === "in_progress" ? "Doing" : group.key)}
      overflowHref={(key) => `#/worklet/tasks?shape=list&group=${key}`}
      onOverflow={onOverflow}
      chrome={{}}
      hrefOf={(r) => `#/worklet/${r.key}`}
      onOpen={() => {}}
      selected=""
      now={NOW}
    />,
  );
  return onOverflow;
}

test("a band is headed in the company's own words, never its stored value", () => {
  draw();
  expect(screen.getByText("Doing")).toBeTruthy();
  expect(screen.queryByText("in_progress")).toBeNull();
});

test("a band its page cut short says how many more, and where they are", () => {
  const onOverflow = draw();
  const more = screen.getByRole("link", { name: "1 more →" });
  expect(more.getAttribute("href")).toBe("#/worklet/tasks?shape=list&group=in_progress");
  fireEvent.click(more);
  expect(onOverflow).toHaveBeenCalledWith("in_progress");
});
