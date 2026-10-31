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
 * questions: Company's rows draw screens from three directories (the
 * Overview, Policies and Integrations are `routes/settings`, the Org chart and
 * Schedules `routes/agents`, Budgets `routes/spend`), and a row that moves
 * moves no code. `routes/org` is the builder, bigger than any other directory,
 * so it is its own (see `routes/org/OrgEdit.tsx`). `routes/signin` is the
 * screens drawn outside the frame, which a reader who is signed in never
 * needs, so it is the one chunk the idle prefetch leaves alone ([CHUNKS]): a
 * reader who needs it is already on it. Each kind's create page
 * (`routes/artifacts/new/`) is a chunk of its own too, and neither is it
 * prefetched: a reader who opens Artifacts to look never needs an editor.
 *
 * The command palette (`palette/Palette.tsx`) is the one chunk of the frame's
 * own. It is a layer mounted only while open, as the New task sheet is, and
 * the largest single piece of the frame, so the first paint does not carry it.
 * Every screen opens it with a key, so it is the FIRST chunk the idle prefetch
 * fetches, and by the time a reader presses ⌘K it is usually in memory.
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
 * the cache FORGETS a rejected load — the next attempt at any screen in that
 * chunk asks the server again.
 *
 * # Forgotten once nobody can show it, not once it has failed
 *
 * WHEN it is forgotten is the whole of it, and it turns on whether a REGION
 * that can still show the failure read the load. A region is a boundary's
 * place on the page ([ChunkRegion], which every boundary in
 * `app/boundaries.tsx` that draws a lazy screen puts around its `Suspense`,
 * inside the error boundary that draws a failed chunk), and its ATTEMPT is one
 * stretch of it drawing one thing: it begins when the region is committed or
 * its key — the routed path, the peeked object — changes, and it ends when the
 * region goes, when its key changes, or when its error boundary draws a
 * failure in its place. A screen reading a load records its region's attempt
 * on it.
 *
 * A failed load is kept while an attempt that read it is still on, and
 * forgotten the moment none is. That covers each way a load can go:
 *
 * - One nobody was drawing — a hover's [preload], the idle prefetch — has no
 *   attempt, so it is forgotten as it fails: nobody was told, and the
 *   navigation that follows asks again.
 * - One a screen is drawing is kept, because React renders that screen
 *   several more times before the boundary draws anything — when the load
 *   settles, and once more when that render throws — and each of those
 *   renders reads the cache. Forgotten at the rejection, as this once did,
 *   every one of them found nothing and started a fresh `import()`: five
 *   requests for a chunk that was gone (seven under the whole frame), the
 *   reader told of the first while the others went on, and React's
 *   "suspended by an uncached promise" in the console. It is forgotten when the boundary draws it — the boundary's
 *   fallback takes the region's place, which ends its attempt — so the
 *   reader's next attempt (the boundary's Try again, a navigation, a layer
 *   opened again) is the one that asks again.
 * - One whose screen went before it failed — the reader left, or moved on to
 *   another path in the same region — is forgotten as it fails, or as the
 *   last attempt that read it ends: no boundary will ever draw it, so it is
 *   no answer to anybody's next attempt. Kept by a flag a render sets and
 *   nothing clears, it would be handed to every hover and prefetch after, and
 *   the reader coming back shown a failure nobody had asked about.
 * - One two regions read — a Work screen and the task peeked beside it, both
 *   out of the Work chunk — is kept until BOTH have drawn it or gone, so the
 *   second is not handed a fresh request by the first one's boundary.
 *
 * ONLY A COMMITTED ATTEMPT HOLDS. An attempt begins in a render, which React
 * may throw away; one it threw away never commits and so never ends, and
 * holding a failure for it would keep that failure for the life of the page.
 * Counting only what committed costs nothing in practice: a region commits
 * in the same task that rendered it, and an `import()` cannot fail before
 * that task ends.
 */

import {
  createContext,
  createElement,
  use,
  useContext,
  useLayoutEffect,
  useState,
  type ComponentType,
  type ReactNode,
} from "react";
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
  work: () => import("~/routes/work/index.ts"),
  code: () => import("~/routes/code/index.ts"),
  agents: () => import("~/routes/agents/index.ts"),
  org: () => import("~/routes/org/index.ts"),
  live: () => import("~/routes/live/index.ts"),
  knowledge: () => import("~/routes/knowledge/index.ts"),
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
} satisfies Record<
  ScreenDir | "org" | "signin" | NewArtifactChunk | "palette",
  () => Promise<unknown>
>;

/** A directory of framed screens under `routes/`, each one chunk. */
type ScreenDir =
  | "home"
  | "inbox"
  | "me"
  | "work"
  | "code"
  | "agents"
  | "live"
  | "knowledge"
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

/** A chunk: a workspace, the org editor, the sign-in screens, a create page or the palette. */
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
  "work-projects": "work",
  "work-tasks": "work",
  "work-views": "work",
  "work-history": "work",
  "work-search": "work",
  project: "work",
  item: "work",
  "code-changes": "code",
  "code-change": "code",
  "code-repos": "code",
  "code-pushes": "code",
  knowledge: "knowledge",
  container: "knowledge",
  page: "knowledge",
  "new-page": "knowledge",
  skills: "knowledge",
  diaries: "knowledge",
  diary: "knowledge",
  artifacts: "artifacts",
  "artifact-folders": "artifacts",
  "artifact-folder": "artifacts",
  artifact: "artifacts",
  "new-artifact": "artifacts",
  live: "live",
  turns: "live",
  turn: "live",
  runs: "live",
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
 * palette, then the sidebar's from the top. NOT `signin` or a create page;
 * see the note above.
 */
export const CHUNKS: readonly Chunk[] = [
  "palette",
  "home",
  "inbox",
  "me",
  "settings",
  "brand",
  "agents",
  "spend",
  "work",
  "code",
  "knowledge",
  "artifacts",
  "live",
  "account",
  "org",
];

/**
 * What a chunk is called in a sentence a reader sees: the places it draws,
 * since a directory's name is nobody's word for anything.
 */
const CHUNK_LABELS: Readonly<Record<ScreenDir | "org" | "signin" | "palette", string>> = {
  home: "Home",
  inbox: "Inbox",
  me: "My work",
  work: "Work",
  code: "Code",
  agents: "Org chart",
  live: "Live",
  knowledge: "Knowledge",
  artifacts: "Artifacts",
  brand: "Brand",
  spend: "Spend and Budgets",
  settings: "Company and Settings",
  account: "Account",
  org: "Edit org",
  signin: "sign-in",
  palette: "Search",
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
      chunk === "palette"
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
type Thenable<T> = Promise<T> & { status?: "fulfilled"; value?: T };

/** One fetch of a chunk, as the cache holds it. */
interface Load {
  readonly chunk: Chunk;
  readonly promise: Thenable<unknown>;
  /** What it failed with, once it has: whether an ending attempt forgets it. */
  failure?: ChunkLoadError;
  /**
   * The attempts a screen read it in while it had not arrived: what decides
   * when its failure is forgotten. Emptied once it arrives, since an arrived
   * load is never forgotten and an attempt is then no concern of it.
   */
  readonly attempts: Set<Attempt>;
}

/**
 * One stretch of a region drawing one thing — see the file's doc. Live from
 * its region's commit until the region goes, changes its key or draws a
 * failure in its place. A StrictMode remount takes it down and up again, so
 * ending is not final, and nothing is taken off the loads that recorded it.
 */
class Attempt {
  live = false;
  /** The loads a screen in it read that have not arrived. */
  readonly loads = new Set<Load>();
}

/**
 * The loads in flight or settled. A rejected one is removed once no live
 * attempt read it — see the file's doc.
 */
const pending = new Map<Chunk, Load>();

/** Whether a load was read by an attempt that can still show its failure. */
function held(load: Load): boolean {
  for (const attempt of load.attempts) if (attempt.live) return true;
  return false;
}

/**
 * Drop a load from the cache, and from every attempt that recorded it. A
 * failure the cache has already replaced with a newer load leaves that newer
 * load where it is.
 */
function forget(load: Load): void {
  if (pending.get(load.chunk) === load) pending.delete(load.chunk);
  for (const attempt of load.attempts) attempt.loads.delete(load);
  load.attempts.clear();
}

/** The cache's load of a chunk, begun if there is none. */
function begin(chunk: Chunk): Load {
  const kept = pending.get(chunk);
  if (kept) return kept;
  const promise: Thenable<unknown> = (overrides.get(chunk) ?? LOADERS[chunk])().then(
    (module) => {
      promise.status = "fulfilled";
      promise.value = module;
      for (const attempt of load.attempts) attempt.loads.delete(load);
      load.attempts.clear();
      return module;
    },
    (cause: unknown) => {
      const failure = new ChunkLoadError(chunk, cause);
      load.failure = failure;
      // NO ATTEMPT THAT COULD SHOW IT is on, so nobody will be told:
      // forgotten now, and the next ask fetches again. One a live attempt
      // read waits for that attempt to end ([ChunkRegion]).
      if (!held(load)) forget(load);
      throw failure;
    },
  );
  const load: Load = { chunk, promise, attempts: new Set() };
  pending.set(chunk, load);
  return load;
}

/**
 * Fetch a chunk, once. A failure no region can show is forgotten as it
 * happens, so the next ask fetches again; one a region is drawing is
 * forgotten once that region has drawn it or gone ([ChunkRegion]).
 */
export function loadChunk<C extends Chunk>(chunk: C): Promise<ChunkModule<C>> {
  return begin(chunk).promise as Promise<ChunkModule<C>>;
}

/** The attempt of the region a lazy screen is drawn in. */
const RegionContext = createContext<Attempt | null>(null);

/**
 * A region that draws lazy screens: what a boundary puts around its
 * `Suspense`, INSIDE the error boundary that draws a [ChunkLoadError], so
 * that drawing the failure ends the attempt that read it. `resetKey` is the
 * boundary's own — the routed path, the peeked object — and a new one is a
 * new attempt, as it is a new screen to the boundary. See the file's doc for
 * what an attempt decides.
 */
export function ChunkRegion({ resetKey, children }: { resetKey?: string; children: ReactNode }) {
  const [current, setCurrent] = useState(() => ({ key: resetKey, attempt: new Attempt() }));
  // A NEW KEY IS A NEW ATTEMPT, taken during the render — React's own pattern
  // for state that follows a prop: React renders this again at once with the
  // new attempt, before any screen below has read the old one.
  let attempt = current.attempt;
  if (current.key !== resetKey) {
    attempt = new Attempt();
    setCurrent({ key: resetKey, attempt });
  }
  // A LAYOUT EFFECT, so the attempt is live in the task that committed it: a
  // passive one could run after the load had failed, and a failure no live
  // attempt holds is forgotten while its screen still reads it.
  useLayoutEffect(() => {
    attempt.live = true;
    return () => {
      attempt.live = false;
      for (const load of attempt.loads) {
        if (load.failure !== undefined && !held(load)) forget(load);
      }
    };
  }, [attempt]);
  return createElement(RegionContext.Provider, { value: attempt }, children);
}

/**
 * One screen out of a chunk, as a component that suspends until the chunk is
 * in. Rendered inside a [ChunkRegion], a `Suspense` (the fallback) and an
 * error boundary (a [ChunkLoadError]) — all three are `app/boundaries.tsx`'s
 * — and one drawn outside a region is refused, since nothing could then say
 * when its failure is forgotten.
 *
 * A CHUNK ALREADY IN MEMORY IS READ SYNCHRONOUSLY. `use` on a promise React
 * has not seen suspends once even when it has settled, so a screen whose chunk
 * the idle prefetch fetched a minute ago would still flash its skeleton for a
 * frame; the load marking itself fulfilled ([Thenable]) is what spares it.
 *
 * `use` IS CALLED ON EVERY RENDER. A component that suspended on it must call
 * it again on the render that resumes, and a fast path that skipped it once
 * the chunk was in (a map of arrived modules read before `use`) did exactly
 * that: every screen that suspended logged React's "did not call use() when
 * it finished" on the render that drew it.
 *
 * A CHUNK IT HAS TO WAIT FOR has its region's attempt recorded on the load
 * before React reads it, so a failure is kept for every render React makes of
 * this screen until its boundary has drawn it.
 */
export function lazyScreen<C extends Chunk, P extends object>(
  chunk: C,
  pick: (module: ChunkModule<C>) => ComponentType<P>,
): ComponentType<P> {
  function Lazy(props: P) {
    const attempt = useContext(RegionContext);
    if (attempt === null) {
      throw new Error(
        `Lazy(${chunk}) was drawn outside a ChunkRegion: draw it inside one of ` +
          "app/boundaries.tsx's boundaries, which say when a failed load is forgotten.",
      );
    }
    const module = use(drawing(chunk, attempt));
    return createElement(pick(module), props);
  }
  Lazy.displayName = `Lazy(${chunk})`;
  return Lazy;
}

/** A chunk's load as a screen reads it: recorded on the reading attempt until it arrives. */
function drawing<C extends Chunk>(chunk: C, attempt: Attempt): Promise<ChunkModule<C>> {
  const load = begin(chunk);
  if (load.promise.status !== "fulfilled") {
    load.attempts.add(attempt);
    attempt.loads.add(load);
  }
  return load.promise as Promise<ChunkModule<C>>;
}

/**
 * Start fetching the chunk a path draws, and say nothing about how it went.
 *
 * Called when a row that leads there is hovered or focused, so the fetch runs
 * while the reader is deciding to click. A failure is swallowed HERE because
 * nobody asked to see anything yet — no region read it, so the cache has
 * forgotten it, and the navigation, if it comes, fetches again and reports
 * what it finds.
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
 * fails, or one that is held until the suite lets it go. Forgets that chunk if
 * it was already fetched, so the next render loads through the override, and
 * forgets it again on the undo, so the next render loads the real one. Returns
 * the undo.
 *
 * THAT CHUNK AND NO OTHER. Forgetting every chunk sent each one the frame
 * draws besides — the palette a case opens with ⌘K, a peek beside the screen
 * — back to a cold `import()` under the test transformer, which a loaded
 * machine stretches past a wait's bound, so a case about a broken Spend screen
 * failed on the palette it never broke.
 */
export function overrideChunkForTest(chunk: Chunk, load: () => Promise<unknown>): () => void {
  overrides.set(chunk, load);
  forgetForTest(chunk);
  return () => {
    overrides.delete(chunk);
    forgetForTest(chunk);
  };
}

/** For [overrideChunkForTest]: forget one chunk's load, whatever state it is in. */
function forgetForTest(chunk: Chunk): void {
  const load = pending.get(chunk);
  if (load) forget(load);
}

/** For a suite: forget every chunk, so a load can be watched from the start. */
export function resetChunksForTest(): void {
  pending.clear();
}
