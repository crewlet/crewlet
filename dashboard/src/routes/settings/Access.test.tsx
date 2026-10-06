/**
 * People & access draws the identity directory from both ends — what it
 * WRITES is `AccessWrites.test.tsx`'s.
 *
 * The invariants, in the order they cost when they go: the whole directory is
 * drawn, not its first page — a directory cut at a page is a company that
 * looks smaller than it is; a reader the engine refuses sees the refusal and
 * the grants that would have admitted them, never an empty company; a sealed
 * row is a state, never a blank; and a reader who may not read the company
 * document is told so rather than shown seats nobody can reach.
 */

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { INVITATION_WORDS, PeopleAndAccess, STAGE_WORDS, TOKEN_ROW_WORDS } from "./Access.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { CHART_ORG } from "~/test/orgchart.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

/** An auditor: reads the directory, and the company document beside it. */
const AUDITOR = { login: "ana.diaz", owner: "jane", grants: ["audit:read", "config:read"] };

const PAGE_ONE = {
  people: [
    {
      id: "p-ana",
      kind: "person",
      stage: "active",
      login: "ana.diaz",
      name: "Ana Diaz",
      seat: "jane",
      grants: ["audit:read", "config:read"],
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
      holder: { person: "p-ana", login: "ana.diaz", stage: "active" },
    },
    { handle: "sam", name: "Sam Support" },
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
  people_with_people_manage: 1,
  bindings_unchecked: 0,
};

const OPEN_INVITATIONS = {
  invitations: [
    { id: "inv-1", email: "sam@example.com", seat: "sam", grants: ["state:read"], state: "open" },
    { id: "inv-2", sealed: true, grants: [], state: "open" },
  ],
  next: "",
};

const ALL_INVITATIONS = {
  invitations: [
    ...OPEN_INVITATIONS.invitations,
    { id: "inv-3", email: "old@example.com", grants: [], state: "redeemed", person: "p-old" },
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

function mount(viewer: Record<string, unknown> = AUDITOR) {
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
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <ViewerProvider>
        <Router>
          <PeopleAndAccess />
        </Router>
      </ViewerProvider>
    </ClientContext.Provider>,
  );
  return { queried };
}

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  location.hash = "#/settings/access";
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
  expect((await screen.findAllByText("ci:release")).length).toBeGreaterThan(0);
  const pages = asked.filter((u) => u.pathname === "/iam/people");
  expect(pages.map((u) => u.searchParams.get("after"))).toEqual([null, "cursor-1"]);
  // Each row's stage in the engine's own words, and a machine as a service
  // account rather than a person.
  expect(screen.getAllByText(STAGE_WORDS.active!.label).length).toBeGreaterThan(0);
  expect(screen.getByText(STAGE_WORDS.suspended!.label)).toBeTruthy();
  expect(screen.getByText("Service account")).toBeTruthy();
});

// A SEALED VALUE IS A STATE the operator can end by putting a key back — never
// a blank name, which reads as somebody who never gave one.
test("a row this node cannot open is drawn as sealed", async () => {
  stubIam();
  mount();
  // THE PERSON'S ROW, and the sealed invitation's address beside it.
  expect((await screen.findAllByText("sealed")).length).toBe(2);
});

// FROM BOTH ENDS: the seat with whoever holds it, and each of this node's
// tokens with the row its login names and the seat that row binds it to — what
// the row holds, with the report alone saying whether a seat is gone.
test("a seat names who holds it and a token the seat its row binds", async () => {
  stubIam();
  mount();
  const deploy = (await screen.findByText("token:deploy")).closest(".grid-row") as HTMLElement;
  expect(within(deploy).getByText(TOKEN_ROW_WORDS.held!.label)).toBeTruthy();
  expect(within(deploy).getByRole("link", { name: /Jane Founder/ })).toBeTruthy();
  const spare = screen.getByText("token:spare").closest(".grid-row") as HTMLElement;
  expect(within(spare).getByText(TOKEN_ROW_WORDS.none!.label)).toBeTruthy();
  expect(within(spare).getByText("Bound to no seat")).toBeTruthy();
  const tokensTile = screen
    .getByText("API tokens on this node", {
      selector: ".crewlet-statcard *",
    })
    .closest(".crewlet-statcard") as HTMLElement;
  expect(within(tokensTile).getByText("1 bound to a seat")).toBeTruthy();
  // The directory's own finding, in the engine's words.
  expect(screen.getByText("The seat removed-seat is not in the org chart.")).toBeTruthy();
  // A SEAT IS HELD BY ONE PERSON OR BY NOBODY: two of the three are vacant.
  const vacant = screen.getByText("Human seats nobody holds").closest(".crewlet-statcard");
  expect(vacant?.querySelector(".crewlet-statcard__value")?.textContent).toBe("2");
  expect(within(vacant as HTMLElement).getByText("of 3 human seats")).toBeTruthy();
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
  mount();
  const row = async (login: string) =>
    (await screen.findByText(login)).closest(".grid-row") as HTMLElement;
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

// A REFUSED DIRECTORY IS NOT AN EMPTY COMPANY: the reader sees the refusal and
// the grants the engine named, and no tile counting nobody.
test("a refused reader sees the refusal and its grants, and no tiles", async () => {
  stubIam((url) =>
    url.pathname.startsWith("/iam/")
      ? json(403, {
          error: "unauthorized",
          message: "you may not",
          reason: "directory",
          grants: ["people:manage", "audit:read"],
        })
      : null,
  );
  mount({ login: "dee", owner: "dee", grants: ["state:read"] });
  const banner = await screen.findByText(/You may not read this/);
  expect(banner.textContent).toMatch(/people:manage/);
  expect(banner.textContent).toMatch(/audit:read/);
  expect(screen.queryByText("People")).toBeNull();
  expect(screen.queryByText("Nobody is in the directory yet")).toBeNull();
});

// THE CONTACT IDENTITIES ARE THE COMPANY DOCUMENT'S: a reader without
// `config:read` is never put the question, and each seat says what it takes.
test("a reader without config:read is told what reading a seat's surfaces needs", async () => {
  stubIam();
  const { queried } = mount({ login: "ana.diaz", owner: "jane", grants: ["audit:read"] });
  // ONE SENTENCE PER SEAT, every seat refused alike.
  expect(
    await screen.findAllByText(
      "Reading where a seat is reached needs config:read, which the credential you presented does not carry.",
    ),
  ).toHaveLength(3);
  expect(queried).not.toContain("config");
});

// OPENING A PERSON reads that one principal's credentials and sessions.
test("an opened person shows the credentials and sessions read for them", async () => {
  location.hash = "#/settings/access?person=p-ana";
  const asked = stubIam();
  mount();
  expect(await screen.findByText("Authenticator app")).toBeTruthy();
  expect(screen.getByText("Password")).toBeTruthy();
  expect(screen.getByText("Signed in")).toBeTruthy();
  const credentials = asked.find((u) => u.pathname === "/iam/credentials");
  expect(credentials?.searchParams.get("person")).toBe("p-ana");
});

// AN ENDED SESSION SAYS WHY, AND NOT WHEN IT WOULD HAVE ENDED. One a counter
// ended carries a reason and no ended_at; its reason rode in a tooltip and its
// deadline read as "in 6d" beside "Ended". Mutation: show the expiry for every
// ended session and the deadline is drawn.
test("an ended session says why and draws no deadline it never reached", async () => {
  location.hash = "#/settings/access?person=p-ana";
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
  const row = reason.closest(".grid-row") as HTMLElement;
  expect(within(row).getByText("Not recorded")).toBeTruthy();
  expect(within(row).queryByText(/^in /)).toBeNull();
});

// A RESET LINK ITS PERSON USED READS USED: setting the password revokes it
// too, and "Revoked" said somebody had withdrawn it. The CONTROL is a link
// withdrawn unused. Mutation: drop `spent` from the word and both read Revoked.
test("a reset link its person used reads used, one withdrawn reads revoked", async () => {
  location.hash = "#/settings/access?person=p-ana";
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

// THE INVITATIONS NOBODY REDEEMED, and — asked — every one the estate holds:
// a sealed address is a state, and a redeemed one says so rather than its
// deadline. Mutation: drop `all=true` from the toggle's read and the redeemed
// row never arrives.
test("the open invitations are listed, and the toggle asks for every state", async () => {
  const asked = stubIam();
  mount();
  expect(await screen.findByText("sam@example.com")).toBeTruthy();
  expect(screen.getAllByText("sealed").length).toBeGreaterThan(0);
  expect(screen.queryByText("old@example.com")).toBeNull();
  fireEvent.click(screen.getByRole("checkbox", { name: "Show expired and redeemed" }));
  const row = (await screen.findByText("old@example.com")).closest(".grid-row") as HTMLElement;
  expect(within(row).getByText(INVITATION_WORDS.redeemed!.label)).toBeTruthy();
  expect(
    asked.some((u) => u.pathname === "/iam/invitations" && u.searchParams.get("all") === "true"),
  ).toBe(true);
});
