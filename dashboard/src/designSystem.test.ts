// @vitest-environment node
/**
 * The rules that say the dashboard is drawn by the design system.
 *
 * Every rule here is a scan over the sources as TEXT, with comments removed so
 * a doc that quotes what was replaced is not read as a use of it. A scan is
 * the only shape these rules can take: each of them is about what the tree
 * does NOT contain, and nothing but a scan notices a reintroduction.
 *
 * Each rule carries a case that proves the pattern still recognises what it
 * polices. A pattern that matched nothing would pass every list below for
 * ever, which is the one way this file can lie.
 */

import { readFileSync, readdirSync } from "node:fs";
import { dirname, join, relative, resolve, sep } from "node:path";
import { fileURLToPath } from "node:url";
import { expect, test } from "vitest";

const SRC = resolve(dirname(fileURLToPath(import.meta.url)));

function walk(dir: string, match: RegExp): string[] {
  const out: string[] = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) out.push(...walk(path, match));
    else if (match.test(entry.name)) out.push(path);
  }
  return out;
}

/** This file, which spells every pattern it polices and is never its own case. */
const SCANNER = "designSystem.test.ts";

/** Every file the dashboard ships, tests included: a test spends tokens too. */
function files(match: RegExp): { name: string; text: string }[] {
  return walk(SRC, match)
    .map((path) => ({
      name: relative(SRC, path).split(sep).join("/"),
      text: readFileSync(path, "utf8"),
    }))
    .filter(({ name }) => name !== SCANNER);
}

function code(source: string): string {
  return source.replace(/\/\*[\s\S]*?\*\//g, "").replace(/^\s*\/\/.*$/gm, "");
}

/**
 * The token names the engine used to declare for itself.
 *
 * They are gone from the tree, so a `var()` naming one resolves to nothing and
 * takes its whole declaration with it, silently: the property falls back to
 * whatever it inherits, which is how `.prose` came to draw the model's speech
 * at the muted step. The names are listed rather than pattern-matched because
 * three of them are prefixes of a uilet name, and one of them, `--density`,
 * survives the move under its own name.
 */
const REMOVED_TOKENS = [
  "--accent",
  "--accent-hover",
  "--accent-ink",
  "--accent-line",
  "--accent-soft",
  "--bg",
  "--bg-sunken",
  "--border",
  "--border-strong",
  "--border-subtle",
  "--caution",
  "--caution-ink",
  "--caution-soft",
  "--control-h",
  "--control-h-sm",
  "--critical",
  "--critical-ink",
  "--critical-soft",
  "--drawer-w",
  "--dur",
  "--dur-fast",
  "--dur-slow",
  "--ease",
  "--ease-out",
  "--focus",
  "--focus-ring",
  "--font-mono",
  "--font-sans",
  "--fs-2xl",
  "--fs-2xs",
  "--fs-3xl",
  "--fs-3xs",
  "--fs-lg",
  "--fs-md",
  "--fs-sm",
  "--fs-xl",
  "--fs-xs",
  "--fw-bold",
  "--fw-medium",
  "--fw-normal",
  "--fw-semibold",
  "--heading",
  "--info",
  "--info-ink",
  "--info-soft",
  "--inset-hairline",
  "--lh-normal",
  "--lh-relaxed",
  "--lh-snug",
  "--lh-tight",
  "--measure",
  "--nav-gutter",
  "--nav-row-pad",
  "--org-card-w",
  "--phase-execute",
  "--phase-execute-ink",
  "--phase-onboarding",
  "--phase-onboarding-ink",
  "--phase-review",
  "--phase-review-ink",
  "--positive",
  "--positive-ink",
  "--positive-soft",
  "--r-full",
  "--r-lg",
  "--r-md",
  "--r-sm",
  "--r-xs",
  "--row-h",
  "--row-h-lg",
  "--row-h-sm",
  "--screen-max",
  "--scrollbar",
  "--scrollbar-hover",
  "--shadow-1",
  "--shadow-2",
  "--shadow-3",
  "--sidebar-w",
  "--space-0",
  "--space-1",
  "--space-10",
  "--space-12",
  "--space-16",
  "--space-2",
  "--space-3",
  "--space-4",
  "--space-5",
  "--space-6",
  "--space-8",
  "--surface-1",
  "--surface-2",
  "--surface-3",
  "--surface-active",
  "--surface-glass",
  "--surface-hover",
  "--surface-inset",
  "--text",
  "--text-faint",
  "--text-muted",
  "--text-on-fill",
  "--text-secondary",
  "--topbar-h",
  "--track-normal",
  "--track-tight",
  "--track-wide",
  "--viz-1",
  "--viz-2",
  "--viz-3",
  "--viz-4",
  "--viz-5",
  "--viz-other",
  "--z-drawer",
  "--z-modal",
  "--z-popover",
  "--z-sticky",
  "--z-toast",
];

/** A `var()` naming one of the removed tokens, whole-word. */
const REMOVED_TOKEN_USE = new RegExp(
  `var\\(\\s*(?:${REMOVED_TOKENS.map((name) => name.replace(/-/g, "\\-")).join("|")})\\s*[,)]`,
);

test("the removed-token scan recognises what it polices", () => {
  // Spelled by joining the pieces, so no case here is itself a use of a
  // removed name. A scanner that matched its own examples would be one edit
  // away from reporting the file that holds it.
  const use = (name: string) => `color: var(${name});`;
  expect(REMOVED_TOKEN_USE.test(use("--text" + "-muted"))).toBe(true);
  expect(REMOVED_TOKEN_USE.test(`padding: var(${"--space" + "-2"}) 0;`)).toBe(true);
  expect(REMOVED_TOKEN_USE.test(`gap: var(${"--r" + "-sm"}, 6px);`)).toBe(true);
  // A uilet name that begins with a removed one is not a use of it.
  expect(REMOVED_TOKEN_USE.test(use("--color-text-muted"))).toBe(false);
  expect(REMOVED_TOKEN_USE.test(use("--font-size-sm"))).toBe(false);
  expect(REMOVED_TOKEN_USE.test(use("--spacing-2"))).toBe(false);
});

test("no source names a token the engine no longer declares", () => {
  const offenders = files(/\.(css|tsx?)$/)
    .filter(({ text }) => REMOVED_TOKEN_USE.test(code(text)))
    .map(({ name }) => name);
  expect(offenders).toEqual([]);
});

/**
 * Every innermost rule of a stylesheet, as `[selector, declarations]`.
 *
 * Innermost because a nested at-rule's own body would otherwise be read as one
 * declaration block: the inner rules are what carry the colours.
 */
function rules(source: string): [string, string][] {
  return [...code(source).matchAll(/([^{}]+)\{([^{}]*)\}/g)].map((m) => [m[1]!.trim(), m[2]!]);
}

/** A `color:` declaration, told from `background-color:` and `border-color:`. */
const TEXT_COLOUR = /(^|[;\s])color:\s*var\(--color-text-muted\)/;

test("the decoration step is spent as a text colour on nothing but a glyph", () => {
  // `--color-text-muted` is measured BETWEEN 2.8:1 and 4.5:1, deliberately: it
  // is the decoration step, and a step that reached the text floor would
  // invite itself into a table cell. The palette suite measures the token and
  // says nothing about where it is spent, which is how the engine came to draw
  // "not set", "none", "nobody" and "not reported by this engine" in it. The
  // rule this restores covered the shell's stylesheet alone, and went with it;
  // every sheet the dashboard ships is in it now.
  const offenders = files(/\.css$/)
    .flatMap(({ name, text }) =>
      rules(text)
        .filter(([, body]) => TEXT_COLOUR.test(body))
        .flatMap(([selector]) => selector.split(",").map((s) => `${name}: ${s.trim()}`)),
    )
    // A glyph beside a label is decoration: the label carries the words.
    .filter((offender) => !/(^|\s|>)svg$/.test(offender));
  expect(offenders).toEqual([]);
});

test("the decoration-step scan reads rules, and recognises what it polices", () => {
  // A parser that found no rules would pass the test above for any stylesheet,
  // and a pattern that matched no declaration would pass it for any rule.
  const sheets = files(/\.css$/).map(({ name }) => name);
  expect(sheets).toContain("styles/base.css");
  expect(rules(".a {\n  color: red;\n}\n").map(([selector]) => selector)).toEqual([".a"]);
  const decoration = `color: var(--color-text${"-muted"})`;
  expect(TEXT_COLOUR.test(`  ${decoration};`)).toBe(true);
  expect(TEXT_COLOUR.test(`  font-size: 1px; ${decoration};`)).toBe(true);
  // A background or a border may carry the step: it is a mark there, not text.
  expect(TEXT_COLOUR.test(`  background-${decoration};`)).toBe(false);
  expect(TEXT_COLOUR.test(`  border-${decoration};`)).toBe(false);
});

/**
 * A recipe the design system owns is spelled nowhere else.
 *
 * Neither of these has an owner in this tree any more: the `.btn` class list
 * was `ui/primitives.tsx` and the `<kbd>` element was `ui/Kbd.tsx`, and both
 * are the design system's. Each used to be written by hand beside its
 * primitive as well: a menu trigger and a list's Move buttons spelled
 * `btn ghost sm icon` themselves, three links spelled `btn primary`, and the
 * shell drew a bare `<kbd>` that told every reader not on a Mac to press a key
 * they do not have.
 */

/** A string literal holding the `btn` class as a whole word. */
const BUTTON_CLASS = /(["\'`])(?:[^"\'`\n]*\s)?btn(?:\s[^"\'`\n]*)?\1/;
/** A `<kbd>` element written by hand. */
const KBD_ELEMENT = /<kbd[\s>]/;

test("the recipe scan recognises the markup it polices", () => {
  // A pattern that matched nothing would pass the two tests below for ever.
  expect(BUTTON_CLASS.test('<a className="btn sm primary" href="#/">')).toBe(true);
  expect(BUTTON_CLASS.test('cx("btn", "ghost")')).toBe(true);
  expect(BUTTON_CLASS.test("className={`btn ${tone}`}")).toBe(true);
  expect(BUTTON_CLASS.test('className="btn-group"')).toBe(false);
  expect(BUTTON_CLASS.test('className="subtn"')).toBe(false);
  expect(KBD_ELEMENT.test("<kbd>Command K</kbd>")).toBe(true);
  expect(KBD_ELEMENT.test('querySelectorAll("kbd")')).toBe(false);
});

test("nothing spells the button class list, which has no owner here", () => {
  const offenders = files(/\.tsx?$/)
    .filter(({ name }) => !/\.test\.tsx?$/.test(name))
    .filter(({ text }) => BUTTON_CLASS.test(code(text)))
    .map(({ name }) => name);
  expect(offenders).toEqual([]);
});

test("nothing draws a keycap by hand, which has no owner here either", () => {
  const offenders = files(/\.tsx?$/)
    .filter(({ name }) => !/\.test\.tsx?$/.test(name))
    .filter(({ text }) => KBD_ELEMENT.test(code(text)))
    .map(({ name }) => name);
  expect(offenders).toEqual([]);
});
