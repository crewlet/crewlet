import { describe, expect, it } from "vitest";
import { MaxDiffLines, collapse, diffLines, diffStat, lines } from "./diff.ts";

function shape(before: string, after: string): string[] {
  return diffLines(before, after).map((l) => `${l.kind[0]} ${l.text}`);
}

describe("splitting a document into lines", () => {
  it("does not count a trailing newline as a line", () => {
    // Every file that ends properly has one, so counting it would report an
    // added last line on every document that gained one and on none that
    // gained a sentence.
    expect(lines("a\nb\n")).toEqual(["a", "b"]);
    expect(lines("a\nb")).toEqual(["a", "b"]);
    // A blank line in the MIDDLE is content — it is what separates two
    // paragraphs of markdown.
    expect(lines("a\n\nb\n")).toEqual(["a", "", "b"]);
    expect(lines("")).toEqual([]);
  });

  it("reads both line endings as one", () => {
    expect(lines("a\r\nb\r\n")).toEqual(["a", "b"]);
  });
});

describe("the line diff", () => {
  it("keeps the lines that did not move as context", () => {
    // A page diff is read as a DOCUMENT rather than as a patch: a reader
    // wants to see the paragraph a sentence was added to.
    expect(shape("one\ntwo\nthree", "one\ntwo\nthree")).toEqual(["s one", "s two", "s three"]);
  });

  it("puts a replaced line's removal above its addition", () => {
    // The other order reads as an insertion followed by an unrelated
    // deletion, which is not what a rewritten paragraph is.
    expect(shape("one\nOLD\nthree", "one\nNEW\nthree")).toEqual([
      "s one",
      "r OLD",
      "a NEW",
      "s three",
    ]);
  });

  it("reports an insertion as an addition and nothing else", () => {
    expect(shape("one\nthree", "one\ntwo\nthree")).toEqual(["s one", "a two", "s three"]);
  });

  it("reports a deletion as a removal and nothing else", () => {
    expect(shape("one\ntwo\nthree", "one\nthree")).toEqual(["s one", "r two", "s three"]);
  });

  it("finds the common subsequence rather than aligning by position", () => {
    // Position-aligned, every line after the insert reads as changed — which
    // is what makes a naive diff useless on a document somebody edited near
    // the top.
    const got = shape("a\nb\nc\nd", "a\nNEW\nb\nc\nd");
    expect(got).toEqual(["s a", "a NEW", "s b", "s c", "s d"]);
  });

  it("handles an empty side in either direction", () => {
    expect(shape("", "a\nb")).toEqual(["a a", "a b"]);
    expect(shape("a\nb", "")).toEqual(["r a", "r b"]);
    expect(shape("", "")).toEqual([]);
  });

  it("numbers each line in the document it belongs to", () => {
    const got = diffLines("one\nOLD\nthree", "one\nNEW\nthree");
    expect(got.map((l) => [l.before, l.after])).toEqual([
      [1, 1],
      [2, null],
      [null, 2],
      [3, 3],
    ]);
  });
});

describe("the summary", () => {
  it("says identical rather than zero and zero", () => {
    // A save that changed only a title leaves the body untouched, and a diff
    // pane showing the whole document with no marks reads as one that failed
    // to load.
    expect(diffStat(diffLines("same\ntext", "same\ntext"))).toEqual({
      added: 0,
      removed: 0,
      identical: true,
    });
  });

  it("counts a replacement as one of each", () => {
    expect(diffStat(diffLines("a\nOLD", "a\nNEW"))).toEqual({
      added: 1,
      removed: 1,
      identical: false,
    });
  });
});

describe("collapsing the unchanged runs", () => {
  const before = Array.from({ length: 30 }, (_, i) => `line ${i}`).join("\n");
  const after = before.replace("line 15", "CHANGED");

  it("keeps context either side of a change", () => {
    const sections = collapse(diffLines(before, after), 2);
    expect(sections).toHaveLength(1);
    const texts = sections[0]?.lines.map((l) => l.text);
    expect(texts).toEqual(["line 13", "line 14", "line 15", "CHANGED", "line 16", "line 17"]);
  });

  it("reports how many lines a gap stands for rather than closing it", () => {
    // A reader has to be able to tell a document with two edits at its ends
    // from one rewritten in the middle.
    const sections = collapse(diffLines(before, after), 2);
    // Thirteen: `line 0` through `line 12`, with `line 13` and `line 14`
    // kept as the context above the change.
    expect(sections[0]?.skipped).toBe(13);
  });

  it("returns two sections for two distant changes", () => {
    const edited = before.replace("line 2", "TOP").replace("line 27", "BOTTOM");
    const sections = collapse(diffLines(before, edited), 1);
    expect(sections).toHaveLength(2);
    expect(sections[0]?.skipped).toBe(1);
    expect(sections[1]?.skipped).toBeGreaterThan(0);
  });

  it("returns nothing at all for two identical documents", () => {
    expect(collapse(diffLines(before, before))).toEqual([]);
  });
});

describe("a document too large to compare", () => {
  it("is reported as a whole-document replacement rather than freezing", () => {
    // The table is quadratic on EVERY pair, however alike, and the honest
    // failure past the cap is to say everything changed — not to stop
    // responding.
    const huge = Array.from({ length: MaxDiffLines + 1 }, (_, i) => `l${i}`).join("\n");
    const stat = diffStat(diffLines(huge, "one line"));
    expect(stat.removed).toBe(MaxDiffLines + 1);
    expect(stat.added).toBe(1);
  });

  // THE CAP IS A MEMORY BUDGET, and this is the arithmetic it was set from —
  // pinned so the constant cannot drift back up without somebody restating the
  // reason. `common` allocates (N+1) rows of (M+1) Uint32 entries and fills
  // every cell on every input, so the worst case is paid by two revisions one
  // line apart. At 10,000 the table is 382 MB and 1.7 s on the main thread;
  // this budget is what keeps it a tab-sized 16 MB.
  it("keeps the table inside the budget the cap was chosen for", () => {
    const bytes = 4 * (MaxDiffLines + 1) ** 2;
    expect(bytes).toBeLessThanOrEqual(24 * 1024 * 1024);
  });
});
