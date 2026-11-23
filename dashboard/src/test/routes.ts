/**
 * A printed dashboard address, turned into the path the resolver is asked
 * about: its placeholders filled with a value of the shape each one names.
 *
 * TWO SUITES READ PRINTED ADDRESSES — the route table in the design doc
 * (`app/router.test.ts`) and every address the rest of the tree prints
 * (`app/links.test.ts`) — and they fill placeholders from this ONE table, so
 * `{handle}` cannot be a valid handle to one gate and a word the other has
 * never heard of. A placeholder with no row here fails the suite that met it
 * rather than being skipped: an address nobody can test is an address nobody
 * is testing.
 *
 * Not a suite itself, and never imported by anything that ships.
 */

import { readFileSync } from "node:fs";
import { join } from "node:path";

/** The uuid every id-shaped placeholder is filled with. */
const UUID = "5f0c5c8e-1a2b-4c3d-8e9f-0a1b2c3d4e5f";

/**
 * A placeholder, filled with a value of the shape it names. The `<…>` forms
 * are the ones prose writes for an id a person pastes (`<uuid>`, `<page id>`);
 * the `{…}` forms are the route table's.
 */
export const FILL: Record<string, string> = {
  "{KEY}": "ENG",
  "{KEY}-{n}": "ENG-42",
  "{CONTAINER}": "ENG",
  "{id}": UUID,
  "{page-id}": UUID,
  "{turn_id}": UUID,
  "<id>": UUID,
  "<uuid>": UUID,
  "<page id>": UUID,
  // AN INVITATION LINK IS ONE SEGMENT: the invitation's id and its secret,
  // joined by a dot (`#/invite/<id>.<secret>`).
  "{id}.{secret}": `${UUID}.secret`,
  "<id>.<secret>": `${UUID}.secret`,
  "{handle}": "pm",
  // A unit is addressed by its KEY, which is a word nobody renames.
  "{unit}": "platform",
  "{scope_type}": "role",
  "{scope_id}": "ceo",
  "{name}": "standup",
  "{kind}": "github",
  "{tool}": "search_work_items",
  "{node}": "node-1",
  "{domain}": "tracker",
  // A CHANGE'S NUMBER on its repository, from 1 (`#/gitlet/repos/{name}/changes/{n}`).
  "{n}": "42",
  // The same two as prose writes them, in the line git prints for a push's
  // change (`change <repo>!<n> opened: …#/gitlet/repos/<repo>/changes/<n>`).
  "<repo>": "web",
  "<n>": "42",
  // AN ADVISOR REPORT'S ISO WEEK (`#/advisor/reports/{week}`).
  "{week}": "2026-W47",
  // A COMMIT'S WHOLE ID (`#/gitlet/repos/{name}/commits/{sha}`).
  "{sha}": "0123456789abcdef0123456789abcdef01234567",
};

/**
 * The path a printed address names. A query (`?tab=…`) is not part of the
 * path. An address ending in `/` is a PREFIX the code completes with an id
 * (`doclet.AddressPrefix`, `#/doclet/pages/`), so it is asked about with one.
 *
 * Throws on a placeholder {@link FILL} does not carry, naming it.
 */
export function segmentsOf(address: string): string[] {
  const [path = ""] = address.replace(/^#\//, "").split("?");
  const completed = path.endsWith("/") ? `${path}{id}` : path;
  return completed
    .split("/")
    .filter((segment) => segment !== "")
    .map((segment) => {
      const filled = FILL[segment];
      if (filled !== undefined) return filled;
      if (/[{}<>]/.test(segment)) {
        throw new Error(`${address}: the placeholder “${segment}” has no value to test it with`);
      }
      return segment;
    });
}

/** From the dashboard, where the suite runs, as `app/links.test.ts` finds the repository. */
const DESIGN_DOC = join(process.cwd(), "..", "docs", "reference", "dashboard-design.md");
const ROUTES_HEADING = "### The routes";

/**
 * Every address the design doc's route table writes in its Route column, in
 * document order: the IA nothing compiles. Throws where the section or its
 * rows are missing, since a guard that cannot find its subject passes.
 */
export function documentedRoutes(): string[] {
  const lines = readFileSync(DESIGN_DOC, "utf8").split("\n");
  const start = lines.findIndex((line) => line.trim() === ROUTES_HEADING);
  if (start < 0) throw new Error(`“${ROUTES_HEADING}” is not in dashboard-design.md`);
  const rest = lines.slice(start + 1);
  const end = rest.findIndex((line) => /^#{2,3} /.test(line));
  if (end < 0) throw new Error(`no heading closes “${ROUTES_HEADING}”`);
  const out: string[] = [];
  for (const line of rest.slice(0, end)) {
    if (!line.startsWith("|")) continue;
    // THE FIRST CELL ONLY: the Page column's prose names addresses too,
    // including the ones it says are NOT routes.
    for (const [token] of (line.split("|")[1] ?? "").matchAll(/`#\/[^`]*`/g)) {
      out.push(token.slice(1, -1));
    }
  }
  // A FLOOR, so a table that lost its rows cannot pass by holding none.
  if (out.length <= 30) throw new Error("the route table holds too few routes to be the table");
  return out;
}
