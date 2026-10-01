/**
 * Creating the company: the form produces one template operation the engine
 * checks like any other draft, the save creates the settings create-only
 * fleet-wide and then writes the chart, and a company that appeared meanwhile
 * is never written over or replayed onto.
 */

import { act, cleanup, fireEvent, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import type { CompanyDocument } from "~/protocol/index.ts";
import { clearSavedChanges } from "./savedChanges.ts";
import {
  checked,
  company,
  Engine,
  lensToolbar,
  mountBuilder,
  pressInToolbar,
  pressInView,
  pressOnBanner,
  settle,
  type SentRequest,
} from "./testkit.tsx";

beforeEach(() => {
  sessionStorage.clear();
  clearSavedChanges();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  sessionStorage.clear();
  clearSavedChanges();
  location.hash = "#/";
});

/** The names a stand-in view lists under `label`. */
const listed = (label: string) =>
  within(screen.getByRole("list", { name: label }))
    .getAllByRole("listitem")
    .map((item) => item.querySelector("span")!.textContent);

/** A company somebody else creates while the draft is open. */
function createdElsewhere(engine: Engine) {
  const { settings, chart } = company();
  engine.settings = settings;
  engine.revision = "r1";
  engine.units = chart.units;
  engine.seats = chart.seats;
  engine.manages = chart.manages ?? {};
}

const isWrite = (r: SentRequest) =>
  r.path === "/config" && r.method === "PUT" && !r.query.has("dry_run");

/** The dialog that says a company exists, once the lens has settled on finding it. */
const companyExists = () =>
  screen.getByRole("dialog", { name: "A company already exists on this engine" });

/** Opens the review, creates the company, and settles on everything the save set in motion. */
async function save() {
  pressInToolbar("Review and save");
  const dialog = screen.getByRole("dialog", { name: "Review and create the company" });
  fireEvent.click(within(dialog).getByRole("button", { name: "Create the company" }));
  await settle();
}

/**
 * The create form: the card that holds the company's name.
 *
 * Its controls are asked for INSIDE it. A role query by name computes the
 * accessible name of every candidate in its container, and across the whole
 * lens that made this form's own presses the costliest lines of these cases.
 */
function createForm(): HTMLElement {
  const form = screen.getByLabelText("Company name").closest<HTMLElement>("section");
  if (!form) throw new Error("the company name is not in the create form");
  return form;
}

/** Presses the create form's Start the company. */
const pressStart = () =>
  fireEvent.click(within(createForm()).getByRole("button", { name: "Start the company" }));

/** Fills the create form and starts the company from a template. */
async function startCompany(options: { template?: string; seat?: boolean; contact?: string } = {}) {
  await settle();
  const form = within(createForm());
  fireEvent.change(form.getByLabelText("Company name"), {
    target: { value: "Nimbus" },
  });
  fireEvent.change(form.getByLabelText("Mission"), { target: { value: "Ship weather." } });
  if (options.template) {
    fireEvent.click(form.getByRole("radio", { name: options.template }));
  }
  if (options.seat) {
    fireEvent.click(form.getByRole("checkbox", { name: "Add a seat for yourself" }));
    fireEvent.change(form.getByLabelText("Your seat's name"), { target: { value: "Founder" } });
    fireEvent.change(form.getByLabelText(/^Slack member ID/), {
      target: { value: options.contact ?? "U0FOUNDER" },
    });
  }
  pressStart();
}

test("the form starts the company from a template, and the check is create-only", async () => {
  const engine = new Engine(null);
  mountBuilder({ engine });
  await startCompany({ seat: true });

  await settle();
  expect(engine.checks().length).toBeGreaterThan(1);
  const check = engine.checks().at(-1)!;
  expect(check.method).toBe("PUT");
  expect(check.headers["If-None-Match"]).toBe("*");
  // THE SETTINGS ONLY: the chart has no dry run, and the configuration
  // refuses a body that carries one.
  expect(check.body).toEqual({ name: "Nimbus", mission: "Ship weather." });
  // The operator's own seat and the template's neutral titles, in the draft.
  expect(listed("Seats")).toContain("Founder");
  expect(listed("Seats")).toContain("Chief Executive");
  expect(listed("Units")).toEqual(["Engineering", "Marketing", "Product"]);
  // And the whole start is one operation: undone, the form is back.
  fireEvent.keyDown(document.body, { key: "z", code: "KeyZ", ctrlKey: true });
  await settle();
  expect(within(createForm()).getByRole("button", { name: "Start the company" })).toBeDefined();
});

// YOUR OWN SEAT NEEDS NO CONTACT IDENTITY. The engine admits a human seat with
// none — an operator who works only through the dashboard has no chat account
// to type — so the form starts the company rather than refusing it, and writes
// no contact block rather than an empty one.
test("your own seat starts the company with no contact identity", async () => {
  const engine = new Engine(null);
  mountBuilder({ engine });
  await startCompany({ seat: true, contact: "" });
  await checked();
  await save();

  const founder = engine.seats.find((s) => s.handle === "founder")!;
  expect(founder).toMatchObject({ name: "Founder", kind: "human" });
  expect(founder).not.toHaveProperty("runtime");
});

// AND WITH ONE, THE IDENTITY THEY GAVE rides the seat's runtime half, where
// the chart keeps a seat's contact identities.
test("your own seat carries the contact identity you gave", async () => {
  const engine = new Engine(null);
  mountBuilder({ engine });
  await startCompany({ seat: true });
  await checked();
  await save();

  expect(engine.seats.find((s) => s.handle === "founder")!.runtime).toEqual({
    contact: { slack_user_id: "U0FOUNDER" },
  });
});

// NOTHING TO UNDO, CHECK OR SAVE until a template is recorded, so the form
// stands alone. `hidden` alone did not hide it: the toolbar's own
// `display: flex` outranks the user agent's rule for the attribute.
test("the create form carries no builder toolbar until a template is recorded", async () => {
  const engine = new Engine(null);
  mountBuilder({ engine });
  await settle();
  expect(screen.getByLabelText("Company name")).toBeDefined();
  expect(screen.queryByRole("toolbar", { name: "Organization builder" })).toBeNull();
  expect(document.querySelector(".org-builder-toolbar")).toBeNull();
  await startCompany({});
  await settle();
  expect(lensToolbar()).toBeDefined();
});

test("a company with no name is refused by the form, not by the engine", async () => {
  const engine = new Engine(null);
  mountBuilder({ engine });
  await settle();
  const before = engine.requests.length;
  pressStart();
  await settle();
  expect(within(createForm()).getByText("Enter the company name.")).toBeDefined();
  expect(engine.requests.length).toBe(before);
});

test("the save creates the settings create-only, then the chart, and says what is left to do", async () => {
  const engine = new Engine(null);
  mountBuilder({ engine });
  await startCompany({});
  await checked();
  await save();

  expect(engine.requests.filter(isWrite)).toHaveLength(1);
  const write = engine.requests.filter(isWrite)[0]!;
  expect(write.headers["If-None-Match"]).toBe("*");
  expect(write.headers["Content-Type"]).toBe("application/json");
  const body = write.body as CompanyDocument & { _summary: string };
  expect(body.name).toBe("Nimbus");
  expect(body).not.toHaveProperty("roles");
  expect(body).not.toHaveProperty("units");
  expect(body._summary).toMatch(
    /^Created Nimbus in the organization builder \(write [0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\)$/,
  );
  // THE SETTINGS FIRST: the company exists before its chart is written.
  expect(engine.units.map((u) => u.key).sort()).toEqual(["engineering", "marketing", "product"]);
  const firstChartWrite = engine.requests.findIndex(
    (r) => r.path.startsWith("/chart/") && r.method !== "GET",
  );
  expect(engine.requests.indexOf(write)).toBeLessThan(firstChartWrite);
  // Every chart write carries the operation id a retry would resend.
  expect(
    engine
      .chartWrites()
      .every((r) => /\.builder-save\.\d+$/.test(r.headers["Idempotency-Key"] ?? "")),
  ).toBe(true);

  // The two steps the dashboard cannot take, with the command for the one
  // that has no screen at all.
  expect(screen.getByText("The company is created")).toBeDefined();
  expect(screen.getByRole("link", { name: "Open Integrations" }).getAttribute("href")).toBe(
    "#/admin/integrations",
  );
  // A MERGE PATCH OF THE PROVIDERS, which changes nothing else: an import of
  // a company file would write that file's org chart over the one just made.
  expect(screen.getByText(/-X PATCH "\$CREWLET_URL\/config"/)).toBeDefined();
  expect(screen.queryByText(/crewlet config import/)).toBeNull();
  expect(
    screen.getByText(/no agent seat takes a turn, and work sent to a seat waits/),
  ).toBeDefined();
  // The first revision has no parent to differ from, so the strip opens the
  // configuration itself rather than an empty diff.
  expect(screen.queryByRole("link", { name: "View changes" })).toBeNull();
  expect(screen.getByRole("link", { name: "View the configuration" }).getAttribute("href")).toBe(
    "#/admin/config",
  );
});

test("a company created while the draft is open is found by the check, before any save", async () => {
  const engine = new Engine(null);
  mountBuilder({ engine });
  await startCompany({});
  await checked();
  createdElsewhere(engine);
  // The next check of the draft is refused as already configured.
  pressInView("Edit Chief Executive");
  await settle();
  expect(companyExists()).toBeDefined();
  expect(engine.requests.filter(isWrite)).toHaveLength(0);
});

// THE ORG PUSH SAYS A COMPANY EXISTS before any edit or save does, so a
// create draft hears of it at once rather than when its next check runs.
test("a company another node creates is reported by the org push, with no edit", async () => {
  const engine = new Engine(null);
  const { store } = mountBuilder({ engine });
  await startCompany({});
  await checked();
  createdElsewhere(engine);
  act(() => store.applyOrg(engine.orgPush()));
  await settle();
  expect(companyExists()).toBeDefined();
  expect(engine.requests.filter(isWrite)).toHaveLength(0);
});

// THE ONE THAT MUST NEVER REGRESS: a create draft is never applied to a
// company somebody else created, and never replayed onto it.
test("a company created meanwhile is offered instead of the draft, never written over", async () => {
  const engine = new Engine(null);
  mountBuilder({ engine });
  await startCompany({});
  await checked();
  createdElsewhere(engine);
  await save();

  const refused = companyExists();
  // The company is untouched: the chart read before the first write found it,
  // and nothing was written at all.
  expect(engine.settings).toEqual(company().settings);
  expect(engine.requests.filter(isWrite)).toHaveLength(0);
  expect(engine.chartWrites()).toHaveLength(0);
  fireEvent.click(within(refused).getByRole("button", { name: "Discard it and open the company" }));

  // The company that exists is loaded, in edit mode, with nothing replayed.
  await checked();
  expect(screen.getByText("CEO")).toBeDefined();
  expect(screen.queryByText("Chief Executive")).toBeNull();
  // Nothing replayed: the company it opened is checked with no settings edit to dry-run.
  expect(engine.checks().every((r) => r.method === "PUT")).toBe(true);
});

// AND WHERE ONLY THE SETTINGS APPEARED: the settings write is create-only
// fleet-wide, so its 412 stops the save before a single chart write.
test("a settings revision created meanwhile refuses the create before the chart is written", async () => {
  const engine = new Engine(null);
  mountBuilder({ engine });
  await startCompany({});
  await checked();
  engine.settings = company().settings;
  await save();

  expect(companyExists()).toBeDefined();
  expect(engine.chartWrites()).toHaveLength(0);
});

// KEEPING THE DRAFT IS NOT A DEAD END. It can never be saved, and the lens is
// read-only over it, so what it offers instead has to stay on screen after
// the dialog is closed: without it the only way out was a reload.
test("a create draft kept after a company appeared still offers the company", async () => {
  const engine = new Engine(null);
  mountBuilder({ engine });
  await startCompany({});
  await checked();
  createdElsewhere(engine);
  pressInView("Edit Chief Executive");
  await settle();
  fireEvent.click(within(companyExists()).getByRole("button", { name: "Keep my draft" }));
  const banner =
    "A company was created on this engine while this draft was being written, so this draft cannot be saved.";
  expect(screen.getByText(banner)).toBeDefined();
  pressOnBanner(banner, "Discard it and open the company");
  await checked();
  expect(screen.getByText("CEO")).toBeDefined();
  expect(engine.requests.filter(isWrite)).toHaveLength(0);
  expect(engine.chartWrites()).toHaveLength(0);
});
