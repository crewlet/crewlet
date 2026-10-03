/**
 * The screens arrive as chunks, and every way that can go is a sentence.
 *
 * What this holds: which chunk draws which screen, that a failed load says
 * what fixes it and can be retried rather than failing for the life of the
 * tab, that a hover or a focus starts the fetch, and that the idle prefetch
 * walks every chunk once, one at a time, and stops when told to. The frame's
 * half — what a reader sees while a chunk loads or when it never comes — is
 * `boundaries.test.tsx`.
 */

import { act, cleanup, fireEvent, render, screen } from "~/test/inCase.ts";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import {
  CHUNKS,
  ChunkLoadError,
  chunkOf,
  lazyScreen,
  loadChunk,
  overrideChunkForTest,
  prefetchOnIdle,
  preload,
  resetChunksForTest,
  type Chunk,
} from "./lazyScreen.ts";
import { WORKSPACES } from "./nav.ts";
import { DESTINATIONS } from "./nav.ts";
import { resolve } from "./routes.ts";
import { ScreenBoundary } from "./boundaries.tsx";
import { App } from "./App.tsx";
import { Router } from "./router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { modules, parse, stringValue, walk } from "~/test/source.ts";

let undo: (() => void)[] = [];

/** Fetch a chunk through this instead of its `import()`, until the case ends. */
function serve(chunk: Chunk, load: () => Promise<unknown>) {
  undo.push(overrideChunkForTest(chunk, load));
}

/** A load the case lets go of when it chooses. */
function held() {
  let release: (module: unknown) => void = () => {};
  let fail: (error: unknown) => void = () => {};
  const promise = new Promise<unknown>((ok, no) => {
    release = ok;
    fail = no;
  });
  return { promise, release, fail };
}

beforeEach(() => {
  resetChunksForTest();
});

afterEach(() => {
  cleanup();
  for (const u of undo.reverse()) u();
  undo = [];
  vi.useRealTimers();
});

describe("which chunk draws a screen", () => {
  test("every workspace is a chunk, and the org editor is one of its own", () => {
    expect([...CHUNKS].sort()).toEqual([...WORKSPACES.map((ws) => ws.key), "org"].sort());
  });

  test("a screen is drawn by its workspace's chunk, the builder by the org chunk", () => {
    const edit = resolve(["agents", "edit"]);
    const chart = resolve(["agents"]);
    const item = resolve(["work", "ENG-4"]);
    expect(edit.resolved && chunkOf(edit)).toBe("org");
    expect(chart.resolved && chunkOf(chart)).toBe("agents");
    expect(item.resolved && chunkOf(item)).toBe("work");
  });

  // THE SIGN-IN SCREENS ARE A CHUNK OF THEIR OWN, and the one the idle
  // prefetch leaves alone: a reader who is signed in never needs it, and a
  // reader who does is already on it. Its own screens and the user block's
  // two proof dialogs are all it exports — a static import of any of them
  // from the frame would put the sign-in code back in every reader's entry.
  test("a sign-in screen is drawn by the sign-in chunk, which nothing prefetches", async () => {
    for (const path of [["login"], ["enrol"], ["invite", "id.secret"]]) {
      const where = resolve(path);
      expect(where.resolved && chunkOf(where), path.join("/")).toBe("signin");
    }
    expect(CHUNKS).not.toContain("signin");
    const module = await loadChunk("signin");
    expect(Object.keys(module).sort()).toEqual(
      ["AuthenticatorDialog", "Enrol", "Invite", "RecoveryCodesDialog", "SignIn"].sort(),
    );
  });

  // Every place the sidebar and the palette can send a reader has a chunk that
  // loads — a `routes/<workspace>/index.ts` that stopped exporting a screen
  // would compile (the dispatch picks by name) and fail here.
  test("every destination's chunk loads and exports a screen", async () => {
    for (const d of DESTINATIONS) {
      const where = resolve(d.path);
      if (!where.resolved) continue;
      const module = await loadChunk(chunkOf(where));
      expect(Object.keys(module).length, d.path.join("/")).toBeGreaterThan(0);
    }
  });
});

describe("a chunk that does not load", () => {
  test("is a ChunkLoadError that names the workspace and says to reload", async () => {
    serve("spend", () =>
      Promise.reject(new TypeError("Failed to fetch dynamically imported module")),
    );
    const error = await loadChunk("spend").catch((e: unknown) => e);
    expect(error).toBeInstanceOf(ChunkLoadError);
    const message = (error as ChunkLoadError).message;
    expect(message).toMatch(/^The Spend screens could not be loaded\./);
    expect(message).toContain("from another engine version");
    expect(message).toMatch(/reload/);
    // The cause survives for whoever reads the console.
    expect((error as ChunkLoadError).cause).toBeInstanceOf(TypeError);
  });

  // `React.lazy` keeps a rejection for the life of the page; this cache must
  // not, or a dropped request would fail the workspace until a reload.
  test("is forgotten, so the next ask fetches again", async () => {
    let calls = 0;
    serve("spend", () => {
      calls++;
      return calls === 1
        ? Promise.reject(new Error("dropped"))
        : Promise.resolve({ Spend: () => null });
    });
    await expect(loadChunk("spend")).rejects.toBeInstanceOf(ChunkLoadError);
    await expect(loadChunk("spend")).resolves.toHaveProperty("Spend");
    expect(calls).toBe(2);
  });

  // REACT DELIVERS A REJECTION BY REPLAYING THE RENDER THAT SUSPENDED, and the
  // replay asks the cache again. Forgotten as it failed, the load was fetched
  // a second time for nobody — and React, handed a promise it had never seen,
  // said the screen made an uncached promise. Kept until the boundary has
  // drawn it, it is one request; drawn, Try again really asks again.
  test("a screen waited on is one request, and forgotten once its failure is drawn", async () => {
    // React reports the failure its boundary caught, which this case causes on
    // purpose — and nothing else may be said (`test/console.ts`).
    const said: unknown[][] = [];
    const quiet = vi.spyOn(console, "error").mockImplementation((...args) => said.push(args));
    let calls = 0;
    let fail = true;
    serve("spend", () => {
      calls++;
      return fail
        ? Promise.reject(new TypeError("Failed to fetch dynamically imported module"))
        : Promise.resolve({ Spend: () => <p>the spend screen</p> });
    });
    const Spend = lazyScreen("spend", (m) => m.Spend);
    await act(async () => {
      render(
        <ScreenBoundary resetKey="spend">
          <Spend />
        </ScreenBoundary>,
      );
    });
    expect(await screen.findByText("The Spend screens could not be loaded")).toBeDefined();
    expect(calls).toBe(1);
    fail = false;
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    });
    expect(await screen.findByText("the spend screen")).toBeDefined();
    expect(calls).toBe(2);
    quiet.mockRestore();
    expect(said.map((args) => args.find((a) => a instanceof ChunkLoadError))).toEqual([
      expect.any(ChunkLoadError),
    ]);
  });

  test("while a load that arrived is fetched once, however often it is asked", async () => {
    let calls = 0;
    serve("spend", () => {
      calls++;
      return Promise.resolve({ Spend: () => null });
    });
    await Promise.all([loadChunk("spend"), loadChunk("spend"), loadChunk("spend")]);
    await loadChunk("spend");
    expect(calls).toBe(1);
  });
});

describe("a lazy screen", () => {
  test("draws the skeleton until its chunk is in, then the screen", async () => {
    const load = held();
    serve("spend", () => load.promise);
    const Spend = lazyScreen("spend", (m) => m.Spend);
    // AWAITED, as every render that can suspend has to be under act: React
    // retries a suspended render from act's queue, so a synchronous act never
    // sees the retry.
    await act(async () => {
      render(
        <ScreenBoundary resetKey="spend">
          <Spend />
        </ScreenBoundary>,
      );
    });
    expect(screen.getByText("Loading this screen")).toBeDefined();
    await act(async () => load.release({ Spend: () => <p>the spend screen</p> }));
    expect(await screen.findByText("the spend screen")).toBeDefined();
    expect(screen.queryByText("Loading this screen")).toBeNull();
  });

  // A chunk the idle prefetch already fetched must not flash its skeleton:
  // `use` suspends once on a promise React has not seen, settled or not.
  test("whose chunk is already in draws at once, with no skeleton", async () => {
    serve("spend", () => Promise.resolve({ Spend: () => <p>the spend screen</p> }));
    await loadChunk("spend");
    const Spend = lazyScreen("spend", (m) => m.Spend);
    render(
      <ScreenBoundary resetKey="spend">
        <Spend />
      </ScreenBoundary>,
    );
    expect(screen.getByText("the spend screen")).toBeDefined();
  });
});

describe("preload", () => {
  test("starts the chunk a path draws, and nothing for a path with no screen", async () => {
    const asked: Chunk[] = [];
    for (const chunk of CHUNKS) {
      serve(chunk, () => {
        asked.push(chunk);
        return Promise.resolve({});
      });
    }
    preload(["agents", "edit"]);
    preload(["settings", "secrets"]);
    preload(["nowhere", "at", "all"]);
    await Promise.resolve();
    expect(asked).toEqual(["org", "settings"]);
  });

  test("swallows a failure, which the navigation will report", async () => {
    serve("spend", () => Promise.reject(new Error("offline")));
    const unhandled = vi.fn();
    process.on("unhandledRejection", unhandled);
    preload(["spend"]);
    await new Promise((r) => setTimeout(r, 0));
    process.off("unhandledRejection", unhandled);
    expect(unhandled).not.toHaveBeenCalled();
  });
});

describe("the idle prefetch", () => {
  function recording() {
    const asked: Chunk[] = [];
    const loads = new Map<Chunk, ReturnType<typeof held>>();
    for (const chunk of CHUNKS) {
      serve(chunk, () => {
        asked.push(chunk);
        const load = held();
        loads.set(chunk, load);
        return load.promise;
      });
    }
    return { asked, loads };
  }

  test("fetches every chunk, in the sidebar's order, one at a time", async () => {
    vi.useFakeTimers();
    const { asked, loads } = recording();
    const stop = prefetchOnIdle();
    await vi.advanceTimersByTimeAsync(5_000);
    // ONE AT A TIME: the second waits for the first to settle.
    expect(asked).toEqual([CHUNKS[0]]);
    for (const chunk of CHUNKS) {
      loads.get(chunk)!.release({});
      await vi.advanceTimersByTimeAsync(5_000);
    }
    expect(asked).toEqual([...CHUNKS]);
    stop();
  });

  test("goes on past a chunk that failed", async () => {
    vi.useFakeTimers();
    const { asked, loads } = recording();
    const stop = prefetchOnIdle();
    await vi.advanceTimersByTimeAsync(5_000);
    loads.get(CHUNKS[0]!)!.fail(new Error("offline"));
    await vi.advanceTimersByTimeAsync(5_000);
    expect(asked).toEqual([CHUNKS[0], CHUNKS[1]]);
    stop();
  });

  test("stops when the page that started it goes", async () => {
    vi.useFakeTimers();
    const { asked } = recording();
    const stop = prefetchOnIdle();
    stop();
    await vi.advanceTimersByTimeAsync(60_000);
    expect(asked).toEqual([]);
  });

  test("stands down for a reader who asked to save data", async () => {
    vi.useFakeTimers();
    const { asked } = recording();
    Object.defineProperty(navigator, "connection", {
      configurable: true,
      value: { saveData: true },
    });
    try {
      prefetchOnIdle();
      await vi.advanceTimersByTimeAsync(60_000);
      expect(asked).toEqual([]);
    } finally {
      delete (navigator as { connection?: unknown }).connection;
    }
  });
});

describe("a sidebar row", () => {
  // The fetch starts while the reader is deciding: the pointer resting on a
  // row, or the keyboard's focus arriving on it.
  test("starts fetching its workspace on hover and on focus", async () => {
    const asked: Chunk[] = [];
    for (const chunk of ["spend", "knowledge"] as const) {
      serve(chunk, async () => {
        asked.push(chunk);
        return chunk === "spend"
          ? import("~/routes/spend/index.ts")
          : import("~/routes/knowledge/index.ts");
      });
    }
    // The other chunks, back in after the overrides forgot them.
    await Promise.all(
      CHUNKS.filter((c) => c !== "spend" && c !== "knowledge").map((c) => loadChunk(c)),
    );
    Object.defineProperty(globalThis, "WebSocket", {
      writable: true,
      value: class {
        readyState = 0;
        send(): void {}
        close(): void {}
      },
    });
    location.hash = "#/";
    const store = new Store();
    const socket = new LiveSocket(store);
    (socket as unknown as { query: () => Promise<unknown> }).query = () => new Promise(() => {});
    await act(async () => {
      render(
        <ClientContext.Provider value={{ store, socket }}>
          <Router>
            <App />
          </Router>
        </ClientContext.Provider>,
      );
    });
    const nav = screen.getByRole("navigation", { name: "Navigation" });
    expect(asked).toEqual([]);
    fireEvent.pointerEnter(nav.querySelector('a[href="#/spend"]')!);
    fireEvent.focus(nav.querySelector('a[href="#/knowledge"]')!);
    expect(asked).toEqual(["spend", "knowledge"]);
  });
});

describe("the split", () => {
  /** Where a module-relative or `~` import lands, as a path under `src/`. */
  function target(from: string, source: string): string {
    if (source.startsWith("~/")) return source.slice(2);
    const parts = from.split("/").slice(0, -1);
    for (const seg of source.split("/")) {
      if (seg === "..") parts.pop();
      else if (seg !== ".") parts.push(seg);
    }
    return parts.join("/");
  }

  // A SCREEN IMPORTED STATICALLY FROM OUTSIDE `routes/` IS BACK IN THE ENTRY.
  // The bundler follows a static import wherever it is, so one line in the
  // frame — a peek, a helper a screen happens to export — pulls that whole
  // workspace into the chunk every reader downloads first, and nothing fails:
  // the page works, only heavier. The screens reach the frame through
  // `lazyScreen.ts`'s `import()` and nothing else. `routes/NotFound.tsx` is the
  // one exception, since it belongs to no workspace and is what the dispatch
  // draws when nothing resolves. A TYPE-ONLY import is erased and pulls in
  // nothing, so it is allowed.
  test("nothing outside routes/ imports a screen's module statically", () => {
    const offences: string[] = [];
    for (const m of modules()) {
      if (m.path.startsWith("routes/")) continue;
      walk(parse(m.text, m.lang), (node) => {
        if (
          node.type !== "ImportDeclaration" &&
          node.type !== "ExportNamedDeclaration" &&
          node.type !== "ExportAllDeclaration"
        )
          return;
        const decl = node as unknown as {
          source?: unknown;
          importKind?: string;
          exportKind?: string;
        };
        if (decl.importKind === "type" || decl.exportKind === "type") return;
        const source = stringValue(decl.source as never);
        if (!source || !(source.startsWith("~/") || source.startsWith("."))) return;
        const to = target(m.path, source);
        if (to.startsWith("routes/") && to !== "routes/NotFound.tsx") {
          offences.push(`${m.path} imports ${source}`);
        }
      });
    }
    expect(
      offences,
      "reach a screen through lazyScreen.ts's import(), never a static import",
    ).toEqual([]);
  });
});
