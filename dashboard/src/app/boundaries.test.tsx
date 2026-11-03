/**
 * A region that throws takes only itself down.
 *
 * Without a boundary React unmounts the whole application on a render error,
 * which is what a seat whose `llm` was a per-phase mapping once did: a blank
 * page with no way out. These cases mount the real application and break one
 * region at a time — the screen, its chunk, the peek, a sidebar section, the
 * palette — and assert what a reader still has.
 */

import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeAll, beforeEach, describe, expect, test } from "vitest";
import { App } from "./App.tsx";
import { Router } from "./router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { CHUNKS, ChunkLoadError, loadChunk, overrideChunkForTest } from "./lazyScreen.ts";
import { LayerBoundary } from "./boundaries.tsx";
import { refToken } from "./frame/objects.ts";
import { resetForTest as resetStarsForTest } from "~/lib/starred.ts";
import { expectConsole } from "~/test/console.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

/** A screen that throws the way a malformed field does. */
function Throws(): never {
  throw new Error("model is an object, not a string");
}

// WHAT THESE CASES PROVOKE ON PURPOSE: React reports every error a boundary
// catches to `console.error`, and the console gate (`~/test/console.ts`) fails
// a case that writes there unasked. Each case declares the report its failure
// makes, and the gate fails it if the report never comes.
const THROWN = /model is an object, not a string/;
const expectCaught = (pattern: RegExp) => expectConsole("error", pattern);

let undo: (() => void)[] = [];

/** The real chunk with one export replaced — what a case breaks. */
function breakExport(chunk: "spend" | "worklet", name: string, component: unknown) {
  const real =
    chunk === "spend"
      ? () => import("~/routes/spend/index.ts")
      : () => import("~/routes/worklet/index.ts");
  undo.push(overrideChunkForTest(chunk, async () => ({ ...(await real()), [name]: component })));
}

/**
 * The application over a socket that answers each query from `answers`.
 *
 * AWAITED UNDER ACT, because a case that replaced a chunk has forgotten that
 * chunk's load and the first render suspends on it: React retries a
 * suspended render from act's queue, and a synchronous render never sees that
 * retry.
 */
async function mount(answers: Record<string, unknown> = {}) {
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    what in answers ? Promise.resolve(answers[what]) : new Promise(() => {});
  await act(async () => {
    render(
      <ClientContext.Provider value={{ store, socket }}>
        <Router>
          <App />
        </Router>
      </ClientContext.Provider>,
    );
  });
}

async function go(hash: string) {
  await act(async () => {
    location.hash = hash;
    window.dispatchEvent(new HashChangeEvent("hashchange"));
  });
}

beforeAll(async () => {
  await Promise.all(CHUNKS.map((chunk) => loadChunk(chunk)));
});

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  Element.prototype.scrollIntoView = () => {};
  localStorage.clear();
  resetStarsForTest();
  location.hash = "#/";
});

afterEach(async () => {
  cleanup();
  for (const u of undo.reverse()) u();
  undo = [];
  location.hash = "#/";
  // Each undo forgot its chunk's load; load the real one again, so the next
  // case meets every chunk loaded, as `beforeAll` left them.
  await Promise.all(CHUNKS.map((chunk) => loadChunk(chunk)));
});

describe("a screen that throws", () => {
  test("leaves the sidebar and ⌘K working", async () => {
    expectCaught(THROWN);
    breakExport("spend", "Spend", Throws);
    location.hash = "#/settings/spend";
    await mount();
    const alert = await screen.findByText("This screen could not be drawn");
    expect(alert).toBeDefined();
    // The message a report needs is on screen.
    expect(screen.getByText("model is an object, not a string")).toBeDefined();
    // THE WAY OUT IS STILL THERE: the sidebar's rows, and the palette.
    const nav = screen.getByRole("navigation", { name: "Navigation" });
    expect(nav.querySelector('a[href="#/home"]')).not.toBeNull();
    fireEvent.keyDown(document, { key: "k", metaKey: true });
    expect(await screen.findByRole("dialog", { name: "Search" })).toBeDefined();
  });

  test("is drawn afresh when the reader goes somewhere else", async () => {
    expectCaught(THROWN);
    breakExport("spend", "Spend", Throws);
    location.hash = "#/settings/spend";
    await mount();
    await screen.findByText("This screen could not be drawn");
    await go("#/company/budgets");
    await waitFor(() => expect(screen.queryByText("This screen could not be drawn")).toBeNull());
  });

  test("but not by a query string alone, which is the same screen", async () => {
    expectCaught(THROWN);
    breakExport("spend", "Spend", Throws);
    location.hash = "#/settings/spend";
    await mount();
    await screen.findByText("This screen could not be drawn");
    await go("#/settings/spend?range=7d");
    expect(screen.getByText("This screen could not be drawn")).toBeDefined();
  });
});

describe("a chunk that never arrives", () => {
  test("has its own sentence, a Reload, and a Try again that really asks again", async () => {
    expectCaught(/The Spend and Budgets screens could not be loaded/);
    let fail = true;
    let asks = 0;
    undo.push(
      overrideChunkForTest("spend", async () => {
        asks++;
        if (fail) throw new TypeError("Failed to fetch dynamically imported module");
        return { ...(await import("~/routes/spend/index.ts")), Spend: () => <p>spend, loaded</p> };
      }),
    );
    location.hash = "#/settings/spend";
    await mount();
    expect(
      await screen.findByText("The Spend and Budgets screens could not be loaded"),
    ).toBeDefined();
    // NOT the render failure's sentence: a reload fixes this, a report does not.
    expect(screen.queryByText("This screen could not be drawn")).toBeNull();
    expect(screen.getByText(/The engine was updated/)).toBeDefined();
    expect(screen.getByRole("button", { name: "Reload" })).toBeDefined();
    // Asked once to learn that it failed, and once more by the Try again.
    expect(asks).toBe(1);
    fail = false;
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    });
    expect(await screen.findByText("spend, loaded")).toBeDefined();
    expect(asks).toBe(2);
  });

  // THE SIGN-IN SCREENS ARE DRAWN OUTSIDE THE FRAME, with no sidebar and no
  // shell, through the routed screen's boundary all the same: so a reader
  // signing in from a tab an upgrade outdated is told once, and asks again.
  test("outside the frame, on the sign-in screen, is told once and asked again", async () => {
    expectCaught(/The sign-in screens could not be loaded/);
    let fail = true;
    let asks = 0;
    undo.push(
      overrideChunkForTest("signin", async () => {
        asks++;
        if (fail) throw new TypeError("Failed to fetch dynamically imported module");
        return { ...(await import("~/routes/signin/index.ts")), SignIn: () => <p>signing in</p> };
      }),
    );
    location.hash = "#/login";
    await mount();
    expect(await screen.findByText("The sign-in screens could not be loaded")).toBeDefined();
    expect(screen.queryByRole("navigation", { name: "Navigation" })).toBeNull();
    expect(asks).toBe(1);
    fail = false;
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    });
    expect(await screen.findByText("signing in")).toBeDefined();
    expect(asks).toBe(2);
  });
});

describe("a peek that throws", () => {
  test("takes its body down and leaves the rail's close and the screen behind", async () => {
    expectCaught(THROWN);
    breakExport("worklet", "ItemPeek", Throws);
    location.hash = `#/settings/spend?peek=${refToken({ kind: "item", id: "ENG-42" })}`;
    await mount();
    expect(await screen.findByText("This preview could not be drawn")).toBeDefined();
    // The rail around the body stands, with its way out…
    const close = screen.getByRole("button", { name: "Close" });
    // …and the screen behind the peek is drawn, not the failure.
    expect(screen.queryByText("This screen could not be drawn")).toBeNull();
    expect(document.querySelector(".crewlet-app-shell__content")?.textContent).not.toBe("");
    fireEvent.click(close);
    await waitFor(() => expect(screen.queryByText("This preview could not be drawn")).toBeNull());
  });
});

describe("a sidebar section that throws", () => {
  // A pinned view whose name arrived as an object is a React child React
  // refuses: the same shape of failure as the per-phase model mapping.
  test("costs its own list and never the navigation above it", async () => {
    expectCaught(/Objects are not valid as a React child/);
    location.hash = "#/live";
    await mount({
      // A READER WITH A RECORD AND TRACKER: the pinned views are theirs, read
      // under the owner their record is kept by, and the tracker's question.
      viewer: {
        login: "ada",
        grants: ["worklet"],
        handle: "ada",
        owner: "ada",
        name: "Ada",
        kind: "human",
        acts: [],
        config_writer: false,
        config_managed_by: [],
      },
      work_saved_views: {
        complete: true,
        views: [
          {
            id: "v-1",
            key: "blocked",
            name: { en: "Blocked" },
            type: "list",
            container: { kind: "workspace", id: "" },
            builtin: false,
            pinned: true,
          },
        ],
      },
    });
    const failed = await screen.findByText("Could not be drawn.");
    expect(failed.closest('[role="group"]')?.textContent).toContain("Pinned");
    const nav = screen.getByRole("navigation", { name: "Navigation" });
    expect(nav.querySelector('a[href="#/inbox"]')).not.toBeNull();
    // And the screen is untouched.
    expect(screen.queryByText("This screen could not be drawn")).toBeNull();
  });
});

describe("a layer's boundary", () => {
  test("is a dialog that says so and closes the way the layer does", () => {
    expectCaught(THROWN);
    let closed = 0;
    render(
      <LayerBoundary title="Search" onClose={() => closed++}>
        <Throws />
      </LayerBoundary>,
    );
    const dialog = screen.getByRole("dialog", { name: "Search could not be drawn" });
    expect(dialog.textContent).toContain("model is an object, not a string");
    fireEvent.click(screen.getAllByRole("button", { name: "Close" })[0]!);
    expect(closed).toBe(1);
  });

  // THE NEW TASK SHEET'S CODE IS THE WORKLET CHUNK'S, so a sheet opened from a
  // tab an upgrade outdated is a chunk that never arrives — and that is the
  // chunk's own sentence and a Reload, not the raw rejection.
  test("a layer whose chunk never came says to reload", () => {
    expectCaught(/The Worklet screens could not be loaded/);
    function ChunkGone(): never {
      throw new ChunkLoadError("worklet", new Error("Failed to fetch"));
    }
    render(
      <LayerBoundary title="New task" onClose={() => {}}>
        <ChunkGone />
      </LayerBoundary>,
    );
    const dialog = screen.getByRole("dialog", { name: "New task could not be drawn" });
    expect(dialog.textContent).toContain("Reload to continue");
    expect(screen.getByRole("button", { name: "Reload" })).toBeTruthy();
  });
});
