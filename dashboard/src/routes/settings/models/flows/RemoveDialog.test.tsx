/**
 * Removing a model draws every place that names it from the engine's own
 * refusal, says each place in words (a seat by its name and a job by its
 * category's words, never a handle or a path outside the list of changes),
 * checks each choice as it is made, names where each place goes before the
 * removal is confirmed, and removes against the entry's own tag with an audit
 * summary. An engine that answered with an error is never said not to have
 * answered.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { Router } from "~/app/router.tsx";
import type { ModelReference } from "~/protocol/models.ts";
import { apiKeyEntry, modelsData, modelsDoc, seat } from "~/routes/settings/models/fixtures.ts";
import { RemoveDialog } from "./RemoveDialog.tsx";

const removeModel = vi.hoisted(() => vi.fn());
vi.mock("../api.ts", () => ({ removeModel }));
// THE DOCUMENT'S TAG IS NOT THE ENTRY'S: a removal sent the document's and was
// refused 412 for every model. Both are answered here, apart, so a case
// reads which one the dialog sent.
vi.mock("~/protocol/configWrite.ts", () => ({
  getConfig: () => Promise.resolve({ kind: "document", document: {}, etag: '"rev-1"' }),
  getEntity: () => Promise.resolve({ kind: "entity", entity: {}, etag: '"llm-providers-sonnet"' }),
}));

const REFS: ModelReference[] = [
  {
    at: { seat: "backend-engineer", job: "executor" },
    segments: ["roles", 0, "llm"],
    chain: ["sonnet"],
  },
  {
    at: { seat: "qa", job: "review" },
    segments: ["roles", 1, "llm_review"],
    chain: ["default", "sonnet"],
  },
  { at: { assistant: "chat" }, segments: ["assistant", "llm"], chain: ["sonnet"] },
];

const refusal = {
  outcome: { kind: "problems", problems: [], derived: null, code: "validation_error", hint: "" },
  references: REFS,
};
const valid = {
  outcome: { kind: "valid", baseRevisionId: "rev-1", warnings: [], derived: null },
  references: [],
};

beforeEach(() => {
  removeModel.mockImplementation((_key, removal: { repoint: unknown[] }) =>
    Promise.resolve(removal.repoint.length === 0 ? refusal : valid),
  );
});

afterEach(() => {
  cleanup();
  removeModel.mockReset();
  window.location.hash = "";
});

function open(assistantWebDenied: boolean) {
  const data = modelsData({
    entries: [
      apiKeyEntry({ key: "sonnet" }),
      apiKeyEntry({ key: "big" }),
      apiKeyEntry({ key: "default", agent: "codex" }),
    ],
    doc: modelsDoc(assistantWebDenied ? { big: { overrides: { web_tools: "denied" } } } : {}),
    seats: [seat("backend-engineer", "Backend Engineer"), seat("qa", "QA")],
    assistant: { enabled: true, key: "sonnet" },
  });
  const onDone = vi.fn();
  view = render(
    <Router>
      <RemoveDialog model={data.models[0]!} data={data} onClose={() => {}} onDone={onDone} />
    </Router>,
  );
  return onDone;
}

let view: ReturnType<typeof render>;

const settle = () => act(async () => {});

test("each place is named in words, a backup said apart, and no handle or path in the prose", async () => {
  open(true);
  const list = await screen.findByRole("list", { name: "Where sonnet is named" });
  const rows = within(list)
    .getAllByRole("listitem")
    .map((li) => li.textContent ?? "");
  expect(rows[0]).toContain("Backend Engineer, coding");
  expect(rows[1]).toContain("QA, reviewing (backup)");
  expect(rows[2]).toContain("Ask Crewlet's chat");
  expect(list.textContent).not.toMatch(/backend-engineer|llm_review|roles\[/);
  // The first ask carried no repoints: the engine's refusal drew the list.
  expect(removeModel.mock.calls[0]![1]).toEqual({ repoint: [], assistantOff: false });
});

test("every choice is checked by a dry run as it is made, and summarised in words", async () => {
  open(true);
  await screen.findByRole("list", { name: "Where sonnet is named" });
  await settle();
  // Defaults: coding moves to a model that can code (codex cannot), the backup
  // to the model already ahead of it in its field, the assistant to one that
  // denies web tools.
  const checked = removeModel.mock.calls.at(-1)!;
  expect(checked[1]).toEqual({
    repoint: [
      { at: REFS[0]!.at, to: "big" },
      { at: REFS[1]!.at, to: "default" },
      { at: REFS[2]!.at, to: "big" },
    ],
    assistantOff: false,
  });
  expect(checked[2]).toMatchObject({ etag: '"llm-providers-sonnet"', dryRun: true });
  expect(
    screen.getByText("Removes the model sonnet, moves 2 uses to big, and moves 1 use to default."),
  ).toBeDefined();

  const asked = removeModel.mock.calls.length;
  fireEvent.click(screen.getByRole("combobox", { name: "Where Backend Engineer, coding moves" }));
  fireEvent.mouseDown(screen.getByRole("option", { name: /Leave it unset/ }));
  await settle();
  expect(removeModel.mock.calls.length).toBe(asked + 1);
  expect(
    screen.getByText(
      "Removes the model sonnet, moves 1 use to default, moves 1 use to big, and leaves 1 unset.",
    ),
  ).toBeDefined();
});

test("with no other model that denies web tools, the assistant's chat turns the assistant off", async () => {
  open(false);
  await screen.findByRole("list", { name: "Where sonnet is named" });
  await settle();
  const removal = removeModel.mock.calls.at(-1)![1];
  expect(removal.assistantOff).toBe(true);
  expect(removal.repoint[2]).toEqual({ at: { assistant: "chat" }, to: null });
  expect(screen.getByText(/turns Ask Crewlet off/)).toBeDefined();
});

test("the removal waits for the typed name, then states the revision it read and a summary", async () => {
  const onDone = open(true);
  await screen.findByRole("list", { name: "Where sonnet is named" });
  await settle();
  const button = screen.getByRole("button", { name: "Remove model" }) as HTMLButtonElement;
  expect(button.disabled).toBe(true);
  fireEvent.change(screen.getByRole("textbox"), { target: { value: "sonnet" } });
  expect(button.disabled).toBe(false);
  removeModel.mockResolvedValueOnce({
    outcome: { kind: "saved", revisionId: "rev-2", epoch: 3, warnings: [], derived: null },
    references: [],
  });
  await act(async () => {
    fireEvent.click(button);
  });
  const sent = removeModel.mock.calls.at(-1)!;
  expect(sent[2]).toEqual({
    etag: '"llm-providers-sonnet"',
    dryRun: false,
    summary: "Remove the model sonnet, move 2 uses to big, and move 1 use to default",
  });
  expect(onDone).toHaveBeenCalled();
  expect(window.location.hash).toContain("settings/models");
});

test("a removal that has started finishes after the dialog closed, and moves nothing on screen", async () => {
  const onDone = open(true);
  await screen.findByRole("list", { name: "Where sonnet is named" });
  await settle();
  fireEvent.change(screen.getByRole("textbox"), { target: { value: "sonnet" } });
  let answer!: (value: unknown) => void;
  let sent: unknown = "unset";
  removeModel.mockImplementationOnce((_k, _r, _o, signal?: AbortSignal) => {
    sent = signal;
    return new Promise((resolve) => (answer = resolve));
  });
  fireEvent.click(screen.getByRole("button", { name: "Remove model" }));
  window.location.hash = "#/somewhere-else";
  view.unmount();
  // NO SIGNAL: closing the dialog cannot stop a removal part way.
  expect(sent).toBeUndefined();
  await act(async () =>
    answer({
      outcome: { kind: "saved", revisionId: "rev-2", epoch: 3, warnings: [], derived: null },
      references: [],
    }),
  );
  expect(onDone).not.toHaveBeenCalled();
  expect(window.location.hash).toBe("#/somewhere-else");
});

test("every request of a removal carries the entry's own tag, never the document's", async () => {
  open(true);
  await screen.findByRole("list", { name: "Where sonnet is named" });
  await settle();
  expect(removeModel.mock.calls.length).toBeGreaterThan(0);
  for (const call of removeModel.mock.calls) expect(call[2].etag).toBe('"llm-providers-sonnet"');
});

test("before it is confirmed, the removal says where each seat and job goes, by name", async () => {
  open(true);
  await screen.findByRole("list", { name: "Where sonnet is named" });
  await settle();
  const moves = await screen.findByRole("list", { name: "Where each moves" });
  const lines = within(moves)
    .getAllByRole("listitem")
    .map((li) => li.textContent);
  expect(lines.length).toBe(REFS.length);
  for (const line of lines) expect(line).toMatch(/^.+: (sonnet → \S+|turned off)/);
  expect(lines.join(" ")).toContain("Backend Engineer");
  expect(lines.join(" ")).not.toMatch(/backend-engineer|roles\./);
});

/**
 * "DID NOT ANSWER" ONLY WHERE THE ENGINE'S ANSWER NEVER ARRIVED: the owner's
 * removal said it while the engine's log showed the engine answering. A
 * gateway's page is not the engine's answer either; a 5xx the engine wrote is.
 */
const UNSETTLED = [
  [0, false, "the engine did not answer within 30 seconds", "The engine did not answer."],
  [504, false, "a 504 came back without the engine's JSON", "The engine did not answer."],
  [500, true, "store: disk full", "The engine failed: store: disk full."],
  // A PROXY'S 403 OR 413, as the classifier reads one without the engine's mark.
  [403, false, "blocked by policy", "The engine did not answer."],
  [413, false, "blocked by policy", "The engine did not answer."],
] as const;

test.each(UNSETTLED)(
  "a check answered with status %i (by the engine: %s) says what happened, and that nothing was removed",
  async (status, answered, detail, said) => {
    removeModel.mockReset();
    removeModel.mockResolvedValue({
      outcome: { kind: "unsettled", answered, status, detail },
      references: [],
    });
    const words = new RegExp(`^${said.replace(/[.()]/g, "\\$&")} Nothing was removed\\.$`);
    open(true);
    const title = await screen.findByText("The removal could not be prepared");
    expect(
      title.closest(".crewlet-callout")?.textContent?.replace(title.textContent ?? "", ""),
    ).toMatch(words);
  },
);

test.each(UNSETTLED)(
  "a removal answered with status %i (by the engine: %s) says what happened, and that it may have landed",
  async (status, answered, detail, said) => {
    open(true);
    await screen.findByRole("list", { name: "Where sonnet is named" });
    await settle();
    fireEvent.change(screen.getByRole("textbox"), { target: { value: "sonnet" } });
    removeModel.mockResolvedValueOnce({
      outcome: { kind: "unsettled", answered, status, detail },
      references: [],
    });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Remove model" }));
    });
    // NOT TITLED "Not removed": it may have been.
    expect(screen.queryByText("Not removed")).toBeNull();
    const title = await screen.findByText("Not known whether it was removed");
    expect(
      title.closest(".crewlet-callout")?.textContent?.replace(title.textContent ?? "", ""),
    ).toBe(
      `${said} sonnet may or may not have been removed. Close this and look at the list before trying again.`,
    );
  },
);
