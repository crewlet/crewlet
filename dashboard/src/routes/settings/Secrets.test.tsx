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

import { act, cleanup, fireEvent, render as rtlRender, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { CredentialPeek, provenance, Secrets } from "./Secrets.tsx";
import type { ReactElement } from "react";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";

/**
 * A screen renders inside the Router.
 *
 * The grid every list is drawn with keeps its sort and its visible columns in
 * the URL — see `app/frame/DataGrid.tsx` — so it reads the route, and a bare
 * `render()` throws "useRoute outside a Router". Wrapping here rather than in
 * every case keeps each assertion about the screen.
 */
function render(ui: ReactElement) {
  return rtlRender(<Router>{ui}</Router>);
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
      source: "api",
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
  localStorage.clear();
  vi.restoreAllMocks();
});

// THE BROWSER'S OWN SESSION and nothing else: the cookie rides a same-origin
// request, and no bearer is ever set by a page — a credential a page held
// was one any script on it could read.
test("the list comes from GET /secrets, on the browser's own session", async () => {
  const spy = stubFetch((path) => (path === "/secrets" ? ok(body) : ok({})));
  render(<Secrets />);

  expect(await screen.findByText("DATADOG_WEBHOOK_TOKEN")).toBeDefined();
  expect(screen.getByText("GITHUB_TOKEN")).toBeDefined();

  const init = spy.mock.calls[0]?.[1];
  const headers = (init?.headers ?? {}) as Record<string, string>;
  expect(init?.credentials).toBe("same-origin");
  expect(headers.Authorization).toBeUndefined();
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
//
// AND IT REPORTS THE CODE, NOT A SENTENCE. `QueryState` is a table over
// `QueryErrorCode`; handing it prose missed the table on every refusal and
// rendered the "code this build does not know" banner — with no way to sign
// in that the same banner exists to offer. This screen is guarded reads
// included, so a refusal on authority is what a reader actually arrives at,
// and it names the grant that would have admitted them.
test("a refusal on authority names the grant, with the button that fixes it", async () => {
  stubFetch(
    () =>
      new Response(
        JSON.stringify({ error: "unauthorized", reason: "no_grant", grants: ["config:read"] }),
        { status: 403 },
      ),
  );
  render(<Secrets />);

  expect(await screen.findByText(/the answer needs/)).toBeDefined();
  expect(screen.getByText("config:read")).toBeDefined();
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

/**
 * AN ANSWER NOBODY IS WAITING FOR ANY MORE WRITES NOTHING.
 *
 * Three things start a read here — mount, the socket coming back, and a store
 * or a removal finishing — and nothing polls, so two are routinely in flight
 * at once. Every answer was written into state unconditionally, which makes the
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

  const store = new Store();
  store.setConnected(true);
  render(
    <ClientContext.Provider value={{ store, socket: new LiveSocket(store) }}>
      <Secrets />
    </ClientContext.Provider>,
  );
  // THE SOCKET COMING BACK starts the second read, which is one of the three
  // real triggers rather than a lever invented for this case.
  act(() => store.setConnected(false));
  act(() => store.setConnected(true));
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

// A COMMAND IS CODE. The info card above the list sets `crewlet secrets get`
// in the code face, and the empty state under it wrote `crewlet secrets set`
// as three words of its sentence — the one a reader is being told to type.
test("an empty store names the command that fills it as code", async () => {
  stubFetch((path) => (path === "/secrets" ? ok({ secrets: [] }) : ok({})));
  render(<Secrets />);
  expect(await screen.findByText("No secrets are stored")).toBeDefined();
  const command = screen.getByText("crewlet secrets set");
  expect(command.tagName).toBe("CODE");
});

// THE NAME IS THE TABLE'S ONE FLEXIBLE TRACK. Name and Set by were both `1fr`,
// so they split the spare width: at a 1280 window "DATADOG_WEBHOOK_TOK…" sat
// cut beside "founder" in ~120px of air, and beside a peek at 1440 every name
// fell to "DAT…". The name is what a reader matches against the `${VAR}` in
// their configuration, so it takes the spare width and never gives up a
// character; every fact beside it sizes to its content.
// THE TILES CLAIM NOTHING ABOUT WHERE A VALUE LIVES. One counted rows whose
// source read "store", a word no writer stamps — `source` is which path wrote
// a row, and every row listed is sealed in the store — so six sealed
// credentials read "0, the rest resolve from this process's environment".
// The tile is what a name's readers say: the names no field points at.
test("the tiles count what the answers say, never where a value lives", async () => {
  stubLoaded();
  render(<Secrets />);
  await screen.findByText("DATADOG_WEBHOOK_TOKEN");
  expect(screen.queryByText(/resolve from this process's environment/)).toBeNull();
  expect(screen.queryByText("In the secret store")).toBeNull();
  const tile = screen.getByText("Read by nothing").closest(".crewlet-statcard")!;
  // DATADOG_WEBHOOK_TOKEN has no reader in the fixture; GITHUB_TOKEN has two.
  expect(tile.textContent).toContain("1");
  expect(tile.textContent).toContain("no config field names them");
});

// AND AN UNANSWERED CHECK IS NOT A COUNT: every name would read as unread.
test("a reference check that failed counts nothing as unread", async () => {
  stubFetch((path) => {
    if (path === "/secrets") return ok(body);
    if (path === "/config/references") {
      return new Response(JSON.stringify({ error: "internal_error" }), { status: 500 });
    }
    return ok({});
  });
  render(<Secrets />);
  await screen.findByText("DATADOG_WEBHOOK_TOKEN");
  const tile = screen.getByText("Read by nothing").closest(".crewlet-statcard")!;
  expect(tile.textContent).not.toMatch(/\d/);
  expect(tile.textContent).toContain("the reference check did not answer");
});

// WHICH PATH WROTE A CREDENTIAL, in words: `source` is provenance, and each
// writer's word is said as what it means. A word no writer in this build
// stamps is said as the writer's own, never guessed at.
test("a credential's provenance is said as the path that wrote it", async () => {
  stubLoaded();
  render(<CredentialPeek name="DATADOG_WEBHOOK_TOKEN" />);
  expect(await screen.findByText("written by an integration's setup")).toBeDefined();
  expect(provenance("api")).toBe("stored over the API — this page or PUT /secrets");
  expect(provenance("cli")).toBe("set with crewlet secrets set");
  expect(provenance("migrated")).toBe("moved onto the fleet from a node's own table");
  expect(provenance("gitlab-provision")).toBe("its writer named itself “gitlab-provision”");
  for (const word of ["api", "cli", "setup", "provision", "rekey", "migrated"]) {
    expect(provenance(word)).not.toMatch(/environment|named itself/);
  }
});

// EACH PROPERTY ONCE. The peek's header carried Key id, Set by and Updated as
// facts and the provenance group said all three again a hundred pixels down;
// stacked in one column they are one reading, so the group states them.
test("the credential's peek says each property once", async () => {
  stubLoaded();
  render(<CredentialPeek name="GITHUB_TOKEN" />);
  expect(await screen.findByText("Where it came from")).toBeDefined();
  const said = (label: string) =>
    [...document.querySelectorAll(".fact-label, dt")].filter((el) => el.textContent === label);
  for (const label of ["Source", "Key id", "Set by", "Updated"]) {
    expect(said(label), label).toHaveLength(1);
  }
  // The key id is a VALUE in the mono face; its label is a word.
  const key = screen.getByText("k1");
  expect(key.classList.contains("mono")).toBe(true);
  expect(said("Key id")[0]!.classList.contains("mono")).toBe(false);
});

describe("the credentials table", () => {
  /** The table's track list, one entry per column. */
  const tracks = (grid: HTMLElement) =>
    grid.style.gridTemplateColumns.match(/minmax\([^)]*\)|fit-content\([^)]*\)|\S+/g) ?? [];

  test("the name is the one flexible track, and it is never narrower than itself", async () => {
    stubLoaded();
    const { container } = render(<Secrets />);
    expect(await screen.findByText("DATADOG_WEBHOOK_TOKEN")).toBeDefined();
    const all = tracks(container.querySelector<HTMLElement>(".grid-wrap")!);
    expect(all).toHaveLength(7);
    expect(all[0]).toBe("minmax(max-content, 1fr)");
    for (const track of all.slice(1)) expect(track).toMatch(/^fit-content\(/);
    expect(all.filter((t) => t.includes("1fr"))).toHaveLength(1);
  });

  // THE ROW THE RAIL IS OPEN ON IS MARKED, as the work list marks its peeked
  // item: six look-alike names, and nothing said which one the rail was about.
  test("the credential the peek is open on is the marked row", async () => {
    stubLoaded();
    location.hash = "#/settings/secrets?peek=credential:GITHUB_TOKEN";
    try {
      render(<Secrets />);
      const name = await screen.findByText("GITHUB_TOKEN");
      expect(name.closest(".grid-row")?.classList.contains("selected")).toBe(true);
      const other = screen.getByText("DATADOG_WEBHOOK_TOKEN");
      expect(other.closest(".grid-row")?.classList.contains("selected")).toBe(false);
    } finally {
      location.hash = "#/";
    }
  });

  // LAID OUT, as the browser measured each column at its content in the
  // harness: the longest name (MATTERMOST_ADMIN_TOKEN) with its padding, every
  // other column at its head or its value. A peek at 1440 leaves the table
  // 486px, a 1280 window 746px.
  describe("laid out", () => {
    const NATURAL: Record<string, number> = {
      Name: 196,
      "Read by": 77,
      Source: 76,
      "Key id": 63,
      "Set by": 70,
      Updated: 76,
      "": 80,
    };
    let box = 486;
    const real = globalThis.ResizeObserver;
    beforeEach(() => {
      globalThis.ResizeObserver = class {
        observe(): void {}
        unobserve(): void {}
        disconnect(): void {}
      } as unknown as typeof ResizeObserver;
      vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockImplementation(function (
        this: HTMLElement,
      ) {
        return this.classList.contains("grid-wrap") ? box : 0;
      });
      // Each head is its column's natural width, laid end to end.
      vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
        this: HTMLElement,
      ) {
        const width = (el: Element) => NATURAL[el.textContent?.trim() ?? ""] ?? 60;
        if (this.classList.contains("grid-th")) {
          const siblings = [...this.parentElement!.children];
          const left = siblings.slice(0, siblings.indexOf(this)).reduce((n, h) => n + width(h), 0);
          return {
            left,
            right: left + width(this),
            width: width(this),
            top: 0,
            bottom: 0,
            height: 0,
          } as DOMRect;
        }
        const right = this.classList.contains("grid-wrap") ? box : 0;
        return { left: 0, right, width: right, top: 0, bottom: 0, height: 0 } as DOMRect;
      });
    });
    afterEach(() => {
      globalThis.ResizeObserver = real;
      box = 486;
    });

    async function drawn(width: number) {
      box = width;
      stubLoaded();
      const { container } = render(<Secrets />);
      expect(await screen.findByText("DATADOG_WEBHOOK_TOKEN")).toBeDefined();
      const grid = container.querySelector<HTMLElement>(".grid-wrap")!;
      return {
        heads: [...grid.querySelectorAll(".grid-head > .grid-th")].map((h) =>
          h.textContent?.trim(),
        ),
        hidden: grid.parentElement?.querySelector(".grid-foot")?.textContent ?? "",
      };
    }

    test("beside a peek it gives way whole facts, and never cuts the name", async () => {
      const at = await drawn(486);
      expect(at.heads).toEqual(["Name", "Read by", "Updated", ""]);
      expect(at.hidden).toContain("Hidden to fit: Key id, Set by and Source");
    });

    test("at a 1280 window it draws every column", async () => {
      const at = await drawn(746);
      expect(at.heads).toEqual(Object.keys(NATURAL));
      expect(at.hidden).not.toContain("Hidden to fit");
    });

    // A COLUMN HIDDEN IN FAVOUR OF THE PEEK HAS TO BE IN IT. Squeezed to
    // nothing, every column that can give way does — and each is a fact the
    // credential's own peek draws. What reads it never gives way.
    test("every column it can give way is one the credential's peek carries", async () => {
      const at = await drawn(160);
      const hidden = at.hidden.replace(/^.*Hidden to fit:\s*/, "").split(/,\s*|\s+and\s+/);
      expect(hidden.sort()).toEqual(["Key id", "Set by", "Source", "Updated"]);
      expect(at.heads).toContain("Read by");
      cleanup();
      stubLoaded();
      render(<CredentialPeek name="DATADOG_WEBHOOK_TOKEN" />);
      expect(await screen.findByText("Where it came from")).toBeDefined();
      for (const column of hidden) {
        expect(screen.getAllByText(column).length, column).toBeGreaterThan(0);
      }
    });
  });
});
