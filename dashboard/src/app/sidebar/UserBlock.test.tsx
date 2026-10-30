/**
 * Who this browser is, what they can do about their own session, and the
 * reader's own preferences.
 *
 * THREE STATES, ONE OF THEM A PERSON, each a sentence rather than a blank
 * avatar — because "who does the engine think I am" is the question every
 * write this dashboard makes turns on — and the grants beside it, because
 * they decide which of those writes the reader reaches. The preferences are
 * the mode, the density and the theme; the zone and the date's shape are the
 * browser's.
 *
 * The session's own gestures are the reader's menu, which the row itself
 * opens, and they keep their invariants: a sign-out ends in a RELOAD at the
 * sign-in (nothing of the last person's company left in the tab) and only
 * once the engine has answered; a revocation nobody can confirm is said
 * rather than reloaded past; the menu opens the Account page, where a
 * person's proof gestures live, and holds none of them itself; and a session
 * the viewer never answers for is still offered its sign-outs. The
 * preferences are their own popover beside the row, holding nothing else.
 */

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { themeRegistry } from "@crewlethq/tokens";
import { AccountRow, HIGHLIGHT_REST_MS, LayerHost, ToastProvider } from "@crewlethq/ui";

import {
  Preferences,
  UserBlock,
  accountOf,
  useAccountMenu,
  viaKind,
  viaLine,
  whoLine,
  type Account,
} from "./UserBlock.tsx";
import { Router } from "~/app/router.tsx";
import { SignOutEverywhereDialog } from "~/components/SignOutEverywhere.tsx";
import { reloadForTest } from "~/lib/prefs.ts";
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
  admin: false,
  worklet: false,
  doclet: false,
  chatlet: false,
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
  configWriter: true,
  configManagedBy: [],
  assistant: "disabled",
  assistantModel: null,
  chatVendors: [],
};
const ada: ViewerState = {
  ...nobody,
  login: "ada.lovelace",
  grants: ["worklet"],
  handle: "ada",
  owner: "ada",
  name: "Ada Lovelace",
  kind: "human",
};

/** A Tier A token bound to Miles's human seat, as `/viewer` answers it. */
const reviewToken: ViewerState = {
  ...nobody,
  login: "token:review",
  grants: ["admin"],
  handle: "miles",
  owner: "miles",
  name: "Miles",
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
  document.documentElement.removeAttribute("data-palette");
  location.hash = "#/";
});

describe("who the block says this is", () => {
  // THE READER IS CALLED BY THEIR OWN NAME, never their seat's: the seat's
  // name is the chart's name for a place, and often a role, so a person with
  // no name of their own was "Founder" here while their Account said they had
  // no name. The seat is said beside it, as the seat. Mutation: name the
  // reader by `viewer.name` (the seat's) and the second case goes red.
  test("a bound person is their own name, with their login and their seat beside it", () => {
    const own = { login: "ada.lovelace", name: "Ada Lovelace", seat: "ada" };
    expect(whoLine(ada, "Founder", own)).toEqual({
      name: "Ada Lovelace",
      detail: "ada.lovelace · Founder",
      grants: "Worklet",
    });
    // NO NAME OF THEIR OWN: their login, and the seat under it — never the
    // seat's name in their place.
    expect(whoLine(ada, "Founder", { login: "ada.lovelace" })).toEqual({
      name: "ada.lovelace",
      detail: "Founder",
      grants: "Worklet",
    });
    // A seat named as the person adds nothing to the login.
    expect(whoLine(ada, "Ada Lovelace", own).detail).toBe("ada.lovelace");
    // THE CHART UNREAD, the handle stands in, written as a handle.
    expect(whoLine({ ...ada, name: "" }, "", own).detail).toBe("ada.lovelace · @ada");
    // Nor is the seat's kind captioned at all: every binding is to a human
    // seat, and a viewer on any other is refused before it answers.
    expect(whoLine({ ...ada, kind: "agent" }, "Release Bot", own).detail).toBe(
      "ada.lovelace · Release Bot",
    );
  });

  // THE SECOND LINE IS THE LOGIN, and nothing beside it where the seat has no
  // role to add. Mutation: caption a person "human seat" and this goes red.
  test("a person with no role to show draws their login alone under their name", async () => {
    engine({ "GET /auth/session": { status: 200, body: { ...PERSON, name: "Ada Lovelace" } } });
    render(block(ada, "Ada Lovelace"));
    const who = await screen.findByRole("button", { name: /Ada Lovelace/ });
    // THE LOGIN IS DRAWN ONCE THE SESSION HAS ANSWERED, a commit after the
    // row: waited for, not read at once.
    await waitFor(() =>
      expect(who.querySelector(".crewlet-account-row__detail")?.textContent).toBe("ada.lovelace"),
    );
    expect(who.textContent).not.toContain("human seat");
  });

  // UNBOUND IS AN ORDINARY STATE — a Tier A token's session, a service
  // account — so it is said as a fact under the login the engine records them
  // as.
  test("an unbound principal is its login, and says it holds no seat", () => {
    const r = whoLine({ ...nobody, login: "token:ops", unbound: true, grants: [] }, "");
    expect(r).toEqual({
      name: "token:ops",
      detail: "No seat",
      grants: "Overview only",
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
      detail: "bob.smith · No seat",
      grants: "Overview only",
    });
  });

  // A CREDENTIAL ACTING AS A SEAT IS THE SEAT. A Tier A token bound to
  // Miles's seat read "token:review" over "Miles": the credential's spelling
  // as the name, and the place it acts for as the aside. It has no name of
  // its own, so it is called by the seat, and the line under it says how it
  // is signed in: the KIND of credential, with its name left to the title and
  // the menu. Mutation: name it by `readerName` and the first case reads
  // "token:review" again; say the whole line and it reads "via API token
  // review", which the rail's width cut short.
  test("a token bound to a seat is the seat, with how it is signed in under it", () => {
    const session = { login: "token:review", seat: "miles", kind: "person" };
    expect(whoLine(reviewToken, "Miles", session)).toEqual({
      name: "Miles",
      detail: "via API token",
      grants: "Admin",
    });
    // THE CHART UNREAD, the handle stands in, written as a handle.
    expect(whoLine({ ...reviewToken, name: "" }, "", session).name).toBe("@miles");
    // A SERVICE ACCOUNT BOUND TO A SEAT is the seat too.
    expect(
      whoLine({ ...reviewToken, login: "ci:deploy" }, "Miles", { login: "ci:deploy" }),
    ).toMatchObject({ name: "Miles", detail: "via service account" });
    // AND WHILE THE VIEWER HAS NOT ANSWERED, from the session alone.
    expect(whoLine({ ...nobody, loading: true }, "", { ...session, grants: ["admin"] })).toEqual({
      name: "@miles",
      detail: "via API token",
      grants: "Admin",
    });
  });

  // THE CREDENTIAL IS READ OFF THE LOGIN'S GRAMMAR, never the session's kind,
  // which is a person's for a seat-bound token. A person's login is never
  // said this way.
  test("how a credential is signed in is read off its login", () => {
    expect(viaLine("token:review")).toBe("via API token review");
    expect(viaLine("ci:deploy")).toBe("via service account ci:deploy");
    expect(viaLine("ada.lovelace")).toBe("");
    expect(viaLine("")).toBe("");
    // THE ROW'S LINE IS THE KIND ALONE, read off the same grammar.
    expect(viaKind("token:review")).toBe("via API token");
    expect(viaKind("ci:deploy")).toBe("via service account");
    expect(viaKind("ada.lovelace")).toBe("");
    expect(viaKind("")).toBe("");
  });

  test("nobody and not-yet-known are their own sentences", () => {
    expect(whoLine({ ...nobody, anonymous: true }, "").name).toBe("Not signed in");
    expect(whoLine({ ...nobody, loading: true }, "").name).toBe("Checking who you are");
  });

  // A VIEWER THAT NEVER ANSWERS — the socket refused a person whose seat was
  // taken out of the chart — is no reason to keep asking: the session names
  // them, and what they hold. The CONTROL is the case above, with no session
  // either. AND BY THE SAME RULE: the session names the reader's SEAT, which
  // that arm dropped for "Signed in". The chart is unread under a refused
  // socket, so the handle stands in, written as a handle.
  test("a session stands in for a viewer that has not answered", () => {
    expect(
      whoLine({ ...nobody, loading: true }, "", { login: "erin.ng", seat: "erin", grants: [] }),
    ).toEqual({ name: "erin.ng", detail: "@erin", grants: "Overview only" });
    expect(
      whoLine({ ...nobody, loading: true }, "", {
        login: "erin.ng",
        name: "Erin Ng",
        seat: "erin",
        grants: ["worklet"],
      }),
    ).toEqual({ name: "Erin Ng", detail: "erin.ng · @erin", grants: "Worklet" });
    // A SESSION BOUND TO NO SEAT says so, as the answered arm does.
    expect(whoLine({ ...nobody, loading: true }, "", { login: "token:ops" }).detail).toBe(
      "No seat",
    );
  });

  // Mutation: draw "Signed in" for the session arm and the seat is gone.
  test("a session the viewer never answers for draws the seat it names", async () => {
    engine({
      "GET /auth/session": {
        status: 200,
        body: { ...PERSON, login: "cara.example", seat: "cara", grants: [] },
      },
    });
    render(block({ ...nobody, loading: true }));
    const who = await screen.findByRole("button", { name: /cara\.example/ });
    expect(who.querySelector(".crewlet-account-row__detail")?.textContent).toBe("@cara");
    expect(who.textContent).not.toContain("Signed in");
  });

  // THE NAME OPENS THE READER'S OWN MENU, and the menu opens their Account,
  // where their password and second factor are. It once went to the SEAT,
  // which has neither, and a company's people clicking their own name
  // concluded the dashboard had no two-step verification at all; then it
  // went straight to the Account, with the sign-outs in a second popover
  // beside it. Mutation: link the row anywhere and the first line goes red.
  test("the reader's name opens their own menu, whose Account is theirs, never their seat", async () => {
    render(block(ada, "Founder"));
    expect(screen.queryByRole("link")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: /ada\.lovelace/ }));
    const menu = await screen.findByRole("menu", { name: "Your account" });
    fireEvent.click(within(menu).getByRole("menuitem", { name: /^Account/ }));
    await waitFor(() => expect(location.hash).toBe("#/account"));
    cleanup();
    // UNBOUND HAS AN ACCOUNT TOO (a token's session is told there what it
    // is), so its row opens the same menu.
    render(block({ ...nobody, login: "token:ops", unbound: true, grants: [] }));
    fireEvent.click(screen.getByRole("button", { name: /token:ops/ }));
    expect(
      within(await screen.findByRole("menu")).getByRole("menuitem", { name: /^Account/ }),
    ).toBeTruthy();
    cleanup();
    // NOBODY has no account to open: the menu offers the sign-in, and nothing else.
    render(block({ ...nobody, anonymous: true }));
    expect(screen.getByText("Not signed in")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /Not signed in/ }));
    const offered = within(await screen.findByRole("menu")).getAllByRole("menuitem");
    expect(offered.map((item) => item.textContent)).toEqual(["Sign in"]);
  });

  // A SESSION THE VIEWER HAS NOT ANSWERED FOR is somebody: it is named, and
  // its row opens its menu as a settled viewer's does, while nothing at all
  // has answered opens nothing.
  test("a session standing in for the viewer opens its menu", async () => {
    render(block({ ...nobody, loading: true }));
    const row = await screen.findByRole("button", { name: /ada\.lovelace/ });
    expect(row.getAttribute("aria-haspopup")).toBe("menu");
    cleanup();
    // THE CONTROL: a read still out is nobody yet, and no menu.
    vi.stubGlobal(
      "fetch",
      vi.fn(() => new Promise<Response>(() => {})),
    );
    render(block({ ...nobody, loading: true }));
    expect(screen.getByText("Checking who you are")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /your menu/ })).toBeNull();
    expect(screen.queryByRole("link")).toBeNull();
  });

  // THE ROW SAYS WHAT IT OPENS, in words and at rest: its name ends ", your
  // menu", it is a menu button, and its title says what the menu holds. In the
  // rail's foot the kit draws no chevron (its width goes to the name beside
  // the larger badge), so the words are what say it. Mutation: drop the spoken
  // suffix or the title and a line here goes red.
  test("the reader's name says it opens their menu, in words and at rest", () => {
    render(block(ada, "Founder"));
    const row = screen.getByRole("button", { name: /your menu/ });
    expect(row.getAttribute("aria-haspopup")).toBe("menu");
    expect(row.getAttribute("title")).toBe("Your account and signing out");
    // THE ACCESS IS SAID IN THE MENU, never as the row's title, which would
    // answer a question the reader had not asked.
    expect(row.getAttribute("title")).not.toMatch(/Worklet/);
  });

  // THE MENU SAYS ONLY WHAT THE ROW DOES NOT: the reader's access above the
  // actions. It said "Signed in as" over the login the row had just said.
  // Mutation: put the login back in the header and the last line goes red.
  test("the menu's header is the reader's access, and repeats nothing the row says", async () => {
    render(block(ada, "Founder"));
    fireEvent.click(screen.getByRole("button", { name: /your menu/ }));
    const menu = await screen.findByRole("menu", { name: "Your account" });
    const header = menu.querySelector(".crewlet-menu__header")!;
    expect(header.textContent).toBe("AccessWorklet");
    expect(menu.getAttribute("aria-describedby")).toBe(header.id);
    expect(menu.textContent).not.toContain("Signed in as");
    expect(header.textContent).not.toContain("ada.lovelace");
  });

  // THE CREDENTIAL'S NAME IS WHERE THE READER LOOKS FOR DETAIL. The row says
  // only "via API token"; the token's name is in the row's title, which a
  // screen reader hears as the row's description, and in the menu's header,
  // under the access. The CONTROL is a person, whose title and header say no
  // credential. Mutation: put the name back in the row and the first line goes
  // red; drop it from the title or the header and a later line does.
  test("a token bound to a seat says its kind in the row and its name in the title and the menu", async () => {
    render(block(reviewToken, "Miles"));
    const row = screen.getByRole("button", { name: /your menu/ });
    expect(row.querySelector(".crewlet-account-row__detail")!.textContent).toBe("via API token");
    expect(row.getAttribute("title")).toBe(
      "Signed in via API token review. Your account and signing out",
    );
    fireEvent.click(row);
    const menu = await screen.findByRole("menu", { name: "Your account" });
    const header = menu.querySelector(".crewlet-menu__header")!;
    expect(header.textContent).toBe("AccessAdminSigned invia API token review");
    expect(within(header as HTMLElement).getByText("via API token review")).toBeTruthy();
    cleanup();
    render(block(ada, "Founder"));
    const person = screen.getByRole("button", { name: /your menu/ });
    expect(person.getAttribute("title")).toBe("Your account and signing out");
    fireEvent.click(person);
    const own = await screen.findByRole("menu", { name: "Your account" });
    expect(own.querySelector(".crewlet-menu__header")!.textContent).toBe("AccessWorklet");
  });

  // THE BLOCK IS THE ACCOUNT'S ROW, so on the Account it is current as a
  // sidebar row is on its own page: the only mark the sidebar can give a
  // place it draws no row for. Mutation: drop the mark and the Account is
  // marked nowhere in the sidebar.
  test("on the Account the block is marked current, and nowhere else", () => {
    location.hash = "#/account";
    render(block(ada, "Founder"));
    expect(screen.getByRole("button", { name: /your menu/ }).hasAttribute("data-current")).toBe(
      true,
    );
    cleanup();
    location.hash = "#/";
    render(block(ada, "Founder"));
    expect(screen.getByRole("button", { name: /your menu/ }).hasAttribute("data-current")).toBe(
      false,
    );
  });

  // A FAILED READ IS NOT NOBODY. The session read failing (a 503, the
  // network) says nothing about whether this browser holds a session, and
  // the block said "Checking who you are" with no way to the Account until
  // the tab came back. Now it says it could not tell, and its menu still
  // offers the Account and the sign-out of this browser, never "everywhere",
  // which needs to know whose. Mutation: read a failure as nothing yet and the
  // menu goes.
  test.each([
    ["a 503", { status: 503, body: { error: "unavailable" } } as Answer],
    ["the network", "offline" as Answer],
  ])("a session read that failed on %s still offers the Account", async (_why, answer) => {
    engine({ "GET /auth/session": answer });
    render(block({ ...nobody, loading: true }));
    expect(await screen.findByText("Can’t check who you are")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /your menu/ }));
    const menu = await screen.findByRole("menu", { name: "Your account" });
    expect(within(menu).getByText("We couldn’t check who’s signed in.")).toBeTruthy();
    expect(within(menu).getByRole("menuitem", { name: /^Account/ })).toBeTruthy();
    expect(within(menu).getByRole("menuitem", { name: "Sign out" })).toBeTruthy();
    expect(within(menu).queryByRole("menuitem", { name: /^Sign out everywhere/ })).toBeNull();
  });

  // THE READER'S BADGE IS THE MEDIUM STEP, the size the kit's account row
  // sizes its badge column around. Mutation: draw it small again and this
  // fails.
  test("the reader's badge is drawn at the medium step", () => {
    render(block(ada, "Founder"));
    const badge = document.querySelector(".crewlet-account-row .crewlet-avatar")!;
    expect(badge.classList.contains("crewlet-avatar--md")).toBe(true);
  });

  // A KEY FOR A FACE, for a principal that is not a person: the initials of
  // "token:ops" drew "TO", as if it were somebody. A person keeps their
  // initials.
  test("an API token's session wears a key, and a person their initials", () => {
    const badge = () => document.querySelector(".crewlet-account-row .crewlet-avatar")!;
    render(block({ ...nobody, login: "token:ops", unbound: true, grants: [] }));
    expect(badge().querySelector(".crewlet-avatar__icon svg")).not.toBeNull();
    expect(badge().textContent).toBe("");
    cleanup();
    render(block(ada, "Founder"));
    expect(badge().querySelector(".crewlet-avatar__icon")).toBeNull();
    expect(badge().textContent).not.toBe("");
  });

  // A TOKEN ACTING AS A SEAT keeps the seat's face, its initials, and wears
  // the key on its corner, so the face says who and the corner says how.
  // The CONTROL is the unbound token above, whose key is its whole face.
  // Mutation: drop the mark and the corner is bare; draw the key as the face
  // and the initials are gone.
  test("a token bound to a seat wears the seat's initials, with a key on its corner", () => {
    const frame = () => document.querySelector(".crewlet-account-row .crewlet-avatar-frame")!;
    render(block(reviewToken, "Miles"));
    expect(frame().querySelector(".crewlet-avatar")!.textContent).toBe("MI");
    expect(frame().querySelector(".crewlet-avatar__icon")).toBeNull();
    expect(frame().querySelector(".crewlet-avatar__status--mark svg")).not.toBeNull();
    cleanup();
    // A PERSON wears neither.
    render(block(ada, "Founder"));
    expect(document.querySelector(".crewlet-account-row .crewlet-avatar__status--mark")).toBeNull();
  });

  // THE ONE BADGE THAT IS THE READER carries the accent's ring — and a badge
  // standing in for nobody, or for an answer still out, does not.
  test("the reader's own badge is ringed in the accent, once the engine has said who it is", () => {
    const ringed = () =>
      document.querySelectorAll(".crewlet-account-row .crewlet-avatar--ring-brand").length;
    render(block(ada, "Founder"));
    expect(ringed()).toBe(1);
    cleanup();
    render(block({ ...nobody, login: "token:ops", unbound: true }));
    expect(ringed()).toBe(1);
    // A MARKED BADGE draws no ring: the kit cuts the mark's notch from the
    // outline the ring would follow.
    cleanup();
    render(block(reviewToken, "Miles"));
    expect(ringed()).toBe(0);
    for (const unknown of [
      { ...nobody, anonymous: true },
      { ...nobody, loading: true },
    ]) {
      cleanup();
      render(block(unknown));
      expect(document.querySelector(".crewlet-account-row .crewlet-avatar")).not.toBeNull();
      expect(ringed()).toBe(0);
    }
  });

  // ONE CONTROL BESIDE THE NAME: the preferences, the mode among them, and
  // nothing of the session's. The foot also carried a one-press mode flip,
  // which with the row's chevron left a name already cut short by the rail's
  // width two glyph buttons to share it with; and the popover held the
  // sign-outs too, under a second "Account" beside the row that is the
  // account. Mutation: put the flip back and there are two controls; put the
  // session back in the popover and the last lines go red.
  test("the row holds one control, the preferences, and it holds only the preferences", async () => {
    render(block(ada, "Founder"));
    const controls = document.querySelectorAll(".crewlet-account-row__actions button");
    expect([...controls].map((b) => b.getAttribute("aria-label"))).toEqual(["Preferences"]);
    fireEvent.click(screen.getByRole("button", { name: "Preferences" }));
    const popover = await screen.findByRole("dialog", { name: "Preferences" });
    expect(within(popover).getByRole("radiogroup", { name: "Mode" })).toBeTruthy();
    expect(within(popover).queryByText("Sign out")).toBeNull();
    expect(within(popover).queryByText("Account")).toBeNull();
    expect(within(popover).queryByText(/Signed in as/)).toBeNull();
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
  // taken out of the chart — never has it answered. The menu drew nothing until it was, so those people had no way
  // to end their own session; the session route still answers them.
  test("a session the viewer never answers for is still offered its sign-outs", () => {
    const waiting = { ...nobody, loading: true };
    expect(accountOf(waiting, PERSON)).toEqual({ kind: "session", login: "ada.lovelace" });
    // NOTHING ANSWERED AT ALL IS STILL NOTHING — while the read is out.
    expect(accountOf(waiting, null)).toBeNull();
    // A READ THAT FAILED is no answer either way, and offers what needs none.
    expect(accountOf(waiting, null, true)).toEqual({ kind: "unknown" });
  });

  test("nobody at all is offered a way to sign in, back to where they are", async () => {
    location.hash = "#/worklet";
    actions({ kind: "nobody" });
    fireEvent.click(screen.getByRole("menuitem", { name: "Sign in" }));
    await waitFor(() =>
      expect(location.hash).toBe(`#/login?next=${encodeURIComponent("#/worklet")}`),
    );
  });

  // THE PROOF GESTURES LIVE ON THE ACCOUNT PAGE, and the menu opens it
  // rather than keeping a second copy of them; choosing it closes the menu.
  // The menu's items are exactly the Account and the two sign-outs, which
  // stay here for the reader the socket refuses. Mutation: add a proof
  // gesture to the menu and the item list goes red.
  test("the menu opens the Account page and holds no proof gesture of its own", async () => {
    actions({ kind: "session", login: "ada.lovelace" });
    const labels = screen
      .getAllByRole("menuitem")
      .map((item) => item.querySelector(".crewlet-menu__label")?.textContent);
    expect(labels).toEqual(["Account", "Sign out", "Sign out everywhere"]);
    // SIGNING OUT EVERYWHERE SAYS WHAT IT ENDS, in the danger ink.
    const everywhere = screen.getByRole("menuitem", { name: /^Sign out everywhere/ });
    expect(everywhere.textContent).toContain("Every device, and stops your tokens");
    expect(everywhere.classList.contains("is-danger")).toBe(true);
    fireEvent.click(screen.getByRole("menuitem", { name: /^Account/ }));
    await waitFor(() => expect(location.hash).toBe("#/account"));
    expect(screen.queryByRole("menu")).toBeNull();
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

/** The reader's menu as `useAccountMenu` builds it, on the kit's row that opens it. */
function MenuRow({
  account,
  onSignOutEverywhere,
}: {
  account: Account;
  onSignOutEverywhere: () => void;
}) {
  const menu = useAccountMenu(account, "Worklet", onSignOutEverywhere);
  return <AccountRow avatar={<span />} name="Ada Lovelace" menu={menu} />;
}

/** The account's menu, drawn on its own with a toaster to report into, and opened. */
function actions(account: Account) {
  const onSignOutEverywhere = vi.fn();
  const client = heldSocket();
  render(
    <ClientContext.Provider value={client.value}>
      <Router>
        <ToastProvider>
          <LayerHost>
            <MenuRow account={account} onSignOutEverywhere={onSignOutEverywhere} />
          </LayerHost>
        </ToastProvider>
      </Router>
    </ClientContext.Provider>,
  );
  fireEvent.click(screen.getByRole("button", { name: /Ada Lovelace/ }));
  return { onSignOutEverywhere, asked: client.asked };
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
    fireEvent.click(screen.getByRole("menuitem", { name: "Sign out" }));
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
    fireEvent.click(screen.getByRole("menuitem", { name: "Sign out" }));
    expect(await screen.findByText(/Signing out didn’t go through/)).toBeDefined();
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
    const { onSignOutEverywhere } = actions(signedIn);
    fireEvent.click(screen.getByRole("menuitem", { name: /^Sign out everywhere/ }));
    expect(screen.queryByRole("menu")).toBeNull();
    expect(onSignOutEverywhere).toHaveBeenCalledOnce();
    expect(sent.filter((s) => s.method === "POST")).toEqual([]);
  });

  // AND FROM THE ROW ITSELF, the dialog it asks with is opened by the block,
  // outside the menu that has closed.
  test("everywhere, from the row: the block opens the dialog that asks", async () => {
    render(block(ada, "Founder"));
    fireEvent.click(screen.getByRole("button", { name: /your menu/ }));
    fireEvent.click(await screen.findByRole("menuitem", { name: /^Sign out everywhere/ }));
    expect(await screen.findByRole("dialog", { name: "Sign out everywhere?" })).toBeDefined();
    expect(screen.queryByRole("menu")).toBeNull();
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
    expect(
      within(ask).getByText("Signs you out on every device and stops your tokens."),
    ).toBeDefined();
    fireEvent.click(within(ask).getByRole("button", { name: "Sign out everywhere" }));
    expect(await screen.findByText(/Signing out everywhere didn’t go through/)).toBeDefined();
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
      if (this.getAttribute("aria-label") === "Preferences" && this.tagName === "BUTTON") {
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
    const trigger = screen.getByRole("button", { name: "Preferences" });
    fireEvent.click(trigger);
    const dialog = await screen.findByRole("dialog", { name: "Preferences" });
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

  // THE MODE IS THREE CHOICES, and the third is what makes the popover the
  // place to set it: the one-press flip beside the name can only say light or
  // dark, and "follow the system" is a choice a reader has to be able to get
  // back to. Each choice is read back from `lib/prefs.ts` and from the root
  // attribute the stylesheet keys on, and "system" REMOVES the attribute, so
  // the media query decides and keeps deciding when the OS setting changes.
  test("the mode is light, dark or the system's, and each is kept", () => {
    open();
    const theme = screen.getByRole("radiogroup", { name: "Mode" });
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

  // THE ZONE AND THE DATE'S SHAPE ARE THE BROWSER'S, so the panel offers
  // neither: a control here would be a setting nothing reads.
  test("offers no time zone and no date format", () => {
    open();
    expect(screen.queryByText("Time zone")).toBeNull();
    expect(screen.queryByText("Dates")).toBeNull();
    // The one list here is the theme's.
    expect(screen.getAllByRole("combobox").map((box) => box.getAttribute("aria-label"))).toEqual([
      "Theme",
    ]);
  });

  // THE ROWS ARE MODE, DENSITY, THEME, in that order, and the two switchers
  // draw one box: the same width, three equal cells each.
  test("the mode, the density and the theme are one column of one width", () => {
    open();
    const rows = [...document.querySelectorAll(".preferences-row")].map(
      (row) => row.querySelector(".crewlet-text--label")?.textContent,
    );
    expect(rows).toEqual(["Mode", "Density", "Theme"]);
    for (const name of ["Mode", "Density"]) {
      const group = screen.getByRole("radiogroup", { name });
      expect(group.classList.contains("crewlet-segmented--fill"), name).toBe(true);
      expect(group.parentElement?.className, name).toBe("preferences-switch");
    }
    // The theme's field sits in the same box, so the three draw one width.
    const theme = screen.getByRole("combobox", { name: "Theme" });
    expect(theme.closest(".preferences-switch")).toBeTruthy();
  });

  test("the theme lists every theme under its group, applies the pick and keeps it", () => {
    render(
      <ToastProvider>
        <Preferences />
      </ToastProvider>,
    );
    const field = screen.getByRole("combobox", { name: "Theme" });
    expect(field.textContent).toContain("Crewlet");
    fireEvent.click(field);
    const list = screen.getByRole("listbox", { name: "Theme" });
    const names = within(list)
      .getAllByRole("option")
      .map((option) => option.querySelector(".crewlet-select__option-label")?.textContent);
    expect(names).toEqual(themeRegistry.map((theme) => theme.name));
    expect(within(list).getByRole("group", { name: "Purple" })).toBeTruthy();
    fireEvent.mouseDown(within(list).getByRole("option", { name: /Abyss/ }));
    expect(document.documentElement.getAttribute("data-palette")).toBe("abyss");
    expect(localStorage.getItem(STORAGE_KEYS.palette)).toBe("abyss");
    expect(screen.getByRole("combobox", { name: "Theme" }).textContent).toContain("Abyss");
    // Said, with the way back, and the way back is the choice before.
    const undo = screen.getByRole("button", { name: "Undo" });
    // Drawn, and announced in the toaster's live region as well.
    expect(screen.getAllByText("Theme set to Abyss").length).toBeGreaterThan(0);
    fireEvent.click(undo);
    expect(document.documentElement.hasAttribute("data-palette")).toBe(false);
    expect(localStorage.getItem(STORAGE_KEYS.palette)).toBeNull();
  });

  // A PREVIEW IS THE LIST'S, NEVER STORAGE'S: the frame takes the theme the
  // highlight rests on, and the list closing puts back the one in effect.
  test("the highlight previews the theme across the frame, and Escape puts back the one in effect", () => {
    vi.useFakeTimers();
    try {
      open();
      const field = screen.getByRole("combobox", { name: "Theme" });
      fireEvent.click(field);
      fireEvent.keyDown(field, { key: "ArrowDown" });
      expect(document.documentElement.getAttribute("data-palette")).toBe("violet");
      expect(localStorage.getItem(STORAGE_KEYS.palette)).toBeNull();
      fireEvent.mouseEnter(screen.getByRole("option", { name: /Ledger/ }));
      act(() => vi.advanceTimersByTime(HIGHLIGHT_REST_MS));
      expect(document.documentElement.getAttribute("data-palette")).toBe("ledger");
      fireEvent.keyDown(field, { key: "Escape" });
      expect(document.documentElement.hasAttribute("data-palette")).toBe(false);
      expect(localStorage.getItem(STORAGE_KEYS.palette)).toBeNull();
    } finally {
      vi.useRealTimers();
    }
  });
});
