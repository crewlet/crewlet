import { describe, expect, test } from "vitest";
import { MAX_EDITS } from "./lineDiff.ts";
import { merge3, mergedText } from "./merge.ts";

const doc = (...ls: string[]) => ls.join("\n") + "\n";

describe("merge3", () => {
  // THE INVARIANT THE EDITOR RESTS ON: moving an edit onto a newer save keeps
  // that save's change. Without it the engine's stale_version refusal is
  // bypassed — the browser re-bases and the next save deletes their line.
  test("the other writer's added line survives an edit elsewhere", () => {
    const base = doc("## Paging", "", "one", "", "## Escalation", "", "two");
    const mine = doc("## Paging", "", "one, edited", "", "## Escalation", "", "two");
    const theirs = doc("## Paging", "", "one", "", "## Escalation", "", "two", "", "Maya's line.");
    const m = merge3(base, mine, theirs);
    expect(m.conflicts).toBe(0);
    expect(mergedText(m)).toBe(
      doc("## Paging", "", "one, edited", "", "## Escalation", "", "two", "", "Maya's line."),
    );
  });

  test("an edit that changed nothing becomes their version", () => {
    const base = doc("a", "b");
    const theirs = doc("a", "b", "c");
    expect(mergedText(merge3(base, base, theirs))).toBe(theirs);
  });

  test("the same change made on both sides is applied once", () => {
    const base = doc("a", "b", "c");
    const both = doc("a", "B", "c");
    const m = merge3(base, both, both);
    expect(m.conflicts).toBe(0);
    expect(mergedText(m)).toBe(both);
  });

  test("changes to adjacent lines are independent", () => {
    const base = doc("a", "b", "c", "d");
    const m = merge3(base, doc("a", "B", "c", "d"), doc("a", "b", "C", "d"));
    expect(m.conflicts).toBe(0);
    expect(mergedText(m)).toBe(doc("a", "B", "C", "d"));
  });

  test("two different rewrites of one line are a conflict, never a pick", () => {
    const base = doc("a", "b", "c");
    const m = merge3(base, doc("a", "mine", "c"), doc("a", "theirs", "c"));
    expect(m.conflicts).toBe(1);
    expect(m.chunks).toEqual([
      { kind: "clean", lines: ["a"] },
      { kind: "conflict", base: ["b"], mine: ["mine"], theirs: ["theirs"] },
      { kind: "clean", lines: ["c"] },
    ]);
    expect(() => mergedText(m)).toThrow(/no resolution/);
    expect(mergedText(m, ["mine"])).toBe(doc("a", "mine", "c"));
    expect(mergedText(m, ["theirs"])).toBe(doc("a", "theirs", "c"));
    expect(mergedText(m, ["both"])).toBe(doc("a", "mine", "theirs", "c"));
  });

  test("two insertions at one place are a conflict — their order is nobody's decision", () => {
    const base = doc("a", "b");
    const m = merge3(base, doc("a", "x", "b"), doc("a", "y", "b"));
    expect(m.conflicts).toBe(1);
    expect(mergedText(m, ["both"])).toBe(doc("a", "x", "y", "b"));
  });

  test("an insertion at the edge of the other side's rewrite is a conflict", () => {
    const base = doc("a", "b", "c");
    const m = merge3(base, doc("a", "B", "c"), doc("a", "b", "new", "c"));
    expect(m.conflicts).toBe(1);
  });

  test("clean passages and conflicts keep document order around each other", () => {
    const base = doc("1", "2", "3", "4", "5");
    const mine = doc("1", "two", "3", "4", "FIVE");
    const theirs = doc("zero", "1", "2", "3", "4", "five");
    const m = merge3(base, mine, theirs);
    expect(m.conflicts).toBe(1);
    expect(mergedText(m, ["theirs"])).toBe(doc("zero", "1", "two", "3", "4", "five"));
  });

  test("the draft's own trailing newline is kept", () => {
    const m = merge3("a\nb\nc", "A\nb\nc", "a\nb\nc\nd\n");
    expect(mergedText(m)).toBe("A\nb\nc\nd");
  });

  // A LONG PAGE MERGES AS A SHORT ONE DOES: past 2,000 lines the design
  // system's line diff answered a whole replacement, so two edits far apart
  // conflicted as the whole document while the diff beside them showed one
  // line each.
  test("edits far apart in a page past 2,000 lines merge cleanly", () => {
    const base = Array.from({ length: 2_500 }, (_, i) => `- step ${i}`);
    const mine = base.map((line, i) => (i === 10 ? `${line}, mine` : line));
    const theirs = base.map((line, i) => (i === 2_400 ? `${line}, theirs` : line));
    const m = merge3(doc(...base), doc(...mine), doc(...theirs));
    expect(m.conflicts).toBe(0);
    expect(mergedText(m)).toBe(
      doc(...base.map((line, i) => (i === 10 ? mine[i]! : i === 2_400 ? theirs[i]! : line))),
    );
  });

  // A SIDE THE LINE DIFF DID NOT COMPARE is a change of the whole document,
  // so it meets any change of the other side as one conflict: the person
  // decides, and the browser guesses at nothing.
  test("a side the line diff did not compare conflicts as the whole document", () => {
    const base = Array.from({ length: MAX_EDITS / 2 + 1 }, (_, i) => `old ${i}`);
    const mine = Array.from({ length: MAX_EDITS / 2 + 1 }, (_, i) => `new ${i}`);
    const theirs = base.map((line, i) => (i === 7 ? `${line}, theirs` : line));
    const m = merge3(doc(...base), doc(...mine), doc(...theirs));
    expect(m.conflicts).toBe(1);
    expect(m.chunks).toEqual([{ kind: "conflict", base, mine, theirs }]);
    // An edit of nothing, though, is no change at any length.
    expect(mergedText(merge3(doc(...base), doc(...base), doc(...theirs)))).toBe(doc(...theirs));
  });
});
