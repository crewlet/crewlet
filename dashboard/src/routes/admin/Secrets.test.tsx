/**
 * The Secrets screen reads the routes that answer, writes without ever reading
 * a value back, and says what a credential is holding up before it goes.
 *
 * It asked the socket for `config_entities {kind: "secrets"}`. That kind does
 * not exist — the entity kinds are roles, units, llm-providers and
 * mcp-servers — so the engine answered an unknown-kind error every time and
 * the table could never hold a row. The screen looked like a company with no
 * credentials, which is exactly what an operator would conclude.
 *
 * The invariants worth breaking a build over, in order of how much they cost
 * when they go: no value ever reaches the page; a value is written as the
 * request BODY rather than wrapped in JSON; and a removal says which config
 * fields point at the name — with "the check did not answer" never rendered
 * as "nothing points at it".
 */

import { act, cleanup, fireEvent, poll, render as rtlRender, screen } from "~/test/inCase.ts";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { CredentialPeek, Secrets } from "./Secrets.tsx";
import type { ReactElement } from "react";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";

/**
 * The client the screen renders under. The lists are REST reads, but they
 * read again when the live socket comes back — what their `closed` banner
 * promises — so the case that holds that moves this store's connection.
 * Nothing dials.
 */
let store = new Store();

beforeEach(() => {
  store = new Store();
});

/**
 * A screen renders inside the Router, under the client.
 *
 * The grid every list is drawn with keeps its sort and its visible columns in
 * the URL — see `app/frame/DataGrid.tsx` — so it reads the route, and a bare
 * `render()` throws "useRoute outside a Router". Wrapping here rather than in
 * every case keeps each assertion about the screen.
 */
function wrapped(ui: ReactElement): ReactElement {
  return (
    <ClientContext.Provider value={{ store, socket: new LiveSocket(store) }}>
      <Router>{ui}</Router>
    </ClientContext.Provider>
  );
}

function render(ui: ReactElement) {
  return rtlRender(wrapped(ui));
}

// The shape internal/api/secretsapi writes: names, provenance, key id. There
// is deliberately no `value` field on this route at all.
const body = {
  secrets: [
    {
      name: "DATADOG_WEBHOOK_TOKEN",
      key_id: "k1",
      updated_at: "2026-08-23T15:00:00Z",
      updated_by: "founder",
      source: "setup",
    },
    {
      name: "GITHUB_TOKEN",
      key_id: "k1",
      updated_at: "2026-08-22T15:00:00Z",
      updated_by: "ops",
      source: "store",
    },
  ],
};

/**
 * What `GET /config/references` answers: which fields of the active company
 * document name a ${VAR}. GITHUB_TOKEN has two readers on purpose — one
 * credential with several pointers is the shape the removal confirmation
 * exists for — and DATADOG_WEBHOOK_TOKEN has none.
 */
const references = {
  revision: "rev-1",
  references: [
    { path: "integrations.github.token", name: "GITHUB_TOKEN" },
    { path: "roles[0].mcp_env.github.GITHUB_TOKEN", name: "GITHUB_TOKEN" },
  ],
};

function stubFetch(handler: (path: string, init?: RequestInit) => Response) {
  const spy = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
    const path = new URL(String(input), "http://engine.test").pathname;
    return Promise.resolve(handler(path, init));
  });
  Object.defineProperty(globalThis, "fetch", { writable: true, value: spy });
  return spy;
}

/** The two reads the screen makes on load, both answering. */
function stubLoaded(extra: (path: string, init?: RequestInit) => Response | null = () => null) {
  return stubFetch((path, init) => {
    const answer = extra(path, init);
    if (answer) return answer;
    if (path === "/secrets") return ok(body);
    if (path === "/config/references") return ok(references);
    return ok({});
  });
}

/** Opens the removal confirmation for one row of the table. */
async function openRemove(name: string) {
  fireEvent.click(await screen.findByRole("button", { name: `Remove ${name}` }));
  return screen.findByRole("dialog", { name: `Remove ${name}` });
}

function ok(payload: unknown): Response {
  return new Response(JSON.stringify(payload), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  // HERE as well as in each case's own `finally`, which a case that overran
  // its budget never reaches: its fake clock then stayed installed, and every
  // case after it in the file failed on a clock that never moved — one
  // overrun reported as a dozen failures, the first of them the only true one.
  vi.useRealTimers();
});

// THE SESSION COOKIE IS THE CREDENTIAL, which the browser attaches and no
// script reads — so the request carries no `Authorization` header of its own,
// and nothing in storage is where one could have come from.
test("the list comes from GET /secrets, on the session and no header of its own", async () => {
  const spy = stubFetch((path) => (path === "/secrets" ? ok(body) : ok({})));
  render(<Secrets />);

  expect(await screen.findByText("DATADOG_WEBHOOK_TOKEN")).toBeDefined();
  expect(screen.getByText("GITHUB_TOKEN")).toBeDefined();

  const init = spy.mock.calls[0]?.[1];
  const headers = (init?.headers ?? {}) as Record<string, string>;
  expect(headers.Authorization).toBeUndefined();
  expect(init?.credentials).toBe("same-origin");
});

// NO VALUE, EVER. The one route that returns one needs an explicit flag and
// logs the access; a dashboard that anyone signed in can open is not where
// that trade gets made. This asserts the screen never asks.
test("the screen never asks for a value", async () => {
  const spy = stubFetch((path) => (path === "/secrets" ? ok(body) : ok({})));
  render(<Secrets />);
  await screen.findByText("DATADOG_WEBHOOK_TOKEN");

  const paths = spy.mock.calls.map(([input]) => String(input));
  for (const path of paths) {
    expect(path).not.toContain("reveal");
    // And never a single-secret read, which is the only route that can
    // return one at all.
    expect(path).not.toMatch(/\/secrets\/.+/);
  }
});

// A refused read keeps the last good list rather than claiming the company
// holds nothing. On the first load there is no last good list, so what it
// must not do is render an empty table as if that were an answer.
//
// AND IT REPORTS THE CODE, NOT A SENTENCE. `QueryState` is a table over
// `QueryErrorCode`; handing it prose missed the table on every refusal and
// rendered the "code this build does not know" banner — with no way to sign
// in, which the same banner exists to offer. This screen is guarded reads
// included, so a 401 is the refusal an operator actually arrives at.
test("a 401 renders the refusal banner, with the button that fixes it", async () => {
  stubFetch(() => new Response(JSON.stringify({ error: "invalid_token" }), { status: 401 }));
  render(<Secrets />);

  expect(await screen.findByText(/does not carry the grant/)).toBeDefined();
  expect(screen.getByRole("button", { name: "Sign in" })).toBeDefined();
  expect(screen.queryByText(/code this build does not know/)).toBeNull();
});

// AND EVERY OTHER REFUSAL TOO, which is the half a single 401 case would let
// rot: an engine fault is the node's, not the reader's, and it is a different
// sentence from a missing token.
test("a failed read is reported as a fault on the node, not as an unknown code", async () => {
  stubFetch((path) =>
    path === "/secrets"
      ? new Response(JSON.stringify({ error: "internal_error" }), { status: 500 })
      : ok({}),
  );
  render(<Secrets />);

  expect(await screen.findByText(/tried to answer and failed/)).toBeDefined();
  expect(screen.queryByText(/code this build does not know/)).toBeNull();
});

/**
 * A `503` THE ENGINE WROTE IS A NODE THAT CANNOT ANSWER HERE, NOT A FAULT —
 * and nothing polls this screen, so it is read again when the engine says.
 *
 * It was drawn as "the engine tried to answer and failed — its log says what
 * went wrong", which sent an operator watching a node catch up to read a log
 * that held no fault, and the screen never asked again until somebody
 * reloaded. The `503`'s `Retry-After` is when; with none, the engine is saying
 * waiting will not change it, so the screen says so and asks nothing.
 */
function unavailableOnce(headers: Record<string, string>, asked: number[] = []) {
  let refused = false;
  return stubFetch((path) => {
    // WHEN each listing read went out, on the fake clock: `poll` moves
    // that clock while it polls, so a wait is held as the gap between two
    // reads, never as a count at an absolute instant.
    if (path === "/secrets") asked.push(Date.now());
    if (path === "/secrets" && !refused) {
      refused = true;
      return new Response(
        JSON.stringify({
          error: "identity_unavailable",
          detail: "this node could not read the identity estate",
        }),
        { status: 503, headers: { "Content-Type": "application/json", ...headers } },
      );
    }
    if (path === "/secrets") return ok(body);
    return ok(references);
  });
}

const listReads = (spy: ReturnType<typeof stubFetch>) =>
  spy.mock.calls.filter(
    ([input]) => new URL(String(input), "http://e.test").pathname === "/secrets",
  ).length;

test("a 503 is a node that cannot answer yet, read again when its Retry-After says", async () => {
  vi.useFakeTimers();
  try {
    const asked: number[] = [];
    const spy = unavailableOnce({ "Retry-After": "12" }, asked);
    render(<Secrets />);
    await poll(() => expect(screen.getByText(/cannot answer yet/)).toBeDefined());
    expect(screen.queryByText(/tried to answer and failed/)).toBeNull();
    expect(listReads(spy)).toBe(1);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(12_000);
    });
    expect(listReads(spy)).toBe(2);
    // WHEN IT SAID: a node twelve seconds behind is asked at twelve, never at
    // any cadence of this screen's own, sooner or later. In whole seconds,
    // because the gap is measured from the request and the wait starts when
    // its answer lands, which `poll`'s own clock steps can follow.
    expect(Math.floor((asked[1]! - asked[0]!) / 1_000)).toBe(12);
    await poll(() => expect(screen.getByText("GITHUB_TOKEN")).toBeDefined());
  } finally {
    vi.useRealTimers();
  }
});

test("a 503 with no Retry-After says the node refused, and is not read again", async () => {
  vi.useFakeTimers();
  try {
    const spy = unavailableOnce({});
    render(<Secrets />);
    await poll(() =>
      expect(screen.getByText(/asking it again will not change that/)).toBeDefined(),
    );
    await act(async () => {
      await vi.advanceTimersByTimeAsync(120_000);
    });
    expect(listReads(spy)).toBe(1);
  } finally {
    vi.useRealTimers();
  }
});

/**
 * A CLOSED PEEK ASKS NOTHING MORE, an answer still in flight when it closed
 * included.
 *
 * The rail's read is armed by its answer: a `503` that lands says when to read
 * again. Closing the rail cancelled what was already armed, but an answer that
 * landed AFTER it closed still armed its re-read — and that re-read, refused
 * again, armed the next — so a rail nobody had open went on reading `/secrets`
 * every few seconds for as long as the node refused, rendering into nothing.
 */
test("a peek closed while its read is in flight is not read again", async () => {
  vi.useFakeTimers();
  try {
    let reads = 0;
    Object.defineProperty(globalThis, "fetch", {
      writable: true,
      value: vi.fn(async (input: RequestInfo | URL) => {
        const path = new URL(String(input), "http://engine.test").pathname;
        if (path !== "/secrets") return ok(references);
        reads++;
        // ANSWERED A SECOND LATER, by which time the rail has closed.
        await new Promise((resolve) => setTimeout(resolve, 1_000));
        return new Response(JSON.stringify({ error: "identity_unavailable" }), {
          status: 503,
          headers: { "Content-Type": "application/json", "Retry-After": "2" },
        });
      }),
    });
    const view = render(<CredentialPeek name="GITHUB_TOKEN" />);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(reads).toBe(1);

    view.rerender(wrapped(<CredentialPeek name="" />));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(60_000);
    });
    expect(reads).toBe(1);
  } finally {
    vi.useRealTimers();
  }
});

/**
 * A LIST NO ANSWER CAME BACK TO IS SAID AS THAT, AND READ AGAIN ON ITS OWN —
 * with the live socket up the whole time, which is the ordinary case: a
 * request past its thirty-second deadline on a slow engine.
 *
 * It was drawn `closed`, "the connection went away; the screen reads again
 * once the socket is back". The socket had not gone anywhere, and nothing
 * polls this surface, so the banner stood — false twice over — until somebody
 * reloaded. Its read backs off now, a second after the deadline first; the
 * rest of the schedule is `lib/restRead.test.tsx`'s.
 *
 * THE FAKE CLOCK IS MOVED ONLY AS FAR AS THE NEXT ANSWER, never past it: the
 * screen re-renders on the shared once-a-second clock, so every fake second
 * is a render of the whole table once the list has arrived. Moved two and a
 * half minutes past it, this case took five seconds under load and its fake
 * clock outlived it.
 */
test("a list past its deadline with the socket up is said truthfully, and read again", async () => {
  vi.useFakeTimers();
  try {
    act(() => store.setConnected(true));
    const asked: number[] = [];
    stubFetch((path, init) => {
      if (path !== "/secrets") return ok(references);
      asked.push(Date.now());
      if (asked.length > 1) return ok(body);
      // NEVER ANSWERED: the request's own deadline is what ends it, by
      // aborting the fetch as a browser's would be.
      return new Promise<Response>((_resolve, reject) => {
        init?.signal?.addEventListener("abort", () =>
          reject(new DOMException("aborted", "AbortError")),
        );
      }) as unknown as Response;
    });
    render(<Secrets />);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(30_000);
    });
    expect(screen.getByText(/No answer from the engine reached this page/)).toBeDefined();
    expect(screen.queryByText(/connection went away/)).toBeNull();

    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_000);
    });
    expect(asked.slice(1).map((t, i) => Math.floor((t - asked[i]!) / 1_000))).toEqual([31]);
    expect(screen.getByText("GITHUB_TOKEN")).toBeDefined();
  } finally {
    vi.useRealTimers();
  }
});

// AND THE SOCKET COMING BACK reads it at once, ahead of the backoff: it is the
// engine being reachable again.
test("a list that never arrived is read again the moment the socket comes back", async () => {
  vi.useFakeTimers();
  try {
    let failed = false;
    const spy = stubFetch((path) => {
      if (path === "/secrets" && !failed) {
        failed = true;
        throw new TypeError("Failed to fetch");
      }
      if (path === "/secrets") return ok(body);
      return ok(references);
    });
    render(<Secrets />);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(500);
    });
    expect(screen.getByText(/No answer from the engine reached this page/)).toBeDefined();
    expect(listReads(spy)).toBe(1);

    act(() => store.setConnected(true));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(listReads(spy)).toBe(2);
    expect(screen.getByText("GITHUB_TOKEN")).toBeDefined();
  } finally {
    vi.useRealTimers();
  }
});

// THE VALUE IS THE BODY, not a field of a document. `PUT /secrets/{name}`
// takes the credential as raw bytes because a credential is arbitrary text,
// and sending it through the JSON writer would seal the quotes into it — a
// 401 from the vendor with nothing to explain it.
test("a stored value goes as the request body rather than wrapped in JSON", async () => {
  const spy = stubLoaded((path, init) =>
    path === "/secrets/NEW_TOKEN" && init?.method === "PUT" ? ok({ name: "NEW_TOKEN" }) : null,
  );
  render(<Secrets />);

  fireEvent.click(await screen.findByRole("button", { name: "Store a secret" }));
  fireEvent.change(screen.getByLabelText("Name"), { target: { value: "NEW_TOKEN" } });
  fireEvent.change(screen.getByLabelText("Value"), { target: { value: 'glpat-"quoted"' } });
  fireEvent.click(screen.getByRole("button", { name: "Store" }));

  await poll(() => {
    const write = spy.mock.calls.find(([, init]) => init?.method === "PUT");
    expect(write).toBeDefined();
    expect(String(write?.[0])).toContain("/secrets/NEW_TOKEN");
    expect(write?.[1]?.body).toBe('glpat-"quoted"');
  });
});

// A SECRET IS A PASSWORD FIELD. The type is what keeps the value out of an
// autofill store and out of a screenshot, and it is the one property of this
// form that is a security property rather than a nicety.
test("the value is a password field and the form offers no autofill", async () => {
  stubLoaded();
  render(<Secrets />);

  fireEvent.click(await screen.findByRole("button", { name: "Store a secret" }));
  const value = screen.getByLabelText("Value");
  expect(value.getAttribute("type")).toBe("password");
  expect(value.getAttribute("autocomplete")).toBe("off");
});

// THE ENGINE JUDGES THE NAME, not a regex written here. A second copy of the
// ${VAR} grammar in the client is the bug this project has already paid for
// twice, so a bad name reaches the engine and its refusal is what the
// operator reads.
test("a name the engine refuses is reported in the engine's own words", async () => {
  stubLoaded((path, init) =>
    init?.method === "PUT"
      ? new Response(
          JSON.stringify({
            error: "invalid_name",
            detail: 'secrets: "gitlab-token" is not an environment-variable name',
          }),
          { status: 400 },
        )
      : null,
  );
  render(<Secrets />);

  fireEvent.click(await screen.findByRole("button", { name: "Store a secret" }));
  fireEvent.change(screen.getByLabelText("Name"), { target: { value: "gitlab-token" } });
  fireEvent.change(screen.getByLabelText("Value"), { target: { value: "glpat-x" } });
  fireEvent.click(screen.getByRole("button", { name: "Store" }));

  expect(await screen.findByText(/not an environment-variable name/)).toBeDefined();
});

// THE SHARP EDGE. The config holds ${VAR} pointers, so removing a row nothing
// checked leaves each pointer resolving to nothing and the surfaces holding
// one refusing every delivery. Both readers of one name have to be on screen
// before the operator can go through with it.
test("a removal names every config field that points at the secret", async () => {
  stubLoaded();
  render(<Secrets />);
  await openRemove("GITHUB_TOKEN");

  expect(screen.getByText("integrations.github.token")).toBeDefined();
  expect(screen.getByText("roles[0].mcp_env.github.GITHUB_TOKEN")).toBeDefined();
  // AND IT IS GATED. A referenced credential does not go on one click.
  const remove = screen.getByRole("button", { name: "Remove" });
  expect((remove as HTMLButtonElement).disabled).toBe(true);
  fireEvent.click(screen.getByRole("checkbox"));
  expect((remove as HTMLButtonElement).disabled).toBe(false);
});

// A row nothing points at is an ordinary delete: no acknowledgement, and the
// engine is asked exactly once.
test("a secret nothing reads is removed without a second gesture", async () => {
  const spy = stubLoaded((path, init) =>
    init?.method === "DELETE" ? ok({ name: "DATADOG_WEBHOOK_TOKEN", removed: true }) : null,
  );
  render(<Secrets />);
  await openRemove("DATADOG_WEBHOOK_TOKEN");

  expect(screen.queryByRole("checkbox")).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Remove" }));

  await poll(() => {
    const gone = spy.mock.calls.find(([, init]) => init?.method === "DELETE");
    expect(gone).toBeDefined();
    expect(String(gone?.[0])).toContain("/secrets/DATADOG_WEBHOOK_TOKEN");
  });
});

// THE ONE THAT MUST NEVER REGRESS. "The check did not answer" and "nothing
// points at it" are different facts, and rendering the first as the second
// would be a reassurance nobody could have earned — the operator would
// believe it, because it looks exactly like the safe case.
test("a reference check that failed is never shown as nothing pointing at it", async () => {
  stubFetch((path) => {
    if (path === "/secrets") return ok(body);
    if (path === "/config/references") {
      return new Response(JSON.stringify({ error: "internal_error" }), { status: 500 });
    }
    return ok({});
  });
  render(<Secrets />);
  await openRemove("GITHUB_TOKEN");

  expect(screen.getByText(/could not be read/)).toBeDefined();
  expect(screen.queryByText(/No field in the active configuration names this/)).toBeNull();
  // And it is gated exactly as a referenced row is: an unknown answer is not
  // a safe one.
  expect((screen.getByRole("button", { name: "Remove" }) as HTMLButtonElement).disabled).toBe(true);
});

// AND NOT THE LAST ANSWER EITHER. Every other read on this screen keeps what it
// held through a failed re-read; this one must not, because the index from
// before the failure is an old answer to "what breaks if this goes", and drawn
// as the current one it lets a removal through with no warning about a field
// that names the row now.
test("a reference check that fails after one that answered is unknown, not the old answer", async () => {
  let checks = 0;
  stubFetch((path) => {
    if (path === "/secrets") return ok(body);
    if (path === "/config/references") {
      checks++;
      return checks === 1
        ? ok(references)
        : new Response(JSON.stringify({ error: "internal_error" }), { status: 500 });
    }
    return ok({});
  });
  render(<Secrets />);
  expect(await screen.findByText("DATADOG_WEBHOOK_TOKEN")).toBeDefined();
  await poll(() => expect(checks).toBe(1));

  // THE SOCKET COMING BACK reads both again, and this time the check fails.
  act(() => store.setConnected(true));
  await poll(() => expect(checks).toBe(2));
  await openRemove("DATADOG_WEBHOOK_TOKEN");
  expect(await screen.findByText(/could not be read/)).toBeDefined();
  expect(screen.queryByText(/No field in the active configuration names this/)).toBeNull();
});

// A REFUSED CHECK NAMES THE GRANT THE ENGINE NAMED. The reference index is
// `config:read`, and a reader who may list credentials without it is refused
// with the grant in the answer — which is the remedy, where "this surface needs
// an operator token" sent a signed-in person to find a token.
test("a reference check refused on authority names the grant it needs", async () => {
  stubFetch((path) => {
    if (path === "/secrets") return ok(body);
    if (path === "/config/references") {
      return new Response(
        JSON.stringify({ error: "unauthorized", reason: "no_grant", grants: ["config:read"] }),
        { status: 403 },
      );
    }
    return ok({});
  });
  render(<Secrets />);
  await openRemove("GITHUB_TOKEN");

  expect(screen.getByText(/This needs config:read/)).toBeDefined();
  expect(screen.queryByText(/operator token/)).toBeNull();
});

// A DEPLOYMENT BEFORE ITS FIRST IMPORT has no active document, so nothing can
// be pointing at anything. That is an answer, not a failed check, and
// treating it as one would put a warning in front of every removal on a new
// install.
test("no active configuration reads as nothing pointing at it, not as a failure", async () => {
  stubFetch((path) => {
    if (path === "/secrets") return ok(body);
    if (path === "/config/references") {
      return new Response(JSON.stringify({ error: "no_active_revision" }), { status: 404 });
    }
    return ok({});
  });
  render(<Secrets />);
  await openRemove("GITHUB_TOKEN");

  expect(screen.getByText(/No field in the active configuration names this/)).toBeDefined();
  expect((screen.getByRole("button", { name: "Remove" }) as HTMLButtonElement).disabled).toBe(
    false,
  );
});

// Editing a secret does not read the old value back (the field opens empty)
// and the write still goes to the name the row already has.
test("editing a secret asks for a new value and never receives the old one", async () => {
  const spy = stubLoaded((path, init) =>
    init?.method === "PUT" ? ok({ name: "GITHUB_TOKEN" }) : null,
  );
  render(<Secrets />);

  fireEvent.click(await screen.findByRole("button", { name: "Edit GITHUB_TOKEN" }));
  expect((screen.getByLabelText("New value") as HTMLInputElement).value).toBe("");
  fireEvent.change(screen.getByLabelText("New value"), { target: { value: "ghp-rotated" } });
  fireEvent.click(screen.getByRole("button", { name: "Save" }));

  await poll(() => {
    const write = spy.mock.calls.find(([, init]) => init?.method === "PUT");
    expect(String(write?.[0])).toContain("/secrets/GITHUB_TOKEN");
  });
  // NOTHING WAS REVEALED to fill that field in.
  for (const [input] of spy.mock.calls) {
    expect(String(input)).not.toContain("reveal");
  }
});

/**
 * AN ANSWER NOBODY IS WAITING FOR ANY MORE WRITES NOTHING.
 *
 * Three things start a read here — mount, a token arriving, and a store or a
 * removal finishing — and nothing polls, so two are routinely in flight at
 * once. Every answer was written into state unconditionally, which makes the
 * screen hold whichever LANDED last rather than whichever was ASKED last: a
 * refresh started after a slow read finishes second and is overwritten by the
 * list it was meant to replace.
 *
 * The other half of the same guard is what was actually failing, and it is
 * not observable from here. A read that outlives its SCREEN set state against
 * a torn-down document — in a test run, an unhandled `ReferenceError: window
 * is not defined` out of React's own dispatch, every case passing and the run
 * still exiting non-zero. It cannot be asserted in jsdom, where React simply
 * drops an update to an unmounted component and `window` is still there; the
 * failure needs the environment itself to be gone. So this case exercises the
 * generation guard through the half that IS observable, and the unmount half
 * rides the identical `generation.current !== mine` check.
 */
test("a read that answers after a newer one began does not overwrite it", async () => {
  const stale = {
    secrets: [{ ...body.secrets[0], name: "STALE_ANSWER", key_id: "k0" }],
  };
  // THE FIRST READ IS HELD until the second has answered, which is the
  // ordering the guard exists for and the one a slow route produces.
  let releaseFirst: (() => void) | undefined;
  let reads = 0;
  const held = new Promise<void>((resolve) => {
    releaseFirst = resolve;
  });
  Object.defineProperty(globalThis, "fetch", {
    writable: true,
    value: vi.fn(async (input: RequestInfo | URL) => {
      const path = new URL(String(input), "http://engine.test").pathname;
      if (path === "/config/references") return ok(references);
      if (path !== "/secrets") return ok({});
      reads += 1;
      if (reads === 1) {
        await held;
        return ok(stale);
      }
      return ok(body);
    }),
  });

  render(<Secrets />);
  // A STORED SECRET starts the second read, which is one of the two real
  // triggers rather than a lever invented for this case: the button is on
  // screen while the first read is still out.
  fireEvent.click(await screen.findByRole("button", { name: "Store a secret" }));
  fireEvent.change(screen.getByLabelText("Name"), { target: { value: "NEW_TOKEN" } });
  fireEvent.change(screen.getByLabelText("Value"), { target: { value: "glpat-x" } });
  fireEvent.click(screen.getByRole("button", { name: "Store" }));
  expect(await screen.findByText("GITHUB_TOKEN")).toBeTruthy();

  // NOW the first read answers, and the screen must ignore it.
  //
  // FLUSHED INSIDE `act`, because the assertion below is an ABSENCE and an
  // absence asserted too early passes for the wrong reason. Waiting on the
  // fetch COUNT is exactly that mistake — it counts requests STARTED, so it
  // was already 2 before the held answer had been handled at all, and this
  // case passed with every guard deleted. Releasing and then letting React
  // process the resolution is what makes the absence mean something.
  await act(async () => {
    releaseFirst?.();
    await new Promise((resolve) => setTimeout(resolve, 20));
  });
  expect(reads).toBe(2);
  expect(screen.queryByText("STALE_ANSWER")).toBeNull();
  expect(screen.getByText("GITHUB_TOKEN")).toBeTruthy();
});
