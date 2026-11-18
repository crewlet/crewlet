/**
 * Every line diff the dashboard draws is the design system's `DiffView`: the
 * adapter hands it the screens' props, and the tree keeps no renderer, sheet
 * or line comparison of its own.
 */

import { cleanup, render } from "@testing-library/react";
import { afterEach, describe, expect, test, vi } from "vitest";
import { sheetRules, splitSelectors } from "~/test/sheets.ts";
import {
  lineOf,
  modules,
  parse,
  shippedText,
  walk,
  type Module,
  type Node,
} from "~/test/source.ts";
import { DiffView, type DiffGap, type DiffHunk, type DiffLine } from "./DiffView.tsx";

afterEach(cleanup);

/** A class of a diff drawn here: `diff-view` and its parts, without the kit's `crewlet-` prefix. */
const LOCAL_CLASS = /(?<![\w-])diff-view(?:__[\w-]+|--[\w-]+)?(?![\w-])/;

/** The functions a line diff of its own would declare. */
const COMPARERS = new Set([
  "compareLines",
  "compareSequences",
  "changedRuns",
  "diffLines",
  "middleSnake",
]);

/** Where a module writes a local diff class or declares a line comparison. */
function localDiffSites(module: Module): string[] {
  const out = shippedText(module)
    .filter((piece) => LOCAL_CLASS.test(piece.text))
    .map((piece) => `${module.path}:${piece.line} writes ${LOCAL_CLASS.exec(piece.text)![0]}`);
  const line = lineOf(module.text);
  walk(parse(module.text, module.lang), (node: Node) => {
    if (node.type !== "FunctionDeclaration" && node.type !== "VariableDeclarator") return;
    const name = (node.id as Node | null)?.name;
    if (typeof name === "string" && COMPARERS.has(name)) {
      out.push(`${module.path}:${line(node.start)} declares ${name}`);
    }
  });
  return out;
}

const tinted = (root: ParentNode) =>
  [...root.querySelectorAll<HTMLElement>(".crewlet-diff-view__line")].map((line) => [
    line.dataset.kind,
    line.classList.contains("crewlet-diff-view__line--add")
      ? "green"
      : line.classList.contains("crewlet-diff-view__line--remove")
        ? "red"
        : "plain",
  ]);

describe("the dashboard's DiffView", () => {
  test("is the design system's, every line tinted by its kind", () => {
    const { container } = render(<DiffView before={"keep\nold\n"} after={"keep\nnew\n"} />);
    expect(tinted(container)).toEqual([
      ["same", "plain"],
      ["remove", "red"],
      ["add", "green"],
    ]);
    expect(container.querySelector("[class]")?.className).toContain("crewlet-diff-view");
  });

  test("takes a screen's hunks, and draws the same with or without the code hue", () => {
    const hunks: DiffHunk[] = [
      {
        skipped: 2,
        lines: [
          { kind: "remove", text: "old", before: 3, after: null },
          { kind: "add", text: "new", before: null, after: 3 },
        ],
      },
    ];
    const plain = render(<DiffView hunks={hunks} />);
    const html = plain.container.innerHTML;
    expect(tinted(plain.container)).toEqual([
      ["remove", "red"],
      ["add", "green"],
    ]);
    plain.unmount();
    const coded = render(<DiffView hunks={hunks} hue="code" />);
    expect(coded.container.innerHTML).toBe(html);
  });
});

describe("a diff a host lets expand", () => {
  const same = (n: number, text: string): DiffLine => ({ kind: "same", text, before: n, after: n });
  const GAPPED: DiffHunk[] = [
    {
      skipped: 13,
      header: "@@ -14,2 +14,2 @@ func New()",
      lines: [
        { kind: "remove", text: "a", before: 14, after: null },
        { kind: "add", text: "b", before: null, after: 14 },
        same(15, "k"),
      ],
    },
    { skipped: 0, header: "@@ -16,1 +16,1 @@", lines: [same(16, "c")] },
  ];
  const row = (gap: DiffGap) => (
    <div data-gap="">{`Expand ${gap.hunk}:${gap.skipped ?? "?"}${gap.header ?? ""}`}</div>
  );
  const gaps = (root: ParentNode) =>
    [...root.querySelectorAll("[data-gap]")].map((g) => g.textContent);

  // GIT'S HEADER AND THE HOST'S ROW, only where the host asks: every other
  // diff keeps the kit's "N unchanged lines". Mutation: draw headers always.
  test("opens every hunk with the host's row, the first above the diff, and without it the skip row is unchanged", () => {
    const { container, unmount } = render(<DiffView hunks={GAPPED} expand={row} trailing={0} />);
    expect(gaps(container)).toEqual([
      "Expand 0:13@@ -14,2 +14,2 @@ func New()",
      "Expand 1:0@@ -16,1 +16,1 @@",
    ]);
    expect(container.querySelector(".crewlet-diff-view__skip")).toBeNull();
    // THE SECOND ROW SITS UNDER THE LINE ABOVE IT, inside the one diff.
    const diff = container.querySelector(".crewlet-diff-view")!;
    expect(gaps(diff)).toEqual(["Expand 1:0@@ -16,1 +16,1 @@"]);
    unmount();
    const plain = render(<DiffView hunks={GAPPED} />).container;
    expect(plain.querySelector("[data-gap]")).toBeNull();
    expect(plain.querySelector(".crewlet-diff-view__skip")!.textContent).toBe("13 unchanged lines");
    expect(plain.textContent).not.toContain("@@");
  });

  test("draws the row after the last hunk only where the host says lines may follow", () => {
    const last = (trailing: number | null) => {
      const { container, unmount } = render(
        <DiffView hunks={GAPPED} expand={row} trailing={trailing} />,
      );
      const said = gaps(container).at(-1);
      unmount();
      return said;
    };
    expect(last(0)).toBe("Expand 1:0@@ -16,1 +16,1 @@");
    expect(last(9)).toBe("Expand 2:9");
    expect(last(null)).toBe("Expand 2:?");
  });

  // TWO FILES OF ONE STACK ARE TWO DIFFS: a pick in one marks nothing in the
  // other. Mutation: share the pick across diffs, and this fails.
  test("two diffs of one stack keep their own picks and rows", () => {
    const { container } = render(
      <>
        <section>
          <DiffView hunks={GAPPED} expand={row} picked={(l) => l.after === 16} />
        </section>
        <section>
          <DiffView hunks={GAPPED} expand={row} />
        </section>
      </>,
    );
    const [one, two] = [...container.querySelectorAll("section")];
    expect(one!.querySelectorAll(".crewlet-diff-view__line--picked")).toHaveLength(1);
    expect(two!.querySelectorAll(".crewlet-diff-view__line--picked")).toHaveLength(0);
    expect(two!.querySelectorAll("[data-gap]")).toHaveLength(3);
  });

  // A HOST'S NOTE AND THE ROW share the line above the gap, the row last.
  test("keeps the host's own note on the line above a gap", () => {
    const { container } = render(
      <DiffView
        hunks={GAPPED}
        expand={row}
        trailing={0}
        notes={(l) => (l.after === 15 ? <p>a finding</p> : null)}
      />,
    );
    const notes = container.querySelector(".crewlet-diff-view__notes")!;
    expect([...notes.children].map((c) => c.textContent)).toEqual([
      "a finding",
      "Expand 1:0@@ -16,1 +16,1 @@",
    ]);
  });
  // A SPLIT HUNK ENDING ON MORE REMOVALS than additions ends on the pair holding
  // its last removal; narrow, the kit draws unified. Mutation: anchor on the last line.
  test("hangs the row under the last row the kit draws, split or narrowed to unified", () => {
    const ENDS_ON_REMOVALS: DiffHunk[] = [
      {
        skipped: 0,
        lines: [
          { kind: "remove", text: "r1", before: 1, after: null },
          { kind: "remove", text: "r2", before: 2, after: null },
          { kind: "add", text: "a1", before: null, after: 1 },
        ],
      },
      { skipped: 5, header: "@@ -8,1 +7,1 @@", lines: [same(8, "a9")] },
    ];
    const marked = (gap: DiffGap) => (
      <div data-gap="" data-split={gap.split || undefined}>{`G${gap.hunk}`}</div>
    );
    const order = (root: ParentNode) =>
      [...root.querySelectorAll("[data-kind]:not([aria-hidden]), [data-gap]")].map((el) =>
        el.hasAttribute("data-gap")
          ? `${el.textContent}${el.hasAttribute("data-split") ? " split" : ""}`
          : el.lastElementChild!.textContent,
      );
    const wide = render(
      <DiffView hunks={ENDS_ON_REMOVALS} expand={marked} layout="split" trailing={0} />,
    );
    expect(order(wide.container)).toEqual(["r1", "a1", "r2", "G1 split", "a9"]);
    wide.unmount();
    const width = vi.spyOn(Element.prototype, "clientWidth", "get").mockReturnValue(390);
    try {
      const narrow = render(
        <DiffView hunks={ENDS_ON_REMOVALS} expand={marked} layout="split" trailing={0} />,
      );
      expect(order(narrow.container)).toEqual(["r1", "r2", "a1", "G1", "a9"]);
    } finally {
      width.mockRestore();
    }
  });
});

describe("the tree", () => {
  test("draws no diff and compares no lines of its own", () => {
    expect(modules().flatMap(localDiffSites)).toEqual([]);
  });

  test("styles no diff of its own", () => {
    const rules = sheetRules().filter((rule) =>
      splitSelectors(rule.selector).some((selector) => LOCAL_CLASS.test(selector)),
    );
    expect(rules.map((rule) => `${rule.file} ${rule.selector}`)).toEqual([]);
  });

  test("it can tell: a local class or comparison is reported", () => {
    const text = [
      'export const A = () => <div className="diff-view__line" />;',
      "export function compareLines() { return null; }",
      "export const changedRuns = () => null;",
      'export const B = () => <div className="crewlet-diff-view__line" />;',
    ].join("\n");
    expect(localDiffSites({ path: "x.tsx", text, lang: "tsx" })).toEqual([
      "x.tsx:1 writes diff-view__line",
      "x.tsx:2 declares compareLines",
      "x.tsx:3 declares changedRuns",
    ]);
  });
});
