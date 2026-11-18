/**
 * A viewed file is found by the digests Gitlet keeps of it, so the page's
 * digest must be the engine's byte for byte, and a file reads viewed only at
 * the state it was marked at.
 */

import { expect, test } from "vitest";
import type { CodeDiffFile } from "~/protocol/index.ts";
import { viewedDigests, viewedFiles, viewedKeyValid } from "./viewed.ts";

const KEY = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f";
const keyBytes = Uint8Array.from({ length: 32 }, (_, i) => i);
const OID = "3b18e512dba79e4c8300dd08aeb37f8e728b8dad";

const page = (extra: Partial<CodeDiffFile> = {}): CodeDiffFile => ({
  path: "internal/render/page.go",
  status: "M",
  added: 1,
  deleted: 1,
  new_oid: OID,
  new_mode: "100644",
  ...extra,
});

const markOf = (f: CodeDiffFile) => {
  const d = viewedDigests(keyBytes, f);
  return { path_digest: d.path, file_digest: d.file, at: "2026-11-18T09:00:00Z" };
};

// THE ENGINE'S OWN VECTOR, held by TestAViewedDigestIsTheOneTheDashboardComputes
// in internal/gitlet/viewed_test.go. Mutation: drop a NUL separator, and the file digest fails.
test("a file's digests are the ones Gitlet keeps", () => {
  expect(viewedDigests(keyBytes, page())).toEqual({
    path: "af0f83b2429025ab636e20b293f5f898d3b3c10ab93d594412234f460285ebae",
    file: "558c14cd8105c9319ef44064c0d064f71c7d4e7fb8d2f7a57304fe52cffbed4c",
  });
});

// A DELETION IS DIGESTED WITH AN EMPTY ID AND MODE, whatever the listing says.
// Mutation: digest its mode, and this fails.
test("a deletion digests an empty id and mode", () => {
  const gone = page({ path: "web/src/legacy.ts", status: "D", new_oid: "", new_mode: "000000" });
  expect(viewedDigests(keyBytes, gone).file).toBe(
    "24f4e4b25a25d96127370b75a3f9e2a89dc52cf0bb56c2febb2f500512635940",
  );
});

// BOTH DIGESTS: a mark at another state is no mark. Mutation: match on the path alone.
test("a file is viewed only where its path and its state both match a mark", () => {
  const shown = page();
  const marked = markOf(page({ new_oid: "1111111111111111111111111111111111111111" }));
  expect(viewedFiles(KEY, [markOf(shown)], [shown])).toEqual(new Set([shown.path]));
  expect(viewedFiles(KEY, [marked], [shown]).size).toBe(0);
  expect(viewedFiles(KEY, [markOf(page({ new_mode: "100755" }))], [shown]).size).toBe(0);
});

test("a path that is not text, a file with no state and a missing key are never viewed", () => {
  const unread = page({ path_b64: "/w==" });
  const stateless = page({ path: "a.go", new_mode: undefined });
  const rows = [markOf(unread), markOf(stateless), markOf(page())];
  expect(viewedFiles(KEY, rows, [unread, stateless]).size).toBe(0);
  expect(viewedFiles(undefined, rows, [page()]).size).toBe(0);
  expect(viewedFiles(KEY.toUpperCase(), rows, [page()]).size).toBe(0);
  expect(viewedKeyValid(KEY)).toBe(true);
  expect(viewedKeyValid(KEY.slice(2))).toBe(false);
});
