/**
 * Models & keys draws the company document's `providers.llm` entries and the
 * chains the org projection carries, and holds no credential's value.
 *
 * The invariants, in the order they cost when they go: no value reaches the
 * page — a credential is the variable it names, or said to be written inline,
 * and the mask itself is never drawn; an entry is drawn as the engine will run
 * it, a field it leaves out said as the engine's default; the document is
 * re-read when the engine publishes a company and never on a clock; and the
 * edit sends back the entity it read with only what was edited replaced.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { Models } from "./Models.tsx";
import { FrameReadings } from "~/app/Shell.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, QueryError, Store, type CompanyDocument } from "~/protocol/index.ts";
import { withDerived } from "~/test/org.ts";

/**
 * Three models, declared in the order a seat that names none falls back
 * through — which a JSON object's sorted keys do not keep, so `llm_order`
 * carries it. `smart` names only its model and a token; `fast` is a Codex
 * entry on an API key, which serves text phases only; `spare` is an OpenCode
 * entry whose executor runs happen on e2b, forwards the engine's own
 * environment, and no seat names it.
 */
const company = (): CompanyDocument => ({
  name: "Acme",
  providers: {
    llm: {
      fast: {
        agent: "codex",
        model: "gpt-5",
        auth: { mode: "api-key", api_key: "${OPENAI_KEY}" },
      },
      smart: { model: "claude-opus-5-5", auth: { token: "${CLAUDE_TOKEN}" } },
      spare: {
        agent: "opencode",
        model: "anthropic/claude-sonnet-5",
        run_in: "e2b",
        auth: { mode: "inherit-env" },
      },
    },
    llm_order: ["smart", "fast", "spare"],
  },
});

/** Two agents: the CEO runs on smart and falls back to fast; the SWE's reviewer runs on fast. */
const org = {
  name: "Acme",
  roles: [
    { name: "CEO", handle: "ceo", llm: { execute: ["smart", "fast"], review: ["smart"] } },
    { name: "SWE", handle: "swe", llm: { execute: ["smart"], review: ["fast"] } },
  ],
};

const OPERATOR = {
  login: "U0FOUNDER",
  grants: ["admin", "worklet", "doclet"],
  handle: "jane",
  owner: "jane",
  name: "Jane",
  kind: "human",
};

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

function mount(id?: string, { refuse = false, answer = company() } = {}) {
  const store = new Store();
  store.applyOrg(withDerived(org) as never);
  store.setConnected(true);
  const socket = new LiveSocket(store);
  const query = vi.fn((what: string) => {
    if (what === "viewer") return Promise.resolve(OPERATOR);
    if (what === "config") {
      return refuse
        ? Promise.reject(new QueryError("unauthorized", { reason: "operator", grants: ["admin"] }))
        : Promise.resolve(answer);
    }
    return new Promise(() => {});
  });
  (socket as unknown as { query: typeof query }).query = query;
  const view = render(
    <ClientContext.Provider value={{ store, socket }}>
      <ToastProvider>
        <LayerHost>
          <FrameReadings>
            <Router>
              <Models id={id} />
            </Router>
          </FrameReadings>
        </LayerHost>
      </ToastProvider>
    </ClientContext.Provider>,
  );
  return { view, query, store };
}

async function settle() {
  await act(async () => {
    for (let i = 0; i < 12; i++) await Promise.resolve();
  });
}

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

const row = (key: string) =>
  screen
    .getAllByText(key)
    .map((el) => el.closest(".crewlet-data-grid__row"))
    .find((el): el is HTMLElement => el !== null)!;

test("every model is drawn as the engine will run it, in the order it was declared", async () => {
  mount();
  await settle();
  // DECLARED ORDER, off `llm_order` — not the sorted keys the JSON carries.
  const keys = [
    ...document.querySelectorAll(".crewlet-data-grid__row .model-name > .mono:first-child"),
  ].map((el) => el.textContent);
  expect(keys).toEqual(["smart", "fast", "spare"]);
  // The CLI and the model under the key; an entry naming no agent drives the default.
  expect(within(row("smart")).getByText("claude-code · claude-opus-5-5")).toBeDefined();
  expect(within(row("fast")).getByText("codex · gpt-5")).toBeDefined();
  // What the engine runs it for, and where its executor runs happen: only a
  // CLI with an in-box runner runs an executor, so Codex serves text phases.
  expect(within(row("spare")).getByText("Executor")).toBeDefined();
  expect(within(row("spare")).getByText("· e2b")).toBeDefined();
  expect(within(row("smart")).getByText("Executor")).toBeDefined();
  expect(within(row("fast")).getByText("Text phases")).toBeDefined();
  // How it signs in, and the variable its credential is read from.
  expect(within(row("smart")).getByText("Subscription")).toBeDefined();
  expect(within(row("smart")).getByText("CLAUDE_TOKEN")).toBeDefined();
  expect(within(row("fast")).getByText("API key")).toBeDefined();
  expect(within(row("fast")).getByText("OPENAI_KEY")).toBeDefined();
  expect(within(row("spare")).getByText("Engine environment")).toBeDefined();
  // Who runs on it, off the chains the org projection carries.
  expect(within(row("smart")).getByText("2 seats")).toBeDefined();
  expect(within(row("fast")).getByText("1 seat")).toBeDefined();
  // The tiles: three models, one of them named by no seat; two that can run an
  // executor; one billed per token.
  const tile = (label: string) =>
    screen
      .getByText(label, { selector: ".crewlet-statcard__label" })
      .closest(".crewlet-statcard") as HTMLElement;
  expect(within(tile("Models")).getByText("1 named by no seat")).toBeDefined();
  expect(within(tile("Executors")).getByText("2")).toBeDefined();
  expect(within(tile("Executors")).getByText("can run a seat’s executor")).toBeDefined();
  expect(within(tile("On an API key")).getByText("1")).toBeDefined();
});

test("no credential's value and no mask reaches the page", async () => {
  const answer = company();
  const llm = (answer.providers as { llm: Record<string, Record<string, unknown>> }).llm;
  llm.smart = {
    model: "claude-opus-5-5",
    auth: { token: "__redacted__", credential_bundle: "sk-leaked-literal" },
    env: { HTTPS_PROXY: "http://user:hunter2@proxy.example.com" },
  };
  const list = mount(undefined, { answer });
  await settle();
  expect(within(row("smart")).getByText("token written inline")).toBeDefined();
  // READ WHILE IT IS MOUNTED: cleanup() unmounts the list and empties its
  // container, so a check made after it reads "" and passes whatever the list
  // drew. And off the base element rather than the container, so what the
  // page sends through LayerHost (a tooltip, a layer) is read too.
  const listHtml = list.view.baseElement.innerHTML;
  cleanup();
  const page = mount("smart", { answer });
  await settle();
  // Inline is said, by name of the field, never drawn as what the document holds.
  expect(screen.getAllByText("written inline").length).toBe(3);
  const pageHtml = page.view.baseElement.innerHTML;
  for (const html of [listHtml, pageHtml]) {
    // Each read holds the screen it was taken of, so neither check is of an
    // emptied tree.
    expect(html).toContain("written inline");
    expect(html).not.toContain("__redacted__");
    expect(html).not.toContain("sk-leaked-literal");
    expect(html).not.toContain("hunter2");
  }
});

// WHAT AN UNSET FIELD MEANS IS THE ENGINE'S, and is said as a default rather
// than as nothing: an entry naming only its model still drives a CLI under a
// timeout.
test("a model's page says the defaults an entry leaves to the engine, as defaults", async () => {
  mount("smart");
  await settle();
  expect(screen.getByRole("heading", { name: "smart" })).toBeDefined();
  const fact = (label: string) =>
    screen.getByText(label, { selector: "*" }).closest(".crewlet-fact-line__fact") as HTMLElement;
  expect(within(fact("CLI")).getByText("claude-code")).toBeDefined();
  expect(fact("CLI").textContent).toMatch(/default/i);
  expect(within(fact("Timeout")).getByText("10 minutes")).toBeDefined();
  expect(fact("Timeout").textContent).toMatch(/default/i);
  // AND WHERE ITS EXECUTOR RUNS: an entry naming no run_in runs in the
  // catalogue's default cell, said as such rather than as nowhere.
  expect(within(fact("Runs as")).getByText("Executor")).toBeDefined();
  expect(fact("Runs as").textContent).toMatch(/default cell/);
});

test("a model's page says how it signs in, and the seats that run on it and fall back to it", async () => {
  mount("fast");
  await settle();
  const signin = screen.getByText("Signing in").closest(".crewlet-card") as HTMLElement;
  expect(within(signin).getByText("API key", { selector: "span" })).toBeDefined();
  expect(within(signin).getByText("OPENAI_KEY")).toBeDefined();
  // The way to change the CLI, where it runs or the sign-in is the entry in Configuration.
  const change = within(signin).getByRole("link", { name: /under Configuration/ });
  expect(change.getAttribute("href")).toContain("entity=fast");
  // The CEO falls back to it; the SWE's reviewer runs on it.
  const ceo = screen.getByText("CEO").closest(".crewlet-data-grid__row") as HTMLElement;
  expect(within(ceo).getByText("executor")).toBeDefined();
  const swe = screen.getByText("SWE").closest(".crewlet-data-grid__row") as HTMLElement;
  expect(within(swe).getByText("reviewer")).toBeDefined();
});

// A MODEL NO SEAT RUNS ON READS THE SAME on its row and on its page: the list
// drew "–" and the page "0 seats", two different facts about one model.
test("a model no seat names says so in the same words on the list and on its page", async () => {
  mount();
  await settle();
  expect(within(row("spare")).getByText("No seat names it")).toBeDefined();
  cleanup();
  mount("spare");
  await settle();
  const fact = screen.getByText("Runs").closest(".crewlet-fact-line__fact") as HTMLElement;
  expect(within(fact).getByText("No seat names it")).toBeDefined();
  expect(within(fact).queryByText(/0 seats/)).toBeNull();
});

// THE MODEL ID IS ONE TOKEN: kept on its line, the whole of it on its title,
// rather than wrapped at a hyphen and then clamped with no way to read it.
test("a model's page keeps its model id whole on one line, all of it on the title", async () => {
  const answer = company();
  const long = "openrouter/anthropic/claude-some-really-long-model-identifier-2026-09";
  const llm = (answer.providers as { llm: Record<string, Record<string, unknown>> }).llm;
  llm.spare!.model = long;
  mount("spare", { answer });
  await settle();
  const value = screen.getByText(long);
  expect(
    value.closest(".crewlet-fact-line__fact")!.classList.contains("crewlet-fact-line__fact--token"),
  ).toBe(true);
  expect(value.getAttribute("title")).toBe(long);
});

test("a key the document does not hold is said, not drawn as an empty model", async () => {
  mount("nope");
  await settle();
  expect(screen.getByText("No model called “nope”")).toBeDefined();
});

// NO COMPANY IS AN ANSWER: the document is null before the first import, and
// that is a company with no model rather than a question still out.
test("a node running no company says there is no model, not that it is loading", async () => {
  mount(undefined, { answer: null as unknown as CompanyDocument });
  await settle();
  expect(screen.getByText(/No model configured/)).toBeDefined();
  expect(screen.queryByLabelText("Loading")).toBeNull();
});

// NO MODEL IS THE KIT'S EMPTY STATE, with the way to where one is added: a
// plain sentence in a padded box was the one empty card on Settings drawn by
// hand, beside the MCP servers card's empty state.
test("a company with no model says so with the empty state and the way to Configuration", async () => {
  mount(undefined, { answer: { name: "Acme", providers: { llm: {} } } });
  await settle();
  const title = await screen.findByText("No model configured yet");
  const state = title.closest(".crewlet-empty-state") as HTMLElement;
  expect(state).not.toBeNull();
  expect(within(state).getByText("providers.llm").tagName).toBe("CODE");
  expect(within(state).getByRole("link", { name: "Open Configuration" }).getAttribute("href")).toBe(
    "#/settings/config",
  );
});

test("a refused reader sees the refusal alone — no tiles, no empty list", async () => {
  const { view } = mount(undefined, { refuse: true });
  await settle();
  // THE ENGINE'S REFUSAL, naming the grant that would have admitted the reader.
  expect(
    await screen.findByText(/This answer needs Admin access\. An admin can give it to you\./),
  ).toBeDefined();
  expect(view.container.querySelector(".crewlet-statcard")).toBeNull();
  expect(screen.queryByText(/No model configured/)).toBeNull();
});

// THE DOCUMENT MOVES ONLY WITH A REVISION, and the engine re-sends the org
// projection on every one it publishes: that is when the screen asks again,
// and it asks on no clock. The CONTROL is the same mount with no publish.
test("the document is re-read when the engine publishes a company, and not otherwise", async () => {
  const { query, store } = mount();
  await settle();
  const asked = () => query.mock.calls.filter(([what]) => what === "config").length;
  expect(asked()).toBe(1);
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 50));
  });
  expect(asked()).toBe(1);
  await act(async () => {
    store.applyOrg(withDerived(org) as never);
  });
  await settle();
  expect(asked()).toBe(2);
});

// --- editing one ------------------------------------------------------------ //

const json = (payload: unknown, status: number, headers: Record<string, string> = {}) =>
  new Response(JSON.stringify(payload), {
    status,
    headers: { "Content-Type": "application/json", ...headers },
  });

interface Call {
  method: string;
  url: string;
  body: Record<string, unknown> | undefined;
  headers: Headers;
}

function stubConfig(respond: (call: Call) => Response): Call[] {
  const calls: Call[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit = {}) => {
      if (String(url).includes("/secrets")) return json({ secrets: [] }, 200);
      const call: Call = {
        method: init.method ?? "GET",
        url: String(url),
        body: init.body ? (JSON.parse(init.body as string) as Record<string, unknown>) : undefined,
        headers: new Headers(init.headers),
      };
      calls.push(call);
      return respond(call);
    }),
  );
  return calls;
}

/** `smart` as the engine serves it: a subscription entry, its token a reference. */
const entity = {
  model: "claude-opus-5-5",
  state_dir: "/var/lib/crewlet/claude",
  env: { HTTPS_PROXY: "${PROXY}" },
  overrides: { binary: "/opt/claude" },
  timeout_seconds: 900,
  auth: { token: "${CLAUDE_TOKEN}", credential_bundle: "${CLAUDE_BUNDLE}" },
};

const valid = (warnings: unknown[] = []) =>
  json({ valid: true, base_revision_id: "r1", warnings, derived: null }, 200);

const saved = () => json({ revision_id: "r2", epoch: 7, warnings: [], derived: null }, 201);

async function openEdit(id = "smart") {
  mount(id);
  await settle();
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Edit" }));
  });
  await settle();
  return screen.getByRole("dialog");
}

async function save(dialog: HTMLElement) {
  await act(async () => {
    fireEvent.click(within(dialog).getByRole("button", { name: "Save" }));
  });
  await settle();
}

const puts = (calls: Call[]) => calls.filter((c) => c.method === "PUT");

test("an edit sends back the entity it read, with only what was edited replaced", async () => {
  const calls = stubConfig((call) => {
    if (call.method === "GET") return json(entity, 200, { ETag: '"r1"' });
    if (call.url.includes("dry_run=true")) return valid();
    return saved();
  });
  const dialog = await openEdit();
  fireEvent.change(within(dialog).getByLabelText(/^Model/), { target: { value: "claude-opus-6" } });
  await save(dialog);

  const shape = calls.map((c) => `${c.method} ${c.url.replace(/^.*\/config/, "/config")}`);
  expect(shape).toEqual([
    "GET /config/llm-providers/smart",
    "PUT /config/llm-providers/smart?dry_run=true",
    "PUT /config/llm-providers/smart?dry_run=true",
    "PUT /config/llm-providers/smart",
  ]);
  const stored = calls[3]!;
  expect(stored.headers.get("If-Match")).toBe('"r1"');
  expect(stored.body).toEqual({
    ...entity,
    model: "claude-opus-6",
    _summary: "Edit the model smart: model claude-opus-5-5 → claude-opus-6",
  });
  expect(screen.queryByRole("dialog")).toBeNull();
});

test("a subscription entry edits its token, and the summary names the variable", async () => {
  const calls = stubConfig((call) => {
    if (call.method === "GET") return json(entity, 200, { ETag: '"r1"' });
    if (call.url.includes("dry_run=true")) return valid();
    return saved();
  });
  const dialog = await openEdit();
  fireEvent.change(within(dialog).getByLabelText(/^Token/), {
    target: { value: "${CLAUDE_TOKEN_B}" },
  });
  await save(dialog);
  const stored = puts(calls).at(-1)!;
  expect(stored.body?.auth).toEqual({ ...entity.auth, token: "${CLAUDE_TOKEN_B}" });
  expect(stored.body?._summary).toBe("Edit the model smart: token CLAUDE_TOKEN → CLAUDE_TOKEN_B");
});

test("an api-key entry edits its API key and offers no token", async () => {
  const keyed = {
    agent: "codex",
    model: "gpt-5",
    auth: { mode: "api-key", api_key: "${OPENAI_KEY}" },
  };
  const calls = stubConfig((call) => {
    if (call.method === "GET") return json(keyed, 200, { ETag: '"r1"' });
    if (call.url.includes("dry_run=true")) return valid();
    return saved();
  });
  const dialog = await openEdit("fast");
  expect(within(dialog).queryByLabelText(/^Token/)).toBeNull();
  fireEvent.change(within(dialog).getByLabelText(/^API key/), {
    target: { value: "${OPENAI_KEY_B}" },
  });
  await save(dialog);
  expect(puts(calls).at(-1)!.body?.auth).toEqual({ mode: "api-key", api_key: "${OPENAI_KEY_B}" });
});

// INHERIT-ENV READS NO CREDENTIAL FROM THE DOCUMENT, so there is no field to
// fill: one would be refused, since the engine reads none under that mode.
test("an inherit-env entry offers no credential, and says how it signs in", async () => {
  stubConfig((call) =>
    call.method === "GET"
      ? json({ model: "sonnet", auth: { mode: "inherit-env" } }, 200, { ETag: '"r1"' })
      : valid(),
  );
  const dialog = await openEdit("spare");
  expect(within(dialog).queryByLabelText(/^Token/)).toBeNull();
  expect(within(dialog).queryByLabelText(/^API key/)).toBeNull();
  expect(within(dialog).getByText(/Nothing to set here\./)).toBeDefined();
});

// AN INLINE CREDENTIAL'S VALUE NEVER REACHED THIS PAGE. It is a locked row,
// sent back as the mask so the engine restores it — and replaced, not shown.
test("an inline credential is locked, kept as the mask, and replaced where it stands", async () => {
  const inline = { model: "sonnet", auth: { token: "__redacted__" } };
  const calls = stubConfig((call) => {
    if (call.method === "GET") return json(inline, 200, { ETag: '"r1"' });
    if (call.url.includes("dry_run=true")) return valid();
    return saved();
  });
  const dialog = await openEdit();
  expect(within(dialog).getByText("The token is inline.")).toBeDefined();
  expect(dialog.innerHTML).not.toContain("__redacted__");
  fireEvent.change(within(dialog).getByLabelText(/^Model/), { target: { value: "opus" } });
  await save(dialog);
  // Every request carrying the edit kept the mask where it was read.
  for (const call of puts(calls).filter((c) => c.body?.model === "opus")) {
    expect(call.body?.auth).toEqual({ token: "__redacted__" });
  }
  cleanup();

  const replaced = stubConfig((call) => {
    if (call.method === "GET") return json(inline, 200, { ETag: '"r1"' });
    if (call.url.includes("dry_run=true")) return valid();
    return saved();
  });
  const again = await openEdit();
  await act(async () => {
    fireEvent.click(
      within(again).getByRole("button", { name: "Replace the token with a reference" }),
    );
  });
  fireEvent.change(within(again).getByLabelText(/^Token/), { target: { value: "${NEW_TOKEN}" } });
  await save(again);
  expect(puts(replaced).at(-1)!.body?.auth).toEqual({ token: "${NEW_TOKEN}" });
  expect(puts(replaced).at(-1)!.body?._summary).toBe(
    "Edit the model smart: token inline → NEW_TOKEN",
  );
});

test("an unchanged form cannot be saved, and a cap out of range is refused before asking", async () => {
  const calls = stubConfig((call) =>
    call.method === "GET" ? json(entity, 200, { ETag: '"r1"' }) : valid(),
  );
  const dialog = await openEdit();
  const button = within(dialog).getByRole("button", { name: "Save" });
  expect(button.getAttribute("aria-disabled") ?? button.getAttribute("disabled")).not.toBeNull();

  fireEvent.click(within(dialog).getByText("Limits"));
  fireEvent.change(within(dialog).getByLabelText(/^Timeout/), { target: { value: "0" } });
  await save(dialog);
  expect(within(dialog).getByText(/A number of seconds above 0/)).toBeDefined();
  expect(puts(calls)).toEqual([]);
});

// THE ENGINE'S PROBLEM GOES BESIDE ITS FIELD, by the problem's segments: a
// token on a CLI that mints none is refused at `auth.token`.
test("the engine's refusal of the credential is said beside it", async () => {
  stubConfig((call) => {
    if (call.method === "GET") return json(entity, 200, { ETag: '"r1"' });
    if (
      call.url.includes("dry_run=true") &&
      call.body?.auth &&
      (call.body.auth as { token?: string }).token === "${OTHER}"
    ) {
      return json(
        {
          error: "validation_error",
          problems: [
            {
              path: "providers.llm.smart.auth.token",
              segments: ["providers", "llm", "smart", "auth", "token"],
              kind: "conflict",
              message: "the profile names no token_env",
            },
          ],
        },
        400,
      );
    }
    return valid();
  });
  const dialog = await openEdit();
  fireEvent.change(within(dialog).getByLabelText(/^Token/), { target: { value: "${OTHER}" } });
  await save(dialog);
  // BESIDE THE FIELD, not in the sentence above the form that carries the
  // problems no field owns.
  const said = within(dialog).getByText("the profile names no token_env");
  expect(said.closest('[role="status"]')).toBeNull();
});

// FOCUS STARTS ON THE FIRST FIELD, not on the footer the frame found while
// the body was still a skeleton.
test("the edit opens with the model field focused", async () => {
  stubConfig((call) => (call.method === "GET" ? json(entity, 200, { ETag: '"r1"' }) : valid()));
  const dialog = await openEdit();
  expect(document.activeElement).toBe(within(dialog).getByLabelText(/^Model/));
});
