/**
 * One file's patch read whole, a page at a time, and drawn as one.
 *
 * The engine answers a file's patch a page at a time (`gitlet.DiffAnswer.Patch`,
 * `next` the byte of the patch the page after begins at), each page at most
 * one answer's bound (`gitlet.MaxGitAnswerBytes`, 56 KiB of JSON) and cut at a
 * line, never inside one: so a change of a thousand lines or so — a generated
 * file, a large refactor, a vendored drop — is several pages, and the pages'
 * texts, joined in order, are the patch git wrote. Read as its first page
 * alone, such a file was drawn as its opening and a note that its end was
 * not, and nothing could read the rest.
 *
 * THE PAGES ARE `usePaged`'s, like a file's lines (`repo/Files.tsx`): the
 * first an ordinary code question, every later one asked by the reader's
 * press, each joined to the one before it. A page is one entry, named by the
 * cursor to the page after it — which no two pages of one patch share, the
 * last one's being none — and the hunks are parsed from the joined text ONCE
 * a page arrives, never on a render that brought none: `parsePatch` reads
 * every line it is handed, and a file of many pages is many thousands.
 *
 * Each caller asks its own question at its own call (`code_diff`, or a
 * change's `diff` part), the kind written out there, because a kind this
 * module took as a variable is one the engine's reader gate cannot see
 * (`usePaged`'s doc).
 */

import { useMemo } from "react";
import type { DiffHunk } from "~/components/DiffView.tsx";
import { parsePatch } from "~/lib/patch.ts";
import { usePaged, type Paged } from "~/lib/usePaged.ts";
import type { QueryResult } from "~/lib/useQuery.ts";
import type { CodeDiffAnswer, CodeDiffFile } from "~/protocol/index.ts";

/** What a page of a patch is answered in: `code_diff`'s answer, or a change's `diff` part. */
export interface PatchAnswer {
  diff?: CodeDiffAnswer;
  next?: string;
}

/** One page of a patch: its text, and the cursor to the page after it ("" for none). */
export interface PatchPage {
  next: string;
  text: string;
}

// MODULE CONSTANTS, never callbacks built in a render: `usePaged` keeps its
// entries by these, and a new function each render is a new list each render.
const pagesOf = (a: PatchAnswer): readonly PatchPage[] =>
  a.diff ? [{ next: a.next ?? "", text: a.diff.patch ?? "" }] : [];
const pageId = (p: PatchPage): string => p.next;
const nextOf = (a: PatchAnswer): string => a.next ?? "";

/** A file's patch as the pages read so far draw it. */
export interface PatchPages<A extends PatchAnswer> {
  /** The pages, followed past the first as the reader asks. */
  paged: Paged<PatchPage, A>;
  /** The hunks of every page read, parsed from their joined text. */
  hunks: DiffHunk[];
  /** The file as the patch's own answer describes it — its status, modes and whether git calls it binary. */
  file: CodeDiffFile | undefined;
}

/**
 * Follow a file's patch past its first page: `first` is the first page's
 * question and `params` its parameters, and `page` asks for the page after a
 * cursor, with the question's kind written out at the call.
 */
export function usePatchPages<A extends PatchAnswer>(
  first: QueryResult<A>,
  params: Record<string, unknown>,
  page: (cursor: string) => Promise<A>,
): PatchPages<A> {
  const paged = usePaged(first, params, page, pagesOf, pageId, nextOf);
  const hunks = useMemo(() => parsePatch(paged.items.map((p) => p.text).join("")), [paged.items]);
  return { paged, hunks, file: first.data?.diff?.files?.[0] };
}
