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

import { readFileSync } from "node:fs";
import { join } from "node:path";
import { startTransition, useState } from "react";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
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
import { modules, parse, stringValue, walk, type Module, type Node } from "~/test/source.ts";

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
  // A CHUNK IS A SCREEN DIRECTORY, not a sidebar row: Company's rows draw
  // screens from four of them, and a row that moves moves no code.
  test("every screen directory is a chunk, and the org editor is one of its own", () => {
    expect([...CHUNKS].sort()).toEqual(
      [
        "account",
        "agents",
        "artifacts",
        "assistant",
        "brand",
        "chatlet",
        "home",
        "inbox",
        "doclet",
        "live",
        "me",
        "org",
        "palette",
        "settings",
        "spend",
        "worklet",
      ].sort(),
    );
    // EVERY SIDEBAR ROW'S LANDING is drawn by one of them.
    for (const ws of WORKSPACES) {
      const where = resolve(ws.path);
      expect(where.resolved && CHUNKS.includes(chunkOf(where)), ws.key).toBe(true);
    }
  });

  // THE DESIGN DOC NAMES EVERY SCREEN DIRECTORY, exactly as `ScreenDir`
  // declares them. Its tree in "How it is built" named nine while the code had
  // thirteen, so a reader never learned that Chatlet, Artifacts, Brand or the
  // Account page is a directory and a chunk of its own. Both lists are read,
  // never copied here: the type from lazyScreen.ts's syntax tree, and the
  // doc's from the tree's `src/routes/<dir>/` entry.
  test("the design doc's tree lists every screen directory, and only those", () => {
    const source = modules().find((m) => m.path === "app/lazyScreen.ts")!;
    const declared: string[] = [];
    walk(parse(source.text, source.lang), (node) => {
      if (node.type !== "TSTypeAliasDeclaration" || (node.id as Node).name !== "ScreenDir") return;
      walk(node.typeAnnotation as Node, (member) => {
        if (member.type !== "TSLiteralType") return;
        const name = stringValue(member.literal as Node);
        if (name !== null) declared.push(name);
      });
      return false;
    });
    expect(declared.length, "ScreenDir is not where this reads it").toBeGreaterThan(0);
    const doc = readFileSync(
      join(process.cwd(), "..", "docs", "reference", "dashboard-design.md"),
      "utf8",
    );
    const entry = /^ {2}src\/routes\/<[a-z]+>\/[^(]*\(([^)]*)\)/m.exec(doc);
    expect(entry, "the tree in How it is built has no src/routes/ entry").not.toBeNull();
    const listed = entry![1]!.split(",").map((name) => name.trim());
    expect([...listed].sort()).toEqual([...declared].sort());
  });

  // EVERY SCREEN OPENS THE PALETTE WITH A KEY, so it is the first chunk the
  // idle prefetch fetches: a ⌘K pressed after the first idle moment finds it in.
  test("the palette is a chunk of its own, and the first one prefetched", async () => {
    expect(CHUNKS[0]).toBe("palette");
    const module = await loadChunk("palette");
    expect(typeof module.CommandPalette).toBe("function");
  });

  test("a screen is drawn by its directory's chunk, the builder by the org chunk", () => {
    const at = (...path: string[]) => {
      const where = resolve(path);
      return where.resolved ? chunkOf(where) : null;
    };
    expect(at("company", "org", "edit")).toBe("org");
    expect(at("company", "org")).toBe("agents");
    expect(at("company", "schedules")).toBe("agents");
    expect(at("company")).toBe("settings");
    expect(at("company", "budgets")).toBe("spend");
    expect(at("settings", "spend")).toBe("spend");
    expect(at("worklet", "ENG-4")).toBe("worklet");
  });

  test("each kind's create page is a chunk of its own, and none is prefetched", () => {
    const doc = resolve(["artifacts", "new", "doc"]);
    const html = resolve(["artifacts", "new", "html"]);
    expect(doc.resolved && chunkOf(doc)).toBe("new-doc");
    expect(html.resolved && chunkOf(html)).toBe("new-html");
    expect(CHUNKS.some((c) => c.startsWith("new-"))).toBe(false);
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
    expect(message).toMatch(/^The Spend and Budgets screens could not be loaded\./);
    expect(message).toContain("The engine was updated.");
    expect(message).toMatch(/Reload/);
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

  // A COMPONENT THAT SUSPENDED ON `use` CALLS IT AGAIN when it resumes. A
  // navigation renders in a TRANSITION, where React keeps the screen it has
  // and replays the suspended component in place once its chunk arrives; a
  // fast path that read the arrived chunk before `use` skipped it on exactly
  // that replay, and React logged "did not call use() when it finished" for
  // every screen a reader opened before its chunk was in.
  test("calls use on the render that resumes, so React reports nothing", async () => {
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      const load = held();
      serve("spend", () => load.promise);
      const Spend = lazyScreen("spend", (m) => m.Spend);
      let open: () => void = () => {};
      function Navigation() {
        const [opened, setOpened] = useState(false);
        open = () => startTransition(() => setOpened(true));
        return (
          <ScreenBoundary resetKey="spend">
            {opened ? <Spend /> : <p>the home screen</p>}
          </ScreenBoundary>
        );
      }
      render(<Navigation />);
      await act(async () => open());
      expect(screen.getByText("the home screen")).toBeDefined();
      await act(async () => load.release({ Spend: () => <p>the spend screen</p> }));
      expect(await screen.findByText("the spend screen")).toBeDefined();
      expect(errors.mock.calls.map((call) => String(call[0]))).toEqual([]);
    } finally {
      errors.mockRestore();
    }
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
    preload(["company", "org", "edit"]);
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
    for (const chunk of ["spend", "live"] as const) {
      serve(chunk, async () => {
        asked.push(chunk);
        return chunk === "spend"
          ? import("~/routes/spend/index.ts")
          : import("~/routes/live/index.ts");
      });
    }
    // The other chunks, back in after the overrides forgot them.
    await Promise.all(CHUNKS.filter((c) => c !== "spend" && c !== "live").map((c) => loadChunk(c)));
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
    fireEvent.pointerEnter(nav.querySelector('a[href="#/company/budgets"]')!);
    fireEvent.focus(nav.querySelector('a[href="#/live"]')!);
    expect(asked).toEqual(["spend", "live"]);
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

  /**
   * Each module-relative or `~` import `m` makes that is not type-only, as
   * the source it spells and the path under `src/` it lands on. A type-only
   * import is erased and pulls in nothing.
   */
  function staticImports(m: Module) {
    const found: { source: string; to: string }[] = [];
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
      found.push({ source, to: target(m.path, source) });
    });
    return found;
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
      for (const { source, to } of staticImports(m)) {
        if (to.startsWith("routes/") && to !== "routes/NotFound.tsx") {
          offences.push(`${m.path} imports ${source}`);
        }
      }
    }
    expect(
      offences,
      "reach a screen through lazyScreen.ts's import(), never a static import",
    ).toEqual([]);
  });

  // A WORKSPACE'S OWN MODULE UNDER `lib/` IS IN THE ENTRY ONCE THE FRAME
  // REACHES IT. A bundler places a module whole, so the frame importing one
  // small helper from a module that is otherwise a screen's carries all of it
  // into the chunk every reader downloads first: the sidebar's and the
  // palette's `artifactPath` once brought every artifact write and read with
  // it. Such a module is named here, and the entry's static graph, walked
  // from `main.tsx`, must not reach it. So is a LAYER the frame mounts only
  // while open and loads as a chunk of its own: the command palette, the
  // largest single piece of the frame, which one static import of it would
  // put back into the first paint.
  // AND THE ASSISTANT'S PANEL, a layer of the same kind: its store, its
  // reducer, its cards and its stream client stay out of the first paint,
  // where the entry keeps only the Ask button, the page context registry,
  // the bus and the one-word status the button reads.
  const SCREEN_ONLY = [
    "lib/artifacts/api.ts",
    "app/palette/Palette.tsx",
    "app/assistant/chunk/index.ts",
    "app/assistant/chunk/Drawer.tsx",
    "app/assistant/chunk/session.ts",
    "protocol/assistant.ts",
  ];

  test("the entry's static graph reaches no module only a screen needs", () => {
    const byPath = new Map(modules().map((m) => [m.path, m]));
    const from = new Map<string, string>([["main.tsx", ""]]);
    const queue = ["main.tsx"];
    for (let path = queue.shift(); path !== undefined; path = queue.shift()) {
      const m = byPath.get(path);
      if (!m) continue;
      for (const { to } of staticImports(m)) {
        if (from.has(to)) continue;
        from.set(to, path);
        queue.push(to);
      }
    }
    const chain = (path: string): string =>
      from.get(path) ? `${chain(from.get(path)!)} > ${path}` : path;
    expect(
      SCREEN_ONLY.filter((path) => from.has(path)).map(chain),
      "move what the frame needs out of the screen's module",
    ).toEqual([]);
    expect(
      SCREEN_ONLY.filter((path) => !byPath.has(path)),
      "a module named here that no longer exists",
    ).toEqual([]);
  });
});
