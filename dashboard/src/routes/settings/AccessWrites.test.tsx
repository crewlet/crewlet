/**
 * People & access writes the directory, for a reader holding `people:manage`.
 *
 * The invariants, in the order they cost when they go: an auditor's read view
 * carries no write control at all; every write sends the request its route
 * takes — a keyed create under a uuid7 `Idempotency-Key`, a mint and a reset
 * link under none — and shows a value the engine shows once exactly once; an
 * unknown answer is retried under the SAME key the engine handed back; a
 * refusal is the engine's sentence with the grants that would admit; a
 * `403 step_up_required` is confirmed and the same request replayed; and every
 * list a write moved is read again.
 *
 * A PERSON IS NOT INVITED OR CREATED HERE: every person holds a human seat,
 * so they come from a vacant one on the org chart (`components/people.test.tsx`
 * holds those dialogs), and this screen points there. What it holds is that a
 * person's seat is changed and never cleared, while a service account may be
 * bound to one or none.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { ToastProvider } from "@crewlethq/ui";
import { PeopleAndAccess } from "./Access.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, setStepUpConfirmer, Store } from "~/protocol/index.ts";
import { CHART_ORG } from "~/test/orgchart.ts";
import { SessionReading } from "~/lib/frameSession.ts";
import { pick } from "~/testing.tsx";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

/** An administrator: every grant but `secrets:read`. */
const ADMIN = {
  login: "ana.admin",
  owner: "ana.admin",
  grants: [
    "state:read",
    "audit:read",
    "config:read",
    "work:write",
    "people:manage",
    "config:write",
  ],
};
/** An auditor: reads the directory and changes none of it. */
const AUDITOR = { login: "ana.diaz", owner: "jane", grants: ["audit:read", "config:read"] };

const UUID7 = /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

const PEOPLE = {
  people: [
    {
      id: "p-bo",
      kind: "person",
      stage: "active",
      login: "bo.lang",
      name: "Bo Lang",
      seat: "jane",
      grants: ["state:read"],
    },
    {
      id: "p-di",
      kind: "person",
      stage: "active",
      login: "di.moss",
      name: "Di Moss",
      seat: "kai",
      // secrets:write is a grant ADMIN does not hold.
      grants: ["state:read", "secrets:write"],
    },
    // A PERSON RECORDED BEFORE EVERY PERSON HELD A SEAT — the one state the
    // engine no longer creates, and the directory reports.
    {
      id: "p-ed",
      kind: "person",
      stage: "active",
      login: "ed.vance",
      name: "Ed Vance",
      grants: ["state:read"],
    },
    {
      id: "p-ci",
      kind: "machine",
      stage: "active",
      login: "ci:release",
      // sandbox:run is a grant ADMIN does not hold.
      grants: ["work:write", "people:manage", "sandbox:run"],
    },
  ],
  next: "",
};

const INVITATIONS = {
  invitations: [
    {
      id: "inv-1",
      email: "sam@example.com",
      seat: "sam",
      grants: ["state:read"],
      invited_by: "ana.admin",
      expires_at: "2026-10-13T09:00:00Z",
      state: "open",
    },
  ],
  next: "",
};

interface Sent {
  method: string;
  path: string;
  query: URLSearchParams;
  key: string | null;
  body: unknown;
}

function json(status: number, payload: unknown): Response {
  return new Response(JSON.stringify(payload), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

/**
 * `/iam` answering its reads, every write recorded and answered from `writes`
 * — a queue per `METHOD /path`, the last answer repeating — and the directory
 * read from `directory` at each read, so a test can move it between two.
 */
/** What `GET /auth/config` says of a second factor, for a case that reads it. */
let secondFactor = "required";
/** Whose session `GET /auth/session` names, or null for none read. */
let reader: string | null = null;
/** How many active people `GET /iam/check` counts holding `people:manage`. */
let administrators = 1;
/** What `GET /iam/invitations` answers. */
let invitations: unknown = INVITATIONS;
/** The human seats nothing holds, as `GET /iam/seats?unheld=true` answers. */
let vacancies = [{ handle: "lee", name: "Lee Ops" }];
/** Every human seat and what holds it, as `GET /iam/seats` answers. */
const SEATS = [
  {
    handle: "sam",
    name: "Sam Support",
    invitation: { id: "inv-1", email: "sam@example.com", expires_at: "2026-10-13T09:00:00Z" },
  },
  {
    handle: "jane",
    name: "Jane Founder",
    holder: { person: "p-bo", kind: "person", login: "bo.lang", stage: "active" },
  },
  {
    handle: "kai",
    name: "Kai Support",
    holder: { person: "p-di", kind: "person", login: "di.moss", stage: "active" },
  },
  { handle: "lee", name: "Lee Ops" },
];
let humanSeats: unknown[] = SEATS;
/**
 * What `GET /iam/seats` answers instead of `humanSeats`: a refusal, or
 * `"pending"` for a read that has not arrived.
 */
let seatsAnswer: Response | "pending" | null = null;

function engine(writes: Record<string, Response[]> = {}, directory: () => unknown = () => PEOPLE) {
  const sent: Sent[] = [];
  const queues = new Map<string, Response[]>(Object.entries(writes).map(([k, v]) => [k, [...v]]));
  const spy = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(String(input), "http://engine.test");
    const method = (init?.method ?? "GET").toUpperCase();
    const headers = (init?.headers ?? {}) as Record<string, string>;
    sent.push({
      method,
      path: url.pathname,
      query: url.searchParams,
      key: headers["Idempotency-Key"] ?? null,
      body: typeof init?.body === "string" ? JSON.parse(init.body) : undefined,
    });
    if (method !== "GET") {
      const queue = queues.get(`${method} ${url.pathname}`);
      const answer = queue && (queue.length > 1 ? queue.shift()! : queue[0]!);
      return Promise.resolve(answer ? answer.clone() : json(404, { error: "no_route" }));
    }
    switch (url.pathname) {
      case "/auth/config":
        return Promise.resolve(json(200, { min_password_length: 12, second_factor: secondFactor }));
      case "/auth/session":
        if (reader) return Promise.resolve(json(200, { person: reader, login: "ana.admin" }));
        break;
      case "/iam/people":
        return Promise.resolve(json(200, directory()));
      case "/iam/invitations":
        return Promise.resolve(json(200, invitations));
      case "/iam/seats":
        if (seatsAnswer === "pending") return new Promise<Response>(() => {});
        if (seatsAnswer) return Promise.resolve(seatsAnswer.clone());
        return Promise.resolve(
          json(200, { seats: url.searchParams.get("unheld") ? vacancies : humanSeats }),
        );
      case "/iam/node-tokens":
        return Promise.resolve(json(200, { tokens: [] }));
      case "/iam/check":
        return Promise.resolve(
          json(200, {
            findings: [],
            people_with_people_manage: administrators,
            bindings_unchecked: 0,
          }),
        );
      case "/iam/credentials":
        return Promise.resolve(
          json(200, {
            credentials: [
              { id: "c-pw", person: "p-bo", method: "password", revoked: false },
              { id: "c-totp", person: "p-bo", method: "totp", revoked: false },
            ],
          }),
        );
      case "/iam/people/p-bo/sessions":
      case "/iam/people/p-di/sessions":
        return Promise.resolve(json(200, { sessions: [] }));
    }
    return Promise.resolve(json(404, { error: "no_route" }));
  });
  Object.defineProperty(globalThis, "fetch", { writable: true, value: spy });
  return {
    sent,
    writes: () => sent.filter((s) => s.method !== "GET"),
    reads: (path: string) => sent.filter((s) => s.method === "GET" && s.path === path).length,
  };
}

function mount(viewer: Record<string, unknown> = ADMIN, identity = "ready") {
  const store = new Store();
  store.applyOrg(CHART_ORG);
  store.applyHealth({ status: "ok", identity });
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) => {
    if (what === "viewer") return Promise.resolve(viewer);
    if (what === "config") return Promise.resolve({ name: "Acme", roles: [], units: [] });
    return new Promise(() => {});
  };
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <ViewerProvider>
        <Router>
          <ToastProvider>
            <SessionReading>
              <PeopleAndAccess />
            </SessionReading>
          </ToastProvider>
        </Router>
      </ViewerProvider>
    </ClientContext.Provider>,
  );
}

/** Every answer in flight lands. */
async function settle() {
  await act(async () => {});
  await act(async () => {});
}

/**
 * The open invitation's row on the Invitations card — the one carrying its
 * cancellation. Its address is drawn on the Human seats card too, against the
 * seat the invitation holds.
 */
async function invitationRow(): Promise<HTMLElement> {
  const cells = await screen.findAllByText("sam@example.com");
  const row = cells
    .map((cell) => cell.closest(".grid-row") as HTMLElement | null)
    .find((r) => r && within(r).queryByRole("button", { name: /^Cancel the invitation/ }));
  if (!row) throw new Error("no invitation row carries a cancellation");
  return row;
}

let uninstall: (() => void) | null = null;

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  location.hash = "#/settings/access";
  secondFactor = "required";
  reader = null;
  administrators = 1;
  invitations = INVITATIONS;
  vacancies = [{ handle: "lee", name: "Lee Ops" }];
  humanSeats = SEATS;
  seatsAnswer = null;
});

afterEach(() => {
  cleanup();
  uninstall?.();
  uninstall = null;
  location.hash = "";
  vi.restoreAllMocks();
});

// AN AUDITOR'S READ VIEW IS AS IT WAS: no control they could not use. The
// control is an administrator, who sees every one of them — and NO "Invite
// person": a person comes from a vacant seat, so the page points at the org
// chart instead. Mutation: draw the pointer for an auditor, or the button for
// anybody, and a line here goes red.
test("an auditor sees no write control, and an administrator sees them", async () => {
  engine();
  location.hash = "#/settings/access?person=p-bo";
  mount(AUDITOR);
  await screen.findByText("Bo Lang");
  await screen.findAllByText("sam@example.com");
  // THE VIEWER HAS ANSWERED once the contact column reads the company
  // document, which only a reader holding config:read is asked for.
  await screen.findAllByText("No surface");
  for (const name of ["Invite person", "New service account", "Cancel", "Remove", "Revoke"]) {
    expect(screen.queryByRole("button", { name: new RegExp(`^${name}`) })).toBeNull();
  }
  expect(screen.queryByRole("link", { name: /Invite or create from the org chart/ })).toBeNull();
  cleanup();
  engine();
  mount(ADMIN);
  await screen.findByText("Bo Lang");
  await screen.findAllByText("sam@example.com");
  await screen.findAllByRole("button", { name: "New service account" });
  for (const name of ["New service account", "Cancel", "Remove", "Revoke"]) {
    expect(screen.getAllByRole("button", { name: new RegExp(`^${name}`) }).length).toBeGreaterThan(
      0,
    );
  }
  for (const name of ["Invite person", "Invite", "Create person"]) {
    expect(screen.queryByRole("button", { name })).toBeNull();
  }
  expect(
    screen.getByRole("link", { name: /Invite or create from the org chart/ }).getAttribute("href"),
  ).toBe("#/agents");
});

// CANCELLING AN INVITATION IS NEVER A BUTTON CALLED "CANCEL" BESIDE ANOTHER:
// the dialog's own dismissal read "Cancel" next to "Cancel invitation", and
// somebody meaning to cancel the invitation pressed it and kept it. Mutation:
// leave the dismissal's word at its default.
test("the cancel dialog's dismissal is not a second Cancel", async () => {
  engine();
  mount();
  const row = await invitationRow();
  fireEvent.click(within(row).getByRole("button", { name: /^Cancel the invitation/ }));
  const dialog = await screen.findByRole("dialog", { name: "Cancel this invitation?" });
  expect(within(dialog).getByRole("button", { name: "Keep it" })).toBeTruthy();
  expect(within(dialog).queryByRole("button", { name: "Cancel" })).toBeNull();
});

// AN INVITATION REDEEMED MEANWHILE IS REFUSED AS STALE, AND THE DIALOG OFFERS
// ONLY A WAY OUT. It kept "Keep it" and an enabled "Cancel invitation" that
// sent the same request to the same 409. The CONTROL is the sentence, which
// still says why. Mutation: draw the confirm on a stale refusal and it is
// offered again.
test("a stale refusal leaves the cancel dialog only a way out", async () => {
  const eng = engine({
    "DELETE /iam/invitations/inv-1": [
      json(409, {
        error: "stale",
        message: "That changed since you read it.",
        detail: "this invitation has already been redeemed",
      }),
    ],
  });
  mount();
  const row = await invitationRow();
  fireEvent.click(within(row).getByRole("button", { name: /^Cancel the invitation/ }));
  const dialog = await screen.findByRole("dialog", { name: "Cancel this invitation?" });
  fireEvent.click(within(dialog).getByRole("button", { name: "Cancel invitation" }));
  await settle();
  expect(within(dialog).getByRole("alert").textContent).toMatch(/already been redeemed/);
  expect(within(dialog).queryByRole("button", { name: "Cancel invitation" })).toBeNull();
  expect(within(dialog).queryByRole("button", { name: "Keep it" })).toBeNull();
  // THE FOOTER'S, beside the modal's own close in its header.
  fireEvent.click(within(dialog).getAllByRole("button", { name: "Close" }).at(-1)!);
  expect(screen.queryByRole("dialog", { name: "Cancel this invitation?" })).toBeNull();
  expect(eng.writes()).toHaveLength(1);
});

// A GESTURE RECORDED AND NOT YET APPLIED HERE (202) IS DONE, AND THE DIALOG
// OFFERS ONLY A WAY OUT. It kept "Keep it", which read as undoing a
// cancellation already durable, beside an enabled "Cancel invitation" that sent
// a second DELETE under a new key; and the edit kept Save, which sent the same
// change again. The CONTROL is the outcome, which says it is recorded.
// Mutation: draw the confirm or Save on a pending answer and it is offered
// again.
test("a write recorded but not yet applied here leaves its dialog only Done", async () => {
  const eng = engine({
    "DELETE /iam/invitations/inv-1": [json(202, { outcome: "pending", op_id: "k" })],
    "PATCH /iam/people/p-bo": [json(202, { id: "p-bo", outcome: "pending", op_id: "k" })],
  });
  mount();
  const row = await invitationRow();
  fireEvent.click(within(row).getByRole("button", { name: /^Cancel the invitation/ }));
  const cancel = await screen.findByRole("dialog", { name: "Cancel this invitation?" });
  fireEvent.click(within(cancel).getByRole("button", { name: "Cancel invitation" }));
  await settle();
  expect(within(cancel).getByRole("status").textContent).toMatch(/^Recorded\./);
  expect(within(cancel).queryByRole("button", { name: "Cancel invitation" })).toBeNull();
  expect(within(cancel).queryByRole("button", { name: "Keep it" })).toBeNull();
  fireEvent.click(within(cancel).getByRole("button", { name: "Done" }));
  expect(screen.queryByRole("dialog", { name: "Cancel this invitation?" })).toBeNull();

  location.hash = "#/settings/access?person=p-bo";
  fireEvent.click(await screen.findByRole("button", { name: "Edit login, seat and grants" }));
  const edit = await screen.findByRole("dialog", { name: "Edit Bo Lang" });
  fireEvent.click(within(edit).getByRole("checkbox", { name: "work:write" }));
  fireEvent.click(within(edit).getByRole("button", { name: "Save" }));
  await settle();
  expect(within(edit).getByRole("status").textContent).toMatch(/^Recorded\./);
  expect(within(edit).queryByRole("button", { name: "Save" })).toBeNull();
  expect(eng.writes()).toHaveLength(2);
});

// A STALE PROOF IS CONFIRMED AND THE SAME REQUEST REPLAYED, key included.
test("a step-up refusal is confirmed and the same cancellation replayed", async () => {
  const confirmer = vi.fn(async () => true);
  uninstall = setStepUpConfirmer(confirmer);
  const eng = engine({
    "DELETE /iam/invitations/inv-1": [
      json(403, { error: "step_up_required", message: "Confirm who you are." }),
      json(200, { id: "inv-1", outcome: "applied", op_id: "k", position: "p" }),
    ],
  });
  mount();
  const row = await invitationRow();
  fireEvent.click(within(row).getByRole("button", { name: /^Cancel the invitation/ }));
  const dialog = await screen.findByRole("dialog", { name: "Cancel this invitation?" });
  const before = eng.reads("/iam/invitations");
  fireEvent.click(within(dialog).getByRole("button", { name: "Cancel invitation" }));
  await settle();
  expect(confirmer).toHaveBeenCalledOnce();
  const [refused, replayed] = eng.writes();
  expect(replayed?.path).toBe("/iam/invitations/inv-1");
  expect(replayed?.key).toBe(refused?.key);
  expect(screen.queryByRole("dialog", { name: "Cancel this invitation?" })).toBeNull();
  expect(eng.reads("/iam/invitations")).toBeGreaterThan(before);
});

// WHILE "CONFIRM IT IS YOU" ASKS, THE DIALOG BEHIND SAYS IT WAITS FOR THE
// PERSON: it pressed "Working" and could not be closed "waiting for the
// engine", while nothing was waiting on the engine at all. Once confirmed it
// is the engine's to answer again. Mutation: drop the step-up from the label
// and the button reads "Working" throughout.
test("a write held for a step-up says it waits for the person", async () => {
  let release!: (confirmed: boolean) => void;
  uninstall = setStepUpConfirmer(
    () =>
      new Promise<boolean>((resolve) => {
        release = resolve;
      }),
  );
  engine({
    "DELETE /iam/invitations/inv-1": [
      json(403, { error: "step_up_required", message: "Confirm who you are." }),
      json(200, { id: "inv-1", outcome: "applied", op_id: "k", position: "p" }),
    ],
  });
  mount();
  const row = await invitationRow();
  fireEvent.click(within(row).getByRole("button", { name: /^Cancel the invitation/ }));
  const dialog = await screen.findByRole("dialog", { name: "Cancel this invitation?" });
  fireEvent.click(within(dialog).getByRole("button", { name: "Cancel invitation" }));
  await settle();
  expect(within(dialog).getByRole("button", { name: "Waiting for you" })).toBeTruthy();
  await act(async () => release(true));
  await settle();
  expect(screen.queryByRole("dialog", { name: "Cancel this invitation?" })).toBeNull();
});

// A RESET SAYS WHAT THIS DEPLOYMENT ASKS: where a second factor is optional,
// it promises no enrolment the next sign-in never asks for. The CONTROL is the
// reset where one is required. Mutation: promise the enrolment whatever the
// setting.
test.each([
  ["optional", /password alone until they set up a new one/],
  ["required", /enrol a new factor at their next sign-in/],
])("a second factor's reset says what %s asks", async (setting, after) => {
  secondFactor = setting;
  engine();
  location.hash = "#/settings/access?person=p-bo";
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Reset second factor" }));
  const reset = await screen.findByRole("dialog", { name: "Reset Bo Lang's second factor?" });
  expect(await within(reset).findByText(after)).toBeTruthy();
  secondFactor = "required";
});

// A PERSON KEEPS THEIR SEAT THROUGH A SUSPENSION, and a removal is what frees
// it: both say so, naming the seat as the chart does — the seat the reader
// looks for next. Mutation: name it by its handle, or say a suspension frees
// it, and a line here goes red.
test("a seated person's suspension withholds their seat, and a removal frees it, by name", async () => {
  engine();
  location.hash = "#/settings/access?person=p-bo";
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Suspend" }));
  const suspend = await screen.findByRole("dialog", { name: "Suspend Bo Lang?" });
  expect(suspend.textContent).toMatch(
    /their seat Jane Founder is withheld — it stays theirs, and nobody else can hold it meanwhile/,
  );
  fireEvent.click(within(suspend).getByRole("button", { name: "Cancel" }));
  fireEvent.click(screen.getByRole("button", { name: "Remove" }));
  const remove = await screen.findByRole("dialog", { name: "Remove Bo Lang?" });
  expect(remove.textContent).toMatch(
    /their seat Jane Founder is freed and stands vacant in the org chart/,
  );
  expect(remove.textContent).toMatch(/invite them onto a seat again/);
});

// A SERVICE ACCOUNT BOUND TO NO SEAT is told nothing about one, and its
// removal is undone the way it was made — a create, never an invitation,
// which nobody redeems for a machine. The CONTROL is the seated person above.
// Mutation: name a seat whatever the row holds, or word a machine's removal
// as a person's.
test("a service account bound to no seat is told nothing about one, and is created back", async () => {
  engine();
  location.hash = "#/settings/access?person=p-ci";
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Suspend" }));
  const suspend = await screen.findByRole("dialog", { name: "Suspend ci:release?" });
  expect(within(suspend).queryByText(/seat/)).toBeNull();
  fireEvent.click(within(suspend).getByRole("button", { name: "Cancel" }));
  fireEvent.click(screen.getByRole("button", { name: "Remove" }));
  const remove = await screen.findByRole("dialog", { name: "Remove ci:release?" });
  expect(within(remove).queryByText(/seat/)).toBeNull();
  expect(remove.textContent).toMatch(/create the service account again/);
  expect(remove.textContent).not.toMatch(/invite/);
});

// WHAT ENDS A PERSON'S SESSIONS ENDS THEIR RESET LINK, and each gesture that
// does says so: an administrator who sent the link and then reset the second
// factor of somebody who lost both handed them a link that answered 410, with
// nothing on either dialog to say why. The link's own dialog names the order
// that works, and a reactivation says the link it ended stays ended. Mutation:
// drop the link from any one dialog and its assertion goes red.
test("every gesture that ends a reset link says so, and the link comes after them", async () => {
  engine();
  location.hash = "#/settings/access?person=p-bo";
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Reset second factor" }));
  const factor = await screen.findByRole("dialog", { name: "Reset Bo Lang's second factor?" });
  expect(
    within(factor).getByText(
      /every session, token and password reset link they hold ends — if they need a new password too, issue the link after this/,
    ),
  ).toBeTruthy();
  fireEvent.click(within(factor).getByRole("button", { name: "Cancel" }));
  fireEvent.click(screen.getByRole("button", { name: "End all sessions" }));
  const sessions = await screen.findByRole("dialog", { name: "End every session Bo Lang holds?" });
  expect(
    within(sessions).getByText(
      /any password reset link issued for them stop working too — if they need a new password, issue the link after this/,
    ),
  ).toBeTruthy();
  fireEvent.click(within(sessions).getByRole("button", { name: "Cancel" }));
  fireEvent.click(screen.getByRole("button", { name: "Suspend" }));
  const suspend = await screen.findByRole("dialog", { name: "Suspend Bo Lang?" });
  expect(
    within(suspend).getByText(/every session, token and password reset link they hold ends/),
  ).toBeTruthy();
  fireEvent.click(within(suspend).getByRole("button", { name: "Cancel" }));
  fireEvent.click(screen.getByRole("button", { name: "Issue password reset link" }));
  const link = await screen.findByRole("dialog", {
    name: "Issue a password reset link for Bo Lang?",
  });
  expect(
    within(link).getByText(
      /resetting their second factor, ending their sessions or suspending them ends it too — so issue it after those/,
    ),
  ).toBeTruthy();
  cleanup();

  engine({}, () => ({
    ...PEOPLE,
    people: PEOPLE.people.map((p) => (p.id === "p-bo" ? { ...p, stage: "suspended" } : p)),
  }));
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Reactivate" }));
  const reactivate = await screen.findByRole("dialog", { name: "Reactivate Bo Lang?" });
  expect(
    within(reactivate).getByText(
      /password reset link the suspension ended stay ended, so issue a new link if they need one/,
    ),
  ).toBeTruthy();
});

// AN EDIT SENDS ONLY WHAT CHANGED, and a suspension is the stage alone.
test("an opened person's edit sends what changed, and suspending sends the stage", async () => {
  const eng = engine({
    "PATCH /iam/people/p-bo": [json(200, { id: "p-bo", outcome: "applied", op_id: "k" })],
  });
  location.hash = "#/settings/access?person=p-bo";
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Edit login, seat and grants" }));
  const edit = await screen.findByRole("dialog", { name: "Edit Bo Lang" });
  fireEvent.click(within(edit).getByRole("checkbox", { name: "work:write" }));
  fireEvent.click(within(edit).getByRole("button", { name: "Save" }));
  await settle();
  fireEvent.click(screen.getByRole("button", { name: "Suspend" }));
  const suspend = await screen.findByRole("dialog", { name: "Suspend Bo Lang?" });
  fireEvent.click(within(suspend).getByRole("button", { name: "Suspend" }));
  await settle();
  expect(eng.writes().map((w) => w.body)).toEqual([
    { add_grants: ["work:write"] },
    { stage: "suspended" },
  ]);
  for (const w of eng.writes()) expect(w.key).toMatch(UUID7);
});

// A REFUSAL PART WAY IS READ AGAIN TOO: the login and seat landed before the
// grants were refused, so the directory is re-read and the dialog says what
// changed. Mutation: re-read only after a `done` answer and the directory
// keeps the old login until the poll.
test("an edit refused part way reads the directory again", async () => {
  const eng = engine({
    "PATCH /iam/people/p-bo": [
      json(403, {
        error: "unauthorized",
        message: "The credential you presented does not carry the grant this request needs.",
        id: "p-bo",
        landed: ["identity"],
      }),
    ],
  });
  location.hash = "#/settings/access?person=p-bo";
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Edit login, seat and grants" }));
  const edit = await screen.findByRole("dialog", { name: "Edit Bo Lang" });
  fireEvent.change(within(edit).getByLabelText("Login"), { target: { value: "bo.lange" } });
  fireEvent.click(within(edit).getByRole("checkbox", { name: "work:write" }));
  const before = eng.reads("/iam/people");
  fireEvent.click(within(edit).getByRole("button", { name: "Save" }));
  await settle();
  expect(within(edit).getByRole("alert").textContent).toMatch(
    /What did change: the login and seat\./,
  );
  expect(eng.reads("/iam/people")).toBeGreaterThan(before);
});

// AN EDIT SENDS WHAT ITS EDITOR CHANGED, measured against the row it opened
// on: while the dialog is open another administrator strips Bo's state:read
// and binds him to a seat, the directory is read again as the tab comes back,
// and a login change saved after it sends the login alone — never the grant
// and the seat the dialog opened with, which would undo both. Mutation:
// measure against the live row and the save sends `grants` and `seat` too.
test("an edit sends what its editor changed, whatever the directory read since", async () => {
  let people: unknown = PEOPLE;
  const eng = engine(
    { "PATCH /iam/people/p-bo": [json(200, { id: "p-bo", outcome: "applied", op_id: "k" })] },
    () => people,
  );
  location.hash = "#/settings/access?person=p-bo";
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Edit login, seat and grants" }));
  const edit = await screen.findByRole("dialog", { name: "Edit Bo Lang" });
  people = {
    ...PEOPLE,
    people: PEOPLE.people.map((p) => (p.id === "p-bo" ? { ...p, grants: [], seat: "sam" } : p)),
  };
  const before = eng.reads("/iam/people");
  act(() => {
    document.dispatchEvent(new Event("visibilitychange"));
  });
  await settle();
  expect(eng.reads("/iam/people")).toBeGreaterThan(before);
  fireEvent.change(within(edit).getByLabelText("Login"), { target: { value: "bo.lange" } });
  fireEvent.click(within(edit).getByRole("button", { name: "Save" }));
  await settle();
  expect(eng.writes().map((w) => w.body)).toEqual([{ login: "bo.lange" }]);
});

// THE SEAT A PERSON HOLDS IS OFFERED BY ITS NAME. The vacancy list leaves it
// out, so the edit added it back named by its handle — "jane / jane" beside
// every vacancy's name. Mutation: name it by its handle and "Jane Founder" is
// nowhere in the dialog.
test("an edit offers the seat its person holds by the seat's name", async () => {
  engine({}, () => ({
    ...PEOPLE,
    people: PEOPLE.people.map((p) => (p.id === "p-bo" ? { ...p, seat: "jane" } : p)),
  }));
  location.hash = "#/settings/access?person=p-bo";
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Edit login, seat and grants" }));
  const edit = await screen.findByRole("dialog", { name: "Edit Bo Lang" });
  await settle();
  expect(within(edit).getAllByText("Jane Founder").length).toBeGreaterThan(0);
});

// A PERSON'S SEAT IS CHANGED, NEVER CLEARED: the engine refuses a person's
// `seat: ""` (`seat_required`), so "No seat" is not offered, and the helper
// says removing them is what frees it. Moving them sends the seat chosen.
// Mutation: offer "No seat" to everybody and the option is drawn here.
test("a person's edit offers no 'No seat', and moving them sends the seat chosen", async () => {
  const eng = engine({
    "PATCH /iam/people/p-bo": [json(200, { id: "p-bo", outcome: "applied", op_id: "k" })],
  });
  location.hash = "#/settings/access?person=p-bo";
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Edit login, seat and grants" }));
  const edit = await screen.findByRole("dialog", { name: "Edit Bo Lang" });
  await settle();
  expect(within(edit).getByText(/A person always holds one/)).toBeTruthy();
  fireEvent.click(within(edit).getByLabelText("Seat"));
  expect(screen.queryByRole("option", { name: /No seat/ })).toBeNull();
  fireEvent.mouseDown(screen.getByRole("option", { name: /Lee Ops/ }));
  fireEvent.click(within(edit).getByRole("button", { name: "Save" }));
  await settle();
  expect(eng.writes().map((w) => w.body)).toEqual([{ seat: "lee" }]);
});

// A PERSON RECORDED WITH NO SEAT — before every person held one — is told so,
// starts on no choice rather than on "No seat", and an edit that chooses none
// sends none: a person's `seat: ""` is refused, and sent beside an unrelated
// change it failed the whole edit. Choosing one sends it. Mutation: start the
// select on the row's empty seat and send it whenever it "differs".
test("a person with no seat is warned, and the edit sends a seat only once one is chosen", async () => {
  const eng = engine({
    "PATCH /iam/people/p-ed": [json(200, { id: "p-ed", outcome: "applied", op_id: "k" })],
  });
  location.hash = "#/settings/access?person=p-ed";
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Edit login, seat and grants" }));
  const edit = await screen.findByRole("dialog", { name: "Edit Ed Vance" });
  await settle();
  expect(
    within(edit).getByText(/Ed Vance holds no seat — recorded before every person held one/),
  ).toBeTruthy();
  expect(within(edit).getByLabelText("Seat").textContent).toMatch(/Choose a seat/);
  fireEvent.click(within(edit).getByRole("checkbox", { name: "work:write" }));
  fireEvent.click(within(edit).getByRole("button", { name: "Save" }));
  await settle();
  expect(eng.writes().map((w) => w.body)).toEqual([{ add_grants: ["work:write"] }]);
  cleanup();

  const again = engine({
    "PATCH /iam/people/p-ed": [json(200, { id: "p-ed", outcome: "applied", op_id: "k" })],
  });
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Edit login, seat and grants" }));
  const chosen = await screen.findByRole("dialog", { name: "Edit Ed Vance" });
  await settle();
  pick(within(chosen).getByLabelText("Seat"), /Lee Ops/);
  fireEvent.click(within(chosen).getByRole("button", { name: "Save" }));
  await settle();
  expect(again.writes().map((w) => w.body)).toEqual([{ seat: "lee" }]);
});

// A SERVICE ACCOUNT MAY HOLD A SEAT OR NONE, so its edit keeps "No seat" and
// choosing it unbinds — the one holder a seat is unbound from. The CONTROL is
// the person above, who is offered no such thing.
test("a service account's edit offers No seat, and choosing it unbinds the account", async () => {
  const eng = engine(
    { "PATCH /iam/people/p-ci": [json(200, { id: "p-ci", outcome: "applied", op_id: "k" })] },
    () => ({
      ...PEOPLE,
      people: PEOPLE.people.map((p) => (p.id === "p-ci" ? { ...p, seat: "ops" } : p)),
    }),
  );
  location.hash = "#/settings/access?person=p-ci";
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Edit login, seat and grants" }));
  const edit = await screen.findByRole("dialog", { name: "Edit ci:release" });
  await settle();
  expect(within(edit).getByText(/No seat unbinds it/)).toBeTruthy();
  pick(within(edit).getByLabelText("Seat"), /^No seat/);
  fireEvent.click(within(edit).getByRole("button", { name: "Save" }));
  await settle();
  expect(eng.writes().map((w) => w.body)).toEqual([{ seat: "" }]);
});

// A GRANT CHANGE IS WHAT ITS EDITOR TICKED AND UNTICKED, which the engine
// applies to what the person holds when it decides: while the dialog is open
// another administrator strips Bo's state:read and gives him audit:read, the
// directory is read again as the tab comes back, and ticking work:write sends
// that tick alone — never the whole list, which would hand state:read back and
// take audit:read away, and which the engine would accept, since only an
// addition needs the editor to hold the grant. Mutation: send `grants` whole
// and the save carries state:read without audit:read.
test("a grant change sends the ticks alone, whatever another administrator changed", async () => {
  let people: unknown = PEOPLE;
  const eng = engine(
    { "PATCH /iam/people/p-bo": [json(200, { id: "p-bo", outcome: "applied", op_id: "k" })] },
    () => people,
  );
  location.hash = "#/settings/access?person=p-bo";
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Edit login, seat and grants" }));
  const edit = await screen.findByRole("dialog", { name: "Edit Bo Lang" });
  people = {
    ...PEOPLE,
    people: PEOPLE.people.map((p) => (p.id === "p-bo" ? { ...p, grants: ["audit:read"] } : p)),
  };
  const before = eng.reads("/iam/people");
  act(() => {
    document.dispatchEvent(new Event("visibilitychange"));
  });
  await settle();
  expect(eng.reads("/iam/people")).toBeGreaterThan(before);
  fireEvent.click(within(edit).getByRole("checkbox", { name: "work:write" }));
  fireEvent.click(within(edit).getByRole("button", { name: "Save" }));
  await settle();
  expect(eng.writes().map((w) => w.body)).toEqual([{ add_grants: ["work:write"] }]);
});

// A SUSPENSION RETRIED IS A SUSPENSION: it is answered unknown, the directory
// read after it shows it landed, and "Try again" sends the same stage under
// the key the engine handed back — the dialog still asks to suspend. Mutation:
// build the request from the live row and the retry sends `stage: "active"`
// under that key, which the engine takes as a new operation and reactivates
// the person just suspended.
test("a suspension retried after an unknown answer sends the suspension again", async () => {
  let people: unknown = PEOPLE;
  const eng = engine(
    {
      "PATCH /iam/people/p-bo": [
        json(503, {
          error: "unavailable",
          outcome: "unknown",
          op_id: "0192f4c8-0000-7000-8000-000000000002",
          message: "This node cannot answer that right now.",
        }),
        json(200, { id: "p-bo", outcome: "applied", op_id: "k" }),
      ],
    },
    () => people,
  );
  location.hash = "#/settings/access?person=p-bo";
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Suspend" }));
  const dialog = await screen.findByRole("dialog", { name: "Suspend Bo Lang?" });
  people = {
    ...PEOPLE,
    people: PEOPLE.people.map((p) => (p.id === "p-bo" ? { ...p, stage: "suspended" } : p)),
  };
  const before = eng.reads("/iam/people");
  fireEvent.click(within(dialog).getByRole("button", { name: "Suspend" }));
  await settle();
  expect(within(dialog).getByText(/could not confirm whether this landed/)).toBeTruthy();
  expect(eng.reads("/iam/people")).toBeGreaterThan(before);
  expect(screen.getByRole("button", { name: "Reactivate" })).toBeTruthy();
  expect(screen.getByRole("dialog", { name: "Suspend Bo Lang?" })).toBe(dialog);
  fireEvent.click(within(dialog).getByRole("button", { name: "Try again" }));
  await settle();
  expect(eng.writes().map((w) => [w.body, w.key])).toEqual([
    [{ stage: "suspended" }, expect.stringMatching(UUID7)],
    [{ stage: "suspended" }, "0192f4c8-0000-7000-8000-000000000002"],
  ]);
});

// AN EDIT MAY TAKE AWAY A GRANT THE EDITOR DOES NOT HOLD, as the engine
// allows (it checks only what an edit adds): Di's secrets:write is enabled
// and unticking it sends its removal. The CONTROL is a grant
// neither of them holds — secrets:read stays locked here, as it does in the
// invitation, which confers from nothing. Mutation: hand the picker the
// viewer's grants alone and secrets:write is locked ticked.
test("an edit takes away a grant the editor does not hold, and adds none", async () => {
  const eng = engine({
    "PATCH /iam/people/p-di": [json(200, { id: "p-di", outcome: "applied", op_id: "k" })],
  });
  location.hash = "#/settings/access?person=p-di";
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Edit login, seat and grants" }));
  const edit = await screen.findByRole("dialog", { name: "Edit Di Moss" });
  const held = within(edit).getByRole("checkbox", { name: "secrets:write" });
  expect(held).toHaveProperty("checked", true);
  expect(held).toHaveProperty("disabled", false);
  expect(within(edit).getByRole("checkbox", { name: "secrets:read" })).toHaveProperty(
    "disabled",
    true,
  );
  fireEvent.click(held);
  fireEvent.click(within(edit).getByRole("button", { name: "Save" }));
  await settle();
  expect(eng.writes().map((w) => w.body)).toEqual([{ remove_grants: ["secrets:write"] }]);
});

// A MACHINE'S EDIT ADDS NEITHER GRANT A TOKEN NEVER CARRIES, and takes away
// one it holds. ci:release holds people:manage — written before the engine
// refused it — so that box stays live and unticking it sends its removal,
// while secrets:read is locked with why, though ADMIN could otherwise confer
// nothing it does not hold either way. Mutation: withhold both whatever the
// account holds, and people:manage is locked ticked where nobody can clear it.
test("a machine's edit adds no grant a token never carries, and takes one away", async () => {
  const eng = engine({
    "PATCH /iam/people/p-ci": [json(200, { id: "p-ci", outcome: "applied", op_id: "k" })],
  });
  location.hash = "#/settings/access?person=p-ci";
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Edit login, seat and grants" }));
  const edit = await screen.findByRole("dialog", { name: "Edit ci:release" });
  const held = within(edit).getByRole("checkbox", { name: "people:manage" });
  expect(held).toHaveProperty("disabled", false);
  expect(within(edit).getByRole("checkbox", { name: "secrets:read" })).toHaveProperty(
    "disabled",
    true,
  );
  expect(within(edit).getByText(/acts only through tokens/)).toBeTruthy();
  fireEvent.click(held);
  fireEvent.click(within(edit).getByRole("button", { name: "Save" }));
  await settle();
  expect(eng.writes().map((w) => w.body)).toEqual([{ remove_grants: ["people:manage"] }]);
});

// A TOKEN STARTS FROM WHAT ITS MINTER MAY CONFER: the account's sandbox:run,
// which ADMIN does not hold and the engine would refuse, is neither ticked nor
// sent. Mutation: seed the selection from the account's grants alone and the
// mint carries sandbox:run, locked ticked where nobody can clear it.
test("a mint leaves out an account's grant its minter does not hold", async () => {
  const eng = engine({
    "POST /iam/credentials": [
      json(201, { id: "c-tok", person: "p-ci", token: "cwl_pat_c-tok_1_s", grants: [] }),
    ],
  });
  location.hash = "#/settings/access?person=p-ci";
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Mint token" }));
  const mint = await screen.findByRole("dialog", { name: "Mint a token for ci:release" });
  expect(within(mint).getByRole("checkbox", { name: "sandbox:run" })).toHaveProperty(
    "checked",
    false,
  );
  fireEvent.click(within(mint).getByRole("button", { name: "Mint" }));
  await settle();
  expect(eng.writes().map((w) => w.body)).toEqual([{ grants: ["work:write"] }]);
});

// A RESET LINK READS NO KEY and is shown once; REMOVING somebody waits for
// their login typed back; REVOKING names the owner.
test("a reset link is issued unkeyed, a removal is typed back, a revocation names the owner", async () => {
  const eng = engine({
    "POST /iam/people/p-bo/password-reset": [
      json(201, {
        id: "p-bo",
        credential: "c-reset",
        url: "https://crewlet.example.com/dashboard#/reset/c-reset.s",
        expires_at: "2026-10-07T09:00:00Z",
        outcome: "applied",
      }),
    ],
    "DELETE /iam/people/p-bo": [json(200, { id: "p-bo", outcome: "applied", op_id: "k" })],
    "DELETE /iam/credentials/c-totp": [json(200, { id: "c-totp", outcome: "applied" })],
  });
  location.hash = "#/settings/access?person=p-bo";
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Issue password reset link" }));
  const issue = await screen.findByRole("dialog", { name: /Issue a password reset link/ });
  fireEvent.click(within(issue).getByRole("button", { name: "Issue link" }));
  await settle();
  expect(screen.getByText("https://crewlet.example.com/dashboard#/reset/c-reset.s")).toBeTruthy();
  // DONE ALONE: the link exists, and a Cancel read as a way to take it back.
  const shown = screen.getByRole("dialog", { name: /Password reset link for/ });
  expect(within(shown).queryByRole("button", { name: "Cancel" })).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Done" }));

  fireEvent.click(await screen.findByRole("button", { name: "Revoke Authenticator app" }));
  const revoke = await screen.findByRole("dialog", { name: /Revoke this authenticator app/ });
  fireEvent.click(within(revoke).getByRole("button", { name: "Revoke" }));
  await settle();

  // LAST, because a removal closes the panel it was made from.
  fireEvent.click(screen.getByRole("button", { name: "Remove" }));
  const remove = await screen.findByRole("dialog", { name: "Remove Bo Lang?" });
  const confirm = within(remove).getByRole("button", { name: "Remove" });
  expect(confirm).toHaveProperty("disabled", true);
  // THE LOGIN IS SHOWN AS IT IS TYPED, outside the micro-label register, which
  // sets a label in capitals: read as BO.LANG, typing that was refused.
  expect(within(remove).getByText("bo.lang").closest(".crewlet-label")).toBeNull();
  fireEvent.change(within(remove).getByLabelText("Type bo.lang to confirm"), {
    target: { value: "bo.lang" },
  });
  fireEvent.click(confirm);
  await settle();

  const [reset, revocation, removal] = eng.writes();
  expect(reset?.path).toBe("/iam/people/p-bo/password-reset");
  expect(reset?.key).toBeNull();
  expect(revocation?.path).toBe("/iam/credentials/c-totp");
  expect(revocation?.query.get("person")).toBe("p-bo");
  expect(removal?.method).toBe("DELETE");
  expect(removal?.path).toBe("/iam/people/p-bo");
});

// A REMOVAL CLOSES THE PANEL IT WAS MADE FROM, and says what it did: left
// open, the directory read again no longer held the row, so the panel turned
// into the note for a link naming nobody — "Nobody in the directory has this
// id", a raw id beside it — right after the reader's own action. The CONTROL
// is that note itself, which a link to somebody since removed still draws
// (`Access.test.tsx`). Mutation: drop the panel's removal callback and the
// note, the id and the `person=` parameter all stay.
test("removing the open person closes their panel and says they were removed", async () => {
  let removed = false;
  engine(
    { "DELETE /iam/people/p-bo": [json(200, { id: "p-bo", outcome: "applied", op_id: "k" })] },
    () => (removed ? { people: PEOPLE.people.filter((p) => p.id !== "p-bo"), next: "" } : PEOPLE),
  );
  location.hash = "#/settings/access?person=p-bo";
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Remove" }));
  const remove = await screen.findByRole("dialog", { name: "Remove Bo Lang?" });
  fireEvent.change(within(remove).getByLabelText("Type bo.lang to confirm"), {
    target: { value: "bo.lang" },
  });
  removed = true;
  fireEvent.click(within(remove).getByRole("button", { name: "Remove" }));
  await settle();
  expect((await screen.findAllByText("Bo Lang was removed")).length).toBeGreaterThan(0);
  expect(location.hash).toBe("#/settings/access");
  expect(screen.queryByText("Nobody in the directory has this id")).toBeNull();
  expect(screen.queryByText("p-bo")).toBeNull();
  expect(screen.queryByRole("dialog", { name: "Remove Bo Lang?" })).toBeNull();
});

// TAKING YOUR OWN people:manage IN AN EDIT SAYS IT IS YOU, and when nobody
// else administers, as a suspension and a removal do: the only administrator
// could untick it on their own row with no word, and the engine has no
// last-administrator guard — afterwards only a Tier A token administers
// people. The dialog speaks to them throughout. The CONTROLS: the same edit
// keeping the grant says nothing, and with a second administrator the
// sentence about nobody else goes. Mutation: drop `you` or `alone` from the
// edit and a line here goes red.
test("taking your own people:manage in an edit says it is you, and when nobody else administers", async () => {
  const ana = {
    id: "p-ana",
    kind: "person",
    stage: "active",
    login: "ana.admin",
    name: "Ana Admin",
    grants: ADMIN.grants,
  };
  const directory = () => ({ people: [...PEOPLE.people, ana], next: "" });
  reader = "p-ana";
  for (const [count, nobodyElse] of [
    [1, true],
    [2, false],
  ] as const) {
    administrators = count;
    engine({}, directory);
    location.hash = "#/settings/access?person=p-ana";
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "Edit login, seat and grants" }));
    const edit = await screen.findByRole("dialog", { name: "Edit Ana Admin" });
    await settle();
    expect(within(edit).queryByText("This is you")).toBeNull();
    expect(within(edit).getByText(/how you sign in, beside your address/)).toBeTruthy();
    fireEvent.click(within(edit).getByRole("checkbox", { name: "people:manage" }));
    expect(within(edit).getByText("This is you")).toBeTruthy();
    expect(within(edit).getByText(/you stop administering people/)).toBeTruthy();
    expect(within(edit).queryByText(/Nobody else active holds/) !== null).toBe(nobodyElse);
    fireEvent.click(within(edit).getByRole("checkbox", { name: "state:read" }));
    expect(within(edit).getByText(/you can open nothing but your own/)).toBeTruthy();
    cleanup();
  }
});

// A GESTURE ON THE READER'S OWN ROW SAYS IT IS THEM and that it signs them out
// at once, and the whole dialog speaks to them; the one administrator left
// suspending or removing themselves is told that only a Tier A token could
// administer people afterwards. The CONTROLS: a second administrator takes
// that sentence away, and somebody else's row says neither and names them.
// Mutation: compare the row with anything but the session's person, drop the
// count, or word a body in the third person whoever reads it, and one side
// goes red.
test("suspending or removing yourself says it is you, and when nobody else administers", async () => {
  const ana = {
    id: "p-ana",
    kind: "person",
    stage: "active",
    login: "ana.admin",
    name: "Ana Admin",
    grants: ADMIN.grants,
  };
  const directory = () => ({ people: [...PEOPLE.people, ana], next: "" });
  reader = "p-ana";
  engine({}, directory);
  location.hash = "#/settings/access?person=p-ana";
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Suspend" }));
  const suspend = await screen.findByRole("dialog", { name: "Suspend yourself?" });
  expect(within(suspend).getByText("This is you")).toBeTruthy();
  expect(await within(suspend).findByText(/Nobody else active holds/)).toBeTruthy();
  // THE WHOLE DIALOG SPEAKS TO THEM, its body included: "Ana Admin may not
  // act while suspended" under "This is you" read as two people.
  expect(within(suspend).getByText(/^You may not act while suspended/)).toBeTruthy();
  expect(within(suspend).queryByText(/Ana Admin/)).toBeNull();
  fireEvent.click(within(suspend).getByRole("button", { name: "Cancel" }));
  fireEvent.click(screen.getByRole("button", { name: "Remove" }));
  const remove = await screen.findByRole("dialog", { name: "Remove yourself?" });
  expect(within(remove).getByText(/Nobody else active holds/)).toBeTruthy();
  expect(within(remove).getByText(/^Your row, credentials and sessions are deleted/)).toBeTruthy();
  fireEvent.click(within(remove).getByRole("button", { name: "Cancel" }));
  fireEvent.click(screen.getByRole("button", { name: "End all sessions" }));
  const sessions = await screen.findByRole("dialog", { name: "End every session you hold?" });
  expect(within(sessions).getByText(/^You are signed out everywhere/)).toBeTruthy();
  fireEvent.click(within(sessions).getByRole("button", { name: "Cancel" }));
  fireEvent.click(screen.getByRole("button", { name: "Reset second factor" }));
  const factor = await screen.findByRole("dialog", { name: "Reset your second factor?" });
  expect(within(factor).getByText(/^Your authenticator app/)).toBeTruthy();
  expect(
    await within(factor).findByText(/You enrol a new factor at your next sign-in/),
  ).toBeTruthy();
  fireEvent.click(within(factor).getByRole("button", { name: "Cancel" }));
  // AND A RESET LINK, which said "for Ana Admin … for them" to Ana herself,
  // and says where a password she knows is changed with no link at all.
  fireEvent.click(screen.getByRole("button", { name: "Issue password reset link" }));
  const link = await screen.findByRole("dialog", {
    name: "Issue a password reset link for yourself?",
  });
  expect(within(link).getByText(/^The link lets you choose a new password/)).toBeTruthy();
  expect(within(link).getByText(/Account › Security needs no link/)).toBeTruthy();
  expect(within(link).queryByText(/Ana Admin/)).toBeNull();
  cleanup();

  administrators = 2;
  engine({}, directory);
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Remove" }));
  const another = await screen.findByRole("dialog", { name: "Remove yourself?" });
  await settle();
  expect(within(another).getByText("This is you")).toBeTruthy();
  expect(within(another).queryByText(/Nobody else active holds/)).toBeNull();
  cleanup();

  location.hash = "#/settings/access?person=p-bo";
  engine({}, directory);
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Suspend" }));
  const theirs = await screen.findByRole("dialog", { name: "Suspend Bo Lang?" });
  expect(within(theirs).queryByText("This is you")).toBeNull();
  expect(within(theirs).getByText(/^Bo Lang may not act while suspended/)).toBeTruthy();
  fireEvent.click(within(theirs).getByRole("button", { name: "Cancel" }));
  fireEvent.click(screen.getByRole("button", { name: "Issue password reset link" }));
  const theirLink = await screen.findByRole("dialog", {
    name: "Issue a password reset link for Bo Lang?",
  });
  expect(within(theirLink).getByText(/^The link lets Bo Lang choose/)).toBeTruthy();
});

// A SERVICE ACCOUNT IS A KEYED CREATE OF A MACHINE, offered neither grant a
// token never carries — it acts only through tokens, and the engine refuses
// either on a machine — and its token's mint is unkeyed, offers the account's
// grants alone and shows the value once. Mutation: drop the create dialog's
// withholding and people:manage is a box like any other.
test("a service account is created keyed and its token minted unkeyed from its grants", async () => {
  const eng = engine({
    "POST /iam/people": [json(201, { id: "p-new", outcome: "applied", op_id: "k" })],
    "POST /iam/credentials": [
      json(201, {
        id: "c-tok",
        person: "p-new",
        token: "cwl_pat_c-tok_1_secret",
        grants: ["work:write"],
        expires_at: "2027-01-04T09:00:00Z",
      }),
    ],
  });
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "New service account" }));
  const create = await screen.findByRole("dialog", { name: "New service account" });
  fireEvent.change(within(create).getByLabelText("Login"), { target: { value: "ci:deploy" } });
  for (const withheld of ["people:manage", "secrets:read"]) {
    expect(within(create).getByRole("checkbox", { name: withheld })).toHaveProperty(
      "disabled",
      true,
    );
  }
  expect(within(create).getAllByText(/acts only through tokens/)).toHaveLength(2);
  fireEvent.click(within(create).getByRole("checkbox", { name: "work:write" }));
  fireEvent.click(within(create).getByRole("button", { name: "Create" }));
  await settle();
  fireEvent.click(screen.getByRole("button", { name: "Mint its token" }));
  const mint = await screen.findByRole("dialog", { name: "Mint a token for ci:deploy" });
  // OUT OF THE ACCOUNT'S OWN GRANTS.
  expect(within(mint).queryByRole("checkbox", { name: "state:read" })).toBeNull();
  fireEvent.click(within(mint).getByRole("button", { name: "Mint" }));
  await settle();
  const [created, minted] = eng.writes();
  expect(created?.body).toEqual({
    kind: "machine",
    login: "ci:deploy",
    name: "",
    grants: ["work:write"],
  });
  expect(created?.key).toMatch(UUID7);
  expect(minted?.path).toBe("/iam/credentials");
  expect(minted?.query.get("person")).toBe("p-new");
  expect(minted?.key).toBeNull();
  expect(minted?.body).toEqual({ grants: ["work:write"] });
  expect(screen.getByText("cwl_pat_c-tok_1_secret")).toBeTruthy();
});

// A SERVICE ACCOUNT MAY ACT AS A SEAT FROM ITS FIRST RECORD: the seat goes in
// the create, never in a second write, which left a pipeline meant to act as
// a seat acting as itself until it landed. The CONTROL is the case above,
// which chose none and sent none. Mutation: drop the seat from the body.
test("a service account created on a seat sends it in the same record", async () => {
  const eng = engine({
    "POST /iam/people": [json(201, { id: "p-new", outcome: "applied", op_id: "k" })],
  });
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "New service account" }));
  const create = await screen.findByRole("dialog", { name: "New service account" });
  fireEvent.change(within(create).getByLabelText("Login"), { target: { value: "ci:deploy" } });
  await settle();
  pick(within(create).getByLabelText(/^Seat/), /Lee Ops/);
  fireEvent.click(within(create).getByRole("button", { name: "Create" }));
  await settle();
  expect(eng.writes().map((w) => w.body)).toEqual([
    { kind: "machine", login: "ci:deploy", name: "", seat: "lee", grants: [] },
  ]);
  expect(within(create).getByText(/acting as the seat Lee Ops/)).toBeTruthy();
});

// A LOGIN OUTSIDE ITS HOLDER'S GRAMMAR IS SAID UNDER THE FIELD, and nothing is
// posted: a service account named like a person, and a person renamed like a
// machine, each came back a 400 carrying the domain's own sentence. The
// CONTROLS are the cases above, whose logins fit and are posted.
test("a login outside its kind's grammar is refused under the field and not posted", async () => {
  const eng = engine({});
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "New service account" }));
  const create = await screen.findByRole("dialog", { name: "New service account" });
  fireEvent.change(within(create).getByLabelText("Login"), { target: { value: "deploybot" } });
  fireEvent.click(within(create).getByRole("button", { name: "Create" }));
  await settle();
  expect(within(create).getByText(/Use lowercase words joined by a colon/)).toBeTruthy();
  fireEvent.click(within(create).getByRole("button", { name: "Cancel" }));

  location.hash = "#/settings/access?person=p-bo";
  await settle();
  fireEvent.click(await screen.findByRole("button", { name: "Edit login, seat and grants" }));
  const edit = await screen.findByRole("dialog", { name: "Edit Bo Lang" });
  fireEvent.change(within(edit).getByLabelText("Login"), { target: { value: "ci:bo" } });
  fireEvent.click(within(edit).getByRole("button", { name: "Save" }));
  await settle();
  expect(within(edit).getByText(/Use lowercase words joined by dots/)).toBeTruthy();
  expect(eng.writes()).toEqual([]);
});

// FIRST RUN: nobody joined and nobody invited, and the screen says what to
// do next — a person comes from their seat, so it points at the org chart,
// and a company with no human seat at all at adding one first. The CONTROL is
// a claimed deployment. Mutation: offer an invitation here, or point a
// company with no human seat at a chart with nothing to invite anybody onto.
test("an unclaimed deployment's screen points at the seat a person comes from", async () => {
  invitations = { invitations: [], next: "" };
  engine();
  mount(ADMIN, "unclaimed");
  const callout = (await screen.findByText("Nobody has joined yet")).closest(
    ".crewlet-callout",
  ) as HTMLElement;
  expect(
    within(callout).getByText(/choose your human seat and invite or create yourself/),
  ).toBeTruthy();
  const chart = await within(callout).findByRole("link", { name: "Open the org chart" });
  expect(chart.getAttribute("href")).toBe("#/agents");
  expect(within(callout).queryByRole("button", { name: /^Invite/ })).toBeNull();
  cleanup();

  humanSeats = [];
  engine();
  mount(ADMIN, "unclaimed");
  const bare = (await screen.findByText("Nobody has joined yet")).closest(
    ".crewlet-callout",
  ) as HTMLElement;
  const add = await within(bare).findByRole("link", { name: "Add a human seat" });
  expect(add.getAttribute("href")).toBe("#/agents/edit?add=human");
  expect(within(bare).getByText(/no human seat for one/)).toBeTruthy();
  cleanup();

  humanSeats = SEATS;
  engine();
  mount(ADMIN, "ready");
  await screen.findByText("Bo Lang");
  expect(screen.queryByText("Nobody has joined yet")).toBeNull();
});

// A NODE RUNNING NO COMPANY answers the seat listing `409
// no_active_revision`, and the first step is creating the company: read as
// an empty answer, the callout sent the operator to "choose your human seat"
// on a chart with nothing behind it, and the Human seats card said the
// engine had failed. The CONTROL is the first case above, whose listing
// answered. Mutation: read only `seats.data` and the callout says "Open the
// org chart".
test("an unclaimed node running no company says to create the company first", async () => {
  invitations = { invitations: [], next: "" };
  seatsAnswer = json(409, {
    error: "no_active_revision",
    message: "This node has not been handed a company yet.",
    hint: "this node runs no company yet",
  });
  engine();
  mount(ADMIN, "unclaimed");
  const callout = (await screen.findByText("Nobody has joined yet")).closest(
    ".crewlet-callout",
  ) as HTMLElement;
  expect(within(callout).getByText(/This node runs no company yet/)).toBeTruthy();
  const create = within(callout).getByRole("link", { name: "Create the company" });
  expect(create.getAttribute("href")).toBe("#/agents/edit");
  expect(within(callout).queryByRole("link", { name: "Open the org chart" })).toBeNull();
  expect(within(callout).queryByRole("link", { name: "Add a human seat" })).toBeNull();
  // THE GRID SAYS IT TOO, as a sentence rather than a failure.
  expect(await screen.findByText(/it has no human seat to list/)).toBeTruthy();
  expect(screen.queryByText(/The engine tried to answer and failed/)).toBeNull();
});

// A SEAT LISTING STILL ON ITS WAY DECIDES NOTHING: drawn once the
// invitations alone had answered, the callout said "Open the org chart"
// while the seats were in flight and "Add a human seat" once they answered
// none. The CONTROL is the same screen with the listing answered, which draws
// the callout. Mutation: gate on the invitations alone and the callout is on
// screen before the seats arrive.
test("an unclaimed deployment's next step waits for the seat listing", async () => {
  invitations = { invitations: [], next: "" };
  seatsAnswer = "pending";
  engine();
  mount(ADMIN, "unclaimed");
  await screen.findByText("Bo Lang");
  await settle();
  expect(screen.queryByText("Nobody has joined yet")).toBeNull();
  cleanup();

  seatsAnswer = null;
  engine();
  mount(ADMIN, "unclaimed");
  expect(await screen.findByText("Nobody has joined yet")).toBeTruthy();
});

// AN OPEN INVITATION IS THE NEXT STEP, not another one: `unclaimed` stays
// until somebody redeems, and keyed on it alone the callout said "Nobody has
// been invited yet" and offered another invitation directly above the one
// just issued. Mutation: ignore the invitations and the callout points at the
// chart again.
test("an unclaimed deployment with an open invitation says to open its link", async () => {
  engine();
  mount(ADMIN, "unclaimed");
  const callout = (await screen.findByText("Nobody has joined yet")).closest(
    ".crewlet-callout",
  ) as HTMLElement;
  await within(callout).findByText(/The invitation to sam@example\.com is waiting to be redeemed/);
  expect(
    within(callout).getByText(/cancelled below or on its seat, and issued again from the seat/),
  ).toBeTruthy();
  expect(within(callout).queryByRole("link", { name: "Open the org chart" })).toBeNull();
  expect(screen.queryByText(/Nobody has been invited/)).toBeNull();
});
