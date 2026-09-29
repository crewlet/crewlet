/**
 * Creating the company: the form produces one template operation the engine
 * checks like any other draft, the save creates the settings create-only
 * fleet-wide and then writes the chart, and a company that appeared meanwhile
 * is never written over or replayed onto.
 */

import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import type { CompanyDocument } from "~/protocol/index.ts";
import { clearSavedChanges } from "./savedChanges.ts";
import { company, Engine, mountBuilder, type SentRequest } from "./testkit.tsx";

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

/** Opens the review and creates the company. */
async function save() {
  fireEvent.click(screen.getByRole("button", { name: "Review and save" }));
  const dialog = await screen.findByRole("dialog", { name: "Review and create the company" });
  fireEvent.click(within(dialog).getByRole("button", { name: "Create the company" }));
}

/** Fills the create form and starts the company from a template. */
async function startCompany(options: { template?: string; seat?: boolean; contact?: string } = {}) {
  fireEvent.change(await screen.findByLabelText("Company name"), {
    target: { value: "Nimbus" },
  });
  fireEvent.change(screen.getByLabelText("Mission"), { target: { value: "Ship weather." } });
  if (options.template) {
    fireEvent.click(screen.getByRole("radio", { name: options.template }));
  }
  if (options.seat) {
    fireEvent.click(screen.getByRole("checkbox", { name: "Add a seat for yourself" }));
    fireEvent.change(screen.getByLabelText("Your seat's name"), { target: { value: "Founder" } });
    fireEvent.change(screen.getByLabelText(/^Slack member ID/), {
      target: { value: options.contact ?? "U0FOUNDER" },
    });
  }
  fireEvent.click(screen.getByRole("button", { name: "Start the company" }));
}

test("the form starts the company from a template, and the check is create-only", async () => {
  const engine = new Engine(null);
  mountBuilder({ engine });
  await startCompany({ seat: true });

  await waitFor(() => expect(engine.checks().length).toBeGreaterThan(1));
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
  expect(await screen.findByRole("button", { name: "Start the company" })).toBeDefined();
});

// YOUR OWN SEAT NEEDS NO CONTACT IDENTITY. The engine admits a human seat with
// none — an operator who works only through the dashboard has no chat account
// to type — so the form starts the company rather than refusing it, and writes
// no contact block rather than an empty one.
test("your own seat starts the company with no contact identity", async () => {
  const engine = new Engine(null);
  mountBuilder({ engine });
  await startCompany({ seat: true, contact: "" });
  await screen.findByText("No problems");
  await save();

  await waitFor(() => expect(engine.seats.find((s) => s.handle === "founder")).toBeDefined());
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
  await screen.findByText("No problems");
  await save();

  await waitFor(() =>
    expect(engine.seats.find((s) => s.handle === "founder")?.runtime).toBeDefined(),
  );
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
  await screen.findByLabelText("Company name");
  expect(screen.queryByRole("toolbar", { name: "Organization builder" })).toBeNull();
  expect(document.querySelector(".org-builder-toolbar")).toBeNull();
  await startCompany({});
  expect(await screen.findByRole("toolbar", { name: "Organization builder" })).toBeDefined();
});

test("a company with no name is refused by the form, not by the engine", async () => {
  const engine = new Engine(null);
  mountBuilder({ engine });
  await screen.findByLabelText("Company name");
  const before = engine.requests.length;
  fireEvent.click(screen.getByRole("button", { name: "Start the company" }));
  expect(await screen.findByText("Enter the company name.")).toBeDefined();
  expect(engine.requests.length).toBe(before);
});

test("the save creates the settings create-only, then the chart, and says what is left to do", async () => {
  const engine = new Engine(null);
  mountBuilder({ engine });
  await startCompany({});
  await screen.findByText("No problems");
  await save();

  await waitFor(() => expect(engine.requests.filter(isWrite)).toHaveLength(1));
  const write = engine.requests.filter(isWrite)[0]!;
  expect(write.headers["If-None-Match"]).toBe("*");
  expect(write.headers["Content-Type"]).toBe("application/json");
  const body = write.body as CompanyDocument & { _summary: string };
  expect(body.name).toBe("Nimbus");
  expect(body).not.toHaveProperty("roles");
  expect(body).not.toHaveProperty("units");
  expect(body._summary).toMatch(/^Created Nimbus in the organization builder \(write [0-9a-f]+\)$/);
  // THE SETTINGS FIRST: the company exists before its chart is written.
  await waitFor(() =>
    expect(engine.units.map((u) => u.key).sort()).toEqual(["engineering", "marketing", "product"]),
  );
  const firstChartWrite = engine.requests.findIndex(
    (r) => r.path.startsWith("/chart/") && r.method !== "GET",
  );
  expect(engine.requests.indexOf(write)).toBeLessThan(firstChartWrite);
  // Every chart write carries the operation id a retry would resend.
  expect(engine.chartWrites().every((r) => /-\d+$/.test(r.headers["Idempotency-Key"] ?? ""))).toBe(
    true,
  );

  // The two steps the dashboard cannot take, with the command for the one
  // that has no screen at all.
  expect(await screen.findByText("The company is created")).toBeDefined();
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
  await screen.findByText("No problems");
  createdElsewhere(engine);
  // The next check of the draft is refused as already configured.
  fireEvent.click(screen.getByRole("button", { name: "Edit Chief Executive" }));
  expect(
    await screen.findByRole("dialog", { name: "A company already exists on this engine" }),
  ).toBeDefined();
  expect(engine.requests.filter(isWrite)).toHaveLength(0);
});

// THE ORG PUSH SAYS A COMPANY EXISTS before any edit or save does, so a
// create draft hears of it at once rather than when its next check runs.
test("a company another node creates is reported by the org push, with no edit", async () => {
  const engine = new Engine(null);
  const { store } = mountBuilder({ engine });
  await startCompany({});
  await screen.findByText("No problems");
  createdElsewhere(engine);
  act(() => store.applyOrg(engine.orgPush()));
  expect(
    await screen.findByRole("dialog", { name: "A company already exists on this engine" }),
  ).toBeDefined();
  expect(engine.requests.filter(isWrite)).toHaveLength(0);
});

// THE ONE THAT MUST NEVER REGRESS: a create draft is never applied to a
// company somebody else created, and never replayed onto it.
test("a company created meanwhile is offered instead of the draft, never written over", async () => {
  const engine = new Engine(null);
  mountBuilder({ engine });
  await startCompany({});
  await screen.findByText("No problems");
  createdElsewhere(engine);
  await save();

  const refused = await screen.findByRole("dialog", {
    name: "A company already exists on this engine",
  });
  // The company is untouched: the chart read before the first write found it,
  // and nothing was written at all.
  expect(engine.settings).toEqual(company().settings);
  expect(engine.requests.filter(isWrite)).toHaveLength(0);
  expect(engine.chartWrites()).toHaveLength(0);
  fireEvent.click(within(refused).getByRole("button", { name: "Discard it and open the company" }));

  // The company that exists is loaded, in edit mode, with nothing replayed.
  expect(await screen.findByText("CEO")).toBeDefined();
  await screen.findByText("No problems");
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
  await screen.findByText("No problems");
  engine.settings = company().settings;
  await save();

  expect(
    await screen.findByRole("dialog", { name: "A company already exists on this engine" }),
  ).toBeDefined();
  expect(engine.chartWrites()).toHaveLength(0);
});

// KEEPING THE DRAFT IS NOT A DEAD END. It can never be saved, and the lens is
// read-only over it, so what it offers instead has to stay on screen after
// the dialog is closed: without it the only way out was a reload.
test("a create draft kept after a company appeared still offers the company", async () => {
  const engine = new Engine(null);
  mountBuilder({ engine });
  await startCompany({});
  await screen.findByText("No problems");
  createdElsewhere(engine);
  fireEvent.click(screen.getByRole("button", { name: "Edit Chief Executive" }));
  const refused = await screen.findByRole("dialog", {
    name: "A company already exists on this engine",
  });
  fireEvent.click(within(refused).getByRole("button", { name: "Keep my draft" }));
  expect(
    screen.getByText(
      "A company was created on this engine while this draft was being written, so this draft cannot be saved.",
    ),
  ).toBeDefined();
  fireEvent.click(screen.getByRole("button", { name: "Discard it and open the company" }));
  expect(await screen.findByText("CEO")).toBeDefined();
  await screen.findByText("No problems");
  expect(engine.requests.filter(isWrite)).toHaveLength(0);
  expect(engine.chartWrites()).toHaveLength(0);
});
