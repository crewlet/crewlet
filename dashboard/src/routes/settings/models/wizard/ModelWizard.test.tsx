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
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { pageSnapshot, resetPageContextForTest, setFramePage } from "~/app/assistant/context.ts";
import { Router } from "~/app/router.tsx";
import type { CompanyDocument } from "~/protocol/types.ts";
import {
  EVERY_JOB,
  modelEntry,
  modelsData,
  runs,
  signIn,
} from "~/routes/settings/models/fixtures.ts";
import type { ModelsData } from "../data.ts";
import type { WizardAt } from "../parts.tsx";
import { ModelWizard } from "./ModelWizard.tsx";

const calls: string[] = [];

vi.mock("../api.ts", () => ({
  testCredential: vi.fn(),
  storeCredential: vi.fn(),
  checkModel: vi.fn(),
  checkHostLogin: vi.fn(),
  writeDocument: vi.fn(),
}));
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
      default: { agent: "claude-code", model: "opus", auth: { mode: "api-key" } },
      sonnet: {
        agent: "claude-code",
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

beforeEach(() => {
  calls.length = 0;
  answers.llm_models = {
    source: "models.dev",
    fetched_at: null,
    provider: "anthropic",
    aliases: [{ id: "sonnet", name: "Sonnet", detail: "The CLI's latest Sonnet" }],
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
async function walkToReview(key = "sonnet-key", accepted = true) {
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
  press("Continue");
  expect(heading()).toBe("Review");
  if (accepted) await screen.findByText("The engine accepts this change");
  primaries.push(primary());
  return primaries;
}

test("the primary action is the wizard's own, last in its footer, on every step", async () => {
  show();
  const primaries = await walkToReview();
  expect(primaries).toHaveLength(8);
  expect(primaries.every((p) => p.footer)).toBe(true);
  expect(primaries.map((p) => p.text)).toEqual([...Array(7).fill("Continue"), "Create model"]);
});

test("the credential is tested, then stored with its draft before the document that names it", async () => {
  show();
  await walkToReview();
  expect(calls).toEqual(["test", "read", "dry-run", "dry-run"]);
  const tested = vi.mocked(api.testCredential).mock.calls[0]!;
  expect(tested[1]).toMatchObject({
    values: { ANTHROPIC_API_KEY_SONNET_KEY: "sk-ant-api03-new" },
    createOnly: true,
  });
  expect(tested[1].entity).toMatchObject({
    auth: { mode: "api-key", api_key: "${ANTHROPIC_API_KEY_SONNET_KEY}" },
  });

  press("Create model");
  await waitFor(() => expect(calls.at(-1)).toMatch(/^write:/));
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
  await walkToReview("sonnet-key", false);
  const list = await screen.findByText("1 problem to fix first");
  expect(within(list.closest("[role]") ?? document.body).queryByText(/values\./)).toBeNull();
  press("Fix in Method");
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
  press("Create model");
  await screen.findByText(/ANTHROPIC_API_KEY_SONNET_KEY is already stored/);
  expect(calls.some((c) => c.startsWith("write:"))).toBe(false);
  press("Fix in Method");
  expect(screen.getByText(/is already stored, and another model may read it/)).toBeTruthy();
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
  press("Continue");
  await screen.findByText("The engine accepts this change");
  press("Create model");
  await waitFor(() => expect(onDevice).toHaveBeenCalledWith("codex-review"));
  expect(api.storeCredential).not.toHaveBeenCalled();
  const written = vi.mocked(api.writeDocument).mock.calls.at(-1)![0] as Record<string, any>;
  expect(written.providers.llm["codex-review"].auth).toEqual({ mode: "subscription" });
});

test("Review names the fallback the write drops, and says the test call has not run", async () => {
  show({ editing: "sonnet", target: { step: "assign", add: [{ job: "reviews", seat: "qa" }] } });
  press("Review changes");
  await screen.findByText("The engine accepts this change");
  expect(
    screen.getByText("Quality Lead's reviewing also named default as a fallback; this removes it."),
  ).toBeTruthy();
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
  expect(box(/^Product Manager: Coding on sonnet$/).checked).toBe(true);
  expect(box(/^Quality Lead: Routines and evaluations on sonnet$/).checked).toBe(true);
  expect(box(/^Product Manager: Reviewing and delegations on sonnet$/).checked).toBe(false);
  expect(box(/^Backend Engineer: Coding on sonnet$/).checked).toBe(false);
  cleanup();
  show({ target: { step: "assign", add } });
  expect(box(/^Product Manager: Coding on /).checked).toBe(true);
  expect(box(/^Quality Lead: Routines and evaluations on /).checked).toBe(true);
  expect(box(/^Quality Lead: Coding on /).checked).toBe(false);
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

test("coding named in the address is not preselected on a CLI that runs no coding", () => {
  const d = data();
  d.models.push(
    ...modelsData({
      entries: [modelEntry({ key: "codex-review", agent: "codex", model: "gpt-6.1" })],
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
