/**
 * Who this browser is, what they can do about their own session, and the
 * reader's own preferences.
 *
 * THREE STATES, ONE OF THEM A PERSON, each a sentence rather than a blank
 * avatar — because "who does the engine think I am" is the question every
 * write this dashboard makes turns on — and the grants beside it, because
 * they decide which of those writes the reader reaches. And the zone and the
 * date format had no control anywhere before this block: `lib/prefs.ts` kept
 * both and nothing could set them.
 *
 * The session's own gestures were the page bar's identity menu, and they
 * keep its invariants here: a sign-out ends in a RELOAD at the sign-in —
 * nothing of the last person's company left in the tab — and only once the
 * engine has answered; a revocation nobody can confirm is said rather than
 * reloaded past; the menu links the Account page, where a person's proof
 * gestures live, and holds none of them itself; and a session the viewer never
 * answers for is still offered its sign-outs.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import {
  AccountActions,
  Preferences,
  UserBlock,
  accountOf,
  supportedZones,
  whoLine,
  type Account,
} from "./UserBlock.tsx";
import { Router } from "~/app/router.tsx";
import { SignOutEverywhereDialog } from "~/components/SignOutEverywhere.tsx";
import { dates, reloadForTest, zone, zoneIsChosen } from "~/lib/prefs.ts";
import { currentReader, noteReader } from "~/lib/reader.ts";
import { recentsKey } from "~/lib/recents.ts";
import { page } from "~/lib/session.ts";
import { starsKey } from "~/lib/starred.ts";
import { STORAGE_KEYS } from "~/lib/storage.ts";
import type { ViewerState } from "~/lib/viewer.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { SessionReading } from "~/lib/frameSession.ts";
import { LiveSocket, Store, sessionRestored, type SessionAnswer } from "~/protocol/index.ts";
import type { ReactNode } from "react";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

/** The block where it lives: under a client, the frame's session read, a router and a toaster. */
function block(viewer: ViewerState, seatName = ""): ReactNode {
  const store = new Store();
  const socket = new LiveSocket(store);
  return (
    <ClientContext.Provider value={{ store, socket }}>
      <SessionReading>
        <Router>
          <ToastProvider>
            <UserBlock viewer={viewer} seatName={seatName} />
          </ToastProvider>
        </Router>
      </SessionReading>
    </ClientContext.Provider>
  );
}

const nobody: ViewerState = {
  login: "",
  grants: [],
  operatesFleet: false,
  handle: "",
  owner: "",
  name: "",
  acts: [],
  project: "",
  kind: "",
  unbound: false,
  anonymous: false,
  loading: false,
  asking: false,
};
const ada: ViewerState = {
  ...nobody,
  login: "ada.lovelace",
  grants: ["state:read", "work:write"],
  handle: "ada",
  owner: "ada",
  name: "Ada Lovelace",
  kind: "human",
};

type Answer = { status: number; body: unknown } | "offline";

/** The engine's REST answers, by `METHOD /path`; anything else is a 404. */
function engine(routes: Record<string, Answer>): { method: string; path: string }[] {
  const sent: { method: string; path: string }[] = [];
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

const PERSON: SessionAnswer = {
  person: "p-1",
  login: "ada.lovelace",
  seat: "ada",
  kind: "person",
  expires_at: "2026-10-05T00:00:00Z",
  status: "signed_in",
};

let reloads: ReturnType<typeof vi.spyOn>;

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  localStorage.clear();
  reloadForTest();
  engine({ "GET /auth/session": { status: 200, body: PERSON } });
  reloads = vi.spyOn(page, "reloadInto").mockImplementation(() => {});
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  sessionRestored();
  sessionStorage.clear();
  localStorage.clear();
  reloadForTest();
  document.documentElement.removeAttribute("data-theme");
  document.documentElement.removeAttribute("data-density");
  location.hash = "#/";
});

describe("who the block says this is", () => {
  test("a bound person is their name, with their login and seat and never a bare handle", () => {
    expect(whoLine(ada, "Founder")).toEqual({
      name: "Ada Lovelace",
      detail: "ada.lovelace · Founder · human seat",
      grants: "Holds state:read, work:write",
    });
    // A seat whose name IS the person's says so once.
    expect(whoLine(ada, "Ada Lovelace").detail).toBe("ada.lovelace · human seat");
    expect(whoLine(ada, "Founder").detail).not.toContain("@");
  });

  // UNBOUND IS AN ORDINARY STATE — an operator outside the chart, a pipeline —
  // so it is said as a fact under the login the engine records them as.
  test("an unbound principal is its login, and says it holds no seat", () => {
    const r = whoLine({ ...nobody, login: "token:ops", unbound: true, grants: [] }, "");
    expect(r).toEqual({
      name: "token:ops",
      detail: "Not bound to a seat",
      grants: "Holds no grants",
    });
  });

  // AND BY THEIR OWN NAME where their row holds one, which no seat gives them:
  // the foot drew the login alone. The login stays beside it, as who the
  // engine records them as. Mutation: drop the name and the login is drawn.
  test("an unbound person is their own name, beside their login", () => {
    const r = whoLine({ ...nobody, login: "bob.smith", unbound: true, grants: [] }, "", {
      login: "bob.smith",
      name: "Bob Smith",
    });
    expect(r).toEqual({
      name: "Bob Smith",
      detail: "bob.smith · Not bound to a seat",
      grants: "Holds no grants",
    });
  });

  test("nobody and not-yet-known are their own sentences", () => {
    expect(whoLine({ ...nobody, anonymous: true }, "").name).toBe("Not signed in");
    expect(whoLine({ ...nobody, loading: true }, "").name).toBe("Checking who you are");
  });

  // A VIEWER THAT NEVER ANSWERS — the socket refused a person without
  // `state:read` — is no reason to keep asking: the session names them, and
  // what they hold. The CONTROL is the case above, with no session either.
  test("a session stands in for a viewer that has not answered", () => {
    expect(whoLine({ ...nobody, loading: true }, "", { login: "erin.ng", grants: [] })).toEqual({
      name: "erin.ng",
      detail: "Signed in",
      grants: "Holds no grants",
    });
    expect(
      whoLine({ ...nobody, loading: true }, "", {
        login: "erin.ng",
        name: "Erin Ng",
        grants: ["work:write"],
      }),
    ).toEqual({ name: "Erin Ng", detail: "erin.ng", grants: "Holds work:write" });
  });

  // THE NAME IS THE WAY TO THE READER'S OWN ACCOUNT, where their password
  // and second factor are. It went to the SEAT, which has neither, and a
  // company's people clicking their own name concluded the dashboard had no
  // two-step verification at all.
  test("the reader's name opens their own Account, never their seat", () => {
    render(block(ada, "Founder"));
    const link = screen.getByRole("link", { name: /Ada Lovelace/ });
    expect(link.getAttribute("href")).toBe("#/account");
    // THE GRANTS RIDE BESIDE THE NAME, where a reader asking "why can I not
    // open this" finds them.
    expect(link.getAttribute("title")).toBe("Holds state:read, work:write");
    cleanup();
    // UNBOUND HAS AN ACCOUNT TOO — a token's session is told there what it
    // is — so it is a link to the same page.
    render(block({ ...nobody, login: "token:ops", unbound: true, grants: ["state:read"] }));
    expect(screen.getByRole("link", { name: /token:ops/ }).getAttribute("href")).toBe("#/account");
    cleanup();
    // NOBODY has no account to open: the popover offers the sign-in.
    render(block({ ...nobody, anonymous: true }));
    expect(screen.queryByRole("link")).toBeNull();
    expect(screen.getByText("Not signed in")).toBeTruthy();
  });

  // A SESSION THE VIEWER HAS NOT ANSWERED FOR is somebody: it is named, and
  // its name opens its Account as a settled viewer's does — while nothing at
  // all has answered is not a link.
  test("a session standing in for the viewer opens its Account", async () => {
    render(block({ ...nobody, loading: true }));
    const link = await screen.findByRole("link", { name: /ada\.lovelace/ });
    expect(link.getAttribute("href")).toBe("#/account");
    cleanup();
    engine({ "GET /auth/session": "offline" });
    render(block({ ...nobody, loading: true }));
    expect(screen.getByText("Checking who you are")).toBeTruthy();
    expect(screen.queryByRole("link")).toBeNull();
  });

  // THE ONE BADGE THAT IS THE READER carries the accent's ring — and a badge
  // standing in for nobody, or for an answer still out, does not.
  test("the reader's own badge is ringed in the accent, once the engine has said who it is", () => {
    const ringed = () =>
      document.querySelectorAll(".user-block .crewlet-avatar--ring-brand").length;
    render(block(ada, "Founder"));
    expect(ringed()).toBe(1);
    cleanup();
    render(block({ ...nobody, login: "token:ops", unbound: true }));
    expect(ringed()).toBe(1);
    for (const unknown of [
      { ...nobody, anonymous: true },
      { ...nobody, loading: true },
    ]) {
      cleanup();
      render(block(unknown));
      expect(document.querySelector(".user-block .crewlet-avatar")).not.toBeNull();
      expect(ringed()).toBe(0);
    }
  });

  // A TAB OPENED WITH A SESSION ALREADY IN THE BROWSER learns who it is read
  // by here, since no sign-in in it said — and a tab read by somebody else is
  // never quietly theirs: it is handed over, reloaded with nothing kept.
  test("the session it finds is adopted as the tab's reader, where none is recorded", async () => {
    render(block(ada));
    await waitFor(() => expect(currentReader()).toBe("p-1"));
    expect(reloads).not.toHaveBeenCalled();
    cleanup();
    noteReader("p-9");
    render(block(ada));
    await waitFor(() => expect(reloads).toHaveBeenCalledTimes(1));
    expect(currentReader()).toBe("p-1");
  });
});

describe("what the account offers", () => {
  test("a signed-in reader is offered the menu under their own login", () => {
    expect(accountOf(ada, PERSON)).toEqual({ kind: "session", login: "ada.lovelace" });
    expect(accountOf({ ...ada, login: "token:ops", unbound: true }, null)).toEqual({
      kind: "session",
      login: "token:ops",
    });
  });

  // THE VIEWER IS A SOCKET QUESTION, and a person the socket refuses — a seat
  // taken out of the chart, a session without `state:read` — never has it
  // answered. The menu drew nothing until it was, so those people had no way
  // to end their own session; the session route still answers them.
  test("a session the viewer never answers for is still offered its sign-outs", () => {
    const waiting = { ...nobody, loading: true };
    expect(accountOf(waiting, PERSON)).toEqual({ kind: "session", login: "ada.lovelace" });
    // NOTHING ANSWERED AT ALL IS STILL NOTHING.
    expect(accountOf(waiting, null)).toBeNull();
  });

  test("nobody at all is offered a way to sign in, back to where they are", async () => {
    location.hash = "#/work";
    actions({ kind: "nobody" });
    fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
    await waitFor(() => expect(location.hash).toBe(`#/login?next=${encodeURIComponent("#/work")}`));
  });

  // THE PROOF GESTURES LIVE ON THE ACCOUNT PAGE, and the menu links it rather
  // than keeping a second copy of them — a link, so it can open in a tab, that
  // closes the menu it is in. The CONTROL is the menu still holding both
  // sign-outs, which stay here for the reader the socket refuses.
  test("the menu links the Account page and holds no proof gesture of its own", () => {
    const { onLeave } = actions({ kind: "session", login: "ada.lovelace" });
    const link = screen.getByRole("link", { name: "Account" });
    expect(link.getAttribute("href")).toBe("#/account");
    fireEvent.click(link);
    expect(onLeave).toHaveBeenCalledOnce();
    expect(screen.queryByRole("button", { name: /Two-step verification/ })).toBeNull();
    expect(screen.queryByRole("button", { name: /recovery codes/i })).toBeNull();
    expect(screen.getByRole("button", { name: "Sign out" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Sign out everywhere" })).toBeTruthy();
  });
});

/**
 * The live socket a sign-out holds, recording what it was asked: `hold` before
 * the request, `release` after one nothing answered.
 */
function heldSocket() {
  const asked: string[] = [];
  const socket = {
    hold: () => asked.push("hold"),
    release: () => asked.push("release"),
  };
  const value = { store: new Store(), socket } as unknown as { store: Store; socket: LiveSocket };
  return { asked, value };
}

/** The account's gestures, drawn on their own with a toaster to report into. */
function actions(account: Account) {
  const onLeave = vi.fn();
  const onSignOutEverywhere = vi.fn();
  const client = heldSocket();
  render(
    <ClientContext.Provider value={client.value}>
      <Router>
        <ToastProvider>
          <LayerHost>
            <AccountActions
              account={account}
              grants="Holds state:read"
              onLeave={onLeave}
              onSignOutEverywhere={onSignOutEverywhere}
            />
          </LayerHost>
        </ToastProvider>
      </Router>
    </ClientContext.Provider>,
  );
  return { onLeave, onSignOutEverywhere, asked: client.asked };
}

/** The confirmation signing out everywhere asks, drawn on its own. */
function everywhere() {
  const onClose = vi.fn();
  const client = heldSocket();
  render(
    <ClientContext.Provider value={client.value}>
      <LayerHost>
        <SignOutEverywhereDialog onClose={onClose} />
      </LayerHost>
    </ClientContext.Provider>,
  );
  return { onClose, asked: client.asked };
}

const signedIn: Account = { kind: "session", login: "ada.lovelace" };

describe("signing out", () => {
  // THE TAB'S OWN STORAGE GOES WITH THE SESSION, because a reload keeps it:
  // a builder draft kept for the person leaving would otherwise be offered to
  // whoever signs in here next, as theirs.
  test("posts, forgets the tab's storage, and reloads into the sign-in", async () => {
    const sent = engine({ "POST /auth/logout": { status: 200, body: {} } });
    sessionStorage.setItem("crewlet_org_draft", "{}");
    // AND THE BROWSER'S LISTS, which outlive the tab: a key per reader kept
    // them out of the next person's sidebar and not out of their browser.
    for (const key of [recentsKey("p-1"), starsKey("p-1"), recentsKey("p-9")]) {
      localStorage.setItem(key, "[]");
    }
    const { asked } = actions(signedIn);
    fireEvent.click(screen.getByRole("button", { name: "Sign out" }));
    await waitFor(() => expect(reloads).toHaveBeenCalledWith("#/login"));
    expect(sent).toContainEqual({ method: "POST", path: "/auth/logout" });
    // THE SOCKET STOPS DIALLING FIRST: the engine closes this session's socket
    // as it applies the sign-out, and that close re-dialled before the reload,
    // a handshake refused 401 on the way out.
    expect(asked).toEqual(["hold"]);
    expect(sessionStorage.length).toBe(0);
    for (const key of [recentsKey("p-1"), starsKey("p-1"), recentsKey("p-9")]) {
      expect(localStorage.getItem(key), key).toBeNull();
    }
  });

  // NOTHING ANSWERED, so nothing cleared the cookie: reloading would put the
  // person back where they were while telling them they had left — and the
  // draft they are still signed in to keep stays where it was.
  test("a sign-out the engine never answered is said, and the page stays", async () => {
    engine({ "POST /auth/logout": "offline" });
    sessionStorage.setItem("crewlet_org_draft", "{}");
    localStorage.setItem(recentsKey("p-1"), "[]");
    const { asked } = actions(signedIn);
    fireEvent.click(screen.getByRole("button", { name: "Sign out" }));
    expect(await screen.findByText(/Signing out did not go through/)).toBeDefined();
    expect(reloads).not.toHaveBeenCalled();
    // AND THE SOCKET IT HELD IS GIVEN BACK: the person is still signed in here.
    expect(asked).toEqual(["hold", "release"]);
    expect(sessionStorage.getItem("crewlet_org_draft")).toBe("{}");
    expect(localStorage.getItem(recentsKey("p-1"))).toBe("[]");
  });

  // EVERYWHERE IS ASKED FIRST, because it ends every personal token too: the
  // menu closes and asks, and nothing is sent until the person confirms.
  // Mutation: post from the menu's own click and the revocation is sent.
  test("everywhere: the menu closes and asks, sending nothing", () => {
    const sent = engine({ "POST /auth/logout/all": { status: 200, body: {} } });
    const { onLeave, onSignOutEverywhere } = actions(signedIn);
    fireEvent.click(screen.getByRole("button", { name: "Sign out everywhere" }));
    expect(onLeave).toHaveBeenCalledOnce();
    expect(onSignOutEverywhere).toHaveBeenCalledOnce();
    expect(sent.filter((s) => s.method === "POST")).toEqual([]);
  });

  test("everywhere: a revocation nobody can confirm is said, never reloaded past", async () => {
    engine({
      "POST /auth/logout/all": {
        status: 503,
        body: {
          error: "unavailable",
          message: "This node cannot answer that right now.",
          op_id: "op-1",
        },
      },
    });
    everywhere();
    const ask = await screen.findByRole("dialog", { name: "Sign out everywhere?" });
    expect(within(ask).getByText(/every personal access token you\s+minted/)).toBeDefined();
    fireEvent.click(within(ask).getByRole("button", { name: "Sign out everywhere" }));
    expect(await screen.findByText(/Signing out everywhere did not go through/)).toBeDefined();
    expect(reloads).not.toHaveBeenCalled();
  });

  test("everywhere, confirmed, reloads into the sign-in too", async () => {
    engine({ "POST /auth/logout/all": { status: 200, body: {} } });
    everywhere();
    const ask = await screen.findByRole("dialog", { name: "Sign out everywhere?" });
    fireEvent.click(within(ask).getByRole("button", { name: "Sign out everywhere" }));
    await waitFor(() => expect(reloads).toHaveBeenCalledWith("#/login"));
  });
});

// THE POPOVER OPENS WHERE THE SIDEBAR PUTS IT. jsdom has no layout, so a
// suite that merely clicks the trigger could not see that the panel was
// closed again on the frame it opened: the kit's placement reads rectangles,
// and at the sidebar's real coordinates (the trigger 26px wide at x=202, the
// panel 320 wide, a 1440x900 window) an end-aligned panel measured the anchor
// from a left edge off the window and closed as "anchor gone". So the
// rectangles are the real ones here.
describe("the preferences popover, laid out", () => {
  const rect = (x: number, y: number, width: number, height: number): DOMRect =>
    ({
      x,
      y,
      left: x,
      top: y,
      width,
      height,
      right: x + width,
      bottom: y + height,
      toJSON: () => ({}),
    }) as DOMRect;
  const window0 = { width: window.innerWidth, height: window.innerHeight };

  beforeEach(() => {
    Object.defineProperty(window, "innerWidth", { configurable: true, value: 1440 });
    Object.defineProperty(window, "innerHeight", { configurable: true, value: 900 });
    vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
      this: HTMLElement,
    ) {
      if (
        this.getAttribute("aria-label") === "Account and preferences" &&
        this.tagName === "BUTTON"
      ) {
        return rect(202, 860, 26, 26);
      }
      if (this.getAttribute("role") === "dialog") return rect(0, 0, 320, 420);
      return rect(0, 0, 0, 0);
    });
  });
  afterEach(() => {
    vi.restoreAllMocks();
    Object.defineProperty(window, "innerWidth", { configurable: true, value: window0.width });
    Object.defineProperty(window, "innerHeight", { configurable: true, value: window0.height });
  });

  test("pressed at the sidebar's foot, it stays open, above the trigger and on screen", async () => {
    render(block(ada, "Founder"));
    const trigger = screen.getByRole("button", { name: "Account and preferences" });
    fireEvent.click(trigger);
    const dialog = await screen.findByRole("dialog", { name: "Account and preferences" });
    // Still there once placement has run, and the trigger says so.
    await new Promise((r) => setTimeout(r, 20));
    expect(dialog.isConnected).toBe(true);
    expect(trigger.getAttribute("aria-expanded")).toBe("true");
    // Placed, not hidden: above the trigger, starting at its edge, inside the window.
    expect(dialog.style.visibility).not.toBe("hidden");
    const left = parseFloat(dialog.style.left);
    const top = parseFloat(dialog.style.top);
    expect(left).toBe(202);
    expect(left + 320).toBeLessThanOrEqual(1440);
    expect(top + 420).toBeLessThanOrEqual(860);
  });
});

describe("the preferences", () => {
  function open() {
    render(<Preferences />);
  }

  // THE THEME IS THREE CHOICES, and the third is what makes the popover the
  // place to set it: the one-press flip beside the name can only say light or
  // dark, and "follow the system" is a choice a reader has to be able to get
  // back to. Each choice is read back from `lib/prefs.ts` and from the root
  // attribute the stylesheet keys on — and "system" REMOVES the attribute, so
  // the media query decides and keeps deciding when the OS setting changes.
  test("the theme is light, dark or the system's, and each is kept", () => {
    open();
    const theme = screen.getByRole("radiogroup", { name: "Theme" });
    const pick = (name: string) => fireEvent.click(within(theme).getByRole("radio", { name }));

    pick("Dark");
    expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
    expect(localStorage.getItem(STORAGE_KEYS.theme)).toBe("dark");

    pick("Light");
    expect(document.documentElement.getAttribute("data-theme")).toBe("light");
    expect(localStorage.getItem(STORAGE_KEYS.theme)).toBe("light");

    pick("Follow the system");
    expect(document.documentElement.hasAttribute("data-theme")).toBe(false);
    expect(localStorage.getItem(STORAGE_KEYS.theme)).toBe("system");
    expect(
      within(theme).getByRole("radio", { name: "Follow the system" }).getAttribute("aria-checked"),
    ).toBe("true");
  });

  // DENSITY IS THE OTHER SWITCHER, and "normal" is the absence of the
  // attribute for the same reason: the default is the stylesheet's own.
  test("the density is a control, and it writes the preference", () => {
    open();
    const density = screen.getByRole("radiogroup", { name: "Density" });
    fireEvent.click(within(density).getByRole("radio", { name: "Compact" }));
    expect(document.documentElement.getAttribute("data-density")).toBe("compact");
    expect(localStorage.getItem(STORAGE_KEYS.density)).toBe("compact");
    fireEvent.click(within(density).getByRole("radio", { name: "Normal" }));
    expect(document.documentElement.hasAttribute("data-density")).toBe(false);
    expect(localStorage.getItem(STORAGE_KEYS.density)).toBe("normal");
  });

  // THE ZONE AND THE DATE FORMAT HAD NO CONTROL. Both were read by every
  // timestamp in the product, and a reader could set neither.
  test("the date format is a control, and it writes the preference", async () => {
    open();
    fireEvent.click(screen.getByRole("combobox", { name: "Dates" }));
    // The kit's listbox commits on the press, as a native select does.
    fireEvent.mouseDown(await screen.findByRole("option", { name: "2026-09-22" }));
    await waitFor(() => expect(dates()).toBe("iso"));
  });

  test("the zone is a control, empty is the browser's, and a choice is kept", async () => {
    open();
    expect(zoneIsChosen()).toBe(false);
    // A SEARCHABLE select: the trigger is a button, and the field inside the
    // list is the combobox.
    fireEvent.click(screen.getByRole("button", { name: "Time zone" }));
    fireEvent.change(await screen.findByPlaceholderText("Find a zone"), {
      target: { value: "UTC" },
    });
    const utc = await screen.findAllByRole("option", { name: "UTC" });
    fireEvent.mouseDown(utc[0]!);
    await waitFor(() => expect(zone()).toBe("UTC"));
    expect(zoneIsChosen()).toBe(true);
  });

  test("every zone offered is one this runtime can format in, UTC among them", () => {
    const zones = supportedZones();
    expect(zones.length).toBeGreaterThan(0);
    // The runtime's canonical list omits it; the reader reading logs wants it.
    expect(zones).toContain("UTC");
    for (const z of zones.slice(0, 50)) {
      expect(() => new Intl.DateTimeFormat("en", { timeZone: z }), z).not.toThrow();
    }
  });
});
