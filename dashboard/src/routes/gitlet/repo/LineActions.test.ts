// @vitest-environment node
import { expect, test } from "vitest";
import { lineActionItems, runText } from "./LineActions.tsx";

const COMMIT = "0123456789abcdef0123456789abcdef01234567";
const drawn = { from: 10, texts: ["a", "b", "c", "d"] };

// ONE LINE AND A RUN NAME THEMSELVES, and a run drawn whole copies its text.
// Mutation: slice the run by index from 0, and the text fails.
test("a run's items name one line or many, and copy the lines drawn", () => {
  expect(lineActionItems({ start: 11, end: 12 }, drawn, COMMIT, false)).toEqual([
    { key: "copy", label: "Copy lines" },
    { key: "link", label: "Copy a link to these lines" },
    { key: "lens", label: "Blame these lines" },
  ]);
  expect(lineActionItems({ start: 11, end: 11 }, drawn, COMMIT, true).map((i) => i.label)).toEqual([
    "Copy line",
    "Copy a link to this line",
    "Show this line's code",
  ]);
  expect(runText({ start: 11, end: 13 }, drawn)).toEqual({ text: "b\nc\nd" });
});

// A RUN NOT WHOLLY DRAWN, A BLAME LINE WITHOUT ITS TEXT AND AN UNKNOWN COMMIT hold
// their items with the reason. Mutation: copy a partial run, and this fails.
test("Copy lines and the link are held with their reasons", () => {
  const [copy, link] = lineActionItems({ start: 12, end: 20 }, drawn, "", false);
  expect(copy?.held).toBe("Lines 12 to 20 are not all loaded: load them to copy them.");
  expect(link?.held).toBe("Waiting for the commit the file was read at.");
  const blame = { from: 1, texts: ["x", undefined, "z"] };
  expect(lineActionItems({ start: 1, end: 3 }, blame, COMMIT, true)[0]?.held).toBe(
    "Line 2 is too long for a blame to carry: copy it from Code.",
  );
});
