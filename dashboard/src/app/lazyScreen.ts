/**
 * The screens, one chunk per screen directory, loaded when they are first needed.
 *
 * # Why the screens are split at all
 *
 * The flat import list this replaced argued that a chunk buys a round trip
 * against a server that is already answering. The round trip was never the
 * cost: the PARSE was. Every screen, the org builder among them, was one entry
 * module a browser had to download, parse and evaluate before it could draw
 * the sidebar — about a megabyte of script for a reader who came to look at
 * one inbox. A chunk per screen directory makes the first paint pay for the
 * frame and the one screen asked for, and the idle prefetch below means the
 * second place a reader opens is usually already in memory, so the round trip
 * the old comment worried about is spent while nobody is waiting on it.
 *
 * # One chunk per screen directory, decided by the SCREEN
 *
 * Each `routes/<dir>/index.ts` is the whole of what this file imports from
 * that directory, and it is imported ONLY here, by `import()`: a static
 * import of one from the frame would pull it back into the entry without a
 * sound. Which chunk draws a screen is [SCREEN_CHUNKS], keyed by the screen
 * rather than by the sidebar row it is in, because the two are different
 * questions: Company's rows draw screens from four directories (Principles
 * and Integrations are `routes/settings`, Brand `routes/brand`, the Org
 * chart and Schedules `routes/agents`, Budgets `routes/spend`), and a row
 * that moves moves no
 * code. `routes/org` is the builder, bigger than any other directory,
 * so it is its own (see `routes/org/OrgEdit.tsx`). `routes/signin` is the
 * screens drawn outside the frame, which a reader who is signed in never
 * needs, so it is the one chunk the idle prefetch leaves alone ([CHUNKS]): a
 * reader who needs it is already on it. Each kind's create page
 * (`routes/artifacts/new/`) is a chunk of its own too, and neither is it
 * prefetched: a reader who opens Artifacts to look never needs an editor.
 *
 * The command palette (`palette/Palette.tsx`) and the assistant's panel
 * (`assistant/chunk/`) are the frame's own chunks. Each is mounted only while
 * open, as the New task sheet is, and each is a large piece of the frame, so
 * the first paint carries neither: the entry keeps only the Ask button, the
 * page context registry, the bus and the one-word status the button reads.
 * Every screen opens them with a key, so they are the FIRST chunks the idle
 * prefetch fetches, and by the time a reader presses ⌘K or ⌘. they are
 * usually in memory.
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
import { NEW_KIND_TITLES } from "~/lib/artifacts/words.ts";
import type { MakeKind } from "~/lib/artifacts/words.ts";
import { resolve, type FramedScreen, type Resolved } from "./routes.ts";

/**
 * How each chunk is fetched. The `import()` literals are what the bundler
 * splits on, so each is spelled out rather than built from the key.
 */
const LOADERS = {
  home: () => import("~/routes/home/index.ts"),
  inbox: () => import("~/routes/inbox/index.ts"),
  me: () => import("~/routes/me/index.ts"),
  worklet: () => import("~/routes/worklet/index.ts"),
  agents: () => import("~/routes/agents/index.ts"),
  org: () => import("~/routes/org/index.ts"),
  chatlet: () => import("~/routes/chatlet/index.ts"),
  live: () => import("~/routes/live/index.ts"),
  doclet: () => import("~/routes/doclet/index.ts"),
  artifacts: () => import("~/routes/artifacts/index.ts"),
  brand: () => import("~/routes/brand/index.ts"),
  spend: () => import("~/routes/spend/index.ts"),
  settings: () => import("~/routes/settings/index.ts"),
  account: () => import("~/routes/account/index.ts"),
  signin: () => import("~/routes/signin/index.ts"),
  "new-doc": () => import("~/routes/artifacts/new/DocCreate.tsx"),
  "new-slides": () => import("~/routes/artifacts/new/SlidesCreate.tsx"),
  "new-sheet": () => import("~/routes/artifacts/new/SheetCreate.tsx"),
  "new-design": () => import("~/routes/artifacts/new/DesignCreate.tsx"),
  "new-dashboard": () => import("~/routes/artifacts/new/DashboardCreate.tsx"),
  "new-html": () => import("~/routes/artifacts/new/HtmlCreate.tsx"),
  palette: () => import("./palette/Palette.tsx"),
  assistant: () => import("./assistant/chunk/index.ts"),
} satisfies Record<
  ScreenDir | "org" | "signin" | NewArtifactChunk | "palette" | "assistant",
  () => Promise<unknown>
>;

/** A directory of framed screens under `routes/`, each one chunk. */
type ScreenDir =
  | "home"
  | "inbox"
  | "me"
  | "worklet"
  | "chatlet"
  | "agents"
  | "live"
  | "doclet"
  | "artifacts"
  | "brand"
  | "spend"
  | "settings"
  | "account";

/**
 * A kind's create page, a chunk of its own: a doc's editor, a deck's composer
 * and a design's canvas share nothing but their frame, and the gallery's
 * reader should download none of them until they start one.
 */
type NewArtifactChunk = `new-${MakeKind}`;

/** A chunk: a workspace, the org editor, the sign-in screens, a create page, the palette or the assistant. */
export type Chunk = keyof typeof LOADERS;

/** What a chunk's `index.ts` exports. */
export type ChunkModule<C extends Chunk> = Awaited<ReturnType<(typeof LOADERS)[C]>>;

/**
 * The chunk each framed screen is drawn from. EXHAUSTIVE over the route
 * table's screens, so a screen added to `routes.ts` and not placed here is a
 * compile error rather than a page that never loads. A kind's create page is
 * decided by its kind ([chunkOf]).
 */
const SCREEN_CHUNKS: Readonly<Record<FramedScreen["screen"], Chunk>> = {
  home: "home",
  inbox: "inbox",
  me: "me",
  principles: "settings",
  brand: "brand",
  "brand-setup": "brand",
  "org-chart": "agents",
  roster: "agents",
  teams: "agents",
  "org-edit": "org",
  seat: "agents",
  schedules: "agents",
  integrations: "settings",
  budgets: "spend",
  "worklet-projects": "worklet",
  "worklet-tasks": "worklet",
  "worklet-views": "worklet",
  "worklet-history": "worklet",
  "worklet-search": "worklet",
  project: "worklet",
  item: "worklet",
  chatlet: "chatlet",
  "chatlet-activity": "chatlet",
  "chatlet-threads": "chatlet",
  "chatlet-drafts": "chatlet",
  "chatlet-saved": "chatlet",
  "chatlet-search": "chatlet",
  "chatlet-browse": "chatlet",
  "chatlet-new": "chatlet",
  doclet: "doclet",
  container: "doclet",
  page: "doclet",
  "new-page": "doclet",
  skills: "doclet",
  diaries: "doclet",
  diary: "doclet",
  artifacts: "artifacts",
  "artifact-folders": "artifacts",
  "artifact-folder": "artifacts",
  artifact: "artifacts",
  "new-artifact": "artifacts",
  live: "live",
  turns: "live",
  turn: "live",
  executors: "live",
  a2a: "live",
  trace: "live",
  events: "live",
  event: "live",
  models: "settings",
  "mcp-tools": "settings",
  secrets: "settings",
  limits: "settings",
  spend: "spend",
  "spend-tasks": "spend",
  people: "settings",
  "service-accounts": "settings",
  tokens: "settings",
  "sign-in": "settings",
  audit: "settings",
  nodes: "settings",
  config: "settings",
  backups: "settings",
  storage: "settings",
  account: "account",
};

/**
 * Every chunk the idle prefetch fetches, in the order it fetches them: the
 * palette, the assistant, then the sidebar's from the top. NOT `signin` or a create page;
 * see the note above.
 */
export const CHUNKS: readonly Chunk[] = [
  "palette",
  "assistant",
  "home",
  "inbox",
  "me",
  "settings",
  "brand",
  "agents",
  "spend",
  "worklet",
  "chatlet",
  "doclet",
  "artifacts",
  "live",
  "account",
  "org",
];

/**
 * What a chunk is called in a sentence a reader sees: the places it draws,
 * since a directory's name is nobody's word for anything.
 */
const CHUNK_LABELS: Readonly<
  Record<ScreenDir | "org" | "signin" | "palette" | "assistant", string>
> = {
  home: "Home",
  inbox: "Inbox",
  me: "My work",
  worklet: "Worklet",
  chatlet: "Chatlet",
  agents: "Org chart",
  live: "Live",
  doclet: "Doclet",
  artifacts: "Artifacts",
  brand: "Brand",
  spend: "Spend and Budgets",
  settings: "Company and Settings",
  account: "Account",
  org: "Edit org",
  signin: "sign-in",
  palette: "Search",
  assistant: "Ask Crewlet",
};

/** What a chunk is called in a sentence a reader sees. */
export function chunkLabel(chunk: Chunk): string {
  if (chunk.startsWith("new-")) return NEW_KIND_TITLES[chunk.slice(4) as MakeKind];
  return CHUNK_LABELS[chunk as keyof typeof CHUNK_LABELS];
}

/** Which chunk draws a resolved screen. */
export function chunkOf(route: Resolved): Chunk {
  if (route.workspace === "") return "signin";
  if (route.screen === "new-artifact" && route.kind) return `new-${route.kind}`;
  return SCREEN_CHUNKS[route.screen];
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
  /** What failed, naming the places it draws: the fallback's heading. */
  readonly title: string;
  /** What to do about it: the fallback's sentence. */
  readonly advice: string;
  constructor(chunk: Chunk, cause: unknown) {
    const title =
      chunk === "palette" || chunk === "assistant"
        ? `${chunkLabel(chunk)} could not be loaded`
        : chunk.startsWith("new-")
          ? `The ${chunkLabel(chunk)} page could not be loaded`
          : `The ${chunkLabel(chunk)} screens could not be loaded`;
    const advice = "The engine was updated. Reload to continue.";
    super(`${title}. ${advice}`, { cause });
    this.chunk = chunk;
    this.title = title;
    this.advice = advice;
  }
}

/**
 * A load as React's `use` reads it: a promise that marks itself once its chunk
 * is in. React reads a thenable's `status` before it suspends, so one already
 * marked `fulfilled` is answered at once, while one React has not seen is
 * suspended on for a frame even when it has settled.
 */
type Load<T> = Promise<T> & { status?: "fulfilled"; value?: T };

/** The loads in flight or settled. A rejected load is removed: see the file's doc. */
const pending = new Map<Chunk, Load<unknown>>();

/** Fetch a chunk, once; a failed fetch is forgotten, so the next ask retries. */
export function loadChunk<C extends Chunk>(chunk: C): Promise<ChunkModule<C>> {
  let load = pending.get(chunk);
  if (!load) {
    const started: Load<unknown> = (overrides.get(chunk) ?? LOADERS[chunk])().then(
      (module) => {
        started.status = "fulfilled";
        started.value = module;
        return module;
      },
      (cause: unknown) => {
        pending.delete(chunk);
        throw new ChunkLoadError(chunk, cause);
      },
    );
    pending.set(chunk, started);
    load = started;
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
 * frame; the load marking itself fulfilled ([Load]) is what spares it.
 *
 * `use` IS CALLED ON EVERY RENDER. A component that suspended on it must call
 * it again on the render that resumes, and a fast path that skipped it once
 * the chunk was in (a map of arrived modules read before `use`) did exactly
 * that: every screen that suspended logged React's "did not call use() when
 * it finished" on the render that drew it.
 */
export function lazyScreen<C extends Chunk, P extends object>(
  chunk: C,
  pick: (module: ChunkModule<C>) => ComponentType<P>,
): ComponentType<P> {
  function Lazy(props: P) {
    const module = use(loadChunk(chunk));
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
}
