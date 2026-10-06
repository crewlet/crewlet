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
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { PeopleAndAccess } from "./Access.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, setStepUpConfirmer, Store } from "~/protocol/index.ts";
import { CHART_ORG } from "~/test/orgchart.ts";

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
      grants: ["state:read"],
    },
    {
      id: "p-di",
      kind: "person",
      stage: "active",
      login: "di.moss",
      name: "Di Moss",
      // secrets:write is a grant ADMIN does not hold.
      grants: ["state:read", "secrets:write"],
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
      case "/iam/people":
        return Promise.resolve(json(200, directory()));
      case "/iam/invitations":
        return Promise.resolve(json(200, INVITATIONS));
      case "/iam/seats":
        return Promise.resolve(
          json(200, {
            seats: url.searchParams.get("unheld")
              ? [{ handle: "sam", name: "Sam Support" }]
              : [
                  { handle: "sam", name: "Sam Support" },
                  {
                    handle: "jane",
                    name: "Jane Founder",
                    holder: { person: "p-ana", login: "ana.admin", stage: "active" },
                  },
                ],
          }),
        );
      case "/iam/node-tokens":
        return Promise.resolve(json(200, { tokens: [] }));
      case "/iam/check":
        return Promise.resolve(
          json(200, { findings: [], people_with_people_manage: 1, bindings_unchecked: 0 }),
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
          <PeopleAndAccess />
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

let uninstall: (() => void) | null = null;

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  location.hash = "#/settings/access";
  secondFactor = "required";
});

afterEach(() => {
  cleanup();
  uninstall?.();
  uninstall = null;
  location.hash = "";
  vi.restoreAllMocks();
});

// AN AUDITOR'S READ VIEW IS AS IT WAS: no control they could not use. The
// control is an administrator, who sees every one of them.
test("an auditor sees no write control, and an administrator sees them", async () => {
  engine();
  location.hash = "#/settings/access?person=p-bo";
  mount(AUDITOR);
  await screen.findByText("Bo Lang");
  await screen.findByText("sam@example.com");
  // THE VIEWER HAS ANSWERED once the contact column reads the company
  // document, which only a reader holding config:read is asked for.
  await screen.findAllByText("No surface");
  for (const name of ["Invite person", "New service account", "Cancel", "Remove", "Revoke"]) {
    expect(screen.queryByRole("button", { name: new RegExp(`^${name}`) })).toBeNull();
  }
  cleanup();
  engine();
  mount(ADMIN);
  await screen.findByText("Bo Lang");
  await screen.findByText("sam@example.com");
  await screen.findAllByRole("button", { name: "Invite person" });
  for (const name of ["Invite person", "New service account", "Cancel", "Remove", "Revoke"]) {
    expect(screen.getAllByRole("button", { name: new RegExp(`^${name}`) }).length).toBeGreaterThan(
      0,
    );
  }
});

// AN INVITATION IS A KEYED CREATE: a uuid7 key, the body the route takes, and
// the link shown once with when it expires — then the lists read again.
test("an invitation is sent keyed and its link is shown once", async () => {
  const eng = engine({
    "POST /iam/invitations": [
      json(201, {
        id: "inv-2",
        url: "https://crewlet.example.com/dashboard#/invite/inv-2.s3cret",
        expires_at: "2026-10-13T09:00:00Z",
        outcome: "applied",
        op_id: "k",
        position: "CREWLET_IAM_LOG@0:7",
      }),
    ],
  });
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Invite person" }));
  const dialog = await screen.findByRole("dialog", { name: "Invite a person" });
  // A GRANT THE VIEWER DOES NOT HOLD is refused before it is sent.
  expect(within(dialog).getByRole("checkbox", { name: "secrets:read" })).toHaveProperty(
    "disabled",
    true,
  );
  fireEvent.change(within(dialog).getByLabelText("Email address"), {
    target: { value: "dee@example.com" },
  });
  fireEvent.click(within(dialog).getByRole("checkbox", { name: "work:write" }));
  const before = eng.reads("/iam/invitations");
  fireEvent.click(within(dialog).getByRole("button", { name: "Invite" }));
  await settle();
  const [sent] = eng.writes();
  expect(sent?.path).toBe("/iam/invitations");
  expect(sent?.key).toMatch(UUID7);
  expect(sent?.body).toEqual({ email: "dee@example.com", grants: ["work:write"] });
  expect(
    within(dialog).getByText("https://crewlet.example.com/dashboard#/invite/inv-2.s3cret"),
  ).toBeTruthy();
  expect(within(dialog).getByText(/works once/)).toBeTruthy();
  expect(eng.reads("/iam/invitations")).toBeGreaterThan(before);
});

// AN UNKNOWN ANSWER IS RETRIED UNDER THE SAME KEY — the one the engine handed
// back — so the retry is the first attempt's write. Mutation: mint a key per
// press and the second request carries another.
test("an unknown invitation is retried under the key the engine handed back", async () => {
  const eng = engine({
    "POST /iam/invitations": [
      json(503, {
        error: "unavailable",
        outcome: "unknown",
        op_id: "0192f4c8-0000-7000-8000-000000000001",
        message: "This node cannot answer that right now.",
      }),
      json(201, { id: "inv-2", url: "https://x/dashboard#/invite/inv-2.s", expires_at: "" }),
    ],
  });
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Invite person" }));
  const dialog = await screen.findByRole("dialog", { name: "Invite a person" });
  fireEvent.change(within(dialog).getByLabelText("Email address"), {
    target: { value: "dee@example.com" },
  });
  fireEvent.click(within(dialog).getByRole("button", { name: "Invite" }));
  await settle();
  expect(within(dialog).getByText(/could not confirm whether this landed/)).toBeTruthy();
  fireEvent.click(within(dialog).getByRole("button", { name: "Try again" }));
  await settle();
  const keys = eng.writes().map((w) => w.key);
  expect(keys).toEqual([expect.stringMatching(UUID7), "0192f4c8-0000-7000-8000-000000000001"]);
  expect(within(dialog).getByText("https://x/dashboard#/invite/inv-2.s")).toBeTruthy();
});

// A REFUSAL IS THE ENGINE'S SENTENCE, with the grants that would admit; and a
// refused create's key is not sent again with the next attempt.
test("a refusal is the engine's sentence and the grants that would admit", async () => {
  const eng = engine({
    "POST /iam/invitations": [
      json(403, {
        error: "unauthorized",
        message: "The credential you presented does not carry the grant this request needs.",
        reason: "directory",
        grants: ["people:manage"],
      }),
    ],
  });
  mount();
  fireEvent.click(await screen.findByRole("button", { name: "Invite person" }));
  const dialog = await screen.findByRole("dialog", { name: "Invite a person" });
  fireEvent.change(within(dialog).getByLabelText("Email address"), {
    target: { value: "dee@example.com" },
  });
  fireEvent.click(within(dialog).getByRole("button", { name: "Invite" }));
  await settle();
  const alert = within(dialog).getByRole("alert");
  expect(alert.textContent).toMatch(/does not carry the grant/);
  expect(alert.textContent).toMatch(/people:manage would admit you/);
  fireEvent.click(within(dialog).getByRole("button", { name: "Invite" }));
  await settle();
  const [first, second] = eng.writes();
  expect(second?.key).not.toBe(first?.key);
});

// CANCELLING AN INVITATION IS NEVER A BUTTON CALLED "CANCEL" BESIDE ANOTHER:
// the dialog's own dismissal read "Cancel" next to "Cancel invitation", and
// somebody meaning to cancel the invitation pressed it and kept it. Mutation:
// leave the dismissal's word at its default.
test("the cancel dialog's dismissal is not a second Cancel", async () => {
  engine();
  mount();
  const row = (await screen.findByText("sam@example.com")).closest(".grid-row") as HTMLElement;
  fireEvent.click(within(row).getByRole("button", { name: /^Cancel the invitation/ }));
  const dialog = await screen.findByRole("dialog", { name: "Cancel this invitation?" });
  expect(within(dialog).getByRole("button", { name: "Keep it" })).toBeTruthy();
  expect(within(dialog).queryByRole("button", { name: "Cancel" })).toBeNull();
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
  const row = (await screen.findByText("sam@example.com")).closest(".grid-row") as HTMLElement;
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
  const row = (await screen.findByText("sam@example.com")).closest(".grid-row") as HTMLElement;
  fireEvent.click(within(row).getByRole("button", { name: /^Cancel the invitation/ }));
  const dialog = await screen.findByRole("dialog", { name: "Cancel this invitation?" });
  fireEvent.click(within(dialog).getByRole("button", { name: "Cancel invitation" }));
  await settle();
  expect(within(dialog).getByRole("button", { name: "Waiting for you" })).toBeTruthy();
  await act(async () => release(true));
  await settle();
  expect(screen.queryByRole("dialog", { name: "Cancel this invitation?" })).toBeNull();
});

// THE DIALOGS SAY WHAT THIS PERSON HOLDS AND WHAT THIS DEPLOYMENT ASKS. Bo
// holds no seat, so none is withheld or freed; where a second factor is
// optional, a reset promises no enrolment the next sign-in never asks for. The
// CONTROL is the reset where one is required. Mutation: name the seat
// whatever the person holds, or promise the enrolment whatever the setting.
test.each([
  ["optional", /password alone until they set up a new one/],
  ["required", /enrol a new factor at their next sign-in/],
])(
  "a seatless person's dialogs name no seat, and a reset says what %s asks",
  async (setting, after) => {
    secondFactor = setting;
    engine();
    location.hash = "#/settings/access?person=p-bo";
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "Suspend" }));
    const suspend = await screen.findByRole("dialog", { name: "Suspend Bo Lang?" });
    expect(within(suspend).queryByText(/seat/)).toBeNull();
    fireEvent.click(within(suspend).getByRole("button", { name: "Cancel" }));
    fireEvent.click(screen.getByRole("button", { name: "Remove" }));
    const remove = await screen.findByRole("dialog", { name: "Remove Bo Lang?" });
    expect(within(remove).queryByText(/seat/)).toBeNull();
    fireEvent.click(within(remove).getByRole("button", { name: "Cancel" }));
    fireEvent.click(await screen.findByRole("button", { name: "Reset second factor" }));
    const reset = await screen.findByRole("dialog", { name: "Reset Bo Lang's second factor?" });
    expect(await within(reset).findByText(after)).toBeTruthy();
    secondFactor = "required";
  },
);

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

  fireEvent.click(await screen.findByRole("button", { name: "Revoke Authenticator app" }));
  const revoke = await screen.findByRole("dialog", { name: /Revoke this authenticator app/ });
  fireEvent.click(within(revoke).getByRole("button", { name: "Revoke" }));
  await settle();

  const [reset, removal, revocation] = eng.writes();
  expect(reset?.path).toBe("/iam/people/p-bo/password-reset");
  expect(reset?.key).toBeNull();
  expect(removal?.method).toBe("DELETE");
  expect(removal?.path).toBe("/iam/people/p-bo");
  expect(revocation?.path).toBe("/iam/credentials/c-totp");
  expect(revocation?.query.get("person")).toBe("p-bo");
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

// FIRST RUN: nobody invited, and the screen says what to do next with the
// button that does it. The control is a claimed deployment.
test("an unclaimed deployment's screen says to invite yourself, with the button", async () => {
  engine();
  mount(ADMIN, "unclaimed");
  const callout = (await screen.findByText("Nobody has been invited yet")).closest(
    ".crewlet-callout",
  ) as HTMLElement;
  expect(await within(callout).findByRole("button", { name: "Invite person" })).toBeTruthy();
  cleanup();
  engine();
  mount(ADMIN, "ready");
  await screen.findByText("Bo Lang");
  expect(screen.queryByText("Nobody has been invited yet")).toBeNull();
});
