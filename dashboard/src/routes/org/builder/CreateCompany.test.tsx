/**
 * Creating the company: the form produces one template operation the engine
 * checks like any other draft, the save is create-only fleet-wide, and a
 * company that appeared meanwhile is never written over or replayed onto.
 */

import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import type { CompanyDocument } from "~/protocol/index.ts";
import { clearSavedRevision } from "./savedRevision.ts";
import { company, Engine, mountBuilder, type SentRequest } from "./testkit.tsx";

beforeEach(() => {
  sessionStorage.clear();
  clearSavedRevision();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  sessionStorage.clear();
  clearSavedRevision();
  location.hash = "#/";
});

const isWrite = (r: SentRequest) =>
  r.path === "/config" && r.method === "PUT" && !r.query.has("dry_run");

/** Fills the create form and starts the company from a template. */
async function startCompany(options: { template?: string; seat?: boolean } = {}) {
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
    fireEvent.change(screen.getByLabelText("Slack member ID"), { target: { value: "U0FOUNDER" } });
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
  const sent = check.body as CompanyDocument;
  expect(sent.name).toBe("Nimbus");
  expect(sent.mission).toBe("Ship weather.");
  // The operator's own seat, with the one identity they gave, and the
  // template's neutral titles below it.
  expect(sent.roles?.[0]).toMatchObject({
    name: "Founder",
    kind: "human",
    contact: { slack_user_id: "U0FOUNDER" },
  });
  expect(sent.roles?.[1]?.name).toBe("Chief Executive");
  expect(sent.units?.map((u) => u.name)).toEqual(["Engineering", "Product", "Marketing"]);
  // And the whole start is one operation: undone, the form is back.
  fireEvent.keyDown(document.body, { key: "z", code: "KeyZ", ctrlKey: true });
  expect(await screen.findByRole("button", { name: "Start the company" })).toBeDefined();
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

test("the save is a create-only PUT, and says what is left to do", async () => {
  const engine = new Engine(null);
  mountBuilder({ engine });
  await startCompany({});
  await screen.findByText("No problems");
  fireEvent.click(screen.getByRole("button", { name: "Review and save" }));
  const dialog = await screen.findByRole("dialog", { name: "Review and create the company" });
  fireEvent.click(within(dialog).getByRole("button", { name: "Create the company" }));

  await waitFor(() => expect(engine.requests.filter(isWrite)).toHaveLength(1));
  const write = engine.requests.filter(isWrite)[0]!;
  expect(write.headers["If-None-Match"]).toBe("*");
  expect(write.headers["Content-Type"]).toBe("application/json");
  const body = write.body as CompanyDocument & { _summary: string };
  expect(body.name).toBe("Nimbus");
  expect(body._summary).toMatch(/^Created Nimbus in the organization builder \(write [0-9a-f]+\)$/);

  // The two steps the dashboard cannot take, with the command for the one
  // that has no screen at all.
  expect(await screen.findByText("The company is created")).toBeDefined();
  expect(screen.getByRole("link", { name: "Open Integrations" }).getAttribute("href")).toBe(
    "#/integrations",
  );
  expect(screen.getByText(/crewlet config import company.yaml/)).toBeDefined();
  expect(screen.getByText(/every node refuses to apply the company/)).toBeDefined();
  // The first revision has no parent to differ from, so the strip opens the
  // configuration itself rather than an empty diff.
  expect(screen.queryByRole("link", { name: "View changes" })).toBeNull();
  expect(screen.getByRole("link", { name: "View the configuration" }).getAttribute("href")).toBe(
    "#/config",
  );
});

test("a company created while the draft is open is found by the check, before any save", async () => {
  const engine = new Engine(null);
  mountBuilder({ engine });
  await startCompany({});
  await screen.findByText("No problems");
  engine.document = company();
  engine.revision = "r1";
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
  engine.document = company();
  engine.revision = "r1";
  act(() => store.applyOrg({ name: "Acme", roles: [], units: [] }));
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
  engine.document = company();
  engine.revision = "r1";
  fireEvent.click(screen.getByRole("button", { name: "Review and save" }));
  const dialog = await screen.findByRole("dialog", { name: "Review and create the company" });
  fireEvent.click(within(dialog).getByRole("button", { name: "Create the company" }));

  const refused = await screen.findByRole("dialog", {
    name: "A company already exists on this engine",
  });
  // The company is untouched: the 412 is the only write that reached it.
  expect(engine.document).toEqual(company());
  fireEvent.click(within(refused).getByRole("button", { name: "Discard it and open the company" }));

  // The company that exists is loaded, in edit mode, with nothing replayed.
  expect(await screen.findByText("CEO")).toBeDefined();
  await waitFor(() => expect(engine.checks().at(-1)!.method).toBe("PATCH"));
  expect(engine.checks().at(-1)!.body).toEqual({});
});

// KEEPING THE DRAFT IS NOT A DEAD END. It can never be saved, and the lens is
// read-only over it, so what it offers instead has to stay on screen after
// the dialog is closed: without it the only way out was a reload.
test("a create draft kept after a company appeared still offers the company", async () => {
  const engine = new Engine(null);
  mountBuilder({ engine });
  await startCompany({});
  await screen.findByText("No problems");
  engine.document = company();
  engine.revision = "r1";
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
  await waitFor(() => expect(engine.checks().at(-1)!.method).toBe("PATCH"));
  expect(engine.requests.filter(isWrite)).toHaveLength(0);
});
