// @vitest-environment node
/**
 * RULE C7, AS AMENDED: a review's disc on a change's timeline wears its
 * verdict's tone, the verdict said in words beside it; nothing else on the
 * timeline reads a state's colour.
 */
import { describe, expect, test } from "vitest";

import { sheetRules, type SheetRule } from "~/test/sheets.ts";
import { type Lang, type Node, lineOf, modules, parse, stringValue, walk } from "~/test/source.ts";

const FEEDBACK = /--color-feedback-([a-z]+)-[\w-]+/g;
/** The modules a disc may take a verdict's tone in. */
const TONED = ["routes/gitlet/change/Conversation.tsx"];

/** Each timeline rule reading a state's colour it may not, as `selector -> token`. */
export function strayTones(rules: readonly SheetRule[]): string[] {
  const out: string[] = [];
  for (const rule of rules) {
    if (!/^\.code-(?:tl-|timeline|disc)/.test(rule.selector)) continue;
    const own = /^\.code-disc\[data-tone="(success|danger)"\]$/.exec(rule.selector)?.[1];
    for (const m of rule.body.matchAll(FEEDBACK)) {
      if (m[1] !== own) out.push(`${rule.selector} -> ${m[0]}`);
    }
  }
  return out;
}

/** Each `<Disc tone=…>` that is neither neutral nor a verdict's in a module allowed one. */
export function strayDiscs(path: string, text: string, lang: Lang): string[] {
  const out: string[] = [];
  const line = lineOf(text);
  walk(parse(text, lang), (node, parent) => {
    if (node.type !== "JSXAttribute") return true;
    const name = node.name as Node;
    const tag = parent?.name as Node | undefined;
    if (name.type !== "JSXIdentifier" || String(name.name) !== "tone") return true;
    if (tag?.type !== "JSXIdentifier" || String(tag.name) !== "Disc") return true;
    const value = node.value as Node | null;
    const expression =
      value?.type === "JSXExpressionContainer" ? (value.expression as Node) : value;
    const literal = stringValue(expression ?? undefined);
    const call =
      expression?.type === "CallExpression" &&
      (expression.callee as Node).type === "Identifier" &&
      String((expression.callee as Node).name) === "verdictTone";
    const ok = literal === "neutral" || ((literal !== null || call) && TONED.includes(path));
    if (!ok) out.push(`${path}:${line(node.start)}`);
    return true;
  });
  return out;
}

const rule = (selector: string, body: string): SheetRule => ({
  file: "gitlet.css",
  selector,
  body,
});

describe("a change's timeline", () => {
  test("reads a state's colour only on a review's disc, each tone its own family", () => {
    const rules = sheetRules();
    // NOT VACUOUS: both toned discs are ruled.
    expect(rules.filter((r) => r.selector.startsWith(".code-disc[data-tone=")).length).toBe(2);
    expect(strayTones(rules)).toEqual([]);
  });

  test("it can tell: a neutral disc, a row or a crossed tone reading a state's colour", () => {
    expect(
      strayTones([
        rule(".code-disc", "color: var(--color-feedback-info-ink);"),
        rule(".code-tl-event__said", "color: var(--color-feedback-danger-ink);"),
        rule('.code-disc[data-tone="success"]', "color: var(--color-feedback-danger-ink);"),
        rule('.code-disc[data-tone="danger"]', "background: var(--color-feedback-danger-soft);"),
      ]),
    ).toEqual([
      ".code-disc -> --color-feedback-info-ink",
      ".code-tl-event__said -> --color-feedback-danger-ink",
      '.code-disc[data-tone="success"] -> --color-feedback-danger-ink',
    ]);
  });

  test("tones a disc only with a verdict's tone, in the conversation", () => {
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

  test("it can tell: a disc toned outside the conversation, or by another word", () => {
    const merge = 'const a = <Disc glyph={g} tone="success" />;';
    expect(strayDiscs("routes/gitlet/change/Merge.tsx", merge, "tsx")).toEqual([
      "routes/gitlet/change/Merge.tsx:1",
    ]);
    const conv = "const a = <Disc glyph={g} tone={pick(x)} />;";
    expect(strayDiscs(TONED[0]!, conv, "tsx")).toEqual([`${TONED[0]}:1`]);
    const fine = [
      'const a = <Disc glyph={g} tone="neutral" />;',
      "const b = <Disc glyph={g} tone={verdictTone(r.verdict)} />;",
    ].join("\n");
    expect(strayDiscs(TONED[0]!, fine, "tsx")).toEqual([]);
  });
});
