// @vitest-environment node
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, test } from "vitest";

/**
 * WHERE THE ACCENT IS SPENT, which is a rule this product states four times and
 * enforced nowhere.
 *
 * `docs/reference/dashboard-design.md`: "**Accent** | *where the reader is* —
 * the active nav row, the primary button, the focus ring, the on filter | 1",
 * and "Everything else … is **neutral**, and its identity is carried by its
 * name, its icon and its position." uilet's own tone contract says the same: "a
 * tone says what a thing IS, never who it is". `components/common.tsx` and
 * `routes/work/SavedViews.tsx` each restate it in a comment beside a call site.
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
 * SCOPE, stated rather than assumed: this reads the literal `"brand"` only.
 * `Tag` is the one uilet component taking `variant: TagVariant`, and
 * `StatusDot`'s `tone` would be caught by the same literal. Out of scope on
 * purpose: `Meter`'s and `MeterCell`'s `tone="accent"`, which is a BAR'S FILL
 * rather than a pill's ground and a different family — the accent list includes
 * the primary button for the same reason — and `uiletTone(toneOf(...))`, which
 * is dynamic and unreadable statically. A floor, not a proof.
 */

const SRC = fileURLToPath(new URL("..", import.meta.url));

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
    file: "routes/admin/Fleet.tsx",
    line: `{data?.this_node && <Tag variant="brand">you are on {data.this_node}</Tag>}`,
    why: "Which node is answering this reader. Literally where the reader is.",
  },
  {
    file: "routes/admin/Fleet.tsx",
    line: `{n.id === data?.this_node && <Tag variant="brand">this one</Tag>}`,
    why: "The same fact, as a row of the fleet table.",
  },
  {
    file: "routes/admin/Fleet.tsx",
    line: `{here && <Tag variant="brand">this one</Tag>}`,
    why: "The same fact again, in the node's own rail.",
  },
  {
    file: "routes/admin/Retention.tsx",
    line: `{n.node_id === thisNode && <Tag variant="brand">this one</Tag>}`,
    why: "The same fact, in the retention table's node column.",
  },
  {
    file: "app/Shell.tsx",
    line: `tone="brand"`,
    why: 'THE badge that IS the reader — the rail\'s own account row. `AvatarTone` in @crewlethq/ui reserves `brand` for exactly this: "the one badge that is the reader themselves". It is the strongest reading of the rule this file enforces, not an exception to it.',
  },
  {
    file: "routes/work/SavedViews.tsx",
    line: `<Tag key="pinned" variant="brand" title="pinned by you — pins are per reader">`,
    why: "A pin is PER READER; the view's own facts beside it are outline. The screen's own comment states the rule, and `viewMarks` is the ONE place that draws it — the grid and the facts block each had their own copy, already drifted.",
  },
];

/** Every source file under `src/`, comments blanked, as trimmed lines. */
function lines(): { file: string; at: number; line: string }[] {
  const walk = (dir: string): string[] =>
    readdirSync(dir, { withFileTypes: true }).flatMap((e) =>
      e.isDirectory()
        ? walk(join(dir, e.name))
        : /\.tsx?$/.test(e.name) && !e.name.includes(".test.")
          ? [join(dir, e.name)]
          : [],
    );
  return walk(SRC).flatMap((path) =>
    readFileSync(path, "utf8")
      // Blanked rather than removed, so a line number still points at the line.
      .replace(/\/\*[\s\S]*?\*\//g, (m) => m.replace(/[^\n]/g, " "))
      .replace(/\/\/[^\n]*/g, "")
      .split("\n")
      .map((line, i) => ({ file: path.slice(SRC.length), at: i + 1, line: line.trim() })),
  );
}

/** Every line spending the accent, wherever it is. */
export function accentSites(source: { file: string; at: number; line: string }[]) {
  return source.filter(({ line }) => /"brand"/.test(line));
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
  });
});
