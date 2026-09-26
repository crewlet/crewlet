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

import { expect, test } from "vitest";

import { DESTINATIONS } from "~/app/nav.ts";
import { resolves } from "~/app/routes.ts";
import { modules } from "~/test/source.ts";
import { SCREENS, screenPath, type ScreenName } from "./dialogParts.tsx";

/** This lens's directory, relative to `src/`. */
const LENS = "routes/org/builder/";

/** This lens's own source, the suites excluded, by path relative to the lens. */
function sources(): { path: string; text: string }[] {
  return modules()
    .filter(({ path }) => path.startsWith(LENS))
    .map(({ path, text }) => ({ path: path.slice(LENS.length), text }));
}

const names = Object.keys(SCREENS) as ScreenName[];

/*
 * THE ROUTE TABLE'S OWN RESOLVER IS WHAT DISPATCH SWITCHES ON, and it is the
 * check `app/source.test.ts` makes over every other link in the tree — held
 * here against the same `resolves` so the two cannot come to disagree about
 * what a screen is.
 */
test("every destination resolves to a screen", () => {
  const dead = names.filter((name) => !resolves(screenPath(name)));
  expect(
    dead.map((name) => `${name} — #/${screenPath(name).join("/")}`),
    "these builder links go to a screen that does not exist",
  ).toEqual([]);
});

/*
 * AND THE WHOLE PATH, not just its head: `["settings", "integrations"]` and
 * `["settings", "integration"]` have the same first segment and only one of them
 * is a screen. `nav.ts` is where a destination is declared, so a builder
 * destination has to BE one — which is also what makes the sidebar and this lens
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
  const lens = sources();
  const text = lens
    .filter((f) => f.path !== "dialogParts.tsx")
    .map((f) => f.text)
    .join("\n");
  const table = lens.find((f) => f.path === "dialogParts.tsx")?.text ?? "";
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
  const declared = new Set(DESTINATIONS.map((d) => d.path.join("/")));
  expect(resolves(["integrations"])).toBe(false);
  expect(declared.has("integrations")).toBe(false);
  // And a path whose head is live but whose tail is not.
  expect(resolves(["settings"])).toBe(true);
  expect(resolves(["settings", "integration"])).toBe(false);
  expect(declared.has("settings/integration")).toBe(false);
  expect(declared.has("settings/integrations")).toBe(true);
  // And the address this lens's destinations lived at before this tree.
  expect(resolves(["admin", "integrations"])).toBe(false);
});
