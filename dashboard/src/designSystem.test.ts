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

/**
 * The dashboard has no component library of its own.
 *
 * `dashboard/src/ui` held one: a table, a chart kit, a canvas, a tree model
 * and twenty primitives, every one of them a second drawing of something the
 * design system ships. The directory is gone, and this is what stops it
 * coming back one convenient file at a time.
 */

/** An import of a path inside the directory that no longer exists. */
const UI_IMPORT = /from\s+["'](?:~\/ui\/|\.{1,2}(?:\/\.\.)*\/ui\/)/;

test("the library-import scan recognises what it polices", () => {
  expect(UI_IMPORT.test('import { DataTable } from "~/ui/DataTable.tsx";')).toBe(true);
  expect(UI_IMPORT.test('import { Canvas } from "../ui/Canvas.tsx";')).toBe(true);
  expect(UI_IMPORT.test('import { Canvas } from "../../ui/Canvas.tsx";')).toBe(true);
  // A directory whose name merely ends in "ui" is not that one.
  expect(UI_IMPORT.test('import { x } from "~/lib/gui/x.ts";')).toBe(false);
  expect(UI_IMPORT.test('import { Button } from "@crewlethq/ui";')).toBe(false);
});

test("no source reaches for a component library of the dashboard's own", () => {
  const offenders = files(/\.tsx?$/)
    .filter(({ text }) => UI_IMPORT.test(code(text)))
    .map(({ name }) => name);
  expect(offenders).toEqual([]);
  // And the directory itself is gone, which is the other half: an import scan
  // over an empty tree passes for ever.
  expect(readdirSync(SRC).map((entry) => entry.toString())).not.toContain("ui");
});

/**
 * A table a screen draws is a table with its frame.
 *
 * Every panel table here was handed to [DataTable] with its pagination, its
 * resizing and its settings cog switched off, and every list screen drew the
 * cog over a frame that had no page size in it because nothing paged. That is
 * now one decision rather than nineteen: a panel draws [RecordTable] and a
 * list screen hands [DataView] what `useTableChoices` holds, so the cog, the
 * pages, the column list and the URL the reader's choices live in arrive
 * together. A screen reaching past both for the table itself, or a list view
 * drawn without those choices, silently takes the frame away again on that
 * one screen, which is the shape this whole change was undoing.
 */
const RAW_TABLE = /<DataTable[\s/<>]/;
const LIST_VIEW = /<DataView[\s/<>]/;

/**
 * The choices a screen holds for its table, and where they are handed over.
 *
 * Two halves, because the identifier alone is not the claim: an import left
 * behind by a screen that stopped calling the hook still spells the name, and
 * a screen that calls it and never spreads the result draws the same frameless
 * table it drew before. So the scan reads what the call was assigned to, and
 * then looks for that name being spread into the view.
 */
const CHOICES_HELD = /const\s+(\w+)\s*=\s*useTableChoices\(/g;
const spreadOf = (name: string) => new RegExp(`\\{\\s*\\.\\.\\.${name}\\s*\\}`);

/** Which screens draw a list view without handing it the reader's choices. */
function unchosen(): string[] {
  return files(/\.tsx$/)
    .filter(({ text }) => LIST_VIEW.test(code(text)))
    .filter(({ text }) => {
      const source = code(text);
      const held = [...source.matchAll(CHOICES_HELD)].map((match) => match[1] as string);
      return !held.some((name) => spreadOf(name).test(source));
    })
    .map(({ name }) => name);
}

test("the table scans recognise what they police", () => {
  expect(RAW_TABLE.test("<DataTable {...recordTable(rows, columns)} />")).toBe(true);
  expect(RAW_TABLE.test("<DataTable<Row> data={rows} />")).toBe(true);
  expect(RAW_TABLE.test('<RecordTable screen="fleet" table="nodes" />')).toBe(false);
  expect(LIST_VIEW.test("<DataView<FeedRow>")).toBe(true);
  expect(LIST_VIEW.test("<DataView framed columns={columns} />")).toBe(true);
  // The toolbar and the footer are parts of a list view, not one.
  expect(LIST_VIEW.test("<DataViewToolbar />")).toBe(false);
  // And the choices are read from the assignment, then looked for at the view.
  const held = [
    ...code('const choices = useTableChoices({ screen: "tools" });').matchAll(CHOICES_HELD),
  ];
  expect(held.map((match) => match[1])).toEqual(["choices"]);
  expect(spreadOf("choices").test("<DataView framed {...choices} rows={rows} />")).toBe(true);
  expect(spreadOf("choices").test("<DataView framed {...others} rows={rows} />")).toBe(false);
});

test("a screen draws its table through the frame, never around it", () => {
  const raw = files(/\.tsx$/)
    .filter(({ name }) => name !== "components/common.tsx")
    .filter(({ text }) => RAW_TABLE.test(code(text)))
    .map(({ name }) => name);
  expect(raw).toEqual([]);
  expect(unchosen()).toEqual([]);
});

/**
 * A class the design system draws is never named by the engine.
 *
 * It is not the engine's to name: it changes on a bump, it is invisible to a
 * reader, and a suite that spells one is a test of the package rather than of
 * the screen. `testing.tsx` is the seam for the cases that genuinely have to
 * reach for an element uilet drew, and it asks uilet what it draws rather than
 * asserting a name. A component VARIABLE is the exception and is not a class:
 * a consumer is meant to set those, which is what they exist for.
 */
const PACKAGE_CLASS = /\.crewlet-[a-z]|contains\(\s*["'`]crewlet-/;

test("the package-class scan recognises what it polices", () => {
  expect(PACKAGE_CLASS.test('document.querySelector(".crewlet-modal-overlay")')).toBe(true);
  expect(PACKAGE_CLASS.test('{ selector: ".crewlet-toast__message" }')).toBe(true);
  expect(PACKAGE_CLASS.test('el.classList.contains("crewlet-callout--warning")')).toBe(true);
  // A component variable is a value the engine sets, not a class it names.
  expect(PACKAGE_CLASS.test("width: var(--crewlet-tree-canvas-card-width);")).toBe(false);
  expect(PACKAGE_CLASS.test('<img src="/static/crewlet-icon.svg" />')).toBe(false);
  expect(PACKAGE_CLASS.test('href="https://github.com/apps/crewlet-cto/"')).toBe(false);
});

test("nothing names a class the design system draws", () => {
  const offenders = files(/\.(css|tsx?)$/)
    .filter(({ text }) => PACKAGE_CLASS.test(code(text)))
    .map(({ name }) => name);
  expect(offenders).toEqual([]);
});

/**
 * A glyph is a component from the icons package, never markup and never a
 * font ligature.
 *
 * A ligature renders the WORD until the font arrives, renders it for good on
 * a closed network, and is read aloud as that word by a screen reader.
 */
const LIGATURE = /material-symbols/;
/** An `<svg>` element written by hand. */
const SVG_ELEMENT = /<svg[\s>]/;
/**
 * The drawings in this tree: none.
 *
 * There was one, and it is gone rather than forgotten. The builder's chart drew
 * the connectors between its cards, which is data with a shape rather than an
 * icon with a name and was a fair exception; the chart is the design system's
 * `TreeCanvas` now and the package draws them. A drawing this application makes
 * itself is a decision again, so the list stays and is empty.
 */
const DRAWINGS: string[] = [];

test("the glyph scans recognise what they police", () => {
  expect(LIGATURE.test('<span className="material-symbols-outlined">add</span>')).toBe(true);
  expect(SVG_ELEMENT.test('<svg viewBox="0 0 16 16">')).toBe(true);
  expect(SVG_ELEMENT.test("<svg>")).toBe(true);
  expect(SVG_ELEMENT.test('querySelector("svg path")')).toBe(false);
});

test("no source reaches for an icon font", () => {
  const offenders = files(/\.(css|tsx?|html)$/)
    .filter(({ text }) => LIGATURE.test(code(text)))
    .map(({ name }) => name);
  expect(offenders).toEqual([]);
});

test("nothing draws an icon by hand, and a real drawing says it is one", () => {
  const offenders = files(/\.tsx$/)
    .filter(({ name }) => !/\.test\.tsx$/.test(name))
    .filter(({ text }) => SVG_ELEMENT.test(code(text)))
    .map(({ name }) => name);
  expect(offenders).toEqual(DRAWINGS);
});

/**
 * A control the design system draws is never restyled from outside it.
 *
 * These are the components whose look IS their contract: a variant, a tone, a
 * size. A `className` on one is a second, invisible set of rules that the next
 * package bump silently wins or silently loses against, and every one of them
 * refuses the prop for that reason. The scan is what catches the attempt
 * before the type error explains it, and it covers the tests too, because a
 * suite that styled a control would be asserting its own CSS.
 */
const CONTROLS = [
  "Avatar",
  "Button",
  "ButtonLink",
  "Checkbox",
  "Count",
  "EmptyValue",
  "FilterChip",
  "IconButton",
  "Input",
  "Kbd",
  "Meter",
  "SegmentedControl",
  "Select",
  "StatCard",
  "StatusDot",
  "Tag",
  "Textarea",
];

const OPENS_CONTROL = new RegExp(`<(?:${CONTROLS.join("|")})(?=[\\s/>])`, "g");

/**
 * Every opening tag of one of those controls, whole.
 *
 * A BALANCED SCAN, for the same reason the class reader below is one: a
 * pattern that stopped at the first `>` stopped inside `onClick={() => x}`,
 * and every control whose handler came before its `className` walked straight
 * through. Quotes are tracked too, so a `>` inside a string is not the end of
 * the tag either.
 */
function styledControls(source: string): string[] {
  const text = code(source);
  const out: string[] = [];
  for (const match of text.matchAll(OPENS_CONTROL)) {
    let depth = 0;
    let quote: string | null = null;
    let i = match.index + match[0].length;
    for (; i < text.length; i += 1) {
      const c = text[i]!;
      if (quote) {
        if (c === quote && text[i - 1] !== "\\") quote = null;
      } else if (c === '"' || c === "'" || c === "`") quote = c;
      else if (c === "{") depth += 1;
      else if (c === "}") depth -= 1;
      else if (c === ">" && depth === 0) break;
    }
    const tag = text.slice(match.index, i + 1);
    if (/(?:^|\s)(?:className|style)=/.test(tag)) out.push(tag);
  }
  return out;
}

test("the styled-control scan recognises what it polices", () => {
  expect(styledControls('<Tag className="mine">x</Tag>')).toHaveLength(1);
  expect(styledControls("<Button\n  variant='primary'\n  style={{ margin: 0 }}\n>")).toHaveLength(
    1,
  );
  // A handler holds a `>` of its own, and used to end the scan early.
  expect(styledControls('<Button onClick={() => go()} className="x">')).toHaveLength(1);
  expect(styledControls('<Tag title="a > b" className="x">')).toHaveLength(1);
  // A component whose whole job is layout is not one of these.
  expect(styledControls('<Card className="bchart">')).toHaveLength(0);
  // And a name that merely starts with one of theirs is a different component.
  expect(styledControls('<ButtonRow className="row">')).toHaveLength(0);
  // A control with no look of its own passed in is the ordinary case.
  expect(styledControls('<Button variant="primary" onClick={() => go()}>')).toHaveLength(0);
});

test("nothing restyles a control the design system draws", () => {
  const offenders = files(/\.tsx$/)
    .filter(({ text }) => styledControls(text).length > 0)
    .map(({ name }) => name);
  expect(offenders).toEqual([]);
});

/**
 * A card's head keeps its own inset, and no screen restates it.
 *
 * Twenty-three heads here carried
 * `style={{ paddingInline: "var(--spacing-4)", paddingTop: "var(--spacing-3)" }}`,
 * which is the padding `.crewlet-card__header` already sets: measured on a
 * running build, a head is 48px tall and inset by 12px and 16px with the
 * attribute and without it. Every one was the engine reaching past the design
 * system for a value the design system owns, and a no-op is the worst shape
 * that can take, because nothing it does can go wrong until the package
 * changes the value underneath it and twenty-three screens quietly refuse the
 * change. The head is also where a panel table's pager and cog are drawn now,
 * so its inset is the gap those controls sit in.
 *
 * The scan is on the DECLARATION rather than on the element, because a style
 * prop is what the rule is about: a screen that genuinely has to move a head
 * would be making a claim about that one head, and this pattern would not
 * catch it.
 */
const HEADER_INSET =
  /paddingInline:\s*"var\(--spacing-4\)"\s*,\s*paddingTop:\s*"var\(--spacing-3\)"/;

test("the card-head scan recognises what it polices", () => {
  expect(
    HEADER_INSET.test(
      '<Card.Header style={{ paddingInline: "var(--spacing-4)", paddingTop: "var(--spacing-3)" }}>',
    ),
  ).toBe(true);
  // A head a screen genuinely moves says something the card does not.
  expect(HEADER_INSET.test("<Card.Header style={{ paddingTop: 0 }}>")).toBe(false);
});

test("no screen restates the inset a card head already carries", () => {
  const offenders = files(/\.tsx$/)
    .filter(({ text }) => HEADER_INSET.test(code(text)))
    .map(({ name }) => name);
  expect(offenders).toEqual([]);
});

/**
 * A rule that clips text is drawn on a box that can clip.
 *
 * `overflow` does NOTHING on an inline box, so `.truncate` clipped inside a
 * flex row, which blockifies its items, and nowhere else. Spelled on a span in
 * a table cell it clipped nothing: the org directory's Doing cell ran past its
 * column and pushed the table 6px, 32px, 54px and 43px past its own scroller
 * at 1440, 1280, 1100 and 900, so a table whose columns fitted exactly scrolled
 * sideways anyway. It is invisible in review, because the same class in the
 * same file does the right thing three lines away.
 *
 * So every ellipsis rule here has to say how its box is laid out: a `display`
 * of its own, or a `flex` shorthand, which says it is an item of a row that
 * will blockify it. Reading the DECLARATIONS rather than the rendering is the
 * only shape this can take in a suite: jsdom computes no layout, and the one
 * measurement that catches it needs a real browser.
 */
function clippingRules(source: string): string[] {
  const out: string[] = [];
  for (const match of source.matchAll(/([^{}]+)\{([^{}]*)\}/g)) {
    const body = match[2] ?? "";
    if (!/text-overflow:\s*ellipsis/.test(body)) continue;
    if (/(^|;|\s)display:/.test(body) || /(^|;|\s)flex:/.test(body)) continue;
    out.push((match[1] ?? "").trim().replace(/\s+/g, " ").slice(-60));
  }
  return out;
}

test("the clipping scan recognises what it polices", () => {
  expect(clippingRules(".a { overflow: hidden; text-overflow: ellipsis; }")).toEqual([".a"]);
  // Blockified by its own display, or by the row it is an item of.
  expect(clippingRules(".a { display: inline-block; text-overflow: ellipsis; }")).toEqual([]);
  expect(clippingRules(".a { flex: 1 1 auto; text-overflow: ellipsis; }")).toEqual([]);
  // A rule that clips nothing is not one of these.
  expect(clippingRules(".a { overflow: hidden; }")).toEqual([]);
});

test("no stylesheet clips text on a box that cannot clip", () => {
  const offenders = files(/\.css$/).flatMap(({ name, text }) =>
    clippingRules(text).map((selector) => `${name}: ${selector}`),
  );
  expect(offenders).toEqual([]);
});

/**
 * Colour comes from a token, never from a literal.
 *
 * A literal is measured by nothing: the palette suite reads the tokens, and a
 * hex in a stylesheet or a style prop is outside every floor it holds. It is
 * also theme-blind, which is how the dashboard came to draw a failure mark in
 * a red that vanished on the dark ground.
 */
const COLOUR_LITERAL = /#[0-9a-fA-F]{3,8}\b|\brgba?\(\s*\d|\bhsla?\(\s*\d/;

test("the colour-literal scan recognises what it polices", () => {
  expect(COLOUR_LITERAL.test("color: #ff0000;")).toBe(true);
  expect(COLOUR_LITERAL.test("background: rgba(0, 0, 0, 0.4);")).toBe(true);
  expect(COLOUR_LITERAL.test("border-color: hsl(210 10% 40%);")).toBe(true);
  expect(COLOUR_LITERAL.test("color: var(--color-text-primary);")).toBe(false);
  // A fragment identifier is not a colour, and neither is a hex in a word.
  expect(COLOUR_LITERAL.test('href="#/seats/dev-a"')).toBe(false);
});

test("no stylesheet or style prop spells a colour", () => {
  // `.ts` as well as `.tsx`: a hue chosen in a plain module and handed to a
  // chart is as unmeasured as one written into a rule, and `lib/phases.ts` is
  // exactly such a module.
  const offenders = files(/\.(css|tsx?)$/)
    .filter(({ text }) => COLOUR_LITERAL.test(code(text)))
    .map(({ name }) => name);
  expect(offenders).toEqual([]);
});

/**
 * Colour is never derived from an identity either.
 *
 * `colorSeed` hashed a name into a hue, which is the rule this design system
 * exists to refuse: colour carries STATE, and a seat, a unit, an event
 * category and a tool origin are all neutral. Their identity is carried by
 * name, glyph and position.
 */
test("nothing hashes a name into a colour", () => {
  const offenders = files(/\.(css|tsx?)$/)
    .filter(({ text }) => /colorSeed/.test(code(text)))
    .map(({ name }) => name);
  expect(offenders).toEqual([]);
});

/**
 * A negative assertion over a class nothing draws is a vacuous pass.
 *
 * `expect(container.querySelector(".badge")).toBeNull()` is trivially true the
 * moment no element carries `.badge`, so a guard written against a primitive
 * this adoption replaced goes on passing while asserting nothing at all, and
 * nothing reports it. The rule is therefore not a list of the classes that
 * went: every class a query names has to be one an engine stylesheet still
 * declares or a piece of engine markup still writes, which is generated from
 * the stylesheets and the sources themselves and cannot fall out of date.
 */
function drawnClassNames(): Set<string> {
  const out = new Set<string>();
  // Every class an engine stylesheet declares.
  for (const { text } of files(/\.css$/)) {
    for (const [, name] of code(text).matchAll(/\.(-?[_a-zA-Z][\w-]*)/g)) out.add(name!);
  }
  /*
   * And every class engine markup writes, because a class with no rule behind
   * it is still a real one: the builder marks its live region with one so a
   * suite can tell it from the notification host's, and nothing draws it.
   */
  const written = writtenClassNames();
  for (const name of written.names) out.add(name);
  for (const stem of written.stems) out.add(stem);
  return out;
}

/**
 * Every class engine markup can put on an element.
 *
 * A NAME OR A STEM. A class built by interpolation
 * (`` `tier tier--${row.tier}` ``) is only knowable up to the fixed part, so
 * the fragment left where the interpolation was is recorded as a stem and
 * anything beginning with it counts as written. Recording the stem as a whole
 * name instead would make every selector under it unmatchable; ignoring it
 * would make every one of them look dead.
 */
function writtenClassNames(): { names: Set<string>; stems: string[] } {
  const names = new Set<string>();
  const stems: string[] = [];
  for (const { text } of files(/\.tsx?$/)) {
    for (const spelling of classAttributes(text)) {
      for (const name of spelling.split(/\s+/)) {
        if (!name) continue;
        if (name.endsWith("-")) stems.push(name);
        else names.add(name);
      }
    }
    // A class an element is given after it is drawn is written just as much.
    for (const [, name] of code(text).matchAll(
      /classList\.(?:add|remove|toggle)\(\s*["\'`]([^"\'`]+)/g,
    )) {
      names.add(name!);
    }
  }
  return { names, stems };
}

/**
 * Every class name a file's `className` attributes spell.
 *
 * A BALANCED SCAN, not a lazy regex. `className={cond ? \`a a--${x}\` : "a"}`
 * holds a `}` of its own, so a pattern that stopped at the first one read half
 * the expression and found no complete literal in it: the class came back as
 * drawn by nobody, its rules were swept as dead, and the scan that should have
 * caught that had the same hole.
 */
function classAttributes(source: string): string[] {
  const text = code(source);
  const out: string[] = [];
  const mark = "className=";
  for (let at = text.indexOf(mark); at >= 0; at = text.indexOf(mark, at + 1)) {
    let i = at + mark.length;
    if (text[i] === '"' || text[i] === "'") {
      const end = text.indexOf(text[i], i + 1);
      if (end > 0) out.push(text.slice(i + 1, end));
      continue;
    }
    if (text[i] !== "{") continue;
    let depth = 0;
    let j = i;
    for (; j < text.length; j += 1) {
      if (text[j] === "{") depth += 1;
      else if (text[j] === "}") {
        depth -= 1;
        if (depth === 0) break;
      }
    }
    // The interpolations go first: what is left of a template literal between
    // them is the fixed part, which is the part that names classes.
    const expression = text.slice(i + 1, j).replace(/\$\{[^}]*\}/g, " ");
    for (const [, literal] of expression.matchAll(/["'`]([^"'`]*)["'`]/g)) out.push(literal!);
  }
  return out;
}

/** Every class named inside a `querySelector`, `querySelectorAll` or `closest`. */
function queriedClasses(source: string): string[] {
  const out: string[] = [];
  for (const [, selector] of code(source).matchAll(
    /(?:querySelector(?:All)?|closest)\(\s*["'`]([^"'`]*)["'`]/g,
  )) {
    for (const [, name] of selector!.matchAll(/\.(-?[_a-zA-Z][\w-]*)/g)) out.push(name!);
  }
  return out;
}

test("the vacuous-assertion scan reads selectors, and finds the stylesheets", () => {
  expect(drawnClassNames().size).toBeGreaterThan(50);
  // A class with no rule behind it is still drawn, so it still counts.
  expect(drawnClassNames().has("org-builder-live")).toBe(true);
  // And one spelled inside a template literal with an interpolation in it.
  expect(classAttributes('className={on ? `tier tier--${x}` : "tier"}')).toEqual([
    "tier tier-- ",
    "tier",
  ]);
  expect(classAttributes('<div className="row gap-2">')).toEqual(["row gap-2"]);
  expect(queriedClasses('el.closest(".diff-line")')).toEqual(["diff-line"]);
  // The singular form too: written `querySelectorAll?` it matched only the
  // plural, and every `querySelector(".gone")` in the tree went unread.
  expect(queriedClasses('root.querySelector(".turn-card")')).toEqual(["turn-card"]);
  expect(queriedClasses('root.querySelectorAll("details.int-seat-form")')).toEqual([
    "int-seat-form",
  ]);
  expect(queriedClasses('el.querySelector("svg path")')).toEqual([]);
  expect(queriedClasses('el.closest("[data-row-id]")')).toEqual([]);
});

test("no query names a class no stylesheet draws", () => {
  const drawn = drawnClassNames();
  const offenders = files(/\.tsx?$/)
    .flatMap(({ name, text }) =>
      queriedClasses(text)
        .filter((cls) => !drawn.has(cls))
        .map((cls) => `${name}: .${cls}`),
    )
    .sort();
  expect(offenders).toEqual([]);
});

/**
 * The shared composition layer spells no em dash, in copy or as a value.
 *
 * TWO RULES MEET HERE. The product writes no em dash in anything that ships,
 * and an absent value is `EmptyValue` rather than a punctuation mark a screen
 * reader says "dash" to or skips. Both were broken in the same three files: a
 * tooltip and two sentences carried the dash as punctuation, and five header
 * facts drew a bare one where a model, a round count or a token total was
 * missing, so the cell with the least to say said nothing at all.
 *
 * SCOPED TO `components/`, and that is not a compromise: these files are the
 * engine's own composition, finished here and imported by every screen, so no
 * later package may edit one. A rule they cannot be held to is a rule nobody
 * can keep. The screens still owe the same sweep and own their own copy.
 */
test("no shared component spells an em dash, in its copy or for an absent value", () => {
  // `files` matches on the BASENAME as it walks, so the directory is a filter
  // over what comes back rather than part of the pattern.
  const shared = files(/\.tsx$/).filter(
    ({ name }) => name.startsWith("components/") && !name.endsWith(".test.tsx"),
  );
  const offenders = shared
    .filter(({ text }) => code(text).includes("\u2014"))
    .map(({ name }) => name);
  expect(offenders).toEqual([]);
  // A pattern that matched nothing would pass that for ever, and the scan has
  // to be reading these files at all.
  expect(shared.map(({ name }) => name)).toContain("components/common.tsx");
  // A dash a comment spells is a dash nobody reads, so `code` takes it out
  // first. A comment that FOLLOWS code on its line is not taken out, here or
  // in any scan above: a `//` inside a string is indistinguishable from one
  // that opens a comment without parsing, and every rule here would rather
  // report a line twice than read past one.
  expect(code("// a prose \u2014 in a note\n").includes("\u2014")).toBe(false);
  expect(code('const a = "a \u2014 in copy";').includes("\u2014")).toBe(true);
});

/**
 * A rule no element can match is a rule that is already wrong.
 *
 * A STYLESHEET IS NOT SELF-CHECKING. Every other scan here is about what a
 * source SPELLS, and a selector is the opposite: it names a class it hopes
 * somebody else writes, so it goes on sitting in the file, formatted and
 * commented and reviewed, long after the markup it was aimed at stopped
 * existing. Nothing goes red. The screen simply loses the rule.
 *
 * That is not hypothetical here. The shell used to draw `.screen-inner`
 * inside its scroller, and the org builder's chart lens took its height from
 * a rule that began there; the design system's shell draws no such wrapper,
 * so the rule stopped applying the day the shell was rebuilt and the canvas
 * collapsed to nothing with every suite still green. Four more went the same
 * way in the same swap: the tab panel's class, the dialog and drawer bodies'
 * and the field's, each one a class a component used to draw and the package
 * draws differently now.
 *
 * A CLASS THE PACKAGE DRAWS IS NOT AN ANSWER: the scan above refuses one of
 * those in engine CSS outright, so every class selected here has to be the
 * engine's own, and the engine's own are exactly the ones its markup writes.
 */

/** Every class name an engine stylesheet selects on. */
function selectedClasses(source: string): string[] {
  const out: string[] = [];
  for (const [, selector] of code(source).matchAll(/([^{}]+)\{[^{}]*\}/g)) {
    if (selector!.trim().startsWith("@")) continue;
    for (const [, name] of selector!.matchAll(/\.(-?[_a-zA-Z][\w-]*)/g)) out.push(name!);
  }
  return out;
}

test("the dead-rule scan reads selectors, and knows a stem from a name", () => {
  expect(selectedClasses(".a .b > .c, .d:hover { color: red }")).toEqual(["a", "b", "c", "d"]);
  // An at-rule's own prelude names no class; the rules inside it are read on
  // their own pass, because the matcher takes the innermost braces.
  expect(selectedClasses("@media (min-width: 40rem) { .e { gap: 0 } }")).toEqual(["e"]);
  const written = writtenClassNames();
  expect(written.names.size).toBeGreaterThan(50);
  // A class built by interpolation is known only up to its fixed part.
  expect(written.stems.some((stem) => stem.endsWith("-"))).toBe(true);
});

test("no stylesheet rule waits for a class nothing writes", () => {
  const { names, stems } = writtenClassNames();
  const offenders = files(/\.css$/)
    .flatMap(({ name, text }) =>
      selectedClasses(text)
        .filter((cls) => !names.has(cls) && !stems.some((stem) => cls.startsWith(stem)))
        .map((cls) => `${name}: .${cls}`),
    )
    .sort();
  expect([...new Set(offenders)]).toEqual([]);
});

/**
 * Every stylesheet the dashboard ships, and how long it is allowed to be.
 *
 * A BUDGET RATHER THAN A SNAPSHOT. The point of this adoption is that the
 * engine stops redrawing what the package draws, and the only honest measure
 * of that is how much CSS is left: a file that grows back has taken something
 * from the package again, one line at a time, and no per-rule scan notices
 * that. The numbers are what the tree holds today, so a change that needs more
 * room has to say so here, in the same commit, where a reader can ask why.
 *
 * `components.css` is not on the list, deliberately: it held the button, the
 * badge, the table, the field, the banner, the chip, the meter, the list and
 * the empty state, and every one of those is the package's now. Neither is
 * `screens.css`, which held every screen's layout at once: one file per family
 * is what stops a rule for one screen reaching another that never asked for
 * it, and the names are who owns them.
 */
const SHEET_BUDGET: Record<string, number> = {
  "styles/base.css": 165,
  "styles/live.css": 160,
  "styles/records.css": 570,
  "styles/org.css": 610,
  "styles/configure.css": 75,
  "styles/integrations.css": 740,
};

test("no stylesheet is longer than its budget, and none has appeared", () => {
  const sheets = Object.fromEntries(
    files(/\.css$/).map(({ name, text }) => [name, text.split("\n").length]),
  );
  expect(Object.keys(sheets).sort()).toEqual(Object.keys(SHEET_BUDGET).sort());
  for (const [name, budget] of Object.entries(SHEET_BUDGET)) {
    expect([name, sheets[name]! <= budget]).toEqual([name, true]);
  }
});
