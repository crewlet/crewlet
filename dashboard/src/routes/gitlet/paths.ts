/**
 * The Gitlet workspace's addresses, written once for every screen that links
 * to one.
 */

import { href } from "~/app/router.tsx";
import type { ObjectRef } from "~/app/frame/objects.ts";
import { changePath, changeRef, commitPath, repoPath } from "~/lib/code.ts";

/**
 * A repository page's tabs, its code first: what a reader opens a repository
 * for, as every code host lands on it — the files at the default branch, its
 * README and what the repository is — then the changes proposed to it, its
 * history, its branches and tags, its pushes, its checks and its settings.
 */
export const REPO_TABS = [
  "code",
  "changes",
  "commits",
  "branches",
  "pushes",
  "checks",
  "settings",
] as const;
export type RepoTab = (typeof REPO_TABS)[number];

/**
 * Where on a repository's page an address points, by its query keys: the tab,
 * and on Code and Commits the revision (`ref`, a branch, a tag, a whole ref
 * or a commit id; none is the default branch) and the path; on Code the lens
 * (`blame`) and the lines picked (`12`, `12-20`); on Commits who pushed (`by`,
 * a seat), from when (`when`) and words in a message (`q`).
 */
export interface RepoAt {
  tab?: RepoTab;
  ref?: string;
  path?: string;
  lens?: "blame";
  lines?: string;
  by?: string;
  when?: "today" | "7d" | "30d" | "90d";
  q?: string;
}

/**
 * A repository's page, at `at`: `#/gitlet/repos/{name}`, its code where `at`
 * names no tab — so a file's address is the repository's and its path,
 * `#/gitlet/repos/web?path=cmd%2Fmain.go`.
 */
export function repoHref(repo: string, at: RepoAt = {}): string {
  const query: Record<string, string> = {};
  for (const [key, value] of Object.entries(at)) {
    // THE LANDING TAB IS NEVER SPELLED, as `useTab` drops it from the address
    // a reader moves to: one page, one address.
    if (value && !(key === "tab" && value === "code")) query[key] = value;
  }
  return href(repoPath(repo), query);
}

/** A commit's page, by its whole id. */
export function commitHref(repo: string, sha: string): string {
  return href(commitPath(repo, sha));
}

/**
 * A comparison of two revisions of a repository, `#/gitlet/repos/{name}/compare`:
 * `base=` (none is the default branch) and `head=`, each as a revision is
 * named on the repository's page, `file=`, the path its diff opens on, and
 * `line=`, the line of that file's NEWER side the diff marks and opens at —
 * where a check's failure points. A line names nothing without its file, so
 * it is spelled only beside one.
 */
export function compareHref(
  repo: string,
  at: { base?: string; head?: string; file?: string; line?: number } = {},
): string {
  const query: Record<string, string> = {};
  if (at.base) query.base = at.base;
  if (at.head) query.head = at.head;
  if (at.file) query.file = at.file;
  if (at.file && at.line && at.line > 0) query.line = String(at.line);
  return href([...repoPath(repo), "compare"], query);
}

/**
 * A change's own page opened on its diff: the Files tab, every file its head
 * revision changes with each one's patch (`ChangePage`'s `tab=files`).
 */
export function changeDiffHref(repo: string, number: number): string {
  return href(changePath(repo, number), { tab: "files" });
}

/**
 * The query key that narrows Pushes to one repository, by its name — and
 * Changes to one repository's changes. A query and never a segment: a section
 * is a path, and a repository is what the section is narrowed to.
 */
export const REPO_PARAM = "repo";

/**
 * What Gitlet › Changes is narrowed to, by its query keys: the engine's own
 * filters of a listing of changes (`gitlet.ReadQuery`), each as the listing
 * sends it.
 */
export interface ChangesFilter {
  state?: string;
  needs?: string;
  repo?: string;
  owner?: string;
  reviewer?: string;
  work?: string;
}

/** Gitlet › Changes, narrowed by `filter`. */
export function changesHref(filter: ChangesFilter = {}): string {
  const query: Record<string, string> = {};
  for (const [key, value] of Object.entries(filter)) {
    if (value) query[key] = value;
  }
  return href(["gitlet"], query);
}

/** A change as the frame addresses it: its reference, `web!42`. */
export function changeObject(repo: string, number: number): ObjectRef {
  return { kind: "change", id: changeRef(repo, number) };
}
