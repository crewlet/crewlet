/**
 * What one segment of a URL path may be: the ONE rule, for the requests this
 * dashboard sends and the links it draws.
 *
 * `encodeURIComponent` escapes `/`, `?` and `#`, which would otherwise reach
 * another route or start the query. It cannot make `.` or `..` a name: a
 * segment of dots alone is a step along the path, and the URL standard reads
 * `%2E%2E` as `..` too, so no escaping sends it as one segment. Such a value
 * names no segment at all, and is refused here:
 *
 *  - [linkSegment] answers null for it, so a link built from text somebody
 *    wrote (an agent's comment, a page title) is drawn as text rather than as
 *    an address that goes to the route above;
 *  - [pathSegment] throws for it, so a request is refused before anything is
 *    sent rather than sent to the route above. It is [linkSegment] and a
 *    throw, so the two can never disagree on what is safe.
 *
 * PURE AND IMPORTING NOTHING, so the transport (`protocol/rest.ts`, which
 * re-exports [pathSegment]), the org builder's pure model and the artifact
 * runtime (built alone) all read this one copy. The only module that calls
 * `encodeURIComponent` itself; a gate holds every other to it
 * (`protocol/rest.test.ts`).
 *
 * NOT FOR THE DASHBOARD'S OWN ROUTES: a fragment is never resolved as a path,
 * so `#/worklet/..` is the route for an item named `..`, and the router
 * escapes its segments with [fragmentSegment].
 */

/** One path segment of a link, or null for a value that cannot be one (empty, or dots alone). */
export function linkSegment(value: string): string | null {
  if (/^\.*$/.test(value)) return null;
  return encodeURIComponent(value);
}

/**
 * One path segment of a request, from a value the engine named (a model's
 * key, a seat's handle): always exactly one segment, or no request at all.
 * No name the engine mints is empty or dots alone, so such a value is a
 * defect upstream, and refused before anything is sent.
 */
export function pathSegment(value: string): string {
  const segment = linkSegment(value);
  if (segment === null) throw new RangeError(`"${value}" cannot be sent as one path segment`);
  return segment;
}

/**
 * A slash-separated path (a file's path inside an artifact or a project) as
 * that many segments of a request, each one [pathSegment]: a `/` in it stays
 * a separator, and an empty or dots-only part is refused like any segment.
 */
export function pathSegments(path: string): string {
  return path.split("/").map(pathSegment).join("/");
}

/**
 * One segment of the dashboard's own route, in the URL's fragment: escaped,
 * so a `/`, `?` or `#` in it stays in it, and otherwise taken as it is. A
 * fragment is not resolved, so dots are a name like any other.
 */
export function fragmentSegment(value: string): string {
  return encodeURIComponent(value);
}
