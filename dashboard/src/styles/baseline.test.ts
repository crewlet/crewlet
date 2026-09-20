// @vitest-environment node
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { fileURLToPath } from "node:url";
import { describe, expect, test } from "vitest";

/**
 * A RESET REACHES THE DECLARATION IT UNDOES, OR IT IS NOT A RESET.
 *
 * `@crewlethq/tokens/css/base` is the design system's document baseline and
 * this product disagrees with one line of it: `a:hover` underlines, and almost
 * nothing here is a link inside a sentence, so a rule struck under a
 * breadcrumb, a grid cell or a caption-sized `turn 28e93bc3 →` decorates a
 * piece of chrome rather than a phrase. `styles/base.css` says so at length.
 *
 * # Why this is a test
 *
 * It said so at length and was not running. The disagreement was written as
 * `a { text-decoration: none }` — a reset at (0,0,1) against a declaration at
 * (0,1,1) — so the baseline won every state a reader could actually see, and
 * the app rail underlined `My work` under the pointer for as long as the
 * comment claiming otherwise had been there. Nothing failed: an outranked
 * declaration is not an error, not a warning and not a build failure, and the
 * unhovered screenshot in every review looked exactly right.
 *
 * That is the same silent shape `tokens.test.ts` and `proselinks.test.ts`
 * exist for, one level up: there the hazard is a name nothing declares, here
 * it is a rule nothing reaches. A comment asserting what a package does is the
 * worst place for the claim to live, because the package is the half that
 * moves — a bump that decorates `a:focus-visible` next would arrive green,
 * with no symptom until somebody tabs to a link.
 *
 * # What it checks, and why it keys on the SELECTOR
 *
 * No scan can decide whether one selector outranks another for the elements
 * they BOTH match — that is a question about a document. What it can do is
 * hold the reset selector for selector against the package: for every selector
 * the baseline decorates an anchor at, this tree carries a rule at the same
 * selector. Equal specificity, and ours is imported second, so the tie falls
 * our way by the order `main.tsx` fixes deliberately.
 *
 * TWO-SIDED, like every gate in this directory: a reset for a selector the
 * baseline no longer decorates fails too. That one is not tidiness — an
 * `a:hover` reset with nothing on the other side is (0,1,1) sitting on top of
 * `.prose-link` at (0,1,0), so it would go on suppressing the one underline
 * this product keeps, for a declaration that had stopped existing. `a` itself
 * is the exemption: what it answers is the UA stylesheet, which underlines
 * every anchor whatever the baseline says.
 */

const require_ = createRequire(import.meta.url);
const BASELINE = "@crewlethq/tokens/css/base";

/** One selector's `text-decoration`, as written. */
export interface Decorated {
  /** A single selector — a rule's comma list is split into one entry each. */
  selector: string;
  /** The declared value, trimmed. `none` is a reset; anything else is a mark. */
  value: string;
}

/**
 * Every selector in `css` that declares `text-decoration`.
 *
 * COMMENTS ARE BLANKED FIRST. Both sheets explain this property in prose that
 * quotes the declarations themselves, and a scan reading its own explanation
 * reports the explanation. `proselinks.test.ts` fell into that on its first
 * run; this one starts on the other side of it.
 */
export function decorated(css: string): Decorated[] {
  const text = css.replace(/\/\*[\s\S]*?\*\//g, " ");
  const out: Decorated[] = [];
  for (const rule of text.matchAll(/([^{}]+)\{([^{}]*)\}/g)) {
    const value = /(?:^|;)\s*text-decoration\s*:\s*([^;]+)/.exec(rule[2]!);
    if (!value) continue;
    for (const selector of rule[1]!.split(",")) {
      const trimmed = selector.trim().replace(/\s+/g, " ");
      if (trimmed) out.push({ selector: trimmed, value: value[1]!.trim() });
    }
  }
  return out;
}

/**
 * Those selectors that target an anchor: `a`, `a:hover`, `a:focus-visible`.
 *
 * DESCENDANT FORMS ARE NOT ANCHOR SELECTORS for this gate — `.prose.md a` is a
 * register that opts back in, not a document-wide rule, and it outranks
 * anything at `a` on its own. What is judged here is the bare-element register
 * the baseline and `base.css` are both writing in.
 */
export function anchorRules(rules: Decorated[]): Decorated[] {
  return rules.filter((rule) => /^a(?:[:[]|$)/.test(rule.selector));
}

function read(specifier: string): string {
  return readFileSync(require_.resolve(specifier), "utf8");
}

describe("the document baseline's anchor decoration", () => {
  const theirs = anchorRules(decorated(read(BASELINE)));
  const ours = anchorRules(decorated(read(fileURLToPath(new URL("base.css", import.meta.url)))));

  test("is answered at every selector it decorates at", () => {
    const unanswered = theirs
      .filter((rule) => rule.value !== "none")
      .filter((rule) => !ours.some((mine) => mine.selector === rule.selector))
      .map(
        (rule) => `${BASELINE} decorates \`${rule.selector}\` and base.css answers no such rule`,
      );
    expect(
      unanswered,
      "a reset at a weaker selector is outranked silently — write base.css's rule at the same one",
    ).toEqual([]);
  });

  test("and a reset cannot outlive the declaration it undoes", () => {
    // `a` is exempt: it answers the UA stylesheet, not this package.
    const stale = ours
      .filter((mine) => mine.selector !== "a")
      .filter(
        (mine) => !theirs.some((rule) => rule.selector === mine.selector && rule.value !== "none"),
      )
      .map((mine) => `base.css resets \`${mine.selector}\`, which ${BASELINE} no longer decorates`);
    expect(
      stale,
      "drop it — at (0,1,1) it outranks `.prose-link` and suppresses the one underline this product keeps",
    ).toEqual([]);
  });

  test("it can tell — an unreached reset is caught, a matching one is not", () => {
    // The mutation this file exists to catch, run against THE FUNCTIONS THE
    // TESTS ABOVE CALL rather than a second copy of their regexes.
    const theirBase =
      "a {\n  text-decoration: none;\n}\na:hover {\n  text-decoration: underline;\n}";
    expect(anchorRules(decorated(theirBase))).toEqual([
      { selector: "a", value: "none" },
      { selector: "a:hover", value: "underline" },
    ]);
    // The shape that shipped: a reset at `a` alone, with the hover unanswered.
    expect(
      anchorRules(decorated("a {\n  text-decoration: none;\n}")).map((r) => r.selector),
    ).toEqual(["a"]);
    // A comma list is one entry per selector, so `a, a:hover { … }` answers both.
    expect(
      anchorRules(decorated("a,\na:hover {\n  text-decoration: none;\n}")).map((r) => r.selector),
    ).toEqual(["a", "a:hover"]);
    // Prose ABOUT the property is not the property.
    expect(decorated("/* a:hover { text-decoration: underline } is wrong here */")).toEqual([]);
    // `text-decoration-offset` and friends are not the shorthand.
    expect(decorated(".x {\n  text-underline-offset: 2px;\n}")).toEqual([]);
    // A descendant register is not the bare-element one this gate judges.
    expect(anchorRules(decorated(".prose.md a {\n  text-decoration: underline;\n}"))).toEqual([]);
  });

  test("it is reading the package at all, so the scan cannot pass on nothing", () => {
    // Vacuous if the entry point moves or the bundle stops shipping the rule:
    // an empty `theirs` satisfies the forward test perfectly. Same floor idiom
    // as tokens.test.ts and proselinks.test.ts.
    expect(theirs.length).toBeGreaterThan(1);
    expect(theirs.some((rule) => rule.value !== "none")).toBe(true);
    expect(ours.map((rule) => rule.selector)).toContain("a:hover");
  });
});
