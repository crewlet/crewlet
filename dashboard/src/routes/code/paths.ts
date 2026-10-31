/**
 * The Code workspace's addresses, written once for every screen that links
 * to one.
 */

import { href } from "~/app/router.tsx";
import type { ObjectRef } from "~/app/frame/objects.ts";
import { changeRef, commitPath, repoPath } from "~/lib/code.ts";

/** A repository page's tabs, the overview first: what a reader opens a repository for. */
export const REPO_TABS = [
  "overview",
  "files",
  "branches",
  "commits",
  "pushes",
  "settings",
] as const;
export type RepoTab = (typeof REPO_TABS)[number];

/**
 * Where on a repository's page an address points, by its query keys: the tab,
 * and on Files and Commits the revision (`ref`, a branch, a tag, a whole ref
 * or a commit id; none is the default branch) and the path; on Files the lens
 * (`blame`) and the lines picked (`12`, `12-20`).
 */
export interface RepoAt {
  tab?: RepoTab;
  ref?: string;
  path?: string;
  lens?: "blame";
  lines?: string;
}

/** A repository's page, at `at`: `#/code/repos/{name}`, the overview where `at` names no tab. */
export function repoHref(repo: string, at: RepoAt = {}): string {
  const query: Record<string, string> = {};
  for (const [key, value] of Object.entries(at)) {
    // THE LANDING TAB IS NEVER SPELLED, as `useTab` drops it from the address
    // a reader moves to: one page, one address.
    if (value && !(key === "tab" && value === "overview")) query[key] = value;
  }
  return href(repoPath(repo), query);
}

/** A commit's page, by its whole id. */
export function commitHref(repo: string, sha: string): string {
  return href(commitPath(repo, sha));
}

/**
 * A comparison of two revisions of a repository, `#/code/repos/{name}/compare`:
 * `base=` (none is the default branch) and `head=`, each as a revision is
 * named on the repository's page.
 */
export function compareHref(repo: string, at: { base?: string; head?: string } = {}): string {
  const query: Record<string, string> = {};
  if (at.base) query.base = at.base;
  if (at.head) query.head = at.head;
  return href([...repoPath(repo), "compare"], query);
}

/**
 * The query key that narrows Pushes to one repository, by its name — and
 * Changes to one repository's changes. A query and never a segment: a section
 * is a path, and a repository is what the section is narrowed to.
 */
export const REPO_PARAM = "repo";

/**
 * What Code › Changes is narrowed to, by its query keys: the engine's own
 * filters of a listing of changes (`code.ReadQuery`), each as the listing
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

/** Code › Changes, narrowed by `filter`. */
export function changesHref(filter: ChangesFilter = {}): string {
  const query: Record<string, string> = {};
  for (const [key, value] of Object.entries(filter)) {
    if (value) query[key] = value;
  }
  return href(["code"], query);
}

/** A change as the frame addresses it: its reference, `web!42`. */
export function changeObject(repo: string, number: number): ObjectRef {
  return { kind: "change", id: changeRef(repo, number) };
}
