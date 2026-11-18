// @vitest-environment node
/**
 * RULE C7, AS AMENDED: a review's disc wears its verdict's tone, the merge box's discs their
 * conditions', a rail reviewer's mark its verdict's and an ask's dot its party's; nothing else.
 */
import { describe, expect, test } from "vitest";

import { sheetRules, type SheetRule } from "~/test/sheets.ts";
import { type Lang, type Node, lineOf, modules, parse, stringValue, walk } from "~/test/source.ts";

const FEEDBACK = /--color-feedback-([a-z]+)-[\w-]+/g;
/** The modules a disc may take a tone in, and the one call each may take it from. */
const TONED: Readonly<Record<string, readonly string[]>> = {
  "routes/gitlet/change/Conversation.tsx": ["verdictTone"],
  "routes/gitlet/change/Merge.tsx": ["sectionTone", "boxTone"],
  "routes/gitlet/change/Rail.tsx": ["verdictTone", "waitingTone"],
};

/** The one module whose discs may take a literal tone: a merged change's is success. */
const LITERAL = new Set(["routes/gitlet/change/Merge.tsx"]);

/** The rail's toned marks and dots: their `data-tone` only from an allowed call, never a literal. */
const RAIL_MARK = /\bcode-rail__(?:mark|dot)\b/;

/** Each timeline rule reading a state's colour it may not, as `selector -> token`. */
export function strayTones(rules: readonly SheetRule[]): string[] {
  const out: string[] = [];
  for (const rule of rules) {
    if (!/^\.code-(?:tl-|timeline|disc|merge-box|rail__mark|rail__dot)/.test(rule.selector))
      continue;
    const own = /^\.code-(?:disc|rail__mark|rail__dot)\[data-tone="(\w+)"\]$/.exec(
      rule.selector,
    )?.[1];
    for (const m of rule.body.matchAll(FEEDBACK)) {
      if (m[1] !== own) out.push(`${rule.selector} -> ${m[0]}`);
    }
  }
  return out;
}

/** Each toned `<Disc>`, rail mark or dot whose tone is neither neutral nor its module's call. */
export function strayDiscs(path: string, text: string, lang: Lang): string[] {
  const out: string[] = [];
  const line = lineOf(text);
  walk(parse(text, lang), (node, parent) => {
    if (node.type !== "JSXAttribute") return true;
    const name = node.name as Node;
    const tag = parent?.name as Node | undefined;
    if (name.type !== "JSXIdentifier") return true;
    const disc =
      String(name.name) === "tone" && tag?.type === "JSXIdentifier" && String(tag.name) === "Disc";
    const mark = String(name.name) === "data-tone" && RAIL_MARK.test(classOf(parent));
    if (!disc && !mark) return true;
    const value = node.value as Node | null;
    const expression =
      value?.type === "JSXExpressionContainer" ? (value.expression as Node) : value;
    const literal = stringValue(expression ?? undefined);
    const callee = expression?.type === "CallExpression" ? (expression.callee as Node) : undefined;
    const allowed = TONED[path] ?? [];
    const call = callee?.type === "Identifier" && allowed.includes(String(callee.name));
    const ok = literal === "neutral" || call || (disc && literal !== null && LITERAL.has(path));
    if (!ok) out.push(`${path}:${line(node.start)}`);
    return true;
  });
  return out;
}

/** A JSX element's `className` where it is a literal, "" otherwise. */
function classOf(element: Node | null): string {
  for (const a of (element?.attributes as Node[] | undefined) ?? []) {
    const n = a.name as Node | undefined;
    if (a.type === "JSXAttribute" && String(n?.name) === "className") {
      return stringValue((a.value as Node | null) ?? undefined) ?? "";
    }
  }
  return "";
}

const rule = (selector: string, body: string): SheetRule => ({
  file: "gitlet.css",
  selector,
  body,
});

describe("a change's timeline", () => {
  test("reads a state's colour only on a toned disc, mark or dot, each tone its own family", () => {
    const rules = sheetRules();
    // NOT VACUOUS: the four disc tones, the two verdict marks and the two waiting dots are ruled.
    const toned = (prefix: string) =>
      rules.filter((r) => r.selector.startsWith(`${prefix}[data-tone=`)).length;
    expect(toned(".code-disc")).toBe(4);
    expect(toned(".code-rail__mark")).toBe(2);
    expect(toned(".code-rail__dot")).toBe(2);
    expect(strayTones(rules)).toEqual([]);
  });

  test("it can tell: a neutral disc, a row or a crossed tone reading a state's colour", () => {
    expect(
      strayTones([
        rule(".code-disc", "color: var(--color-feedback-info-ink);"),
        rule(".code-tl-event__said", "color: var(--color-feedback-danger-ink);"),
        rule('.code-disc[data-tone="success"]', "color: var(--color-feedback-danger-ink);"),
        rule('.code-disc[data-tone="danger"]', "background: var(--color-feedback-danger-soft);"),
        rule(".code-merge-box__title", "color: var(--color-feedback-success-ink);"),
        rule('.code-rail__mark[data-tone="success"]', "color: var(--color-feedback-danger-ink);"),
        rule(
          '.code-rail__dot[data-tone="warning"]',
          "background: var(--color-feedback-warning-ink);",
        ),
      ]),
    ).toEqual([
      ".code-disc -> --color-feedback-info-ink",
      ".code-tl-event__said -> --color-feedback-danger-ink",
      '.code-disc[data-tone="success"] -> --color-feedback-danger-ink',
      ".code-merge-box__title -> --color-feedback-success-ink",
      '.code-rail__mark[data-tone="success"] -> --color-feedback-danger-ink',
    ]);
  });

  test("tones a disc, mark or dot only by its module's own call", () => {
    const stray: string[] = [];
    let discs = 0;
    for (const mod of modules()) {
      if (mod.lang !== "tsx" || mod.path.includes(".test.")) continue;
      if (mod.text.includes("<Disc")) discs++;
      stray.push(...strayDiscs(mod.path, mod.text, mod.lang));
    }
    expect(discs).toBeGreaterThan(0);
    expect(stray).toEqual([]);
  });

  test("it can tell: a disc toned outside its modules, or by another module's word", () => {
    const rail = 'const a = <Disc glyph={g} tone="success" />;';
    expect(strayDiscs("routes/gitlet/change/Rail.tsx", rail, "tsx")).toEqual([
      "routes/gitlet/change/Rail.tsx:1",
    ]);
    const conv = "routes/gitlet/change/Conversation.tsx";
    const merge = "routes/gitlet/change/Merge.tsx";
    expect(strayDiscs(conv, "const a = <Disc glyph={g} tone={pick(x)} />;", "tsx")).toEqual([
      `${conv}:1`,
    ]);
    expect(strayDiscs(conv, "const a = <Disc glyph={g} tone={sectionTone(s)} />;", "tsx")).toEqual([
      `${conv}:1`,
    ]);
    expect(strayDiscs(merge, "const a = <Disc glyph={g} tone={verdictTone(v)} />;", "tsx")).toEqual(
      [`${merge}:1`],
    );
    const railPath = "routes/gitlet/change/Rail.tsx";
    const marks = [
      '<span className="code-rail__mark" data-tone="danger" />;',
      '<span className="code-rail__dot" data-tone={kind === "agent" ? "info" : "warning"} />;',
      '<span className="code-rail__mark" data-tone={sectionTone(s)} />;',
      '<span className="code-rail__mark" data-tone={verdictTone(v)} />;',
      '<span className="code-rail__dot" data-tone={waitingTone(k)} />;',
      '<span className="code-check-mark" data-tone={checkStateTone(s)} />;',
    ].join("\n");
    expect(strayDiscs(railPath, marks, "tsx")).toEqual([
      `${railPath}:1`,
      `${railPath}:2`,
      `${railPath}:3`,
    ]);
    const row = "routes/gitlet/ChangeRow.tsx";
    expect(strayDiscs(row, marks.split("\n")[3]!, "tsx")).toEqual([`${row}:1`]);
    const fine = [
      'const a = <Disc glyph={g} tone="neutral" />;',
      "const b = <Disc glyph={g} tone={verdictTone(r.verdict)} />;",
    ].join("\n");
    expect(strayDiscs(conv, fine, "tsx")).toEqual([]);
    expect(strayDiscs(merge, "const c = <Disc glyph={g} tone={boxTone(r)} />;", "tsx")).toEqual([]);
  });
});
