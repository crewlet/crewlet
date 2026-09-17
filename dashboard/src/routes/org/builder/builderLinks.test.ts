// @vitest-environment node
/**
 * Every screen the builder links out to exists, and every name it offers is
 * used.
 *
 * WHY THIS SUITE EXISTS AT ALL. The lens links out to five screens it does
 * not own — the configuration, the fleet, Integrations, Schedules and the
 * company's credentials — and a link to a screen that moved is the one
 * navigation failure with NO symptom before the click: it renders, it has the
 * right words on it, `tsc` is happy, and it lands on "there is no such
 * screen". The builder arrived here from a tree whose routes were flat, so
 * all five of its destinations were wrong at once and nothing in the build
 * said a word.
 *
 * `app/source.test.ts` catches the shapes it can read — `href([…])`,
 * `nav.to([…])`, `path: […]` — and it could not read this lens's, because a
 * destination went through `ScreenLink to={[…]}`, a fourth spelling. The
 * answer is not a fourth pattern in that gate: it is that the path stops
 * being written at the call site at all. [SCREENS] is now the one place a
 * builder destination is a route, `ScreenName` makes a call site's mistake a
 * type error, and this file is what holds the table itself against the
 * application's own destinations.
 *
 * TWO-SIDED, because each direction fails silently on its own. An entry
 * naming a screen `nav.ts` does not have is a dead link; a name nothing
 * links to is an entry that outlived its caller, which is how a table comes
 * to carry a route nobody has checked since it moved.
 */

import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative } from "node:path";
import { fileURLToPath } from "node:url";
import { expect, test } from "vitest";

import { DESTINATIONS, RAIL } from "~/app/nav.ts";
import { SCREENS, screenPath, type ScreenName } from "./dialogParts.tsx";

const LENS = fileURLToPath(new URL(".", import.meta.url));

/** This lens's own source, the suites excluded. */
function sources(): { path: string; text: string }[] {
  const out: { path: string; text: string }[] = [];
  (function walk(dir: string): void {
    for (const entry of readdirSync(dir)) {
      const full = join(dir, entry);
      if (statSync(full).isDirectory()) {
        walk(full);
        continue;
      }
      if (!/\.tsx?$/.test(full) || full.includes(".test.")) continue;
      out.push({ path: relative(LENS, full), text: readFileSync(full, "utf8") });
    }
  })(LENS);
  return out;
}

const names = Object.keys(SCREENS) as ScreenName[];

/*
 * THE FIRST SEGMENT IS WHAT ROUTE DISPATCH SWITCHES ON, which is the check
 * `app/source.test.ts` makes over every other link in the tree — held here
 * against the same `RAIL` so the two cannot come to disagree about what a
 * live workspace is.
 */
test("every destination names a segment a workspace owns", () => {
  const owned = new Set(RAIL.flatMap((r) => r.owns));
  const dead = names.filter((name) => !owned.has(screenPath(name)[0] ?? ""));
  expect(dead, "these builder links go to a workspace that does not exist").toEqual([]);
});

/*
 * AND THE WHOLE PATH, not just its head: `["admin", "integrations"]` and
 * `["admin", "integration"]` have the same first segment and only one of them
 * is a screen. `nav.ts` is where a destination is declared, so a builder
 * destination has to BE one — which is also what makes the rail and this lens
 * agree about where Integrations is when it next moves.
 */
test("every destination is a destination this application declares", () => {
  const declared = new Set(DESTINATIONS.map((d) => d.path.join("/")));
  const unknown = names.filter((name) => !declared.has(screenPath(name).join("/")));
  expect(
    unknown.map((name) => `${name} — #/${screenPath(name).join("/")}`),
    "these are not in app/nav.ts's destinations: the screen moved, or never existed",
  ).toEqual([]);
});

/*
 * THE OTHER DIRECTION. An entry nothing links to is a route nobody has
 * clicked and therefore nobody has checked — exactly the state all five of
 * these were in when the lens arrived.
 */
test("every destination is linked to by something in the lens", () => {
  const text = sources()
    .filter((f) => f.path !== "dialogParts.tsx")
    .map((f) => f.text)
    .join("\n");
  const table = readFileSync(join(LENS, "dialogParts.tsx"), "utf8");
  // Named at a `ScreenLink`, at a `ReadOnlyFact`'s link, or through
  // `screenPath` where the caller builds the href itself.
  const used = (name: string) =>
    new RegExp(`(?:to=|to:\\s*|screenPath\\()\\s*"${name}"`).test(text) ||
    new RegExp(`(?:to=|to:\\s*|screenPath\\()\\s*"${name}"`).test(
      // dialogParts links to Integrations and Schedules from its own parts.
      table.slice(table.indexOf("export function ScreenLink")),
    );
  expect(
    names.filter((name) => !used(name)),
    "these are declared and nothing links to them — delete the entry, or find the caller that lost its link",
  ).toEqual([]);
});

/*
 * THE MUTATION, run against the same values the cases above read rather than
 * a re-spelling of them: a table that named a flat route — which is what this
 * lens shipped with — has to come back red from both directions.
 */
test("the check can tell: a flat route is caught, head and whole", () => {
  const owned = new Set(RAIL.flatMap((r) => r.owns));
  const declared = new Set(DESTINATIONS.map((d) => d.path.join("/")));
  expect(owned.has("integrations")).toBe(false);
  expect(declared.has("integrations")).toBe(false);
  // And a path whose head is live but whose tail is not.
  expect(owned.has("admin")).toBe(true);
  expect(declared.has("admin/integration")).toBe(false);
  expect(declared.has("admin/integrations")).toBe(true);
});
