/**
 * People draws the identity directory from both ends, what it
 * WRITES is `AccessWrites.test.tsx`'s.
 *
 * The invariants, in the order they cost when they go: the whole directory is
 * drawn, not its first page — a directory cut at a page is a company that
 * looks smaller than it is; a reader the engine refuses sees the refusal and
 * the grants that would have admitted them, never an empty company; a sealed
 * row is a state, never a blank; and a reader who may not read the company
 * document is told so rather than shown seats nobody can reach.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { Suspense, type ComponentType } from "react";
import { afterEach, beforeAll, beforeEach, expect, test, vi } from "vitest";

import {
  ApiTokens,
  INVITATION_WORDS,
  People,
  STAGE_WORDS,
  ServiceAccounts,
  TOKEN_ROW_WORDS,
} from "./Access.tsx";
import { PeekHost, PeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { loadChunk } from "~/app/lazyScreen.ts";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { CHART_ORG } from "~/test/orgchart.ts";
import { SessionReading } from "~/lib/frameSession.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

/** An administrator: reads the directory, and the company document beside it. */
const ADMIN = { login: "ana.diaz", owner: "jane", grants: ["admin"] };

const PAGE_ONE = {
  people: [
    {
      id: "p-ana",
      kind: "person",
      stage: "active",
      login: "ana.diaz",
      name: "Ana Diaz",
      seat: "jane",
      grants: ["admin"],
    },
    { id: "p-sealed", kind: "person", stage: "active", login: "bo.lang", sealed: true },
  ],
  next: "cursor-1",
};

const PAGE_TWO = {
  people: [{ id: "p-ci", kind: "machine", stage: "suspended", login: "ci:release" }],
  next: "",
};

const SEATS = {
  seats: [
    {
      handle: "jane",
      name: "Jane Founder",
      holder: { person: "p-ana", kind: "person", login: "ana.diaz", stage: "active" },
    },
    // HELD BY ITS OPEN INVITATION, which `OPEN_INVITATIONS` lists too.
    {
      handle: "sam",
      name: "Sam Support",
      invitation: { id: "inv-1", email: "sam@example.com", expires_at: "2026-10-14T09:00:00Z" },
    },
    { handle: "lee", name: "Lee Legal" },
  ],
};

const TOKENS = {
  tokens: [
    { id: "deploy", login: "token:deploy", row: "held", person: "p-deploy", seat: "jane" },
    { id: "spare", login: "token:spare", row: "none" },
  ],
};

const CHECK = {
  findings: [
    {
      kind: "binding_dangling",
      person: "p-gone",
      login: "gone.person",
      seat: "removed-seat",
      detail: "the seat removed-seat is not in the org chart",
    },
  ],
  people_with_admin: 1,
  bindings_unchecked: 0,
};

const OPEN_INVITATIONS = {
  invitations: [
    { id: "inv-1", email: "sam@example.com", seat: "sam", grants: [], state: "open" },
    { id: "inv-2", sealed: true, grants: [], state: "open" },
  ],
  next: "",
};

const ALL_INVITATIONS = {
  invitations: [
    ...OPEN_INVITATIONS.invitations,
    { id: "inv-3", email: "old@example.com", grants: [], state: "redeemed", person: "p-old" },
    // REDEEMED BY SOMEBODY SINCE REMOVED: the removal erased the address.
    { id: "inv-4", grants: [], state: "redeemed", person: "p-gone", invited_by: "jane" },
  ],
  next: "",
};

function json(status: number, payload: unknown): Response {
  return new Response(JSON.stringify(payload), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

/** `/iam` answering, with `override` consulted first. */
function stubIam(override: (url: URL) => Response | null = () => null) {
  const asked: URL[] = [];
  const spy = vi.fn((input: RequestInfo | URL) => {
    const url = new URL(String(input), "http://engine.test");
    asked.push(url);
    const answer = override(url);
    if (answer) return Promise.resolve(answer);
    // ONE PERSON, as the peek reads them: the row the directory holds under
    // that id, or the engine's 404.
    const one = /^\/iam\/people\/([^/]+)$/.exec(url.pathname);
    if (one) {
      const row = [...PAGE_ONE.people, ...PAGE_TWO.people].find((p) => p.id === one[1]);
      return Promise.resolve(row ? json(200, row) : json(404, { error: "not_found" }));
    }
    switch (url.pathname) {
      case "/iam/people":
        return Promise.resolve(json(200, url.searchParams.get("after") ? PAGE_TWO : PAGE_ONE));
      case "/iam/seats":
        return Promise.resolve(json(200, SEATS));
      case "/iam/node-tokens":
        return Promise.resolve(json(200, TOKENS));
      case "/iam/check":
        return Promise.resolve(json(200, CHECK));
      case "/iam/invitations":
        return Promise.resolve(
          json(200, url.searchParams.get("all") === "true" ? ALL_INVITATIONS : OPEN_INVITATIONS),
        );
      case "/iam/credentials":
        return Promise.resolve(
          json(200, {
            credentials: [
              { id: "c-1", person: "p-ana", method: "password", revoked: false },
              { id: "c-2", person: "p-ana", method: "totp", revoked: false },
            ],
          }),
        );
      case "/iam/people/p-ana/sessions":
        return Promise.resolve(
          json(200, { sessions: [{ lineage: "s-1", person: "p-ana", live: true }] }),
        );
    }
    return Promise.resolve(json(404, { error: "no_route", message: "no such route" }));
  });
  Object.defineProperty(globalThis, "fetch", { writable: true, value: spy });
  return asked;
}

/** One of the three Access screens, People unless a case names another. */
function mount(viewer: Record<string, unknown> = ADMIN, Screen: ComponentType = People) {
  const store = new Store();
  store.applyOrg(CHART_ORG);
  const socket = new LiveSocket(store);
  const queried: string[] = [];
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) => {
    queried.push(what);
    if (what === "viewer") return Promise.resolve(viewer);
    if (what === "config") return Promise.resolve({ name: "Acme", roles: [], units: [] });
    return new Promise(() => {});
  };
  // THE SCREEN AND THE FRAME'S ONE PEEK beside it, as the shell mounts them:
  // a row opens its person there, and a pasted `peek=` opens it on arrival.
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <ViewerProvider>
        <Router>
          <SessionReading>
            <PeekNeighbours>
              <Screen />
              <Suspense fallback={null}>
                <PeekHost />
              </Suspense>
            </PeekNeighbours>
          </SessionReading>
        </Router>
      </ViewerProvider>
    </ClientContext.Provider>,
  );
  return { queried };
}

/** The peek, once it is open. */
async function peek(): Promise<HTMLElement> {
  return (await screen.findByRole("complementary", { name: "Person detail" })) as HTMLElement;
}

// THE PEEK'S BODY IS THE SETTINGS CHUNK'S, loaded once so a case measures what
// it draws rather than a cold import.
beforeAll(async () => {
  await loadChunk("settings");
});

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  location.hash = "#/settings/people";
});

afterEach(() => {
  cleanup();
  location.hash = "";
  vi.restoreAllMocks();
});

// THE WHOLE DIRECTORY: the second page is asked for with the first page's
// cursor, and the walk stops at the page that names none.
test("every page of the directory is drawn, walked by its cursor", async () => {
  const asked = stubIam();
  mount();
  expect(await screen.findByText("Ana Diaz")).toBeTruthy();
  const pages = asked.filter((u) => u.pathname === "/iam/people");
  expect(pages.map((u) => u.searchParams.get("after"))).toEqual([null, "cursor-1"]);
  // Each row's stage in the engine's own words.
  expect(screen.getAllByText(STAGE_WORDS.active!.label).length).toBeGreaterThan(0);
});

// THE RESTING STATE IS QUIET WORDS, AND ONLY NEWS IS A PILL: "Active" on every
// person and service account, and "None" on every token, were pills down a
// whole column. Mutation: draw a resting state as a tag and the first
// expectation finds one.
test("a list's resting state is words, and a state that differs is a pill", async () => {
  stubIam();
  mount();
  const active = (await screen.findAllByText(STAGE_WORDS.active!.label)).filter((el) =>
    el.closest(".crewlet-data-grid__row"),
  );
  expect(active.length).toBeGreaterThan(0);
  for (const word of active) expect(word.closest(".crewlet-tag")).toBeNull();
  cleanup();

  stubIam();
  mount(ADMIN, ServiceAccounts);
  expect(
    (await screen.findByText(STAGE_WORDS.suspended!.label)).closest(".crewlet-tag"),
  ).not.toBeNull();
  cleanup();

  stubIam();
  mount(ADMIN, ApiTokens);
  const spare = (await screen.findByText("token:spare")).closest(
    ".crewlet-data-grid__row",
  ) as HTMLElement;
  expect(within(spare).getByText(TOKEN_ROW_WORDS.none!.label).closest(".crewlet-tag")).toBeNull();
});

// NO ROW IS DRAWN TWICE: the directory is split between the two pages that
// read it, a service account on Service accounts and every other principal on
// People. The second page's machine is a service account, in its stage's
// words, and the first page's person is not on that page. Mutation: drop the
// split from either page and its half goes red.
test("people and service accounts split the directory, and no row is on both", async () => {
  stubIam();
  mount();
  expect(await screen.findByText("Ana Diaz")).toBeTruthy();
  await waitFor(() => expect(screen.queryByText("Service account")).toBeNull());
  expect(screen.queryByText("ci:release")).toBeNull();
  cleanup();

  stubIam();
  mount(ADMIN, ServiceAccounts);
  expect((await screen.findAllByText("ci:release")).length).toBeGreaterThan(0);
  expect(screen.getByText(STAGE_WORDS.suspended!.label)).toBeTruthy();
  expect(screen.queryByText("Ana Diaz")).toBeNull();
});

// THE PERSON'S BADGE IS BESIDE THEIR NAME, AND THE SEAT IS ITS HANDLE. The
// Seat cell drew the seat's name and badge, which echoed the person's a cell
// over ("Lee Park  LP Lee Park"); a service account wears a key.
test("a person's row draws their badge by their name and their seat by its handle", async () => {
  stubIam();
  mount();
  const ana = (await screen.findByText("Ana Diaz")).closest(
    ".crewlet-data-grid__row",
  ) as HTMLElement;
  expect(ana.querySelector(".crewlet-avatar")).not.toBeNull();
  const seat = within(ana).getByRole("link", { name: "@jane" });
  expect(seat.getAttribute("href")).toBe("#/company/org/seats/jane");
  expect(seat.getAttribute("title")).toBe("Jane Founder");
  cleanup();

  stubIam();
  mount(ADMIN, ServiceAccounts);
  const ci = (await screen.findAllByText("ci:release"))
    .map((el) => el.closest(".crewlet-data-grid__row") as HTMLElement)
    .find(Boolean)!;
  expect(ci.querySelector(".crewlet-avatar__icon svg")).not.toBeNull();
});

// HELD BY A NAME, NOT A STATUS PILL: the holder is plain text, and the stage
// is a tag only where it is news. Mutation: draw the active stage too.
test("a human seat's holder is named in plain text, its stage only when not active", async () => {
  stubIam();
  mount();
  const jane = (await screen.findAllByRole("link", { name: /Jane Founder/ }))
    .map((link) => link.closest(".crewlet-data-grid__row") as HTMLElement)
    .find((row) => row && within(row).queryByText("ana.diaz"))!;
  const held = within(jane).getByText("ana.diaz");
  expect(held.closest(".crewlet-tag")).toBeNull();
  expect(within(jane).queryByText(STAGE_WORDS.active!.label)).toBeNull();
});

// A SEALED VALUE IS A STATE the operator can end by putting a key back — never
// a blank name, which reads as somebody who never gave one.
test("a row this node cannot open is drawn as sealed", async () => {
  stubIam();
  mount();
  // THE PERSON'S ROW, and the sealed invitation's address beside it.
  expect((await screen.findAllByText("Locked")).length).toBe(2);
});

// FROM BOTH ENDS: the seat with whoever holds it, and each of this node's
// tokens with the row its login names and the seat that row binds it to — what
// the row holds, with the report alone saying whether a seat is gone.
test("a seat names who holds it and a token the seat its row binds", async () => {
  stubIam();
  mount(ADMIN, ApiTokens);
  const deploy = (await screen.findByText("token:deploy")).closest(
    ".crewlet-data-grid__row",
  ) as HTMLElement;
  expect(within(deploy).getByText(TOKEN_ROW_WORDS.held!.label)).toBeTruthy();
  expect(within(deploy).getByRole("link", { name: /Jane Founder/ })).toBeTruthy();
  const spare = screen.getByText("token:spare").closest(".crewlet-data-grid__row") as HTMLElement;
  expect(within(spare).getByText(TOKEN_ROW_WORDS.none!.label)).toBeTruthy();
  // The row's hint is the command that binds it, whole: the CLI refuses a
  // service account's create naming no `-grants`, so without it the command
  // fails as pasted.
  expect(
    within(spare)
      .getByTitle(/crewlet iam create/)
      .getAttribute("title"),
  ).toContain("crewlet iam create -kind machine -login token:<id> -seat <seat> -grants none");
  expect(within(spare).getByText("No seat")).toBeTruthy();
  cleanup();

  stubIam();
  mount();
  await screen.findByText("Ana Diaz");
  // The directory's finding, written here about the person it names.
  expect(
    screen.getByText("gone.person is on a seat that was removed. Move them to another seat."),
  ).toBeTruthy();
  // AN INVITED SEAT IS NOT VACANT: the open invitation holds it as surely as
  // a person would, so the tile counts the one seat nothing holds and says
  // the invited one beside it. Mutation: count every seat with no holder and
  // the figure reads 2.
  const vacant = screen.getByText("Open seats").closest(".crewlet-statcard");
  expect(vacant?.querySelector(".crewlet-statcard__value")?.textContent).toBe("1");
  expect(within(vacant as HTMLElement).getByText("1 invited · 3 total")).toBeTruthy();
  // AND ITS ROW NAMES THE INVITATION rather than "Open seat".
  const sam = screen
    .getAllByRole("link", { name: /Sam Support/ })
    .map((link) => link.closest(".crewlet-data-grid__row") as HTMLElement)
    .find((row) => row && within(row).queryByText("Invited"))!;
  expect(within(sam).getByText("sam@example.com")).toBeTruthy();
  expect(within(sam).queryByText("Open seat")).toBeNull();
  const lee = screen
    .getByRole("link", { name: /Lee Legal/ })
    .closest(".crewlet-data-grid__row") as HTMLElement;
  // A SEAT NOTHING HOLDS READS "Open seat", in words, rather than a dash.
  expect(within(lee).getAllByText("Open seat").length).toBeGreaterThan(0);
});

// A FINDING IS DRAWN IN WORDS, never its code: the kind's label from
// `FINDING_WORDS` beside a sentence written here about the person it names,
// so the screen never prints a code, or the engine's API path, an
// administrator would have to look up. A service account bound to
// none reads as the ordinary state it is. Mutation: draw the finding's kind
// raw and its code appears.
test("a finding is drawn in words, never as its code or the engine's detail", async () => {
  stubIam(() => null);
  mount();
  const finding = (
    await screen.findByText("gone.person is on a seat that was removed. Move them to another seat.")
  ).parentElement as HTMLElement;
  expect(within(finding).getByText("Seat removed")).toBeTruthy();
  expect(screen.queryByText("binding_dangling")).toBeNull();
  expect(screen.queryByText(/is not in the org chart/)).toBeNull();
  cleanup();

  stubIam(() => null);
  mount(ADMIN, ServiceAccounts);
  // A SERVICE ACCOUNT BOUND TO NONE reads as the ordinary state it is.
  const ci = (await screen.findAllByText("ci:release"))
    .map((cell) => cell.closest(".crewlet-data-grid__row") as HTMLElement | null)
    .find((row) => row !== null)!;
  expect(within(ci).getByText("No seat")).toBeTruthy();
});

// THE BOUND SEAT'S KIND IS THE CHART'S: a token bound to an AGENT seat is the
// residue the report names, and a person's badge on it would say the opposite
// of the chart on the very row the report calls dangling.
test("a row bound to an agent seat draws the agent's badge", async () => {
  stubIam((url) =>
    url.pathname === "/iam/node-tokens"
      ? json(200, {
          tokens: [
            { id: "deploy", login: "token:deploy", row: "held", person: "p-deploy", seat: "jane" },
            { id: "boss", login: "token:boss", row: "held", person: "p-boss", seat: "ceo" },
          ],
        })
      : null,
  );
  mount(ADMIN, ApiTokens);
  const row = async (login: string) =>
    (await screen.findByText(login)).closest(".crewlet-data-grid__row") as HTMLElement;
  const outline = (r: HTMLElement) => {
    const mark = r.querySelector(".crewlet-avatar") as HTMLElement;
    return mark.classList.contains("crewlet-avatar--human") ? "human" : "agent";
  };
  const boss = await row("token:boss");
  // RESOLVED by the chart, so the outline is the seat's own kind and not the
  // default a handle the chart does not hold is drawn with.
  expect(within(boss).getByRole("link", { name: /CEO/ })).toBeTruthy();
  expect(outline(boss)).toBe("agent");
  expect(outline(await row("token:deploy"))).toBe("human");
});

// A MANAGED DOCUMENT (ADR-0030) IS PART OF THE POSTURE: the tokens that alone
// write it are named on this node's tokens card, in words that cover every
// credential — a session and a machine token are refused exactly as an
// unlisted token is — and nothing is said when every credential may write.
// Mutation: drop the list from the read and the first half fails; draw it
// unconditionally and the second does.
test("a managed document names its writers beside the tokens", async () => {
  stubIam((url) =>
    url.pathname === "/iam/node-tokens"
      ? json(200, { ...TOKENS, company_writers: ["gitops", "ci"] })
      : null,
  );
  mount(ADMIN, ApiTokens);
  const line = (await screen.findByText(/can change the company document/)).closest("span");
  expect(line?.textContent).toBe("Only gitops, ci can change the company document.");
  cleanup();

  stubIam();
  mount(ADMIN, ApiTokens);
  await screen.findAllByText("token:spare");
  expect(screen.queryByText(/can change the company document/)).toBeNull();
});

// A REFUSED DIRECTORY IS NOT AN EMPTY COMPANY: the reader sees the refusal and
// the grants the engine named, and no tile counting nobody.
test("a refused reader sees the refusal and its grants, and no tiles", async () => {
  stubIam((url) =>
    url.pathname.startsWith("/iam/")
      ? json(403, {
          error: "unauthorized",
          message: "you may not",
          reason: "directory",
          grants: ["admin"],
        })
      : null,
  );
  mount({ login: "dee", owner: "dee", grants: [] });
  const [banner] = await screen.findAllByText(/^This answer needs Admin access\./);
  expect(banner!.closest(".crewlet-callout")?.textContent).toMatch(/Refused as directory/);
  expect(screen.queryByText("People")).toBeNull();
  expect(screen.queryByText("Nobody is in the directory yet")).toBeNull();
});

// THE CONTACT IDENTITIES ARE THE COMPANY DOCUMENT'S: a reader without
// Admin is never put the question, and each seat says what it takes.
test("a reader without Admin is told what reading a seat's surfaces needs", async () => {
  stubIam();
  const { queried } = mount({
    login: "ana.diaz",
    owner: "jane",
    grants: ["worklet", "doclet"],
  });
  // ONE SENTENCE PER SEAT, every seat refused alike.
  expect(
    await screen.findAllByText(
      "Reading where a seat is reached needs Admin access. An admin can give it to you.",
    ),
  ).toHaveLength(3);
  expect(queried).not.toContain("config");
});

// A ROW OPENS ITS PERSON IN THE PEEK, which is in view beside the list: the
// panel was a card drawn under the stats, the report and the whole grid, so
// pressing a row opened it below the fold and the page did not move. The row
// is a real link to that peek's address, and is marked while it is open.
// Mutation: open the row anywhere but the frame's peek and the rail never
// appears.
test("activating a row opens that person in the peek, by their id", async () => {
  stubIam();
  mount();
  const row = (await screen.findByText("Ana Diaz")).closest(
    ".crewlet-data-grid__row",
  ) as HTMLElement;
  const link = row.querySelector<HTMLAnchorElement>("a.crewlet-data-grid__row-link")!;
  expect(link.getAttribute("href")).toBe("#/settings/people?peek=person%3Ap-ana");
  fireEvent.click(link);
  await waitFor(() => expect(location.hash).toContain("peek=person%3Ap-ana"));
  const rail = await peek();
  expect(await within(rail).findByText("Authenticator app")).toBeTruthy();
  // ITS ONLY PAGE IS THIS ONE, so the rail offers no Open that would close it.
  expect(within(rail).queryByRole("link", { name: /^Open/ })).toBeNull();
  // AND THE ROW IS MARKED as the one the peek is open on.
  await waitFor(() => expect(link.getAttribute("aria-current")).toBe("true"));
});

// THE PEEK MAKES THE PEOPLE LIST A COLUMN OF THE SHEET, and the peek's own
// grids are a 385px column too. A flexible track floored at zero gives way to
// every content-sized one: beside the peek the seat drew its avatar alone and
// the grants stacked a row ten lines tall, and in the peek "Password" read
// "P…" while Expires and Carries, which go first, kept a pixel each. So every
// flexible column these grids keep has a floor — the tracks overrun and the
// grid drops by `drop` instead — and the peek's pill and timestamp take their
// own width rather than a fifth of the column. Mutation: drop a floor, or put
// a `shrink` back on State, and its track reads at zero or capped.
test("no grid beside or inside the person peek floors a flexible column at zero", async () => {
  location.hash = "#/settings/people?peek=person:p-ana";
  stubIam();
  mount();
  const rail = await peek();
  await within(rail).findByText("Authenticator app");
  await within(rail).findByText("Signed in");
  const tracks = (wrap: Element) => {
    const heads = [...wrap.querySelectorAll(":scope > .crewlet-data-grid__head > *")].map(
      (h) => h.textContent?.trim() ?? "",
    );
    const list = splitTracks((wrap as HTMLElement).style.gridTemplateColumns);
    return new Map(heads.map((h, i) => [h, list[i]]));
  };
  const people = screen
    .getAllByText("Ana Diaz")
    .find((e) => !rail.contains(e))!
    .closest(".crewlet-data-grid")!;
  const credentials = within(rail).getByText("Password").closest(".crewlet-data-grid")!;
  const sessions = within(rail).getByText("Signed in").closest(".crewlet-data-grid")!;
  for (const wrap of [people, credentials, sessions]) {
    expect([...tracks(wrap).values()].filter((t) => t === "minmax(0, 1fr)")).toEqual([]);
  }
  expect(tracks(people).get("Seat")).toBe("minmax(8rem, 1fr)");
  expect(tracks(people).get("Access")).toBe("minmax(8rem, 1fr)");
  expect(tracks(credentials).get("Credential")).toBe("minmax(8rem, 1fr)");
  expect(tracks(credentials).get("State")).toBe("max-content");
  expect(tracks(sessions).get("Session")).toBe("max-content");
});

/** A track list split at its top-level spaces, a `minmax(a, b)` kept whole. */
function splitTracks(list: string): string[] {
  const out: string[] = [];
  let depth = 0;
  let current = "";
  for (const ch of list) {
    if (ch === "(") depth++;
    if (ch === ")") depth--;
    if (ch === " " && depth === 0) {
      if (current) out.push(current);
      current = "";
    } else current += ch;
  }
  if (current) out.push(current);
  return out;
}

// OPENING A PERSON reads that one principal's row, credentials and sessions.
test("an opened person shows the credentials and sessions read for them", async () => {
  location.hash = "#/settings/people?peek=person:p-ana";
  const asked = stubIam();
  mount();
  const rail = await peek();
  expect(await within(rail).findByText("Authenticator app")).toBeTruthy();
  expect(within(rail).getByText("Password")).toBeTruthy();
  expect(within(rail).getByText("Signed in")).toBeTruthy();
  const credentials = asked.find((u) => u.pathname === "/iam/credentials");
  expect(credentials?.searchParams.get("person")).toBe("p-ana");
  expect(asked.some((u) => u.pathname === "/iam/people/p-ana")).toBe(true);
});

// THE READER'S OWN ROW POINTS AT THEIR ACCOUNT, first: every gesture in the
// peek is an administrator's, made about somebody, and a person who opened
// their own row was offered a reset link and an "end all sessions" and nothing
// saying their password and second factor are theirs to manage. The CONTROL
// is somebody else's row, which carries no such pointer. Mutation: drop the
// callout and the first half goes red; draw it on every row and the second.
test("the reader's own row leads with their Account; somebody else's does not", async () => {
  location.hash = "#/settings/people?peek=person:p-ana";
  stubIam((url) =>
    url.pathname === "/auth/session"
      ? json(200, { person: "p-ana", login: "ana.diaz", kind: "person", grants: [] })
      : null,
  );
  mount();
  const rail = await peek();
  expect(await within(rail).findByText("This is you")).toBeTruthy();
  expect(
    within(rail)
      .getByRole("link", { name: /Open Account/ })
      .getAttribute("href"),
  ).toBe("#/account");
  cleanup();

  location.hash = "#/settings/people?peek=person:p-ci";
  stubIam((url) =>
    url.pathname === "/auth/session"
      ? json(200, { person: "p-ana", login: "ana.diaz", kind: "person", grants: [] })
      : null,
  );
  mount();
  const other = await peek();
  await within(other).findAllByText("ci:release");
  expect(within(other).queryByText("This is you")).toBeNull();
  expect(other.querySelector('a[href="#/account"]')).toBeNull();
});

// A PERSON THE LINK NAMES AND THE DIRECTORY DOES NOT HOLD is said, and Close
// takes the address back to the list: a saved link to somebody since removed
// drew the list with no panel and no word. The CONTROL is a person the
// directory holds, which draws no such note. Mutation: drop the note and the
// first half goes red.
test("a link to a person the directory does not hold says so, and closes", async () => {
  location.hash = "#/settings/people?peek=person:p-gone";
  stubIam();
  mount();
  const rail = await peek();
  expect(await within(rail).findByText("Not found")).toBeTruthy();
  expect(within(rail).getByText("They may have been removed.")).toBeTruthy();
  // NO RAW ID in what the reader is told.
  expect(within(rail).queryByText(/p-gone/)).toBeNull();
  fireEvent.click(within(rail).getByRole("button", { name: "Close" }));
  await waitFor(() => expect(location.hash).not.toContain("peek="));
  expect(screen.queryByText("Not found")).toBeNull();
  cleanup();

  location.hash = "#/settings/people?peek=person:p-ana";
  stubIam();
  mount();
  await within(await peek()).findByText("Authenticator app");
  expect(screen.queryByText("Not found")).toBeNull();
});

// AN ENDED SESSION SAYS WHY, AND NOT WHEN IT WOULD HAVE ENDED. One a counter
// ended carries a reason and no ended_at; its reason rode in a tooltip and its
// deadline read as "in 6d" beside "Ended". Mutation: show the expiry for every
// ended session and the deadline is drawn.
test("an ended session says why and draws no deadline it never reached", async () => {
  location.hash = "#/settings/people?peek=person:p-ana";
  stubIam((url) =>
    url.pathname === "/iam/people/p-ana/sessions"
      ? json(200, {
          sessions: [
            {
              lineage: "s-2",
              person: "p-ana",
              live: false,
              created_at: new Date(Date.now() - 3_600_000).toISOString(),
              expires_at: new Date(Date.now() + 6 * 86_400_000).toISOString(),
              ended_reason: "ended with every session they held",
            },
          ],
        })
      : null,
  );
  mount();
  const reason = await screen.findByText("ended with every session they held");
  const row = reason.closest(".crewlet-data-grid__row") as HTMLElement;
  expect(within(row).getByText("Not recorded")).toBeTruthy();
  expect(within(row).queryByText(/^in /)).toBeNull();
  // AND IT CAN BE READ WHOLE. The State column was a shrink column, capped at
  // a fifth of the grid and clipped with no ellipsis, so a reason was cut at
  // its edge with no way to read the rest. It is the flexible column now, and
  // a reason longer than it is cut with an ellipsis and whole on its title.
  // Mutation: make the State column shrink again, or drop the title.
  expect(
    reason
      .closest(".crewlet-data-grid__cell")
      ?.classList.contains("crewlet-data-grid__cell--shrink"),
  ).toBe(false);
  expect(reason.getAttribute("title")).toBe("ended with every session they held");
});

// A RESET LINK ITS PERSON USED READS USED: setting the password revokes it
// too, and "Revoked" said somebody had withdrawn it. The CONTROL is a link
// withdrawn unused. Mutation: drop `spent` from the word and both read Revoked.
test("a reset link its person used reads used, one withdrawn reads revoked", async () => {
  location.hash = "#/settings/people?peek=person:p-ana";
  stubIam((url) =>
    url.pathname === "/iam/credentials"
      ? json(200, {
          credentials: [
            {
              id: "c-used",
              person: "p-ana",
              method: "reset",
              revoked: true,
              revoked_at: "2026-10-06T03:00:00Z",
              spent: true,
            },
            {
              id: "c-withdrawn",
              person: "p-ana",
              method: "reset",
              revoked: true,
              revoked_at: "2026-10-05T03:00:00Z",
            },
          ],
        })
      : null,
  );
  mount();
  expect(await screen.findByText("Used")).toBeTruthy();
  expect(screen.getAllByText("Revoked")).toHaveLength(1);
});

// A RESET LINK CARRIES NO ACCESS: it sets a password once and signs nobody
// in, and the panel said it carried "Its holder's grants". The CONTROL is a
// password, which signs its holder in with what they hold. Mutation: give the
// link the password's words and the first expectation goes red.
test("a reset link says it sets a password, never that it carries access", async () => {
  location.hash = "#/settings/people?peek=person:p-ana";
  stubIam((url) =>
    url.pathname === "/iam/credentials"
      ? json(200, {
          credentials: [
            { id: "c-link", person: "p-ana", method: "reset", revoked: false },
            { id: "c-pw", person: "p-ana", method: "password", revoked: false },
          ],
        })
      : null,
  );
  mount();
  const link = (await screen.findByText("Password reset link")).closest(
    ".crewlet-data-grid__row",
  ) as HTMLElement;
  expect(within(link).getByText("One-time")).toBeTruthy();
  expect(within(link).queryByText("Same as owner")).toBeNull();
  // AN UNUSED LINK IS NOT "IN USE": nobody has opened it yet.
  expect(within(link).getByText("Not used yet")).toBeTruthy();
  expect(within(link).queryByText("In use")).toBeNull();
  const password = screen.getByText("Password").closest(".crewlet-data-grid__row") as HTMLElement;
  expect(within(password).getByText("Same as owner")).toBeTruthy();
  expect(within(password).getByText("In use")).toBeTruthy();
});

// AN ENDED CREDENTIAL DRAWS NO DEADLINE IT WILL NEVER REACH. A used reset
// link and a revoked token keep the deadline they were issued with, and the
// person panel read "Used — in 1d". The CONTROLS: a credential that EXPIRED
// reached its deadline, which is shown, and one in use counts down to its own.
// Mutation: draw the deadline whatever the state and the used link counts
// down.
test("an ended credential draws no deadline it will never reach", async () => {
  location.hash = "#/settings/people?peek=person:p-ana";
  const later = new Date(Date.now() + 86_400_000).toISOString();
  const earlier = new Date(Date.now() - 86_400_000).toISOString();
  stubIam((url) =>
    url.pathname === "/iam/credentials"
      ? json(200, {
          credentials: [
            {
              id: "c-used",
              person: "p-ana",
              method: "reset",
              revoked: true,
              revoked_at: earlier,
              spent: true,
              expires_at: later,
            },
            {
              id: "c-lapsed",
              person: "p-ana",
              method: "token",
              label: "lapsed",
              revoked: true,
              expires_at: earlier,
            },
            {
              id: "c-live",
              person: "p-ana",
              method: "token",
              label: "live",
              revoked: false,
              expires_at: later,
            },
          ],
        })
      : null,
  );
  mount();
  const used = (await screen.findByText("Used")).closest(".crewlet-data-grid__row") as HTMLElement;
  expect(within(used).getByText("Already ended")).toBeTruthy();
  const lapsed = screen.getByText(/· lapsed/).closest(".crewlet-data-grid__row") as HTMLElement;
  expect(within(lapsed).queryByText("Already ended")).toBeNull();
  expect(within(lapsed).getByText("Expired")).toBeTruthy();
  const live = screen.getByText(/· live/).closest(".crewlet-data-grid__row") as HTMLElement;
  expect(within(live).getByText(/^in /)).toBeTruthy();
});

// AN INVITATION SAYS WHO SENT IT AS THE CHART NAMES THEM, and why its address
// is gone. A person bound to a seat writes as the seat, so the table read
// "jane" where the invitation's own page reads "Jane Founder"; and a removal
// erases the address of the invitation its person redeemed, which left an
// empty cell. Mutation: draw the recorded author, or nothing for the erased
// address.
test("an invitation names its sender by the chart and says its address was erased", async () => {
  stubIam();
  mount();
  // TWICE: the invitation's own row, and the seat it holds on the Human seats card.
  await screen.findAllByText("sam@example.com");
  fireEvent.click(screen.getByRole("checkbox", { name: "Show used and expired" }));
  const erased = await screen.findByText("Erased with the person it invited");
  const row = erased.closest(".crewlet-data-grid__row") as HTMLElement;
  expect(within(row).getByText("Jane Founder")).toBeTruthy();
});

// THE INVITATIONS NOBODY REDEEMED, and — asked — every one the estate holds:
// a sealed address is a state, and a redeemed one says so rather than its
// deadline. Mutation: drop `all=true` from the toggle's read and the redeemed
// row never arrives.
test("the open invitations are listed, and the toggle asks for every state", async () => {
  const asked = stubIam();
  mount();
  // TWICE: the invitation's own row, and the seat it holds on the Human seats card.
  expect((await screen.findAllByText("sam@example.com")).length).toBe(2);
  expect(screen.getAllByText("Locked").length).toBeGreaterThan(0);
  expect(screen.queryByText("old@example.com")).toBeNull();
  fireEvent.click(screen.getByRole("checkbox", { name: "Show used and expired" }));
  const row = (await screen.findByText("old@example.com")).closest(
    ".crewlet-data-grid__row",
  ) as HTMLElement;
  expect(within(row).getByText(INVITATION_WORDS.redeemed!.label)).toBeTruthy();
  expect(
    asked.some((u) => u.pathname === "/iam/invitations" && u.searchParams.get("all") === "true"),
  ).toBe(true);
});
