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
 * # One chunk per workspace, plus the org editor and the console's frame
 *
 * Each workspace's `index.ts` — `routes/<workspace>/` in the person shell,
 * `routes/admin/<workspace>/` in the console — is the whole of what this file
 * imports from that workspace, and it is imported ONLY here, by `import()`: a
 * static import of a workspace's module from the frame would pull it back
 * into the entry without a sound. Two chunks are not workspaces:
 * `routes/admin/org`, because the builder is a section of Company and bigger
 * than any whole workspace (see `routes/admin/org/OrgEdit.tsx`), and
 * `app/console`, the console's own sidebar and the door into it, because the
 * frame draws them and a member must not download them.
 *
 * # A member's browser never fetches the console
 *
 * The chunks are listed PER SHELL ([chunksOf]), and the idle prefetch walks
 * the person shell's for everybody and the console's only for a reader the
 * engine has said is an admin — so a member's browser downloads no console
 * code, by hover, by idle time or by a mistyped address (which draws the
 * frame's refusal rather than a console screen). Code is not the boundary —
 * the engine refuses what a member's key does not reach whatever a browser
 * holds (ADR-0031) — but a member's tab parsing a megabyte of screens it may
 * never open is a cost they pay for nothing.
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
import { WORKSPACES, workspaceRow, type Shell, type Workspace } from "./nav.ts";
import { resolve, type Resolved } from "./routes.ts";

/**
 * How each chunk is fetched. The `import()` literals are what the bundler
 * splits on, so each is spelled out rather than built from the key.
 */
const LOADERS = {
  today: () => import("~/routes/today/index.ts"),
  inbox: () => import("~/routes/inbox/index.ts"),
  me: () => import("~/routes/me/index.ts"),
  work: () => import("~/routes/work/index.ts"),
  team: () => import("~/routes/team/index.ts"),
  knowledge: () => import("~/routes/knowledge/index.ts"),
  overview: () => import("~/routes/admin/overview/index.ts"),
  live: () => import("~/routes/admin/live/index.ts"),
  setup: () => import("~/routes/admin/agents/index.ts"),
  spend: () => import("~/routes/admin/spend/index.ts"),
  company: () => import("~/routes/admin/company/index.ts"),
  org: () => import("~/routes/admin/org/index.ts"),
  connect: () => import("~/routes/admin/connect/index.ts"),
  engine: () => import("~/routes/admin/engine/index.ts"),
  console: () => import("~/app/console/index.ts"),
} satisfies Record<Workspace | "org" | "console", () => Promise<unknown>>;

/** A chunk: a workspace, the org editor, or the console's frame. */
export type Chunk = keyof typeof LOADERS;

/** What a chunk's `index.ts` exports. */
export type ChunkModule<C extends Chunk> = Awaited<ReturnType<(typeof LOADERS)[C]>>;

/**
 * Every chunk, in the order the idle prefetch fetches them: the sidebars',
 * with the console's own frame first among its chunks, since an admin's first
 * crossing draws it before any console screen.
 */
export const CHUNKS: readonly Chunk[] = [
  ...WORKSPACES.filter((ws) => ws.shell === "person").map((ws) => ws.key),
  "console",
  ...WORKSPACES.filter((ws) => ws.shell === "admin").map((ws) => ws.key),
  "org",
];

/** The shell a chunk's code is drawn in: the org editor and the frame chunk are the console's. */
export function shellOfChunk(chunk: Chunk): Shell {
  if (chunk === "org" || chunk === "console") return "admin";
  return workspaceRow(chunk)?.shell ?? "person";
}

/** One shell's chunks, in the sidebar's order — [CHUNKS] narrowed. */
export function chunksOf(shell: Shell): Chunk[] {
  return CHUNKS.filter((chunk) => shellOfChunk(chunk) === shell);
}

/** What a chunk is called in a sentence a reader sees. */
export function chunkLabel(chunk: Chunk): string {
  if (chunk === "org") return "Edit org";
  if (chunk === "console") return "admin console";
  return WORKSPACES.find((ws) => ws.key === chunk)?.label ?? chunk;
}

/**
 * Which chunk draws a resolved screen — or null for the landing, which the
 * frame turns into an address before anything is drawn.
 */
export function chunkOf(route: Resolved): Exclude<Chunk, "console"> | null {
  if (route.screen === "org-edit") return "org";
  return route.workspace === "" ? null : route.workspace;
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
  const chunk = chunkOf(where);
  if (chunk) loadChunk(chunk).catch(() => {});
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
 * Fetch every chunk of the shells this reader may open, one at a time, each in
 * its own idle moment.
 *
 * ONE AT A TIME so the prefetch never competes with itself for the socket's
 * connection or the main thread's parse, and each only when the browser says
 * it is idle, so it never delays what the reader is looking at. It stands
 * down for a reader who asked the browser to save data. `shells` is the
 * person shell for everybody and the console too for an admin — see the
 * file's doc. Returns the cancel, for the effect that started it.
 */
export function prefetchOnIdle(shells: readonly Shell[]): () => void {
  const chunks = shells.flatMap((shell) => chunksOf(shell));
  const connection = (navigator as Navigator & { connection?: { saveData?: boolean } }).connection;
  if (connection?.saveData) return () => {};
  let cancel = () => {};
  let stopped = false;
  const next = (at: number) => {
    const chunk = chunks[at];
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
