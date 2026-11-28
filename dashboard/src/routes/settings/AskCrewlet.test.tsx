/**
 * Settings › Ask Crewlet: the company's instructions, an administrator's to
 * change, and the reader's own, saved under their seat.
 */

import { act, cleanup, fireEvent, screen, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { ASSISTANT_INSTRUCTIONS_MAX_RUNES } from "~/contract/instructions.ts";
import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { APPLYING_SENTENCE } from "~/lib/savedToast.ts";
import { ENGINE_HEADERS } from "~/test/engineAnswer.ts";
import { healthFrame } from "~/test/health.ts";
import {
  engineAnswer,
  mountSettings,
  READER_VIEWER,
  savedAnswer,
  stubEngine,
  validAnswer,
} from "~/test/settingsScreen.tsx";
import { toastText } from "~/testing.tsx";
import { afterMicrotasks } from "~/test/settle.ts";
import { ADMIN_VIEWER } from "~/test/viewer.ts";
import { AskCrewletSettings } from "./AskCrewlet.tsx";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const ACTS = ["save_assistant_instructions"];

async function show(
  answer: Record<string, unknown>,
  viewer: Record<string, unknown> = { ...ADMIN_VIEWER, acts: ACTS },
) {
  const mounted = mountSettings(
    <AskCrewletSettings />,
    { assistant_instructions: { seat: "jane", company: {}, mine: {}, revision: 0, ...answer } },
    { hash: "#/settings/ask-crewlet", viewer },
  );
  await act(afterMicrotasks);
  return mounted.store;
}

const card = (title: string) => screen.getByRole("heading", { name: title }).closest("section")!;

// A READER WHO CANNOT SAVE THEIR OWN is told why on Save, and nothing is sent.
// Mutation: drop the act's access from Save's reason and this fails.
test.each([
  [
    "an anonymous reader",
    { ...READER_VIEWER, login: "", acts: [], grants: [] },
    WRITE_REASONS.anonymous,
  ],
  ["a person the engine does not serve", { ...READER_VIEWER, acts: [] }, WRITE_REASONS.not_served],
])("Save for %s says why it cannot save", async (_, viewer, reason) => {
  await show({ mine: { text: "Lead with the numbers." } }, viewer);
  const save = within(card("Your instructions")).getByRole("button", { name: "Save" });
  expect(save.getAttribute("aria-disabled")).toBe("true");
  const described = (save.getAttribute("aria-describedby") ?? "")
    .split(" ")
    .map((id) => document.getElementById(id)?.textContent);
  expect(described).toContain(reason);
});

// A TEXT IS COUNTED AGAINST THE ENGINE'S CAP WHERE IT IS TYPED, and Save holds
// past it. Mutation: drop overCap from Save's reason and this fails.
test("a text past the cap is said under its field and Save holds", async () => {
  await show({});
  const company = card("Company instructions");
  const save = within(company).getByRole("button", { name: "Save" });
  expect(save.getAttribute("aria-disabled")).toBe("true");
  fireEvent.change(within(company).getByRole("textbox"), {
    target: { value: "x".repeat(ASSISTANT_INSTRUCTIONS_MAX_RUNES + 1) },
  });
  expect(within(company).getByText(/Shorten it to 1000 characters/)).toBeTruthy();
  expect(save.getAttribute("aria-disabled")).toBe("true");
});

// A READER READS THE COMPANY'S AND CHANGES ONLY THEIR OWN: the company layer is
// text with where it is set, never a field; their own is a form.
test("a reader reads the company's instructions and edits only their own", async () => {
  await show(
    { company: { length: "brief", text: "Use British spelling." } },
    { ...READER_VIEWER, acts: ACTS },
  );
  const company = card("Company instructions");
  expect(within(company).queryByRole("textbox")).toBeNull();
  expect(company.textContent).toContain("Use British spelling.");
  expect(company.textContent).toContain("Brief");
  expect(company.textContent).toContain("Set by an administrator for everybody.");
  expect(within(card("Your instructions")).getByRole("textbox")).toBeTruthy();
});

// YOUR SAVE SENDS THE WHOLE LAYER AT THE REVISION READ, through the act route
// Ask Crewlet also proposes it by. Mutation: send if_match 0 and this fails.
test("your instructions are saved whole at the revision read", async () => {
  const posted: { tool: string; args: Record<string, unknown> }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      const tool = decodeURIComponent(String(url).split("/operator/act/")[1] ?? "");
      posted.push({
        tool,
        args: (JSON.parse(init.body as string) as { args: Record<string, unknown> }).args,
      });
      return new Response(
        JSON.stringify({
          tool,
          outcome: "applied",
          position: null,
          receipt: { seat: "jane", revision: 4 },
        }),
        { status: 200, headers: { ...ENGINE_HEADERS, "Content-Type": "application/json" } },
      );
    }),
  );
  await show({ mine: { text: "Lead with the numbers." }, revision: 3 });
  const yours = card("Your instructions");
  fireEvent.click(within(yours).getByRole("radio", { name: "Brief" }));
  fireEvent.click(within(yours).getByRole("button", { name: "Save" }));
  await act(afterMicrotasks);
  expect(posted).toEqual([
    {
      tool: "save_assistant_instructions",
      args: { instructions: { length: "brief", text: "Lead with the numbers." }, if_match: 3 },
    },
  ]);
});

/** The act route answering every press with `answer`, at `status`. */
function actAnswers(status: number, answer: Record<string, unknown>) {
  vi.stubGlobal(
    "fetch",
    vi.fn(
      async () =>
        new Response(JSON.stringify(answer), {
          status,
          headers: { ...ENGINE_HEADERS, "Content-Type": "application/json" },
        }),
    ),
  );
}

// YOUR SAVE IS CONFIRMED BY THE TOAST every write raises, in this page's
// words, and by no line under the form. Mutation: save quietly and draw the
// sentence in the card, and this fails.
test("saving your instructions is confirmed by its toast, never a line in the card", async () => {
  actAnswers(200, { outcome: "applied", position: null, receipt: { seat: "jane", revision: 1 } });
  await show({});
  const yours = card("Your instructions");
  fireEvent.click(within(yours).getByRole("radio", { name: "Brief" }));
  fireEvent.click(within(yours).getByRole("button", { name: "Save" }));
  await act(afterMicrotasks);
  expect(toastText()).toContain("Saved your instructions");
  expect(toastText()).toContain("Ask Crewlet follows them from your next question.");
  expect(within(yours).queryByText(/follows them/)).toBeNull();
  expect(within(yours).queryByRole("status")).toBeNull();
});

// A REFUSAL STAYS BESIDE THE FORM, where the person fixes it, and no toast
// says it saved.
test("a refused save of your instructions is said in the card, not as a toast", async () => {
  actAnswers(409, { error: "stale_version", message: "Changed by somebody else." });
  await show({});
  const yours = card("Your instructions");
  fireEvent.click(within(yours).getByRole("radio", { name: "Brief" }));
  fireEvent.click(within(yours).getByRole("button", { name: "Save" }));
  await act(afterMicrotasks);
  expect(within(yours).getByText("Nothing was saved")).toBeTruthy();
  expect(toastText()).not.toContain("Saved your instructions");
});

/** The company document's write, answered valid and stored as r2 at epoch 5. */
function companyEngine() {
  return stubEngine((call) => {
    if (call.method === "GET") return engineAnswer({ name: "Acme" }, 200, { ETag: '"r1"' });
    if (call.path.includes("dry_run=true")) return validAnswer();
    return savedAnswer();
  });
}

async function saveCompany() {
  const store = await show({});
  const company = card("Company instructions");
  fireEvent.click(within(company).getByRole("radio", { name: "Brief" }));
  fireEvent.click(within(company).getByRole("button", { name: "Save" }));
  await act(afterMicrotasks);
  return { store, company };
}

// THE COMPANY'S SAVE IS ITS TOAST, applying, then replaced in place once this
// node applies the revision. Mutation: draw the standing in the card, or raise
// a second toast for the apply, and this fails.
test("saving the company's instructions is one toast, updated once this node applies it", async () => {
  companyEngine();
  const { store, company } = await saveCompany();
  expect(toastText()).toContain("Saved the company's instructions");
  expect(toastText()).toContain(APPLYING_SENTENCE);
  expect(within(company).queryByText(/Saved|Applying/)).toBeNull();
  await act(async () => {
    store.applyHealth(healthFrame({ applied_epoch: 5 }));
  });
  await act(afterMicrotasks);
  expect(toastText()).toContain("Ask Crewlet follows them from the next question.");
  expect(toastText()).not.toContain(APPLYING_SENTENCE);
  expect(toastText().match(/Saved the company's instructions/g)).toHaveLength(1);
  expect(within(company).queryByText(/follows them/)).toBeNull();
});

// A NODE THAT REFUSED THE REVISION is said in the card, as a refusal, and
// the toast that said it was applying goes. It said "Ask Crewlet follows
// them" over a revision this node refused.
test("a company save this node refused is said in the card, and its toast goes", async () => {
  companyEngine();
  const { store, company } = await saveCompany();
  await act(async () => {
    store.applyHealth(
      healthFrame({
        applied_epoch: 4,
        config_attempt: { epoch: 5, revision_id: "r2", status: "error" },
      }),
    );
  });
  await act(afterMicrotasks);
  expect(within(company).getByText("Saved, but this node refused it")).toBeTruthy();
  expect(toastText()).not.toContain(APPLYING_SENTENCE);
  expect(toastText()).not.toContain("follows them");
});
