/**
 * The model wizard against the engine's routes, faked only at the transport.
 *
 * The invariants: the primary action is the wizard's own on every step, so a
 * phone keeps it in one place; a credential is stored before the document
 * that names it, with the draft it is for; every refusal is said in words at
 * the field it is about; a deep link opens quiet; a filled wizard is not
 * thrown away without asking; a device sign-in starts once the model exists;
 * and Review names the fallback a write drops.
 */

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { pageSnapshot, resetPageContextForTest, setFramePage } from "~/app/assistant/context.ts";
import { Router } from "~/app/router.tsx";
import { ToastProvider } from "@crewlethq/ui";
import type { Fleet } from "~/protocol/fleets.ts";
import type { CompanyDocument } from "~/protocol/types.ts";
import {
  DEFAULT_FLEET_EMPTY,
  EVERY_JOB,
  fleetsFixture,
  modelEntry,
  modelsData,
  runs,
  seat,
  signIn,
} from "~/routes/settings/models/fixtures.ts";
import type { ModelsData } from "../data.ts";
import type { WizardAt } from "../parts.tsx";
import { ModelWizard } from "./ModelWizard.tsx";

const calls: string[] = [];

vi.mock("../api.ts", () => ({
  testCredential: vi.fn(),
  storeCredential: vi.fn(),
  checkHostLogin: vi.fn(),
  writeDocument: vi.fn(),
}));
const checkModel = vi.hoisted(() => vi.fn());
vi.mock("../useCheckModel.ts", () => ({ useCheckModel: () => checkModel }));
vi.mock("~/protocol/configWrite.ts", () => ({ getConfig: vi.fn() }));

const answers: Record<string, unknown> = {};
vi.mock("~/lib/useQuery.ts", () => ({
  useQuery: (name: string) => ({
    data: answers[name] ?? null,
    loading: false,
    error: null,
    refusal: null,
    refetch: () => {},
  }),
}));

const api = await import("../api.ts");
const { getConfig } = await import("~/protocol/configWrite.ts");

const DOC = {
  name: "Acme",
  providers: {
    // A COMPANY WITH MODELS has a place to run coding; M-002's case takes it away.
    sandbox: { local: {}, default_run_in: "direct" },
    llm: {
      default: { harness: "claude-code", model: "opus", auth: { mode: "api-key" } },
      sonnet: {
        harness: "claude-code",
        model: "sonnet",
        auth: { mode: "api-key", api_key: "${ANTHROPIC_API_KEY_SONNET}" },
      },
    },
  },
  roles: [
    { name: "Backend Engineer", handle: "backend-engineer", llm: "default" },
    { name: "Product Manager", handle: "pm", llm: "default" },
    { name: "Quality Lead", handle: "qa", llm: "default", llm_review: ["default", "sonnet"] },
  ],
} as unknown as CompanyDocument;

function data(): ModelsData {
  return modelsData({
    doc: DOC,
    entries: [
      modelEntry({
        key: "default",
        model: "opus",
        sign_in: signIn({ route: "api-key", mode: "api-key", variable: "ANTHROPIC_API_KEY" }),
        used_by: [
          ...runs("backend-engineer", ...EVERY_JOB),
          ...runs("pm", ...EVERY_JOB),
          ...runs("qa", ...EVERY_JOB),
        ],
      }),
      modelEntry({
        key: "sonnet",
        model: "sonnet",
        sign_in: signIn({
          route: "api-key",
          mode: "api-key",
          variable: "ANTHROPIC_API_KEY_SONNET",
        }),
      }),
    ],
  });
}

function show(props: { editing?: string; target?: WizardAt } = {}) {
  const onClose = vi.fn();
  const onDevice = vi.fn();
  const d = data();
  const editing = props.editing ? d.models.find((m) => m.key === props.editing) : undefined;
  render(
    <Router>
      <ModelWizard
        data={d}
        {...(editing ? { editing } : {})}
        {...(props.target ? { target: props.target } : {})}
        onClose={onClose}
        onDevice={onDevice}
      />
    </Router>,
  );
  return { onClose, onDevice };
}

const primaryButton = () =>
  document.querySelector(".crewlet-setup-wizard__primary")?.querySelector("button") ?? null;
const heading = () => document.querySelector(".crewlet-setup-wizard__heading")?.textContent;
const keyField = () => screen.getByLabelText(/API key/, { selector: "input[type='password']" });
const press = (name: string | RegExp) => fireEvent.click(screen.getByRole("button", { name }));
/** Review, by its step in the wizard's steps. */
const toReview = () =>
  fireEvent.click(
    within(screen.getByRole("list", { name: /^Steps to/ })).getByRole("button", { name: /Review/ }),
  );

beforeEach(() => {
  calls.length = 0;
  answers.llm_models = {
    source: "models.dev",
    fetched_at: null,
    provider: "anthropic",
    aliases: [{ id: "sonnet", name: "Sonnet", detail: "The harness's latest Sonnet" }],
    models: [{ id: "claude-opus-5-5", name: "Claude Opus 5.5" }],
    docs_url: "https://example.com/models",
  };
  answers.llm_runners = {
    default_run_in: "container",
    cells: {
      direct: { declared: true, ready_on: ["node-1"], not_ready_on: [] },
      container: { declared: true, ready_on: ["node-1"], not_ready_on: [] },
      e2b: { declared: false, ready_on: [], not_ready_on: ["node-1"] },
    },
    nodes: [
      {
        id: "node-1",
        reported: true,
        direct: null,
        container: null,
        e2b: {
          state: "not_ready",
          detail: "E2B key not set",
          fix: "Add E2B_API_KEY.",
          checked_at: null,
        },
      },
    ],
  };
  vi.mocked(getConfig).mockImplementation(async () => {
    calls.push("read");
    return { kind: "document", document: structuredClone(DOC), etag: '"rev-1"' };
  });
  vi.mocked(api.writeDocument).mockImplementation(async (_doc, options) => {
    calls.push(options.dryRun ? "dry-run" : `write:${options.summary}`);
    return options.dryRun
      ? { kind: "valid", baseRevisionId: "rev-1", warnings: [], derived: null }
      : { kind: "saved", revisionId: "rev-2", epoch: 2, warnings: [], derived: null };
  });
  vi.mocked(api.testCredential).mockImplementation(async () => {
    calls.push("test");
    return {
      kind: "ok",
      status: 200,
      value: {
        ok: true,
        verified: "Answered a one-turn Claude Code run in 3.4 s with only this value.",
        hints: {},
        check: { at: "", node: "node-1", ok: true, lines: [] },
        limited: null,
      },
    };
  });
  vi.mocked(api.storeCredential).mockImplementation(async () => {
    calls.push("store");
    return {
      kind: "ok",
      status: 200,
      value: {
        verified: "",
        check: { at: "", node: "node-1", ok: true, lines: [] },
        names: [],
        hints: {},
        entries: [],
        reload: null,
        cleared: [],
        running_runs: 0,
        limited: null,
      },
    };
  });
});

afterEach(() => {
  cleanup();
  resetPageContextForTest();
  vi.clearAllMocks();
  location.hash = "";
});

/**
 * Walks a new Claude Code model on an API key to Review, reading the footer on
 * every step: the primary's words, and whether Finish and save is offered.
 */
async function walkToReview(key = "sonnet-key", { accepted = true } = {}) {
  const footers: { primary: string; finish: boolean }[] = [];
  const footer = () => ({
    primary: primaryButton()?.textContent ?? "",
    finish: screen.queryAllByRole("button", { name: "Finish and save" }).length > 0,
  });
  fireEvent.change(screen.getByRole("textbox", { name: /Name/ }), { target: { value: key } });
  footers.push(footer());
  press("Continue");
  fireEvent.change(keyField(), { target: { value: "sk-ant-api03-new" } });
  footers.push(footer());
  press("Continue");
  fireEvent.click(screen.getByRole("radio", { name: /Sonnet/ }));
  footers.push(footer());
  for (const step of ["Where it runs", "Limits", "Review"]) {
    press("Continue");
    expect(heading()).toBe(step);
    footers.push(footer());
  }
  if (accepted) await screen.findByText("The configuration is valid");
  return footers;
}

/**
 * NO TEST CALL STEP: adding a model ends on Review, which runs the test call
 * and says it in one line, with Finish and save as its one primary. Finish and
 * save is offered beside Continue from the first step where nothing is missing.
 */
test("adding a model ends on Review with the test call on it, and Finish and save once nothing is missing", async () => {
  show();
  const footers = await walkToReview();
  expect(footers.map((f) => f.primary)).toEqual([...Array(5).fill("Continue"), "Finish and save"]);
  // NAME ALONE, then a key but no model: something is missing. From Model on, nothing is.
  expect(footers.map((f) => f.finish)).toEqual([false, false, true, true, true, true]);
  const steps = screen.getByRole("list", { name: /^Steps to/ });
  expect(within(steps).queryByRole("button", { name: /Test call/ })).toBeNull();
  expect(within(steps).getByRole("button", { name: /Review/ })).toBeTruthy();
  // THE TEST CALL RAN ON REVIEW, its result one line.
  expect(screen.getByText("Passed in 3.4 s")).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Run test call" })).toBeNull();
});

/**
 * FINISH AND SAVE FROM AN EARLY STEP: the steps left keep their defaults (no
 * seat, the harness's limits, web tools as they are), and the save still tests
 * the key, checks the change and stores it before the document.
 */
test("Finish and save from the Model step saves with the defaults of the steps left", async () => {
  show();
  fireEvent.change(screen.getByRole("textbox", { name: /Name/ }), {
    target: { value: "sonnet-key" },
  });
  press("Continue");
  fireEvent.change(keyField(), { target: { value: "sk-ant-api03-new" } });
  press("Continue");
  fireEvent.click(screen.getByRole("radio", { name: /Sonnet/ }));
  expect(heading()).toBe("Model");
  press("Finish and save");
  await waitFor(() => expect(calls.at(-1)).toMatch(/^write:/));
  expect(calls).toEqual([
    "test",
    "read",
    "dry-run",
    "dry-run",
    "store",
    "write:Add model sonnet-key in the default-fleet fleet",
  ]);
  const written = vi.mocked(api.writeDocument).mock.calls.at(-1)![0] as Record<string, any>;
  const entry = written.providers.llm["sonnet-key"];
  expect(entry.model).toBe("sonnet");
  expect(entry.max_concurrent).toBeUndefined();
  expect(entry.timeout_seconds).toBeUndefined();
  expect(location.hash).toContain("settings/models/sonnet-key");
});

test("Finish and save tests the credential, then stores it with its draft before the document that names it", async () => {
  show();
  await walkToReview();
  // REVIEW TESTED THE KEY AND CHECKED THE CHANGE as it opened; the save stores it.
  expect(calls).toEqual(["test", "read", "dry-run", "dry-run"]);
  press("Finish and save");
  await waitFor(() => expect(calls.at(-1)).toMatch(/^write:/));
  const tested = vi.mocked(api.testCredential).mock.calls[0]!;
  expect(tested[1]).toMatchObject({
    values: { ANTHROPIC_API_KEY_SONNET_KEY: "sk-ant-api03-new" },
    createOnly: true,
  });
  expect(tested[1].entity).toMatchObject({
    auth: { mode: "api-key", api_key: "${ANTHROPIC_API_KEY_SONNET_KEY}" },
  });

  expect(calls.slice(4)).toEqual([
    "store",
    "write:Add model sonnet-key in the default-fleet fleet",
  ]);
  const stored = vi.mocked(api.storeCredential).mock.calls[0]!;
  expect(stored[1].entity).toBeDefined();
  expect(stored[1].createOnly).toBe(true);
  expect(stored[2]).toBe('"rev-1"');
  const written = vi.mocked(api.writeDocument).mock.calls.at(-1)!;
  expect(written[1]).toMatchObject({ etag: '"rev-1"', dryRun: false });
  expect(JSON.stringify(written[0])).not.toContain("sk-ant-api03-new");
  expect(location.hash).toContain("settings/models/sonnet-key");
});

/**
 * AN EDIT SAVED RETURNS TO WHERE IT WAS OPENED, as Cancel does. Renew token
 * or Replace API key from a row's menu on the list is opened over the list,
 * and the save hands the page back to it through
 * the wizard's own close; it does not travel to the model's page, which took
 * a person working down the list away from it. Mutation: send every save to
 * the model's page, and both readings are red.
 */
test("an edit saved returns to where it was opened, never to the model's page", async () => {
  location.hash =
    "#/settings/models?sort=status&flow=edit&entry=sonnet&step=signin&focus=credential";
  const { onClose } = show({ editing: "sonnet", target: { step: "signin", focus: "credential" } });
  fireEvent.change(keyField(), { target: { value: "sk-ant-api03-rotated" } });
  toReview();
  await screen.findByText("The configuration is valid");
  press("Finish and save");
  await waitFor(() => expect(calls.at(-1)).toMatch(/^write:/));
  await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1));
  expect(location.hash).not.toContain("settings/models/sonnet");
  expect(location.hash).toContain("sort=status");
});

/**
 * THE REVIEW SAYS ONCE WHAT SAVING MAKES: the lead says what the step shows,
 * and the engine's acceptance that saving makes one revision. Mutation: say
 * the revision in the lead again, and this is red.
 */
test("the review says once that saving makes one revision", async () => {
  location.hash = "#/settings/models?flow=edit&entry=sonnet&step=signin&focus=credential";
  show({ editing: "sonnet", target: { step: "signin", focus: "credential" } });
  fireEvent.change(keyField(), { target: { value: "sk-ant-api03-rotated" } });
  toReview();
  await screen.findByText("The configuration is valid");
  expect(document.body.textContent?.match(/one revision/g)).toHaveLength(1);
});

/**
 * A SAVE SAYS WHICH WRITE IS RUNNING. Storing a credential can run the harness
 * on the engine, which takes seconds: the form names the write it is waiting
 * on and counts the wait, so it never reads as stopped. Mutation: leave the
 * button's spinner as the only sign, and this is red.
 */
test("a save names the write it is waiting on while it runs", async () => {
  show({ editing: "sonnet", target: { step: "signin", focus: "credential" } });
  fireEvent.change(keyField(), { target: { value: "sk-ant-api03-rotated" } });
  toReview();
  await screen.findByText("The configuration is valid");
  let stored!: (value: unknown) => void;
  vi.mocked(api.storeCredential).mockImplementationOnce(() => {
    calls.push("store");
    return new Promise((resolve) => (stored = resolve)) as never;
  });
  let written!: (value: unknown) => void;
  vi.mocked(api.writeDocument).mockImplementationOnce(() => {
    calls.push("write");
    return new Promise((resolve) => (written = resolve)) as never;
  });
  press("Finish and save");
  const storing = await screen.findByRole("status");
  expect(storing.textContent).toMatch(/^Storing the key and re-activating the company/);
  expect(document.querySelector(".models-waiting__elapsed")?.textContent).toMatch(/^\d+ s$/);
  await act(async () =>
    stored({
      kind: "ok",
      status: 200,
      value: {
        verified: "",
        check: { at: "", node: "node-1", ok: true, lines: [] },
        names: [],
        hints: {},
        entries: [],
        reload: null,
        cleared: [],
        running_runs: 0,
        limited: null,
      },
    }),
  );
  await waitFor(() =>
    expect(screen.getByRole("status").textContent).toBe("Saving the model as one revision."),
  );
  await act(async () =>
    written({ kind: "saved", revisionId: "rev-2", epoch: 2, warnings: [], derived: null }),
  );
});

/**
 * A SAVE THAT HAS STARTED RUNS TO ITS END, whatever happens to the wizard:
 * left between the credential and the document, it still reads the revision
 * the credential re-activated and writes the document, so no sealed secret is
 * left that no document names. Only what the wizard does with the answers is
 * gated by its lifetime: nothing moves the page the person went on to.
 */
test("a save left between its two writes still makes both, and moves nothing on screen", async () => {
  show();
  await walkToReview();
  let stored!: (value: unknown) => void;
  vi.mocked(api.storeCredential).mockImplementationOnce(() => {
    calls.push("store");
    return new Promise((resolve) => (stored = resolve)) as never;
  });
  press("Finish and save");
  await waitFor(() => expect(calls.at(-1)).toBe("store"));
  location.hash = "#/somewhere-else";
  cleanup();
  await act(async () =>
    stored({
      kind: "ok",
      status: 200,
      value: {
        verified: "",
        check: { at: "", node: "node-1", ok: true, lines: [] },
        names: [],
        hints: {},
        entries: [],
        reload: { revision_id: "rev-1b", epoch: 2 },
        cleared: [],
        running_runs: 0,
        limited: null,
      },
    }),
  );
  await waitFor(() => expect(calls.at(-1)).toMatch(/^write:/));
  expect(calls.slice(-3)).toEqual([
    "store",
    "read",
    "write:Add model sonnet-key in the default-fleet fleet",
  ]);
  // NO SIGNAL rides a write that has started: nothing could stop it part way.
  const written = vi.mocked(api.writeDocument).mock.calls.at(-1)!;
  expect(written[2]).toBeUndefined();
  expect(location.hash).toBe("#/somewhere-else");
});

/**
 * ACME'S SPLIT SURVIVES AN EDIT OF ONE SIDE. `default` codes for the
 * engineers and QA; `default-host`, its coding on the engine's host, for the
 * PM and the designer. An edit keeps the seats the model serves, and Review
 * lists every seat that changes model, first.
 */
const SPLIT_DOC = {
  name: "Acme",
  providers: {
    llm: {
      default: { harness: "claude-code", model: "opus", auth: { mode: "api-key" } },
      "default-host": {
        harness: "claude-code",
        model: "opus",
        run_in: "direct",
        auth: { mode: "api-key" },
      },
    },
  },
  roles: [
    { name: "Backend Engineer", handle: "backend-engineer", llm: "default" },
    { name: "Quality Lead", handle: "qa", llm: "default" },
    { name: "Product Manager", handle: "pm", llm: "default-host" },
    { name: "Designer", handle: "designer", llm: "default-host" },
  ],
} as unknown as CompanyDocument;

function splitData(): ModelsData {
  const key = signIn({ route: "api-key", mode: "api-key", variable: "ANTHROPIC_API_KEY" });
  return modelsData({
    doc: SPLIT_DOC,
    entries: [
      modelEntry({
        key: "default",
        model: "opus",
        sign_in: key,
        used_by: [...runs("backend-engineer", ...EVERY_JOB), ...runs("qa", ...EVERY_JOB)],
      }),
      modelEntry({
        key: "default-host",
        model: "opus",
        sign_in: key,
        used_by: [...runs("pm", ...EVERY_JOB), ...runs("designer", ...EVERY_JOB)],
      }),
    ],
    seats: [
      seat("backend-engineer", "Backend Engineer"),
      seat("qa", "Quality Lead"),
      seat("pm", "Product Manager"),
      seat("designer", "Designer"),
    ],
  });
}

test("Acme's coding split survives an edit of default for its key", async () => {
  vi.mocked(getConfig).mockImplementation(async () => {
    calls.push("read");
    return { kind: "document", document: structuredClone(SPLIT_DOC), etag: '"rev-1"' };
  });
  const d = splitData();
  render(
    <Router>
      <ModelWizard
        data={d}
        editing={d.models.find((m) => m.key === "default")!}
        target={{ step: "signin", focus: "credential" }}
        onClose={() => {}}
        onDevice={() => {}}
      />
    </Router>,
  );
  fireEvent.change(keyField(), { target: { value: "sk-ant-api03-rotated" } });
  for (const step of ["Model", "Where it runs", "Limits", "Review"]) {
    press("Continue");
    expect(heading()).toBe(step);
  }
  await screen.findByText("The configuration is valid");
  // NO SEAT MOVES ONTO default: the PM and the designer stay where they are.
  expect(screen.queryByRole("group", { name: "Who uses a model changes from today" })).toBeNull();
  press("Finish and save");
  await waitFor(() => expect(calls.at(-1)).toMatch(/^write:/));
  const written = vi.mocked(api.writeDocument).mock.calls.at(-1)![0] as unknown as {
    roles: { handle: string; llm: unknown }[];
  };
  const llmOf = (h: string) => written.roles.find((r) => r.handle === h)?.llm;
  expect(llmOf("pm")).toBe("default-host");
  expect(llmOf("designer")).toBe("default-host");
  expect(llmOf("backend-engineer")).toBe("default");
  expect(llmOf("qa")).toBe("default");
});

test("Review lists each seat an edit moves, by name, from where and to where, before the rest", async () => {
  const d = splitData();
  render(
    <Router>
      <ModelWizard
        data={d}
        editing={d.models.find((m) => m.key === "default")!}
        target={{ step: "review", add: [{ job: "coding", seat: "pm" }] }}
        onClose={() => {}}
        onDevice={() => {}}
      />
    </Router>,
  );
  toReview();
  const moves = await screen.findByRole("group", { name: "Who uses a model changes from today" });
  expect(moves.textContent).toContain("Product Manager");
  expect(moves.textContent).toContain("CodingChanged from default-host to default");
  expect(moves.textContent).not.toMatch(/Designer|Backend Engineer|Quality Lead/);
  // FIRST, above the model's own summary.
  const head = document.querySelector(".models-review-head")!;
  expect(moves.compareDocumentPosition(head) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  await act(async () => {});
});

/**
 * "DID NOT ANSWER" ONLY WHERE THE ENGINE'S ANSWER NEVER ARRIVED, and "may
 * have been stored" only for the save: a check is a read and dry runs, which
 * store nothing whoever answered.
 */
describe.each([
  [0, false, "the engine did not answer within 30 seconds", "The engine did not answer."],
  [504, false, "a 504 came back without the engine's JSON", "The engine did not answer."],
  [500, true, "store: disk full", "The engine failed: store: disk full."],
  // A PROXY'S 403 OR 413, as the classifier reads one without the engine's mark.
  [403, false, "blocked by policy", "The engine did not answer."],
  [413, false, "blocked by policy", "The engine did not answer."],
] as const)("an answer of status %i (by the engine: %s)", (status, answered, detail, said) => {
  const unsettled = { kind: "unsettled", answered, status, detail } as const;
  const openReview = async () => {
    const d = splitData();
    render(
      <Router>
        <ModelWizard
          data={d}
          editing={d.models.find((m) => m.key === "default")!}
          target={{ step: "review", add: [{ job: "coding", seat: "pm" }] }}
          onClose={() => {}}
          onDevice={() => {}}
        />
      </Router>,
    );
    toReview();
  };
  const blocked = async () => {
    const title = await screen.findByText("This cannot be saved from here yet");
    return title.closest(".crewlet-callout")?.textContent?.replace(title.textContent ?? "", "");
  };

  test("at the check says what happened, and that nothing was stored", async () => {
    vi.mocked(api.writeDocument).mockImplementation(async () => unsettled);
    await openReview();
    expect(await blocked()).toBe(`${said} Nothing was stored. Check the change again.`);
  });

  test("at the save says what happened, and that it may have been stored", async () => {
    vi.mocked(api.writeDocument).mockImplementation(async (_doc, options) =>
      options.dryRun
        ? { kind: "valid", baseRevisionId: "rev-1", warnings: [], derived: null }
        : unsettled,
    );
    await openReview();
    await screen.findByText("The configuration is valid");
    press("Finish and save");
    expect(await blocked()).toBe(
      `${said} The change may or may not have been stored. Reopen the model before trying again.`,
    );
  });
});

test("a place the company never set up says so in plain words, never the configuration's own sentence", () => {
  show({ editing: "default", target: { step: "runs" } });
  expect(heading()).toBe("Where it runs");
  expect(document.body.textContent).toContain(
    "E2B isn't set up for this company. Set it up in Configuration.",
  );
  expect(document.body.textContent).not.toMatch(/declares no|E2B key not set|providers\.sandbox/);
  // M-017: THE FACT IS A STATUS, not "Ready" over a reason it is not.
  expect(
    screen.queryAllByText("Ready", { selector: "dt, .crewlet-choice-card__fact-label" }),
  ).toEqual([]);
});

test("a refused key is said at the key, and the Method step is where it is fixed", async () => {
  vi.mocked(api.testCredential).mockResolvedValue({
    kind: "refused",
    status: 422,
    code: "credential_rejected",
    sentence: "invalid x-api-key. Nothing was stored.",
    detail: "",
    hint: "",
    body: {
      problems: [
        {
          path: "values.ANTHROPIC_API_KEY_SONNET_KEY",
          segments: ["values", "ANTHROPIC_API_KEY_SONNET_KEY"],
          kind: "invalid",
          message: "invalid x-api-key. Nothing was stored.",
        },
      ],
    },
  });
  show();
  await walkToReview("sonnet-key", { accepted: false });
  // REVIEW'S TEST CALL SAYS IT IN ONE LINE, and nothing is checked or written.
  expect(await screen.findByText("Failed: invalid x-api-key. Nothing was stored.")).toBeTruthy();
  expect(screen.getByRole("button", { name: "Test again" })).toBeTruthy();
  expect(calls.some((c) => c === "dry-run" || c.startsWith("write:"))).toBe(false);
  expect(document.body.textContent).not.toMatch(/values\./);
  fireEvent.click(
    within(screen.getByRole("list", { name: /^Steps to/ })).getByRole("button", { name: /Method/ }),
  );
  expect(heading()).toBe("Method");
  expect(screen.getByText("Invalid x-api-key. Nothing was stored.")).toBeTruthy();
});

test("a secret name already stored is said at Stored as, in words", async () => {
  vi.mocked(api.storeCredential).mockResolvedValue({
    kind: "refused",
    status: 412,
    code: "already_exists",
    sentence: "already exists",
    detail: "",
    hint: "",
    body: { names: ["ANTHROPIC_API_KEY_SONNET_KEY"] },
  });
  show();
  await walkToReview();
  press("Finish and save");
  await screen.findByText(/ANTHROPIC_API_KEY_SONNET_KEY is already stored/);
  expect(calls.some((c) => c.startsWith("write:"))).toBe(false);
  press("Fix in Method");
  expect(heading()).toBe("Method");
  expect(
    screen.getAllByText(/is already stored, and another model may read it/).length,
  ).toBeGreaterThan(0);
});

test("a key deep link opens quiet, focused, and says what is wrong only after Continue", async () => {
  show({ editing: "sonnet", target: { step: "signin", focus: "credential" } });
  expect(heading()).toBe("Method");
  expect(screen.queryByText("Paste the API key.")).toBeNull();
  expect(
    screen.getByText("Paste the new key. It is tested before it replaces the stored one."),
  ).toBeTruthy();
  expect(document.querySelector("[data-status='attention'], .is-attention")).toBeNull();
  const field = keyField();
  await waitFor(() => expect(document.activeElement).toBe(field));

  press("Continue");
  expect(screen.getByText("Paste the API key.")).toBeTruthy();
  await waitFor(() => expect(document.activeElement?.getAttribute("aria-invalid")).toBe("true"));
});

test("Cancel asks before throwing a filled wizard away, and not before an empty one", async () => {
  const empty = show();
  press("Cancel");
  expect(empty.onClose).toHaveBeenCalledTimes(1);
  cleanup();

  const filled = show();
  fireEvent.change(screen.getByRole("textbox", { name: /Name/ }), { target: { value: "draft" } });
  press("Cancel");
  expect(filled.onClose).not.toHaveBeenCalled();
  expect(screen.getByText("Discard this model?")).toBeTruthy();
  press("Keep editing");
  expect(filled.onClose).not.toHaveBeenCalled();
  press("Cancel");
  press("Discard");
  expect(filled.onClose).toHaveBeenCalledTimes(1);
});

test("a Codex model on a ChatGPT sign-in is created, then handed to the device sign-in", async () => {
  answers.llm_models = {
    source: "models.dev",
    fetched_at: null,
    provider: "openai",
    aliases: [{ id: "gpt-6", name: "GPT-6", detail: "" }],
    models: [],
    docs_url: "",
  };
  const { onDevice } = show();
  fireEvent.change(screen.getByRole("textbox", { name: /Name/ }), {
    target: { value: "codex-review" },
  });
  fireEvent.click(screen.getByRole("radio", { name: /Codex/ }));
  press("Continue");
  fireEvent.click(screen.getByRole("radio", { name: /ChatGPT sign-in/ }));
  press("Continue");
  fireEvent.click(screen.getByRole("radio", { name: /GPT-6/ }));
  press("Continue");
  expect(heading()).toBe("Limits");
  press("Continue");
  expect(heading()).toBe("Review");
  await screen.findByText("The configuration is valid");
  // NOTHING TO TEST BEFORE THE SAVE: the row says when it is.
  expect(screen.getByText("After it is saved")).toBeTruthy();
  press("Finish and save");
  await waitFor(() => expect(onDevice).toHaveBeenCalledWith("codex-review"));
  // A new device sign-in is made after the save, so nothing is tested before it.
  expect(checkModel).not.toHaveBeenCalled();
  expect(api.storeCredential).not.toHaveBeenCalled();
  const written = vi.mocked(api.writeDocument).mock.calls.at(-1)![0] as Record<string, any>;
  expect(written.providers.llm["codex-review"].auth).toEqual({ mode: "subscription" });
});

/**
 * THE MODEL A JOB RAN ON FIRST IS NEVER CALLED A FALLBACK. Quality Lead's
 * reviewing runs on default, with sonnet behind it; giving it to sonnet moves
 * it off default, which the change list says, and drops no fallback at all.
 */
test("Review lists a job's first model as the change, never as a fallback, and says the test call has not run", async () => {
  show({ editing: "sonnet", target: { step: "review", add: [{ job: "reviews", seat: "qa" }] } });
  toReview();
  await screen.findByText("The configuration is valid");
  const moves = screen.getByRole("group", { name: "Who uses a model changes from today" });
  expect(moves.textContent).toContain("Quality Lead");
  expect(moves.textContent).toContain("Reviewing and delegationsChanged from default to sonnet");
  expect(screen.queryByText(/as a fallback/)).toBeNull();
  expect(screen.getByText("Not run")).toBeTruthy();
  expect(screen.getByRole("button", { name: "Run test call" })).toBeTruthy();
  await act(async () => {});
});

/** Review's list of who changes model, as one line of text; empty where nobody moves. */
const movesSaid = () =>
  screen.queryByRole("group", { name: "Who uses a model changes from today" })?.textContent ?? "";

/**
 * QA M10: seats moved off a limit arrive at Review already added, on the
 * model chosen and on a model being added alike, and nothing else is.
 */
test("the seats an alarm moves arrive added at Review, and only those", () => {
  const add: WizardAt["add"] = [
    { job: "coding", seat: "pm" },
    { job: "routines", seat: "qa" },
  ];
  show({ editing: "sonnet", target: { step: "review", add } });
  expect(heading()).toBe("Review");
  expect(movesSaid()).toContain("Product Manager");
  expect(movesSaid()).toContain("CodingChanged from default to sonnet");
  expect(movesSaid()).toContain("Quality Lead");
  expect(movesSaid()).toContain("Routines and evaluationsChanged from default to sonnet");
  expect(movesSaid()).not.toMatch(/Backend Engineer|Reviewing and delegations/);
  cleanup();
  show({ target: { step: "review", add } });
  expect(movesSaid()).toContain("Product Manager");
  expect(movesSaid()).toContain("Quality Lead");
  expect(movesSaid()).not.toMatch(/Backend Engineer/);
});

/**
 * ONLY A SEAT THE COMPANY HAS NOW: a stale or hand-typed seat is dropped
 * without a word, so a link kept from before a seat was removed opens the
 * wizard as if it had never named it.
 */
test("a seat the company no longer has, named in the address, is not moved", () => {
  show({
    editing: "sonnet",
    target: { step: "review", add: [{ job: "coding", seat: "removed-seat" }] },
  });
  expect(heading()).toBe("Review");
  expect(movesSaid()).toBe("");
  cleanup();
  show({
    editing: "sonnet",
    target: {
      step: "review",
      add: [
        { job: "coding", seat: "removed-seat" },
        { job: "coding", seat: "pm" },
      ],
    },
  });
  expect(movesSaid()).toContain("Product Manager");
  expect(movesSaid()).not.toMatch(/removed-seat|Backend Engineer|Quality Lead/);
});

test("coding named in the address is not moved on a harness that runs no coding", () => {
  const d = data();
  d.models.push(
    ...modelsData({
      entries: [modelEntry({ key: "codex-review", harness: "codex", model: "gpt-6.1" })],
    }).models,
  );
  const editing = d.models.find((m) => m.key === "codex-review")!;
  render(
    <Router>
      <ModelWizard
        data={d}
        editing={editing}
        target={{
          step: "review",
          add: [
            { job: "coding", seat: "pm" },
            { job: "routines", seat: "pm" },
          ],
        }}
        onClose={() => {}}
        onDevice={() => {}}
      />
    </Router>,
  );
  expect(heading()).toBe("Review");
  expect(movesSaid()).toContain("Routines and evaluations");
  expect(movesSaid()).not.toContain("Coding");
});

/**
 * A PASTED KEY NEVER LEAVES THE PAGE, read where it would leave: the page
 * context the Dashboard Assistant is sent, whole, as text. The field list's
 * own test proves today's fields; this one fails if any later field reads
 * the credential, or the secret it is stored under, however indirectly.
 */
test("a key typed into the wizard never reaches what the assistant is sent", async () => {
  const SENTINEL = "sk-test-SENTINEL";
  setFramePage({
    screen: "models",
    title: "Settings › Models & Fleets",
    path: ["settings", "models"],
    params: {},
    query: {},
  });
  show();
  fireEvent.change(screen.getByRole("textbox", { name: /Name/ }), { target: { value: "probe" } });
  press("Continue");
  fireEvent.change(keyField(), { target: { value: SENTINEL } });
  // THE SNAPSHOT IS THE LIVE DRAFT: it names the model typed a step ago, so an
  // absent sentinel means the key was left out, not that nothing was read.
  await waitFor(() => expect(pageSnapshot()?.context.draft?.step).toBe("signin"));
  const sent = JSON.stringify(pageSnapshot());
  expect(sent).toContain("probe");
  expect(sent).not.toContain("SENTINEL");
  expect(sent).not.toContain("ANTHROPIC_API_KEY_PROBE");
});

test("focus starts on the step's heading, and Cancel gives it back to what opened the wizard", async () => {
  function Page() {
    const [open, setOpen] = useState(false);
    return open ? (
      <ModelWizard data={data()} onClose={() => setOpen(false)} onDevice={() => {}} />
    ) : (
      <button type="button" onClick={() => setOpen(true)}>
        Add model
      </button>
    );
  }
  render(
    <Router>
      <Page />
    </Router>,
  );
  const add = screen.getByRole("button", { name: "Add model" });
  add.focus();
  fireEvent.click(add);
  await waitFor(() =>
    expect(document.activeElement?.classList.contains("crewlet-setup-wizard__heading")).toBe(true),
  );
  press("Cancel");
  await waitFor(() =>
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Add model" })),
  );
});

test("a secret name the reader types is kept, whatever the name of the model becomes", () => {
  show();
  fireEvent.change(screen.getByRole("textbox", { name: /Name/ }), { target: { value: "first" } });
  press("Continue");
  const storedAs = screen.getByLabelText(/Stored as/);
  fireEvent.change(storedAs, { target: { value: "MY_OWN_KEY" } });
  expect((screen.getByLabelText(/Stored as/) as HTMLInputElement).value).toBe("MY_OWN_KEY");
  press("Back");
  fireEvent.change(screen.getByRole("textbox", { name: /Name/ }), { target: { value: "second" } });
  press("Continue");
  expect((screen.getByLabelText(/Stored as/) as HTMLInputElement).value).toBe("MY_OWN_KEY");
});

/**
 * THE MODEL STEP FOLLOWS THE PROVIDER: a model typed for Anthropic is cleared
 * when the provider becomes OpenAI, with its key, and the step's example, hint
 * and secret name are OpenAI's. Kept, an Anthropic id was saved as `openai/...`.
 */
test("a changed provider clears the model and key typed for the last one, and the step shows the new one's example", () => {
  answers.llm_models = { source: "none", aliases: [], models: [], reason: "" };
  show();
  fireEvent.change(screen.getByRole("textbox", { name: /Name/ }), { target: { value: "oc" } });
  fireEvent.click(screen.getByRole("radio", { name: /OpenCode/ }));
  press("Continue");
  fireEvent.change(keyField(), { target: { value: "sk-ant-api03-fake" } });
  press("Continue");
  const model = () => screen.getByRole("combobox", { name: /^Model/ }) as HTMLInputElement;
  expect(model().placeholder).toBe("claude-sonnet-5-5");
  fireEvent.change(model(), { target: { value: "claude-sonnet-5-5" } });
  expect(screen.getByText(/Sent as anthropic\/claude-sonnet-5-5\./)).toBeTruthy();

  press("Back");
  fireEvent.click(screen.getByRole("combobox", { name: /Provider/ }));
  fireEvent.mouseDown(screen.getByRole("option", { name: "OpenAI" }));
  expect((keyField() as HTMLInputElement).value).toBe("");
  expect(keyField().getAttribute("placeholder")).toBe("sk-…");
  expect((screen.getByLabelText(/Stored as/) as HTMLInputElement).value).toBe("OPENAI_API_KEY");
  fireEvent.change(keyField(), { target: { value: "sk-fake-openai" } });
  press("Continue");
  expect(heading()).toBe("Model");
  expect(model().value).toBe("");
  expect(model().placeholder).toBe("gpt-5.5");
  expect(screen.getByText(/Sent as openai\/<model>\./)).toBeTruthy();
});

/**
 * THE STORED KEY IS ITS OWN SCOPE'S: replacing it ends when the method moves,
 * so "Keep the stored key" never names the old secret for another sign-in.
 */
test("a replaced key's Keep is offered only on its own method, and the secret follows the method", () => {
  show({ editing: "sonnet", target: { step: "signin", focus: "credential" } });
  expect(screen.getByRole("button", { name: /Keep the stored key/ })).toBeTruthy();
  const method = (value: string) =>
    fireEvent.click(document.querySelector(`input[type="radio"][value="${value}"]`)!);
  method("token");
  expect(screen.queryByRole("button", { name: /Keep the stored/ })).toBeNull();
  expect((screen.getByLabelText(/Stored as/) as HTMLInputElement).value).toBe(
    "CLAUDE_CODE_OAUTH_TOKEN",
  );
  method("api-key");
  expect(screen.getByText("ANTHROPIC_API_KEY_SONNET")).toBeTruthy();
  expect(screen.getByRole("button", { name: /Replace the key/ })).toBeTruthy();
});

/** A SUBSCRIPTION TOKEN IS ASKED FOR WITH ITS COMMAND ALONE: no notice about whose subscription it is. */
test("the token sign-in shows its command and no subscription notice", () => {
  show({ editing: "sonnet", target: { step: "signin", focus: "credential" } });
  fireEvent.click(document.querySelector('input[type="radio"][value="token"]')!);
  expect(screen.getByText("claude setup-token")).toBeTruthy();
  expect(screen.queryByText(/own subscription|terms|serve other people/i)).toBeNull();
});

/** A model check whose lines are `lines`, as the engine answers one. */
const checked = (
  lines: { check: string; ok: boolean; detail: string; kind?: string; remedy?: string }[],
) => ({
  kind: "ok" as const,
  status: 200,
  value: { at: "", node: "node-1", ok: lines.every((l) => l.ok), lines },
});

/**
 * FINISH AND SAVE IS ONE CLICK FROM ANY STEP OF AN EDIT: the model's test call,
 * the engine's check, then the save, with no stop on Review.
 */
test("Finish and save tests the model, checks the change and saves it in one click", async () => {
  checkModel.mockImplementation(async () => {
    calls.push("check");
    return checked([{ check: "smoke", ok: true, detail: "It answers." }]);
  });
  const { onClose } = show({ editing: "sonnet", target: { step: "limits" } });
  expect(screen.queryByRole("button", { name: "Review changes" })).toBeNull();
  press("Finish and save");
  await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1));
  expect(checkModel).toHaveBeenCalledWith(
    "sonnet",
    expect.objectContaining({ smoke: true }),
    expect.anything(),
  );
  expect(calls).toEqual(["check", "read", "dry-run", "dry-run", "write:Edit model sonnet"]);
});

test("Finish and save with a pasted key tests the key, stores it and writes the change", async () => {
  const { onClose } = show({ editing: "sonnet", target: { step: "signin", focus: "credential" } });
  fireEvent.change(keyField(), { target: { value: "sk-ant-api03-rotated" } });
  press("Finish and save");
  await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1));
  expect(calls).toEqual(["test", "read", "dry-run", "dry-run", "store", "write:Edit model sonnet"]);
  expect(document.body.textContent).not.toContain("sk-ant-api03-rotated");
});

test("a failed test call saves nothing, stays, and says what failed and how to fix it", async () => {
  checkModel.mockImplementation(async () =>
    checked([
      { check: "binary", ok: true, detail: "claude" },
      {
        check: "sign-in",
        ok: false,
        detail: "401",
        kind: "auth",
        remedy: "Replace the key on the Method step.",
      },
    ]),
  );
  const { onClose } = show({ editing: "sonnet", target: { step: "limits" } });
  press("Finish and save");
  const title = await screen.findByText("Not saved: the test call failed");
  expect(title.closest(".crewlet-callout")?.textContent).toContain(
    "Replace the key on the Method step.",
  );
  expect(calls.some((c) => c.startsWith("write:") || c === "dry-run")).toBe(false);
  expect(onClose).not.toHaveBeenCalled();
  expect(heading()).toBe("Limits");
  press("See it on Review");
  expect(heading()).toBe("Review");
  // REVIEW'S TEST CALL ROW says what failed, in one line, with its button to run it again.
  expect(screen.getByText(/^Failed: .*Replace the key on the Method step\.$/)).toBeTruthy();
  expect(screen.getByRole("button", { name: "Test again" })).toBeTruthy();
});

test("a save the engine refuses stays in the wizard and says nothing may be lost", async () => {
  checkModel.mockImplementation(async () =>
    checked([{ check: "smoke", ok: true, detail: "It answers." }]),
  );
  vi.mocked(api.writeDocument).mockImplementation(async (_doc, options) =>
    options.dryRun
      ? { kind: "valid", baseRevisionId: "rev-1", warnings: [], derived: null }
      : ({ kind: "conflict", reason: "stale", currentRevisionId: null } as never),
  );
  const { onClose } = show({ editing: "sonnet", target: { step: "limits" } });
  press("Finish and save");
  const title = await screen.findByText("Not saved");
  expect(title.closest(".crewlet-callout")?.textContent).toContain(
    "The configuration changed since this was checked, so nothing was stored.",
  );
  expect(onClose).not.toHaveBeenCalled();
});

test("Finish and save is busy while it runs and is never sent twice", async () => {
  let answer!: (value: unknown) => void;
  checkModel.mockImplementation(() => new Promise((resolve) => (answer = resolve)));
  show({ editing: "sonnet", target: { step: "limits" } });
  press("Finish and save");
  const button = screen.getByRole("button", { name: /Finish and save/ });
  expect(button.hasAttribute("disabled") || button.getAttribute("aria-disabled") === "true").toBe(
    true,
  );
  expect((await screen.findByRole("status")).textContent).toMatch(
    /^Testing the model with one harness run/,
  );
  fireEvent.click(button);
  expect(checkModel).toHaveBeenCalledTimes(1);
  await act(async () => answer(checked([{ check: "smoke", ok: true, detail: "It answers." }])));
});

// M-015: AN EDIT SHOWS ITS ONE HARNESS, the rule said once, not a refusal on
// every other harness's card.
test("an edit's first step shows its one harness and says the rule once", () => {
  show({ editing: "sonnet", target: { step: "harness" } });
  const harness = screen.getByRole("radiogroup", { name: "Harness" });
  expect(within(harness).getAllByRole("radio")).toHaveLength(1);
  expect(document.body.textContent?.match(/keeps its/g)).toHaveLength(1);
});

// M-014: A NEW NAME STOPS AT ITS LIMIT, and its one rule is the field's line.
test("a new name's field stops at 40 characters and says the rule it is held to", () => {
  show();
  const name = screen.getByRole("textbox", { name: /Name/ }) as HTMLInputElement;
  expect(name.maxLength).toBe(40);
  expect(
    screen.getByText(
      "Lowercase letters, digits, dots, hyphens and underscores, up to 40 characters.",
    ),
  ).toBeTruthy();
});

// M-016: A FAILED TEST SAYS ITS FIRST SENTENCE, the engine's chain on hover,
// and the valid-change callout is not worded as the test passing.
test("a failed test call says its first sentence, with the engine's chain on hover", async () => {
  checkModel.mockImplementation(async () =>
    checked([
      {
        check: "sign-in",
        ok: false,
        detail: "401",
        kind: "auth",
        remedy: "failed: llm harness/claude-code/haiku: auth: Invalid API key",
      },
    ]),
  );
  show({ editing: "sonnet", target: { step: "limits" } });
  toReview();
  await screen.findByText("The configuration is valid");
  fireEvent.click(screen.getByRole("button", { name: "Run test call" }));
  const line = await screen.findByText(/^Failed: /);
  expect(line.textContent).not.toMatch(/llm harness|failed:/);
  expect(line.getAttribute("title")).toMatch(/llm harness\/claude-code\/haiku/);
});

// M-019: A RELOAD ASKS FIRST while something is typed, and not before.
test("a reload asks before it throws a typed draft away, and not before anything is typed", () => {
  show();
  const reload = () => {
    const event = new Event("beforeunload", { cancelable: true });
    window.dispatchEvent(event);
    return event.defaultPrevented;
  };
  expect(reload()).toBe(false);
  fireEvent.change(screen.getByRole("textbox", { name: /Name/ }), { target: { value: "draft" } });
  expect(reload()).toBe(true);
});

// M-002: A FIRST CODING MODEL ON A COMPANY WITH NO PLACE TO RUN CODING. The
// engine checks a draft's test against the company as it stands, which refuses
// it, so Finish and save sets this host up first, then tests and saves; opening
// Review writes nothing.
test("with no sandbox, Finish and save sets up this host, then tests and saves the first coding model", async () => {
  const doc = structuredClone(DOC) as Record<string, any>;
  delete doc.providers.sandbox;
  vi.mocked(getConfig).mockImplementation(async () => {
    calls.push("read");
    return { kind: "document", document: structuredClone(doc), etag: '"rev-1"' };
  });
  const written: Record<string, any>[] = [];
  vi.mocked(api.writeDocument).mockImplementation(async (next, options) => {
    calls.push(options.dryRun ? "dry-run" : `write:${options.summary}`);
    if (!options.dryRun) {
      written.push(next as Record<string, any>);
      Object.assign(doc, structuredClone(next));
    }
    return options.dryRun
      ? { kind: "valid", baseRevisionId: "rev-1", warnings: [], derived: null }
      : { kind: "saved", revisionId: "rev-2", epoch: 2, warnings: [], derived: null };
  });
  const d = data();
  render(
    <Router>
      <ModelWizard
        data={{ ...d, doc: structuredClone(doc) as never }}
        onClose={() => {}}
        onDevice={() => {}}
      />
    </Router>,
  );
  await walkToReview("sonnet-key", { accepted: true });
  // OPENING REVIEW TESTED NOTHING AND WROTE NOTHING: the test waits for the save.
  expect(calls).toEqual(["read", "dry-run", "dry-run"]);
  expect(screen.getByText("Runs when you save")).toBeTruthy();
  expect(screen.getByText("On this engine's host, set up by this save")).toBeTruthy();
  calls.length = 0;
  press("Finish and save");
  await waitFor(() => expect(calls.at(-1)).toMatch(/^write:Add model/));
  expect(calls.slice(0, 4)).toEqual([
    "read",
    "dry-run",
    "write:Set up running coding on this engine's host",
    "test",
  ]);
  expect(written[0]!.providers.sandbox).toEqual({ local: {}, default_run_in: "direct" });
});

// M-004: A PROGRAM MISSING ON THE ENGINE HOST flags no step and offers none to
// fix it in, and Finish and save is held while Review lists it.
test("a harness missing on the host is said plainly, flags no step, and holds Finish and save", async () => {
  vi.mocked(api.testCredential).mockResolvedValue({
    kind: "refused",
    status: 422,
    code: "validation_error",
    sentence: "The configuration this change would produce is not valid, so nothing was stored.",
    detail: "",
    hint: "",
    body: {
      problems: [
        {
          path: "providers.llm.sonnet-key.harness",
          segments: ["providers", "llm", "sonnet-key", "harness"],
          kind: "invalid",
          message:
            'not on PATH "claude" is not on this host\'s PATH \u2014 the harness runs on the ENGINE host, so it must be installed here',
        },
      ],
    },
  } as never);
  show();
  await walkToReview("sonnet-key", { accepted: false });
  await screen.findByText(/^Failed: /);
  expect(document.body.textContent).toContain(
    "Install claude on the engine host: the harness runs there.",
  );
  expect(document.body.textContent).not.toMatch(/PATH|ENGINE host/);
  expect(screen.queryByRole("button", { name: /^Fix in/ })).toBeNull();
  const steps = screen.getByRole("list", { name: /^Steps to/ });
  expect(within(steps).queryByText("Needs attention")).toBeNull();
  const finish = screen.getByRole("button", { name: "Finish and save" });
  expect(finish.hasAttribute("disabled") || finish.getAttribute("aria-disabled") === "true").toBe(
    true,
  );
});

// A COMPANY'S FIRST MODEL DOES EVERYTHING, the helpers included, and keeps
// them through a harness that runs none and back; Review lists each move.
/** The company with no model yet: three seats naming none, both helpers off. */
const NO_MODEL = {
  name: "Acme",
  providers: { sandbox: { local: {}, default_run_in: "direct" }, llm: {} },
  roles: [
    { name: "Backend Engineer", handle: "backend-engineer" },
    { name: "Product Manager", handle: "pm" },
    { name: "Quality Lead", handle: "qa", llm_review: "fleet:default-fleet" },
  ],
} as unknown as CompanyDocument;

/** The last document the wizard saved. */
const savedDoc = () =>
  vi
    .mocked(api.writeDocument)
    .mock.calls.filter((c) => !c[1].dryRun)
    .at(-1)![0] as Record<string, any>;

// A COMPANY'S FIRST MODEL PINS NOBODY: it joins the default fleet, and every
// seat job and both helpers run there, in the one revision that adds it.
test("a company's first model joins the default fleet, with every seat and helper on it", async () => {
  vi.mocked(getConfig).mockImplementation(async () => ({
    kind: "document",
    document: structuredClone(NO_MODEL),
    etag: '"rev-1"',
  }));
  const first = modelsData({ doc: NO_MODEL, entries: [] });
  render(
    <ToastProvider>
      <Router>
        <ModelWizard data={first} onClose={() => {}} onDevice={() => {}} />
      </Router>
    </ToastProvider>,
  );
  await walkToReview();
  const box = joinBox();
  expect(box.checked).toBe(true);
  expect(box.disabled).toBe(true);
  expect(box.closest("label")?.textContent ?? document.body.textContent).toContain(
    "A company's first model joins default-fleet, where its seats and helpers run.",
  );
  press("Finish and save");
  await waitFor(() => expect(calls.at(-1)).toMatch(/^write:/));
  const doc = savedDoc();
  expect(doc.providers.llm_fleets).toEqual({
    "default-fleet": { name: "default-fleet", strategy: "balanced", members: ["sonnet-key"] },
  });
  expect(doc.roles).toEqual([
    { name: "Backend Engineer", handle: "backend-engineer", llm: "fleet:default-fleet" },
    { name: "Product Manager", handle: "pm", llm: "fleet:default-fleet" },
    // A JOB ALREADY ON A FLEET KEEPS IT.
    {
      name: "Quality Lead",
      handle: "qa",
      llm: "fleet:default-fleet",
      llm_review: "fleet:default-fleet",
    },
  ]);
  expect(doc.assistant).toEqual({ model: "fleet:default-fleet" });
  expect(doc.advisor).toEqual({ model: "fleet:default-fleet" });
  expect(JSON.stringify(doc.roles)).not.toContain("sonnet-key");
  // ONE REVISION: no write of the fleet after the model's.
  expect(calls.filter((c) => c.startsWith("write:"))).toEqual([
    "write:Add model sonnet-key in the default-fleet fleet, every seat on it",
  ]);
});

/** The default fleet with one member, `default`, which runs `model` on Claude Code. */
const homeOf = (cls: string): Fleet => ({
  ...DEFAULT_FLEET_EMPTY,
  class: cls,
  harness: "claude-code",
  members: [
    {
      entry: "default",
      kind: "api",
      state: "available",
      until: null,
      why: "",
      share: 0,
      overflow: true,
      last_served_at: null,
      serving: [],
    },
  ],
});

/** The wizard adding a model, on a company whose default fleet is `home`. */
function showAdding(home: Fleet) {
  const d = data();
  d.fleets = fleetsFixture({ entries: [], fleets: [home] });
  render(
    <ToastProvider>
      <Router>
        <ModelWizard data={d} onClose={() => {}} onDevice={() => {}} />
      </Router>
    </ToastProvider>,
  );
}

const joinBox = () =>
  screen.getByRole("checkbox", {
    name: /^Add to default-fleet, the default fleet/,
  }) as HTMLInputElement;

// ADD MODEL OFFERS THE DEFAULT FLEET on its Review, ticked where the model fits
// it: any class while it has no member, else its own. Mutation: tick it regardless.
test.each([
  [
    "with no member, any model sets its class",
    DEFAULT_FLEET_EMPTY,
    true,
    "default-fleet has no members yet: this model sets the class it runs.",
  ],
  [
    "a model of its class joins",
    homeOf("Claude Sonnet"),
    true,
    "It runs Claude Sonnet, as every member of default-fleet does. Seats on the default fleet can run on it.",
  ],
  [
    "a model of another class is held out",
    homeOf("Claude Opus"),
    false,
    "It runs Claude Sonnet; every member of default-fleet runs Claude Opus, and a fleet keeps one class.",
  ],
] as const)("the default fleet's box on Review: %s", async (_, home, fits, said) => {
  showAdding(home);
  await walkToReview();
  const box = joinBox();
  expect(box.checked).toBe(fits);
  expect(box.disabled).toBe(!fits);
  expect(box.closest("label")?.textContent ?? document.body.textContent).toContain(said);
});

// ONE REVISION: the model and its place in the default fleet are written together,
// and nobody is moved onto it but a first model's seats.
test("a ticked box writes the model and its default-fleet membership as one revision", async () => {
  vi.mocked(getConfig).mockImplementation(async () => ({
    kind: "document",
    document: {
      ...structuredClone(DOC),
      providers: {
        ...structuredClone(DOC.providers),
        llm_fleets: { "default-fleet": { name: "default-fleet", members: ["default"] } },
      },
    } as unknown as CompanyDocument,
    etag: '"rev-1"',
  }));
  showAdding(homeOf("Claude Sonnet"));
  await walkToReview();
  press("Finish and save");
  await waitFor(() => expect(calls.at(-1)).toMatch(/^write:/));
  const doc = savedDoc();
  expect(doc.providers.llm_fleets["default-fleet"]).toEqual({
    name: "default-fleet",
    members: ["default", "sonnet-key"],
  });
  expect(doc.roles).toEqual(DOC.roles);
  expect(calls.filter((c) => c.startsWith("write:"))).toEqual([
    "write:Add model sonnet-key in the default-fleet fleet",
  ]);
});

test("an unticked box adds the model to no fleet and checks the change again, and an edit offers none", async () => {
  showAdding(DEFAULT_FLEET_EMPTY);
  await walkToReview();
  const checks = calls.filter((c) => c === "dry-run").length;
  fireEvent.click(joinBox());
  expect(joinBox().checked).toBe(false);
  // THE CHECKED WRITE NO LONGER STANDS: Review checks the change again.
  await screen.findByText("The configuration is valid");
  expect(calls.filter((c) => c === "dry-run").length).toBeGreaterThan(checks);
  press("Finish and save");
  await waitFor(() => expect(calls.at(-1)).toMatch(/^write:/));
  expect(savedDoc().providers.llm_fleets).toBeUndefined();
  cleanup();
  show({ editing: "sonnet", target: { step: "review" } });
  expect(screen.queryByRole("checkbox", { name: /the default fleet/ })).toBeNull();
});
