/**
 * The Code workspace's addresses, written once for every screen that links
 * to one.
 */

import { href } from "~/app/router.tsx";

/**
 * The query key that narrows Pushes to one repository, by its name. A query
 * and never a segment: a section is a path, and a repository is what the
 * section is narrowed to.
 */
export const REPO_PARAM = "repo";

/** Code › Pushes, narrowed to one repository's history. */
export function pushesHref(repo: string): string {
  return href(["code", "pushes"], { [REPO_PARAM]: repo });
}
