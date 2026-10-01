/**
 * The identity menu: who you are, what you can change about how you sign in,
 * and the two ways a session ends by its owner's hand.
 *
 * The invariants: a sign-out ends in a RELOAD at the sign-in — nothing of the
 * last person's company left in the tab — and only once the engine has
 * answered; a revocation nobody can confirm is said rather than reloaded
 * past; the second-factor gestures are offered to a person's session and to
 * nothing else; and a new set of recovery codes, which retires the old one,
 * is issued only when asked.
 */

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import { IdentityMenu } from "./IdentityMenu.tsx";
import { Router } from "~/app/router.tsx";
import { currentReader, noteReader } from "~/lib/reader.ts";
import { recentsKey } from "~/lib/recents.ts";
import { page } from "~/lib/session.ts";
import { starsKey } from "~/lib/starred.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store, sessionRestored, type Viewer } from "~/protocol/index.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

type Answer = { status: number; body: unknown } | "offline";

interface Sent {
  method: string;
  path: string;
}

function engine(routes: Record<string, Answer>): Sent[] {
  const sent: Sent[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input), "http://engine.test");
      const method = (init?.method ?? "GET").toUpperCase();
      sent.push({ method, path: url.pathname });
      const answer = routes[`${method} ${url.pathname}`];
      if (answer === "offline") throw new TypeError("Failed to fetch");
      return new Response(JSON.stringify(answer?.body ?? { error: "no_route" }), {
        status: answer?.status ?? 404,
        headers: { "Content-Type": "application/json" },
      });
    }),
  );
  return sent;
}

const JANE: Viewer = {
  login: "jane.doe",
  grants: ["state:read", "work:write"],
  handle: "jane",
  name: "Jane Doe",
  kind: "human",
  owner: "jane",
};

const PERSON: Answer = {
  status: 200,
  body: {
    person: "p-1",
    login: "jane.doe",
    seat: "jane",
    kind: "person",
    expires_at: "2026-10-05T00:00:00Z",
    status: "signed_in",
  },
};

function mount(viewer: Viewer) {
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = async (what) =>
    what === "viewer" ? viewer : {};
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <ToastProvider>
          <LayerHost>
            <IdentityMenu />
          </LayerHost>
        </ToastProvider>
      </Router>
    </ClientContext.Provider>,
  );
}

/** The open menu's entries, in order, as they are drawn now. */
function entries(): string[] {
  return within(screen.getByRole("menu"))
    .getAllByRole("menuitem")
    .map((item) => item.querySelector(".crewlet-menu__label")?.textContent ?? "");
}

/** Opens the menu by the name its trigger shows, and answers its entries, in order. */
async function openMenu(name: string): Promise<string[]> {
  fireEvent.click(await screen.findByRole("button", { name }));
  await screen.findByRole("menu");
  return entries();
}

/**
 * Opens the menu ONCE its trigger is named, and waits for what it draws to
 * hold `entry` — one of the entries the session's own answer adds, which
 * arrives after the viewer that names the trigger.
 *
 * NO POLL'S BUDGET IS SPENT ON WORK THAT IS NOT WAITING. This was a `waitFor`
 * around `openMenu`, an ASYNC poll: its one-second budget covered the first
 * role query by accessible name the file makes — every element's role and
 * name computed cold, a quarter of a second idle, measured — and the click,
 * the render and a second query, and every retry clicked the trigger again.
 * With the suite sharing its cores that first pass alone outlasted the budget
 * and the case failed before its answer could be read. So the wait is on the
 * viewer's ANSWER, observed as the session being asked for (it is asked only
 * once a viewer is known), with a check that costs nothing; the trigger is
 * then named before the first query looks, which finds it on its first,
 * synchronous pass; and the menu, opened once, redraws as the session answers.
 */
async function openAnswered(name: string, entry: string): Promise<string[]> {
  await waitFor(() => expect(vi.mocked(fetch)).toHaveBeenCalled());
  await openMenu(name);
  await waitFor(() => expect(entries()).toContain(entry));
  return entries();
}

function choose(label: string) {
  const item = screen
    .getAllByRole("menuitem")
    .find((el) => el.querySelector(".crewlet-menu__label")?.textContent === label);
  if (!item) throw new Error(`no menu entry ${label}`);
  fireEvent.click(item);
}

let reloads: ReturnType<typeof vi.spyOn>;

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  Element.prototype.scrollIntoView = () => {};
  location.hash = "#/work";
  reloads = vi.spyOn(page, "reloadInto").mockImplementation(() => {});
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  sessionRestored();
  sessionStorage.clear();
  localStorage.clear();
  location.hash = "#/";
});

describe("what the menu offers", () => {
  test("a person signed in with a session: their seat, their second factor, and both sign-outs", async () => {
    engine({ "GET /auth/session": PERSON });
    mount(JANE);
    expect(await openAnswered("Jane Doe", "Two-step verification…")).toEqual([
      "Your seat",
      "Two-step verification…",
      "New recovery codes…",
      "Sign out",
      "Sign out everywhere",
    ]);
  });

  // A MACHINE HOLDS NO SECOND FACTOR, and a dialog the engine would refuse
  // is a control that lies — the Tier A token's own session is the case.
  test("a machine is offered no second factor, and an unbound one is told it holds no seat", async () => {
    engine({
      "GET /auth/session": {
        status: 200,
        body: { person: "t-1", login: "token:ops", kind: "machine", status: "signed_in" },
      },
    });
    mount({ ...JANE, login: "token:ops", handle: "", name: "", kind: "", owner: "token:ops" });
    // WAIT FOR THE SESSION to have answered, so the absence below is a
    // decision rather than a read still in flight.
    await waitFor(() => expect(vi.mocked(fetch)).toHaveBeenCalled());
    await act(() => new Promise((settled) => setTimeout(settled, 0)));
    expect(await openMenu("token:ops")).toEqual([
      "Not bound to a seat",
      "Sign out",
      "Sign out everywhere",
    ]);
  });

  // THE VIEWER IS A SOCKET QUESTION, and a person the socket refuses — a seat
  // taken out of the chart, a session without `state:read` — never has it
  // answered. The menu drew nothing until it was, so those people had no way
  // to end their own session; the session route still answers them.
  test("a session the viewer never answers for is still offered both sign-outs", async () => {
    engine({ "GET /auth/session": PERSON, "POST /auth/logout": { status: 200, body: {} } });
    const store = new Store();
    const socket = new LiveSocket(store);
    (socket as unknown as { query: () => Promise<unknown> }).query = () => new Promise(() => {});
    render(
      <ClientContext.Provider value={{ store, socket }}>
        <Router>
          <ToastProvider>
            <LayerHost>
              <IdentityMenu />
            </LayerHost>
          </ToastProvider>
        </Router>
      </ClientContext.Provider>,
    );
    expect(await openMenu("jane.doe")).toEqual(["Sign out", "Sign out everywhere"]);
    choose("Sign out");
    await waitFor(() => expect(reloads).toHaveBeenCalledWith("#/login"));
  });

  // A TAB OPENED WITH A SESSION ALREADY IN THE BROWSER learns who it is read
  // by here, since no sign-in in it said — and a sign-in later in this tab is
  // decided against it. One already recorded is never overwritten.
  test("the session it finds is adopted as the tab's reader, where none is recorded", async () => {
    engine({ "GET /auth/session": PERSON });
    mount(JANE);
    await waitFor(() => expect(currentReader()).toBe("p-1"));

    cleanup();
    noteReader("p-9");
    mount(JANE);
    await openMenu("Jane Doe");
    expect(currentReader()).toBe("p-9");
  });

  test("nobody at all is offered a way to sign in, back to where they are", async () => {
    engine({});
    mount({ ...JANE, login: "", handle: "", name: "", kind: "", owner: "", grants: [] });
    fireEvent.click(await screen.findByRole("button", { name: "Sign in" }));
    await waitFor(() => expect(location.hash).toBe(`#/login?next=${encodeURIComponent("#/work")}`));
  });
});

describe("signing out", () => {
  // THE TAB'S OWN STORAGE GOES WITH THE SESSION, because a reload keeps it:
  // a builder draft kept for the person leaving would otherwise be offered to
  // whoever signs in here next, as theirs.
  test("posts, forgets the tab's storage, and reloads into the sign-in", async () => {
    const sent = engine({
      "GET /auth/session": PERSON,
      "POST /auth/logout": { status: 200, body: {} },
    });
    sessionStorage.setItem("crewlet_org_draft", "{}");
    // AND THE BROWSER'S LISTS, which outlive the tab: a key per reader kept
    // them out of the next person's rail and not out of their browser.
    for (const key of [recentsKey("p-1"), starsKey("p-1"), recentsKey("p-9")]) {
      localStorage.setItem(key, "[]");
    }
    mount(JANE);
    await openMenu("Jane Doe");
    choose("Sign out");
    await waitFor(() => expect(reloads).toHaveBeenCalledWith("#/login"));
    expect(sent).toContainEqual({ method: "POST", path: "/auth/logout" });
    expect(sessionStorage.length).toBe(0);
    for (const key of [recentsKey("p-1"), starsKey("p-1"), recentsKey("p-9")]) {
      expect(localStorage.getItem(key), key).toBeNull();
    }
  });

  // NOTHING ANSWERED, so nothing cleared the cookie: reloading would put the
  // person back where they were while telling them they had left — and the
  // draft they are still signed in to keep stays where it was.
  test("a sign-out the engine never answered is said, and the page stays", async () => {
    engine({ "GET /auth/session": PERSON, "POST /auth/logout": "offline" });
    sessionStorage.setItem("crewlet_org_draft", "{}");
    localStorage.setItem(recentsKey("p-1"), "[]");
    mount(JANE);
    await openMenu("Jane Doe");
    choose("Sign out");
    expect(await screen.findByText(/Signing out did not go through/)).toBeDefined();
    expect(reloads).not.toHaveBeenCalled();
    expect(sessionStorage.getItem("crewlet_org_draft")).toBe("{}");
    expect(localStorage.getItem(recentsKey("p-1"))).toBe("[]");
  });

  test("everywhere: a revocation nobody can confirm is said, never reloaded past", async () => {
    engine({
      "GET /auth/session": PERSON,
      "POST /auth/logout/all": {
        status: 503,
        body: {
          error: "unavailable",
          message: "This node cannot answer that right now.",
          op_id: "op-1",
        },
      },
    });
    mount(JANE);
    await openMenu("Jane Doe");
    choose("Sign out everywhere");
    expect(await screen.findByText(/Signing out everywhere did not go through/)).toBeDefined();
    expect(reloads).not.toHaveBeenCalled();
  });

  test("everywhere, confirmed, reloads into the sign-in too", async () => {
    engine({ "GET /auth/session": PERSON, "POST /auth/logout/all": { status: 200, body: {} } });
    mount(JANE);
    await openMenu("Jane Doe");
    choose("Sign out everywhere");
    await waitFor(() => expect(reloads).toHaveBeenCalledWith("#/login"));
  });
});

describe("recovery codes", () => {
  // A NEW SET RETIRES THE OLD ONE, so opening the dialog to look must not.
  test("are issued only when asked, and shown the once", async () => {
    const sent = engine({
      "GET /auth/session": PERSON,
      "POST /auth/totp/recovery": { status: 200, body: { codes: ["a1b2-c3d4", "e5f6-g7h8"] } },
    });
    mount(JANE);
    await openAnswered("Jane Doe", "New recovery codes…");
    choose("New recovery codes…");
    await screen.findByRole("dialog", { name: "Recovery codes" });
    expect(sent.filter((s) => s.path === "/auth/totp/recovery")).toEqual([]);

    fireEvent.click(screen.getByRole("button", { name: "Issue new codes" }));
    expect(await screen.findByText("a1b2-c3d4")).toBeDefined();
    expect(screen.getByText("e5f6-g7h8")).toBeDefined();
    expect(sent.filter((s) => s.path === "/auth/totp/recovery")).toHaveLength(1);
  });

  // AN ANSWER NOBODY CAN CONFIRM may have stored a set, which retires the
  // one held — so "the set you hold still works" is not said.
  test("a set nobody can confirm was stored is not called harmless", async () => {
    engine({
      "GET /auth/session": PERSON,
      "POST /auth/totp/recovery": {
        status: 503,
        body: { error: "unavailable", message: "This node cannot answer that right now." },
      },
    });
    mount(JANE);
    await openAnswered("Jane Doe", "New recovery codes…");
    choose("New recovery codes…");
    fireEvent.click(await screen.findByRole("button", { name: "Issue new codes" }));
    expect(await screen.findByText(/It is not known whether a new set was stored/)).toBeDefined();
    expect(screen.queryByText(/still works/)).toBeNull();
  });
});
