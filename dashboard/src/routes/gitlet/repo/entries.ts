/**
 * A directory's entries in a code host's order, and each one's last commit
 * (`code_tree_commits`), read page by page at the listing's commit.
 */
import { useCallback, useEffect, useRef, useState } from "react";
import { baseName } from "~/lib/code.ts";
import { codeRest } from "~/lib/useCodeQuery.ts";
import type { CodeLogCommit, CodeTreeCommit, CodeTreeEntry } from "~/protocol/index.ts";
import { failureOf, type MarksFailure } from "./marks.ts";

/** Directories first, then by name case-folded; entries that tie keep git's order. */
export function sortEntries(entries: readonly CodeTreeEntry[]): CodeTreeEntry[] {
  const keyOf = (e: CodeTreeEntry) => baseName(e.path).toLowerCase();
  return [...entries].sort((a, b) => {
    const dirs = Number(b.type === "tree") - Number(a.type === "tree");
    if (dirs !== 0) return dirs;
    const x = keyOf(a);
    const y = keyOf(b);
    return x < y ? -1 : x > y ? 1 : 0;
  });
}

/** What the walk holds for one directory at one commit. */
interface Walk {
  key: string;
  entries: ReadonlyMap<string, CodeTreeCommit>;
  commits: ReadonlyMap<string, CodeLogCommit>;
  /** The next page's cursor: null before the first page, "" once none is left. */
  next: string | null;
  asking: boolean;
  failed: MarksFailure | null;
}

export interface TreeCommits {
  /** An entry's last commit by its path; undefined until a page answers it. */
  of: (path: string) => CodeTreeCommit | undefined;
  commit: (oid: string) => CodeLogCommit | undefined;
  /** Every page there is has been read. */
  done: boolean;
  failed: MarksFailure | null;
  /** Ask the failed page again. */
  retry: () => void;
}

const fresh = (key: string): Walk => ({
  key,
  entries: new Map(),
  commits: new Map(),
  next: null,
  asking: false,
  failed: null,
});

/**
 * Each loaded entry's last commit: the first page, then the next one at a time
 * while the loaded entries hold a path no page has answered. Another commit or
 * path starts again.
 */
export function useTreeCommits(
  repo: string,
  commit: string,
  path: string,
  entries: readonly CodeTreeEntry[],
): TreeCommits {
  const key = `${repo}\n${commit}\n${path}`;
  const [held, setHeld] = useState<Walk>(() => fresh(key));
  const walk = held.key === key ? held : fresh(key);
  const flight = useRef<AbortController | null>(null);
  const unanswered = entries.some((e) => !walk.entries.has(e.path));
  const due = commit !== "" && !walk.asking && !walk.failed && walk.next !== "" && unanswered;
  const cursor = walk.next;

  useEffect(() => {
    if (!due) return;
    const abort = new AbortController();
    flight.current = abort;
    setHeld((h) => ({ ...(h.key === key ? h : fresh(key)), asking: true }));
    codeRest
      .query(
        "code_tree_commits",
        { name: repo, ref: commit, ...(path ? { path } : {}), ...(cursor ? { cursor } : {}) },
        abort.signal,
      )
      .then(
        (answer) => {
          if (abort.signal.aborted) return;
          const page = answer.tree_commits;
          setHeld((h) => {
            if (h.key !== key) return h;
            const entries = new Map(h.entries);
            for (const e of page?.entries ?? []) entries.set(e.path, e);
            const commits = new Map(h.commits);
            for (const c of page?.commits ?? []) commits.set(c.oid, c);
            return { ...h, entries, commits, next: answer.next ?? "", asking: false };
          });
        },
        (err: unknown) => {
          if (abort.signal.aborted) return;
          setHeld((h) => (h.key === key ? { ...h, asking: false, failed: failureOf(err) } : h));
        },
      );
  }, [due, key, repo, commit, path, cursor]);

  // ANOTHER DIRECTORY, OR LEAVING, lets the page on its way go.
  useEffect(() => () => flight.current?.abort(), [key]);

  const retry = useCallback(() => setHeld((h) => ({ ...h, failed: null })), []);
  return {
    of: (p) => walk.entries.get(p),
    commit: (oid) => walk.commits.get(oid),
    done: walk.next === "" && !walk.asking,
    failed: walk.failed,
    retry,
  };
}
