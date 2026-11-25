/**
 * Settings › Ask Crewlet: the company's instructions, an administrator's to
 * change, and the reader's own, saved under their seat.
 */

import { act, cleanup, fireEvent, screen, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { ASSISTANT_INSTRUCTIONS_MAX_RUNES } from "~/contract/instructions.ts";
import { ENGINE_HEADERS } from "~/test/engineAnswer.ts";
import { mountSettings, READER_VIEWER } from "~/test/settingsScreen.tsx";
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
  mountSettings(
    <AskCrewletSettings />,
    { assistant_instructions: { seat: "jane", company: {}, mine: {}, revision: 0, ...answer } },
    { hash: "#/settings/ask-crewlet", viewer },
  );
  await act(afterMicrotasks);
}

const card = (title: string) => screen.getByRole("heading", { name: title }).closest("section")!;

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
