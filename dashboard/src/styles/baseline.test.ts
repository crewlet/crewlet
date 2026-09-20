// @vitest-environment node
import { readdirSync, readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, test } from "vitest";

/**
 * A RESET REACHES THE DECLARATION IT UNDOES, AND IS A RESET WHEN IT GETS THERE.
 *
 * `@crewlethq/tokens` is the design system's document baseline and this product
 * disagrees with one line of it: `a:hover` underlines, and almost nothing here
 * is a link inside a sentence, so a rule struck under a breadcrumb, a grid cell
 * or a caption-sized `turn 28e93bc3 →` decorates a piece of chrome rather than
 * a phrase. `styles/base.css` says so at length.
 *
 * # Why this is a test, twice over
 *
 * It said so at length and was not running. The disagreement was written as
 * `a { text-decoration: none }` — a reset at (0,0,1) against a declaration at
 * (0,1,1) — so the baseline won every state a reader could actually see, and
 * the app rail underlined `My work` under the pointer for as long as the
 * comment claiming otherwise had been there.
 *
 * The first version of this file then checked the wrong half of that. It held
 * the reset SELECTOR for selector and never looked at the value, so writing
 * `a:hover { text-decoration: underline }` in base.css — the reported bug,
 * verbatim — left all four tests green. A gate that cannot fail for the one
 * regression it is named after is worth less than no gate, because it also
 * stops anybody writing the real one.
 *
 * Both failures are the same silent shape `tokens.test.ts` and
 * `proselinks.test.ts` exist for: an outranked declaration is not an error,
 * not a warning and not a build failure, and an unhovered screenshot looks
 * exactly right. A comment asserting what a package does is the worst place
 * for the claim to live, because the package is the half that moves.
 *
 * # What it checks
 *
 * No scan can decide whether one selector outranks another for the elements
 * they BOTH match — that is a question about a document. What it can do is
 * hold the reset against the package on four points, each of which was a live
 * hole when it was written:
 *
 *   1. FOR EVERY SELECTOR the baseline decorates an anchor at, base.css
 *      carries the same selector — equal specificity, ours imported second.
 *   2. AND WHAT IT CARRIES THERE IS A RESET. This is the half the review bot
 *      on the pull request caught missing.
 *   3. NO SHEET OF OURS decorates at a bare-anchor selector at all, so the
 *      hole cannot be reopened from `screens.css` (imported after `base.css`,
 *      where a bare `a:hover { text-decoration: underline }` would win on
 *      order with base.css untouched).
 *   4. THE IMPORT ORDER the whole arrangement rests on holds in `main.tsx`.
 *      Equal specificity means the later sheet wins; if the baseline were
 *      imported after our reset the bug returns with every rule unchanged.
 *
 * TWO-SIDED, like every gate in this directory: a reset for a selector the
 * baseline no longer decorates fails too. Not because it would suppress
 * anything — `.prose-link:hover` is (0,2,0) and outranks `a:hover` at (0,1,1),
 * so it would not — but because it is a rule with nothing on the other side,
 * indistinguishable to the next reader from one whose declaration they simply
 * failed to find. `a` itself is the exemption: what it answers is the UA
 * stylesheet, which underlines every anchor whatever the baseline says.
 *
 * CONDITIONAL RULES ARE NOT THE RESET AND ARE JUDGED SEPARATELY. base.css puts
 * the underline BACK under `forced-colors: active`, deliberately: that mode
 * forces `color` in both states, so the rung step the reset trades the
 * underline for is unobservable there and `text-decoration` is the one
 * property left to the author. A scan that lifted rules out of their at-rule
 * would read that block as the reset re-underlining everything. So every rule
 * carries its condition, the four checks above judge only unconditional ones,
 * and the forced-colors block gets a requirement of its own — it must exist
 * for as long as the reset does.
 */

const STYLES = fileURLToPath(new URL(".", import.meta.url));
const require_ = createRequire(import.meta.url);

/**
 * The baseline entry points, which is EVERY ONE `main.tsx` imports rather than
 * just the one named `base`.
 *
 * The first version read `/base` alone, on the reasoning that a document
 * baseline is where a document baseline lives. That is a fact about today's
 * package: nothing stops a bump moving the anchor rule into `themes` or
 * `density`, and the promise this file makes in prose — "a bump that decorates
 * `a:focus-visible` fails here rather than in somebody's browser" — would then
 * be false for four of the five sheets the application actually loads.
 */
const BASELINE = ["", "/themes", "/density", "/fonts", "/base"].map(
  (entry) => `@crewlethq/tokens/css${entry}`,
);

/** One selector's `text-decoration`, as written, with the condition it is under. */
export interface Decorated {
  /** Where it came from, for the failure message. */
  source: string;
  /** A single selector — a rule's comma list is split into one entry each. */
  selector: string;
  /** The declared value, lowercased and trimmed. */
  value: string;
  /**
   * The at-rule this sits under (`media (forced-colors: active)`), or "" when
   * it is unconditional. Only unconditional rules are the reset.
   */
  condition: string;
}

/** Is this declaration a reset — does it remove the line rather than draw one? */
export function isReset(value: string): boolean {
  // `(\s|$)` keeps `none !important` a reset. `initial` and `unset` both
  // compute to `none` here (text-decoration-line does not inherit), so they
  // are resets too; `revert` and `revert-layer` are NOT — they hand the
  // property back to the previous origin, which is the underline we are
  // undoing.
  // Case-insensitive because CSS keywords are: `decorated()` lowercases what
  // it parses, but this predicate is exported and a caller need not have.
  return /^(none|initial|unset)(\s|$)/i.test(value.trim());
}

/**
 * Every selector in `css` that declares a text-decoration line, with its
 * at-rule condition.
 *
 * Written as a brace walk rather than a regex over rule bodies, because the
 * first version's `[^{}]+\{[^{}]*\}` lifted a nested rule clean out of its
 * `@media` — which is precisely the misreading that would turn base.css's
 * forced-colors block into "the reset re-underlines everything".
 *
 * BOTH SPELLINGS COUNT. `text-decoration: underline` and
 * `text-decoration-line: underline` draw the same line, and a gate that knew
 * only the shorthand would be blind to a package that switched to the longhand
 * — silently, since the floor test would still pass on the other entry points.
 *
 * THE LAST DECLARATION IN A BLOCK WINS, as the cascade says it does: a block
 * that resets and then decorates is a decorating block.
 *
 * COMMENTS ARE BLANKED FIRST. Both sheets explain this property in prose that
 * quotes the declarations themselves, and a scan reading its own explanation
 * reports the explanation. `proselinks.test.ts` fell into that on its first
 * run; this one starts on the other side of it.
 */
export function decorated(css: string, source = "css"): Decorated[] {
  const text = css.replace(/\/\*[\s\S]*?\*\//g, " ");
  const out: Decorated[] = [];
  const conditions: string[] = [];
  let head = 0;
  for (let i = 0; i < text.length; i++) {
    const ch = text[i];
    if (ch === "{") {
      const prelude = text.slice(head, i).trim().replace(/\s+/g, " ");
      if (prelude.startsWith("@")) {
        // An at-rule: push its condition and keep walking into the body.
        conditions.push(prelude.replace(/^@/, ""));
        head = i + 1;
        continue;
      }
      const end = text.indexOf("}", i);
      const body = end === -1 ? text.slice(i + 1) : text.slice(i + 1, end);
      let value: string | null = null;
      for (const m of body.matchAll(/(?:^|;)\s*text-decoration(?:-line)?\s*:\s*([^;]+)/gi)) {
        value = m[1]!.trim().toLowerCase();
      }
      if (value !== null) {
        for (const selector of prelude.split(",")) {
          const trimmed = selector.trim();
          if (trimmed) {
            out.push({ source, selector: trimmed, value, condition: conditions.join(" and ") });
          }
        }
      }
      i = end === -1 ? text.length : end;
      head = i + 1;
      continue;
    }
    if (ch === "}") {
      conditions.pop();
      head = i + 1;
    }
  }
  return out;
}

/**
 * Those selectors that target an anchor as a bare element: `a`, `a:hover`,
 * `a:focus-visible`.
 *
 * DESCENDANT AND COMPOUND FORMS ARE NOT THIS REGISTER — `.prose.md a` and
 * `a.work-cal-chip` are registers that opt back in, and each outranks anything
 * at `a` on its own. What is judged here is the bare-element register the
 * baseline and `base.css` are both writing in.
 */
export function anchorRules(rules: Decorated[]): Decorated[] {
  return rules.filter((rule) => /^a(?::[a-z-]+(?:\([^)]*\))?)*$/i.test(rule.selector));
}

/** Only the rules that apply unconditionally — an at-rule block is its own question. */
export function unconditional(rules: Decorated[]): Decorated[] {
  return rules.filter((rule) => rule.condition === "");
}

/**
 * The effective declaration for `selector` — the LAST one in source order,
 * which is what the cascade resolves to among rules of equal specificity.
 *
 * A `some()` here was the second half of the value bug: with it, a base.css
 * that reset `a:hover` and then decorated it again lower down satisfied the
 * gate on the strength of the rule it had already overridden.
 */
export function effective(rules: Decorated[], selector: string): Decorated | undefined {
  let found: Decorated | undefined;
  for (const rule of rules) if (rule.selector === selector) found = rule;
  return found;
}

/** Baseline decorations our sheets do not answer with a reset at the same selector. */
export function unanswered(theirs: Decorated[], ours: Decorated[]): string[] {
  const out: string[] = [];
  for (const rule of unconditional(anchorRules(theirs))) {
    if (isReset(rule.value)) continue;
    const mine = effective(unconditional(anchorRules(ours)), rule.selector);
    if (!mine) {
      out.push(`${rule.source} decorates \`${rule.selector}\` and base.css carries no rule there`);
    } else if (!isReset(mine.value)) {
      out.push(
        `${rule.source} decorates \`${rule.selector}\` and base.css answers it with ` +
          `\`text-decoration: ${mine.value}\`, which is not a reset`,
      );
    }
  }
  return out;
}

/** Resets of ours with nothing on the other side of them. */
export function stale(theirs: Decorated[], ours: Decorated[]): string[] {
  const decorated_ = unconditional(anchorRules(theirs)).filter((rule) => !isReset(rule.value));
  return (
    unconditional(anchorRules(ours))
      // `a` answers the UA stylesheet, which underlines every anchor regardless.
      .filter((mine) => mine.selector !== "a" && isReset(mine.value))
      .filter((mine) => !decorated_.some((rule) => rule.selector === mine.selector))
      .map(
        (mine) => `base.css resets \`${mine.selector}\`, which no baseline entry point decorates`,
      )
  );
}

/** Bare-anchor decorations in OUR OWN sheets — the hole reopened from the other side. */
export function selfDecorated(ours: Decorated[]): string[] {
  return unconditional(anchorRules(ours))
    .filter((mine) => !isReset(mine.value))
    .map(
      (mine) =>
        `${mine.source} decorates \`${mine.selector}\` with \`${mine.value}\` — a bare-anchor ` +
        `rule in our own sheets re-underlines the whole product`,
    );
}

function read(specifier: string): string {
  return readFileSync(require_.resolve(specifier), "utf8");
}

/** Every stylesheet this application ships, ours and the package's. */
function baseline(): Decorated[] {
  return BASELINE.flatMap((name) => decorated(read(name), name));
}

function mine(): Decorated[] {
  return readdirSync(STYLES)
    .filter((f) => f.endsWith(".css"))
    .flatMap((name) => decorated(readFileSync(join(STYLES, name), "utf8"), name));
}

describe("the document baseline's anchor decoration", () => {
  const theirs = baseline();
  const ours = mine();
  const base = ours.filter((rule) => rule.source === "base.css");

  test("is answered at every selector it decorates at, and answered with a reset", () => {
    expect(
      unanswered(theirs, base),
      "a reset at a weaker selector is outranked silently, and a reset that is not `none` is not a reset",
    ).toEqual([]);
  });

  test("and a reset cannot outlive the declaration it undoes", () => {
    expect(
      stale(theirs, base),
      "drop it — a rule with nothing on the other side reads as one whose declaration nobody found",
    ).toEqual([]);
  });

  test("and no sheet of ours re-decorates a bare anchor from the other side", () => {
    // screens.css, frame.css and the rest are imported AFTER base.css, so a
    // bare `a:hover { text-decoration: underline }` in any of them wins on
    // order with base.css untouched and the two tests above green.
    expect(selfDecorated(ours)).toEqual([]);
  });

  test("and the import order the whole arrangement rests on holds", () => {
    // Equal specificity means the later sheet wins. This is the one premise
    // the rules themselves cannot carry: swap two lines in main.tsx and the
    // bug returns with every stylesheet byte-identical.
    const boot = readFileSync(fileURLToPath(new URL("../main.tsx", import.meta.url)), "utf8");
    const theirBase = boot.indexOf('"@crewlethq/tokens/css/base"');
    const ourBase = boot.indexOf('"./styles/base.css"');
    expect(theirBase, "main.tsx no longer imports the tokens baseline").toBeGreaterThan(-1);
    expect(ourBase, "main.tsx no longer imports our base.css").toBeGreaterThan(-1);
    expect(
      theirBase < ourBase,
      "main.tsx must import @crewlethq/tokens/css/base BEFORE ./styles/base.css — " +
        "at equal specificity the later sheet wins, so the reset has to come second",
    ).toBe(true);
  });

  test("and forced colours keep the underline the reset takes away", () => {
    // The reset trades the underline for a step within the text rungs. Forced
    // colours forces `color` in both states, so that step cannot be seen and
    // `text-decoration` is the only property left to the author. Measured:
    // without this block, hovering any link in that mode changes nothing.
    const forced = anchorRules(base).filter(
      (rule) => /forced-colors/.test(rule.condition) && !isReset(rule.value),
    );
    expect(
      forced.map((rule) => rule.selector),
      "base.css must put the underline back under `@media (forced-colors: active)` for every " +
        "anchor state it resets, or that mode loses its only hover affordance",
      // Every state the reset covers, minus the rest state: `a` is not a hover
      // affordance, so it is not owed one.
    ).toEqual(
      unconditional(anchorRules(base))
        .filter((rule) => isReset(rule.value) && rule.selector !== "a")
        .map((rule) => rule.selector),
    );
  });

  test("and the permanent-underline registers state their hover", () => {
    // `.prose-link` is (0,1,0) and the reset is (0,1,1), so a class alone puts
    // the mark back everywhere EXCEPT under a pointer — the one place a
    // permanent mark must not go missing, since it is where a reader decides
    // whether the word is a link. Deleting one of these `:hover` selectors
    // reads as removing a redundant duplicate.
    const drawn = new Set(ours.filter((rule) => !isReset(rule.value)).map((rule) => rule.selector));
    for (const register of [".prose-link", ".prose.md a", ".int-form-note a"]) {
      expect(drawn.has(register), `${register} draws the underline`).toBe(true);
      expect(
        drawn.has(`${register}:hover`),
        `${register} must also draw it at :hover — the (0,1,1) reset outranks it otherwise`,
      ).toBe(true);
    }
  });

  test("it is reading the package at all, so the scan cannot pass on nothing", () => {
    // Vacuous if an entry point moves or the bundle stops shipping the rule:
    // an empty `theirs` satisfies every check above perfectly. Same floor
    // idiom as tokens.test.ts and proselinks.test.ts.
    expect(BASELINE.length).toBe(5);
    expect(anchorRules(theirs).length).toBeGreaterThan(1);
    expect(anchorRules(theirs).some((rule) => !isReset(rule.value))).toBe(true);
    const hover = effective(unconditional(anchorRules(base)), "a:hover");
    expect(hover, "base.css declares no unconditional a:hover decoration").toBeDefined();
    expect(isReset(hover!.value)).toBe(true);
    expect(ours.length).toBeGreaterThan(5);
  });

  test("it can tell — every check fails on the mutation it is named for", () => {
    // THE MUTATIONS RUN AGAINST THE FUNCTIONS THE TESTS ABOVE CALL, not
    // against a second copy of their logic. tokens.test.ts records why: when
    // the real check is inlined in the test body and the "it can tell" case
    // re-spells it over a literal, the mutation test never touches the thing
    // it claims to cover — which is exactly how the value hole below shipped.
    const THEIRS = decorated(
      "a { text-decoration: none }\na:hover { text-decoration: underline }",
      "theirs",
    );
    const reset = decorated(
      "a { text-decoration: none }\na:hover { text-decoration: none }",
      "base.css",
    );

    // The control: the arrangement this PR ships.
    expect(unanswered(THEIRS, reset)).toEqual([]);
    expect(stale(THEIRS, reset)).toEqual([]);
    expect(selfDecorated(reset)).toEqual([]);

    // THE BUG, VERBATIM — the one the first version of this file let through.
    const reUnderlined = decorated(
      "a { text-decoration: none }\na:hover { text-decoration: underline }",
      "base.css",
    );
    expect(unanswered(THEIRS, reUnderlined)).toHaveLength(1);
    expect(unanswered(THEIRS, reUnderlined)[0]).toContain("not a reset");
    // And from the other side, where base.css is untouched.
    expect(selfDecorated(reUnderlined)).toHaveLength(1);

    // A reset overridden lower down in the same sheet is not a reset either.
    const overridden = decorated(
      "a:hover { text-decoration: none }\n.x {}\na:hover { text-decoration: underline }",
      "base.css",
    );
    expect(unanswered(THEIRS, overridden)).toHaveLength(1);
    // ...and a decoration overridden by a later reset IS answered.
    const repaired = decorated(
      "a:hover { text-decoration: underline }\na:hover { text-decoration: none }",
      "base.css",
    );
    expect(unanswered(THEIRS, repaired)).toEqual([]);

    // The reset dropped entirely.
    expect(unanswered(THEIRS, decorated("a { text-decoration: none }", "base.css"))).toHaveLength(
      1,
    );
    expect(unanswered(THEIRS, decorated("a { text-decoration: none }", "base.css"))[0]).toContain(
      "carries no rule there",
    );

    // A new state the package starts decorating.
    const focus = decorated("a:focus-visible { text-decoration: underline }", "theirs");
    expect(unanswered(focus, reset)).toHaveLength(1);

    // The longhand draws the same line, so it is seen and it is the same
    // finding. (Against `reset` it is ANSWERED, which is the other half of the
    // point: what the longhand changes is detection, not the verdict.)
    const longhand = decorated("a:hover { text-decoration-line: underline }", "theirs");
    expect(anchorRules(longhand)).toHaveLength(1);
    expect(unanswered(longhand, reset)).toEqual([]);
    expect(unanswered(longhand, decorated("a { text-decoration: none }", "base.css"))).toHaveLength(
      1,
    );
    // And our own sheets are judged on the longhand too.
    expect(
      selfDecorated(decorated("a:hover { text-decoration-line: underline }", "screens.css")),
    ).toHaveLength(1);

    // A comma list is one entry per selector, so `a, a:hover { none }` answers both.
    expect(
      unanswered(THEIRS, decorated("a,\na:hover { text-decoration: none }", "base.css")),
    ).toEqual([]);

    // The package dropping its decoration leaves our reset with nothing to reset.
    expect(stale(decorated("a { text-decoration: none }", "theirs"), reset)).toHaveLength(1);

    // AT-RULE CONTEXT. The forced-colors block must not read as the reset
    // re-underlining everything — the whole point of the brace walk.
    const withForced = decorated(
      "a:hover { text-decoration: none }\n@media (forced-colors: active) { a:hover { text-decoration: underline } }",
      "base.css",
    );
    expect(unanswered(THEIRS, withForced)).toEqual([]);
    expect(selfDecorated(withForced)).toEqual([]);
    expect(withForced.find((r) => r.value === "underline")?.condition).toBe(
      "media (forced-colors: active)",
    );
    expect(withForced.find((r) => r.value === "none")?.condition).toBe("");

    // What a reset is, and is not.
    expect(["none", "NONE", "none !important", "initial", "unset"].every(isReset)).toBe(true);
    expect(["underline", "revert", "revert-layer", "line-through", "overline"].some(isReset)).toBe(
      false,
    );

    // A descendant register is not the bare-element one these checks judge.
    expect(anchorRules(decorated(".prose.md a { text-decoration: underline }"))).toEqual([]);
    expect(anchorRules(decorated("a.work-cal-chip { text-decoration: underline }"))).toEqual([]);
    expect(anchorRules(decorated("a:focus-visible { text-decoration: none }")).length).toBe(1);

    // Prose ABOUT the property is not the property.
    expect(decorated("/* a:hover { text-decoration: underline } is wrong here */")).toEqual([]);
    // `text-underline-offset` is not a decoration line.
    expect(decorated(".x { text-underline-offset: 2px }")).toEqual([]);
  });
});
