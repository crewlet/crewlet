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
      id: "p-ci",
      kind: "machine",
      stage: "active",
      login: "ci:release",
      grants: ["work:write", "people:manage"],
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
 * — a queue per `METHOD /path`, the last answer repeating.
 */
function engine(writes: Record<string, Response[]> = {}) {
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
      case "/iam/people":
        return Promise.resolve(json(200, PEOPLE));
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
    { grants: ["state:read", "work:write"] },
    { stage: "suspended" },
  ]);
  for (const w of eng.writes()) expect(w.key).toMatch(UUID7);
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
  fireEvent.click(screen.getByRole("button", { name: "Done" }));

  fireEvent.click(screen.getByRole("button", { name: "Remove" }));
  const remove = await screen.findByRole("dialog", { name: "Remove Bo Lang?" });
  const confirm = within(remove).getByRole("button", { name: "Remove" });
  expect(confirm).toHaveProperty("disabled", true);
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

// A SERVICE ACCOUNT IS A KEYED CREATE OF A MACHINE, and its token's mint is
// unkeyed, offers the account's grants alone — never one a token may not carry
// — and shows the value once.
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
  fireEvent.click(within(create).getByRole("checkbox", { name: "work:write" }));
  fireEvent.click(within(create).getByRole("checkbox", { name: "people:manage" }));
  fireEvent.click(within(create).getByRole("button", { name: "Create" }));
  await settle();
  fireEvent.click(screen.getByRole("button", { name: "Mint its token" }));
  const mint = await screen.findByRole("dialog", { name: "Mint a token for ci:deploy" });
  // OUT OF THE ACCOUNT'S OWN GRANTS, and people:manage greyed out.
  expect(within(mint).queryByRole("checkbox", { name: "state:read" })).toBeNull();
  expect(within(mint).getByRole("checkbox", { name: "people:manage" })).toHaveProperty(
    "disabled",
    true,
  );
  fireEvent.click(within(mint).getByRole("button", { name: "Mint" }));
  await settle();
  const [created, minted] = eng.writes();
  expect(created?.body).toEqual({
    kind: "machine",
    login: "ci:deploy",
    name: "",
    grants: ["work:write", "people:manage"],
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
