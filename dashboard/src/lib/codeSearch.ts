/**
 * A code search's answer as Code › Search and ⌘K's Code scope draw it: its
 * lines grouped by repository and then by file, each line's address on the
 * repository's page, and what the search did NOT cover, in words.
 *
 * # A hit is read at the commit its repository was searched at
 *
 * The engine answers each repository at the commit its git node's copy held
 * for the ref the search named (`tips`), never at the branch, which may have
 * moved since. So a hit links to its file AT THAT COMMIT, by its whole id,
 * with the line picked (`lines=`): an address that says tomorrow what the
 * search said today, which the repository's page answers immutably.
 *
 * # The coverage is never hidden
 *
 * A search across the company's repositories is a scatter over its git nodes,
 * and every answer names what it left out: a node that did not answer, an
 * index still being built, a copy behind the log, a repository searched only
 * when a `repo:` filter names it. "No match" over half the company is not "no
 * match", so the coverage is said on every answer, the repositories searched
 * first — a quiet sentence where everything was, a warning where lines may be
 * missing.
 */

import type { CodeSearchAnswer } from "~/contract/code.ts";
import { plural } from "./format.ts";

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
      file = { path: hit.path, pathB64: hit.path_b64, blob: hit.blob, lines: [] };
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
 * (`routes/code/paths.ts`'s `RepoAt`): Files, at the commit the repository
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
  const at: Record<string, string> = { tab: "files" };
  if (tip) at.ref = tip;
  at.path = file.path;
  at.lines = String(line);
  return at;
}

/** What a search covered, in words: the tone, the lead sentence and one line per gap. */
export interface CoverageNotes {
  /** `warning` where lines may be missing — a repository unanswered, or the answer cut. */
  variant: "info" | "warning";
  lead: string;
  notes: string[];
}

/** A set of repositories, by name — the first hundred the engine lists, and how many more. */
function named(set: RepoSet): string {
  const names = set.names ?? [];
  const more = set.count - names.length;
  if (names.length === 0) return "";
  return more > 0 ? `${names.join(", ")} and ${more} more` : names.join(", ");
}

function counted(set: RepoSet): string {
  const list = named(set);
  const head = plural(set.count, "repository", "repositories");
  return list ? `${head} (${list})` : head;
}

/**
 * The coverage of an answer in words, every set the engine names that is not
 * empty: what was searched, then what was left unanswered — which makes it a
 * warning — then what was read another way or left out by the search's own
 * terms.
 */
export function coverageNotes(coverage: Coverage): CoverageNotes {
  const notes: string[] = [];
  let missing = false;
  const unanswered = (set: RepoSet, why: string) => {
    if (set.count <= 0) return;
    missing = true;
    notes.push(`Not searched: ${counted(set)}, ${why}.`);
  };
  unanswered(coverage.building, "the git node asked was still building its index");
  unanswered(coverage.stale, "the git node asked was behind the code log or the configuration");
  for (const gap of coverage.not_answering ?? []) {
    if (gap.repos.count <= 0) continue;
    missing = true;
    notes.push(
      `Not searched: ${counted(gap.repos)}, git node ${gap.node} did not answer${gap.why ? ` (${gap.why})` : ""}.`,
    );
  }
  unanswered(coverage.not_permitted, "a git node judged you may not read them now");
  if (coverage.truncated) {
    missing = true;
    notes.push(
      "Lines may be missing: a repository’s search stopped at its limit, or the answer was cut at the most lines it holds.",
    );
  }
  if (coverage.not_indexed.count > 0) {
    notes.push(
      `Read with git grep, no index answering for the commit: ${counted(coverage.not_indexed)} — the same lines, read more slowly.`,
    );
  }
  if (coverage.grep_not_named.count > 0) {
    notes.push(
      `Left out: ${counted(coverage.grep_not_named)}, searched only when a repo: filter names them.`,
    );
  }
  if (coverage.no_ref.count > 0) {
    notes.push(`Nothing to search at the ref named in ${counted(coverage.no_ref)}.`);
  }
  if (coverage.skipped_files > 0) {
    notes.push(
      `${plural(coverage.skipped_files, "file")} passed over for their size or as binaries.`,
    );
  }
  return {
    variant: missing ? "warning" : "info",
    lead: `Searched ${plural(coverage.searched, "repository", "repositories")}.`,
    notes,
  };
}
