/** A change's files as a person's viewed marks name them: two HMAC-SHA256 digests a file under
 *  its repository's key, computed here as Gitlet keeps them (`gitlet.ViewedKey`). */

import { hmacSha256 } from "~/protocol/hmac.ts";
import type { CodeDiffFile, CodeViewedRow } from "~/protocol/index.ts";

/** A file as a mark names it: its path, and the state the revision gives it. */
export type ViewedState = Pick<CodeDiffFile, "path" | "status" | "new_oid" | "new_mode">;

/** Whether `key` is a viewed key as Gitlet writes one: 32 bytes in lowercase hex. */
export const viewedKeyValid = (key: string): boolean => /^[0-9a-f]{64}$/.test(key);

const utf8 = new TextEncoder();

const hex = (bytes: Uint8Array) =>
  Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");

/** The 64 hex digits of a key as its 32 bytes. */
const keyBytes = (key: string) => Uint8Array.from(key.match(/../g)!, (b) => parseInt(b, 16));

/** A file's viewed digests under `key`: its path, and its path, id and mode (both empty for a deletion). */
export function viewedDigests(key: Uint8Array, f: ViewedState): { path: string; file: string } {
  const deleted = f.status === "D";
  const oid = deleted ? "" : (f.new_oid ?? "");
  const mode = deleted ? "" : (f.new_mode ?? "");
  return {
    path: hex(hmacSha256(key, utf8.encode(f.path))),
    file: hex(hmacSha256(key, utf8.encode(`${f.path}\0${oid}\0${mode}`))),
  };
}

/** Whether a file names a state a mark can hold: a deletion, or an id and a mode. */
export const markable = (f: ViewedState): boolean =>
  f.status === "D" || (Boolean(f.new_oid) && Boolean(f.new_mode));

// DIGESTED ONCE A FILE ENTRY under the key held, so an arriving page digests only its own files.
let memo: { key: string; bytes: Uint8Array; at: WeakMap<object, { path: string; file: string }> } =
  { key: "", bytes: new Uint8Array(), at: new WeakMap() };

/** The paths of the listed files `rows` mark at the state shown; none without a valid key. */
export function viewedFiles(
  key: string | undefined,
  rows: readonly CodeViewedRow[],
  files: readonly (ViewedState & Pick<CodeDiffFile, "path_b64">)[],
): ReadonlySet<string> {
  const out = new Set<string>();
  if (!key || !viewedKeyValid(key) || rows.length === 0) return out;
  if (memo.key !== key) memo = { key, bytes: keyBytes(key), at: new WeakMap() };
  const marks = new Map(rows.map((r) => [r.path_digest, r.file_digest]));
  for (const f of files) {
    if (f.path_b64 || !markable(f)) continue;
    let digests = memo.at.get(f);
    if (!digests) {
      digests = viewedDigests(memo.bytes, f);
      memo.at.set(f, digests);
    }
    if (marks.get(digests.path) === digests.file) out.add(f.path);
  }
  return out;
}
