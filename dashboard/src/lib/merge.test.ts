import { describe, expect, test } from "vitest";
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
});
