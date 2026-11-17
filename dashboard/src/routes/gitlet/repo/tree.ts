/**
 * A repository's tree beside a file: one `code_tree` read per directory opened,
 * at the file's commit, kept for the visit (an answer at a whole id never changes).
 */
import { useCallback, useEffect, useMemo, useSyncExternalStore } from "react";
import type { TreeInput } from "@crewlethq/ui";
import { baseName } from "~/lib/code.ts";
import { codeRest } from "~/lib/useCodeQuery.ts";
import { QueryError, queryErrorCode, type CodeTreeEntry } from "~/protocol/index.ts";
import { sortEntries } from "./entries.ts";
import { REFUSED_CODES, refusalSentence } from "./marks.ts";

/** How many directories are read at once. */
export const TREE_READS_AT_ONCE = 4;
/** How many commits' trees a visit keeps. */
export const TREE_COMMITS_KEPT = 8;

/** One directory's listing as far as it was read. */
export interface Level {
  entries: readonly CodeTreeEntry[];
  total: number;
  /** The next page's cursor, "" when none is left. */
  next: string;
  state: "reading" | "read" | "refused" | "failed";
  /** The engine's sentence for a refusal, where it wrote one. */
  refusal: string | null;
  /** Load more is being read, or failed. */
  paging: boolean;
  pageFailed: boolean;
}

type Levels = ReadonlyMap<string, Level>;

interface Kept {
  repo: string;
  commit: string;
  levels: Levels;
  queue: { dir: string; cursor: string }[];
  flying: number;
  /** Bumped when the last reader leaves: answers to an older generation are dropped. */
  generation: number;
  controller: AbortController;
  users: number;
  listeners: Set<() => void>;
}

const kept = new Map<string, Kept>();

/** Forget every kept tree: a test's fresh page load. */
export function forgetTrees(): void {
  kept.clear();
}

function keptFor(repo: string, commit: string): Kept {
  const key = `${repo}\n${commit}`;
  let k = kept.get(key);
  if (k) {
    kept.delete(key);
    kept.set(key, k);
    return k;
  }
  k = {
    repo,
    commit,
    levels: new Map(),
    queue: [],
    flying: 0,
    generation: 0,
    controller: new AbortController(),
    users: 0,
    listeners: new Set(),
  };
  kept.set(key, k);
  for (const [old, o] of kept) {
    if (kept.size <= TREE_COMMITS_KEPT) break;
    if (o.users === 0) kept.delete(old);
  }
  return k;
}

function setLevel(k: Kept, dir: string, level: Level | null): void {
  const next = new Map(k.levels);
  if (level) next.set(dir, level);
  else next.delete(dir);
  k.levels = next;
  for (const l of k.listeners) l();
}

const READING: Level = {
  entries: [],
  total: 0,
  next: "",
  state: "reading",
  refusal: null,
  paging: false,
  pageFailed: false,
};

function pump(k: Kept): void {
  while (k.flying < TREE_READS_AT_ONCE && k.queue.length > 0) {
    const { dir, cursor } = k.queue.shift()!;
    const generation = k.generation;
    k.flying++;
    codeRest
      .query(
        "code_tree",
        {
          name: k.repo,
          ref: k.commit,
          ...(dir ? { path: dir } : {}),
          ...(cursor ? { cursor } : {}),
        },
        k.controller.signal,
      )
      .then(
        (answer) => {
          if (generation !== k.generation) return;
          const was = k.levels.get(dir) ?? READING;
          const page = answer.tree?.entries ?? [];
          setLevel(k, dir, {
            ...was,
            entries: cursor ? [...was.entries, ...page] : page,
            total: answer.tree?.total ?? page.length,
            next: answer.next ?? "",
            state: "read",
            paging: false,
            pageFailed: false,
          });
        },
        (err: unknown) => {
          if (generation !== k.generation) return;
          const was = k.levels.get(dir) ?? READING;
          if (cursor) {
            setLevel(k, dir, { ...was, paging: false, pageFailed: true });
            return;
          }
          const code = queryErrorCode(err instanceof Error ? err.message : null) ?? "query_failed";
          const refused = REFUSED_CODES.has(code);
          setLevel(k, dir, {
            ...READING,
            state: refused ? "refused" : "failed",
            refusal:
              refused && err instanceof QueryError
                ? refusalSentence({ detail: err.detail ?? undefined, refusal: err.refusal })
                : null,
          });
        },
      )
      .finally(() => {
        if (generation !== k.generation) return;
        k.flying--;
        pump(k);
      });
  }
}

function ask(k: Kept, dir: string): void {
  if (k.levels.has(dir)) return;
  setLevel(k, dir, READING);
  k.queue.push({ dir, cursor: "" });
  pump(k);
}

function acquire(k: Kept): void {
  k.users++;
  if (k.controller.signal.aborted) k.controller = new AbortController();
}

/** The last reader left: abort what is in flight and forget what it would have answered. */
function release(k: Kept): void {
  k.users--;
  if (k.users > 0) return;
  k.controller.abort();
  k.generation++;
  k.flying = 0;
  k.queue = [];
  const next = new Map<string, Level>();
  for (const [dir, level] of k.levels) {
    if (level.state === "reading") continue;
    next.set(dir, level.paging ? { ...level, paging: false } : level);
  }
  k.levels = next;
}

const NO_LEVELS: Levels = new Map();

export interface RepoTreeReads {
  /** Each directory asked, by its path ("" the root); empty while the commit is unknown. */
  levels: Levels;
  /** Read a directory's next page. */
  more: (dir: string) => void;
  /** Ask a failed directory, or its failed page, again. */
  retry: (dir: string) => void;
}

/** Read `dirs` of `repo` at `commit` (none while it is ""), each once for the visit. */
export function useRepoTree(repo: string, commit: string, dirs: readonly string[]): RepoTreeReads {
  const k = useMemo(() => (commit ? keptFor(repo, commit) : null), [repo, commit]);
  useEffect(() => {
    if (!k) return undefined;
    acquire(k);
    return () => release(k);
  }, [k]);
  const subscribe = useCallback(
    (listener: () => void) => {
      if (!k) return () => {};
      k.listeners.add(listener);
      return () => k.listeners.delete(listener);
    },
    [k],
  );
  const levels = useSyncExternalStore(subscribe, () => k?.levels ?? NO_LEVELS);
  const wanted = dirs.join("\n");
  useEffect(() => {
    if (!k) return;
    for (const dir of wanted.split("\n")) ask(k, dir);
  }, [k, wanted]);
  const more = useCallback(
    (dir: string) => {
      const level = k?.levels.get(dir);
      if (!k || !level || level.state !== "read" || !level.next || level.paging) return;
      setLevel(k, dir, { ...level, paging: true, pageFailed: false });
      k.queue.push({ dir, cursor: level.next });
      pump(k);
    },
    [k],
  );
  const retry = useCallback(
    (dir: string) => {
      const level = k?.levels.get(dir);
      if (!k || !level) return;
      if (level.state === "failed") {
        setLevel(k, dir, null);
        ask(k, dir);
      } else if (level.pageFailed) {
        more(dir);
      }
    },
    [k, more],
  );
  return { levels, more, retry };
}

/** What one node of the tree is. */
export type TreeNode =
  | { kind: "dir"; path: string; name: string }
  | { kind: "file"; path: string; name: string; entry: CodeTreeEntry }
  /** A submodule, or a path git cannot spell as text: listed, with no link. */
  | { kind: "inert"; path: string; name: string; entry: CodeTreeEntry }
  | { kind: "loading"; dir: string }
  | { kind: "more"; dir: string; level: Level }
  | { kind: "refused"; dir: string; refusal: string | null }
  | { kind: "failed"; dir: string };

/** The kit's forest and what each node is, from the levels read. */
export interface Forest {
  roots: readonly TreeInput[];
  nodes: ReadonlyMap<string, TreeNode>;
}

// A NODE'S ID is its path; a state's row and a path git cannot spell take "//", which no path holds.
const markId = (dir: string, mark: string) => `${dir}//${mark}`;
const entryId = (e: CodeTreeEntry) => (e.path_b64 ? `b64//${e.path_b64}` : e.path);

/** A directory's name as its rows name it: the repository's root is "the root". */
export const dirWord = (dir: string) => (dir ? baseName(dir) : "the root");

/**
 * The forest of `levels` under the root: a directory read whole in the code
 * host's order, a paged one in git's with Load more after it, an unread one
 * holding one placeholder so it can be opened.
 */
export function treeForest(levels: Levels): Forest {
  const nodes = new Map<string, TreeNode>();
  const childrenOf = (dir: string): TreeInput[] => {
    const level = levels.get(dir);
    if (!level || level.state === "reading") {
      const id = markId(dir, "loading");
      nodes.set(id, { kind: "loading", dir });
      return [{ id, label: `Loading ${dirWord(dir)}` }];
    }
    if (level.state === "refused" || level.state === "failed") {
      const id = markId(dir, level.state);
      nodes.set(
        id,
        level.state === "refused"
          ? { kind: "refused", dir, refusal: level.refusal }
          : { kind: "failed", dir },
      );
      return [{ id, label: dirWord(dir) }];
    }
    const whole = level.entries.length >= level.total && !level.next;
    const entries = whole ? sortEntries(level.entries) : level.entries;
    const out: TreeInput[] = entries.map((e) => {
      const id = entryId(e);
      const name = baseName(e.path);
      if (e.type === "tree" && !e.path_b64) {
        nodes.set(id, { kind: "dir", path: e.path, name });
        return { id, label: name, children: childrenOf(e.path) };
      }
      const inert = Boolean(e.path_b64) || e.mode === "160000";
      nodes.set(id, { kind: inert ? "inert" : "file", path: e.path, name, entry: e });
      return { id, label: name };
    });
    if (level.next) {
      const id = markId(dir, "more");
      nodes.set(id, { kind: "more", dir, level });
      out.push({ id, label: `Load more in ${dirWord(dir)}` });
    }
    return out;
  };
  const root = levels.get("");
  if (!root || root.state !== "read") return { roots: [], nodes };
  return { roots: childrenOf(""), nodes };
}

/** The directories a path passes through, the root's children first ("cmd", "cmd/web"). */
export function ancestorsOf(path: string): string[] {
  const parts = path.split("/");
  return parts.slice(0, -1).map((_, i) => parts.slice(0, i + 1).join("/"));
}
