/**
 * The screens arrive as chunks, and every way that can go is a sentence.
 *
 * What this holds: which chunk draws which screen, which shell each chunk is
 * in, that a failed load says what fixes it and can be retried rather than
 * failing for the life of the tab, that a hover or a focus starts the fetch,
 * and that the idle prefetch walks the chunks of the shells the reader may
 * open — the console's for an admin only — once, one at a time, and stops when
 * told to. The frame's half — what a reader sees while a chunk loads or when
 * it never comes — is `boundaries.test.tsx`.
 */

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import {
  CHUNKS,
  ChunkLoadError,
  chunkOf,
  chunksOf,
  lazyScreen,
  loadChunk,
  overrideChunkForTest,
  prefetchOnIdle,
  preload,
  resetChunksForTest,
  shellOfChunk,
  type Chunk,
} from "./lazyScreen.ts";
import { DESTINATIONS, WORKSPACES, workspacesOf } from "./nav.ts";
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
  test("every workspace is a chunk, and the org editor and the console's frame are their own", () => {
    expect([...CHUNKS].sort()).toEqual(
      [...WORKSPACES.map((ws) => ws.key), "org", "console"].sort(),
    );
  });

  // A MEMBER'S BROWSER NEVER FETCHES THE CONSOLE, which is only true while
  // every chunk of the console's code is listed under the console: the org
  // editor, and the frame chunk its sidebar and door are in.
  test("each chunk is in one shell, and the console's are every admin chunk", () => {
    expect(chunksOf("person")).toEqual(workspacesOf("person").map((ws) => ws.key));
    expect(chunksOf("admin")).toEqual([
      "console",
      ...workspacesOf("admin").map((ws) => ws.key),
      "org",
    ]);
    expect(shellOfChunk("org")).toBe("admin");
    expect(shellOfChunk("console")).toBe("admin");
    expect([...chunksOf("person"), ...chunksOf("admin")].sort()).toEqual([...CHUNKS].sort());
  });

  test("a screen is drawn by its workspace's chunk, the builder by the org chunk", () => {
    const edit = resolve(["admin", "company", "org"]);
    const chart = resolve(["team"]);
    const item = resolve(["work", "ENG-4"]);
    const landing = resolve([]);
    expect(edit.resolved && chunkOf(edit)).toBe("org");
    expect(chart.resolved && chunkOf(chart)).toBe("team");
    expect(item.resolved && chunkOf(item)).toBe("work");
    // THE LANDING IS NO CHUNK'S: the frame turns it into an address first.
    expect(landing.resolved && chunkOf(landing)).toBeNull();
  });

  // Every place the sidebar and the palette can send a reader has a chunk that
  // loads — a `routes/<workspace>/index.ts` that stopped exporting a screen
  // would compile (the dispatch picks by name) and fail here.
  test("every destination's chunk loads and exports a screen", async () => {
    for (const d of DESTINATIONS) {
      const where = resolve(d.path);
      const chunk = where.resolved ? chunkOf(where) : null;
      expect(chunk, d.path.join("/")).not.toBeNull();
      const module = await loadChunk(chunk!);
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
    preload(["admin", "company", "org"]);
    preload(["admin", "connect", "secrets"]);
    preload(["nowhere", "at", "all"]);
    // THE LANDING DRAWS NOTHING OF ITS OWN, so there is nothing to start.
    preload([]);
    await Promise.resolve();
    expect(asked).toEqual(["org", "connect"]);
  });

  test("swallows a failure, which the navigation will report", async () => {
    serve("spend", () => Promise.reject(new Error("offline")));
    const unhandled = vi.fn();
    process.on("unhandledRejection", unhandled);
    preload(["admin", "spend"]);
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
    const stop = prefetchOnIdle(["person", "admin"]);
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

  // A MEMBER'S BROWSER IS NEVER HANDED THE CONSOLE: the prefetch walks the
  // person shell's chunks and stops, however long the page stays idle.
  test("fetches only the person shell's chunks for a reader who is not an admin", async () => {
    vi.useFakeTimers();
    const { asked, loads } = recording();
    const stop = prefetchOnIdle(["person"]);
    for (let i = 0; i < CHUNKS.length + 2; i++) {
      await vi.advanceTimersByTimeAsync(5_000);
      for (const chunk of asked) loads.get(chunk)!.release({});
    }
    expect(asked).toEqual(chunksOf("person"));
    for (const chunk of chunksOf("admin")) expect(asked).not.toContain(chunk);
    stop();
  });

  test("goes on past a chunk that failed", async () => {
    vi.useFakeTimers();
    const { asked, loads } = recording();
    const stop = prefetchOnIdle(["person", "admin"]);
    await vi.advanceTimersByTimeAsync(5_000);
    loads.get(CHUNKS[0]!)!.fail(new Error("offline"));
    await vi.advanceTimersByTimeAsync(5_000);
    expect(asked).toEqual([CHUNKS[0], CHUNKS[1]]);
    stop();
  });

  test("stops when the page that started it goes", async () => {
    vi.useFakeTimers();
    const { asked } = recording();
    const stop = prefetchOnIdle(["person", "admin"]);
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
      prefetchOnIdle(["person", "admin"]);
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
    for (const chunk of ["work", "knowledge"] as const) {
      serve(chunk, async () => {
        asked.push(chunk);
        return chunk === "work"
          ? import("~/routes/work/index.ts")
          : import("~/routes/knowledge/index.ts");
      });
    }
    // The other chunks, back in after the overrides forgot them.
    await Promise.all(
      CHUNKS.filter((c) => c !== "work" && c !== "knowledge").map((c) => loadChunk(c)),
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
    fireEvent.pointerEnter(nav.querySelector('a[href="#/work"]')!);
    fireEvent.focus(nav.querySelector('a[href="#/knowledge"]')!);
    expect(asked).toEqual(["work", "knowledge"]);
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

  // AND THE CONSOLE'S FRAME CHUNK THE SAME WAY: the sidebar and the door the
  // frame draws for an admin are imported by nothing outside `app/console/`
  // but `lazyScreen.ts`'s `import()` — one static import from the frame and
  // every member downloads them.
  test("nothing outside app/console/ imports the console's frame statically", () => {
    const offences: string[] = [];
    for (const m of modules()) {
      if (m.path.startsWith("app/console/")) continue;
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
        if (target(m.path, source).startsWith("app/console/")) {
          offences.push(`${m.path} imports ${source}`);
        }
      });
    }
    expect(offences, "reach the console's frame through lazyScreen.ts's import()").toEqual([]);
  });
});
