/**
 * JSON values: compared by meaning, copied, and edited without mutation.
 *
 * THE DRAFT IS THE ENGINE'S DOCUMENT, NOT A TYPED COPY OF IT. Every entity in
 * the builder carries its authored JSON verbatim, including keys this build
 * has never heard of, so the helpers here work on plain JSON rather than on
 * `ConfigRole` fields. A typed copy would silently drop a key a newer engine
 * wrote, and the save would delete it.
 *
 * ABSENT AND `undefined` ARE THE SAME THING, deliberately. An operation records
 * "this field was not set" as an omitted `before`, and a JSON round trip (the
 * persisted log) drops an `undefined` property on the way out. Treating the two
 * spellings as one value is what lets a log replay identically after
 * `JSON.parse(JSON.stringify(log))`. `null` stays a value of its own: the
 * engine writes `enabled: null` for a toggle that is explicitly unset.
 *
 * Every edit returns a new object and shares every branch it did not touch, so
 * a draft replayed from its base keeps the base's untouched subtrees by
 * reference and an undo costs a replay rather than a deep copy per step.
 */

/** A JSON value as `JSON.parse` produces it. */
export type Json = null | boolean | number | string | Json[] | { [key: string]: Json };

/** A plain JSON object, open to any key. */
export type JsonRecord = Record<string, unknown>;

/** Whether a value is a plain object (not an array, not null). */
export function isRecord(value: unknown): value is JsonRecord {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

/**
 * Structural equality with JSON semantics: key order does not matter, and a
 * property holding `undefined` equals a missing one.
 */
export function jsonEqual(a: unknown, b: unknown): boolean {
  if (a === b) return true;
  if (a === undefined || b === undefined) return false;
  if (Array.isArray(a)) {
    if (!Array.isArray(b) || a.length !== b.length) return false;
    for (let i = 0; i < a.length; i++) if (!jsonEqual(a[i], b[i])) return false;
    return true;
  }
  if (isRecord(a)) {
    if (!isRecord(b)) return false;
    const keys = new Set([...Object.keys(a), ...Object.keys(b)]);
    for (const key of keys) if (!jsonEqual(a[key], b[key])) return false;
    return true;
  }
  return false;
}

/**
 * A deep copy of a JSON value with every `undefined` property removed.
 *
 * Used where a value crosses from a caller into the model (a fetched document)
 * or from the model into an operation payload, so nothing the model holds can
 * be mutated behind its back by the object it was built from.
 */
export function cloneJson<T>(value: T): T {
  if (Array.isArray(value)) return value.map((v) => cloneJson(v)) as T;
  if (isRecord(value)) {
    const out: JsonRecord = {};
    for (const [key, v] of Object.entries(value)) {
      if (v !== undefined) out[key] = cloneJson(v);
    }
    return out as T;
  }
  return value;
}

/** The value at a key path inside nested objects, or `undefined` when any step is absent. */
export function getPath(value: unknown, path: readonly string[]): unknown {
  let at: unknown = value;
  for (const key of path) {
    if (!isRecord(at)) return undefined;
    at = at[key];
  }
  return at;
}

/**
 * A copy of `record` with the value at `path` replaced, or removed when `value`
 * is `undefined`.
 *
 * Setting creates the objects on the way. Removing PRUNES the objects the
 * removal emptied: clearing the only key of `integrations.jira` leaves no
 * `integrations: { jira: {} }` behind, because an empty block is not "the
 * same document minus one field" to the engine (an empty Jira block still
 * decodes as a block) and it would show up as an edit nobody made.
 */
export function setPath(record: JsonRecord, path: readonly string[], value: unknown): JsonRecord {
  if (path.length === 0) throw new RangeError("setPath: the path is empty");
  const [head, ...rest] = path as [string, ...string[]];
  if (rest.length === 0) {
    if (value === undefined) {
      if (!(head in record)) return record;
      const out = { ...record };
      delete out[head];
      return out;
    }
    return { ...record, [head]: value };
  }
  const child = record[head];
  if (!isRecord(child)) {
    if (value === undefined) return record;
    return { ...record, [head]: setPath({}, rest, value) };
  }
  const next = setPath(child, rest, value);
  if (next === child) return record;
  if (value === undefined && Object.keys(next).length === 0) {
    const out = { ...record };
    delete out[head];
    return out;
  }
  return { ...record, [head]: next };
}
