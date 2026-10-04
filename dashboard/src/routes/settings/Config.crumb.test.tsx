/**
 * What the page bar, the browser tab and the palette's recents call one
 * revision.
 *
 * A REVISION ID IS A ULID, so the trail ended in `01JCFGAAAA…` over a header
 * titled with the revision's own summary, `Shell` titled the browser tab from
 * that same trail, and the palette's recents kept a stack of them. The screen
 * had the name all along — it draws it in 24px — and simply never published
 * it. `Seat.crumb.test.tsx` is the same bug on the seat screen and
 * `routes/live/crumbs.test.tsx` is the same bug on the five objects under
 * Activity.
 *
 * And the crumb asserted the MONO face unconditionally, from when it could
 * only ever be the id. A summary is prose; set in the mono face it reads as a
 * value.
 */

import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test } from "vitest";

import { ConfigScreen } from "./Config.tsx";
import { Shell } from "~/app/Shell.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { noteReader } from "~/lib/reader.ts";
import { recentsKey, resetForTest } from "~/lib/recents.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

const NAMED = "01JCFGAAAA0000000000000001";
const UNNAMED = "01JCFGBBBB0000000000000002";

// `configapi.meta`'s own field names — the same fixture `Config.test.tsx`
// keeps, for the same reason: a shape declared from memory is a screen nobody
// has seen.
const revisions = [
  {
    revision_id: NAMED,
    created_at: "2026-08-23T15:00:00Z",
    created_by: "founder",
    created_by_kind: "operator",
    source: "api",
    summary: "connect datadog",
    is_active: true,
    parent_revision_id: UNNAMED,
  },
  {
    revision_id: UNNAMED,
    created_at: "2026-08-22T15:00:00Z",
    created_by: "ops",
    created_by_kind: "operator",
    source: "file",
    summary: "",
    is_active: false,
  },
];

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  localStorage.clear();
  sessionStorage.clear();
  noteReader("p-1");
  resetForTest();
});

afterEach(() => {
  cleanup();
  location.hash = "#/";
  document.title = "";
});

async function settle() {
  await act(async () => {
    await Promise.resolve();
    await Promise.resolve();
    await Promise.resolve();
  });
}

let asked: string[] = [];

function mount(revision?: string) {
  location.hash =
    revision === undefined
      ? "#/settings/config/revisions"
      : `#/settings/config/revisions/${revision}`;
  asked = [];
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) => {
    asked.push(what);
    return Promise.resolve(what === "config_audit" ? revisions : {});
  };
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Shell>
          <ConfigScreen revision={revision} revisions />
        </Shell>
      </Router>
    </ClientContext.Provider>,
  );
}

const trail = () => screen.getByRole("navigation", { name: "Breadcrumb" });
const here = () => trail().querySelector("[aria-current='page']");

function recents(): string[] {
  const raw = localStorage.getItem(recentsKey("p-1"));
  return raw ? (JSON.parse(raw) as { label: string }[]).map((r) => r.label) : [];
}

test("a revision is named by its summary, and the crumb leaves the mono face", async () => {
  mount(NAMED);
  await settle();

  expect(here()?.textContent).toBe("connect datadog");
  expect(here()?.classList.contains("mono"), "prose drawn in the mono face").toBe(false);
  expect(trail().textContent, "the trail still carries the id").not.toContain("01JCFGAAAA");
  expect(document.title).toBe("connect datadog · Crewlet");
  expect(recents()).toEqual(["connect datadog"]);
});

// THE GATE, and the face that goes with it: a revision nobody wrote a summary
// for keeps its id, and an id is drawn as one.
test("a revision with no summary keeps its id, in the mono face", async () => {
  mount(UNNAMED);
  await settle();

  expect(here()?.textContent).toBe(UNNAMED);
  expect(here()?.classList.contains("mono")).toBe(true);
  expect(recents(), "the header's placeholder reached the palette").not.toContain(
    "No summary was written",
  );
});

// THE ADDRESS THE TRAIL CALLS "Revisions" IS THE HISTORY. It showed the Active
// lens — the running document — under a crumb reading Configuration ›
// Revisions, and a revision's own "Revisions" crumb led to that same page.
test("the bare revisions address lands on the history", async () => {
  mount();
  await settle();

  expect(here()?.textContent).toBe("Revisions");
  const lens = screen.getByRole("radiogroup", { name: "Configuration view" });
  expect(lens.querySelector("[aria-checked='true']")?.textContent).toBe("History");
  expect(await screen.findByText("connect datadog")).toBeDefined();
  expect(asked, "the running document asked for on the history's address").not.toContain("config");
  expect(location.hash, "the landing lens is the one the URL need not spell").not.toContain(
    "lens=",
  );
});

// ONE REVISION'S PAGE IS AN OBJECT. It drew the lens bar with Active pressed
// and the whole running document under the revision's header, so the one
// pressed control on the screen said the reader was looking at what is running
// now. And what one save changed is that revision against its PARENT: against
// the active one, the active revision's own "See its changes" was empty.
test("a revision's page draws no lens and reads its changes against its parent", async () => {
  mount(NAMED);
  await settle();

  expect(screen.queryByRole("radiogroup", { name: "Configuration view" })).toBeNull();
  expect(asked).not.toContain("config");
  const changes = screen.getByRole("link", { name: "See its changes" });
  const target = new URLSearchParams(changes.getAttribute("href")!.split("?")[1]);
  expect(target.get("lens")).toBe("diff");
  expect(target.get("revision")).toBe(NAMED);
  expect(target.get("against")).toBe(UNNAMED);
});

// THE FIRST REVISION HAS NO PARENT, and a comparison against the active one
// would answer a question nobody asked, so it offers none.
test("a revision with no parent offers no comparison", async () => {
  mount(UNNAMED);
  await settle();

  expect(here()?.textContent).toBe(UNNAMED);
  expect(screen.queryByRole("link", { name: "See its changes" })).toBeNull();
});
