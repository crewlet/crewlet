// @vitest-environment node
import { describe, expect, test } from "vitest";
import { inlineText } from "./markdownText.ts";
import {
  atomsOf,
  blocksOf,
  compareProse,
  compareWords,
  overlap,
  previewOf,
  sameBlock,
  type ProseBlock,
  type WordPart,
} from "./proseDiff.ts";

const PAGE = "#/doclet/pages/3a0fa3c2-887f-52a7-bd64-bdedee31019b";
const opening = (word: string, link: string) =>
  `The flow ships to every workspace on ${word}, and hands off to the [${link}](${PAGE}) only when a second person joins.`;

function compared(before: string, after: string): ProseBlock[] {
  const answer = compareProse(before, after);
  if (answer.kind !== "compared") throw new Error(answer.kind);
  return answer.blocks;
}

/** What a reader reads of the parts: each run's rendered text, marked `[-…]` and `[+…]`. */
function read(parts: readonly WordPart[]): string {
  return parts
    .map((p) => (p.op === "=" ? inlineText(p.text) : `[${p.op}${inlineText(p.text)}]`))
    .join("");
}

describe("a block edited in a few words", () => {
  test("is one block, with only the changed words marked, never the paragraph twice", () => {
    const blocks = compared(
      `${opening("Monday", "pricing page")}\n\nQuestions go to the team room.\n`,
      `${opening("Tuesday", "plans page")}\n\nQuestions go to the team room.\n`,
    );
    expect(blocks.map((b) => b.change)).toEqual(["changed", "same"]);
    const edited = blocks[0]!;
    if (edited.change !== "changed") throw new Error("not changed");
    expect(read(edited.runs[0]!)).toBe(
      "The flow ships to every workspace on [-Monday][+Tuesday], and hands off to the [-pricing page][+plans page] only when a second person joins.",
    );
    expect(edited.added).toBe(3);
    expect(edited.removed).toBe(3);
  });

  test("a link is compared whole and read as its label: no target reaches the words", () => {
    const parts = compareWords(opening("Monday", "pricing page"), opening("Monday", "plans page"));
    expect(parts.filter((p) => p.op !== "=").map((p) => p.text)).toEqual([
      `[pricing page](${PAGE})`,
      `[plans page](${PAGE})`,
    ]);
    expect(read(parts)).not.toContain("#/doclet");
  });

  test("an address, a code span and an emphasis are one piece each", () => {
    expect(atomsOf("see https://example.com/a/b-c and `make dashboard` **now**")).toEqual([
      "see",
      " ",
      "https://example.com/a/b-c",
      " ",
      "and",
      " ",
      "`make dashboard`",
      " ",
      "**now**",
    ]);
  });

  test("a lone kept word between two changes is folded into them", () => {
    expect(read(compareWords("on Monday after the review", "on Tuesday before the review"))).toBe(
      "on [-Monday after][+Tuesday before] the review",
    );
  });

  test("a sentence only extended is marked in place, however much it gained", () => {
    expect(
      read(
        compareWords(
          "Who answers support each week.",
          "Who answers support each week, and who covers when they are away.",
        ),
      ),
    ).toBe("Who answers support each week[+, and who covers when they are away].");
  });

  test("a run mostly rewritten is the old words, then the new", () => {
    expect(
      read(compareWords("Ship the first slice in October.", "Platform work comes first now.")),
    ).toBe("[-Ship the first slice in October.][+Platform work comes first now.]");
  });
});

describe("blocks added, removed and rewritten", () => {
  const base =
    "# Plan\n\nWe ship in October.\n\n## Legacy importer\n\nIt reads the old format.\n\nIt stays behind a flag.\n";

  test("a deleted section is its heading and paragraphs removed, words counted", () => {
    const answer = compareProse(base, "# Plan\n\nWe ship in October.\n");
    if (answer.kind !== "compared") throw new Error(answer.kind);
    expect(answer.blocks.map((b) => b.change)).toEqual([
      "same",
      "same",
      "removed",
      "removed",
      "removed",
    ]);
    expect(answer.removed).toBe(12);
    expect(answer.added).toBe(0);
  });

  test("a new section is its blocks added", () => {
    const blocks = compared(base, `${base}\n## Pricing\n\nSeats are billed monthly.\n`);
    expect(blocks.slice(-2).map((b) => b.change)).toEqual(["added", "added"]);
  });

  test("a paragraph rewritten is the old one removed and the new one added", () => {
    const blocks = compared(
      "The team keeps the current plan for this theme.\n",
      "Platform work goes first, and this moves to December.\n",
    );
    expect(blocks.map((b) => b.change)).toEqual(["removed", "added"]);
  });

  test("a heading never pairs with a paragraph", () => {
    const blocks = compared("## Launch day\n", "Launch day\n");
    expect(blocks.map((b) => b.change)).toEqual(["removed", "added"]);
  });
});

describe("lists, tables and code", () => {
  test("each list item is a block, and an ordered item's number is not its identity", () => {
    expect(blocksOf("1. one\n2. two\n").map((b) => b.kind)).toEqual(["list", "list"]);
    const blocks = compared(
      "1. Name it\n2. Invite someone\n",
      "1. Sign up\n2. Name it\n3. Invite someone\n",
    );
    expect(blocks.map((b) => b.change)).toEqual(["added", "same", "same"]);
  });

  test("a list item edited marks its words", () => {
    const blocks = compared("- Owner: Design team\n", "- Owner: Product design team\n");
    const item = blocks[0]!;
    if (item.change !== "changed") throw new Error(item.change);
    expect(read(item.runs[0]!)).toBe("Owner: [-Design][+Product design] team");
  });

  test("a table keeps its rows: an edited row marks its cells, a new row is added", () => {
    const head = "| Step | Owner | Status |\n| --- | --- | --- |\n";
    const blocks = compared(
      `${head}| Name it | Design | Done |\n| Invite | Design | In review |\n`,
      `${head}| Name it | Design | Done |\n| Invite | Design | Done |\n| Project | Engineering | Not started |\n`,
    );
    expect(blocks).toHaveLength(1);
    const table = blocks[0]!;
    if (table.change !== "changed" || table.block.kind !== "table") throw new Error("not a table");
    expect(table.block.rows).toHaveLength(3);
    expect(table.runs.map(read)).toEqual([
      "Step",
      "Owner",
      "Status",
      "Name it",
      "Design",
      "Done",
      "Invite",
      "Design",
      "[-In review][+Done]",
      "[+Project]",
      "[+Engineering]",
      "[+Not started]",
    ]);
  });

  test("a code block edited is its lines marked", () => {
    const blocks = compared(
      "```sh\nmake dashboard\ngo build ./...\n```\n",
      "```sh\nmake dashboard\ngo vet ./...\n```\n",
    );
    const code = blocks[0]!;
    if (code.change !== "changed") throw new Error(code.change);
    expect(code.code).toEqual([
      { op: "=", text: "make dashboard" },
      { op: "-", text: "go build ./..." },
      { op: "+", text: "go vet ./..." },
    ]);
  });
});

describe("the preview", () => {
  const same = (text: string): ProseBlock => ({
    change: "same",
    block: { kind: "paragraph", text },
    words: 1,
  });
  const heading = (text: string): ProseBlock => ({
    change: "same",
    block: { kind: "heading", level: 2, text },
    words: 1,
  });
  const gone = (text: string): ProseBlock => ({
    change: "removed",
    block: { kind: "paragraph", text },
    words: 1,
  });
  const edited: ProseBlock = {
    change: "changed",
    block: { kind: "paragraph", text: "b" },
    runs: [[{ op: "+", text: "b" }]],
    added: 1,
    removed: 0,
  };

  test("counts what it leaves out, keeps the heading a change sits under, and stops after the last change", () => {
    const { entries, more } = previewOf(
      [same("a"), same("b"), heading("Launch"), same("c"), edited, same("d")],
      6,
    );
    expect(entries).toEqual([
      { kind: "skipped", count: 2, of: "same" },
      { kind: "block", block: heading("Launch") },
      { kind: "skipped", count: 1, of: "same" },
      { kind: "block", block: edited },
    ]);
    expect(more).toBe(0);
  });

  test("a long removed run with sections keeps each section's heading and counts what was under it", () => {
    const goneHeading = (text: string): ProseBlock => ({
      change: "removed",
      block: { kind: "heading", level: 2, text },
      words: 1,
    });
    const run = [
      goneHeading("Open questions"),
      gone("1"),
      gone("2"),
      goneHeading("Legacy importer"),
      gone("3"),
      gone("4"),
      gone("5"),
    ];
    expect(previewOf(run, 8).entries).toEqual([
      { kind: "block", block: run[0] },
      { kind: "skipped", count: 2, of: "removed" },
      { kind: "block", block: run[3] },
      { kind: "skipped", count: 3, of: "removed" },
    ]);
  });

  test("a long removed run shows its start and counts the rest; past the cap, changes are counted as more", () => {
    const run = [gone("1"), gone("2"), gone("3"), gone("4"), gone("5")];
    const { entries } = previewOf(run, 6);
    expect(entries).toEqual([
      { kind: "block", block: run[0] },
      { kind: "block", block: run[1] },
      { kind: "skipped", count: 3, of: "removed" },
    ]);
    const capped = previewOf([edited, same("x"), edited, same("y"), edited], 2);
    expect(capped.entries.filter((e) => e.kind === "block")).toHaveLength(2);
    expect(capped.more).toBe(1);
  });
});

test("one block edited: half the words shared, or a sentence kept nearly whole inside the other", () => {
  const words = (text: string) => text.toLowerCase().split(" ");
  expect(
    sameBlock(
      words("who answers support each week"),
      words("who answers support each week and who covers when they are away"),
    ),
  ).toBe(true);
  expect(sameBlock(words("done"), words("not done yet waiting on the review of the copy"))).toBe(
    false,
  );
  expect(
    sameBlock(words("ship the first slice in october"), words("platform work comes first now")),
  ).toBe(false);
});

test("overlap is the shared share of two word lists, with repeats", () => {
  expect(overlap(["a", "b"], ["a", "b"])).toBe(1);
  expect(overlap(["a", "a", "b"], ["a", "c", "d"])).toBeCloseTo(2 / 6);
  expect(overlap([], [])).toBe(1);
});
