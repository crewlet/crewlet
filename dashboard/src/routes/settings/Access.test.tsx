/**
 * People & access draws the identity directory as the engine answers it, and
 * changes none of it.
 *
 * The invariants, in the order they cost when they go: a reader the directory
 * is refused to sees the refusal alone — never tiles over an empty company; a
 * directory longer than one page is drawn whole; a bound seat is drawn by the
 * chart's CURRENT name, never by the identity the binding records; each Tier A
 * token's binding reads as its own state, because each has its own remedy; a
 * report this node could only half-evaluate says so rather than reading clean;
 * and a reader the chart's report alone is refused to sees every other card.
 */

import { act, answered, cleanup, fireEvent, render, screen, within } from "~/test/inCase.ts";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import type { ReactElement } from "react";

import { BINDING_WORDS, PeopleAndAccess, STAGE_WORDS, TOKEN_ROW_WORDS } from "./Access.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";

let store = new Store();

beforeEach(() => {
  store = new Store();
  // The chart this node composed: Ana's seat, under the handle a binding
  // records as its identity.
  store.applyOrg({
    name: "Acme",
    roles: [
      { name: "Ana Diaz", handle: "ana", kind: "human" },
      { name: "Ops Desk", handle: "ops", kind: "human" },
    ],
  } as never);
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function mount(): ReturnType<typeof render> {
  const ui: ReactElement = (
    <ClientContext.Provider value={{ store, socket: new LiveSocket(store) }}>
      <Router>
        <PeopleAndAccess />
      </Router>
    </ClientContext.Provider>
  );
  return render(ui);
}

function json(payload: unknown, status = 200): Response {
  return new Response(JSON.stringify(payload), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

const ANA = {
  id: "p-ana",
  kind: "person",
  stage: "active",
  login: "ana.diaz",
  name: "Ana Diaz",
  email: "ana@example.com",
  seat: "ana",
  grants: ["people:manage", "config:read"],
  colleague: "write",
  revocation_epoch: 0,
  version: 3,
};

const BO = {
  id: "p-bo",
  kind: "person",
  stage: "suspended",
  login: "bo.lang",
  name: "Bo Lang",
  colleague: "write",
  revocation_epoch: 1,
  version: 2,
};

const CI = {
  id: "p-ci",
  kind: "machine",
  stage: "active",
  login: "ci:release",
  colleague: "none",
  grants: ["work:write"],
  revocation_epoch: 0,
  version: 1,
};

/** A seat identity the chart no longer holds, drawn as written. */
const GONE = {
  id: "p-gone",
  kind: "person",
  stage: "active",
  login: "cy.moss",
  name: "Cy Moss",
  seat: "retired-desk",
  colleague: "write",
  revocation_epoch: 0,
  version: 1,
};

const TOKENS = {
  tokens: [
    { id: "break-glass", login: "token:break-glass", row: "none", binding: "unbound" },
    { id: "half", login: "token:half", row: "reserved", binding: "unbound" },
    {
      id: "ops",
      login: "token:ops",
      row: "held",
      person: "p-ops",
      stage: "active",
      seat: "ops",
      binding: "bound",
    },
    {
      id: "old",
      login: "token:old",
      row: "held",
      person: "p-old",
      stage: "active",
      seat: "gone",
      binding: "dangling",
      detail: "the seat gone is no longer in the org chart",
    },
  ],
};

const CHECK = {
  findings: [
    {
      kind: "claim_duplicated",
      claim: "login",
      login: "ana.diaz",
      people: ["p-ana", "p-bo"],
      detail: "more than one person holds this login claim",
    },
    {
      kind: "claim_orphaned",
      person: "p-half",
      login: "half.done",
      detail: "an enrolment stopped after reserving these claims",
    },
  ],
  position: "12",
  people_with_people_manage: 1,
  bindings_unchecked: 0,
};

const CHART = {
  report: {
    findings: [
      {
        kind: "seat_unheld",
        severity: "warning",
        object: "ops",
        detail: "nobody in the identity directory is bound to this seat",
        remedy: "invite its person and bind them",
      },
      {
        kind: "budget_idle",
        severity: "info",
        object: "ada",
        detail: "a ceiling nothing charges",
      },
    ],
    seats: 2,
    units: 0,
    evaluated: true,
  },
  worst: "warning",
};

type Handler = (url: URL) => Response | null;

/** Every read the screen makes, answering; `over` answers first. */
function serve(over: Handler = () => null) {
  const spy = vi.fn((input: RequestInfo | URL) => {
    const url = new URL(String(input), "http://engine.test");
    const answer = over(url);
    if (answer) return Promise.resolve(answer);
    switch (url.pathname) {
      case "/iam/people":
        return Promise.resolve(json({ people: [ANA, BO, CI, GONE], next: "", position: "12" }));
      case "/iam/node-tokens":
        return Promise.resolve(json(TOKENS));
      case "/iam/check":
        return Promise.resolve(json(CHECK));
      case "/chart/check":
        return Promise.resolve(json(CHART));
      default:
        return Promise.resolve(json({}));
    }
  });
  Object.defineProperty(globalThis, "fetch", { writable: true, value: spy });
  return spy;
}

function rowOf(text: string): HTMLElement {
  return screen.getAllByText(text)[0]!.closest(".grid-row") as HTMLElement;
}

test("a reader refused the directory sees the refusal alone — no tiles, no empty lists", async () => {
  serve((url) =>
    url.pathname.startsWith("/iam/") || url.pathname === "/chart/check"
      ? json(
          { error: "unauthorized", reason: "directory", grants: ["people:manage", "audit:read"] },
          403,
        )
      : null,
  );
  const view = mount();
  await answered();
  expect(screen.getByText("people:manage")).toBeDefined();
  expect(screen.getByText("audit:read")).toBeDefined();
  expect(view.container.querySelector(".crewlet-statcard")).toBeNull();
  expect(screen.queryByText("People")).toBeNull();
  expect(screen.queryByText("API tokens on this node")).toBeNull();
  expect(screen.queryByText("Nobody is enrolled")).toBeNull();
});

test("a directory longer than one page is walked to its end", async () => {
  const spy = serve((url) => {
    if (url.pathname !== "/iam/people") return null;
    return url.searchParams.get("after") === "p-bo"
      ? json({ people: [CI, GONE], next: "", position: "12" })
      : json({ people: [ANA, BO], next: "p-bo", position: "12" });
  });
  mount();
  await answered();
  for (const who of ["Ana Diaz", "Bo Lang", "ci:release", "Cy Moss"]) {
    expect(screen.getAllByText(who).length).toBeGreaterThan(0);
  }
  const asked = spy.mock.calls
    .map(([input]) => new URL(String(input), "http://engine.test"))
    .filter((url) => url.pathname === "/iam/people");
  expect(asked.map((url) => url.searchParams.get("after"))).toEqual([null, "p-bo"]);
});

test("each principal reads its stage and kind, and a bound seat by the chart's current name", async () => {
  serve();
  mount();
  await answered();
  const ana = rowOf("ana@example.com");
  expect(within(ana).getByText(STAGE_WORDS.active!.label)).toBeDefined();
  // THE SEAT BY ITS NAME IN THE CHART, linked to its page.
  const seat = within(ana).getByRole("link", { name: /Ana Diaz/ });
  expect(seat.getAttribute("href")).toBe("#/agents/seats/ana");
  expect(within(ana).getByText("people:manage")).toBeDefined();

  expect(within(rowOf("Bo Lang")).getByText(STAGE_WORDS.suspended!.label)).toBeDefined();
  expect(within(rowOf("ci:release")).getByText("Service account")).toBeDefined();
  // AN IDENTITY THE CHART DOES NOT HOLD is drawn as written, never as nobody.
  expect(within(rowOf("Cy Moss")).getByText("retired-desk")).toBeDefined();
});

test("each Tier A token's directory row and binding reads as its own state", async () => {
  serve();
  mount();
  await answered();
  expect(within(rowOf("break-glass")).getByText(TOKEN_ROW_WORDS.none!.label)).toBeDefined();
  expect(within(rowOf("break-glass")).getByText(BINDING_WORDS.unbound!.label)).toBeDefined();
  expect(within(rowOf("half")).getByText(TOKEN_ROW_WORDS.reserved!.label)).toBeDefined();
  const ops = rowOf("token:ops");
  expect(within(ops).getByText(BINDING_WORDS.bound!.label)).toBeDefined();
  expect(within(ops).getByRole("link", { name: /Ops Desk/ })).toBeDefined();
  // A DANGLING BINDING carries the engine's own sentence on why.
  const dangling = within(rowOf("token:old")).getByText(BINDING_WORDS.dangling!.label);
  expect(dangling.closest("[title]")?.getAttribute("title")).toBe(
    "The seat gone is no longer in the org chart.",
  );
});

test("opening a principal reads its credentials and sessions, and no verifier", async () => {
  const spy = serve((url) => {
    if (url.pathname === "/iam/credentials") {
      return json({
        credentials: [
          {
            id: "c-1",
            person: "p-ci",
            method: "token",
            label: "release pipeline",
            revoked: false,
            grants: ["work:write"],
          },
          {
            id: "c-2",
            person: "p-ci",
            method: "token",
            label: "old pipeline",
            revoked: true,
            revoked_at: "2026-09-01T00:00:00Z",
          },
          { id: "c-3", person: "p-ci", method: "token", label: "lapsed", revoked: true },
        ],
      });
    }
    if (url.pathname === "/iam/people/p-ci/sessions") return json({ sessions: [] });
    return null;
  });
  mount();
  await answered();
  act(() => fireEvent.click(rowOf("ci:release")));
  await answered();
  const asked = spy.mock.calls.map(([input]) => new URL(String(input), "http://engine.test"));
  expect(
    asked.some((u) => u.pathname === "/iam/credentials" && u.searchParams.get("person") === "p-ci"),
  ).toBe(true);
  expect(screen.getByText(/credentials and sessions/)).toBeDefined();
  expect(within(rowOf("release pipeline")).getByText("Live")).toBeDefined();
  expect(within(rowOf("old pipeline")).getByText("Revoked")).toBeDefined();
  expect(within(rowOf("lapsed")).getByText("Expired")).toBeDefined();
  expect(screen.getByText("No session")).toBeDefined();
});

test("the directory's report names a claim's holders by name, and says when bindings went unchecked", async () => {
  serve((url) =>
    url.pathname === "/iam/check" ? json({ ...CHECK, bindings_unchecked: 2 }) : null,
  );
  mount();
  await answered();
  const duplicate = document.querySelector('[data-finding="claim_duplicated"]') as HTMLElement;
  expect(within(duplicate).getByText("Held twice")).toBeDefined();
  expect(within(duplicate).getByText("Held by Ana Diaz, Bo Lang")).toBeDefined();
  const orphan = document.querySelector('[data-finding="claim_orphaned"]') as HTMLElement;
  expect(within(orphan).getByText("Reservation left behind")).toBeDefined();
  expect(screen.getByText(/2 seat bindings could not be checked on this node/)).toBeDefined();
  expect(screen.queryByText("The directory reports nothing wrong.")).toBeNull();
});

test("the chart's report shows the seats nobody holds and nothing else it found", async () => {
  serve();
  mount();
  await answered();
  const unheld = document.querySelector('[data-finding="seat_unheld"]') as HTMLElement;
  expect(within(unheld).getByText("Nobody holds it")).toBeDefined();
  expect(within(unheld).getByRole("link", { name: /Ops Desk/ })).toBeDefined();
  expect(within(unheld).getByText("Invite its person and bind them.")).toBeDefined();
  expect(document.querySelector('[data-finding="budget_idle"]')).toBeNull();
});

test("a chart that was never evaluated is not a clean bill", async () => {
  serve((url) =>
    url.pathname === "/chart/check"
      ? json({ report: { findings: null, seats: 0, units: 0, evaluated: false }, worst: "" })
      : null,
  );
  mount();
  await answered();
  expect(screen.getByText(/has not evaluated the org chart yet/)).toBeDefined();
  expect(screen.queryByText(/Every human seat is held/)).toBeNull();
});

test("a reader refused only the chart's report sees every other card and that refusal", async () => {
  serve((url) =>
    url.pathname === "/chart/check"
      ? json({ error: "unauthorized", reason: "operator", grants: ["audit:read"] }, 403)
      : null,
  );
  mount();
  await answered();
  expect(screen.getAllByText("Ana Diaz").length).toBeGreaterThan(0);
  expect(screen.getByText("break-glass")).toBeDefined();
  const card = screen.getByText("Human seats nobody holds or reaches").closest(".crewlet-card");
  expect(within(card as HTMLElement).getByText("audit:read")).toBeDefined();
});

// ABSENT EVIDENCE IS NOT A CLEAN BILL ON THE TILE EITHER. It summed whatever
// had answered, so a directory report that failed beside a clean chart — or a
// chart this node never evaluated — read "0, nothing reported" above the
// cards saying otherwise.
test("the findings tile claims nothing the reports did not establish", async () => {
  const clean = { ...CHECK, findings: [] };
  const tile = () => {
    const card = screen.getByText("Findings").closest(".crewlet-statcard") as HTMLElement;
    return card.textContent ?? "";
  };

  serve((url) =>
    url.pathname === "/iam/check"
      ? json({ error: "internal_error" }, 500)
      : url.pathname === "/chart/check"
        ? json({ ...CHART, report: { ...CHART.report, findings: [] } })
        : null,
  );
  mount();
  await answered();
  expect(tile()).toContain("a report could not be read");
  expect(tile()).not.toContain("nothing reported");
  cleanup();

  serve((url) =>
    url.pathname === "/iam/check"
      ? json(clean)
      : url.pathname === "/chart/check"
        ? json({ report: { findings: null, seats: 0, units: 0, evaluated: false }, worst: "" })
        : null,
  );
  mount();
  await answered();
  expect(tile()).toContain("not everything could be checked");
  cleanup();

  // THE CONTROL: both reports answered whole, and found nothing.
  serve((url) =>
    url.pathname === "/iam/check"
      ? json(clean)
      : url.pathname === "/chart/check"
        ? json({ ...CHART, report: { ...CHART.report, findings: [] } })
        : null,
  );
  mount();
  await answered();
  expect(tile()).toContain("nothing reported");
});
