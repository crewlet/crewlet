import { describe, expect, test } from "vitest";
import { changedRuns, segments, tokens, type DiffSpan, type Run } from "./wordDiff.ts";

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

/** A line's segments as `text`, `[run]` or `class:text`, in order. */
function drawn(text: string, spans: readonly DiffSpan[] | null, run: Run | null) {
  return segments(text, spans, run).map((s) => {
    const piece = text.slice(s.start, s.end);
    const named = s.className ? `${s.className}:${piece}` : piece;
    return s.run ? `[${named}]` : named;
  });
}

/** A seeded xorshift, so every run cuts the same lines. */
function random(seed: number): () => number {
  let x = seed | 0 || 1;
  return () => {
    x ^= x << 13;
    x ^= x >>> 17;
    x ^= x << 5;
    return (x >>> 0) / 4294967296;
  };
}

describe("a line's segments", () => {
  const go = "func (s *Store) Get() {";
  // func, space, (s *Store), space, Get, () {
  const spans: DiffSpan[] = [
    { end: 4, className: "kw" },
    { end: 5, className: null },
    { end: 15, className: "ty" },
    { end: 16, className: null },
    { end: 19, className: "fn" },
    { end: 23, className: null },
  ];

  // Mutation: keeping a span's class inside the run fails "a run inside one span".
  test.each([
    ["no spans and no run", "a b", null, null, ["a b"]],
    ["spans and no run", go, spans, null, ["kw:func", " ", "ty:(s *Store)", " ", "fn:Get", "() {"]],
    [
      "a run inside one span, the span split around it",
      go,
      spans,
      { start: 9, end: 14 },
      ["kw:func", " ", "ty:(s *", "[Store]", "ty:)", " ", "fn:Get", "() {"],
    ],
    [
      "a run across three spans",
      go,
      spans,
      { start: 2, end: 18 },
      ["kw:fu", "[nc (s *Store) Ge]", "fn:t", "() {"],
    ],
    [
      "a run at the start",
      go,
      spans,
      { start: 0, end: 4 },
      ["[func]", " ", "ty:(s *Store)", " ", "fn:Get", "() {"],
    ],
    [
      "a run at the end",
      go,
      spans,
      { start: 19, end: 23 },
      ["kw:func", " ", "ty:(s *Store)", " ", "fn:Get", "[() {]"],
    ],
    ["an empty line", "", null, null, [""]],
    ["an empty line with spans", "", [{ end: 0, className: "kw" }], null, [""]],
    [
      "spans ending short of the text",
      go,
      [{ end: 4, className: "kw" }],
      null,
      ["kw:func", " (s *Store) Get() {"],
    ],
    [
      "spans past the text, clipped",
      "if x",
      [
        { end: 2, className: "kw" },
        { end: 9, className: "ty" },
      ],
      null,
      ["kw:if", "ty: x"],
    ],
    [
      "adjacent plain pieces merged",
      "a b c",
      [
        { end: 1, className: null },
        { end: 2, className: null },
        { end: 5, className: null },
      ],
      { start: 4, end: 5 },
      ["a b ", "[c]"],
    ],
  ] as const)("%s", (_, text, given, run, want) => {
    expect(drawn(text, given, run)).toEqual(want);
    expect(segments(text, given, run)[0]!.start).toBe(0);
  });

  test("cover the line in order, hold one run when there is one, and no class inside it", () => {
    const next = random(35);
    for (let k = 0; k < 2000; k++) {
      const length = Math.floor(next() * 40);
      const text = "x".repeat(length);
      const spans: DiffSpan[] = [];
      for (let end = 0; end < length + 3 && next() < 0.9;) {
        end += 1 + Math.floor(next() * 6);
        spans.push({ end, className: next() < 0.6 ? `k${spans.length % 3}` : null });
      }
      const a = Math.floor(next() * (length + 1));
      const b = Math.floor(next() * (length + 1));
      const run = next() < 0.5 && a !== b ? { start: Math.min(a, b), end: Math.max(a, b) } : null;
      const got = segments(text, next() < 0.1 ? null : spans, run);
      let at = 0;
      for (const s of got) {
        expect(s.start, `case ${k}`).toBe(at);
        expect(s.end).toBeGreaterThanOrEqual(s.start);
        at = s.end;
      }
      expect(at).toBe(length);
      const runs = got.filter((s) => s.run);
      expect(runs.length).toBe(run ? 1 : 0);
      if (run) {
        expect(runs[0]).toMatchObject({ start: run.start, end: run.end, className: null });
        for (const s of got) {
          if (s.run) continue;
          expect(s.end <= run.start || s.start >= run.end, `case ${k}`).toBe(true);
        }
      }
    }
  });
});
