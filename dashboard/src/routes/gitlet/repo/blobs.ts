/** A file's whole text read at a commit, every page joined, for the editor and Copy. */
import { fmtBytes } from "~/lib/format.ts";
import { codeRest } from "~/lib/useCodeQuery.ts";

/** Said where a file is past what a caller reads whole. */
export class TooLarge extends Error {
  constructor(readonly bound: number) {
    super(`past ${fmtBytes(bound)}`);
    this.name = "TooLarge";
  }
}

/** Every page of `path` at `commit`, joined; a text past `max` bytes ends the read as [TooLarge]. */
export async function readWhole(
  repo: string,
  path: string,
  commit: string,
  signal: AbortSignal,
  max: number,
): Promise<string> {
  const utf8 = new TextEncoder();
  let text = "";
  let bytes = 0;
  let cursor = "";
  for (;;) {
    const answer = await codeRest.query(
      "code_blob",
      { name: repo, path, ref: commit, ...(cursor ? { cursor } : {}) },
      signal,
    );
    const page = answer.blob?.text ?? "";
    bytes += utf8.encode(page).length;
    if (bytes > max) throw new TooLarge(max);
    text += page;
    cursor = answer.next ?? "";
    if (!cursor) return text;
  }
}
