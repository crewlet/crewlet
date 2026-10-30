/**
 * The models list: four figures, the engine's alarms, and a row per model
 * saying its condition, its sign-in and who runs on it.
 *
 * The invariants: Status is health alone, and use is Used by's (QA M14); Used
 * by counts the Dashboard Assistant as a use for either reader (QA M13) and
 * names seats by their names; the figures say how many are not ready rather
 * than "3 of 4", and the card does not repeat the count (QA Minor 14); Add
 * model is the page's action, in the page bar (QA Minor 15); the actions
 * column is named (QA Minor 17); and a member reads the list without a
 * credential, a cost or an action.
 */

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import { TestClient } from "~/test/client.tsx";
import {
  ASSISTANT_CHAT,
  EVERY_JOB,
  MODELS_NOW,
  at,
  modelEntry,
  modelsData,
  runs,
  signIn,
} from "~/routes/settings/models/fixtures.ts";
import type { ModelsData } from "./data.ts";
import { sheetRules } from "~/test/sheets.ts";
import { Overview } from "./Overview.tsx";

/**
 * `default` runs every job for Backend Engineer and codes for Product
 * Manager, on a priced API key; `opus` sits out its limit; `sonnet` cannot
 * sign in; `assistant` serves the Dashboard Assistant alone; `spare` is new
 * and runs nothing.
 */
function company({ admin = true } = {}): ModelsData {
  return modelsData({
    admin,
    entries: [
      modelEntry({
        key: "default",
        sign_in: signIn({ route: "api-key", mode: "api-key", variable: "ANTHROPIC_API_KEY" }),
        usage: { days: 7, calls: 40, total_tokens: 900_000 },
        used_by: [...runs("backend-engineer", ...EVERY_JOB), ...runs("pm", "executor")],
      }),
      modelEntry({
        key: "opus",
        health: "sitting_out",
        sitting_out: {
          until: at(90),
          resets_at: null,
          window: "session",
          since: at(-10),
          nodes: ["node-1"],
        },
        used_by: runs("qa", "executor"),
      }),
      modelEntry({ key: "sonnet", health: "rejected", used_by: runs("qa", "review") }),
      modelEntry({ key: "assistant", used_by: [ASSISTANT_CHAT] }),
      modelEntry({ key: "spare", health: "untested", last_success: null }),
    ],
  });
}

function mount(data: ModelsData = company()) {
  const onFlow = vi.fn();
  render(
    <TestClient>
      <LayerHost>
        <Router>
          <Overview data={data} onFlow={onFlow} />
        </Router>
      </LayerHost>
    </TestClient>,
  );
  return onFlow;
}

/** A model's row in the list, by its key. */
const row = (key: string) =>
  screen
    .getAllByRole("row")
    .find((r) => r.querySelector(".models-entry-cell .mono")?.textContent === key)!;

/** A stat tile's whole text, by its label. */
const tile = (label: string) =>
  [...document.querySelectorAll<HTMLElement>(".crewlet-statcard")].find(
    (t) => t.querySelector(".crewlet-statcard__label")?.textContent === label,
  )?.textContent ?? "";

beforeEach(() => {
  vi.useFakeTimers({ now: MODELS_NOW, toFake: ["Date"] });
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

test("Status says a model's condition, and only Used by says whether anything runs on it", () => {
  mount();
  const spare = row("spare");
  const cells = within(spare).getAllByRole("cell");
  const status = cells[1]!.textContent ?? "";
  expect(status).toContain("Not tested");
  expect(status).not.toMatch(/used/i);
  expect(spare.textContent).toContain("Not used");
  expect(within(row("sonnet")).getAllByRole("cell")[1]!.textContent).toContain("Sign-in refused");
});

test("Used by leads with Everything, counts the rest whole, and names every seat by name", () => {
  mount();
  const uses = row("default").querySelector<HTMLAnchorElement>(".models-uses")!;
  expect(uses.querySelector(".models-uses__label")?.textContent).toBe("Everything");
  expect(uses.querySelector(".models-uses__more")?.textContent).toBe("+1");
  expect(uses.textContent).toContain("2 seats");
  expect(uses.getAttribute("aria-label")).toBe(
    "Used for Everything: Backend Engineer; Coding: Product Manager",
  );
  expect(uses.getAttribute("href")).toContain("settings/models/default");
});

test("the Dashboard Assistant is a use, for an admin and a member alike", () => {
  for (const admin of [true, false]) {
    mount(company({ admin }));
    const uses = row("assistant").querySelector(".models-uses");
    expect(uses?.querySelector(".models-uses__label")?.textContent).toBe("Ask Crewlet");
    expect(tile("Models")).toContain("1 not used by any seat or Ask Crewlet");
    cleanup();
  }
});

test("the figures say how many are not ready and why, and the card does not count again", () => {
  mount();
  expect(tile("Models")).toContain("5");
  expect(tile("Ready")).toMatch(/2\s*of 5/);
  expect(tile("Ready")).toContain("3 models not ready");
  expect(tile("Sitting out or refused")).toContain("1 sitting out and 1 cannot sign in");
  // TOKENS, NEVER MONEY (dashboard-design rule 19).
  expect(tile("Tokens, 7 days")).toContain("900");
  expect(tile("Tokens, 7 days")).toContain("over 40 calls");
  expect(document.body.textContent).not.toMatch(/\$\d/);
  const head = document.querySelector(".crewlet-card__header")?.textContent ?? "";
  expect(head).not.toMatch(/\d/);
});

test("Add model is the page's action, and opens the add flow", () => {
  const onFlow = mount();
  const add = within(document.querySelector<HTMLElement>(".page-actions")!).getByRole("button", {
    name: "Add model",
  });
  fireEvent.click(add);
  expect(onFlow).toHaveBeenCalledWith("add");
  expect(screen.getAllByRole("button", { name: "Add model" })).toHaveLength(1);
});

test("each row opens the model's page, and its actions column is named", () => {
  mount();
  const link = row("opus").querySelector<HTMLAnchorElement>(".crewlet-data-grid__row-link")!;
  expect(link.getAttribute("href")).toContain("settings/models/opus");
  expect(screen.getAllByRole("columnheader").map((h) => h.textContent)).toContain("Actions");
  expect(within(row("opus")).getByRole("button", { name: "Actions for opus" })).toBeDefined();
});

/**
 * NOTHING IN A ROW IS A CONTROL PRETENDING NOT TO BE. The CLI's mark is an
 * image the row's link lies over, never raised and never pressed; the names
 * are raised only so a pointer reaches their tip, and so they are a real link
 * to the page the row opens, out of the tab order and hidden from assistive
 * technology, which reach the model once, by the row's link.
 */
test("the mark is an inert image, and the names are a real link to the row's page", () => {
  mount();
  const opus = row("opus");
  const mark = within(opus).getByRole("img", { name: "Claude Code" });
  expect(mark.closest("[data-above-row]")).toBeNull();
  expect(mark.closest("a, button")).toBeNull();
  expect(mark.onclick).toBeNull();
  const names = opus.querySelector(".models-entry-cell .models-row-reveal")!;
  expect(names.tagName).toBe("A");
  expect(names.getAttribute("href")).toContain("settings/models/opus");
  expect(names.getAttribute("tabindex")).toBe("-1");
  expect(names.getAttribute("aria-hidden")).toBe("true");
  // NOT A SECOND WAY IN for a screen reader: the row's own link is that.
  expect(within(opus).getAllByRole("link")).not.toContain(names);
});

/**
 * A ROW READS AS ONE: its CLI's mark centred against the two lines beside it,
 * and the secret a model signs in with set exactly as its model id is (the
 * same type, tone and one-line clamp), each under its label at the same
 * spacing, because both pairs are one rule (`.models-pair`).
 */
test("the mark is centred, and the secret's name is set and spaced as the model id is", () => {
  mount();
  const rule = (selector: string) =>
    sheetRules().find((r) => r.file === "models.css" && r.selector === selector)?.body ?? "";
  expect(rule(".models-entry-cell")).toMatch(/align-items: center;/);
  expect(rule(".models-pair")).not.toMatch(/gap:/);
  const cells = within(row("default")).getAllByRole("cell");
  const id = within(cells[0]!).getByText("claude-sonnet-5", { exact: false });
  const secret = within(cells[2]!).getByText("ANTHROPIC_API_KEY");
  expect([...secret.classList].sort()).toEqual([...id.classList].sort());
  expect(secret.parentElement?.className).toBe("models-pair");
  expect(id.parentElement?.className).toBe("models-pair");
});

test("a member reads the list without a credential, a cost or an action", () => {
  mount(company({ admin: false }));
  expect(screen.getByText("Read-only")).toBeDefined();
  const heads = screen.getAllByRole("columnheader").map((h) => h.textContent);
  expect(heads).not.toContain("Sign-in");
  expect(heads).not.toContain("Actions");
  expect(screen.queryByText("Tokens, 7 days")).toBeNull();
  expect(screen.queryByRole("button", { name: "Add model" })).toBeNull();
  expect(document.body.textContent).not.toContain("ANTHROPIC_API_KEY");
});

test("with no model, the list says so and offers to add one", () => {
  const onFlow = mount(modelsData({ entries: [] }));
  expect(screen.getByText("No model yet")).toBeDefined();
  const buttons = screen.getAllByRole("button", { name: "Add model" });
  fireEvent.click(buttons[buttons.length - 1]!);
  expect(onFlow).toHaveBeenCalledWith("add");
});
