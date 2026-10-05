// @vitest-environment node
import { describe, expect, test } from "vitest";
import {
  decodeSections,
  findCounts,
  findPattern,
  neighbours,
  outlineHalf,
  sectionsKey,
  sectionsOf,
  share,
  stepToMatch,
  tileByHeadings,
  tileByMap,
} from "./promptmap.ts";

/**
 * A PROMPT'S OUTLINE TILES THE PROMPT, and comes from its builder when the
 * builder said where its parts are.
 *
 * No case here names a section a real builder writes: what is asserted is the
 * SHAPE — spans that add up, a map that is trusted only when it tiles, a quoted
 * heading that stays inside its span — so a prompt that grows or renames a
 * section changes nothing here.
 */

const bytes = (s: string) => new TextEncoder().encode(s).length;

/** A map for `parts`, cut exactly where they meet. */
function mapOf(parts: [key: string, title: string, text: string][]) {
  return parts.map(([key, title, text]) => ({ key, title, bytes: bytes(text) }));
}

describe("a section map off the wire", () => {
  test("is read whole", () => {
    expect(decodeSections([{ key: "a", title: "A", bytes: 3 }])).toEqual([
      { key: "a", title: "A", bytes: 3 },
    ]);
  });

  test("is refused whole when one entry is malformed, since every later boundary would shift", () => {
    expect(
      decodeSections([
        { key: "a", title: "A", bytes: 3 },
        { key: "b", bytes: 1 },
      ]),
    ).toBeNull();
    expect(decodeSections([{ key: "a", title: "A", bytes: 1.5 }])).toBeNull();
    expect(decodeSections("nope")).toBeNull();
    expect(decodeSections([])).toBeNull();
    expect(decodeSections(undefined)).toBeNull();
  });

  test("carries `headed` when it is a boolean, and leaves it absent when it is", () => {
    expect(
      decodeSections([
        { key: "a", title: "A", bytes: 3, headed: true },
        { key: "b", title: "B", bytes: 1, headed: false },
        { key: "c", title: "C", bytes: 1 },
      ]),
    ).toEqual([
      { key: "a", title: "A", bytes: 3, headed: true },
      { key: "b", title: "B", bytes: 1, headed: false },
      { key: "c", title: "C", bytes: 1 },
    ]);
  });

  test("is refused whole when `headed` is anything but a boolean", () => {
    for (const headed of ["true", 1, null, {}]) {
      expect(
        decodeSections([{ key: "a", title: "A", bytes: 3, headed }]),
        String(headed),
      ).toBeNull();
    }
  });
});

describe("a map's value", () => {
  test("is equal for equal maps however often they are decoded", () => {
    const wire = [{ key: "a", title: "A", bytes: 3, headed: true }];
    expect(sectionsKey(decodeSections(wire))).toBe(sectionsKey(decodeSections(wire)));
    expect(sectionsKey(null)).toBe("");
  });

  test("moves with every field a reader draws", () => {
    const base = { key: "a", title: "A", bytes: 3 };
    const key = sectionsKey([base]);
    for (const changed of [
      { ...base, key: "b" },
      { ...base, title: "B" },
      { ...base, bytes: 4 },
      { ...base, headed: false },
    ]) {
      expect(sectionsKey([changed]), JSON.stringify(changed)).not.toBe(key);
    }
  });
});

describe("a map that tiles its prompt", () => {
  const LEAD = "You are **Engineer**.\n\n";
  // A QUOTED HEADING inside the builder's span: the shape a chat trigger's
  // "## Triage" takes, and the one the heading split let escape.
  const TASK = "## Your turn\nThe ask:\n\n## Triage\nquoted from the thread\n\n";
  const TAIL = "## Contract\n- **done** — it meets the ask.";
  const TEXT = LEAD + TASK + TAIL;
  const MAP = mapOf([
    ["identity", "Identity", LEAD],
    ["turn", "Your turn", TASK],
    ["contract", "Contract", TAIL],
  ]);

  test("is the top level, sliced by bytes, and every span is its source", () => {
    const half = outlineHalf("system", TEXT, MAP);
    expect(half.from).toBe("map");
    expect(half.sections.map((s) => s.key)).toEqual(["identity", "turn", "contract"]);
    expect(half.sections.map((s) => s.source).join("")).toBe(TEXT);
    expect(half.sections.reduce((n, s) => n + s.bytes, 0)).toBe(half.bytes);
  });

  test("keeps a heading quoted inside a span INSIDE it, nested under the span", () => {
    const turn = outlineHalf("system", TEXT, MAP).sections[1]!;
    expect(turn.title).toBe("Your turn");
    expect(turn.body).toBe("The ask:");
    expect(turn.children.map((c) => c.title)).toEqual(["Triage"]);
    expect(turn.children[0]!.body).toBe("quoted from the thread");
    // THE CONTROL: the headings alone put the quoted heading at the top level,
    // as a section of the prompt in its own right.
    const derived = outlineHalf("system", TEXT, null);
    expect(derived.from).toBe("headings");
    expect(derived.sections.map((s) => s.title)).toContain("Triage");
  });

  test("names a headless span by the builder's title and shows its text", () => {
    const identity = outlineHalf("system", TEXT, MAP).sections[0]!;
    expect(identity.title).toBe("Identity");
    expect(identity.body).toBe("You are **Engineer**.");
    expect(identity.children).toEqual([]);
  });

  test("marks which sections open with their own heading line", () => {
    const half = outlineHalf("system", TEXT, MAP);
    expect(half.sections.map((s) => s.headed)).toEqual([false, true, true]);
  });

  test("keeps a span's slice whole when it opens with a byte order mark", () => {
    // A pasted document can carry U+FEFF; the decoder's default strips it
    // from every slice that opens with one, while the byte count keeps it.
    const a = "## A\nalpha\n\n";
    const b = "﻿bom section";
    const half = outlineHalf(
      "user",
      a + b,
      mapOf([
        ["a", "A", a],
        ["b", "Pasted", b],
      ]),
    );
    expect(half.from).toBe("map");
    expect(half.sections.map((s) => s.source).join("")).toBe(a + b);
    expect(half.sections[1]!.source).toBe(b);
  });

  test("cuts multi-byte text on character boundaries", () => {
    const a = "## Café ☕\nrésumé — 日本語\n\n";
    const b = "## Next\n😀 done";
    const half = outlineHalf(
      "user",
      a + b,
      mapOf([
        ["a", "Café ☕", a],
        ["b", "Next", b],
      ]),
    );
    expect(half.from).toBe("map");
    expect(half.sections.map((s) => s.source)).toEqual([a, b]);
    expect(half.sections[0]!.bytes).toBe(bytes(a));
  });

  test("keeps CRLF line endings in the span they belong to", () => {
    const a = "## One\r\nfirst\r\n\r\n";
    const b = "## Two\r\nsecond";
    const half = outlineHalf(
      "user",
      a + b,
      mapOf([
        ["a", "One", a],
        ["b", "Two", b],
      ]),
    );
    expect(half.sections.map((s) => s.source)).toEqual([a, b]);
    expect(half.sections[0]!.body).toBe("first");
  });
});

/**
 * A builder's HEADLESS part quoting somebody else's markdown — a worker's
 * persona, a task the executor wrote, a trigger's text — which so often opens
 * with "## Goal" or "# Task". Taken as the span's own heading, those words
 * appeared nowhere in the reading view.
 */
describe("a headless span that opens with a quoted heading", () => {
  const TASK = "## Goal\nDo X\n\n## Steps\n1. a\n";
  const RULES = "## Worker rules\nstay a leaf";
  const TEXT = TASK + "\n" + RULES;
  const map = (headed: [boolean | undefined, boolean | undefined]) =>
    mapOf([
      ["task", "Task", TASK + "\n"],
      ["worker_rules", "Worker rules", RULES],
    ]).map((s, i) => (headed[i] === undefined ? s : { ...s, headed: headed[i] }));

  test.each([
    ["the map says it is headless", [false, true] as [boolean, boolean]],
    // An engine that predates `headed`: the first heading's title is not the
    // span's, so it is quoted rather than the span's own.
    ["the map does not say", [undefined, undefined] as [undefined, undefined]],
  ])("nests the quoted heading under the builder's title when %s", (_why, headed) => {
    const [task, rules] = outlineHalf("system", TEXT, map(headed)).sections;
    expect(task).toMatchObject({ title: "Task", body: "", headed: false });
    expect(task!.children.map((c) => c.title)).toEqual(["Goal", "Steps"]);
    expect(task!.children[0]!.body).toBe("Do X");
    // THE CONTROL: a headed span still takes its own heading as its body's.
    expect(rules).toMatchObject({ title: "Worker rules", body: "stay a leaf", headed: true });
    expect(rules!.children).toEqual([]);
  });

  test("keeps the lead a headless span opens with as its body", () => {
    const text = "Context first.\n\n## Goal\nDo X";
    const [task] = outlineHalf("user", text, mapOf([["task", "Task", text]])).sections;
    expect(task).toMatchObject({ body: "Context first.", headed: false });
    expect(task!.children.map((c) => c.title)).toEqual(["Goal"]);
  });

  test("trusts a map that says a span is headed", () => {
    // The builder's own heading, whatever its title: what `headed` is for.
    const text = "## Your turn\nask";
    const [turn] = outlineHalf("user", text, [
      { key: "turn", title: "Turn", bytes: bytes(text), headed: true },
    ]).sections;
    expect(turn).toMatchObject({ title: "Turn", body: "ask", headed: true, children: [] });
  });
});

describe("a map that does not tile its prompt is not trusted", () => {
  const TEXT = "## A\nalpha\n\n## B\nbeta";
  const [A, B] = ["## A\nalpha\n\n", "## B\nbeta"];

  test.each([
    ["the bytes do not add up", [{ key: "a", title: "A", bytes: bytes(A) }]],
    [
      "a span is empty",
      [
        { key: "a", title: "A", bytes: bytes(A) },
        { key: "z", title: "Z", bytes: 0 },
        { key: "b", title: "B", bytes: bytes(B) },
      ],
    ],
    [
      "a key repeats",
      [
        { key: "a", title: "A", bytes: bytes(A) },
        { key: "a", title: "B", bytes: bytes(B) },
      ],
    ],
    [
      "a key is empty",
      [
        { key: "", title: "A", bytes: bytes(A) },
        { key: "b", title: "B", bytes: bytes(B) },
      ],
    ],
  ])("— %s — and falls back to the headings", (_why, map) => {
    expect(tileByMap(TEXT, map)).toBeNull();
    const half = outlineHalf("system", TEXT, map);
    expect(half.from).toBe("headings");
    expect(half.sections.map((s) => s.title)).toEqual(["A", "B"]);
  });

  test("— a boundary falls inside a character — and falls back too", () => {
    // "é" is two bytes; a cut after its first byte is inside it.
    const text = "## é\nx";
    const map = [
      { key: "a", title: "A", bytes: 4 },
      { key: "b", title: "B", bytes: bytes(text) - 4 },
    ];
    expect(tileByMap(text, map)).toBeNull();
    expect(outlineHalf("system", text, map).from).toBe("headings");
  });
});

describe("the outline the headings give", () => {
  test("tiles the prompt exactly, heading lines and separators included", () => {
    const text = "Lead line.\n\n## One\nfirst\n\n### Inner\ndeep\n\n## Two\nsecond\n";
    const spans = tileByHeadings(text);
    expect(spans.map((s) => s.source).join("")).toBe(text);
    expect(spans.map((s) => s.title)).toEqual(["", "One", "Two"]);
    expect(spans.reduce((n, s) => n + s.bytes, 0)).toBe(bytes(text));
  });

  test("nests a deeper heading under its section rather than on the top level", () => {
    const half = outlineHalf(
      "user",
      "## Earlier\nrules\n\n### 2026-08-20\nshipped it\n\n## Task\nask",
      null,
    );
    expect(half.sections.map((s) => s.title)).toEqual(["Earlier", "Task"]);
    expect(half.sections[0]!.children.map((c) => c.title)).toEqual(["2026-08-20"]);
  });

  test("keeps a lead run as a section of its own, before the first heading", () => {
    const half = outlineHalf("system", "You are X.\n\n## A\nalpha", null);
    expect(half.sections[0]).toMatchObject({ title: "", body: "You are X." });
  });

  test("folds a blank lead into the first section rather than drawing an empty row", () => {
    const text = "\n\n## A\nalpha";
    const spans = tileByHeadings(text);
    expect(spans).toHaveLength(1);
    expect(spans[0]!.source).toBe(text);
  });

  test("ignores a heading inside a fence", () => {
    const half = outlineHalf("system", "## Tools\n```sh\n# install deps\n```\n\n## B\nb", null);
    expect(half.sections.map((s) => s.title)).toEqual(["Tools", "B"]);
  });

  test("is one section for a prompt with no headings", () => {
    const half = outlineHalf("user", "just a task", null);
    expect(half.sections).toHaveLength(1);
    expect(half.sections[0]).toMatchObject({ title: "", body: "just a task", bytes: 11 });
  });
});

describe("walking and finding", () => {
  const halves = [
    outlineHalf("system", "## A\nalpha beta\n\n## B\nnothing", null),
    outlineHalf("user", "## C\nBETA beta", null),
  ];
  const all = sectionsOf(halves);

  test("walks across the two halves in reading order", () => {
    expect(neighbours(all, "system:h2")).toMatchObject({
      prev: { id: "system:h1" },
      next: { id: "user:h1" },
    });
    expect(neighbours(all, "system:h1").prev).toBeNull();
    expect(neighbours(all, "user:h1").next).toBeNull();
  });

  test("counts matches on the source, ignoring case", () => {
    const counts = findCounts(all, "Beta");
    expect(Object.fromEntries(counts)).toEqual({ "system:h1": 1, "user:h1": 2 });
    // A query of whitespace is no query.
    expect(findCounts(all, "   ").size).toBe(0);
  });

  test("treats every character of a query literally", () => {
    const one = outlineHalf("system", "## A\ncost $1.50 (approx)", null).sections;
    expect(findCounts(one, "$1.50 (").get("system:h1")).toBe(1);
    expect(findCounts(one, ".*").size).toBe(0);
    expect(findPattern("a.b")!.test("axb")).toBe(false);
  });

  test("steps to the next and previous section with a match, wrapping", () => {
    const counts = findCounts(all, "beta");
    expect(stepToMatch(all, counts, "system:h1", 1)?.id).toBe("user:h1");
    expect(stepToMatch(all, counts, "user:h1", 1)?.id).toBe("system:h1");
    expect(stepToMatch(all, counts, "system:h1", -1)?.id).toBe("user:h1");
    expect(stepToMatch(all, counts, "system:h2", -1)?.id).toBe("system:h1");
    // The only match is the current section: Enter stays rather than failing.
    const only = findCounts(all, "alpha");
    expect(stepToMatch(all, only, "system:h1", 1)?.id).toBe("system:h1");
    expect(stepToMatch(all, new Map(), "system:h1", 1)).toBeNull();
  });

  test("says a share, and says a tiny one rather than rounding it to nothing", () => {
    expect(share(23, 100)).toBe("23%");
    expect(share(1, 1000)).toBe("under 1%");
    expect(share(0, 10)).toBe("0%");
    expect(share(1, 0)).toBe("0%");
  });
});
