/**
 * A run of prose is read once, not once per render: the recogniser is the
 * costly part of drawing a run, and a surface re-renders for reasons that
 * change none of its words (an Inbox list ticking its times, a page of
 * comments taking one more).
 */

import { act, cleanup, render } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { useState } from "react";
import { inlineTokens } from "~/lib/inlineTokens.ts";
import { renderMarkdown } from "~/lib/markdown.ts";

vi.mock("~/lib/inlineTokens.ts", async (importOriginal) => {
  const actual = await importOriginal<typeof import("~/lib/inlineTokens.ts")>();
  return { ...actual, inlineTokens: vi.fn(actual.inlineTokens) };
});

afterEach(cleanup);

// Mutation: read the run in `tokenise` again rather than in `TokenRun`'s
// memo, and the second render reads it a second time.
test("a re-render that changes no words reads no run again", () => {
  let tick: () => void = () => {};
  function Surface() {
    const [n, setN] = useState(0);
    tick = () => setN((v) => v + 1);
    return <div data-n={n}>{renderMarkdown("Ship gophrase/backend#14 before #ffe100 lands.")}</div>;
  }
  render(<Surface />);
  const read = vi.mocked(inlineTokens).mock.calls.length;
  expect(read).toBe(1);
  act(() => tick());
  act(() => tick());
  expect(vi.mocked(inlineTokens).mock.calls.length).toBe(read);
});
