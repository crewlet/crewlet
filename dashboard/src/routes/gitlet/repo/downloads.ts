/**
 * Where a repository's bytes download (`/git/{repo}/-/raw` and `archive.zip`):
 * routes the engine that serves this dashboard serves too, so they are linked
 * once the commit is known, never probed.
 */
import { pathSegment } from "~/protocol/index.ts";

/** A whole commit's files as a ZIP, on the dashboard's own origin so the session cookie carries it. */
export function archiveHref(name: string, commit: string): string {
  return `/git/${pathSegment(name)}/-/archive.zip?${new URLSearchParams({ rev: commit })}`;
}

/** One file's bytes at a whole commit; `download` asks for an attachment whatever the file is. */
export function rawHref(name: string, commit: string, path: string, download = false): string {
  const query = new URLSearchParams({ rev: commit, path, ...(download ? { download: "1" } : {}) });
  return `/git/${pathSegment(name)}/-/raw?${query}`;
}
