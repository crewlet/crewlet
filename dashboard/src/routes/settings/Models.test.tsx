/**
 * Models & keys draws the engine's `credential_pool` and the chains the org
 * projection carries, and holds no key's value.
 *
 * The invariants, in the order they cost when they go: no value reaches the
 * page — the answer has no member for one, and a key the document holds inline
 * is served as the mask and drawn as a locked row, never as the mask's text;
 * the bench is drawn as the engine judged it, a cooling key with when it comes
 * back and a model whose every key is benched said as one; a node that could
 * not read the fleet's ledger says so; and the edit sends back the entity it
 * read with only what was edited replaced.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { Models } from "./Models.tsx";
import { FrameReadings } from "~/app/Shell.tsx";
import { Router } from "~/app/router.tsx";
import { POOL_STATE_WORDS } from "~/lib/models.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, QueryError, Store } from "~/protocol/index.ts";
import type { CredentialPoolAnswer } from "~/contract/credentials.ts";

const inMinutes = (m: number) => new Date(Date.now() + m * 60_000).toISOString();

const pool = (): CredentialPoolAnswer => ({
  node: "node-a",
  fleet: true,
  fleet_error: "",
  providers: [
    {
      key: "smart",
      type: "anthropic",
      model: "claude-sonnet-5",
      state: "degraded",
      ready: 1,
      rate_limit_seconds: 3600,
      auth_seconds: 300,
      keys: [
        {
          ref: "ANTHROPIC_KEY_A",
          source: "reference",
          hint: "aaaaaaaaaaaa",
          state: "ready",
          cooling_until: null,
          same_as: 0,
          uses: 12,
          in_flight: 1,
        },
        {
          ref: "ANTHROPIC_KEY_B",
          source: "reference",
          hint: "bbbbbbbbbbbb",
          state: "cooling",
          cooling_until: inMinutes(38),
          same_as: 0,
          uses: 4,
          in_flight: 0,
        },
        {
          ref: "",
          source: "inline",
          hint: "",
          state: "unresolved",
          cooling_until: null,
          same_as: 0,
          uses: 0,
          in_flight: 0,
        },
      ],
    },
    {
      key: "fast",
      type: "openai",
      model: "gpt-mini",
      state: "exhausted",
      ready: 0,
      rate_limit_seconds: 600,
      auth_seconds: 300,
      keys: [
        {
          ref: "OPENAI_API_KEY",
          source: "default",
          hint: "cccccccccccc",
          state: "cooling",
          cooling_until: inMinutes(5),
          same_as: 0,
          uses: 30,
          in_flight: 0,
        },
      ],
    },
    {
      key: "subscription",
      type: "cli-agent",
      model: "sonnet",
      state: "login",
      ready: 0,
      rate_limit_seconds: 3600,
      auth_seconds: 300,
      keys: [],
    },
  ],
});

/** Two agents: the CEO runs on smart and falls back to fast; the SWE's reviewer runs on fast. */
const org = {
  name: "Acme",
  roles: [
    { name: "CEO", handle: "ceo", llm: { execute: ["smart", "fast"], review: ["smart"] } },
    { name: "SWE", handle: "swe", llm: { execute: ["subscription"], review: ["fast"] } },
  ],
};

const OPERATOR = {
  operator_id: "U0FOUNDER",
  operator: true,
  handle: "jane",
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

function mount(id?: string, { refuse = false, answer = pool() } = {}) {
  const store = new Store();
  store.applyOrg(org as never);
  store.setConnected(true);
  const socket = new LiveSocket(store);
  const query = vi.fn((what: string) => {
    if (what === "viewer") return Promise.resolve(OPERATOR);
    if (what === "credential_pool") {
      return refuse ? Promise.reject(new QueryError("unauthorized")) : Promise.resolve(answer);
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
  return { view, query };
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

const smart0 = () =>
  screen.getByText("claude-sonnet-5", { exact: false }).closest(".grid-row") as HTMLElement;

test("every model is drawn with its keys as the engine judged them", async () => {
  mount();
  await settle();
  // One row per model, by its config key, in the engine's order.
  for (const key of ["smart", "fast", "subscription"]) {
    expect(screen.getAllByText(key).length).toBeGreaterThan(0);
  }
  // THE MODEL'S STATE IS THE ENGINE'S, in words.
  expect(screen.getByText(POOL_STATE_WORDS.degraded.label)).toBeDefined();
  expect(screen.getByText(POOL_STATE_WORDS.exhausted.label)).toBeDefined();
  expect(screen.getByText(POOL_STATE_WORDS.login.label)).toBeDefined();
  // A COOLING KEY SAYS WHEN IT COMES BACK, on its own mark.
  const b = screen.getByText("ANTHROPIC_KEY_B").closest("[title]") as HTMLElement;
  expect(b.getAttribute("title")).toMatch(/cooling, back in 3[78]m/);
  // AND IN WORDS, not only in the tone: a screen reader, or eyes that cannot
  // tell the inks apart, hear which state each mark is in.
  const marks = screen.getByRole("list", { name: "smart keys" });
  expect(
    within(marks)
      .getAllByRole("listitem")
      .map((li) => li.textContent?.replace(/\s+\d+m$/, "")),
  ).toEqual(["ANTHROPIC_KEY_A, ready", "ANTHROPIC_KEY_B, cooling", "key 3 · inline, not set"]);
  // THE BENCH NAMED FOR ITS CAUSE, one cause per line: on one line the pair
  // outgrew its column at 1280 and was clipped mid-glyph ("auth 2").
  const rate = within(smart0()).getByText("rate limit 1h");
  expect(within(smart0()).getByText("auth 5m").parentElement).toBe(rate.parentElement);
  expect(rate.parentElement!.classList.contains("is-stacked")).toBe(true);
  expect(within(smart0()).queryByText("·")).toBeNull();
  // Two of four counted keys cooling, one ready (the inline one resolves to nothing).
  const tile = screen.getByText("Keys ready").closest(".crewlet-statcard") as HTMLElement;
  expect(within(tile).getByText("1 of 4")).toBeDefined();
  expect(within(tile).getByText("2 cooling")).toBeDefined();
  // Who runs on it, off the chains the org projection carries.
  const smart = screen
    .getByText("claude-sonnet-5", { exact: false })
    .closest(".grid-row") as HTMLElement;
  expect(within(smart).getByText("1 seat")).toBeDefined();
});

test("no key's value and no mask reaches the page", async () => {
  const { view } = mount("smart");
  await settle();
  // The inline key is named by its place, never drawn as what the document holds.
  expect(screen.getByText("Key 3 (written inline)")).toBeDefined();
  expect(view.container.innerHTML).not.toContain("__redacted__");
});

// A DUPLICATE IS A ROW OF THE KEYS CARD and not a key the pool holds, so the
// header names it rather than leaving it as the gap between two counts.
test("a model's header counts the keys its card lists, a duplicate named", async () => {
  const answer = pool();
  const smart = answer.providers[0]!;
  smart.keys.push({ ...smart.keys[0]!, ref: "ANTHROPIC_KEY_D", state: "duplicate", same_as: 1 });
  mount("smart", { answer });
  await settle();
  const fact = screen.getByText("1 of 3 ready").parentElement as HTMLElement;
  expect(within(fact).getByText("1 duplicate")).toBeDefined();
});

// A DUPLICATE IS IN THE POOL ONCE, as the key it repeats — "Not in the pool"
// is the unresolved key's answer, not its.
test("a duplicate key comes back as the key it repeats", async () => {
  const answer = pool();
  const smart = answer.providers[0]!;
  smart.keys.push({ ...smart.keys[0]!, ref: "ANTHROPIC_KEY_D", state: "duplicate", same_as: 1 });
  mount("smart", { answer });
  await settle();
  const dup = screen.getByText("ANTHROPIC_KEY_D").closest(".grid-row") as HTMLElement;
  expect(within(dup).getByText("As key 1")).toBeDefined();
  const unset = screen.getByText("Key 3 (written inline)").closest(".grid-row") as HTMLElement;
  expect(within(unset).getByText("Not in the pool")).toBeDefined();
});

// A MODEL NO SEAT RUNS ON READS THE SAME on its row and on its page: the list
// drew "–" and the page "0 seats", two different facts about one model.
test("a model no seat names says so in the same words on the list and on its page", async () => {
  const answer = pool();
  answer.providers.push({ ...answer.providers[1]!, key: "spare", model: "gpt-spare" });
  mount(undefined, { answer });
  await settle();
  const row = screen.getByText("spare").closest(".grid-row") as HTMLElement;
  expect(within(row).getByText("No seat names it")).toBeDefined();
  cleanup();
  mount("spare", { answer });
  await settle();
  const fact = screen.getByText("Runs").closest(".fact") as HTMLElement;
  expect(within(fact).getByText("No seat names it")).toBeDefined();
  expect(within(fact).queryByText(/0 seats/)).toBeNull();
});

// THE VENDOR ID IS ONE TOKEN: kept on its line, the whole of it on its title,
// rather than wrapped at a hyphen and then clamped with no way to read it.
test("a model's page keeps its vendor id whole on one line, all of it on the title", async () => {
  const answer = pool();
  const long = "gpt-some-really-long-model-identifier-2026-09";
  answer.providers[1]!.model = long;
  mount("fast", { answer });
  await settle();
  const value = screen.getByText(long);
  expect(value.closest(".fact")!.classList.contains("is-token")).toBe(true);
  expect(value.getAttribute("title")).toBe(long);
});

test("a node that could not read the fleet's ledger says its cooldowns are its own", async () => {
  mount(undefined, {
    answer: { ...pool(), fleet: false, fleet_error: "the coordination store did not answer" },
  });
  await settle();
  expect(screen.getByText(/own cooldowns only/)).toBeDefined();
  expect(screen.getByText("the coordination store did not answer")).toBeDefined();
});

test("a model's page lists its keys whole and the seats that run on it, and which fall back", async () => {
  mount("fast");
  await settle();
  expect(screen.getByRole("heading", { name: "fast" })).toBeDefined();
  // Exhausted is said in full, not just as a pill.
  expect(screen.getByText(POOL_STATE_WORDS.exhausted.hint)).toBeDefined();
  // The conventional variable is marked as the default it is.
  expect(screen.getByText("OPENAI_API_KEY")).toBeDefined();
  expect(screen.getByText("default")).toBeDefined();
  // The CEO falls back to it; the SWE's reviewer runs on it.
  const ceo = screen.getByText("CEO").closest(".grid-row") as HTMLElement;
  expect(within(ceo).getByText("executor")).toBeDefined();
  const swe = screen.getByText("SWE").closest(".grid-row") as HTMLElement;
  expect(within(swe).getByText("reviewer")).toBeDefined();
});

test("a key the engine does not know is said, not drawn as an empty model", async () => {
  mount("nope");
  await settle();
  expect(screen.getByText("No model called “nope”")).toBeDefined();
});

test("a refused reader sees the refusal alone — no tiles, no empty list", async () => {
  const { view } = mount(undefined, { refuse: true });
  await settle();
  expect(await screen.findByText(/This answer is auth-gated/)).toBeDefined();
  expect(view.container.querySelector(".crewlet-statcard")).toBeNull();
  expect(screen.queryByText(/configures no model/)).toBeNull();
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

/** The entity as the engine serves it: one inline key masked, one reference. */
const entity = {
  type: "anthropic",
  model: "claude-sonnet-5",
  api_keys: ["${ANTHROPIC_KEY_A}", "__redacted__"],
  reasoning: true,
  timeout_seconds: 300,
};

const valid = (warnings: unknown[] = []) =>
  json({ valid: true, base_revision_id: "r1", warnings, derived: null }, 200);

async function openEdit() {
  mount("smart");
  await settle();
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Edit" }));
  });
  await settle();
  return screen.getByRole("dialog");
}

test("an edit sends back the entity it read, with only what was edited replaced", async () => {
  const calls = stubConfig((call) => {
    if (call.method === "GET") return json(entity, 200, { ETag: '"r1"' });
    if (call.url.includes("dry_run=true")) return valid();
    return json({ revision_id: "r2", epoch: 7, warnings: [], derived: null }, 201);
  });
  const dialog = await openEdit();
  // THE INLINE KEY IS LOCKED, and the mask is never drawn.
  expect(
    within(dialog).getByText(/Key 2 is written inline — its value is never sent here/),
  ).toBeDefined();
  expect(dialog.innerHTML).not.toContain("__redacted__");

  fireEvent.change(within(dialog).getByLabelText(/^Model/), { target: { value: "claude-opus-5" } });
  await act(async () => {
    fireEvent.click(within(dialog).getByRole("button", { name: "Save" }));
  });
  await settle();

  const shape = calls.map((c) => `${c.method} ${c.url.replace(/^.*\/config/, "/config")}`);
  expect(shape).toEqual([
    "GET /config/llm-providers/smart",
    "PUT /config/llm-providers/smart?dry_run=true",
    "PUT /config/llm-providers/smart?dry_run=true",
    "PUT /config/llm-providers/smart",
  ]);
  const save = calls[3]!;
  expect(save.headers.get("If-Match")).toBe('"r1"');
  expect(save.body).toEqual({
    ...entity,
    model: "claude-opus-5",
    _summary: "Edit the model smart: model claude-sonnet-5 → claude-opus-5",
  });
  expect(screen.queryByRole("dialog")).toBeNull();
});

test("an unchanged form cannot be saved, and a bench time out of range is refused before asking", async () => {
  const calls = stubConfig((call) =>
    call.method === "GET" ? json(entity, 200, { ETag: '"r1"' }) : valid(),
  );
  const dialog = await openEdit();
  const save = within(dialog).getByRole("button", { name: "Save" });
  expect(save.getAttribute("aria-disabled") ?? save.getAttribute("disabled")).not.toBeNull();

  fireEvent.click(within(dialog).getByText("Bench times"));
  fireEvent.change(within(dialog).getByLabelText(/^After a rate limit/), {
    target: { value: "5" },
  });
  await act(async () => {
    fireEvent.click(within(dialog).getByRole("button", { name: "Save" }));
  });
  await settle();
  expect(within(dialog).getByText(/from 60 to 86400/)).toBeDefined();
  expect(calls.filter((c) => c.method === "PUT")).toEqual([]);
});

// AN INLINE KEY IS PUT BACK BY ITS PLACE in the list, so while one is there
// the list neither grows nor shrinks: the engine refuses a changed length, and
// at an unchanged length with the mask moved it would restore the mask from
// its neighbour's value — a key silently swapped. Add and every removal that
// would move it are unavailable with the reason; the key is replaced IN ITS
// PLACE, after which the list is free again.
test("an inline key keeps its place: no gesture moves it, and it is replaced where it stands", async () => {
  const calls = stubConfig((call) => {
    if (call.method === "GET") return json(entity, 200, { ETag: '"r1"' });
    return valid();
  });
  const dialog = await openEdit();
  const unavailable = (el: HTMLElement) =>
    el.getAttribute("aria-disabled") === "true" || el.hasAttribute("disabled");
  const add = () => within(dialog).getByRole("button", { name: "Add key" });
  expect(unavailable(add())).toBe(true);
  expect(add().getAttribute("title")).toMatch(/Key 2 is written inline/);
  // Removing key 1 would move the inline key to index 0: unavailable.
  expect(unavailable(within(dialog).getByRole("button", { name: "Remove key 1" }))).toBe(true);
  // Removing the inline key itself leaves no mask to restore: available.
  expect(unavailable(within(dialog).getByRole("button", { name: "Remove key 2" }))).toBe(false);
  fireEvent.click(add());
  expect(within(dialog).queryByLabelText(/^Key 3/)).toBeNull();

  // REPLACED IN ITS PLACE: an empty field at key 2, and the list is free.
  await act(async () => {
    fireEvent.click(within(dialog).getByRole("button", { name: "Replace key 2 with a reference" }));
  });
  fireEvent.change(within(dialog).getByLabelText(/^Key 2/), { target: { value: "${ACME_KEY}" } });
  expect(unavailable(add())).toBe(false);
  await act(async () => {
    fireEvent.click(within(dialog).getByRole("button", { name: "Save" }));
  });
  await settle();
  // No request ever carried the mask at another length or another place.
  for (const call of calls.filter((c) => c.method === "PUT")) {
    const keys = (call.body as { api_keys?: string[] }).api_keys ?? [];
    const at = keys.indexOf("__redacted__");
    expect(at === -1 || (keys.length === 2 && at === 1)).toBe(true);
  }
  const edited = calls.filter((c) => c.method === "PUT")[1]!;
  expect((edited.body as { api_keys: string[] }).api_keys).toEqual([
    "${ANTHROPIC_KEY_A}",
    "${ACME_KEY}",
  ]);
});

// FOCUS STARTS ON THE FIRST FIELD, not on the footer the frame found while
// the body was still a skeleton.
test("the edit opens with the model field focused", async () => {
  stubConfig((call) => (call.method === "GET" ? json(entity, 200, { ETag: '"r1"' }) : valid()));
  const dialog = await openEdit();
  expect(document.activeElement).toBe(within(dialog).getByLabelText(/^Model/));
});
