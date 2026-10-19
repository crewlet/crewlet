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
 * change sends the current one as its proof — and a code, from somebody who
 * holds a second factor, whose field nobody else is shown — and says what it
 * ended, and one that ended this browser's session sends it to sign in saying
 * so; a wrong current password is the engine's sentence, a wrong code the
 * step-up dialog's, and neither sends the person to sign in; recovery codes
 * are offered only beside an authenticator; and a Tier
 * A token's session is told what it is and asks the directory nothing.
 *
 * And what a person comes for is where they look first: two-step
 * verification leads Security and the password form waits behind its button;
 * a session row's sign-out names itself on a phone's card, and signing out
 * everywhere stands beside the sentence saying what it ends rather than in
 * the title's way; the seat is written as a seat — its name and `@handle`,
 * linked only for a reader its page serves; and the page's note describes
 * the session it shows.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { Account } from "./Account.tsx";
import { fmtDateTime } from "~/lib/format.ts";
import { Router } from "~/app/router.tsx";
import { page } from "~/lib/session.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { SessionReading } from "~/lib/frameSession.ts";
import { withDerived } from "~/test/org.ts";
import { href } from "~/app/router.tsx";
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
  grants: ["worklet"],
  expires_at: "2026-10-07T00:00:00Z",
  lineage: HERE,
  status: "signed_in",
};

const TIER_A: SessionAnswer = {
  person: "t-ops",
  login: "token:ops",
  kind: "machine",
  grants: ["admin"],
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
  grants: ["worklet"],
};

const SESSIONS = {
  sessions: [
    { lineage: HERE, person: "p-1", created_at: "2026-10-06T00:00:00Z", live: true },
    { lineage: LAPTOP, person: "p-1", created_at: "2026-10-05T09:00:00Z", live: true },
    { lineage: ENDED, person: "p-1", created_at: "2026-10-01T09:00:00Z", live: false },
  ],
};

const PASSWORD = { id: "c-pw", person: "p-1", method: "password", revoked: false };
const APP = { id: "c-totp", person: "p-1", method: "totp", revoked: false };
const CODES = { id: "c-codes", person: "p-1", method: "recovery", revoked: false };
const TOKEN = {
  id: "c-tok",
  person: "p-1",
  method: "token",
  label: "my assistant",
  revoked: false,
  grants: [],
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
  row = ROW,
  credentials = [PASSWORD, APP, TOKEN],
  writes = {},
}: {
  session?: SessionAnswer;
  /** The caller's own directory row. */
  row?: Record<string, unknown>;
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
          return json(200, row);
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

/** The page, over a store holding `org` where the chart is readable. */
function mount(org?: unknown, refused: string | null = null) {
  const store = new Store();
  if (org) store.applyOrg(withDerived(org) as never);
  if (refused !== null) store.setAccessRefused(refused);
  const socket = new LiveSocket(store);
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <ToastProvider>
          <LayerHost>
            <SessionReading>
              <Account />
            </SessionReading>
          </LayerHost>
        </ToastProvider>
      </Router>
    </ClientContext.Provider>,
  );
}

function type(label: string, value: string) {
  fireEvent.change(screen.getByLabelText(label), { target: { value } });
}

/** Open the password's dialog from its line, as a person does. */
async function openPasswordDialog(): Promise<HTMLElement> {
  fireEvent.click(await screen.findByRole("button", { name: "Change password…" }));
  return screen.findByRole("dialog", { name: "Change password" });
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
    expect(screen.getByText("Only an admin can change these.")).toBeDefined();
    // A PERSON'S OWN ACCOUNT IS ALWAYS A PERSON'S: no Kind row says so.
    expect(screen.queryByText("Kind")).toBeNull();
    // THE CALLER'S OWN: a credential read naming a person would be a read the
    // authority table decides about somebody else.
    const credentialReads = engineIs.reads("/iam/credentials");
    expect(credentialReads.length).toBeGreaterThan(0);
    for (const read of credentialReads) expect(read.query.has("person")).toBe(false);
    expect(engineIs.reads("/iam/people/p-1").length).toBeGreaterThan(0);
    // WHO THIS IS IS THE FRAME'S ONE READ: the page asked again for itself,
    // and again on every focus beside the frame. Mutation: read it here too
    // and it is asked twice.
    expect(engineIs.reads("/auth/session")).toHaveLength(1);
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
    expect(screen.getAllByText(/stops your tokens/).length).toBeGreaterThan(0);
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
  /** Somebody holding no second factor: a password and a token. */
  const PASSWORD_ONLY = [PASSWORD, TOKEN];

  async function fill(current: string, next: string, again = next, code?: string) {
    if (!screen.queryByRole("dialog", { name: "Change password" })) await openPasswordDialog();
    await screen.findByLabelText("Current password");
    type("Current password", current);
    if (code !== undefined) type("Code", code);
    type("New password", next);
    type("Confirm new password", again);
    fireEvent.click(screen.getByRole("button", { name: "Change password" }));
  }

  // SOMEBODY HOLDING NO SECOND FACTOR is asked for nothing beside the
  // password, which is the whole of a step-up from them.
  test("sends the current one as its proof, and keeps this browser signed in", async () => {
    const engineIs = engine({
      credentials: PASSWORD_ONLY,
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
    await screen.findByRole("button", { name: "Set up…" });
    await openPasswordDialog();
    expect(screen.queryByLabelText("Code")).toBeNull();
    const before = sessionReads();
    await fill("the old long passphrase", "a brand new long passphrase");
    expect(await screen.findByText("Password changed. Signed out everywhere else.")).toBeDefined();
    expect(screen.getByText("Password changed. Signed out everywhere else.")).toBeDefined();
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
    expect(await screen.findByText("The passwords don’t match.")).toBeDefined();
    expect(engineIs.writes()).toEqual([]);
  });

  // A WRONG CURRENT PASSWORD IS ABOUT WHAT WAS TYPED, not the browser's
  // session: the one sign-in refusal, said here, and the person is NOT sent
  // to sign in — a 401 of any other kind would have.
  test("a wrong current password is the engine's sentence, and nobody is signed out", async () => {
    engine({
      credentials: PASSWORD_ONLY,
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
    await screen.findByRole("button", { name: "Replace app…" });
    await openPasswordDialog();
    await screen.findByLabelText("Code");
    const before = engineIs.reads("/auth/session").length;
    await fill(
      "the old long passphrase",
      "a brand new long passphrase",
      "a brand new long passphrase",
      "123456",
    );
    expect((await screen.findAllByText("Password changed")).length).toBeGreaterThan(0);
    expect(screen.getAllByText("Sign in again.").length).toBeGreaterThan(0);
    await waitFor(() => expect(location.hash.startsWith("#/login")).toBe(true));
    expect(engineIs.reads("/auth/session").length).toBe(before);
    expect(currentSessionNeed()).toBeNull();
  });
});

describe("changing the password while holding a second factor", () => {
  async function fill(code: string) {
    if (!screen.queryByRole("dialog", { name: "Change password" })) await openPasswordDialog();
    await screen.findByLabelText("Code");
    type("Current password", "the old long passphrase");
    if (code) type("Code", code);
    type("New password", "a brand new long passphrase");
    type("Confirm new password", "a brand new long passphrase");
    fireEvent.click(screen.getByRole("button", { name: "Change password" }));
  }

  // THE CHANGE ENDS EVERY OTHER SESSION, so the engine asks somebody holding
  // a second factor for its code as a step-up does: on the password alone a
  // stolen cookie and a known password signed the owner out everywhere. The
  // CONTROL is the password-only change above, which draws no field and
  // sends none. Mutation: send the body without the code and the write
  // carries none.
  test.each([
    ["an authenticator app", [PASSWORD, APP, CODES]],
    ["recovery codes alone", [PASSWORD, CODES]],
  ])("holding %s, it asks for a code and sends it", async (_, credentials) => {
    const engineIs = engine({
      credentials,
      writes: {
        "POST /auth/password": () => json(200, { ...PERSON, lineage: undefined, position: "1:9" }),
      },
    });
    mount();
    await openPasswordDialog();
    expect(await screen.findByLabelText("Code")).toBeDefined();
    expect(screen.getByText("The current code from your app, or a recovery code.")).toBeDefined();
    await fill(" 123456 ");
    expect(await screen.findByText("Password changed. Signed out everywhere else.")).toBeDefined();
    expect(engineIs.writes()).toHaveLength(1);
    expect(engineIs.writes()[0]!.body).toEqual({
      current_password: "the old long passphrase",
      new_password: "a brand new long passphrase",
      code: "123456",
    });
  });

  // NOTHING IS SENT WITHOUT IT: the engine would only ask for it back.
  test("a change without the code posts nothing, and says what is missing", async () => {
    const engineIs = engine();
    mount();
    await fill("");
    expect(await screen.findByText("Enter a code from your app or a recovery code.")).toBeDefined();
    expect(engineIs.writes()).toEqual([]);
  });

  // A WRONG CODE IS THE SIGN-IN'S ONE REFUSAL, naming no field — said as the
  // step-up dialog says it, the spent code cleared, and nobody signed out. The
  // CONTROL is the wrong current password above, which the engine names and
  // words itself.
  test("a wrong code is worded as the step-up dialog words it, and nobody is signed out", async () => {
    engine({
      writes: {
        "POST /auth/password": () =>
          json(401, {
            error: "sign_in_refused",
            message: "Those sign-in details were not accepted. Check them and try again.",
          }),
      },
    });
    mount();
    await fill("000000");
    expect(await screen.findByText("Wrong password or code. Try again.")).toBeDefined();
    expect(screen.getByLabelText("Code")).toHaveProperty("value", "");
    expect(currentSessionNeed()).toBeNull();
  });

  // THE ENGINE IS WHAT DECIDES whether a code is held: a list that is behind
  // — a factor enrolled in another tab — draws no field, and the engine's
  // `second_factor_required` draws it, saying why, with nobody signed out.
  test("an engine asking for a code the list did not show draws the field", async () => {
    const engineIs = engine({
      credentials: [PASSWORD],
      writes: {
        "POST /auth/password": () =>
          json(401, { error: "second_factor_required", message: "Enter your code." }),
      },
    });
    mount();
    await screen.findByRole("button", { name: "Set up…" });
    await openPasswordDialog();
    expect(screen.queryByLabelText("Code")).toBeNull();
    type("Current password", "the old long passphrase");
    type("New password", "a brand new long passphrase");
    type("Confirm new password", "a brand new long passphrase");
    fireEvent.click(screen.getByRole("button", { name: "Change password" }));
    expect(await screen.findByLabelText("Code")).toBeDefined();
    expect(screen.getByText("Enter a code from your app or a recovery code too.")).toBeDefined();
    expect(engineIs.writes()[0]!.body).not.toHaveProperty("code");
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
    expect(await screen.findByRole("button", { name: "Replace app…" })).toBeDefined();
    expect(screen.getByRole("button", { name: "Create new codes…" })).toBeDefined();
    cleanup();
    engine({ credentials: [] });
    mount();
    expect(await screen.findByRole("button", { name: "Set up…" })).toBeDefined();
    expect(screen.queryByRole("button", { name: "Create new codes…" })).toBeNull();
  });
});

describe("a first authenticator", () => {
  // A FIRST AUTHENTICATOR ISSUES A FIRST SET OF RECOVERY CODES, as a required
  // enrolment does; it said "your recovery codes are unchanged" to somebody who
  // held none and prompted nothing. The CONTROL replaces an app beside a set,
  // which keeps it and says so. Mutation: skip the set and the first case reads
  // the replacement's sentence.
  test.each([
    ["holding no recovery codes", [], "Set up…", true],
    ["replacing an app beside a set (the control)", [APP, CODES], "Replace app…", false],
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
    // THE SIX-DIGIT FIELD BY ITS OWN LABEL: the setup also draws the seed as a
    // QR code, an image whose name says "code" too.
    fireEvent.change(await within(dialog).findByLabelText(/^code from the app/i), {
      target: { value: "123456" },
    });
    fireEvent.submit(
      within(dialog)
        .getByLabelText(/^code from the app/i)
        .closest("form")!,
    );
    if (issued) {
      expect(await within(dialog).findByText("aaaa-bbbb")).toBeDefined();
      expect(within(dialog).queryByText(/Codes from your old app stop working/)).toBeNull();
    } else {
      expect(await within(dialog).findByText(/Codes from your old app stop working/)).toBeDefined();
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
            grants: [],
            expires_at: "2027-01-04T00:00:00Z",
          }),
      },
    });
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "New token" }));
    const dialog = await screen.findByRole("dialog", { name: "New personal access token" });
    fireEvent.change(within(dialog).getByLabelText(/^Label/), { target: { value: "laptop" } });
    // THE PERSON'S OWN ACCESS STARTS TICKED, the token they almost always
    // want: with nothing ticked, one pressed through could not read their own
    // inbox. They untick what the token does not need.
    expect(within(dialog).getByRole("checkbox", { name: "Worklet" })).toHaveProperty(
      "checked",
      true,
    );
    expect(within(dialog).queryByText(/Nothing ticked/)).toBeNull();
    // AND A TOKEN CARRYING NO ACCESS SAYS WHAT IT REACHES rather than reading
    // as one that does nothing. Mutation: drop the sentence and this goes red.
    fireEvent.click(within(dialog).getByRole("checkbox", { name: "Worklet" }));
    expect(
      within(dialog).getByText("Nothing ticked: overview only. No tasks, pages or chat."),
    ).toBeDefined();
    fireEvent.click(within(dialog).getByRole("button", { name: "Create token" }));
    expect(await within(dialog).findByText(/Copy it now\. You won’t see it again\./)).toBeDefined();
    const mint = engineIs.writes().find((w) => w.path === "/iam/credentials")!;
    expect(mint.method).toBe("POST");
    // THE OWNER IS THE CALLER: a person's token is theirs alone to mint.
    expect(mint.query.has("person")).toBe(false);
    expect(mint.body).toEqual({ label: "laptop", grants: [] });
  });

  // ADMIN IS NEVER TICKED FOR A PERSON'S OWN TOKEN: a bearer secret carrying
  // Admin is host access for whatever script holds it, so it is ticked on
  // purpose or not at all. The apps an admin holds start ticked as anybody's
  // do. Mutation: drop the Admin exception and Admin starts ticked.
  test("an admin's own token starts with their apps and never Admin", async () => {
    const grants = ["admin", "worklet", "doclet", "artifacts", "chatlet"];
    engine({ session: { ...PERSON, grants }, row: { ...ROW, grants } });
    mount();
    await screen.findByText("ada@example.com");
    fireEvent.click(await screen.findByRole("button", { name: "New token" }));
    const dialog = await screen.findByRole("dialog", { name: "New personal access token" });
    expect(within(dialog).getByRole("checkbox", { name: "Admin" })).toHaveProperty(
      "checked",
      false,
    );
    for (const name of ["Worklet", "Doclet", "Artifacts", "Chatlet"]) {
      expect(within(dialog).getByRole("checkbox", { name })).toHaveProperty("checked", true);
    }
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
    expect(within(dialog).getByRole("button", { name: "Create token" })).toHaveProperty(
      "disabled",
      true,
    );
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
    // ONLY WORKING TOKENS BY DEFAULT: the ended ones wait behind the box.
    fireEvent.click(await screen.findByLabelText("Show revoked and expired"));
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

describe("what a person comes for is where they look", () => {
  // TWO-STEP VERIFICATION LEADS, and the password form waits behind its
  // button: drawn in place, its four fields stood above the second factor and
  // pushed it below the fold at 1280x720 and on a phone, and a person looking
  // for two-step verification found a password form. Mutations: put the
  // password's line first, or draw its form in place, and this goes red.
  test("two-step verification comes first, and the password's form opens in a dialog", async () => {
    engine({ credentials: [PASSWORD] });
    mount();
    await screen.findByRole("button", { name: "Set up…" });
    const twoStep = screen.getByRole("region", { name: "Two-step verification" });
    const password = screen.getByRole("region", { name: "Password" });
    expect(
      twoStep.compareDocumentPosition(password) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
    expect(screen.queryByLabelText("Current password")).toBeNull();
    expect(screen.queryByLabelText("New password")).toBeNull();
    const dialog = await openPasswordDialog();
    expect(within(dialog).getByLabelText("Current password")).toBeDefined();
    expect(within(dialog).getByLabelText("New password")).toBeDefined();
  });

  // THE HEADER IS THE TITLE'S: "Sign out everywhere" there took its width,
  // and on a phone the title read "Where you are sign…". It stands beside the
  // sentence saying what it ends. Mutation: hand it back to `actions`.
  test("signing out everywhere stands beside what it ends, not in the title's header", async () => {
    engine();
    mount();
    const title = await screen.findByText("Where you are signed in");
    const header = title.closest<HTMLElement>(".crewlet-card__header")!;
    expect(header).not.toBeNull();
    expect(within(header).queryAllByRole("button")).toEqual([]);
    const everywhere = screen.getByRole("button", { name: "Sign out everywhere" });
    expect(title.closest(".crewlet-card")!.contains(everywhere)).toBe(true);
    expect(everywhere.parentElement!.textContent).toMatch(
      /Signs you out on every device and stops your tokens/,
    );
  });

  // A BUTTON IS ITS OWN NAME on a phone's card, where a labelled column drew
  // "SIGN OUT  Sign out" on every row and "SIGN OUT  This browser" on this
  // one. The CONTROL is the Signed in column, which still says its word.
  // Mutation: declare the column with `label: "Sign out"` and no `action`.
  test("a session's sign-out names itself, with no column word beside it", async () => {
    engine();
    mount();
    const mark = await screen.findByText("This browser");
    const grid = mark.closest<HTMLElement>(".crewlet-data-grid")!;
    const labels = [
      ...grid.querySelectorAll(".crewlet-data-grid__row > .crewlet-data-grid__cell"),
    ].map((c) => c.getAttribute("data-label"));
    expect(labels).toContain("Signed in");
    expect(labels).not.toContain("Sign out");
    expect(mark.closest(".crewlet-data-grid__cell")!.hasAttribute("data-label")).toBe(false);
  });
});

describe("the profile", () => {
  /** The Seat fact, once the profile has answered. */
  async function seatFact(): Promise<HTMLElement> {
    await screen.findByText("ada@example.com");
    return screen.getByText("Seat").nextElementSibling as HTMLElement;
  }

  // WRITTEN AS A SEAT: the name and its `@handle`, linked to the seat's page.
  // It was the name and the bare handle — "Bob bob". Mutation: drop the `@`.
  test("a reader the seat's page serves sees the seat's name, its @handle, and a link", async () => {
    engine();
    mount({
      name: "Nimbus",
      roles: [{ name: "Ada Lovelace", handle: "ada", kind: "human" }],
      units: [],
    });
    const seat = await seatFact();
    await waitFor(() =>
      expect(within(seat).getByRole("link", { name: "Ada Lovelace" }).getAttribute("href")).toBe(
        href(["company", "org", "seats", "ada"]),
      ),
    );
    expect(within(seat).getByText("@ada")).toBeDefined();
  });

  // A READER THE ENGINE REFUSED THE LIVE SOCKET — a seat taken out of the
  // chart — has no chart to name the seat by, so it is the handle alone, and
  // the seat's page is the panel refusing them the company, so it is no link.
  // The CONTROL is the same reader holding no app and served: every signed-in
  // principal reads the overview, so the seat links. Mutation: link it
  // whatever the socket said, and the refused half goes red.
  test("a reader the seat's page does not serve sees the handle alone, unlinked", async () => {
    engine({ session: { ...PERSON, grants: [] } });
    mount(undefined, "your seat is no longer in the chart");
    const seat = await seatFact();
    expect(seat.textContent).toBe("@ada");
    expect(within(seat).queryByRole("link")).toBeNull();
    cleanup();

    engine({ session: { ...PERSON, grants: [] } });
    mount();
    const served = await seatFact();
    expect(within(served).getByRole("link", { name: "@ada" }).getAttribute("href")).toBe(
      href(["company", "org", "seats", "ada"]),
    );
  });

  /** The words each access tag under a term says, in order. */
  const tagWords = (el: HTMLElement) =>
    [...el.querySelectorAll(".crewlet-tag")].map((n) => n.textContent);

  // "YOUR ACCESS" IS THE DIRECTORY ROW'S, said in the People list's words:
  // the apps by name, Admin alone where it is held (it includes every app), and
  // "Overview only" for none. The row wins over the session because it is what
  // an administrator changes, and the session's copy is whatever it was when
  // this browser signed in. Mutations: delete the row and the first half goes
  // red; read `session.grants` and the second does.
  test("your access is the directory row's, said as the People list says it", async () => {
    engine();
    mount();
    await screen.findByText("ada@example.com");
    expect(tagWords(screen.getByText("Access").nextElementSibling as HTMLElement)).toEqual([
      "Worklet",
    ]);
    cleanup();

    engine({ session: { ...PERSON, grants: ["worklet"] }, row: { ...ROW, grants: [] } });
    mount();
    await screen.findByText("ada@example.com");
    expect(tagWords(screen.getByText("Access").nextElementSibling as HTMLElement)).toEqual([
      "Overview only",
    ]);
    cleanup();

    engine({ row: { ...ROW, grants: ["admin", "worklet", "doclet"] } });
    mount();
    await screen.findByText("ada@example.com");
    expect(tagWords(screen.getByText("Access").nextElementSibling as HTMLElement)).toEqual([
      "Admin",
    ]);
    // AN ADMIN IS SENT TO THEIR OWN ROW, where these are changed.
    expect(screen.queryByText("Only an admin can change these.")).toBeNull();
    expect(screen.getByRole("link", { name: "People" }).getAttribute("href")).toContain("p-1");
  });

  // A CREDENTIAL THAT IS NOT A PERSON SAYS ITS OWN ACCESS too, from the
  // session it is — it has no directory row to read it from.
  test("a Tier A token's session says its access", async () => {
    engine({ session: TIER_A });
    mount();
    const label = await screen.findByText("Access");
    expect(tagWords(label.parentElement as HTMLElement)).toEqual(["Admin"]);
  });

  // AT THE CARD'S OWN INSET: in a flush card the first fact touched the
  // header's rule, so the list sits in the card's body. It is the kit's
  // DescriptionList, which carries no padding of its own to double the edge.
  // Mutation: `padding="none"` on the card, or a local list in its place.
  test("the facts sit in the card's body, at its inset", async () => {
    engine();
    mount();
    const seat = await seatFact();
    expect(seat.closest(".crewlet-card__body")).not.toBeNull();
    expect(seat.closest("dl.crewlet-description-list")).not.toBeNull();
  });
});

describe("while the engine has not said who this is", () => {
  // THE READER'S OWN PAGE IS NEVER BLANK: the note waits for the session it
  // describes, and `QueryState` draws nothing for a read still out, so the
  // page drew nothing at all for as long as `GET /auth/session` took. The
  // CONTROL is every case above, whose read answers. Mutation: hand the
  // pending read to `QueryState` and the page is empty.
  test("says it is reading, and draws no note until it knows which", () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(() => new Promise<Response>(() => {})),
    );
    mount();
    expect(screen.getByText("Reading who you are signed in as")).toBeDefined();
    expect(screen.queryByText(/Your profile, security|Signed in with an API token/)).toBeNull();
  });
});

describe("the page's note", () => {
  // THE NOTE DESCRIBES THE SESSION IT SHOWS: the person's sentence, promising
  // a password and a second factor, stood above the card telling a Tier A
  // token's session it has neither. The CONTROL is a person's page, which
  // carries it. Mutation: draw the person's note for every session.
  test("a person's note promises their own credentials, and a token's does not", async () => {
    engine();
    mount();
    expect(await screen.findByText("Your profile, security, sessions and tokens.")).toBeDefined();
    cleanup();
    engine({ session: TIER_A });
    mount();
    expect(
      await screen.findByText("Signed in with an API token. Ends within an hour."),
    ).toBeDefined();
    expect(screen.queryByText(/Your profile, security/)).toBeNull();
  });
});

describe("a credential that is not a person's", () => {
  // THE CONTROL is every case above, which reads the directory.
  test("a Tier A token's session is told what it is, and asks the directory nothing", async () => {
    const engineIs = engine({ session: TIER_A });
    mount();
    expect(await screen.findByText(/You’re signed in with the API token/)).toBeDefined();
    expect(screen.getByText("API token")).toBeDefined();
    expect(screen.getByText("ops")).toBeDefined();
    expect(screen.queryByLabelText("Current password")).toBeNull();
    expect(screen.queryByRole("button", { name: "New token" })).toBeNull();
    expect(engineIs.sent.filter((s) => s.path.startsWith("/iam/"))).toEqual([]);
  });

  // A TOKEN BOUND TO A HUMAN SEAT acts as that seat's holder, so the engine
  // answers its session `kind: "person"` with the seat beside it, and an id
  // derived from the token rather than a directory row's. Read by its kind,
  // the page asked `/iam/people/<that id>` and drew "Not found" where the
  // profile goes. Mutation: decide on `kind` alone and the directory is asked.
  test("a Tier A token bound to a seat is still a token's session, not a missing person", async () => {
    const engineIs = engine({ session: { ...TIER_A, kind: "person", seat: "miles" } });
    mount();
    expect(await screen.findByText(/You’re signed in with the API token/)).toBeDefined();
    expect(screen.getByText("Signed in with an API token. Ends within an hour.")).toBeDefined();
    expect(screen.queryByText("Not found")).toBeNull();
    expect(engineIs.sent.filter((s) => s.path.startsWith("/iam/"))).toEqual([]);
  });
});
