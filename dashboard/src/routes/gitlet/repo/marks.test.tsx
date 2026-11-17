/**
 * A list of commits' marks: asked once per page by its whole ids, a refusal
 * said once, and asked again each minute only while a check on screen runs.
 */

import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { CODE_POSITION, jsonAnswer, stubCodeEngine } from "~/test/codeScreens.tsx";
import { engineJson } from "~/test/engineAnswer.ts";
import { afterMicrotasks } from "~/test/settle.ts";
import { MARKS_POLL_MS, useCommitMarks } from "./marks.ts";

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

const A = "a".repeat(40);
const B = "b".repeat(40);
const C = "c".repeat(40);

function Marks({ pages }: { pages: string[][] }) {
  const marks = useCommitMarks("web", pages);
  return (
    <ul>
      {pages.flat().map((oid) => {
        const m = marks.of(oid);
        const word = m === undefined ? "waiting" : (m?.checks?.[0]?.state ?? "none");
        return <li key={oid}>{`${oid.slice(0, 1)} ${word}`}</li>;
      })}
      {marks.failed && <li>{`failed ${marks.failed.code}: ${marks.failed.sentence ?? ""}`}</li>}
    </ul>
  );
}

const rows = () => screen.getAllByRole("listitem").map((li) => li.textContent);

/** The engine answering marks: `state` for A's rollup, none for the rest. */
function stubMarks(state: () => string) {
  return stubCodeEngine({
    "/query/code_commit_marks": () =>
      jsonAnswer({
        commit_marks: [
          { commit: A, checks: [{ target: "refs/heads/main", state: state(), counts: {} }] },
        ],
        position: CODE_POSITION,
        level: "session",
      }),
  });
}

// ONE ASK PER PAGE, by its whole ids: a page added asks for itself alone, and a commit with
// no marks reads none, never waiting. Mutation: ask every page again on Load more.
test("each page's marks are asked once, by its whole ids", async () => {
  const asked = stubMarks(() => "passed");
  const view = render(<Marks pages={[[A, B]]} />);
  expect(rows()).toEqual(["a waiting", "b waiting"]);
  await afterMicrotasks();
  expect(rows()).toEqual(["a passed", "b none"]);
  view.rerender(<Marks pages={[[A, B], [C]]} />);
  await afterMicrotasks();
  expect(rows()).toEqual(["a passed", "b none", "c none"]);
  expect(asked.map((u) => u.searchParams.get("commits"))).toEqual([`${A},${B}`, C]);
  expect(asked.every((u) => u.searchParams.get("name") === "web")).toBe(true);
});

// A RUNNING CHECK IS ASKED AGAIN each minute, a settled one never. Mutation: poll always.
test("a running check is asked again and a settled one is not", async () => {
  vi.useFakeTimers();
  let state = "running";
  const asked = stubMarks(() => state);
  render(<Marks pages={[[A]]} />);
  await afterMicrotasks();
  expect(rows()).toEqual(["a running"]);
  state = "passed";
  act(() => {
    vi.advanceTimersByTime(MARKS_POLL_MS);
  });
  await afterMicrotasks();
  expect(asked).toHaveLength(2);
  expect(rows()).toEqual(["a passed"]);
  act(() => {
    vi.advanceTimersByTime(5 * MARKS_POLL_MS);
  });
  await afterMicrotasks();
  expect(asked).toHaveLength(2);
});

// A FAILED RE-ASK KEEPS THE MARKS DRAWN, and the next minute asks again. Mutation: store the
// failure over the page's marks.
test("a failed re-ask keeps the marks already drawn", async () => {
  vi.useFakeTimers();
  let fail = false;
  const asked = stubCodeEngine({
    "/query/code_commit_marks": () =>
      fail
        ? engineJson({ error: "query_failed" }, 500)
        : jsonAnswer({
            commit_marks: [
              { commit: A, checks: [{ target: "refs/heads/main", state: "running", counts: {} }] },
            ],
            position: CODE_POSITION,
            level: "session",
          }),
  });
  render(<Marks pages={[[A]]} />);
  await afterMicrotasks();
  expect(rows()).toEqual(["a running"]);
  fail = true;
  act(() => {
    vi.advanceTimersByTime(MARKS_POLL_MS);
  });
  await afterMicrotasks();
  expect(asked).toHaveLength(2);
  expect(rows()).toEqual(["a running"]);
  act(() => {
    vi.advanceTimersByTime(MARKS_POLL_MS);
  });
  await afterMicrotasks();
  expect(asked).toHaveLength(3);
});

// A REFUSAL IS ITS CODE AND THE ENGINE'S SENTENCE, once. Mutation: drop the sentence.
test("a refused read is its code and the engine's sentence", async () => {
  stubCodeEngine({
    "/query/code_commit_marks": () =>
      engineJson({ error: "unavailable", detail: "this node answers no marks yet" }, 503),
  });
  render(<Marks pages={[[A], [B]]} />);
  await afterMicrotasks();
  expect(rows()).toEqual([
    "a waiting",
    "b waiting",
    "failed unavailable: this node answers no marks yet",
  ]);
});
