/**
 * What a fleet answer is missing, said by name — and nothing at all when it
 * is missing nothing, because a warning on every complete answer is one a
 * reader learns to skip.
 */

import { cleanup, render } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";

import { CoverageNote, missingFrom } from "./CoverageNote.tsx";

afterEach(cleanup);

const node = (id: string, answered: boolean, error = "") => ({ id, answered, error });

test("a node silent on two answers is named once, with the first reason", () => {
  const got = missingFrom([
    { complete: false, nodes: [node("b", false, "no answer inside the fleet read budget")] },
    undefined,
    { complete: false, nodes: [node("a", true), node("b", false, "a newer protocol")] },
  ]);
  expect(got).toEqual({
    nodes: [{ id: "b", error: "no answer inside the fleet read budget" }],
    incomplete: true,
  });
});

test("a complete answer draws nothing", () => {
  const { container } = render(
    <CoverageNote coverage={[{ complete: true, nodes: [node("a", true)] }]} what="this list" />,
  );
  expect(container.textContent).toBe("");
});

// INCOMPLETE WITH NOBODY TO NAME is the roster itself unread — a different
// sentence, since there is no node to point at.
test("an unread roster says so rather than naming nobody", () => {
  const { container } = render(
    <CoverageNote coverage={[{ complete: false, nodes: [node("a", true)] }]} what="this list" />,
  );
  expect(container.textContent).toMatch(/roster could not be read, so this list may not include/);
});
