/** A code search's answer as Gitlet › Search and ⌘K's Code scope draw it: files of blocks, the
 *  matches marked, facets, and what was searched in words. */

import type { CodeSearchAnswer } from "~/contract/code.ts";
import { plural } from "./format.ts";
import type { Kind, Token } from "./syntax/types.ts";

/** Lines of context asked on each side of a hit: the engine's most (`gitlet.MaxSearchContext`). */
export const SEARCH_CONTEXT = 3;
/** Lines of one file a search answers; the rest are behind Show N more. */
export const SEARCH_PER_FILE = 3;

/** One matching line, as the engine answers it (`codesearch.Hit`). */
export type CodeSearchHit = CodeSearchAnswer["hits"][number];
type Coverage = CodeSearchAnswer["coverage"];
type RepoSet = Coverage["building"];

/** One file's matching lines, in line order. */
export interface FileHits {
  path: string;
  /** The path's exact bytes, in base64, where they are not UTF-8: no address names such a path. */
  pathB64?: string;
  /** The file's object id. */
  blob: string;
  lines: CodeSearchHit[];
  /** How many of the file's lines matched, where the engine answered it (`file_matches`). */
  total?: number;
}

/** One repository's matching files, in path order, and the commit it was searched at. */
export interface RepoHits {
  repo: string;
  /** The commit its lines were read at; absent only where the answer named none. */
  tip?: string;
  files: FileHits[];
  /** How many lines matched in it. */
  lines: number;
}

/**
 * The hits grouped by repository, then by file, IN THE ENGINE'S ORDER: it
 * answers them by repository, path and line, and a group is drawn where its
 * first line falls, so nothing here re-sorts what the engine merged. A file
 * is its path's bytes — the base64 where the engine gave one — so two paths
 * that read alike as text are never folded into one.
 */
export function groupHits(answer: Pick<CodeSearchAnswer, "hits" | "tips">): RepoHits[] {
  const repos = new Map<string, RepoHits>();
  const files = new Map<string, FileHits>();
  for (const hit of answer.hits ?? []) {
    let repo = repos.get(hit.repo);
    if (!repo) {
      repo = { repo: hit.repo, tip: answer.tips?.[hit.repo], files: [], lines: 0 };
      repos.set(hit.repo, repo);
    }
    const fileKey = `${hit.repo}\u0000${hit.path_b64 ?? hit.path}`;
    let file = files.get(fileKey);
    if (!file) {
      file = {
        path: hit.path,
        pathB64: hit.path_b64,
        blob: hit.blob,
        lines: [],
        ...(hit.file_matches !== undefined ? { total: hit.file_matches } : {}),
      };
      files.set(fileKey, file);
      repo.files.push(file);
    }
    file.lines.push(hit);
    repo.lines++;
  }
  return [...repos.values()];
}

/**
 * Where a hit's line is on its repository's page, as the page's query keys
 * (`routes/gitlet/paths.ts`'s `RepoAt`): Files, at the commit the repository
 * was searched at, the file, and the line picked — or null for a path that is
 * not UTF-8, which no address can name.
 *
 * WITHOUT A TIP the file is opened at the default branch: the engine names a
 * tip for every repository with a hit, so this is an answer from an engine
 * that named none, and the default branch is the one revision a search with
 * no ref: reads.
 */
export function hitAt(
  tip: string | undefined,
  file: FileHits,
  line: number,
): Record<string, string> | null {
  if (file.pathB64) return null;
  // THE CODE TAB IS THE PAGE'S LANDING, so a hit's address names no tab.
  const at: Record<string, string> = {};
  if (tip) at.ref = tip;
  at.path = file.path;
  at.lines = String(line);
  return at;
}

/** What a search covered, in words: the tone, the lead sentence and one line per gap. */
export interface CoverageNotes {
  /** `warning` where lines may be missing: a repository unanswered, or the answer cut. */
  variant: "info" | "warning";
  lead: SearchLead;
  notes: string[];
}

/** A set of repositories in words: "web and api", "docs, wiki and 3 more", or "2 repositories". */
function named(set: RepoSet): string {
  const names = set.names ?? [];
  const more = set.count - names.length;
  if (names.length === 0) return plural(set.count, "repository", "repositories");
  if (more > 0) return `${names.join(", ")} and ${more} more`;
  return names.length === 1 ? names[0]! : `${names.slice(0, -1).join(", ")} and ${names.at(-1)}`;
}

/** The search's lead sentence in parts, so the count can be drawn strong and the ref in mono. */
export interface SearchLead {
  /** "14 matching lines", "At least 14 matching lines", or "" where nothing matched. */
  count: string;
  /** What follows the count, up to the revision. */
  rest: string;
  /** The ref the address names, "" for each repository's default branch, or null where the
   *  phrase names one with `ref:`, so the sentence names no revision. */
  ref: string | null;
}

/** A `ref:` word in the phrase: a token check, not a reading of the grammar. */
const PHRASE_REF = /(^|\s)ref:\S/;

/** What was found and on what: lines, files and repositories, "at least" where a count is short. */
export function searchLine(
  groups: readonly RepoHits[],
  coverage: Pick<Coverage, "searched" | "truncated">,
  ref: string,
  q = "",
): SearchLead {
  const on = ref || (PHRASE_REF.test(q) ? null : "");
  const files = groups.flatMap((g) => g.files);
  if (files.length === 0) {
    const searched = plural(coverage.searched, "repository", "repositories");
    return { count: "", rest: `No line matched in ${searched}`, ref: on };
  }
  const lines = matchingLines(files);
  const words = plural(lines.count, "matching line");
  return {
    count: lines.exact && !coverage.truncated ? words : `At least ${words}`,
    rest: ` in ${plural(files.length, "file")} across ${plural(groups.length, "repository", "repositories")}`,
    ref: on,
  };
}

/**
 * The coverage of an answer in words, every set the engine names that is not empty: first what
 * may be missing, which makes it a warning, then what was read another way or left out by the
 * search's own terms.
 */
export function coverageNotes(
  coverage: Coverage,
  groups: readonly RepoHits[] = [],
  ref = "",
  q = "",
): CoverageNotes {
  const notes: string[] = [];
  let missing = false;
  const warn = (note: string) => {
    missing = true;
    notes.push(note);
  };
  const one = (set: RepoSet) => set.count === 1;
  if (coverage.building.count > 0) {
    warn(
      `Not searched: ${named(coverage.building)}, the Gitlet node asked was still building ${one(coverage.building) ? "its" : "their"} index.`,
    );
  }
  if (coverage.stale.count > 0) {
    warn(
      `Not searched: ${named(coverage.stale)}, the Gitlet node asked was behind the code log or the configuration.`,
    );
  }
  for (const gap of coverage.not_answering ?? []) {
    if (gap.repos.count <= 0) continue;
    warn(
      `Not searched: ${named(gap.repos)}, Gitlet node ${gap.node} did not answer${gap.why ? ` (${gap.why})` : ""}.`,
    );
  }
  if (coverage.not_permitted.count > 0) {
    warn(
      `Not searched: ${named(coverage.not_permitted)}, a Gitlet node judged you may not read ${one(coverage.not_permitted) ? "it" : "them"} now.`,
    );
  }
  if (coverage.truncated) {
    warn(
      "Lines may be missing: the answer holds the most lines one search answers. Narrow it with repo: or path:.",
    );
  }
  if (coverage.past_limit.count > 0) {
    warn(
      `Past the limit: ${named(coverage.past_limit)}, named after the last line shown. Narrow the search with repo: or path: to reach them.`,
    );
  }
  const ni = coverage.not_indexed;
  if (ni.count > 0) {
    notes.push(
      one(ni)
        ? `${named(ni)} was read with git grep: its index was not ready, so the same lines came more slowly.`
        : `${named(ni)} were read with git grep: their indexes were not ready, so the same lines came more slowly.`,
    );
  }
  const grep = coverage.grep_not_named;
  if (grep.count > 0) {
    const lone = one(grep) ? (grep.names ?? [])[0] : undefined;
    notes.push(
      lone
        ? `${lone} is not indexed: name it with repo:${lone} to search it.`
        : `${named(grep)} ${one(grep) ? "is" : "are"} not indexed: name ${one(grep) ? "it" : "one"} with repo: to search it.`,
    );
  }
  if (coverage.no_ref.count > 0) {
    notes.push(`Nothing to search at ${ref || "the ref named"} in ${named(coverage.no_ref)}.`);
  }
  if (coverage.skipped_files > 0) {
    notes.push(
      `${plural(coverage.skipped_files, "file")} passed over for their size or as binaries.`,
    );
  }
  return {
    variant: missing ? "warning" : "info",
    lead: searchLine(groups, coverage, ref, q),
    notes,
  };
}

/** Directories a path facet lists, the largest first; the rest are counted under them. */
export const FACET_PATHS = 8;

/** One facet row: what it narrows to, and how many matching lines the answer holds there. */
export interface FacetRow {
  key: string;
  count: number;
}

/** The answer's matching lines by repository and by directory, counted from what came back. */
export interface Facets {
  all: number;
  repos: FacetRow[];
  /** Each directory as a `path:` pattern names it, without its trailing `/**`. */
  paths: FacetRow[];
  /** Directories past the ones listed. */
  morePaths: number;
  /** Why the counts are a floor: the answer was cut, or a file's total was not answered. */
  partial: "cut" | "no total" | null;
}

export function facetsOf(
  groups: readonly RepoHits[],
  coverage: Pick<Coverage, "truncated">,
): Facets {
  let partial: Facets["partial"] = coverage.truncated ? "cut" : null;
  const repos: FacetRow[] = [];
  const dirs = new Map<string, number>();
  for (const g of groups) {
    const lines = matchingLines(g.files);
    if (!lines.exact) partial ??= "no total";
    repos.push({ key: g.repo, count: lines.count });
    for (const f of g.files) {
      const dir = dirOf(f.path).slice(0, -1);
      // A root file counts in All only, and a path that is not UTF-8 has no pattern to name it.
      if (!dir || f.pathB64) continue;
      dirs.set(dir, (dirs.get(dir) ?? 0) + (f.total ?? f.lines.length));
    }
  }
  const paths = [...dirs]
    .map(([key, count]) => ({ key, count }))
    .sort((a, b) => b.count - a.count || (a.key < b.key ? -1 : a.key > b.key ? 1 : 0));
  return {
    all: repos.reduce((n, r) => n + r.count, 0),
    repos,
    paths: paths.slice(0, FACET_PATHS),
    morePaths: Math.max(0, paths.length - FACET_PATHS),
    partial,
  };
}

/** A path's directory with its trailing slash ("" at the root), and its last component. */
export function dirOf(path: string): string {
  return path.slice(0, path.lastIndexOf("/") + 1);
}
export function baseOf(path: string): string {
  return path.slice(path.lastIndexOf("/") + 1);
}

/** How many lines matched across files: each file's total, else the lines answered (then "at least"). */
export function matchingLines(files: readonly FileHits[]): { count: number; exact: boolean } {
  let count = 0;
  let exact = true;
  for (const f of files) {
    if (f.total === undefined) exact = false;
    count += f.total ?? f.lines.length;
  }
  return { count, exact };
}

/** One drawn line of a file: a hit (with where it matched) or a line around one. */
export interface SearchRow {
  line: number;
  text: string;
  hit: boolean;
  matches?: readonly (readonly [number, number])[];
}

/** A run of consecutive lines; the lines between two blocks are a gap. */
export interface SearchBlock {
  first: number;
  last: number;
  rows: SearchRow[];
}

/**
 * A file's hits with their context, and any lines revealed, as blocks of consecutive lines.
 * A hit's own line wins over the same line answered as context; otherwise the first answer wins.
 */
export function fileBlocks(
  hits: readonly CodeSearchHit[],
  revealed: ReadonlyMap<number, string> = new Map(),
): SearchBlock[] {
  const rows = new Map<number, SearchRow>();
  const put = (row: SearchRow) => {
    const had = rows.get(row.line);
    if (!had || (row.hit && !had.hit)) rows.set(row.line, row);
  };
  for (const h of hits) {
    const before = h.before ?? [];
    before.forEach((text, i) => put({ line: h.line - before.length + i, text, hit: false }));
    put({ line: h.line, text: h.text, hit: true, ...(h.matches ? { matches: h.matches } : {}) });
    (h.after ?? []).forEach((text, i) => put({ line: h.line + 1 + i, text, hit: false }));
  }
  for (const [line, text] of revealed) put({ line, text, hit: false });
  const blocks: SearchBlock[] = [];
  for (const row of [...rows.values()].sort((a, b) => a.line - b.line)) {
    const block = blocks[blocks.length - 1];
    if (block && row.line === block.last + 1) {
      block.rows.push(row);
      block.last = row.line;
    } else blocks.push({ first: row.line, last: row.line, rows: [row] });
  }
  return blocks;
}

/** UTF-8 bytes of a code point; a lone surrogate is sent as U+FFFD, three bytes. */
function utf8Length(cp: number): number {
  return cp < 0x80 ? 1 : cp < 0x800 ? 2 : cp < 0x10000 ? 3 : 4;
}

/**
 * A hit's matches, UTF-8 byte offsets into its text, as `[from, to)` string indices.
 * A pair off a character's boundary, past the text, empty or overlapping the one before is dropped.
 */
export function matchRanges(
  text: string,
  matches: readonly (readonly [number, number])[] | undefined,
): [number, number][] {
  if (!matches || matches.length === 0) return [];
  const at = new Map<number, number>();
  let bytes = 0;
  let index = 0;
  for (const ch of text) {
    at.set(bytes, index);
    bytes += utf8Length(ch.codePointAt(0)!);
    index += ch.length;
  }
  at.set(bytes, index);
  const out: [number, number][] = [];
  for (const [start, end] of matches) {
    const from = at.get(start);
    const to = at.get(end);
    if (from === undefined || to === undefined || to <= from) continue;
    if (out.length > 0 && from < out[out.length - 1]![1]) continue;
    out.push([from, to]);
  }
  return out;
}

/** A piece of a line: where it ends, its token's kind, and the match it falls in (-1 for none). */
export interface Segment {
  end: number;
  kind: Kind | null;
  mark: number;
}

/** A line's tokens (or its plain text where there are none) split at every match's edges. */
export function markedSegments(
  text: string,
  tokens: readonly Token[] | null,
  ranges: readonly (readonly [number, number])[],
): Segment[] {
  const runs: readonly Token[] = tokens && tokens.length > 0 ? tokens : [];
  const out: Segment[] = [];
  let from = 0;
  let t = 0;
  let r = 0;
  while (from < text.length) {
    while (t < runs.length && runs[t]!.end <= from) t++;
    while (r < ranges.length && ranges[r]![1] <= from) r++;
    const token = runs[t];
    const range = ranges[r];
    const inRange = range !== undefined && range[0] <= from;
    let end = Math.min(token?.end ?? text.length, text.length);
    if (range) end = Math.min(end, inRange ? range[1] : range[0]);
    out.push({ end, kind: token?.kind ?? null, mark: inRange ? r : -1 });
    from = end;
  }
  return out;
}
