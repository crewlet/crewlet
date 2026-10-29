/**
 * People writes the directory, for a reader holding Admin.
 *
 * The invariants, in the order they cost when they go: a reader without Admin
 * who reaches the screen carries no write control at all; every write sends the request its route
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
import { Suspense, type ComponentType } from "react";
import { afterEach, beforeAll, beforeEach, expect, test, vi } from "vitest";

import { ToastProvider } from "@crewlethq/ui";
import { People, ServiceAccounts } from "./Access.tsx";
import { PeekHost, PeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { loadChunk } from "~/app/lazyScreen.ts";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, setStepUpConfirmer, Store } from "~/protocol/index.ts";
import { healthFrame } from "~/test/health.ts";
import { CHART_ORG } from "~/test/orgchart.ts";
import { SessionReading } from "~/lib/frameSession.ts";
import { pick } from "~/testing.tsx";
import { ENGINE_HEADERS } from "~/test/engineAnswer.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

/**
 * An administrator, and so every app, though the row names Worklet alone
 * beside Admin: a stored row is read through `expand`, as the engine reads it.
 */
const ADMIN = {
  login: "ana.admin",
  owner: "ana.admin",
  grants: ["admin", "worklet"],
};
/**
 * Every app and no Admin. The section is closed to them (`nav.ts`), so the
 * frame draws its lock instead; mounted anyway — a viewer read that failed
 * holds no screen — the screen offers no control they could not use.
 */
const MEMBER = { login: "ana.diaz", owner: "jane", grants: ["worklet", "doclet"] };

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
      grants: [],
    },
    {
      id: "p-di",
      kind: "person",
      stage: "active",
      login: "di.moss",
      name: "Di Moss",
      seat: "kai",
      // STORED AS ADMIN ALONE, as a Tier A file or the CLI writes it.
      grants: ["admin"],
    },
    {
      id: "p-ci",
      kind: "machine",
      stage: "active",
      login: "ci:release",
      // billing is a name this build does not know — a permission a newer
      // engine might add: kept, granting nothing.
      grants: ["worklet", "admin", "billing"],
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
      grants: [],
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
    headers: { ...ENGINE_HEADERS, "Content-Type": "application/json" },
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
/** How many active people `GET /iam/check` counts holding Admin. */
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
    // ONE PERSON, as the peek reads them: the row the directory holds under
    // that id at this read, or the engine's 404.
    const one = /^\/iam\/people\/([^/]+)$/.exec(url.pathname);
    if (one) {
      const people = (directory() as { people: { id: string }[] }).people;
      const row = people.find((p) => p.id === one[1]);
      return Promise.resolve(row ? json(200, row) : json(404, { error: "not_found" }));
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
            people_with_admin: administrators,
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

/** One of the Access screens, People unless a case names Service accounts. */
function mount(
  viewer: Record<string, unknown> = ADMIN,
  identity = "ready",
  Screen: ComponentType = People,
) {
  const store = new Store();
  store.applyOrg(CHART_ORG);
  store.applyHealth(healthFrame({ identity }));
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
              {/* THE SCREEN AND THE FRAME'S ONE PEEK, as the shell mounts
                  them: an opened person's gestures are the peek's. */}
              <PeekNeighbours>
                <Screen />
                <Suspense fallback={null}>
                  <PeekHost />
                </Suspense>
              </PeekNeighbours>
            </SessionReading>
          </ToastProvider>
        </Router>
      </ViewerProvider>
    </ClientContext.Provider>,
  );
}

/** Every answer in flight lands. */
/** Open an opened principal's More menu and press one of its gestures. */
async function more(name: string) {
  fireEvent.click(await screen.findByRole("button", { name: /^More for / }));
  fireEvent.click(await screen.findByRole("menuitem", { name }));
}

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
    .map((cell) => cell.closest(".crewlet-data-grid__row") as HTMLElement | null)
    .find((r) => r && within(r).queryByRole("button", { name: /^Cancel the invitation/ }));
  if (!row) throw new Error("no invitation row carries a cancellation");
  return row;
}

let uninstall: (() => void) | null = null;

// THE PEEK'S BODY IS THE SETTINGS CHUNK'S, loaded once.
beforeAll(async () => {
  await loadChunk("settings");
});

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  location.hash = "#/settings/people";
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

// A READER WITHOUT ADMIN IS OFFERED NO CONTROL they could not use. The
// control is an administrator, who sees every one of them, "Invite person"
// first: it picks an open seat, since a person comes from a seat. Mutation:
// draw a control without Admin, and a line here goes red.
test("a reader without Admin sees no write control, and an administrator sees them", async () => {
  engine();
  location.hash = "#/settings/people?peek=person:p-bo";
  mount(MEMBER);
  await screen.findAllByText("Bo Lang");
  await screen.findAllByText("sam@example.com");
  // THE VIEWER HAS ANSWERED once the contact column says what reading it
  // takes — the company document is Admin's, and they are never asked it —
  // and the opened person's peek has drawn their credentials.
  await screen.findAllByText(
    "Reading where a seat is reached needs Admin access. An admin can give it to you.",
  );
  await screen.findByText("Authenticator app");
  for (const name of ["Invite person", "Cancel", "Revoke"]) {
    expect(screen.queryByRole("button", { name: new RegExp(`^${name}`) })).toBeNull();
  }
  expect(screen.queryByRole("button", { name: /^More for / })).toBeNull();
  cleanup();
  engine();
  mount(MEMBER, "ready", ServiceAccounts);
  await screen.findByText(/Changing any of this needs Admin access/);
  expect(screen.queryByRole("button", { name: "New service account" })).toBeNull();
  cleanup();
  engine();
  mount(ADMIN, "ready", ServiceAccounts);
  expect(await screen.findByRole("button", { name: "New service account" })).toBeTruthy();
  cleanup();
  engine();
  mount(ADMIN);
  await screen.findAllByText("Bo Lang");
  await screen.findAllByText("sam@example.com");
  await screen.findAllByRole("button", { name: "Invite person" });
  for (const name of ["Invite person", "Cancel", "Revoke"]) {
    expect(screen.getAllByRole("button", { name: new RegExp(`^${name}`) }).length).toBeGreaterThan(
      0,
    );
  }
  expect(screen.getByRole("button", { name: /^More for / })).toBeTruthy();
  // A SEAT'S OWN GESTURES stay on the seat: this page invites through a picker.
  for (const name of ["Invite", "Add person"]) {
    expect(screen.queryByRole("button", { name })).toBeNull();
  }
});

// INVITE PERSON PICKS AN OPEN SEAT FIRST, then opens that seat's own
// invitation: the seats offered are the ones nothing holds, and the invitation
// names the seat chosen. Mutation: skip the picker, or offer a held seat, and
// a line here goes red.
test("invite person picks an open seat, then invites onto it", async () => {
  const eng = engine({
    "POST /iam/invitations": [
      json(201, {
        id: "inv-9",
        seat: "lee",
        url: "https://crewlet.example.com/dashboard#/invite/inv-9.s3cret",
        expires_at: "2026-10-20T09:00:00Z",
        outcome: "applied",
      }),
    ],
  });
  mount(ADMIN);
  fireEvent.click(await screen.findByRole("button", { name: "Invite person" }));
  const picker = await screen.findByRole("dialog", { name: "Invite person" });
  await settle();
  pick(within(picker).getByLabelText(/^Seat/), /Lee Ops/);
  fireEvent.click(within(picker).getByRole("button", { name: "Continue" }));
  const invite = await screen.findByRole("dialog", { name: "Invite someone as Lee Ops" });
  fireEvent.change(within(invite).getByLabelText("Email address"), {
    target: { value: "new@example.com" },
  });
  fireEvent.click(within(invite).getByRole("button", { name: "Invite" }));
  await settle();
  expect(eng.writes().map((w) => [w.path, (w.body as { seat?: string }).seat])).toEqual([
    ["/iam/invitations", "lee"],
  ]);
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
  expect(within(cancel).getByRole("status").textContent).toMatch(
    /^Saved\. The list will update in a moment\./,
  );
  expect(within(cancel).queryByRole("button", { name: "Cancel invitation" })).toBeNull();
  expect(within(cancel).queryByRole("button", { name: "Keep it" })).toBeNull();
  fireEvent.click(within(cancel).getByRole("button", { name: "Done" }));
  expect(screen.queryByRole("dialog", { name: "Cancel this invitation?" })).toBeNull();

  location.hash = "#/settings/people?peek=person:p-bo";
  fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
  const edit = await screen.findByRole("dialog", { name: "Edit Bo Lang" });
  fireEvent.click(within(edit).getByRole("checkbox", { name: "Worklet" }));
  fireEvent.click(within(edit).getByRole("button", { name: "Save" }));
  await settle();
  expect(within(edit).getByRole("status").textContent).toMatch(
    /^Saved\. The list will update in a moment\./,
  );
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

// EACH GESTURE ON A PERSON SAYS WHAT THEY WILL NOTICE, in a sentence or two:
// the dialog names the person and the one consequence that matters. A
// suspended person keeps their seat; a removal opens it up. Mutation: drop a
// sentence and its assertion goes red.
test("a seated person's suspension and removal each say what happens to them", async () => {
  engine();
  location.hash = "#/settings/people?peek=person:p-bo";
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Suspend" }));
  const suspend = await screen.findByRole("dialog", { name: "Suspend Bo Lang?" });
  expect(within(suspend).getByText("They can’t sign in until reactivated.")).toBeTruthy();
  fireEvent.click(within(suspend).getByRole("button", { name: "Cancel" }));
  await more("Remove");
  const remove = await screen.findByRole("dialog", { name: "Remove Bo Lang?" });
  expect(remove.textContent).toMatch(
    /Their account is deleted and their seat opens up\. Can’t be undone\./,
  );
});

// A SERVICE ACCOUNT BOUND TO NO SEAT is told nothing about one, and its
// removal is worded as a service account's. The CONTROL is the seated person
// above. Mutation: name a seat whatever the row holds, or word a machine's
// removal as a person's.
test("a service account bound to no seat is told nothing about one", async () => {
  engine();
  location.hash = "#/settings/people?peek=person:p-ci";
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Suspend" }));
  const suspend = await screen.findByRole("dialog", { name: "Suspend ci:release?" });
  expect(within(suspend).queryByText(/seat/)).toBeNull();
  fireEvent.click(within(suspend).getByRole("button", { name: "Cancel" }));
  await more("Remove");
  const remove = await screen.findByRole("dialog", { name: "Remove ci:release?" });
  expect(within(remove).queryByText(/seat/)).toBeNull();
  expect(remove.textContent).toMatch(/Deletes the service account and its tokens/);
  expect(remove.textContent).not.toMatch(/invite/);
});

// THE MORE MENU'S GESTURES each name what stops working, in the words the
// person's own Account uses: two-step verification, signing out everywhere,
// a reset link that works once. Mutation: drop a sentence and its assertion
// goes red.
test("every gesture under More says what stops working", async () => {
  engine();
  location.hash = "#/settings/people?peek=person:p-bo";
  mount();
  await more("Reset two-step verification");
  const factor = await screen.findByRole("dialog", {
    name: "Reset Bo Lang’s two-step verification?",
  });
  expect(within(factor).getByText("Their app and recovery codes stop working.")).toBeTruthy();
  fireEvent.click(within(factor).getByRole("button", { name: "Cancel" }));
  await more("Sign out everywhere");
  const sessions = await screen.findByRole("dialog", { name: "Sign Bo Lang out everywhere?" });
  expect(
    within(sessions).getByText("Their sessions, tokens and reset link stop working."),
  ).toBeTruthy();
  fireEvent.click(within(sessions).getByRole("button", { name: "Cancel" }));
  await more("Get reset link");
  const link = await screen.findByRole("dialog", { name: "Get a reset link for Bo Lang?" });
  expect(within(link).getByText("Works once, for a day.")).toBeTruthy();
  cleanup();

  engine({}, () => ({
    ...PEOPLE,
    people: PEOPLE.people.map((p) => (p.id === "p-bo" ? { ...p, stage: "suspended" } : p)),
  }));
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Reactivate" }));
  const reactivate = await screen.findByRole("dialog", { name: "Reactivate Bo Lang?" });
  expect(within(reactivate).getByText("Bo Lang can sign in again.")).toBeTruthy();
});

// AN EDIT SENDS ONLY WHAT CHANGED, and a suspension is the stage alone.
test("an opened person's edit sends what changed, and suspending sends the stage", async () => {
  const eng = engine({
    "PATCH /iam/people/p-bo": [json(200, { id: "p-bo", outcome: "applied", op_id: "k" })],
  });
  location.hash = "#/settings/people?peek=person:p-bo";
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
  const edit = await screen.findByRole("dialog", { name: "Edit Bo Lang" });
  fireEvent.click(within(edit).getByRole("checkbox", { name: "Worklet" }));
  fireEvent.click(within(edit).getByRole("button", { name: "Save" }));
  await settle();
  fireEvent.click(screen.getByRole("button", { name: "Suspend" }));
  const suspend = await screen.findByRole("dialog", { name: "Suspend Bo Lang?" });
  fireEvent.click(within(suspend).getByRole("button", { name: "Suspend" }));
  await settle();
  expect(eng.writes().map((w) => w.body)).toEqual([
    { add_grants: ["worklet"] },
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
        message:
          "You don’t have the access this needs (Admin, Worklet or Doclet). Ask an admin for it.",
        id: "p-bo",
        landed: ["identity"],
      }),
    ],
  });
  location.hash = "#/settings/people?peek=person:p-bo";
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
  const edit = await screen.findByRole("dialog", { name: "Edit Bo Lang" });
  fireEvent.change(within(edit).getByLabelText("Login"), { target: { value: "bo.lange" } });
  fireEvent.click(within(edit).getByRole("checkbox", { name: "Worklet" }));
  const before = eng.reads("/iam/people");
  fireEvent.click(within(edit).getByRole("button", { name: "Save" }));
  await settle();
  expect(within(edit).getByRole("alert").textContent).toMatch(
    /What did change: the login and seat\./,
  );
  expect(eng.reads("/iam/people")).toBeGreaterThan(before);
});

// AN EDIT SENDS WHAT ITS EDITOR CHANGED, measured against the row it opened
// on: while the dialog is open another administrator gives Bo Worklet and
// binds him to a seat, the directory is read again as the tab comes back,
// and a login change saved after it sends the login alone — never the grant
// and the seat the dialog opened with, which would undo both. Mutation:
// measure against the live row and the save sends `grants` and `seat` too.
test("an edit sends what its editor changed, whatever the directory read since", async () => {
  let people: unknown = PEOPLE;
  const eng = engine(
    { "PATCH /iam/people/p-bo": [json(200, { id: "p-bo", outcome: "applied", op_id: "k" })] },
    () => people,
  );
  location.hash = "#/settings/people?peek=person:p-bo";
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
  const edit = await screen.findByRole("dialog", { name: "Edit Bo Lang" });
  people = {
    ...PEOPLE,
    people: PEOPLE.people.map((p) =>
      p.id === "p-bo" ? { ...p, grants: ["worklet"], seat: "sam" } : p,
    ),
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
  location.hash = "#/settings/people?peek=person:p-bo";
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
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
  location.hash = "#/settings/people?peek=person:p-bo";
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
  const edit = await screen.findByRole("dialog", { name: "Edit Bo Lang" });
  await settle();
  expect(within(edit).getByText("Pick an open seat.")).toBeTruthy();
  fireEvent.click(within(edit).getByLabelText("Seat"));
  expect(screen.queryByRole("option", { name: /No seat/ })).toBeNull();
  fireEvent.mouseDown(screen.getByRole("option", { name: /Lee Ops/ }));
  fireEvent.click(within(edit).getByRole("button", { name: "Save" }));
  await settle();
  expect(eng.writes().map((w) => w.body)).toEqual([{ seat: "lee" }]);
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
  location.hash = "#/settings/people?peek=person:p-ci";
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
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
// another administrator gives Bo Admin, the directory is read again as the
// tab comes back, and ticking Worklet sends that tick alone, never the
// whole list, which would take Admin away again, and which the engine would
// accept. Mutation: send `grants` whole and the save carries Worklet without
// Admin.
test("a grant change sends the ticks alone, whatever another administrator changed", async () => {
  let people: unknown = PEOPLE;
  const eng = engine(
    { "PATCH /iam/people/p-bo": [json(200, { id: "p-bo", outcome: "applied", op_id: "k" })] },
    () => people,
  );
  location.hash = "#/settings/people?peek=person:p-bo";
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
  const edit = await screen.findByRole("dialog", { name: "Edit Bo Lang" });
  people = {
    ...PEOPLE,
    people: PEOPLE.people.map((p) => (p.id === "p-bo" ? { ...p, grants: ["admin"] } : p)),
  };
  const before = eng.reads("/iam/people");
  act(() => {
    document.dispatchEvent(new Event("visibilitychange"));
  });
  await settle();
  expect(eng.reads("/iam/people")).toBeGreaterThan(before);
  fireEvent.click(within(edit).getByRole("checkbox", { name: "Worklet" }));
  fireEvent.click(within(edit).getByRole("button", { name: "Save" }));
  await settle();
  expect(eng.writes().map((w) => w.body)).toEqual([{ add_grants: ["worklet"] }]);
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
  location.hash = "#/settings/people?peek=person:p-bo";
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
  expect(within(dialog).getByText(/Try again\. It won’t happen twice\./)).toBeTruthy();
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

// UNTICKING ADMIN KEEPS THE APPS IT INCLUDED: Di's row stores Admin alone,
// which is every app, so the edit opens with Worklet and Doclet ticked and
// locked ("Included with Admin"); unticking Admin unlocks them still ticked,
// and the save adds them as it takes Admin away — leaving Di their apps rather
// than the overview alone. Mutation: let Admin go without reading the row
// through `expand` and the save takes Admin away and adds nothing.
test("unticking Admin keeps the apps it included", async () => {
  const eng = engine({
    "PATCH /iam/people/p-di": [json(200, { id: "p-di", outcome: "applied", op_id: "k" })],
  });
  location.hash = "#/settings/people?peek=person:p-di";
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
  const edit = await screen.findByRole("dialog", { name: "Edit Di Moss" });
  const admin = within(edit).getByRole("checkbox", { name: "Admin" });
  const tracker = within(edit).getByRole("checkbox", { name: "Worklet" });
  expect(admin).toHaveProperty("checked", true);
  expect(admin).toHaveProperty("disabled", false);
  expect(tracker).toHaveProperty("checked", true);
  expect(tracker).toHaveProperty("disabled", true);
  expect(within(edit).getAllByText("Included with Admin.")).toHaveLength(3);
  fireEvent.click(admin);
  expect(within(edit).getByRole("checkbox", { name: "Worklet" })).toHaveProperty("checked", true);
  expect(within(edit).getByRole("checkbox", { name: "Worklet" })).toHaveProperty("disabled", false);
  fireEvent.click(within(edit).getByRole("button", { name: "Save" }));
  await settle();
  expect(eng.writes().map((w) => w.body)).toEqual([
    { add_grants: ["worklet", "doclet", "artifacts"], remove_grants: ["admin"] },
  ]);
});

// A NAME THIS BUILD DOES NOT KNOW IS KEPT UNTIL SOMEBODY UNTICKS IT: the
// engine keeps it and grants nothing for it, so the edit offers it as a box
// of its own, ticked, and unticking it sends its removal and nothing else —
// while a service account is told nothing about reaching work, which is a
// person's concern. Mutation: offer only the names this build knows and the
// box is gone, so nobody can remove it.
test("a machine's edit keeps a name this build does not know until it is unticked", async () => {
  const eng = engine({
    "PATCH /iam/people/p-ci": [json(200, { id: "p-ci", outcome: "applied", op_id: "k" })],
  });
  location.hash = "#/settings/people?peek=person:p-ci";
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
  const edit = await screen.findByRole("dialog", { name: "Edit ci:release" });
  const unknown = within(edit).getByRole("checkbox", { name: "billing" });
  expect(unknown).toHaveProperty("checked", true);
  expect(unknown).toHaveProperty("disabled", false);
  expect(within(edit).queryByText(/agents can’t send/)).toBeNull();
  fireEvent.click(unknown);
  fireEvent.click(within(edit).getByRole("button", { name: "Save" }));
  await settle();
  expect(eng.writes().map((w) => w.body)).toEqual([{ remove_grants: ["billing"] }]);
});

// A SERVICE ACCOUNT'S TOKEN STARTS FROM ITS OWN ACCESS — Admin and so every
// app — and never from a name this build does not know, which it could not
// say what a token would carry for. Mutation: seed the selection from the
// account's stored grants and the mint carries billing, a box nobody is
// offered.
test("a service account's mint starts from its own access, and never a name this build does not know", async () => {
  const eng = engine({
    "POST /iam/credentials": [
      json(201, { id: "c-tok", person: "p-ci", token: "cwl_pat_c-tok_1_s", grants: [] }),
    ],
  });
  location.hash = "#/settings/people?peek=person:p-ci";
  mount();
  await more("Create token");
  const mint = await screen.findByRole("dialog", { name: "New token for ci:release" });
  expect(within(mint).queryByRole("checkbox", { name: "billing" })).toBeNull();
  expect(within(mint).getByRole("checkbox", { name: "Admin" })).toHaveProperty("checked", true);
  fireEvent.click(within(mint).getByRole("button", { name: "Create token" }));
  await settle();
  expect(eng.writes().map((w) => w.body)).toEqual([
    { grants: ["worklet", "admin", "doclet", "artifacts"] },
  ]);
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
  location.hash = "#/settings/people?peek=person:p-bo";
  mount();
  await more("Get reset link");
  const issue = await screen.findByRole("dialog", { name: /Get a reset link/ });
  fireEvent.click(within(issue).getByRole("button", { name: "Get link" }));
  await settle();
  expect(screen.getByText("https://crewlet.example.com/dashboard#/reset/c-reset.s")).toBeTruthy();
  // DONE ALONE: the link exists, and a Cancel read as a way to take it back.
  const shown = screen.getByRole("dialog", { name: /Reset link for/ });
  expect(within(shown).queryByRole("button", { name: "Cancel" })).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Done" }));

  fireEvent.click(await screen.findByRole("button", { name: "Revoke Authenticator app" }));
  const revoke = await screen.findByRole("dialog", { name: /Revoke this authenticator app/ });
  fireEvent.click(within(revoke).getByRole("button", { name: "Revoke" }));
  await settle();

  // LAST, because a removal closes the panel it was made from.
  await more("Remove");
  const remove = await screen.findByRole("dialog", { name: "Remove Bo Lang?" });
  const confirm = within(remove).getByRole("button", { name: "Remove" });
  expect(confirm).toHaveProperty("disabled", true);
  // THE LOGIN IS SHOWN AS IT IS TYPED. The field's label is set in capitals,
  // and read as BO.LANG, typing that was refused; so the login is the label's
  // inline code, which the kit keeps in its own case inside a label.
  const login = within(remove).getByText("bo.lang");
  expect(login.tagName).toBe("CODE");
  expect(login.closest(".crewlet-label")).not.toBeNull();
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

// A REMOVAL CLOSES THE PEEK IT WAS MADE FROM, and says what it did: left
// open, the row read again answered 404, so the peek turned into the note for
// a link naming nobody — "Nobody in the directory has this id", a raw id
// beside it — right after the reader's own action. The CONTROL is that note
// itself, which a link to somebody since removed still draws
// (`Access.test.tsx`). Mutation: drop the peek's removal callback and the
// note, the id and the `peek=` parameter all stay.
test("removing the open person closes their panel and says they were removed", async () => {
  let removed = false;
  engine(
    { "DELETE /iam/people/p-bo": [json(200, { id: "p-bo", outcome: "applied", op_id: "k" })] },
    () => (removed ? { people: PEOPLE.people.filter((p) => p.id !== "p-bo"), next: "" } : PEOPLE),
  );
  location.hash = "#/settings/people?peek=person:p-bo";
  mount();
  await more("Remove");
  const remove = await screen.findByRole("dialog", { name: "Remove Bo Lang?" });
  fireEvent.change(within(remove).getByLabelText("Type bo.lang to confirm"), {
    target: { value: "bo.lang" },
  });
  removed = true;
  fireEvent.click(within(remove).getByRole("button", { name: "Remove" }));
  await settle();
  expect((await screen.findAllByText("Bo Lang was removed")).length).toBeGreaterThan(0);
  expect(location.hash).toBe("#/settings/people");
  expect(screen.queryByText("Nobody in the directory has this id")).toBeNull();
  expect(screen.queryByText("p-bo")).toBeNull();
  expect(screen.queryByRole("dialog", { name: "Remove Bo Lang?" })).toBeNull();
});

// TAKING YOUR OWN Admin IN AN EDIT SAYS IT IS YOU, and when nobody
// else administers, as a suspension and a removal do: the only administrator
// could untick it on their own row with no word, and the engine has no
// last-administrator guard — afterwards only a Tier A token administers
// people. The dialog speaks to them throughout. The CONTROLS: the same edit
// keeping the grant says nothing, and with a second administrator the
// sentence about nobody else goes. Mutation: drop `you` or `alone` from the
// edit and a line here goes red.
test("taking your own Admin in an edit says it is you, and when nobody else administers", async () => {
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
    location.hash = "#/settings/people?peek=person:p-ana";
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
    const edit = await screen.findByRole("dialog", { name: "Edit Ana Admin" });
    await settle();
    expect(within(edit).queryByText("This is you")).toBeNull();
    expect(within(edit).getByText("Lowercase words with dots, like jane.doe.")).toBeTruthy();
    fireEvent.click(within(edit).getByRole("checkbox", { name: "Admin" }));
    expect(within(edit).getByText("This is you")).toBeTruthy();
    expect(within(edit).getByText(/You’ll lose admin access when you save/)).toBeTruthy();
    expect(within(edit).queryByText(/You’re the only admin/) !== null).toBe(nobodyElse);
    // AND WHAT THEY KEEP IS SAID TO THEM: letting Worklet go too leaves them
    // no way to receive what agents send people.
    fireEvent.click(within(edit).getByRole("checkbox", { name: "Worklet" }));
    expect(within(edit).getByText(/agents can’t send you work/)).toBeTruthy();
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
  location.hash = "#/settings/people?peek=person:p-ana";
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Suspend" }));
  const suspend = await screen.findByRole("dialog", { name: "Suspend yourself?" });
  expect(within(suspend).getByText("This is you")).toBeTruthy();
  expect(await within(suspend).findByText(/You’re the only admin/)).toBeTruthy();
  // THE WHOLE DIALOG SPEAKS TO THEM, its body included: a third-person body
  // under "This is you" read as two people.
  expect(
    within(suspend).getByText("You can’t sign in until another admin reactivates you."),
  ).toBeTruthy();
  expect(within(suspend).queryByText(/Ana Admin/)).toBeNull();
  fireEvent.click(within(suspend).getByRole("button", { name: "Cancel" }));
  await more("Remove");
  const remove = await screen.findByRole("dialog", { name: "Remove yourself?" });
  expect(within(remove).getByText(/You’re the only admin/)).toBeTruthy();
  expect(within(remove).getByText("Can’t be undone.")).toBeTruthy();
  fireEvent.click(within(remove).getByRole("button", { name: "Cancel" }));
  await more("Sign out everywhere");
  const sessions = await screen.findByRole("dialog", { name: "Sign out everywhere?" });
  expect(
    within(sessions).getByText("Your sessions, tokens and reset link stop working."),
  ).toBeTruthy();
  fireEvent.click(within(sessions).getByRole("button", { name: "Cancel" }));
  await more("Reset two-step verification");
  const factor = await screen.findByRole("dialog", {
    name: "Reset your two-step verification?",
  });
  expect(within(factor).getByText("Your app and recovery codes stop working.")).toBeTruthy();
  fireEvent.click(within(factor).getByRole("button", { name: "Cancel" }));
  // AND A RESET LINK, which said "for Ana Admin … for them" to Ana herself,
  // and says where a password she knows is changed with no link at all.
  await more("Get reset link");
  const link = await screen.findByRole("dialog", { name: "Get a reset link for yourself?" });
  expect(
    within(link).getByText(
      "Works once, within a day. To change a known password, use Account › Security.",
    ),
  ).toBeTruthy();
  expect(within(link).queryByText(/Ana Admin/)).toBeNull();
  cleanup();

  administrators = 2;
  engine({}, directory);
  mount();
  await more("Remove");
  const another = await screen.findByRole("dialog", { name: "Remove yourself?" });
  await settle();
  expect(within(another).getByText("This is you")).toBeTruthy();
  expect(within(another).queryByText(/You’re the only admin/)).toBeNull();
  cleanup();

  location.hash = "#/settings/people?peek=person:p-bo";
  engine({}, directory);
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Suspend" }));
  const theirs = await screen.findByRole("dialog", { name: "Suspend Bo Lang?" });
  expect(within(theirs).queryByText("This is you")).toBeNull();
  expect(within(theirs).getByText("They can’t sign in until reactivated.")).toBeTruthy();
  fireEvent.click(within(theirs).getByRole("button", { name: "Cancel" }));
  await more("Get reset link");
  const theirLink = await screen.findByRole("dialog", { name: "Get a reset link for Bo Lang?" });
  expect(within(theirLink).getByText("Works once, for a day.")).toBeTruthy();
});

// A SERVICE ACCOUNT IS A KEYED CREATE OF A MACHINE whose access STARTS EMPTY
// — what a script may do is decided on purpose, never defaulted — and its
// token's mint is unkeyed, offers the account's access alone and shows the
// value once. Mutation: pre-tick the apps as a person's invitation does and
// the create carries Doclet nobody chose.
test("a service account is created keyed and its token minted unkeyed from its grants", async () => {
  const eng = engine({
    "POST /iam/people": [json(201, { id: "p-new", outcome: "applied", op_id: "k" })],
    "POST /iam/credentials": [
      json(201, {
        id: "c-tok",
        person: "p-new",
        token: "cwl_pat_c-tok_1_secret",
        grants: ["worklet"],
        expires_at: "2027-01-04T09:00:00Z",
      }),
    ],
  });
  mount(ADMIN, "ready", ServiceAccounts);
  fireEvent.click(await screen.findByRole("button", { name: "New service account" }));
  const create = await screen.findByRole("dialog", { name: "New service account" });
  fireEvent.change(within(create).getByLabelText("Login"), { target: { value: "ci:deploy" } });
  for (const name of ["Admin", "Worklet", "Doclet"]) {
    expect(within(create).getByRole("checkbox", { name })).toHaveProperty("checked", false);
  }
  fireEvent.click(within(create).getByRole("checkbox", { name: "Worklet" }));
  fireEvent.click(within(create).getByRole("button", { name: "Create" }));
  await settle();
  fireEvent.click(screen.getByRole("button", { name: "Create token" }));
  const mint = await screen.findByRole("dialog", { name: "New token for ci:deploy" });
  // OUT OF THE ACCOUNT'S OWN ACCESS: what it does not hold is not offered.
  expect(within(mint).queryByRole("checkbox", { name: "Doclet" })).toBeNull();
  expect(within(mint).getByRole("checkbox", { name: "Worklet" })).toHaveProperty("checked", true);
  fireEvent.click(within(mint).getByRole("button", { name: "Create token" }));
  await settle();
  const [created, minted] = eng.writes();
  expect(created?.body).toEqual({
    kind: "machine",
    login: "ci:deploy",
    name: "",
    grants: ["worklet"],
  });
  expect(created?.key).toMatch(UUID7);
  expect(minted?.path).toBe("/iam/credentials");
  expect(minted?.query.get("person")).toBe("p-new");
  expect(minted?.key).toBeNull();
  expect(minted?.body).toEqual({ grants: ["worklet"] });
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
  mount(ADMIN, "ready", ServiceAccounts);
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
  mount(ADMIN, "ready", ServiceAccounts);
  fireEvent.click(await screen.findByRole("button", { name: "New service account" }));
  const create = await screen.findByRole("dialog", { name: "New service account" });
  fireEvent.change(within(create).getByLabelText("Login"), { target: { value: "deploybot" } });
  fireEvent.click(within(create).getByRole("button", { name: "Create" }));
  await settle();
  expect(
    within(create).getByText("Use lowercase words with colons, like ci:release."),
  ).toBeTruthy();
  fireEvent.click(within(create).getByRole("button", { name: "Cancel" }));

  location.hash = "#/settings/people?peek=person:p-bo";
  await settle();
  fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
  const edit = await screen.findByRole("dialog", { name: "Edit Bo Lang" });
  fireEvent.change(within(edit).getByLabelText("Login"), { target: { value: "ci:bo" } });
  fireEvent.click(within(edit).getByRole("button", { name: "Save" }));
  await settle();
  expect(within(edit).getByText("Use lowercase words with dots, like jane.doe.")).toBeTruthy();
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
    within(callout).getByText("Invite yourself from your seat in the org chart."),
  ).toBeTruthy();
  const chart = await within(callout).findByRole("link", { name: "Open the org chart" });
  expect(chart.getAttribute("href")).toBe("#/company/org");
  expect(within(callout).queryByRole("button", { name: /^Invite/ })).toBeNull();
  cleanup();

  humanSeats = [];
  engine();
  mount(ADMIN, "unclaimed");
  const bare = (await screen.findByText("Nobody has joined yet")).closest(
    ".crewlet-callout",
  ) as HTMLElement;
  const add = await within(bare).findByRole("link", { name: "Add a human seat" });
  expect(add.getAttribute("href")).toBe("#/company/org/edit?add=human");
  expect(within(bare).getByText("No people in the chart yet. Add one in Edit org.")).toBeTruthy();
  cleanup();

  humanSeats = SEATS;
  engine();
  mount(ADMIN, "ready");
  await screen.findByText("Bo Lang");
  expect(screen.queryByText("Nobody has joined yet")).toBeNull();
});

// AN EMPTY DIRECTORY SAYS THE STEP ONCE, in the People card's own empty
// state: a callout over two stat tiles of zero over the card's "Nobody's here
// yet" said one thing four times before any data. The CONTROL is the case
// above, whose directory lists somebody and keeps the callout. Mutation: draw
// the callout whatever the directory holds and the first query finds two.
test("an unclaimed deployment with nobody listed says the step once, in the People card", async () => {
  invitations = { invitations: [], next: "" };
  engine({}, () => ({ people: [], next: "" }));
  mount(ADMIN, "unclaimed");
  const title = await screen.findByText("Nobody has joined yet");
  expect(screen.getAllByText("Nobody has joined yet")).toHaveLength(1);
  expect(title.closest(".crewlet-callout")).toBeNull();
  const state = title.closest(".crewlet-empty-state") as HTMLElement;
  expect(within(state).getByText("Invite yourself from your seat in the org chart.")).toBeTruthy();
  expect(within(state).getByRole("link", { name: "Open the org chart" })).toBeTruthy();
  expect(document.querySelector(".crewlet-stat-group")).toBeNull();
  expect(screen.queryByText("Nobody’s here yet")).toBeNull();
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
  expect(within(callout).getByText("No company yet. Create one in Edit org.")).toBeTruthy();
  const create = within(callout).getByRole("link", { name: "Create the company" });
  expect(create.getAttribute("href")).toBe("#/company/org/edit");
  expect(within(callout).queryByRole("link", { name: "Open the org chart" })).toBeNull();
  expect(within(callout).queryByRole("link", { name: "Add a human seat" })).toBeNull();
  // THE GRID SAYS IT TOO, as a sentence rather than a failure.
  expect(await screen.findByText(/it has no human seat to list/)).toBeTruthy();
  expect(screen.queryByText(/The engine failed to answer/)).toBeNull();
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
  await within(callout).findByText("Lost the invitation link? Cancel it and invite again.");
  expect(within(callout).queryByRole("link", { name: "Open the org chart" })).toBeNull();
  expect(screen.queryByText(/Nobody has been invited/)).toBeNull();
});
