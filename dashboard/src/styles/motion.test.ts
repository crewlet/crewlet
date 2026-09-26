// @vitest-environment node
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, test } from "vitest";

import { modules, parse, walk } from "../test/source.ts";

/**
 * WHAT MOVES, AND WHY — rule 4 of `dashboard-design.md`, held against the
 * stylesheets.
 *
 * Nothing animates on a data push. `agents` is pushed twice per tool-loop round
 * and once more before every tool call, so an animation that plays when a row
 * ARRIVES or CHANGES plays continuously on a live company, and a list that
 * moves every time the engine speaks is a list nobody can read while it runs.
 * Two kinds of motion are allowed, and every animation here is declared as
 * one of them with its reason:
 *
 * - an ENTRANCE of a surface the reader opened — the palette rising, its veil
 *   fading in. It plays once, because somebody pressed something.
 * - a STEADY-STATE PULSE on something live right now — a round in flight, a
 *   run waiting on its answer, text being written. It runs for as long as the
 *   state holds rather than playing once when the state arrives, so a push
 *   that starts or ends one changes whether it runs and never triggers it.
 *
 * AND BOTH ARE NEUTRALISED FOR A READER WHO ASKED FOR LESS MOTION. An
 * entrance is covered by `base.css`'s document-wide rule, which ends it on its
 * first frame. A pulse is not — the document-wide rule plays one iteration
 * instantly and leaves the element wherever the animation's own resting state
 * is, which is a pulse that stopped rather than one that was taken away — so
 * each pulse has its own `animation: none` under the media query, and is
 * STATIC: drawn in its resting look, still saying "live", never moving.
 *
 * Keyed on the declaration for the reason every gate in this directory gives:
 * jsdom computes no cascade and runs no animation, so a screenshot is the only
 * other witness, and nobody screenshots a reduced-motion session.
 */

const STYLES = fileURLToPath(new URL(".", import.meta.url));
const SHEETS = readdirSync(STYLES).filter((f) => f.endsWith(".css"));

type Kind = "entrance" | "steady";

/**
 * Every animated rule, by selector. An entry nothing matches fails as loudly
 * as a rule nothing declares, so a deleted animation comes back here rather
 * than leaving an exemption behind for the next one to borrow.
 */
const ANIMATED: { selector: string; kind: Kind; why: string }[] = [
  {
    selector: ".veil",
    kind: "entrance",
    why: "The command palette's ground fading in, once, because somebody pressed ⌘K or /.",
  },
  {
    selector: ".palette",
    kind: "entrance",
    why: "The command palette itself rising into place on the same press.",
  },
  {
    selector: ".round.live .round-node::before",
    kind: "steady",
    why:
      "The ring on the tool-loop round that is running now. It runs for as " +
      "long as the round is live and stops when the round ends — the push " +
      "that ends the round removes the state rather than playing anything.",
  },
  {
    selector: ".waiting-dot",
    kind: "steady",
    why: "A phase waiting on its first answer from the model: live for as long as it waits.",
  },
  {
    selector: ".prose.stream::after",
    kind: "steady",
    why: "The caret on text being streamed: live for as long as the model is writing.",
  },
];

/**
 * Words a selector for a DATA PUSH is spelled with: a row that is new, a
 * value that changed, something that arrived or flashed. An animation keyed
 * to one of these plays every time the engine speaks.
 */
const PUSH_WORDS =
  /(^|[-_.\s])(new|fresh|flash|changed?|updated?|arriv\w*|pushed|enter\w*|appear\w*|incoming|delta|bump\w*)([-_.\s:[]|$)/i;

interface Rule {
  selector: string;
  body: string;
  /** The `@media` preludes the rule sits inside, outermost first. */
  media: string[];
  sheet: string;
}

/**
 * Every style rule with the media it sits in, and every keyframes name.
 *
 * A WALK OF THE BRACES rather than a regular expression over the text,
 * because the answer depends on nesting: a rule inside
 * `@media (prefers-reduced-motion: reduce)` means the opposite of the same
 * rule outside it, and a keyframes block's `0%`/`100%` are not selectors.
 */
function parseSheet(css: string, sheet: string): { rules: Rule[]; keyframes: string[] } {
  const text = css.replace(/\/\*[\s\S]*?\*\//g, "");
  const rules: Rule[] = [];
  const keyframes: string[] = [];
  const stack: { kind: "media" | "keyframes" | "rule" | "other"; prelude: string }[] = [];
  let start = 0;
  for (let i = 0; i < text.length; i++) {
    const c = text[i];
    if (c === "{") {
      const prelude = text.slice(start, i).trim().replace(/\s+/g, " ");
      const inKeyframes = stack.some((s) => s.kind === "keyframes");
      if (prelude.startsWith("@media")) stack.push({ kind: "media", prelude });
      else if (prelude.startsWith("@keyframes")) {
        keyframes.push(prelude.slice("@keyframes".length).trim());
        stack.push({ kind: "keyframes", prelude });
      } else if (prelude.startsWith("@") || inKeyframes) stack.push({ kind: "other", prelude });
      else {
        const end = text.indexOf("}", i);
        const body = text.slice(i + 1, end);
        const media = stack.filter((s) => s.kind === "media").map((s) => s.prelude);
        for (const selector of prelude.split(",")) {
          rules.push({ selector: selector.trim(), body, media, sheet });
        }
        i = end;
      }
      start = i + 1;
    } else if (c === "}") {
      stack.pop();
      start = i + 1;
    } else if (c === ";" && stack.length === 0) {
      start = i + 1;
    }
  }
  return { rules, keyframes };
}

const parsed = SHEETS.map((sheet) => parseSheet(readFileSync(join(STYLES, sheet), "utf8"), sheet));
const RULES = parsed.flatMap((p) => p.rules);
const KEYFRAMES = parsed.flatMap((p) => p.keyframes);

const REDUCED = /prefers-reduced-motion:\s*reduce/;

/** The value of the last `animation`/`animation-name` declaration in a body. */
function animationOf(body: string): string | null {
  const found = [...body.matchAll(/(?:^|;|\s)animation(?:-name)?\s*:\s*([^;]+)/g)];
  return found.length ? found[found.length - 1]![1]!.trim() : null;
}

/** The rules that START an animation: outside reduced motion, and not `none`. */
const ANIMATING = RULES.filter((r) => {
  if (r.media.some((m) => REDUCED.test(m))) return false;
  const value = animationOf(r.body);
  return value !== null && value !== "none";
});

describe("what animates", () => {
  test("every animation is declared, with its kind and its reason", () => {
    const declared = new Set(ANIMATED.map((a) => a.selector));
    const undeclared = ANIMATING.filter((r) => !declared.has(r.selector)).map(
      (r) => `${r.sheet}: ${r.selector}`,
    );
    expect(
      undeclared,
      "an animation nobody declared — add it to ANIMATED as an entrance or a steady-state " +
        "pulse, with why; if it plays when data arrives, it is neither and it goes",
    ).toEqual([]);
  });

  test("and every declaration is still a rule that animates", () => {
    const live = new Set(ANIMATING.map((r) => r.selector));
    expect(ANIMATED.filter((a) => !live.has(a.selector)).map((a) => a.selector)).toEqual([]);
  });

  test("no animation is keyed to a data push", () => {
    const pushed = ANIMATING.filter((r) => PUSH_WORDS.test(r.selector)).map(
      (r) => `${r.sheet}: ${r.selector}`,
    );
    expect(
      pushed,
      "an animation on a class a push adds plays every time the engine speaks",
    ).toEqual([]);
  });

  test("the push-word check itself catches what it exists for", () => {
    // A guard nothing catches is a claim: these are the shapes it must see.
    for (const selector of [".row.is-new", ".cell-flash", ".feed-row.entering", ".value.changed"]) {
      expect(PUSH_WORDS.test(selector), selector).toBe(true);
    }
    for (const selector of [".veil", ".palette", ".round.live .round-node::before"]) {
      expect(PUSH_WORDS.test(selector), selector).toBe(false);
    }
  });

  test("every keyframes block is used, and every animation names one that exists", () => {
    const used = new Set<string>();
    for (const rule of ANIMATING) {
      const value = animationOf(rule.body)!;
      const name = KEYFRAMES.find((k) => new RegExp(`(^|\\s)${k}(\\s|$)`).test(value));
      expect(
        name,
        `${rule.selector} animates "${value}", which no @keyframes declares`,
      ).toBeDefined();
      used.add(name!);
    }
    expect(KEYFRAMES.filter((k) => !used.has(k))).toEqual([]);
  });
});

describe("a reader who asked for less motion", () => {
  test("gets every entrance ended on its first frame, document-wide", () => {
    const global = RULES.filter(
      (r) => r.selector === "*" && r.media.some((m) => REDUCED.test(m)),
    ).map((r) => r.body.replace(/\s+/g, " "));
    expect(global.length, "base.css's document-wide reduced-motion rule is gone").toBe(1);
    for (const declaration of [
      /animation-duration:\s*0\.01ms !important/,
      /animation-iteration-count:\s*1 !important/,
      /transition-duration:\s*0\.01ms !important/,
    ]) {
      expect(global[0]).toMatch(declaration);
    }
  });

  test("gets every steady-state pulse static — taken away, not played once", () => {
    const stopped = new Set(
      RULES.filter(
        (r) => r.media.some((m) => REDUCED.test(m)) && animationOf(r.body) === "none",
      ).map((r) => r.selector),
    );
    const moving = ANIMATED.filter((a) => a.kind === "steady" && !stopped.has(a.selector)).map(
      (a) => a.selector,
    );
    expect(
      moving,
      "a pulse with no `animation: none` under prefers-reduced-motion: reduce",
    ).toEqual([]);
  });

  test("an entrance is not a pulse: it plays once", () => {
    for (const rule of ANIMATING) {
      const kind = ANIMATED.find((a) => a.selector === rule.selector)?.kind;
      const infinite = /\binfinite\b/.test(animationOf(rule.body)!);
      expect(infinite, `${rule.selector} is declared ${kind}`).toBe(kind === "steady");
    }
  });

  test("no component animates from its own style attribute, where no sheet can stop it", () => {
    // A `style={{ animation }}` is outside every stylesheet, so neither the
    // document-wide rule's specificity nor a pulse's own media query is
    // guaranteed to reach it, and this gate would never see it.
    // PARSED, so a comment that says "animation:" is not a style.
    const inline: string[] = [];
    for (const mod of modules()) {
      walk(parse(mod.text, mod.lang), (node) => {
        if (node.type !== "Property") return;
        const key = node.key as { type: string; name?: string; value?: unknown };
        const name = key.type === "Identifier" ? key.name : String(key.value ?? "");
        if (/^animation/.test(name ?? "")) inline.push(`${mod.path}: ${name}`);
      });
    }
    expect(inline).toEqual([]);
  });
});
