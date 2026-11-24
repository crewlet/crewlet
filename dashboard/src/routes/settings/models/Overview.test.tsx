/**
 * The models list: the engine's alarms, four figures, and a row per model
 * saying its condition and its sign-in; with no model, one empty state: its
 * title, a line and the add.
 *
 * The invariants: each figure is a label and a value alone, Fleets among
 * them; Status is the condition's word and dot on one line, its time in the
 * tooltip; there is no Used by, which Who runs says; the sign-in is its method
 * alone; Add model is the page's action, in the page bar (QA Minor 15); the
 * actions column is named (QA Minor 17); and a member reads the list without
 * a credential, a cost or an action.
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

test("Status is the condition's word and dot on one line, its time in the tooltip", () => {
  mount();
  const status = (key: string) => within(row(key)).getAllByRole("cell")[1]!;
  expect(status("spare").textContent?.trim()).toBe("Not tested");
  expect(status("sonnet").textContent?.trim()).toBe("Sign-in refused");
  expect(status("opus").textContent?.trim()).toBe("Sitting out");
  expect(status("opus").querySelector(".models-one-line")?.getAttribute("aria-label")).toMatch(
    /^Sitting out, back /,
  );
});

test("the list has no Used by: who runs on a model is said under Who runs", () => {
  mount();
  const grid = screen.getAllByRole("table")[0]!;
  expect(
    within(grid)
      .getAllByRole("columnheader")
      .map((h) => h.textContent),
  ).toEqual(["Model", "Status", "Usage limits", "Sign-in", "Last answer", "Actions"]);
  expect(document.querySelector(".models-uses")).toBeNull();
});

// EACH FIGURE ITS LABEL AND VALUE ALONE, Fleets beside the models' three.
test("the four figures are a label and a value each, Fleets among them", () => {
  mount();
  expect(tile("Models")).toBe("Models5");
  expect(tile("Ready")).toMatch(/^Ready2\s*of 5$/);
  expect(tile("Tokens, 7 days")).toContain("900");
  expect(tile("Fleets")).toBe("Fleets1");
  expect(document.querySelectorAll(".crewlet-statcard")).toHaveLength(4);
  for (const sub of document.querySelectorAll(".crewlet-statcard__sub"))
    expect(sub.textContent).toBe("");
  // TOKENS, NEVER MONEY (dashboard-design rule 19).
  expect(document.body.textContent).not.toMatch(/\$\d/);
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
 * NOTHING IN A ROW IS A CONTROL PRETENDING NOT TO BE. The harness's mark is an
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

// A ROW READS AS ONE: its harness's mark centred against its two lines; its own page names the secret.
test("the mark is centred, and the sign-in is its method alone", () => {
  mount();
  const rule = (selector: string) =>
    sheetRules().find((r) => r.file === "models.css" && r.selector === selector)?.body ?? "";
  expect(rule(".models-entry-cell")).toMatch(/align-items: center;/);
  const cells = within(row("default")).getAllByRole("cell");
  const signInCell = cells.find((c) => c.dataset["label"] === "Sign-in")!;
  expect(signInCell.textContent).toBe("API key");
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

// WITH NO MODEL, ONE EMPTY STATE: its title, what a model is in a line, and
// the add; nothing else (no picture, no faces, no rows to pick in).
test("with no model, the page is one card saying what a model is and offering Add model", () => {
  const onFlow = mount(modelsData({ entries: [] }));
  const empty = document.querySelector<HTMLElement>(".crewlet-empty-state")!;
  expect(within(empty).getByText("No model yet")).toBeDefined();
  expect(empty.textContent).toBe(
    "No model yet" +
      "A model is a coding harness, such as Claude Code or Codex, signed in to its vendor. Add one and choose who runs on it." +
      "Add model",
  );
  expect(empty.querySelector(".crewlet-empty-state__mark")).toBeNull();
  const pictures = [...empty.querySelectorAll("svg, img")].filter((el) => !el.closest("button"));
  expect(pictures).toHaveLength(0);
  expect(document.body.textContent).not.toMatch(/waiting for one/);
  // THE PAGE'S OWN Add model, and the card's.
  const page = within(document.querySelector<HTMLElement>(".page-actions")!);
  expect(page.getAllByRole("button", { name: "Add model" })).toHaveLength(1);
  fireEvent.click(within(empty).getByRole("button", { name: "Add model" }));
  expect(onFlow).toHaveBeenCalledWith("add");
  // NOTHING TO PICK, and no rows saying so: no figures, no grid, no who-runs section.
  expect(screen.queryByRole("list", { name: "Seats" })).toBeNull();
  expect(screen.queryByRole("heading", { name: "Who runs on which model" })).toBeNull();
  expect(screen.queryAllByRole("combobox")).toHaveLength(0);
  expect(document.querySelectorAll(".crewlet-statcard")).toHaveLength(0);
});

// A MEMBER reads the same card without the add.
test("with no model, a member reads who waits and is told an admin adds them", () => {
  mount(modelsData({ admin: false, entries: [] }));
  expect(document.body.textContent).toContain("An admin adds models here.");
  expect(screen.queryByRole("button", { name: "Add model" })).toBeNull();
});

// NO LEDE: each card's heading says what it holds.
test("the page has no lede; the models card is headed, then the fleets and who runs follow", () => {
  mount();
  expect(document.querySelector(".crewlet-page-note")).toBeNull();
  const models = screen.getByRole("heading", { name: "Models" }).closest("section")!;
  expect(models.textContent).toContain("A model and its sign-in: a plan, an API key or a gateway.");
  const fleets = screen.getByRole("heading", { name: "Fleets" });
  const who = screen.getByRole("heading", { name: "Who runs on which model" });
  expect(models.compareDocumentPosition(fleets) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  expect(fleets.compareDocumentPosition(who) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
});

// THE ORDER IN FORCE IS ON A HEAD, as on every list: the models are in key
// order, so the Model head says so; Status sorts worst first.
test("the list says its order on the Model head, and Status sorts the worst condition first", () => {
  mount();
  // THE MODELS' TABLE, the first on the page; every seat's jobs is another under it.
  const grid = screen.getAllByRole("table")[0]!;
  const head = within(grid).getByRole("columnheader", { name: /^Model/ });
  expect(head.getAttribute("aria-sort")).toBe("ascending");
  const keys = () =>
    within(grid)
      .getAllByRole("row")
      .slice(1)
      .map((r) => r.querySelector(".models-clamp")?.textContent);
  expect(keys()).toEqual(["assistant", "default", "opus", "sonnet", "spare"]);
  fireEvent.click(
    within(within(grid).getByRole("columnheader", { name: /^Status/ })).getByRole("button"),
  );
  expect(keys()).toEqual(["sonnet", "opus", "spare", "default", "assistant"]);
});
