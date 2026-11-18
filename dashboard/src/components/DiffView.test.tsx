/**
 * Every line diff the dashboard draws is the design system's `DiffView`: the
 * adapter hands it the screens' props, and the tree keeps no renderer, sheet
 * or line comparison of its own.
 */

import { cleanup, render } from "@testing-library/react";
import { afterEach, describe, expect, test } from "vitest";
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
import { DiffView, type DiffHunk } from "./DiffView.tsx";

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
