/**
 * The Account page: a signed-in person's own profile, password, second factor,
 * sessions and personal access tokens — and, for a credential that is not a
 * person's, the plain fact of what it is.
 *
 * The invariants, in the order they cost when they go: every read and write
 * names the CALLER and never somebody picked — no `?person=` on a credential
 * read, a mint or a revocation; this browser's own session is marked and
 * offered no named sign-out, while another is ended by its lineage — after a
 * gesture whose step-up replaced this browser's session too; a password
 * change sends the current one as its proof and says what it ended, and one
 * that ended this browser's session sends it to sign in saying so; a wrong
 * current password is the engine's sentence and does NOT send the person to
 * sign in; recovery codes are offered only beside an authenticator; and a Tier
 * A token's session is told what it is and asks the directory nothing.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { Account } from "./Account.tsx";
import { fmtDateTime } from "~/lib/format.ts";
import { Router } from "~/app/router.tsx";
import { page } from "~/lib/session.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import {
  currentSessionNeed,
  LiveSocket,
  sessionRestored,
  Store,
  type SessionAnswer,
} from "~/protocol/index.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

const HERE = "0192e7a0-0000-7000-8000-00000000000a";
const LAPTOP = "0192e7a0-0000-7000-8000-00000000000b";
const ENDED = "0192e7a0-0000-7000-8000-00000000000c";

const PERSON: SessionAnswer = {
  person: "p-1",
  login: "ada.lovelace",
  seat: "ada",
  kind: "person",
  grants: ["state:read", "work:write"],
  expires_at: "2026-10-07T00:00:00Z",
  lineage: HERE,
  status: "signed_in",
};

const TIER_A: SessionAnswer = {
  person: "t-ops",
  login: "token:ops",
  kind: "machine",
  grants: ["state:read", "fleet:operate"],
  expires_at: "2026-10-06T02:00:00Z",
  lineage: "0192e7a0-0000-7000-8000-0000000000ff",
  status: "signed_in",
};

const ROW = {
  id: "p-1",
  kind: "person",
  stage: "active",
  login: "ada.lovelace",
  name: "Ada Lovelace",
  email: "ada@example.com",
  seat: "ada",
  grants: ["state:read", "work:write"],
};

const SESSIONS = {
  sessions: [
    { lineage: HERE, person: "p-1", created_at: "2026-10-06T00:00:00Z", live: true },
    { lineage: LAPTOP, person: "p-1", created_at: "2026-10-05T09:00:00Z", live: true },
    { lineage: ENDED, person: "p-1", created_at: "2026-10-01T09:00:00Z", live: false },
  ],
};

const APP = { id: "c-totp", person: "p-1", method: "totp", revoked: false };
const CODES = { id: "c-codes", person: "p-1", method: "recovery", revoked: false };
const TOKEN = {
  id: "c-tok",
  person: "p-1",
  method: "token",
  label: "my assistant",
  revoked: false,
  grants: ["state:read"],
};

interface Sent {
  method: string;
  path: string;
  query: URLSearchParams;
  body: unknown;
}

function json(status: number, payload: unknown): Response {
  return new Response(JSON.stringify(payload), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

/**
 * The engine: the reads a person's page makes, from `state` — who this
 * browser is (null once its cookie is gone, answered 401), its sessions and
 * `credentials` — and every write answered from `writes` by `METHOD /path`,
 * which may move `state` as the engine's own write would.
 */
function engine({
  session = PERSON,
  credentials = [{ id: "c-pw", person: "p-1", method: "password", revoked: false }, APP, TOKEN],
  writes = {},
}: {
  session?: SessionAnswer;
  credentials?: unknown[];
  writes?: Record<string, () => Response>;
} = {}) {
  const state: { session: SessionAnswer | null; sessions: typeof SESSIONS } = {
    session,
    sessions: SESSIONS,
  };
  const sent: Sent[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input), "http://engine.test");
      const method = (init?.method ?? "GET").toUpperCase();
      sent.push({
        method,
        path: url.pathname,
        query: url.searchParams,
        body: typeof init?.body === "string" ? JSON.parse(init.body) : undefined,
      });
      if (method !== "GET") {
        const answer = writes[`${method} ${url.pathname}`];
        return answer ? answer() : json(404, { error: "no_route" });
      }
      switch (url.pathname) {
        case "/auth/session":
          return state.session
            ? json(200, state.session)
            : json(401, { error: "unauthenticated", message: "Sign in." });
        case "/auth/config":
          return json(200, { min_password_length: 12, second_factor: "optional" });
        case "/iam/people/p-1":
          return json(200, ROW);
        case "/iam/people/p-1/sessions":
          return json(200, state.sessions);
        case "/iam/credentials":
          return json(200, { credentials });
      }
      return json(404, { error: "no_route" });
    }),
  );
  return {
    sent,
    state,
    writes: () => sent.filter((s) => s.method !== "GET"),
    reads: (path: string) => sent.filter((s) => s.method === "GET" && s.path === path),
  };
}

function mount() {
  const store = new Store();
  const socket = new LiveSocket(store);
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <ToastProvider>
          <LayerHost>
            <Account />
          </LayerHost>
        </ToastProvider>
      </Router>
    </ClientContext.Provider>,
  );
}

function type(label: string, value: string) {
  fireEvent.change(screen.getByLabelText(label), { target: { value } });
}

let reloads: ReturnType<typeof vi.spyOn>;

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  reloads = vi.spyOn(page, "reloadInto").mockImplementation(() => {});
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  sessionRestored();
  location.hash = "#/";
});

describe("a person's own page", () => {
  test("reads the caller's own records, naming nobody, and shows the profile read-only", async () => {
    const engineIs = engine();
    mount();
    expect(await screen.findByText("ada@example.com")).toBeDefined();
    expect(screen.getByText("Ada Lovelace")).toBeDefined();
    expect(screen.getByText(/An administrator changes these/)).toBeDefined();
    // THE CALLER'S OWN: a credential read naming a person would be a read the
    // authority table decides about somebody else.
    const credentialReads = engineIs.reads("/iam/credentials");
    expect(credentialReads.length).toBeGreaterThan(0);
    for (const read of credentialReads) expect(read.query.has("person")).toBe(false);
    expect(engineIs.reads("/iam/people/p-1").length).toBeGreaterThan(0);
  });

  // THE CONTROL is the other live session, which IS offered a named sign-out;
  // an ended one is not listed at all.
  test("marks this browser, and signs another session out by its lineage", async () => {
    const engineIs = engine({
      writes: { [`POST /auth/logout/${LAPTOP}`]: () => json(200, { status: "ended" }) },
    });
    mount();
    expect(await screen.findByText("This browser")).toBeDefined();
    const signOuts = await screen.findAllByRole("button", { name: /^Sign out the session/ });
    expect(signOuts).toHaveLength(1);
    // NAMED BY A TIME A PERSON READS, never the raw instant a screen reader
    // spelled out. Mutation: name it by `created_at` as it arrives.
    expect(signOuts[0]!.getAttribute("aria-label")).toBe(
      `Sign out the session started ${fmtDateTime("2026-10-05T09:00:00Z")}`,
    );
    expect(signOuts[0]!.getAttribute("aria-label")).not.toMatch(/\d{4}-\d{2}-\d{2}T/);
    const before = engineIs.reads("/iam/people/p-1/sessions").length;
    fireEvent.click(signOuts[0]!);
    await waitFor(() =>
      expect(engineIs.writes().map((w) => `${w.method} ${w.path}`)).toEqual([
        `POST /auth/logout/${LAPTOP}`,
      ]),
    );
    await waitFor(() =>
      expect(engineIs.reads("/iam/people/p-1/sessions").length).toBeGreaterThan(before),
    );
    expect(reloads).not.toHaveBeenCalled();
  });

  // A STEP-UP REPLACES THIS BROWSER'S SESSION under a new lineage, and nothing
  // else tells the page: a mark read before it left this browser's new
  // session untagged and offered it a named sign-out, which ends this browser.
  // The CONTROL is the other session the new list carries, which IS offered
  // one.
  test("a gesture whose step-up replaced this browser's session marks the new one", async () => {
    const STEPPED_UP = "0192e7a0-0000-7000-8000-00000000000d";
    const PHONE = "0192e7a0-0000-7000-8000-00000000000e";
    const engineIs = engine({
      writes: {
        "DELETE /iam/credentials/c-tok": () => {
          engineIs.state.session = { ...PERSON, lineage: STEPPED_UP };
          engineIs.state.sessions = {
            sessions: [
              {
                lineage: STEPPED_UP,
                person: "p-1",
                created_at: "2026-10-06T01:00:00Z",
                live: true,
              },
              { lineage: PHONE, person: "p-1", created_at: "2026-10-05T20:00:00Z", live: true },
              { lineage: HERE, person: "p-1", created_at: "2026-10-06T00:00:00Z", live: false },
            ],
          };
          return json(200, { revoked: true });
        },
      },
    });
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "Revoke my assistant" }));
    const dialog = await screen.findByRole("dialog", { name: "Revoke this token?" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Revoke" }));
    expect(
      await screen.findByRole("button", {
        name: `Sign out the session started ${fmtDateTime("2026-10-05T20:00:00Z")}`,
      }),
    ).toBeDefined();
    expect(screen.getByText("This browser")).toBeDefined();
    expect(
      screen.queryByRole("button", {
        name: `Sign out the session started ${fmtDateTime("2026-10-06T01:00:00Z")}`,
      }),
    ).toBeNull();
  });

  test("signing out everywhere says it ends personal tokens, and ends in the sign-in", async () => {
    const engineIs = engine({
      writes: { "POST /auth/logout/all": () => json(200, {}) },
    });
    mount();
    await screen.findByText("This browser");
    expect(screen.getAllByText(/every personal\s+access token you minted/).length).toBeGreaterThan(
      0,
    );
    fireEvent.click(screen.getByRole("button", { name: "Sign out everywhere" }));
    // ASKED FIRST: nothing is sent until the person confirms.
    const ask = await screen.findByRole("dialog", { name: "Sign out everywhere?" });
    expect(engineIs.writes()).toEqual([]);
    fireEvent.click(within(ask).getByRole("button", { name: "Sign out everywhere" }));
    await waitFor(() => expect(reloads).toHaveBeenCalledWith("#/login"));
    expect(engineIs.writes().map((w) => w.path)).toEqual(["/auth/logout/all"]);
  });
});

describe("changing the password", () => {
  async function fill(current: string, next: string, again = next) {
    await screen.findByLabelText("Current password");
    type("Current password", current);
    type("New password", next);
    type("Confirm new password", again);
    fireEvent.click(screen.getByRole("button", { name: "Change password" }));
  }

  test("sends the current one as its proof, and keeps this browser signed in", async () => {
    const engineIs = engine({
      writes: {
        "POST /auth/password": () =>
          json(200, {
            ...PERSON,
            lineage: undefined,
            position: "1:9",
            expires_at: PERSON.expires_at,
          }),
      },
    });
    mount();
    const sessionReads = () => engineIs.reads("/auth/session").length;
    await screen.findByText("This browser");
    const before = sessionReads();
    await fill("the old long passphrase", "a brand new long passphrase");
    expect(
      await screen.findByText(/Every other session and every personal access token/),
    ).toBeDefined();
    expect(screen.getByText(/this browser stays signed in/)).toBeDefined();
    expect(engineIs.writes()).toHaveLength(1);
    expect(engineIs.writes()[0]!.body).toEqual({
      current_password: "the old long passphrase",
      new_password: "a brand new long passphrase",
    });
    // THE SESSION MOVED — a password change replaces it — so who this browser
    // is, and which session is it, is read again.
    await waitFor(() => expect(sessionReads()).toBeGreaterThan(before));
  });

  test("a confirmation that differs posts nothing", async () => {
    const engineIs = engine();
    mount();
    await fill(
      "the old long passphrase",
      "a brand new long passphrase",
      "a brand new long passfrase",
    );
    expect(await screen.findByText("The two passwords are not the same.")).toBeDefined();
    expect(engineIs.writes()).toEqual([]);
  });

  // A WRONG CURRENT PASSWORD IS ABOUT WHAT WAS TYPED, not the browser's
  // session: the one sign-in refusal, said here, and the person is NOT sent
  // to sign in — a 401 of any other kind would have.
  test("a wrong current password is the engine's sentence, and nobody is signed out", async () => {
    engine({
      writes: {
        "POST /auth/password": () =>
          json(401, {
            error: "sign_in_refused",
            message: "Those details were not accepted.",
          }),
      },
    });
    mount();
    await fill("not my password at all", "a brand new long passphrase");
    expect(await screen.findByText("Those details were not accepted.")).toBeDefined();
    expect(currentSessionNeed()).toBeNull();
  });

  // THE 202 ENDED THIS BROWSER'S SESSION and cleared its cookie, so every read
  // after it is refused 401 — which sent the tab to sign in with the page's
  // own sentence unmounted before anybody read it. It goes to sign in at once
  // instead, saying why, and reads nothing first.
  test("a change this node has not applied yet sends this browser to sign in, saying why", async () => {
    const engineIs = engine({
      writes: {
        "POST /auth/password": () => {
          engineIs.state.session = null;
          return json(202, {
            status: "password_changed",
            detail: "this node has not applied the change yet",
          });
        },
      },
    });
    mount();
    await screen.findByText("This browser");
    const before = engineIs.reads("/auth/session").length;
    await fill("the old long passphrase", "a brand new long passphrase");
    expect((await screen.findAllByText("Your password is changed")).length).toBeGreaterThan(0);
    expect(
      screen.getAllByText(/this one included: sign in with the new password/).length,
    ).toBeGreaterThan(0);
    await waitFor(() => expect(location.hash.startsWith("#/login")).toBe(true));
    expect(engineIs.reads("/auth/session").length).toBe(before);
    expect(currentSessionNeed()).toBeNull();
  });
});

describe("the second factor", () => {
  // HELD ALONE, recovery codes are a second factor of their own, which the
  // engine refuses to issue — so they are offered beside an app and not
  // without one. The CONTROL is the person holding an app.
  test("recovery codes are offered only beside an authenticator", async () => {
    engine({ credentials: [APP, CODES] });
    mount();
    expect(await screen.findByRole("button", { name: "Replace authenticator…" })).toBeDefined();
    expect(screen.getByRole("button", { name: "New recovery codes…" })).toBeDefined();
    cleanup();
    engine({ credentials: [] });
    mount();
    expect(await screen.findByRole("button", { name: "Set up an authenticator…" })).toBeDefined();
    expect(screen.queryByRole("button", { name: "New recovery codes…" })).toBeNull();
  });
});

describe("a first authenticator", () => {
  // A FIRST AUTHENTICATOR ISSUES A FIRST SET OF RECOVERY CODES, as a required
  // enrolment does; it said "your recovery codes are unchanged" to somebody who
  // held none and prompted nothing. The CONTROL replaces an app beside a set,
  // which keeps it and says so. Mutation: skip the set and the first case reads
  // the replacement's sentence.
  test.each([
    ["holding no recovery codes", [], "Set up an authenticator…", true],
    ["replacing an app beside a set (the control)", [APP, CODES], "Replace authenticator…", false],
  ])("%s", async (_, credentials, press, issued) => {
    let legs = 0;
    const engineIs = engine({
      credentials,
      writes: {
        "POST /auth/totp": () =>
          legs++ === 0
            ? json(200, { secret: "JBSWY3DPEHPK3PXP", uri: "otpauth://totp/x" })
            : json(200, { status: "enrolled" }),
        "POST /auth/totp/recovery": () => json(200, { codes: ["aaaa-bbbb", "cccc-dddd"] }),
      },
    });
    mount();
    fireEvent.click(await screen.findByRole("button", { name: press }));
    const dialog = await screen.findByRole("dialog", { name: "Two-step verification" });
    fireEvent.change(await within(dialog).findByLabelText(/code/i), {
      target: { value: "123456" },
    });
    fireEvent.submit(within(dialog).getByLabelText(/code/i).closest("form")!);
    if (issued) {
      expect(await within(dialog).findByText("aaaa-bbbb")).toBeDefined();
      expect(within(dialog).queryByText(/recovery codes are unchanged/)).toBeNull();
    } else {
      expect(await within(dialog).findByText(/recovery codes are unchanged/)).toBeDefined();
    }
    expect(engineIs.writes().some((w) => w.path === "/auth/totp/recovery")).toBe(issued);
  });
});

describe("personal access tokens", () => {
  test("a mint names nobody, and its value is shown once as acting as you", async () => {
    const engineIs = engine({
      writes: {
        "POST /iam/credentials": () =>
          json(201, {
            id: "c-new",
            person: "p-1",
            token: "cwl_pat_c-new_9_secret",
            grants: ["state:read"],
            expires_at: "2027-01-04T00:00:00Z",
          }),
      },
    });
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "New token" }));
    const dialog = await screen.findByRole("dialog", { name: "New personal access token" });
    fireEvent.change(within(dialog).getByLabelText(/^Label/), { target: { value: "laptop" } });
    // NOTHING STARTS TICKED: a token carries what its holder chose, one grant
    // at a time, rather than everything they hold.
    for (const box of within(dialog).getAllByRole("checkbox")) {
      expect(box).toHaveProperty("checked", false);
    }
    fireEvent.click(within(dialog).getByRole("checkbox", { name: "state:read" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Mint" }));
    expect(await within(dialog).findByText(/This token acts as you/)).toBeDefined();
    const mint = engineIs.writes().find((w) => w.path === "/iam/credentials")!;
    expect(mint.method).toBe("POST");
    // THE OWNER IS THE CALLER: a person's token is theirs alone to mint.
    expect(mint.query.has("person")).toBe(false);
    expect(mint.body).toEqual({ label: "laptop", grants: ["state:read"] });
  });

  // THE ENGINE'S CEILING, said under the field: past it the dialog posted and
  // showed the engine's 400 back. The CONTROL is the mint above, which names
  // no lifetime and is posted.
  test("a lifetime past the engine's ceiling is refused under the field, and nothing is posted", async () => {
    const engineIs = engine({});
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "New token" }));
    const dialog = await screen.findByRole("dialog", { name: "New personal access token" });
    fireEvent.change(within(dialog).getByLabelText(/^Expires in days/), {
      target: { value: "400" },
    });
    expect(within(dialog).getByText("At most 365 days.")).toBeDefined();
    expect(within(dialog).getByRole("button", { name: "Mint" })).toHaveProperty("disabled", true);
    expect(engineIs.writes()).toEqual([]);
  });

  // A TOKEN A COUNTER ENDED — a password change, signing out everywhere —
  // carries no `revoked_at`, and the engine lists it revoked: it was revoked,
  // not aged out. The CONTROL is a token past its own deadline.
  test("a token a counter ended reads revoked, and only one past its deadline expired", async () => {
    engine({
      credentials: [
        {
          ...TOKEN,
          id: "c-ended",
          label: "ended",
          revoked: true,
          expires_at: "2099-01-01T00:00:00Z",
        },
        { ...TOKEN, id: "c-old", label: "old", revoked: true, expires_at: "2020-01-01T00:00:00Z" },
      ],
    });
    mount();
    expect(await screen.findByText("Revoked")).toBeDefined();
    expect(screen.getAllByText("Revoked")).toHaveLength(1);
    expect(screen.getAllByText("Expired")).toHaveLength(1);
  });

  test("a token is revoked by its id, naming nobody", async () => {
    const engineIs = engine({
      writes: { "DELETE /iam/credentials/c-tok": () => json(200, { revoked: true }) },
    });
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "Revoke my assistant" }));
    const dialog = await screen.findByRole("dialog", { name: "Revoke this token?" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Revoke" }));
    await waitFor(() => expect(engineIs.writes()).toHaveLength(1));
    const revoke = engineIs.writes()[0]!;
    expect(`${revoke.method} ${revoke.path}`).toBe("DELETE /iam/credentials/c-tok");
    expect(revoke.query.has("person")).toBe(false);
  });
});

describe("a credential that is not a person's", () => {
  // THE CONTROL is every case above, which reads the directory.
  test("a Tier A token's session is told what it is, and asks the directory nothing", async () => {
    const engineIs = engine({ session: TIER_A });
    mount();
    expect(await screen.findByText(/this deployment.s API token/)).toBeDefined();
    expect(screen.getByText("ops")).toBeDefined();
    expect(screen.queryByLabelText("Current password")).toBeNull();
    expect(screen.queryByRole("button", { name: "New token" })).toBeNull();
    expect(engineIs.sent.filter((s) => s.path.startsWith("/iam/"))).toEqual([]);
  });
});
