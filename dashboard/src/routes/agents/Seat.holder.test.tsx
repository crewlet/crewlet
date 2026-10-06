/**
 * A human seat's page says who holds it — or that nobody does, with the
 * invitation that would fill it.
 *
 * The chart cannot say who holds a human seat: the identity directory does
 * (`GET /iam/seats`). The read takes `people:manage` or `audit:read`, so it is
 * asked only of a reader holding one; the Invite button is drawn only for
 * `people:manage`; and the invitation it opens is for THIS seat.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { SeatScreen } from "./Seat.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { InboxCountsProvider } from "~/lib/useInboxCounts.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

const derivedSeat = (handle: string, name: string) => ({
  handle,
  name,
  kind: "human",
  placed_by_ref: false,
  manager: "",
  managers: null,
  reports: null,
  auto_reports: null,
  onboarding_chain: null,
});

const COMPANY = {
  roles: [
    { name: "Ana", handle: "ana", kind: "human" },
    { name: "Sam Support", handle: "sam", kind: "human" },
  ],
  units: [],
  derived: { units: [], seats: [derivedSeat("ana", "Ana"), derivedSeat("sam", "Sam Support")] },
};

function json(status: number, payload: unknown): Response {
  return new Response(JSON.stringify(payload), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

/** `/iam/seats` answering: Ana's seat held, Sam's nobody's. Every request recorded. */
function engine() {
  const sent: { method: string; path: string; body: unknown }[] = [];
  const spy = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(String(input), "http://engine.test");
    const method = (init?.method ?? "GET").toUpperCase();
    sent.push({
      method,
      path: url.pathname,
      body: typeof init?.body === "string" ? JSON.parse(init.body) : undefined,
    });
    if (method === "POST" && url.pathname === "/iam/invitations") {
      return Promise.resolve(
        json(201, { id: "inv", url: "https://x/dashboard#/invite/inv.s", expires_at: "" }),
      );
    }
    if (url.pathname === "/iam/seats") {
      const unheld = { handle: "sam", name: "Sam Support" };
      return Promise.resolve(
        json(200, {
          seats: url.searchParams.get("unheld")
            ? [unheld]
            : [
                {
                  handle: "ana",
                  name: "Ana",
                  holder: { person: "p-ana", login: "ana.lee", stage: "suspended" },
                },
                unheld,
              ],
        }),
      );
    }
    return Promise.resolve(json(404, { error: "no_route" }));
  });
  Object.defineProperty(globalThis, "fetch", { writable: true, value: spy });
  return sent;
}

/** What the profile's other questions are answered, in the wire's shapes. */
const ANSWERS: Record<string, unknown> = {
  work_items: { items: [], total_hint: 0 },
  work_inbox: { handle: "ana", notices: [], primary_reasons: [], unread: 0, primary: 0 },
  seat_activity: { since: "", until: "", days: 7, seats: [], quantile_resolution: 0.06 },
  turns: { turns: [], next: null },
};

async function mount(handle: string, grants: string[]) {
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    what === "viewer"
      ? Promise.resolve({ login: "ana.lee", grants, handle: "ana", owner: "ana", acts: [] })
      : Promise.resolve(ANSWERS[what] ?? {});
  store.applyOrg(COMPANY as never);
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <ViewerProvider>
          <InboxCountsProvider>
            <SeatScreen handle={handle} />
          </InboxCountsProvider>
        </ViewerProvider>
      </Router>
    </ClientContext.Provider>,
  );
  await act(async () => {});
  await act(async () => {});
}

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
});

afterEach(() => {
  cleanup();
  location.hash = "";
  vi.restoreAllMocks();
});

test("a vacant seat offers its invitation to an administrator, prefilled with the seat", async () => {
  location.hash = "#/agents/seats/sam";
  const sent = engine();
  await mount("sam", ["state:read", "people:manage"]);
  const note = (await screen.findByText("Nobody holds this seat.")).closest(
    ".crewlet-callout",
  ) as HTMLElement;
  fireEvent.click(await within(note).findByRole("button", { name: "Invite" }));
  const dialog = await screen.findByRole("dialog", { name: "Invite a person" });
  fireEvent.change(within(dialog).getByLabelText("Email address"), {
    target: { value: "sam@example.com" },
  });
  fireEvent.click(within(dialog).getByRole("button", { name: "Invite" }));
  await act(async () => {});
  const post = sent.find((s) => s.method === "POST");
  expect(post?.body).toEqual({ email: "sam@example.com", grants: [], seat: "sam" });
});

test("an auditor reads the fact and is offered no invitation", async () => {
  location.hash = "#/agents/seats/sam";
  engine();
  await mount("sam", ["state:read", "audit:read"]);
  const note = (await screen.findByText("Nobody holds this seat.")).closest(
    ".crewlet-callout",
  ) as HTMLElement;
  expect(within(note).queryByRole("button")).toBeNull();
});

test("a held seat names its holder and their stage", async () => {
  location.hash = "#/agents/seats/ana";
  engine();
  await mount("ana", ["state:read", "audit:read"]);
  const note = (await screen.findByText("ana.lee")).closest(".crewlet-callout") as HTMLElement;
  expect(note.textContent).toMatch(/Held by ana\.lee \(suspended\)/);
});

// THE READ IS ASKED ONLY OF A READER IT ANSWERS: anybody else would be handed
// a refusal on a page they came to for something else.
test("a reader holding neither grant is never asked the directory", async () => {
  location.hash = "#/agents/seats/sam";
  const sent = engine();
  await mount("sam", ["state:read"]);
  expect(sent.some((s) => s.path === "/iam/seats")).toBe(false);
  expect(screen.queryByText("Nobody holds this seat.")).toBeNull();
});
