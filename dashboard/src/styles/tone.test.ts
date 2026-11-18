// @vitest-environment node
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { describe, expect, test } from "vitest";

import { sheetRules, type SheetRule } from "../test/sheets.ts";
import { modules } from "../test/source.ts";

/**
 * WHERE THE ACCENT IS SPENT, which is a rule this product states four times and
 * enforced nowhere.
 *
 * `docs/reference/dashboard-design.md`'s "The one rule": the accent is what to
 * do, and everything else is neutral, carried by its name, its icon and its
 * position. uilet's own tone contract says the same: "a
 * tone says what a thing IS, never who it is". `components/common.tsx` and
 * `routes/worklet/SavedViews.tsx` each restate it in a comment beside a call site.
 *
 * It decayed anyway. The work item's Woke panel drew its "N asked" summary chip
 * and one recipient's reason pill in `variant="brand"`, which resolves to
 * `--color-brand-accent-soft` under `--color-brand-accent-ink` — the pair
 * `.crewlet-filter-chip[aria-pressed='true']` takes, byte for byte. So a panel
 * whose other half IS a selection list carried two pills wearing the ground of a
 * switched-on filter, over a fact (`addressed`) that is about the NOTICE and
 * never about the reader.
 *
 * KEYED ON THE TRIMMED SOURCE LINE rather than a line number: a line that moves
 * still matches, and a line that is EDITED stops matching, which is exactly when
 * the site should be re-justified. Two-sided, so an entry that stopped firing
 * fails as loudly as a site that is not excused.
 *
 * SCOPE, stated rather than assumed: this reads the literal `"brand"` where a
 * TONE is named (a `variant`, `ring`, `tone` or `hue` prop or key, and the
 * translation's `accent` key), since `"brand"` is also an artifact kind, the
 * Company row of the company's brand and its named read, none of which is a
 * tone. The two places the kit still spends it are the ones excused here: a
 * `Tag` of `variant="brand"`, and `Avatar`'s `ring="brand"`, the accent drawn
 * round a badge to say "this one is you". `StatusDot`'s `tone` would be
 * caught by the same literal. Out of scope on
 * purpose: `Meter`'s `tone="accent"`, which is a BAR'S FILL
 * rather than a pill's ground and a different family — the accent list includes
 * the primary button for the same reason — and `uiletTone(toneOf(...))`, which
 * is dynamic and unreadable statically. A floor, not a proof.
 *
 * AND THE ACCENT UNDER ANOTHER NAME: a sheet rule reading one of the code host
 * family's aliases of the accent, found in the installed tokens, is listed
 * below with its reason.
 */

interface Site {
  file: string;
  line: string;
  why: string;
}

const ACCENT: Site[] = [
  {
    file: "ui/primitives.tsx",
    line: `accent: "brand",`,
    why: "THE TRANSLATION, not a call site: our `accent` said in uilet's spelling. The one place the two vocabularies meet.",
  },
  {
    file: "routes/settings/Fleet.tsx",
    line: `{n.id === data?.this_node && <Tag variant="brand">this one</Tag>}`,
    why: "The same fact, as a row of the fleet table.",
  },
  {
    file: "routes/settings/Fleet.tsx",
    line: `{here && <Tag variant="brand">this one</Tag>}`,
    why: "The same fact again, in the node's own rail.",
  },
  {
    file: "routes/settings/Retention.tsx",
    line: `{n.node_id === thisNode && <Tag variant="brand">this one</Tag>}`,
    why: "The same fact, in the retention table's node column.",
  },
  {
    file: "app/sidebar/UserBlock.tsx",
    line: `ring={resolved ? "brand" : undefined}`,
    why: "The badge in the sidebar's account row IS the reader — `ring` is the kit's selected badge, and this is the one badge that means \"you\". Nobody, and an answer still out, are not ringed.",
  },
  {
    file: "routes/gitlet/ChangeRow.tsx",
    line: `ring={f.handle === me ? "brand" : undefined}`,
    why: "The reader's own face in a change's row: the kit's selected badge, the one face there that means \"you\".",
  },
  {
    file: "routes/gitlet/change/Merge.tsx",
    line: `<Face key={h} who={h} ring={h === viewer.handle ? "brand" : undefined} />`,
    why: "The reader's own face among a merge box's reviewers: the one face there that means \"you\".",
  },
  {
    file: "routes/gitlet/change/Rail.tsx",
    line: `<Face who={who} ring={who === viewer.handle ? "brand" : undefined} />`,
    why: "The reader's own face among a change rail's reviewers.",
  },
  {
    file: "routes/gitlet/change/Rail.tsx",
    line: `ring={p.handle === viewer.handle ? "brand" : undefined}`,
    why: "The reader's own face among a change rail's participants.",
  },
  {
    file: "routes/worklet/SavedViews.tsx",
    line: `<Tag key="pinned" variant="brand" title="Pinned by you">`,
    why: "A pin is PER READER; the view's own facts beside it are outline. The screen's own comment states the rule, and `viewMarks` is the ONE place that draws it — the grid and the facts block each had their own copy, already drifted.",
  },
];

/** Every source file under `src/`, comments blanked, as trimmed lines. */
function lines(): { file: string; at: number; line: string }[] {
  return modules().flatMap(({ path, text }) =>
    text
      // Blanked rather than removed, so a line number still points at the line.
      .replace(/\/\*[\s\S]*?\*\//g, (m) => m.replace(/[^\n]/g, " "))
      .replace(/\/\/[^\n]*/g, "")
      .split("\n")
      .map((line, i) => ({ file: path, at: i + 1, line: line.trim() })),
  );
}

/**
 * A tone named `"brand"`: a prop (`variant="brand"`, `ring={you ? "brand" :
 * undefined}`) or a key (`variant: "brand"`, the translation's `accent:
 * "brand"`).
 */
const ACCENT_TONE =
  /\b(?:variant|ring|tone|hue)=\{?(?:[^{}"]*\?\s*)?"brand"|\b(?:variant|ring|tone|hue|accent):\s*"brand"/;

/** Every line spending the accent, wherever it is. */
export function accentSites(source: { file: string; at: number; line: string }[]) {
  return source.filter(({ line }) => ACCENT_TONE.test(line));
}

describe("where the accent is spent", () => {
  test("every accent-toned tag says where the reader is", () => {
    const loose = accentSites(lines()).filter(
      ({ file, line }) => !ACCENT.some((s) => s.file === file && s.line === line),
    );
    expect(
      loose.map(({ file, at, line }) => `${file}:${at} — ${line}`),
      "the accent means WHERE THE READER IS; every other fact is neutral and carried by its word",
    ).toEqual([]);
  });

  // THE OTHER SIDE, which is what stops the list outliving what it excuses.
  test("every entry still has a site to excuse", () => {
    const source = lines();
    const stale = ACCENT.filter(
      ({ file, line }) => !source.some((l) => l.file === file && l.line === line),
    );
    expect(
      stale.map((s) => `${s.file} — ${s.line}`),
      "this site is gone or was edited; re-justify it or drop the entry",
    ).toEqual([]);
  });

  // AND A VACUITY FLOOR PLUS A MUTATION GUARD. A collector that silently matched
  // nothing would pass exactly like a clean tree.
  test("the collector finds the sites that are there", () => {
    expect(accentSites(lines()).length).toBeGreaterThanOrEqual(ACCENT.length);
    expect(
      accentSites([{ file: "/x.tsx", at: 1, line: `<Tag variant="brand">whatever</Tag>` }]),
    ).toHaveLength(1);
    // EVERY SPELLING OF A TONE IS CAUGHT, and the kind, the Company row and
    // the named read that share the word are not.
    const spelled = [
      `<Avatar ring={you ? "brand" : undefined} />`,
      `toast.show({ variant: "brand", title })`,
      `<StatusDot tone="brand" />`,
      `accent: "brand",`,
    ];
    const named = [
      `brand: { render: "native", store: "none" },`,
      `case "brand":`,
      `useQuery("brand", {})`,
      `kind: "brand",`,
    ];
    const at = (line: string) => ({ file: "/x.tsx", at: 1, line });
    expect(accentSites(spelled.map(at))).toHaveLength(spelled.length);
    expect(accentSites(named.map(at))).toEqual([]);
  });
});

interface SheetSite {
  file: string;
  selector: string;
  why: string;
}

const ACCENT_RULES: SheetSite[] = [
  {
    file: "gitlet.css",
    selector: '.code-state[data-state="merged"]',
    why: "A merged change on the kit's merged pill, the accent under its label, beside its glyph and word: a kind of change, never an action.",
  },
  {
    file: "gitlet.css",
    selector: '.code-state-glyph[data-state="merged"]',
    why: "A merged change's glyph leading its row, in the kit's merged ink beside the state's word: a kind of change, never an action.",
  },
  {
    file: "gitlet.css",
    selector: ".code-branch-chip",
    why: "A branch on the kit's code-ref chip, the accent's soft step and ink, beside its glyph: a kind in Gitlet's code, never an action.",
  },
  {
    file: "gitlet.css",
    selector: ".code-gap",
    why: "A diff's gap row on the diff family's hunk row, as the kit's DiffView draws its skipped lines: a place in a diff, never an action.",
  },
  {
    file: "gitlet.css",
    selector: ".code-search-gap",
    why: "A search hit's gap row on the diff family's hunk row, as a diff's gap: lines of a file left out, never an action.",
  },
  {
    file: "gitlet-code.css",
    selector: ".code-syn-keyword",
    why: "A keyword in code read in Gitlet, in the accent's ink beside its own word: a kind of word, never an action.",
  },
];

/** The code host family's names, which alias other steps. */
const CODE_FAMILY =
  /^--color-(?:diff|syntax|change-(?:open|merged|closed|draft)|code|file)-[\w-]+$/;

/**
 * The code host family's names whose alias chain in `tokensCss` reaches the
 * accent (`--color-brand-accent…`).
 */
export function accentAliases(tokensCss: string): Set<string> {
  const alias = new Map<string, string>();
  for (const m of tokensCss.matchAll(/(--[\w-]+)\s*:\s*var\((--[\w-]+)\)\s*[;}]/g)) {
    alias.set(m[1]!, m[2]!);
  }
  const out = new Set<string>();
  for (const name of alias.keys()) {
    if (!CODE_FAMILY.test(name)) continue;
    let at: string | undefined = name;
    for (let hops = 0; at && hops < 16; hops++) {
      if (at.startsWith("--color-brand-accent")) {
        out.add(name);
        break;
      }
      at = alias.get(at);
    }
  }
  return out;
}

/** The sheet rules reading one of `aliases`. */
export function accentRules(rules: readonly SheetRule[], aliases: ReadonlySet<string>) {
  return rules
    .filter((rule) => [...rule.body.matchAll(/var\((--[\w-]+)/g)].some((m) => aliases.has(m[1]!)))
    .map(({ file, selector }) => ({ file, selector }));
}

const tokensCss = () =>
  readFileSync(createRequire(import.meta.url).resolve("@crewlethq/tokens/css"), "utf8");

describe("the accent under the code host family's names", () => {
  test("every rule spending the accent through the code host family says why", () => {
    const loose = accentRules(sheetRules(), accentAliases(tokensCss())).filter(
      (r) => !ACCENT_RULES.some((s) => s.file === r.file && s.selector === r.selector),
    );
    expect(loose.map((r) => `${r.file} ${r.selector}`)).toEqual([]);
  });

  test("every such entry still has a rule to excuse", () => {
    const found = accentRules(sheetRules(), accentAliases(tokensCss()));
    const stale = ACCENT_RULES.filter(
      (s) => !found.some((r) => r.file === s.file && r.selector === s.selector),
    );
    expect(stale.map((s) => `${s.file} ${s.selector}`)).toEqual([]);
    for (const { selector, why } of ACCENT_RULES) expect(why.length, selector).toBeGreaterThan(40);
  });

  test("the aliases come from the installed tokens, and the collector finds them", () => {
    const aliases = accentAliases(tokensCss());
    for (const name of [
      "--color-diff-hunk-row",
      "--color-syntax-keyword",
      "--color-change-merged-fill",
      "--color-change-merged-ink",
      "--color-code-ref-fill",
      "--color-code-ref-ink",
    ]) {
      expect(aliases, name).toContain(name);
    }
    const rule = (body: string): SheetRule => ({ file: "x.css", selector: ".x", body });
    expect(accentRules([rule("color: var(--color-code-ref-ink);")], aliases)).toHaveLength(1);
    expect(accentRules([rule("background: var(--color-code-match);")], aliases)).toEqual([]);
  });
});
