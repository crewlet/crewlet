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
