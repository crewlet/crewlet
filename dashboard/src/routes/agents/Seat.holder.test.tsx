/**
 * A human seat's page says what holds it — a person, the open invitation that
 * names it, or nobody — with the ways to fill or free it.
 *
 * The chart cannot say who holds a human seat: the identity directory does
 * (`GET /iam/seats`). The read takes `people:manage` or `audit:read`, so it is
 * asked only of a reader holding one; the gestures are drawn only for
 * `people:manage`; and what they open is for THIS seat — every person holds
 * one, so the seat is stated, never chosen. A read that failed is said.
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
    { name: "Lee Ops", handle: "lee", kind: "human" },
  ],
  units: [],
  derived: {
    units: [],
    seats: [
      derivedSeat("ana", "Ana"),
      derivedSeat("sam", "Sam Support"),
      derivedSeat("lee", "Lee Ops"),
    ],
  },
};

function json(status: number, payload: unknown): Response {
  return new Response(JSON.stringify(payload), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

/**
 * `/iam/seats` answering: Ana's seat held, Lee's invited, Sam's nobody's — or
 * `failing`, when a case wants the read refused. Every request recorded.
 */
function engine(failing?: Response) {
  const sent: { method: string; path: string; key: string | null; body: unknown }[] = [];
  const spy = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(String(input), "http://engine.test");
    const method = (init?.method ?? "GET").toUpperCase();
    sent.push({
      method,
      path: url.pathname,
      key: ((init?.headers ?? {}) as Record<string, string>)["Idempotency-Key"] ?? null,
      body: typeof init?.body === "string" ? JSON.parse(init.body) : undefined,
    });
    if (method === "POST" && url.pathname === "/iam/invitations") {
      return Promise.resolve(
        json(201, { id: "inv", url: "https://x/dashboard#/invite/inv.s", expires_at: "" }),
      );
    }
    if (method === "POST" && url.pathname === "/iam/people") {
      return Promise.resolve(
        json(201, {
          id: "p-sam",
          kind: "person",
          login: "sam.ito",
          seat: "sam",
          credential: "c-first",
          url: "https://x/dashboard#/reset/c-first.s",
          expires_at: "2026-10-14T09:00:00Z",
        }),
      );
    }
    if (method === "DELETE" && url.pathname === "/iam/invitations/inv-7") {
      return Promise.resolve(json(200, { id: "inv-7", outcome: "applied" }));
    }
    if (url.pathname === "/iam/seats") {
      if (failing) return Promise.resolve(failing.clone());
      const unheld = { handle: "sam", name: "Sam Support" };
      return Promise.resolve(
        json(200, {
          seats: url.searchParams.get("unheld")
            ? [unheld]
            : [
                {
                  handle: "ana",
                  name: "Ana",
                  holder: { person: "p-ana", kind: "person", login: "ana.lee", stage: "suspended" },
                },
                {
                  handle: "lee",
                  name: "Lee Ops",
                  invitation: {
                    id: "inv-7",
                    email: "lee@example.com",
                    expires_at: "2026-10-14T09:00:00Z",
                  },
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

/** The seat the reader holds: Ana's own unless a test names another. */
async function mount(handle: string, grants: string[], as = "ana") {
  const store = new Store();
  store.setConnected(true);
  const socket = new LiveSocket(store);
  const viewer = {
    login: as === "ana" ? "ana.lee" : `${as}.reader`,
    grants,
    handle: as,
    owner: as,
    acts: ["create_work_item", "update_work_item"],
  };
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    what === "viewer" ? Promise.resolve(viewer) : Promise.resolve(ANSWERS[what] ?? {});
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

// THE INVITATION IS FOR THIS SEAT, stated rather than chosen: every person
// holds one, so there is no select to leave empty. Mutation: send the body
// without the seat and the engine refuses `seat_required`.
test("a vacant seat offers its invitation to an administrator, for this seat", async () => {
  location.hash = "#/agents/seats/sam";
  const sent = engine();
  await mount("sam", ["state:read", "people:manage"]);
  const note = (await screen.findByText("Nobody holds this seat.")).closest(
    ".crewlet-callout",
  ) as HTMLElement;
  fireEvent.click(await within(note).findByRole("button", { name: "Invite" }));
  const dialog = await screen.findByRole("dialog", { name: "Invite a person to Sam Support" });
  expect(within(dialog).queryByLabelText("Seat")).toBeNull();
  fireEvent.change(within(dialog).getByLabelText("Email address"), {
    target: { value: "sam@example.com" },
  });
  fireEvent.click(within(dialog).getByRole("button", { name: "Invite" }));
  await act(async () => {});
  const post = sent.find((s) => s.method === "POST");
  expect(post?.body).toEqual({ email: "sam@example.com", grants: [], seat: "sam" });
});

// THE OTHER WAY A PERSON COMES TO EXIST: created on the seat, keyed — the
// engine derives the person and their first password link from the key —
// and the link the engine answers is shown once, beside the login it took.
// Mutation: post the create unkeyed and a retry makes a second person.
test("a vacant seat creates a person on itself, keyed, and shows their first password link", async () => {
  location.hash = "#/agents/seats/sam";
  const sent = engine();
  await mount("sam", ["state:read", "people:manage"]);
  const note = (await screen.findByText("Nobody holds this seat.")).closest(
    ".crewlet-callout",
  ) as HTMLElement;
  fireEvent.click(await within(note).findByRole("button", { name: "Create person" }));
  const dialog = await screen.findByRole("dialog", { name: "Create a person on Sam Support" });
  fireEvent.change(within(dialog).getByLabelText("Email address"), {
    target: { value: "sam.ito@example.com" },
  });
  fireEvent.click(within(dialog).getByRole("button", { name: "Create" }));
  await act(async () => {});
  await act(async () => {});
  const post = sent.find((s) => s.method === "POST");
  expect(post?.path).toBe("/iam/people");
  expect(post?.key).toMatch(
    /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/,
  );
  expect(post?.body).toEqual({
    kind: "person",
    seat: "sam",
    email: "sam.ito@example.com",
    grants: [],
  });
  expect(within(dialog).getByText("https://x/dashboard#/reset/c-first.s")).toBeTruthy();
  expect(within(dialog).getByText("sam.ito")).toBeTruthy();
});

// AN INVITED SEAT IS NOT VACANT: it names the invitation that holds it and
// offers its cancellation — never a second invitation onto it.
test("an invited seat names its invitation, and cancelling it frees the seat", async () => {
  location.hash = "#/agents/seats/lee";
  const sent = engine();
  await mount("lee", ["state:read", "people:manage"]);
  const note = (await screen.findByText("lee@example.com")).closest(
    ".crewlet-callout",
  ) as HTMLElement;
  expect(within(note).queryByRole("button", { name: "Invite" })).toBeNull();
  fireEvent.click(within(note).getByRole("button", { name: "Cancel invitation" }));
  const dialog = await screen.findByRole("dialog", { name: "Cancel this invitation?" });
  expect(dialog.textContent).toMatch(/the seat Lee Ops are free again/);
  fireEvent.click(within(dialog).getByRole("button", { name: "Cancel invitation" }));
  await act(async () => {});
  expect(sent.filter((s) => s.method !== "GET").map((s) => [s.method, s.path])).toEqual([
    ["DELETE", "/iam/invitations/inv-7"],
  ]);
});

// A READ THAT FAILED IS SAID, not drawn as nothing: the gestures live here,
// and a page without them said nothing about why. Mutation: return null on
// an error and the banner is gone.
test("a seat listing that failed is said on the page", async () => {
  location.hash = "#/agents/seats/sam";
  engine(json(500, { error: "internal", message: "boom" }));
  await mount("sam", ["state:read", "people:manage"]);
  expect(await screen.findByText(/The engine tried to answer and failed/)).toBeTruthy();
  expect(screen.queryByText("Nobody holds this seat.")).toBeNull();
  expect(screen.queryByRole("button", { name: "Invite" })).toBeNull();
});

test("an auditor reads the fact and is offered no gesture", async () => {
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
  await mount("ana", ["state:read", "audit:read"], "sam");
  const note = (await screen.findByText("ana.lee")).closest(".crewlet-callout") as HTMLElement;
  expect(note.textContent).toMatch(/^Held by ana\.lee \(suspended\)/);
  expect(screen.getByText("Nothing open is assigned to them")).toBeDefined();
  const message = screen.getByRole("button", { name: /^Message/ });
  expect(message.getAttribute("aria-disabled")).not.toBe("true");
});

// YOUR OWN SEAT SPEAKS TO YOU, the whole page and not only its day: "Your day
// … waiting on you" sat beside "Held by jane.doe" and "Nothing open is
// assigned to them", and the header offered to Message yourself — an ask only
// you could answer. The CONTROL is the same seat read by somebody else, above.
// Mutation: compare the seat with anybody but the reader's own and every line
// here goes red.
test("the holder reading their own seat is spoken to, and is not offered to message it", async () => {
  location.hash = "#/agents/seats/ana";
  engine();
  await mount("ana", ["state:read", "audit:read"]);
  const note = (await screen.findByText("ana.lee")).closest(".crewlet-callout") as HTMLElement;
  expect(note.textContent).toMatch(/^Held by you, as ana\.lee \(suspended\)/);
  expect(screen.getByText("Nothing open is assigned to you")).toBeDefined();
  expect(screen.queryByText("Nothing open is assigned to them")).toBeNull();
  const message = screen.getByRole("button", { name: /^Message/ });
  expect(message.getAttribute("aria-disabled")).toBe("true");
  expect(message.getAttribute("title")).toMatch(/your own seat/);
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
