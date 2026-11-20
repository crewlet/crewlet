/**
 * Removing a model draws every place that names it from the engine's own
 * refusal and asks one question: where all of it moves. One line says what
 * uses the model, by counts and names; "Choose per seat" opens a row per
 * place, each just its name and a picker; no handle, field or config key is
 * printed; each set of moves is checked by a dry run as it changes; and a
 * confirm is asked only where the removal leaves something with no model.
 * The removal states the entry's own tag and an audit summary, and an engine
 * that answered with an error is never said not to have answered.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { Router } from "~/app/router.tsx";
import type { ModelReference } from "~/protocol/models.ts";
import { apiKeyEntry, modelsData, modelsDoc, seat } from "~/routes/settings/models/fixtures.ts";
import {
  defaultChoice,
  leftWithNoModel,
  placeNames,
  refId,
  RemoveDialog,
  summaryOf,
} from "./RemoveDialog.tsx";

const removeModel = vi.hoisted(() => vi.fn());
vi.mock("../api.ts", () => ({ removeModel }));
// THE DOCUMENT'S TAG IS NOT THE ENTRY'S: a removal sent the document's and was
// refused 412 for every model. The entry's is the one answered here.
vi.mock("~/protocol/configWrite.ts", () => ({
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
  { at: { assistant: "chat" }, segments: ["assistant", "model"], chain: ["sonnet"] },
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

/** The dialog on `sonnet`, Ask Crewlet's model, with `big` one a helper can run on or not. */
function open(bigRunsAHelper: boolean) {
  const data = modelsData({
    entries: [
      apiKeyEntry({ key: "sonnet" }),
      apiKeyEntry({
        key: "big",
        helper_problem: bigRunsAHelper ? null : "Its tool runs with tools Crewlet cannot turn off.",
      }),
      apiKeyEntry({
        key: "default",
        harness: "codex",
        helper_problem: "Its tool runs with tools Crewlet cannot turn off.",
      }),
    ],
    doc: modelsDoc({}),
    seats: [seat("backend-engineer", "Backend Engineer"), seat("qa", "QA")],
    assistant: { key: "sonnet" },
  });
  const onDone = vi.fn();
  view = render(
    <Router>
      <RemoveDialog
        model={data.models[0]!}
        data={data}
        onClose={() => {}}
        onDone={onDone}
        onRemoved={onRemoved}
      />
    </Router>,
  );
  return onDone;
}

let view: ReturnType<typeof render>;

/** Where the dialog sends the page once the model is gone: its opener decides. */
const onRemoved = vi.fn();

const settle = () => act(async () => {});

const button = () => screen.getByRole("button", { name: "Remove model" }) as HTMLButtonElement;

/** An option's key: what stands before the model's name. */
const keyOf = (text: string | null) => (text ?? "").split(" · ")[0];

/** Picks an option of a kit Select: the list opens on a click, and an option commits on its press. */
async function pick(picker: HTMLElement, option: RegExp | string) {
  fireEvent.click(picker);
  fireEvent.mouseDown(screen.getByRole("option", { name: option }));
  await settle();
}

const ADVISOR_REF: ModelReference = {
  at: { advisor: "findings" },
  segments: ["advisor", "model"],
  chain: ["sonnet"],
};

test("one line says what uses the model, and no handle, field or config key is printed", async () => {
  open(true);
  expect(await screen.findByText("Used by 2 seats and Ask Crewlet.")).toBeDefined();
  await settle();
  const said = document.body.textContent ?? "";
  expect(said).not.toMatch(
    /backend-engineer|llm_review|roles\[|assistant\.model|providers\.llm|Ready to remove|Every change in the revision/,
  );
  // NO PER-PLACE ROWS until they are asked for, and no caption under any.
  expect(screen.queryByRole("list", { name: "Each place" })).toBeNull();
  expect(said).not.toContain("runs the model with no tools of its own");
  // The first ask carried no repoints: the engine's refusal drew the places.
  expect(removeModel.mock.calls[0]![1]).toEqual({ repoint: [] });
});

test("the one choice moves every place, and each set of moves is checked by a dry run", async () => {
  open(true);
  const choice = await screen.findByRole("combobox", { name: "Move them to" });
  await settle();
  // big fits every place (codex's default cannot run Ask Crewlet), so all of it moves there.
  expect(choice.textContent).toContain("big");
  const checked = removeModel.mock.calls.at(-1)!;
  expect(checked[1]).toEqual({ repoint: REFS.map((r) => ({ at: r.at, to: "big" })) });
  expect(checked[2]).toMatchObject({ etag: '"llm-providers-sonnet"', dryRun: true });

  // ONLY WHAT FITS EVERY PLACE is offered, then Leave unset.
  fireEvent.click(choice);
  // EACH MODEL NAMED IN FULL, its key first: "big · claude-sonnet-5".
  expect(screen.getAllByRole("option").map((o) => keyOf(o.textContent))).toEqual([
    "big",
    "Leave unset",
  ]);
  fireEvent.mouseDown(screen.getByRole("option", { name: "Leave unset" }));
  await settle();
  expect(removeModel.mock.calls.at(-1)![1]).toEqual({
    repoint: REFS.map((r) => ({ at: r.at, to: null })),
  });
});

test("Choose per seat opens a row per place, just its name and a picker, and a row overrides the one choice", async () => {
  open(true);
  const toggle = await screen.findByRole("button", { name: "Choose per seat" });
  await settle();
  expect(toggle.getAttribute("aria-expanded")).toBe("false");
  fireEvent.click(toggle);
  expect(toggle.getAttribute("aria-expanded")).toBe("true");
  const list = screen.getByRole("list", { name: "Each place" });
  // A SEAT WITH ONE JOB ON THE MODEL IS ITS NAME ALONE: no "Coding" under it.
  expect(
    within(list)
      .getAllByRole("listitem")
      .map((li) => li.querySelector(".models-repoint__name")?.textContent),
  ).toEqual(["Backend Engineer", "QA", "Ask Crewlet"]);
  expect(list.textContent).not.toMatch(/Coding|Reviewing|Chat|no tools of its own/);

  await pick(screen.getByRole("combobox", { name: "Where QA moves" }), /^default/);
  expect(removeModel.mock.calls.at(-1)![1]).toEqual({
    repoint: [
      { at: REFS[0]!.at, to: "big" },
      { at: REFS[1]!.at, to: "default" },
      { at: REFS[2]!.at, to: "big" },
    ],
  });
  // A SPLIT READS AS ONE: the one choice no longer names a model.
  expect(screen.getByRole("combobox", { name: "Move them to" }).textContent).toContain(
    "Different for each",
  );
  // …and choosing it again moves every place once more.
  await pick(screen.getByRole("combobox", { name: "Move them to" }), /^big/);
  expect(removeModel.mock.calls.at(-1)![1]).toEqual({
    repoint: REFS.map((r) => ({ at: r.at, to: "big" })),
  });
});

test("a seat that runs on the model for more than one job is told apart by the job", () => {
  const refs: ModelReference[] = [
    { at: { seat: "qa", job: "executor" }, segments: ["roles", 1, "llm"], chain: ["sonnet"] },
    { at: { seat: "qa", job: "review" }, segments: ["roles", 1, "llm_review"], chain: ["sonnet"] },
    { at: { seat: "pm", job: "review" }, segments: ["roles", 2, "llm_review"], chain: ["sonnet"] },
    REFS[2]!,
    ADVISOR_REF,
  ];
  const names = placeNames(refs, [seat("qa", "QA"), seat("pm", "Product Manager")]);
  expect(refs.map((r) => names.get(refId(r)))).toEqual([
    "QA, coding",
    "QA, reviewing",
    "Product Manager",
    "Ask Crewlet",
    "The Advisor",
  ]);
});

test("when everything moves to another model, the button alone removes it, with the entry's tag and a summary", async () => {
  const onDone = open(true);
  await screen.findByRole("combobox", { name: "Move them to" });
  await settle();
  // NO TYPED NAME, NO BOX TO TICK: nothing is left with no model.
  expect(screen.queryByRole("textbox")).toBeNull();
  expect(screen.queryByRole("checkbox")).toBeNull();
  expect(button().disabled).toBe(false);
  removeModel.mockResolvedValueOnce({
    outcome: { kind: "saved", revisionId: "rev-2", epoch: 3, warnings: [], derived: null },
    references: [],
  });
  await act(async () => {
    fireEvent.click(button());
  });
  const sent = removeModel.mock.calls.at(-1)!;
  expect(sent[1]).toEqual({ repoint: REFS.map((r) => ({ at: r.at, to: "big" })) });
  expect(sent[2]).toEqual({
    etag: '"llm-providers-sonnet"',
    dryRun: false,
    summary: "Remove the model sonnet and move 3 uses to big",
  });
  expect(onDone).toHaveBeenCalled();
  expect(onRemoved).toHaveBeenCalledTimes(1);
});

test("leaving something with no model asks for one confirm, which names what is lost", async () => {
  open(true);
  await pick(await screen.findByRole("combobox", { name: "Move them to" }), "Leave unset");
  // SEATS LEFT UNSET FALL BACK on the company's other models; Ask Crewlet is off.
  const confirm = screen.getByRole("checkbox", { name: "Turn Ask Crewlet off" });
  expect(button().disabled).toBe(true);
  fireEvent.click(confirm);
  expect(button().disabled).toBe(false);
  // A DIFFERENT LOSS ASKS AGAIN: moving Ask Crewlet back clears the confirm.
  await pick(screen.getByRole("combobox", { name: "Move them to" }), /^big/);
  expect(screen.queryByRole("checkbox")).toBeNull();
  expect(button().disabled).toBe(false);
});

// NO MODEL FITS EVERY PLACE: each place opens on its own default and the list is shown.
test("where no model fits every place, the list opens with each place's own default", async () => {
  open(false);
  const list = await screen.findByRole("list", { name: "Each place" });
  await settle();
  expect(list).toBeDefined();
  const removal = removeModel.mock.calls.at(-1)![1];
  expect(Object.keys(removal)).toEqual(["repoint"]);
  // Coding to a model that codes, the backup to the model ahead of it, and Ask
  // Crewlet, with no model a helper can run on, to none.
  expect(removal.repoint).toEqual([
    { at: REFS[0]!.at, to: "big" },
    { at: REFS[1]!.at, to: "default" },
    { at: { assistant: "chat" }, to: null },
  ]);
  expect(screen.getByRole("checkbox", { name: "Turn Ask Crewlet off" })).toBeDefined();
  expect(button().disabled).toBe(true);
});

test("a model nothing uses says so in one line, and the button removes it", async () => {
  removeModel.mockImplementation(() => Promise.resolve(valid));
  open(true);
  expect(await screen.findByText("Nothing uses this model.")).toBeDefined();
  await settle();
  expect(screen.queryByRole("combobox")).toBeNull();
  expect(screen.queryByRole("checkbox")).toBeNull();
  expect(button().disabled).toBe(false);
});

test("a set of moves the engine refuses is said inline in its words, and nothing can be removed", async () => {
  removeModel.mockImplementation((_key, removal: { repoint: { to: string | null }[] }) =>
    Promise.resolve(
      removal.repoint.length === 0
        ? refusal
        : removal.repoint.some((r) => r.to === null)
          ? {
              outcome: {
                kind: "problems",
                problems: [{ path: "", message: "QA still needs a model to review with." }],
                derived: null,
                code: "validation_error",
                hint: "",
              },
              references: [],
            }
          : valid,
    ),
  );
  open(true);
  await pick(await screen.findByRole("combobox", { name: "Move them to" }), "Leave unset");
  const title = screen.getByText("This cannot be removed as it stands");
  expect(title.closest(".crewlet-callout")?.textContent).toContain(
    "QA still needs a model to review with.",
  );
  fireEvent.click(screen.getByRole("checkbox", { name: "Turn Ask Crewlet off" }));
  expect(button().disabled).toBe(true);
});

test("a removal that has started finishes after the dialog closed, and moves nothing on screen", async () => {
  const onDone = open(true);
  await screen.findByRole("combobox", { name: "Move them to" });
  await settle();
  let answer!: (value: unknown) => void;
  let sent: unknown = "unset";
  removeModel.mockImplementationOnce((_k, _r, _o, signal?: AbortSignal) => {
    sent = signal;
    return new Promise((resolve) => (answer = resolve));
  });
  fireEvent.click(button());
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
  expect(onRemoved).not.toHaveBeenCalled();
  expect(window.location.hash).toBe("#/somewhere-else");
});

test("every request of a removal carries the entry's own tag, never the document's", async () => {
  open(true);
  await screen.findByRole("combobox", { name: "Move them to" });
  await settle();
  expect(removeModel.mock.calls.length).toBeGreaterThan(0);
  for (const call of removeModel.mock.calls) expect(call[2].etag).toBe('"llm-providers-sonnet"');
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
    await screen.findByRole("combobox", { name: "Move them to" });
    await settle();
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

// A DEFAULT IS A RECOMMENDATION: it never sends a seat's job to a model that
// cannot answer while one that can is eligible, and it keeps the job on the
// removed model's harness where it can.
test("a place's default is a model that answers, on the same harness where one does", () => {
  const data = modelsData({
    entries: [
      apiKeyEntry({ key: "sonnet" }),
      apiKeyEntry({ key: "a-refused", health: "rejected" }),
      apiKeyEntry({ key: "b-sitting", health: "sitting_out" }),
      apiKeyEntry({ key: "c-codex", harness: "codex" }),
      apiKeyEntry({ key: "d-ready" }),
    ],
    doc: modelsDoc({}),
    seats: [seat("qa", "QA")],
  });
  const ref: ModelReference = {
    at: { seat: "qa", job: "review" },
    segments: ["roles", 0, "llm_review"],
    chain: ["sonnet"],
  };
  expect(defaultChoice(ref, "sonnet", data.models)).toEqual({ to: "d-ready" });
  // Its own backup still wins, whatever the list order.
  expect(defaultChoice({ ...ref, chain: ["sonnet", "c-codex"] }, "sonnet", data.models)).toEqual({
    to: "c-codex",
  });
});

// THE ADVISOR'S MODEL IS MOVED LIKE ASK CREWLET'S: named in the one line, a
// row of its own, and running with no model the one confirm names.
test("the Advisor's model moves with the rest, or runs with no model once confirmed", async () => {
  removeModel.mockImplementation((_key, removal: { repoint: unknown[] }) =>
    Promise.resolve(
      removal.repoint.length === 0 ? { ...refusal, references: [...REFS, ADVISOR_REF] } : valid,
    ),
  );
  open(true);
  expect(await screen.findByText("Used by 2 seats, Ask Crewlet, and the Advisor.")).toBeDefined();
  fireEvent.click(screen.getByRole("button", { name: "Choose per seat" }));
  const picker = screen.getByRole("combobox", { name: "Where The Advisor moves" });
  fireEvent.click(picker);
  // EACH MODEL NAMED IN FULL, its key first: "big · claude-sonnet-5".
  expect(screen.getAllByRole("option").map((o) => keyOf(o.textContent))).toEqual([
    "big",
    "Leave unset",
  ]);
  fireEvent.mouseDown(screen.getByRole("option", { name: "Leave unset" }));
  await settle();
  expect(screen.getByRole("checkbox", { name: "Run the Advisor with no model" })).toBeDefined();
  await pick(screen.getByRole("combobox", { name: "Move them to" }), "Leave unset");
  expect(
    screen.getByRole("checkbox", {
      name: "Turn Ask Crewlet off and run the Advisor with no model",
    }),
  ).toBeDefined();
});

test("seats stop only where no model is left to fall back on", () => {
  const two = (models: string[]) =>
    modelsData({
      entries: models.map((key) => apiKeyEntry({ key })),
      doc: modelsDoc({}),
      seats: [seat("backend-engineer", "Backend Engineer"), seat("qa", "QA")],
    }).models;
  const none = Object.fromEntries(REFS.map((r) => [refId(r), { to: null }]));
  // ANOTHER MODEL IS LEFT: the seats run on the company's fallback, Ask Crewlet is off.
  expect(leftWithNoModel(REFS, none, "sonnet", two(["sonnet", "big"]))).toBe(
    "Turn Ask Crewlet off",
  );
  // THE LAST MODEL: the seats have nothing to run on.
  expect(leftWithNoModel(REFS, none, "sonnet", two(["sonnet"]))).toBe(
    "Turn Ask Crewlet off and leave 2 seats with no model until one is added",
  );
  expect(leftWithNoModel(REFS.slice(0, 2), none, "sonnet", two(["sonnet", "big"]))).toBeNull();
});

test("the audit summary says each move once, and Ask Crewlet turned off", () => {
  expect(
    summaryOf("sonnet", {
      repoint: [
        { at: REFS[0]!.at, to: "big" },
        { at: REFS[1]!.at, to: null },
        { at: REFS[2]!.at, to: null },
      ],
    }),
  ).toBe("Remove the model sonnet, move 1 use to big, leave 1 unset, and turn Ask Crewlet off");
});
