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
import type { CompanyDocument } from "~/protocol/types.ts";
import {
  EVERY_JOB,
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

/** Walks a new Claude Code model on an API key to Review, checking the footer on every step. */
async function walkToTest(key = "sonnet-key") {
  // READ AS EACH STEP STANDS: the wizard keeps one button in the slot and changes its words.
  const primaries: { footer: boolean; text: string }[] = [];
  const primary = () => {
    const button = primaryButton();
    return {
      footer: !!button?.closest(".crewlet-setup-wizard__footer"),
      text: button?.textContent ?? "",
    };
  };
  fireEvent.change(screen.getByRole("textbox", { name: /Name/ }), { target: { value: key } });
  primaries.push(primary());
  press("Continue");
  fireEvent.change(keyField(), { target: { value: "sk-ant-api03-new" } });
  primaries.push(primary());
  press("Continue");
  fireEvent.click(screen.getByRole("radio", { name: /Sonnet/ }));
  primaries.push(primary());
  for (const step of ["Where it runs", "Use it for", "Limits", "Test call"]) {
    press("Continue");
    expect(heading()).toBe(step);
    primaries.push(primary());
  }
  return primaries;
}

test("adding a model ends on Test call with Finish and save, and has no Review step", async () => {
  show();
  const primaries = await walkToTest();
  expect(primaries).toHaveLength(7);
  expect(primaries.every((p) => p.footer)).toBe(true);
  expect(primaries.map((p) => p.text)).toEqual([...Array(6).fill("Continue"), "Finish and save"]);
  const steps = screen.getByRole("list", { name: /^Steps to/ });
  expect(within(steps).queryByRole("button", { name: /Review/ })).toBeNull();
});

test("Finish and save tests the credential, then stores it with its draft before the document that names it", async () => {
  show();
  await walkToTest();
  press("Finish and save");
  await waitFor(() => expect(calls.at(-1)).toMatch(/^write:/));
  expect(calls.slice(0, 4)).toEqual(["test", "read", "dry-run", "dry-run"]);
  const tested = vi.mocked(api.testCredential).mock.calls[0]!;
  expect(tested[1]).toMatchObject({
    values: { ANTHROPIC_API_KEY_SONNET_KEY: "sk-ant-api03-new" },
    createOnly: true,
  });
  expect(tested[1].entity).toMatchObject({
    auth: { mode: "api-key", api_key: "${ANTHROPIC_API_KEY_SONNET_KEY}" },
  });

  expect(calls.slice(4)).toEqual(["store", "write:Add model sonnet-key"]);
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
 * or Replace API key from a row's menu on the list is opened over the list
 * (here on its By job lens), and the save hands the page back to it through
 * the wizard's own close; it does not travel to the model's page, which took
 * a person working down the list away from it. Mutation: send every save to
 * the model's page, and both readings are red.
 */
test("an edit saved returns to where it was opened, never to the model's page", async () => {
  location.hash = "#/settings/models?view=jobs&flow=edit&entry=sonnet&step=signin&focus=credential";
  const { onClose } = show({ editing: "sonnet", target: { step: "signin", focus: "credential" } });
  fireEvent.change(keyField(), { target: { value: "sk-ant-api03-rotated" } });
  toReview();
  await screen.findByText("The engine accepts this change");
  press("Save changes");
  await waitFor(() => expect(calls.at(-1)).toMatch(/^write:/));
  await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1));
  expect(location.hash).not.toContain("settings/models/sonnet");
  expect(location.hash).toContain("view=jobs");
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
  await screen.findByText("The engine accepts this change");
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
  await screen.findByText("The engine accepts this change");
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
  press("Save changes");
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
  await walkToTest();
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
  expect(calls.slice(-3)).toEqual(["store", "read", "write:Add model sonnet-key"]);
  // NO SIGNAL rides a write that has started: nothing could stop it part way.
  const written = vi.mocked(api.writeDocument).mock.calls.at(-1)!;
  expect(written[2]).toBeUndefined();
  expect(location.hash).toBe("#/somewhere-else");
});

/**
 * ACME'S SPLIT SURVIVES AN EDIT OF ONE SIDE. `default` codes for the
 * engineers and QA; `default-host`, its coding on the engine's host, for the
 * PM and the designer. Editing `default` for its key moved the PM and the
 * designer onto it, because a choice on Use it for reset its seats to every
 * seat. An edit keeps the seats the model serves, and Review lists every seat
 * that changes model, first.
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

test.each([
  ["a key replaced and nothing else", false],
  ["a key replaced and Coding chosen on Use it for", true],
])("Acme's coding split survives an edit of default: %s", async (_, chooseCoding) => {
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
  for (const step of ["Model", "Where it runs", "Use it for"]) {
    press("Continue");
    expect(heading()).toBe(step);
  }
  if (chooseCoding) fireEvent.click(screen.getByRole("radio", { name: /^Coding/ }));
  for (const step of ["Limits", "Test call"]) {
    press("Continue");
    expect(heading()).toBe(step);
  }
  toReview();
  await screen.findByText("The engine accepts this change");
  // NO SEAT MOVES ONTO default: the PM and the designer stay where they are.
  const moves = screen.queryByRole("list", { name: "Seats that change model" });
  const listed = moves ? (moves.textContent ?? "") : "";
  expect(listed).not.toMatch(/Product Manager|Designer/);
  if (!chooseCoding) expect(screen.getByText("No seat changes model.")).toBeTruthy();
  press("Save changes");
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
        target={{ step: "assign", add: [{ job: "coding", seat: "pm" }] }}
        onClose={() => {}}
        onDevice={() => {}}
      />
    </Router>,
  );
  toReview();
  const moves = await screen.findByRole("list", { name: "Seats that change model" });
  expect(moves.textContent).toContain("Product Manager, coding: default-host → default");
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
          target={{ step: "assign", add: [{ job: "coding", seat: "pm" }] }}
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
    await screen.findByText("The engine accepts this change");
    press("Save changes");
    expect(await blocked()).toBe(
      `${said} The change may or may not have been stored. Reopen the model before trying again.`,
    );
  });
});

test("a place the company never set up says so in plain words, never the configuration's own sentence", () => {
  show({ editing: "default", target: { step: "runs" } });
  expect(heading()).toBe("Where it runs");
  expect(document.body.textContent).toContain(
    "E2B isn't set up for this company. Add it under providers.sandbox to use it.",
  );
  expect(document.body.textContent).not.toMatch(/declares no|E2B key not set/);
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
  await walkToTest();
  press("Finish and save");
  await screen.findByText("Not saved: the test call below failed");
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
  await walkToTest();
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
  expect(heading()).toBe("Use it for");
  press("Continue");
  press("Continue");
  expect(heading()).toBe("Test call");
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
  show({ editing: "sonnet", target: { step: "assign", add: [{ job: "reviews", seat: "qa" }] } });
  toReview();
  await screen.findByText("The engine accepts this change");
  const moves = screen.getByRole("list", { name: "Seats that change model" });
  expect(moves.textContent).toContain("Quality Lead, reviewing and delegations: default → sonnet");
  expect(screen.queryByText(/as a fallback/)).toBeNull();
  expect(screen.getByText("Not run")).toBeTruthy();
  expect(screen.getByRole("button", { name: "Run test call" })).toBeTruthy();
  await act(async () => {});
});

/**
 * QA M10: seats moved off a limit arrive at Use it for already added, on the
 * model chosen and on a model being added alike, and nothing else is.
 */
test("the seats an alarm moves arrive added at Use it for, and only those", () => {
  const add: WizardAt["add"] = [
    { job: "coding", seat: "pm" },
    { job: "routines", seat: "qa" },
  ];
  const box = (name: RegExp) => screen.getByRole("checkbox", { name }) as HTMLInputElement;
  show({ editing: "sonnet", target: { step: "assign", add } });
  expect(heading()).toBe("Use it for");
  expect(box(/^Product Manager Coding$/).checked).toBe(true);
  expect(box(/^Quality Lead Routines and evaluations$/).checked).toBe(true);
  expect(box(/^Product Manager Reviewing and delegations$/).checked).toBe(false);
  expect(box(/^Backend Engineer Coding$/).checked).toBe(false);
  cleanup();
  show({ target: { step: "assign", add } });
  expect(box(/^Product Manager Coding$/).checked).toBe(true);
  expect(box(/^Quality Lead Routines and evaluations$/).checked).toBe(true);
  expect(box(/^Quality Lead Coding$/).checked).toBe(false);
});

/**
 * THE ADDRESS ONLY PRESELECTS, and only a seat the company has now: a stale or
 * hand-typed seat is dropped without a word, so a link kept from before a seat
 * was removed opens the wizard as if it had never named it.
 */
test("a seat the company no longer has, named in the address, is not preselected", () => {
  const radio = (name: RegExp) => screen.getByRole("radio", { name }) as HTMLInputElement;
  show({
    editing: "sonnet",
    target: { step: "assign", add: [{ job: "coding", seat: "removed-seat" }] },
  });
  expect(heading()).toBe("Use it for");
  expect(radio(/^Not yet/).checked).toBe(true);
  cleanup();
  show({
    editing: "sonnet",
    target: {
      step: "assign",
      add: [
        { job: "coding", seat: "removed-seat" },
        { job: "coding", seat: "pm" },
      ],
    },
  });
  expect(radio(/^Coding/).checked).toBe(true);
  // ONE SEAT, the one the company has: the stale one added nothing beside it.
  const chips = within(screen.getByRole("group", { name: "Seats that use this model" }));
  expect(chips.getAllByRole("button", { pressed: true }).map((b) => b.textContent)).toEqual([
    "Product Manager",
  ]);
});

test("coding named in the address is not preselected on a harness that runs no coding", () => {
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
          step: "assign",
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
  expect(heading()).toBe("Use it for");
  const radio = (name: RegExp) => screen.getByRole("radio", { name }) as HTMLInputElement;
  expect(radio(/^Routines and evaluations/).checked).toBe(true);
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
    title: "Settings › Models",
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
  press("See the test call");
  expect(heading()).toBe("Test call");
  // On the Test call step its own result says what failed, so the note does not repeat it.
  expect(screen.getByText("Not saved: the test call below failed")).toBeTruthy();
  expect(screen.queryByText("Not saved: the test call failed")).toBeNull();
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
