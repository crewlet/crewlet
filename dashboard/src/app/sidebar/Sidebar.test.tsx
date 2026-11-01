/**
 * The sidebar's Pinned group lists what the reader pinned: their saved views
 * (with Worklet) and the artifacts they pinned (with no grant at all, since a
 * share alone admits a reader to Artifacts). "Pin to your sidebar" is a
 * promise these cases hold the sidebar to.
 */

import { act, cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test } from "vitest";
import { App } from "../App.tsx";
import { Router } from "../router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { announcePinsChanged } from "~/lib/artifacts/pins.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

const viewer = (grants: string[]) => ({
  login: "ada",
  grants,
  handle: "ada",
  owner: "ada",
  name: "Ada",
  kind: "human",
  acts: [],
  config_writer: false,
  config_managed_by: [],
});

const listed = (id: string, title: string) => ({
  id,
  kind: "sheet",
  title,
  owner: "ada",
  role: "owner",
  status: "live",
  pinned: true,
  updated_at: "2026-10-18T10:00:00Z",
});

/** The application over a socket that answers each query from `answer`. */
async function mount(answer: (what: string, params: Record<string, unknown>) => unknown) {
  const store = new Store();
  const socket = new LiveSocket(store);
  (
    socket as unknown as {
      query: (what: string, params: Record<string, unknown>) => Promise<unknown>;
    }
  ).query = (what, params) => {
    const out = answer(what, params ?? {});
    return out === undefined ? new Promise(() => {}) : Promise.resolve(out);
  };
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

const pinnedGroup = () =>
  screen
    .getAllByRole("group")
    .find(
      (g) =>
        g.closest("nav")?.getAttribute("aria-label") === "Navigation" &&
        g.textContent?.startsWith("Pinned"),
    );

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  Element.prototype.scrollIntoView = () => {};
  localStorage.clear();
  location.hash = "#/live";
});

afterEach(() => {
  cleanup();
  location.hash = "#/";
});

describe("the sidebar's Pinned group", () => {
  test("lists a pinned artifact for a reader with no grant, linked to its page", async () => {
    await mount((what, params) => {
      if (what === "viewer") return viewer([]);
      if (what === "artifacts" && params["pinned"] === true)
        return { contents: [listed("a-1", "Launch gates")], total: 1 };
      return undefined;
    });
    const link = await screen.findByRole("link", { name: /Launch gates/ });
    expect(link.getAttribute("href")).toBe("#/artifacts/a-1");
    expect(pinnedGroup()).toBeDefined();
    expect(within(pinnedGroup()!).getByRole("link", { name: /Launch gates/ })).toBe(link);
  });

  test("moves at once when a pin lands anywhere in the tab", async () => {
    let pins = [listed("a-1", "Launch gates")];
    await mount((what, params) => {
      if (what === "viewer") return viewer([]);
      if (what === "artifacts" && params["pinned"] === true)
        return { contents: pins, total: pins.length };
      return undefined;
    });
    await screen.findByRole("link", { name: /Launch gates/ });
    pins = [...pins, listed("a-2", "Team retro board")];
    await act(async () => announcePinsChanged());
    expect(await screen.findByRole("link", { name: /Team retro board/ })).toBeDefined();
  });

  test("is not drawn when nothing is pinned", async () => {
    await mount((what, params) => {
      if (what === "viewer") return viewer([]);
      if (what === "artifacts" && params["pinned"] === true) return { contents: [], total: 0 };
      return undefined;
    });
    await screen.findByRole("navigation", { name: "Navigation" });
    expect(pinnedGroup()).toBeUndefined();
  });
});
