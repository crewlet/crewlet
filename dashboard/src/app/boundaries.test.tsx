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
import { CHUNKS, loadChunk, overrideChunkForTest } from "./lazyScreen.ts";
import { PaletteBoundary } from "./boundaries.tsx";
import { refToken } from "./frame/objects.ts";
import { resetForTest as resetStarsForTest } from "~/lib/starred.ts";

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

let undo: (() => void)[] = [];

/** The real chunk with one export replaced — what a case breaks. */
function breakExport(chunk: "spend" | "work", name: string, component: unknown) {
  const real =
    chunk === "spend"
      ? () => import("~/routes/spend/index.ts")
      : () => import("~/routes/work/index.ts");
  undo.push(overrideChunkForTest(chunk, async () => ({ ...(await real()), [name]: component })));
}

/**
 * The application over a socket that answers each query from `answers`.
 *
 * AWAITED UNDER ACT, because a case that replaced a chunk has forgotten the
 * real ones and the first render suspends: React retries a suspended render
 * from act's queue, and a synchronous render never sees that retry.
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
  // The overrides forgot every chunk; put the real ones back for the next case.
  await Promise.all(CHUNKS.map((chunk) => loadChunk(chunk)));
});

describe("a screen that throws", () => {
  test("leaves the sidebar and ⌘K working", async () => {
    breakExport("spend", "Spend", Throws);
    location.hash = "#/spend";
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
    breakExport("spend", "Spend", Throws);
    location.hash = "#/spend";
    await mount();
    await screen.findByText("This screen could not be drawn");
    await go("#/spend/budgets");
    await waitFor(() => expect(screen.queryByText("This screen could not be drawn")).toBeNull());
  });

  test("but not by a query string alone, which is the same screen", async () => {
    breakExport("spend", "Spend", Throws);
    location.hash = "#/spend";
    await mount();
    await screen.findByText("This screen could not be drawn");
    await go("#/spend?range=7d");
    expect(screen.getByText("This screen could not be drawn")).toBeDefined();
  });
});

describe("a chunk that never arrives", () => {
  test("has its own sentence, a Reload, and a Try again that really asks again", async () => {
    let fail = true;
    undo.push(
      overrideChunkForTest("spend", async () => {
        if (fail) throw new TypeError("Failed to fetch dynamically imported module");
        return { ...(await import("~/routes/spend/index.ts")), Spend: () => <p>spend, loaded</p> };
      }),
    );
    location.hash = "#/spend";
    await mount();
    expect(await screen.findByText("The Spend screens could not be loaded")).toBeDefined();
    // NOT the render failure's sentence: a reload fixes this, a report does not.
    expect(screen.queryByText("This screen could not be drawn")).toBeNull();
    expect(screen.getByText(/from another engine version/)).toBeDefined();
    expect(screen.getByRole("button", { name: "Reload" })).toBeDefined();
    fail = false;
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    });
    expect(await screen.findByText("spend, loaded")).toBeDefined();
  });
});

describe("a peek that throws", () => {
  test("takes its body down and leaves the rail's close and the screen behind", async () => {
    breakExport("work", "ItemPeek", Throws);
    location.hash = `#/spend?peek=${refToken({ kind: "item", id: "ENG-42" })}`;
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
  // A project whose name arrived as an object is a React child React refuses —
  // the same shape of failure as the per-phase model mapping.
  test("costs its own list and never the navigation above it", async () => {
    // ON A SCREEN THAT DOES NOT READ THE PROJECTS ITSELF: Home draws a
    // projects card from the same answer, and its own boundary is the
    // screen's, which is the case below this one.
    location.hash = "#/live";
    await mount({
      work_projects: {
        projects: [
          {
            key: "ENG",
            name: { en: "Engineering" },
            task_counts: { todo: 1, active: 0, done: 0, closed: 0 },
          },
        ],
        complete: true,
      },
    });
    const failed = await screen.findByText("Could not be drawn.");
    expect(failed.closest('[role="group"]')?.textContent).toContain("Projects");
    const nav = screen.getByRole("navigation", { name: "Navigation" });
    expect(nav.querySelector('a[href="#/inbox"]')).not.toBeNull();
    // And the screen is untouched.
    expect(screen.queryByText("This screen could not be drawn")).toBeNull();
  });
});

describe("the palette's boundary", () => {
  test("is a dialog that says so and closes the way the palette does", () => {
    let closed = 0;
    render(
      <PaletteBoundary onClose={() => closed++}>
        <Throws />
      </PaletteBoundary>,
    );
    const dialog = screen.getByRole("dialog", { name: "Search could not be drawn" });
    expect(dialog.textContent).toContain("model is an object, not a string");
    fireEvent.click(screen.getAllByRole("button", { name: "Close" })[0]!);
    expect(closed).toBe(1);
  });
});
