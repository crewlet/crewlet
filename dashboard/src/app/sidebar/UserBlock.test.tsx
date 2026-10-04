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
 * reloaded past; the second-factor gestures are offered to a person's session
 * and to nothing else; and a session the viewer never answers for is still
 * offered its sign-outs.
 */

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
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
import { dates, reloadForTest, zone, zoneIsChosen } from "~/lib/prefs.ts";
import { currentReader, noteReader } from "~/lib/reader.ts";
import { recentsKey } from "~/lib/recents.ts";
import { page } from "~/lib/session.ts";
import { starsKey } from "~/lib/starred.ts";
import { STORAGE_KEYS } from "~/lib/storage.ts";
import type { ViewerState } from "~/lib/viewer.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
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

/** The block where it lives: under a client (its session read is the shared REST read), a router and a toaster. */
function block(viewer: ViewerState, seatName = ""): ReactNode {
  const store = new Store();
  const socket = new LiveSocket(store);
  return (
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <ToastProvider>
          <UserBlock viewer={viewer} seatName={seatName} />
        </ToastProvider>
      </Router>
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

  test("nobody and not-yet-known are their own sentences", () => {
    expect(whoLine({ ...nobody, anonymous: true }, "").name).toBe("Not signed in");
    expect(whoLine({ ...nobody, loading: true }, "").name).toBe("Checking who you are");
  });

  test("only a person is a link, and it goes to their seat", () => {
    render(block(ada, "Founder"));
    const link = screen.getByRole("link", { name: /Ada Lovelace/ });
    expect(link.getAttribute("href")).toBe("#/agents/seats/ada");
    // THE GRANTS RIDE BESIDE THE NAME, where a reader asking "why can I not
    // open this" finds them.
    expect(link.getAttribute("title")).toBe("Holds state:read, work:write");
    cleanup();
    render(block({ ...nobody, anonymous: true }));
    expect(screen.queryByRole("link")).toBeNull();
    expect(screen.getByText("Not signed in")).toBeTruthy();
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
  // by here, since no sign-in in it said — and one already recorded is never
  // overwritten.
  test("the session it finds is adopted as the tab's reader, where none is recorded", async () => {
    render(block(ada));
    await waitFor(() => expect(currentReader()).toBe("p-1"));
    cleanup();
    noteReader("p-9");
    render(block(ada));
    await waitFor(() => expect(vi.mocked(fetch)).toHaveBeenCalledTimes(2));
    expect(currentReader()).toBe("p-9");
  });
});

describe("what the account offers", () => {
  test("a person signed in with a session: their proof and both sign-outs", () => {
    expect(accountOf(ada, PERSON)).toEqual({
      kind: "session",
      login: "ada.lovelace",
      proof: true,
    });
  });

  // A MACHINE HOLDS NO SECOND FACTOR, and a dialog the engine would refuse
  // is a control that lies — the Tier A token's own session is the case.
  test("a machine is offered no proof", () => {
    const machine = { ...PERSON, kind: "machine", expires_at: undefined };
    expect(accountOf({ ...ada, login: "token:ops", unbound: true }, machine)).toEqual({
      kind: "session",
      login: "token:ops",
      proof: false,
    });
    // NOR IS A SESSION NOBODY HAS ANSWERED FOR YET.
    expect(accountOf(ada, null)).toMatchObject({ proof: false });
  });

  // THE VIEWER IS A SOCKET QUESTION, and a person the socket refuses — a seat
  // taken out of the chart, a session without `state:read` — never has it
  // answered. The menu drew nothing until it was, so those people had no way
  // to end their own session; the session route still answers them.
  test("a session the viewer never answers for is still offered its sign-outs", () => {
    const waiting = { ...nobody, loading: true };
    expect(accountOf(waiting, PERSON)).toEqual({
      kind: "session",
      login: "ada.lovelace",
      proof: false,
    });
    // NOTHING ANSWERED AT ALL IS STILL NOTHING.
    expect(accountOf(waiting, null)).toBeNull();
  });

  test("nobody at all is offered a way to sign in, back to where they are", async () => {
    location.hash = "#/work";
    actions({ kind: "nobody" });
    fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
    await waitFor(() => expect(location.hash).toBe(`#/login?next=${encodeURIComponent("#/work")}`));
  });

  test("the proof gestures are drawn for a person and for nothing else", () => {
    const { onFactor, onCodes } = actions({ kind: "session", login: "ada.lovelace", proof: true });
    fireEvent.click(screen.getByRole("button", { name: "Two-step verification…" }));
    fireEvent.click(screen.getByRole("button", { name: "New recovery codes…" }));
    expect(onFactor).toHaveBeenCalledOnce();
    expect(onCodes).toHaveBeenCalledOnce();
    cleanup();
    actions({ kind: "session", login: "token:ops", proof: false });
    expect(screen.queryByRole("button", { name: "Two-step verification…" })).toBeNull();
    expect(screen.getByRole("button", { name: "Sign out" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Sign out everywhere" })).toBeTruthy();
  });
});

/** The account's gestures, drawn on their own with a toaster to report into. */
function actions(account: Account) {
  const onFactor = vi.fn();
  const onCodes = vi.fn();
  render(
    <Router>
      <ToastProvider>
        <LayerHost>
          <AccountActions
            account={account}
            grants="Holds state:read"
            onFactor={onFactor}
            onCodes={onCodes}
          />
        </LayerHost>
      </ToastProvider>
    </Router>,
  );
  return { onFactor, onCodes };
}

const signedIn: Account = { kind: "session", login: "ada.lovelace", proof: true };

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
    actions(signedIn);
    fireEvent.click(screen.getByRole("button", { name: "Sign out" }));
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
    engine({ "POST /auth/logout": "offline" });
    sessionStorage.setItem("crewlet_org_draft", "{}");
    localStorage.setItem(recentsKey("p-1"), "[]");
    actions(signedIn);
    fireEvent.click(screen.getByRole("button", { name: "Sign out" }));
    expect(await screen.findByText(/Signing out did not go through/)).toBeDefined();
    expect(reloads).not.toHaveBeenCalled();
    expect(sessionStorage.getItem("crewlet_org_draft")).toBe("{}");
    expect(localStorage.getItem(recentsKey("p-1"))).toBe("[]");
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
    actions(signedIn);
    fireEvent.click(screen.getByRole("button", { name: "Sign out everywhere" }));
    expect(await screen.findByText(/Signing out everywhere did not go through/)).toBeDefined();
    expect(reloads).not.toHaveBeenCalled();
  });

  test("everywhere, confirmed, reloads into the sign-in too", async () => {
    engine({ "POST /auth/logout/all": { status: 200, body: {} } });
    actions(signedIn);
    fireEvent.click(screen.getByRole("button", { name: "Sign out everywhere" }));
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
