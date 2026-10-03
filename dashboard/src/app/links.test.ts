/**
 * EVERY DASHBOARD ADDRESS THE REST OF THE TREE PRINTS RESOLVES.
 *
 * A dashboard address outlives the screen it was written for in places no
 * compiler reads: a doc page telling an operator where to click, a tool skill
 * teaching a seat how to link a page, a company example's comment, a string
 * the engine composes into a chat message. The one-sidebar rebuild moved every
 * workspace once, and the addresses printed against the old heads —
 * `#/pages/{page-id}` in a tool skill and in the Nimbus example among them —
 * kept rendering as live links to a Not Found. `source.test.ts` holds the
 * dashboard's OWN links; this suite holds everybody else's, against the same
 * resolver the router uses, with placeholders filled from the one table
 * `router.test.ts` fills the design doc's route table from.
 *
 * The second half holds every anchor into `dashboard-design.md` — in the docs, the
 * engine's comments and the dashboard's own — to a heading the design doc
 * actually carries, by GitHub's slug. The design doc is where a rule's
 * reasoning lives and code points at it by anchor, so a renamed heading
 * orphans every pointer at once and nothing else would notice:
 * `internal/docsgate` reads markdown, and these pointers are mostly in code.
 */

// @vitest-environment node

import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative } from "node:path";
import { expect, test } from "vitest";
import { resolve } from "./routes.ts";
import { segmentsOf } from "~/test/routes.ts";

const REPO = join(process.cwd(), "..");

/** Every file under `dir` (relative to the repository) whose name passes `keep`. */
function files(dir: string, keep: (name: string) => boolean): string[] {
  const out: string[] = [];
  (function walk(abs: string): void {
    for (const entry of readdirSync(abs)) {
      if (entry === "node_modules" || entry === ".git") continue;
      const full = join(abs, entry);
      if (statSync(full).isDirectory()) walk(full);
      else if (keep(entry)) out.push(full);
    }
  })(join(REPO, dir));
  return out;
}

const rel = (abs: string): string => relative(REPO, abs).split("\\").join("/");

/**
 * A printed address: `#/`, then segments — each a run of address characters,
 * a `{placeholder}` or a `<placeholder>` (which may hold a space: `<page id>`).
 * It stops at whitespace, a quote, a bracket, a table bar or a comma, and a
 * trailing sentence stop is not part of it.
 */
const ADDRESS = /#\/(?:\{[^}\n]*\}|<[^>\n]*>|[^\s`'"()<>|,*[\]{}])*/g;

function addressesIn(text: string): { address: string; line: number }[] {
  const out: { address: string; line: number }[] = [];
  text.split("\n").forEach((line, i) => {
    for (const [hit] of line.matchAll(ADDRESS)) {
      out.push({ address: hit.replace(/[.:;]+$/, ""), line: i + 1 });
    }
  });
  return out;
}

/**
 * The string literals of one Go source file, with the line each starts on —
 * interpreted, raw and rune literals, skipping comments, which a scanner that
 * only matched quotes would read as code.
 */
function goStrings(src: string): { text: string; line: number }[] {
  const out: { text: string; line: number }[] = [];
  let line = 1;
  for (let i = 0; i < src.length; i++) {
    const c = src[i]!;
    if (c === "\n") line++;
    else if (c === "/" && src[i + 1] === "/") {
      while (i < src.length && src[i] !== "\n") i++;
      line++;
    } else if (c === "/" && src[i + 1] === "*") {
      const end = src.indexOf("*/", i + 2);
      const stop = end < 0 ? src.length : end + 2;
      line += (src.slice(i, stop).match(/\n/g) ?? []).length;
      i = stop - 1;
    } else if (c === '"' || c === "'" || c === "`") {
      const start = line;
      let j = i + 1;
      let text = "";
      while (j < src.length && src[j] !== c) {
        if (c !== "`" && src[j] === "\\") {
          text += src[j + 1] ?? "";
          j += 2;
          continue;
        }
        if (src[j] === "\n") line++;
        text += src[j];
        j++;
      }
      out.push({ text, line: start });
      i = j;
    }
  }
  return out;
}

test("the Go string scanner reads strings and nothing else", () => {
  const src = [
    '// a comment naming "#/nowhere"',
    'const A = "#/work" // and "#/nowhere" again',
    '/* a block with "#/nowhere"',
    "   across lines */",
    "var b = `#/live",
    "turns`",
    'var c = "an \\"escaped\\" #/inbox"',
    "var r = '\"'",
  ].join("\n");
  const found = goStrings(src).map((s) => `${s.line}:${s.text}`);
  expect(found).toEqual(["2:#/work", "5:#/live\nturns", '7:an "escaped" #/inbox', '8:"']);
});

/** Every printed address in the tree outside the dashboard's own source. */
function printed(): { where: string; address: string }[] {
  const out: { where: string; address: string }[] = [];
  const prose = [
    ...files("docs", (n) => n.endsWith(".md")),
    ...files("examples", (n) => /\.(md|ya?ml)$/.test(n)),
    ...files("skills", () => true),
    join(REPO, "README.md"),
    join(REPO, "CONTRIBUTING.md"),
  ];
  for (const path of prose) {
    for (const { address, line } of addressesIn(readFileSync(path, "utf8"))) {
      // `#/$defs/…` is a JSON Schema reference, not a dashboard address:
      // every route segment is a word, and `$` starts none of them.
      if (address.startsWith("#/$")) continue;
      out.push({ where: `${rel(path)}:${line}`, address });
    }
  }
  const code = [
    ...files("internal", (n) => n.endsWith(".go") && !n.endsWith("_test.go")),
    ...files("cmd", (n) => n.endsWith(".go") && !n.endsWith("_test.go")),
  ];
  for (const path of code) {
    for (const s of goStrings(readFileSync(path, "utf8"))) {
      for (const { address, line } of addressesIn(s.text)) {
        if (address.startsWith("#/$")) continue;
        out.push({ where: `${rel(path)}:${s.line + line - 1}`, address });
      }
    }
  }
  return out;
}

test("every dashboard address printed outside the dashboard resolves to a screen", () => {
  const all = printed();
  // A FLOOR: the route table alone prints more than this, so a walk that found
  // nothing — a moved docs directory, a broken pattern — cannot pass.
  expect(all.length, "found too few printed addresses to be the tree").toBeGreaterThan(100);
  // And the code half is not vacuous: the engine composes the page address.
  expect(
    all.some((a) => a.where.endsWith(".go") || a.where.includes(".go:")),
    "found no address in a Go string — the code half is asserting nothing",
  ).toBe(true);

  const dead: string[] = [];
  for (const { where, address } of all) {
    let path: string[];
    try {
      path = segmentsOf(address);
    } catch (err) {
      dead.push(`${where} — ${(err as Error).message}`);
      continue;
    }
    if (!resolve(path).resolved) dead.push(`${where} — ${address}`);
  }
  expect(dead, "these printed addresses open Not Found").toEqual([]);
});

// ---------------------------------------------------------------------------

const DESIGN_DOC = join(REPO, "docs", "reference", "dashboard-design.md");

/**
 * GitHub's anchor for a heading — the same rule `internal/docsgate` pins: the
 * rendered text lower-cased, keeping letters, marks, digits, connectors,
 * spaces and hyphens, each space a hyphen.
 */
function slug(heading: string): string {
  return heading
    .replace(/!?\[([^\]]*)\]\([^)]*\)/g, "$1")
    .replace(/<[^>]+>/g, "")
    .toLowerCase()
    .replace(/[^\p{L}\p{M}\p{N}\p{Pc} -]/gu, "")
    .replace(/ /g, "-");
}

test("the slug is GitHub's", () => {
  expect(slug("`window=` — one vocabulary for every time range")).toBe(
    "window--one-vocabulary-for-every-time-range",
  );
  expect(slug("⌘K: search, answer, act")).toBe("k-search-answer-act");
  expect(slug("Spend › Budgets, raised in place")).toBe("spend--budgets-raised-in-place");
  expect(slug("The `budget_meters` push")).toBe("the-budget_meters-push");
});

/** Every anchor the design doc renders, numbered on repetition, outside fences. */
function designAnchors(): Set<string> {
  const out = new Set<string>();
  const seen = new Map<string, number>();
  let fenced = false;
  for (const line of readFileSync(DESIGN_DOC, "utf8").split("\n")) {
    if (/^\s{0,3}(```|~~~)/.test(line)) {
      fenced = !fenced;
      continue;
    }
    const m = fenced ? null : /^\s{0,3}#{1,6}\s+(.*?)\s*#*\s*$/.exec(line);
    if (!m) continue;
    const s = slug(m[1]!);
    const n = seen.get(s) ?? 0;
    out.add(n > 0 ? `${s}-${n}` : s);
    seen.set(s, n + 1);
  }
  return out;
}

test("every pointer at a design-doc heading names one it carries", () => {
  const anchors = designAnchors();
  expect(anchors.size, "the design doc renders too few headings to be the doc").toBeGreaterThan(50);
  const sources = [
    ...files("docs", (n) => n.endsWith(".md")),
    ...files("adr", (n) => n.endsWith(".md")),
    ...files("internal", (n) => n.endsWith(".go")),
    ...files("cmd", (n) => n.endsWith(".go")),
    ...files("dashboard/src", (n) => /\.(tsx?|css)$/.test(n)),
    join(REPO, "README.md"),
    join(REPO, "CONTRIBUTING.md"),
    join(REPO, "CLAUDE.md"),
  ];
  let pointers = 0;
  const dead: string[] = [];
  for (const path of sources) {
    readFileSync(path, "utf8")
      .split("\n")
      .forEach((line, i) => {
        for (const m of line.matchAll(/dashboard-design\.md#([\p{L}\p{N}_-]+)/gu)) {
          pointers++;
          if (!anchors.has(m[1]!)) dead.push(`${rel(path)}:${i + 1} — #${m[1]}`);
        }
      });
  }
  expect(pointers, "found too few pointers at the design doc to be the tree").toBeGreaterThan(5);
  expect(dead, "these pointers name a heading dashboard-design.md does not carry").toEqual([]);
});
