/**
 * The screens, one chunk per workspace, loaded when they are first needed.
 *
 * # Why the screens are split at all
 *
 * The flat import list this replaced argued that a chunk buys a round trip
 * against a server that is already answering. The round trip was never the
 * cost: the PARSE was. Every screen, the org builder among them, was one entry
 * module a browser had to download, parse and evaluate before it could draw
 * the sidebar — about a megabyte of script for a reader who came to look at
 * one inbox. A chunk per workspace makes the first paint pay for the frame and
 * the one screen asked for, and the idle prefetch below means the second
 * workspace a reader opens is usually already in memory, so the round trip
 * the old comment worried about is spent while nobody is waiting on it.
 *
 * # One chunk per workspace, plus the org editor
 *
 * Each `routes/<workspace>/index.ts` is the whole of what this file imports
 * from that workspace, and it is imported ONLY here, by `import()` — a static
 * import of a workspace's module from the frame would pull it back into the
 * entry without a sound. `routes/org` is the one chunk that is not a
 * workspace: the builder is a section of Agents, and it is bigger than any
 * whole workspace, so it is its own (see `routes/org/OrgEdit.tsx`).
 *
 * # A failed load is a ChunkLoadError, and it can be retried
 *
 * The usual reason a chunk does not load is that the engine was upgraded while
 * the tab was open: the page names chunks by their content hash, the new
 * binary serves different ones, and the old names are gone. So the rejection
 * becomes a [ChunkLoadError] carrying a sentence that says so and asks for a
 * reload, which is what fixes it — and what `app/boundaries.tsx` draws.
 *
 * The OTHER reason is a network that dropped one request, and that one a
 * retry fixes. `React.lazy` cannot give it one: it caches a rejection in the
 * component for the life of the page, so a screen whose chunk failed once
 * would stay failed through every navigation away and back. [lazyScreen]
 * therefore reads the chunk through React's `use` over the cache below, and
 * the cache FORGETS a rejected load — the next render of any screen in that
 * chunk asks the server again.
 */

import { createElement, use, type ComponentType } from "react";
import { WORKSPACES, type Workspace } from "./nav.ts";
import { resolve, type Resolved } from "./routes.ts";

/**
 * How each chunk is fetched. The `import()` literals are what the bundler
 * splits on, so each is spelled out rather than built from the key.
 */
const LOADERS = {
  home: () => import("~/routes/home/index.ts"),
  inbox: () => import("~/routes/inbox/index.ts"),
  me: () => import("~/routes/me/index.ts"),
  work: () => import("~/routes/work/index.ts"),
  agents: () => import("~/routes/agents/index.ts"),
  org: () => import("~/routes/org/index.ts"),
  live: () => import("~/routes/live/index.ts"),
  knowledge: () => import("~/routes/knowledge/index.ts"),
  spend: () => import("~/routes/spend/index.ts"),
  settings: () => import("~/routes/settings/index.ts"),
} satisfies Record<Workspace | "org", () => Promise<unknown>>;

/** A chunk: a workspace, or the org editor. */
export type Chunk = keyof typeof LOADERS;

/** What a chunk's `index.ts` exports. */
export type ChunkModule<C extends Chunk> = Awaited<ReturnType<(typeof LOADERS)[C]>>;

/** Every chunk, in the order the idle prefetch fetches them: the sidebar's. */
export const CHUNKS: readonly Chunk[] = [...WORKSPACES.map((ws) => ws.key), "org"];

/** What a chunk is called in a sentence a reader sees. */
export function chunkLabel(chunk: Chunk): string {
  if (chunk === "org") return "Edit org";
  return WORKSPACES.find((ws) => ws.key === chunk)?.label ?? chunk;
}

/** Which chunk draws a resolved screen. */
export function chunkOf(route: Resolved): Chunk {
  return route.screen === "org-edit" ? "org" : route.workspace;
}

/**
 * A chunk that did not load.
 *
 * ITS OWN CLASS, so a boundary can tell "this screen's code never arrived"
 * from "this screen threw on what it was given": the first is fixed by a
 * reload, the second by a report, and a fallback that said the same thing for
 * both would send half its readers the wrong way.
 */
export class ChunkLoadError extends Error {
  override name = "ChunkLoadError";
  readonly chunk: Chunk;
  /** What failed, naming the workspace: the fallback's heading. */
  readonly title: string;
  /** What to do about it: the fallback's sentence. */
  readonly advice: string;
  constructor(chunk: Chunk, cause: unknown) {
    const title = `The ${chunkLabel(chunk)} screens could not be loaded`;
    const advice =
      "This page is probably from another engine version — the engine was upgraded " +
      "since this tab opened — so reload it to get the current one.";
    super(`${title}. ${advice}`, { cause });
    this.chunk = chunk;
    this.title = title;
    this.advice = advice;
  }
}

/** The loads in flight or settled. A rejected load is removed — see the file's doc. */
const pending = new Map<Chunk, Promise<unknown>>();
/** The chunks that arrived, so a screen whose code is in memory never suspends. */
const loaded = new Map<Chunk, unknown>();

/** Fetch a chunk, once; a failed fetch is forgotten, so the next ask retries. */
export function loadChunk<C extends Chunk>(chunk: C): Promise<ChunkModule<C>> {
  let load = pending.get(chunk);
  if (!load) {
    load = (overrides.get(chunk) ?? LOADERS[chunk])().then(
      (module) => {
        loaded.set(chunk, module);
        return module;
      },
      (cause: unknown) => {
        pending.delete(chunk);
        throw new ChunkLoadError(chunk, cause);
      },
    );
    pending.set(chunk, load);
  }
  return load as Promise<ChunkModule<C>>;
}

/**
 * One screen out of a chunk, as a component that suspends until the chunk is
 * in. Rendered inside a `Suspense` (the fallback) and an error boundary (a
 * [ChunkLoadError]) — both are `app/boundaries.tsx`'s.
 *
 * A CHUNK ALREADY IN MEMORY IS READ SYNCHRONOUSLY. `use` on a promise React
 * has not seen suspends once even when it has settled, so a screen whose chunk
 * the idle prefetch fetched a minute ago would still flash its skeleton for a
 * frame; the `loaded` map is what spares it.
 */
export function lazyScreen<C extends Chunk, P extends object>(
  chunk: C,
  pick: (module: ChunkModule<C>) => ComponentType<P>,
): ComponentType<P> {
  function Lazy(props: P) {
    const module = (loaded.get(chunk) as ChunkModule<C> | undefined) ?? use(loadChunk(chunk));
    return createElement(pick(module), props);
  }
  Lazy.displayName = `Lazy(${chunk})`;
  return Lazy;
}

/**
 * Start fetching the chunk a path draws, and say nothing about how it went.
 *
 * Called when a row that leads there is hovered or focused, so the fetch runs
 * while the reader is deciding to click. A failure is swallowed HERE because
 * nobody asked to see anything yet — the cache has forgotten it, so the
 * navigation, if it comes, fetches again and reports what it finds.
 */
export function preload(path: string[]): void {
  const where = resolve(path);
  if (!where.resolved) return;
  loadChunk(chunkOf(where)).catch(() => {});
}

/** The browser's idle scheduler, or a timer where it has none (Safari, jsdom). */
function whenIdle(run: () => void): () => void {
  if (typeof requestIdleCallback === "function") {
    const id = requestIdleCallback(run, { timeout: IDLE_TIMEOUT_MS });
    return () => cancelIdleCallback(id);
  }
  const id = setTimeout(run, IDLE_FALLBACK_MS);
  return () => clearTimeout(id);
}

/**
 * How long an idle prefetch waits for a quiet moment before running anyway:
 * five seconds, because a page that is never idle — a live feed repainting
 * every second — would otherwise never prefetch at all, and five seconds is
 * past the first paint and the first round of reads on every screen measured.
 */
const IDLE_TIMEOUT_MS = 5_000;

/**
 * Where there is no idle scheduler, the gap before each chunk: long enough
 * that the first screen's own reads go first, short enough that the whole
 * set is in before a reader's second click.
 */
const IDLE_FALLBACK_MS = 1_000;

/**
 * Fetch every chunk, one at a time, each in its own idle moment.
 *
 * ONE AT A TIME so the prefetch never competes with itself for the socket's
 * connection or the main thread's parse, and each only when the browser says
 * it is idle, so it never delays what the reader is looking at. It stands
 * down for a reader who asked the browser to save data. Returns the cancel,
 * for the effect that started it.
 */
export function prefetchOnIdle(): () => void {
  const connection = (navigator as Navigator & { connection?: { saveData?: boolean } }).connection;
  if (connection?.saveData) return () => {};
  let cancel = () => {};
  let stopped = false;
  const next = (at: number) => {
    const chunk = CHUNKS[at];
    if (stopped || chunk === undefined) return;
    cancel = whenIdle(() => {
      loadChunk(chunk).then(
        () => next(at + 1),
        () => next(at + 1),
      );
    });
  };
  next(0);
  return () => {
    stopped = true;
    cancel();
  };
}

/** What a suite put in place of a chunk's `import()`. */
const overrides = new Map<Chunk, () => Promise<unknown>>();

/**
 * For a suite: fetch a chunk with this instead of its `import()` — a load that
 * fails, or one that is held until the suite lets it go. Forgets every chunk
 * already fetched, so the next render loads through the override. Returns the
 * undo.
 */
export function overrideChunkForTest(chunk: Chunk, load: () => Promise<unknown>): () => void {
  overrides.set(chunk, load);
  resetChunksForTest();
  return () => {
    overrides.delete(chunk);
    resetChunksForTest();
  };
}

/** For a suite: forget every chunk, so a load can be watched from the start. */
export function resetChunksForTest(): void {
  pending.clear();
  loaded.clear();
}
