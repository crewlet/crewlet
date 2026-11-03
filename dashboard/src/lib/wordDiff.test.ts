import { describe, expect, test } from "vitest";
import { changedRuns, tokens, type Run } from "./wordDiff.ts";

/** The text a run covers, or null where nothing is underlined. */
const cut = (text: string, run: Run | null) => (run ? text.slice(run.start, run.end) : null);

/** What a pair underlines, as the two runs' texts. */
function underlined(before: string, after: string) {
  const runs = changedRuns(before, after);
  return { before: cut(before, runs.before), after: cut(after, runs.after) };
}

describe("the run an edited line underlines", () => {
  // THE INVARIANT THE UNDERLINE IS FOR: a small edit in a long line is found
  // without reading the line twice, and it is the ONLY thing underlined.
  test("is what lies between the start and the end the two lines share", () => {
    expect(
      underlined(
        "Equal balances are paid in any order.",
        "Equal balances are ordered by join order.",
      ),
    ).toEqual({ before: "paid in any", after: "ordered by join" });
  });

  test("is one run however many words the edit moved past", () => {
    // A word-by-word diff would mark "quick", "fox" and "dog" apart; one run
    // spans from the first difference to the last.
    expect(
      underlined("the quick brown fox jumps over the dog", "the slow brown cat jumps over the cow"),
    ).toEqual({
      before: "quick brown fox jumps over the dog",
      after: "slow brown cat jumps over the cow",
    });
  });

  test("starts and ends on whole words", () => {
    // "colour" and "color" share "colo" letter by letter; the underline is the
    // word, never "ur" against "r".
    expect(underlined("the colour of it", "the color of it")).toEqual({
      before: "colour",
      after: "color",
    });
    expect(underlined("count: 10", "count: 12")).toEqual({ before: "10", after: "12" });
  });

  test("is nothing on the side that only lost or only gained", () => {
    expect(underlined("call(a)", "call(a, b)")).toEqual({ before: null, after: ", b" });
    expect(underlined("call(a, b)", "call(a)")).toEqual({ before: ", b", after: null });
  });

  test("counts a token once when the two lines repeat it", () => {
    // The shared end stops where the shared start began, or "aa" against
    // "aa aa" would share more than either line holds.
    expect(underlined("go go", "go go go")).toEqual({ before: null, after: " go" });
  });

  test("underlines a change of spacing, which is the whole of that edit", () => {
    expect(underlined("  return x", "    return x")).toEqual({ before: "  ", after: "    " });
  });

  // A PAIR THAT SHARES NO WORD IS TWO LINES: an indentation and a semicolon in
  // common make the "changed run" the whole of both, which the line's mark
  // already says.
  test("is nothing at all when the two lines share no word at their ends", () => {
    expect(changedRuns("  a = 1;", "  b = 2;")).toEqual({ before: null, after: null });
    expect(changedRuns("Rewritten from scratch.", "Something else entirely.")).toEqual({
      before: null,
      after: null,
    });
  });

  test("is nothing for two identical lines", () => {
    expect(changedRuns("same line", "same line")).toEqual({ before: null, after: null });
  });

  test("finds an edit in a sentence written with no spaces", () => {
    // Each ideograph is a word: as one token the sentence shares nothing.
    expect(underlined("今日は晴れです", "今日は雨です")).toEqual({ before: "晴れ", after: "雨" });
  });
});

describe("a line's tokens", () => {
  test("cover every character, in order", () => {
    const text = "  foo_bar(1, 'x') → ok\t";
    expect(
      tokens(text)
        .map((t) => t.text)
        .join(""),
    ).toBe(text);
    expect(tokens(text).map((t) => t.text)).toEqual([
      "  ",
      "foo_bar",
      "(",
      "1",
      ",",
      " ",
      "'",
      "x",
      "'",
      ")",
      " ",
      "→",
      " ",
      "ok",
      "\t",
    ]);
  });

  test("keep a word whole across combining marks", () => {
    expect(tokens("café naïve").map((t) => t.text)).toEqual(["café", " ", "naïve"]);
  });
});
