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

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { Secrets } from "./Secrets.tsx";

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

beforeEach(() => {
  localStorage.setItem("crewlet_api_token", "operator-token");
});

afterEach(() => {
  cleanup();
  localStorage.clear();
  vi.restoreAllMocks();
});

test("the list comes from GET /secrets, carrying the operator token", async () => {
  const spy = stubFetch((path) => (path === "/secrets" ? ok(body) : ok({})));
  render(<Secrets />);

  expect(await screen.findByText("DATADOG_WEBHOOK_TOKEN")).toBeDefined();
  expect(screen.getByText("GITHUB_TOKEN")).toBeDefined();

  const init = spy.mock.calls[0]?.[1];
  const headers = (init?.headers ?? {}) as Record<string, string>;
  expect(headers.Authorization).toBe("Bearer operator-token");
});

// NO VALUE, EVER. The one route that returns one needs an explicit flag and
// logs the access; a dashboard that anyone holding the token can open is not
// where that trade gets made. This asserts the screen never asks.
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
test("a refused read says so instead of showing an empty company", async () => {
  stubFetch(() => new Response(JSON.stringify({ error: "invalid_token" }), { status: 401 }));
  render(<Secrets />);

  expect(await screen.findByText(/needs an operator token/)).toBeDefined();
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

  await vi.waitFor(() => {
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

  await vi.waitFor(() => {
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

  await vi.waitFor(() => {
    const write = spy.mock.calls.find(([, init]) => init?.method === "PUT");
    expect(String(write?.[0])).toContain("/secrets/GITHUB_TOKEN");
  });
  // NOTHING WAS REVEALED to fill that field in.
  for (const [input] of spy.mock.calls) {
    expect(String(input)).not.toContain("reveal");
  }
});
