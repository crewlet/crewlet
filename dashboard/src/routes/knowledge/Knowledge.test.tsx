/**
 * What a container's rail says about what has been written in it.
 *
 * It used to read the PAGE LIST — ordered by container and TITLE and cut at its
 * limit — and say three things from it that only hold when the read is the
 * whole container: which pages were written lately (the newest of an
 * alphabetical fifty), how many more there were (that fifty's remainder), and
 * who writes here (the authors of the same slice, the rest behind a `+N more`
 * nothing could open). On a four-hundred-page container every one was wrong.
 *
 * It reads the CHANGE FEED instead, narrowed to the container's creates and
 * saves, which is in order of writing — so the newest write is its first row
 * however large the container — and it counts pages with the container's own
 * count rather than with the rows it holds.
 *
 * LESS THE TRASH, which the page list asked for by status and the feed cannot:
 * a trash keeps the page's head, so its writes come back filtered to the
 * container, and the container's count leaves it out.
 */

import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { ContainerPeek, writtenPages } from "./Knowledge.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { PageChange, QueryName } from "~/protocol/index.ts";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

function change(n: number, over: Partial<PageChange> = {}): PageChange {
  return {
    id: `c-${n}`,
    page_id: `p-${n}`,
    title: `Page ${n}`,
    container: "LEAD",
    kind: "saved",
    actor: `seat-${n}`,
    at: new Date(Date.now() - n * 60_000).toISOString(),
    log_seq: 1000 - n,
    ...over,
  };
}

/**
 * The container rail over a socket answering `containers`, `page_activity` and
 * the trashed-pages read, whose ids are `trashed`.
 */
function mount(
  changes: PageChange[],
  pages: number,
  next_cursor?: string,
  trash: { trashed?: string[]; truncated?: boolean } = {},
) {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  const asked: { what: string; params: Record<string, unknown> }[] = [];
  const store = new Store();
  store.applyHealth({ status: "healthy" });
  const socket = new LiveSocket(store);
  (
    socket as unknown as {
      query: (what: string, params?: Record<string, unknown>) => Promise<unknown>;
    }
  ).query = (what: string, params = {}) => {
    asked.push({ what, params });
    switch (what as QueryName) {
      case "containers":
        return Promise.resolve({ containers: [{ key: "LEAD", name: "Leadership", pages }] });
      case "page_activity":
        return Promise.resolve({ changes, next_cursor });
      case "pages":
        return Promise.resolve({
          pages: (trash.trashed ?? []).map((id) => ({ id, status: "trashed" })),
          truncated: trash.truncated ?? false,
        });
      default:
        return Promise.resolve({});
    }
  };
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <ContainerPeek id="LEAD" />
      </Router>
    </ClientContext.Provider>,
  );
  return asked;
}

test("the rail asks the container's creates and saves, and its trash", async () => {
  const asked = mount([change(1)], 1);
  await waitFor(() => expect(screen.getByText("Leadership")).toBeTruthy());
  expect(asked.find((a) => a.what === "page_activity")?.params).toMatchObject({
    container: "LEAD",
    kinds: "created,saved",
  });
  // THE PAGE LIST ONLY FOR THE TRASH, never for the rows: it is title-ordered.
  expect(asked.filter((a) => a.what === "pages").map((a) => a.params)).toEqual([
    { container: "LEAD", status: "trashed", limit: 500 },
  ]);
});

// A TRASHED PAGE IS NOT WRITTEN HERE. The container's count leaves the trash
// out, so a row for a trashed page stands under a count that does not include
// it — and, counted against that number, hides the link to the rest.
test("a trashed page's writes are not the container's", async () => {
  const trashed = Array.from({ length: 10 }, (_, i) =>
    change(i, { page_id: `t-${i}`, title: `Trashed ${i}`, actor: `binned-${i}` }),
  );
  const live = [change(20), change(21), change(22)];
  mount([...trashed, ...live], 3, undefined, { trashed: trashed.map((c) => c.page_id) });
  expect(await screen.findByText("Page 20")).toBeTruthy();
  expect(screen.getByText("Page 22")).toBeTruthy();
  expect(screen.queryByText(/Trashed \d/)).toBeNull();
  expect(screen.queryByText("binned-0")).toBeNull();
  expect(screen.getByText("seat-20")).toBeTruthy();
  expect(screen.queryByRole("link", { name: /in this container/ })).toBeNull();
});

test("a trash too large for one read is said, not assumed empty", async () => {
  mount([change(1)], 1, undefined, { truncated: true });
  expect(await screen.findByText("Page 1")).toBeTruthy();
  expect(screen.getByText(/more pages in its trash than one read returns/)).toBeTruthy();
});

test("an unread container is not one nobody has written in", async () => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  const store = new Store();
  store.applyHealth({ status: "healthy" });
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    what === "containers"
      ? Promise.resolve({ containers: [{ key: "LEAD", name: "Leadership", pages: 4 }] })
      : new Promise(() => {});
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <ContainerPeek id="LEAD" />
      </Router>
    </ClientContext.Provider>,
  );
  expect(await screen.findByText("Leadership")).toBeTruthy();
  expect(screen.queryByText(/Nobody has written/)).toBeNull();
  expect(screen.queryByText(/Nothing written here/)).toBeNull();
});

// THE LINK NAMES THE CONTAINER'S COUNT. It said "42 more pages" off a read of
// fifty on a container of four hundred.
test("a large container is counted by its own count, and its browse holds the rest", async () => {
  mount(
    Array.from({ length: 12 }, (_, i) => change(i)),
    400,
  );
  expect(await screen.findByText("Page 0")).toBeTruthy();
  // EIGHT ROWS, the newest writes first.
  expect(screen.queryByText("Page 7")).toBeTruthy();
  expect(screen.queryByText("Page 8")).toBeNull();
  const link = screen.getByRole("link", { name: /All 400 pages in this container/ });
  expect(link.getAttribute("href")).toContain("knowledge/LEAD");
  // EVERY KIND: the browse's default leaves the tool-skill pages out, and the
  // count does not.
  expect(link.getAttribute("href")).toContain("kind=all");
});

test("a container the rail shows whole offers no link to more of it", async () => {
  mount([change(1), change(2)], 2);
  expect(await screen.findByText("Page 1")).toBeTruthy();
  expect(screen.queryByRole("link", { name: /in this container/ })).toBeNull();
});

// EVERY WRITER THE READ SAW IS NAMED. A `+N more` on an inert span left the
// rest on no surface at all.
test("every writer the feed names is drawn, and a cut feed says whose it is", async () => {
  mount(
    Array.from({ length: 12 }, (_, i) => change(i)),
    400,
    "900",
  );
  for (let i = 0; i < 12; i++) {
    expect(await screen.findByText(`seat-${i}`), `seat-${i}`).toBeTruthy();
  }
  expect(screen.queryByText(/more$/)).toBeNull();
  // THE CUT IN THE FOOTER, since a subtitle truncates by contract.
  expect(screen.getByText(/The newest 12 writes; there are more\./)).toBeTruthy();
});

test("a page's writes fold into one row, and a purged page is not linked", () => {
  const rows = writtenPages([
    change(1),
    change(1, { id: "c-1b", kind: "created" }),
    change(2, { title: "", container: "" }),
    change(3),
  ]);
  expect(rows.map((r) => r.page_id)).toEqual(["p-1", "p-3"]);
});
