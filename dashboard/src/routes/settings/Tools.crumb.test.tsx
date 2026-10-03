/**
 * What the page bar and the browser tab call one tool.
 *
 * The trail fell back to the address's raw name — `search_knowledge`, in the
 * mono face meant for a key still awaiting its name — over a header titled
 * with the tool's own title in the body face. The page had the name all
 * along and never published it: the same bug `Config.crumb.test.tsx` holds for
 * a revision.
 */

import { act, cleanup, render, screen } from "~/test/inCase.ts";
import { afterEach, beforeEach, expect, test } from "vitest";

import { Tools } from "./Tools.tsx";
import { Shell } from "~/app/Shell.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { ToolAnnotations, ToolRow } from "~/protocol/index.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

const unadvertised: ToolAnnotations = {
  read_only: "unknown",
  destructive: "unknown",
  idempotent: "unknown",
  open_world: "unknown",
};

const tools: ToolRow[] = [
  {
    name: "search_knowledge",
    title: "Search knowledge",
    description: "Search the company's pages",
    source: "builtin",
    annotations: unadvertised,
    delivers: "",
  },
  {
    name: "create_issue",
    description: "Open an issue on a repository",
    source: "mcp:github",
    annotations: unadvertised,
    delivers: "github",
  },
];

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
});

afterEach(() => {
  cleanup();
  location.hash = "#/";
  document.title = "";
});

function mount(tool: string) {
  location.hash = `#/settings/tools/${tool}`;
  const store = new Store();
  store.applyTools(tools);
  store.setConnected(true);
  const socket = new LiveSocket(store);
  (socket as unknown as { query: () => Promise<unknown> }).query = () => new Promise(() => {});
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Shell>
          <Tools tool={tool} />
        </Shell>
      </Router>
    </ClientContext.Provider>,
  );
}

async function settle() {
  await act(async () => {
    for (let i = 0; i < 6; i++) await Promise.resolve();
  });
}

const here = () =>
  screen.getByRole("navigation", { name: "Breadcrumb" }).querySelector("[aria-current='page']");

test("a titled tool is named in the trail by its title, in the body face", async () => {
  mount("search_knowledge");
  await settle();
  expect(here()?.textContent).toBe("Search knowledge");
  expect(here()?.classList.contains("mono"), "a title drawn as a key").toBe(false);
  expect(document.title).toBe("Search knowledge · Crewlet");
});

test("an untitled tool is named by its name, as its header names it", async () => {
  mount("create_issue");
  await settle();
  expect(here()?.textContent).toBe("create_issue");
  expect(here()?.classList.contains("mono")).toBe(false);
});
