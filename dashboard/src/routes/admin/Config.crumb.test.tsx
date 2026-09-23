/**
 * What the page bar, the browser tab and the palette's recents call one
 * revision.
 *
 * A REVISION ID IS A UUID, so the trail ended in `aaaaaaaa-…` over a header
 * titled with the revision's own summary, `Shell` titled the browser tab from
 * that same trail, and the palette's recents kept a stack of them. The screen
 * had the name all along — it draws it in 24px — and simply never published
 * it. `Seat.crumb.test.tsx` is the same bug on the seat screen and
 * `routes/activity/crumbs.test.tsx` is the same bug on the five objects under
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
import { resetForTest } from "~/lib/recents.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

// UUIDS, which is what `store.Configs` mints (`uuid.NewString`).
const NAMED = "aaaaaaaa-0000-4000-8000-000000000001";
const UNNAMED = "bbbbbbbb-0000-4000-8000-000000000002";

// `configapi.meta`'s own field names — the same fixture `Config.test.tsx`
// keeps, for the same reason: a shape declared from memory is a screen nobody
// has seen.
const revisions = [
  {
    revision_id: NAMED,
    created_at: "2026-08-23T15:00:00Z",
    created_by: "founder",
    source: "api",
    summary: "connect datadog",
    is_active: true,
  },
  {
    revision_id: UNNAMED,
    created_at: "2026-08-22T15:00:00Z",
    created_by: "ops",
    source: "cli",
    summary: "",
    is_active: false,
  },
];

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  localStorage.clear();
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

function mount(revision: string) {
  location.hash = `#/admin/config/revisions/${revision}`;
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    Promise.resolve(what === "config_audit" ? revisions : {});
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Shell>
          <ConfigScreen revision={revision} />
        </Shell>
      </Router>
    </ClientContext.Provider>,
  );
}

const trail = () => screen.getByRole("navigation", { name: "Breadcrumb" });
const here = () => trail().querySelector("[aria-current='page']");

function recents(): string[] {
  const raw = localStorage.getItem("crewlet_recents");
  return raw ? (JSON.parse(raw) as { label: string }[]).map((r) => r.label) : [];
}

test("a revision is named by its summary, and the crumb leaves the mono face", async () => {
  mount(NAMED);
  await settle();

  expect(here()?.textContent).toBe("connect datadog");
  expect(here()?.classList.contains("mono"), "prose drawn in the mono face").toBe(false);
  expect(trail().textContent, "the trail still carries the id").not.toContain("aaaaaaaa");
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
