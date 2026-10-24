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
};

/**
 * The path a printed address names. A query (`?tab=…`) is not part of the
 * path. An address ending in `/` is a PREFIX the code completes with an id
 * (`pages.AddressPrefix`, `#/knowledge/pages/`), so it is asked about with one.
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
