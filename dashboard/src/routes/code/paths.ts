/**
 * The Code workspace's addresses, written once for every screen that links
 * to one.
 */

import { href } from "~/app/router.tsx";
import type { ObjectRef } from "~/app/frame/objects.ts";
import { changeRef } from "~/lib/code.ts";

/**
 * The query key that narrows Pushes to one repository, by its name — and
 * Changes to one repository's changes. A query and never a segment: a section
 * is a path, and a repository is what the section is narrowed to.
 */
export const REPO_PARAM = "repo";

/** Code › Pushes, narrowed to one repository's history. */
export function pushesHref(repo: string): string {
  return href(["code", "pushes"], { [REPO_PARAM]: repo });
}

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
