/**
 * Creating the company: the form produces one template operation the engine
 * checks like any other draft, the save is create-only fleet-wide, and a
 * company that appeared meanwhile is never written over or replayed onto.
 */

import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { PROVIDER_STEPS } from "~/contract/providersteps.ts";
import type { CompanyDocument } from "~/protocol/index.ts";
import { clearSavedRevision } from "./savedRevision.ts";
import { company, EDITOR, Engine, mountBuilder, type SentRequest } from "./testkit.tsx";
import { withDerived } from "~/test/org.ts";
import {
  applyPageAction,
  pageSnapshot,
  resetPageContextForTest,
  setFramePage,
} from "~/app/assistant/context.ts";

beforeEach(() => {
  sessionStorage.clear();
  clearSavedRevision();
});

afterEach(() => {
  cleanup();
  resetPageContextForTest();
  vi.unstubAllGlobals();
  sessionStorage.clear();
  clearSavedRevision();
  location.hash = "#/";
});

const isWrite = (r: SentRequest) =>
  r.path === "/config" && r.method === "PUT" && !r.query.has("dry_run");

/**
 * Fills the create form and starts the company from a template. The form
 * offers the operator's own seat ticked, named "Founder": `seat: false`
 * unticks it, and `seat: true` gives it a contact identity besides.
 */
async function startCompany(options: { template?: string; seat?: boolean } = {}) {
  fireEvent.change(await screen.findByLabelText("Company name"), {
    target: { value: "Nimbus" },
  });
  if (options.template) {
    fireEvent.click(screen.getByRole("radio", { name: options.template }));
  }
  if (options.seat === false) {
    fireEvent.click(screen.getByRole("checkbox", { name: /^Add a seat for yourself/ }));
  }
  if (options.seat === true) {
    fireEvent.change(screen.getByLabelText(/^Slack member ID/), {
      target: { value: "U0FOUNDER" },
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
  const sent = check.body as CompanyDocument;
  expect(sent.name).toBe("Nimbus");
  // THE PRINCIPLES ARE NOT ASKED HERE: Company › Principles writes them, once
  // the company exists, and the next steps below open it.
  expect(screen.queryByLabelText("Mission")).toBeNull();
  expect(sent).not.toHaveProperty("mission");
  // The operator's own seat, with the one identity they gave, and the
  // template's neutral titles below it.
  // Every node carries its identity from the start, minted from its name.
  expect(sent.roles?.[0]).toMatchObject({
    name: "Founder",
    handle: "founder",
    kind: "human",
    contact: { slack_user_id: "U0FOUNDER" },
    manages: ["chief-executive"],
  });
  expect(sent.roles?.[1]).toMatchObject({ name: "Chief Executive", handle: "chief-executive" });
  expect(sent.units?.map((u) => u.name)).toEqual(["Engineering", "Product", "Marketing"]);
  // And the whole start is one operation: undone, the form is back.
  fireEvent.keyDown(document.body, { key: "z", code: "KeyZ", ctrlKey: true });
  expect(await screen.findByRole("button", { name: "Start the company" })).toBeDefined();
});

// EVERY PERSON HOLDS A HUMAN SEAT, so a company started with none is one
// nobody can join until somebody adds one: the operator's own seat is
// offered ticked and named, and leaving it out is a choice made by unticking.
test("the operator's own seat is offered from the start, ticked and named", async () => {
  const engine = new Engine(null);
  mountBuilder({ engine });
  const own = await screen.findByRole("checkbox", { name: /^Add a seat for yourself/ });
  expect((own as HTMLInputElement).checked).toBe(true);
  expect((screen.getByLabelText("Your seat's name") as HTMLInputElement).value).toBe("Founder");
  screen.getByText(/invite or create yourself on it from the org chart/);
  await startCompany({});
  await waitFor(() => expect(engine.checks().length).toBeGreaterThan(1));
  expect((engine.checks().at(-1)!.body as CompanyDocument).roles?.[0]).toMatchObject({
    name: "Founder",
    kind: "human",
  });
});

test("a company started with the own seat unticked has no human seat", async () => {
  const engine = new Engine(null);
  mountBuilder({ engine });
  await startCompany({ seat: false });
  await waitFor(() => expect(engine.checks().length).toBeGreaterThan(1));
  const roles = (engine.checks().at(-1)!.body as CompanyDocument).roles ?? [];
  expect(roles.length).toBeGreaterThan(0);
  expect(roles.some((r) => r.kind === "human")).toBe(false);
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

// A COMPANY STILL BEING CREATED HAS NO PRINCIPLES PAGE: it is renamed in the
// builder, in place, rather than sent to a page that has nothing to show.
test("the company a draft is creating is renamed in the builder", async () => {
  const engine = new Engine(null);
  mountBuilder({ engine });
  await startCompany({});
  await screen.findByText("No problems");
  const before = location.hash;
  fireEvent.click(screen.getByRole("button", { name: "Open the company's name form" }));
  const dialog = await screen.findByRole("dialog", { name: /Nimbus/ });
  expect(within(dialog).getByLabelText(/^Company name/)).toBeDefined();
  expect(location.hash).toBe(before);
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

  // What is left: the principles, on their own page in edit, and the two
  // steps the builder cannot take, with the terminal steps beside the model's
  // own screen: exactly the declaration the engine walks from a company
  // only the store holds (`contract/providersteps.ts`), so a card drawn from
  // anything else is a card no gate has run.
  expect(await screen.findByText("The company is created")).toBeDefined();
  expect(screen.getByRole("link", { name: "Open Principles" }).getAttribute("href")).toBe(
    "#/company/principles?edit=mission",
  );
  expect(screen.getByRole("link", { name: "Open Integrations" }).getAttribute("href")).toBe(
    "#/company/integrations",
  );
  // The model is added on Settings › Models, signed in and tested there; the
  // terminal steps stay folded beside it for a reader at the node's host.
  expect(screen.getByRole("link", { name: "Add a model" }).getAttribute("href")).toBe(
    "#/settings/models?flow=add",
  );
  fireEvent.click(screen.getByRole("button", { name: "Or from a terminal" }));
  expect(screen.getByText(/crewlet config export > company\.yaml/).textContent).toBe(
    PROVIDER_STEPS,
  );
  expect(screen.getByText(/Agents can’t work until one is added/)).toBeDefined();
  // The first revision has no parent to differ from, so the strip opens the
  // configuration itself rather than an empty diff.
  expect(screen.queryByRole("link", { name: "View changes" })).toBeNull();
  expect(screen.getByRole("link", { name: "View the configuration" }).getAttribute("href")).toBe(
    "#/settings/config",
  );
  // ONE TELLING, NOT TWO: the card says the agents wait and gives the steps,
  // so the builder's own caution about the missing model holds back while it
  // is up, and says it once the card is dismissed.
  expect(screen.queryByText(/No model provider is set/)).toBeNull();
  const card = screen.getByText("The company is created").closest("section") as HTMLElement;
  fireEvent.click(within(card).getByRole("button", { name: "Dismiss" }));
  expect(await screen.findByText(/No model provider is set/)).toBeDefined();
});

// A MANAGED DOCUMENT'S MODEL IS ADDED WHERE IT IS WRITTEN. A writer the engine
// names can create the company here, and the card then says the model comes
// from the managing system rather than giving terminal steps whose offline
// import the document refuses.
test("a create on a managed document says the model is added there, with no steps", async () => {
  const engine = new Engine(null);
  mountBuilder({
    engine,
    viewer: () => ({ ...EDITOR, config_writer: true, config_managed_by: ["gitops"] }),
  });
  await startCompany({});
  await screen.findByText("No problems");
  fireEvent.click(screen.getByRole("button", { name: "Review and save" }));
  const dialog = await screen.findByRole("dialog", { name: "Review and create the company" });
  fireEvent.click(within(dialog).getByRole("button", { name: "Create the company" }));

  const card = (await screen.findByText("The company is created")).closest(
    "section",
  ) as HTMLElement;
  expect(card.textContent).toContain(
    "Agents can’t work until one is added. The company’s configuration is managed by gitops, so the model is added there.",
  );
  expect(card.textContent).not.toContain("crewlet config");
  expect(card.textContent).not.toContain("crewlet llm");
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
  // The tab already holds the snapshot of a node running no company: the
  // push below REPLACES it, which is what an apply looks like.
  const { store } = mountBuilder({ engine, org: {} });
  await startCompany({});
  await screen.findByText("No problems");
  engine.document = company();
  engine.revision = "r1";
  act(() => store.applyOrg(withDerived({ name: "Acme", roles: [], units: [] })));
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

// KEEPING THE DRAFT IS NOT A DEAD END. It can never be saved, and the builder is
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
    screen.getByText("A company was created meanwhile, so this draft can’t be saved."),
  ).toBeDefined();
  fireEvent.click(screen.getByRole("button", { name: "Discard it and open the company" }));
  expect(await screen.findByText("CEO")).toBeDefined();
  await waitFor(() => expect(engine.checks().at(-1)!.method).toBe("PATCH"));
  expect(engine.requests.filter(isWrite)).toHaveLength(0);
});

test("the create form is the assistant's to read and fill in, and the draft it starts is the builder's", async () => {
  setFramePage({
    screen: "company-org-edit",
    title: "Org chart › Edit org",
    path: ["company", "org", "edit"],
    params: {},
    query: {},
  });
  const engine = new Engine(null);
  mountBuilder({ engine });
  await screen.findByLabelText("Company name");
  const open = () => pageSnapshot()!.context;
  const field = (path: string) => open().draft?.fields.find((f) => f.path === path);
  await waitFor(() => expect(open().draft).toMatchObject({ kind: "client", step: "create" }));
  expect(field("template")).toMatchObject({ value: "new_company", editable: true });
  // A SHAPE THE FORM DOES NOT OFFER IS REFUSED in the form's own words.
  expect(
    applyPageAction({
      correlation_id: "c0",
      action: "draft.set",
      payload: { values: { template: "startup" } },
      source_revision: open().source_revision,
    }),
  ).toMatchObject({ outcome: "invalid", reason: expect.stringMatching(/^Choose the shape/) });
  act(() => {
    applyPageAction({
      correlation_id: "c1",
      action: "draft.set",
      payload: { values: { name: "Nimbus", template: "empty", own_seat: "no" } },
      source_revision: open().source_revision,
    });
  });
  expect(((await screen.findByLabelText("Company name")) as HTMLInputElement).value).toBe("Nimbus");
  expect(screen.getByRole("radio", { name: "Start empty" }).getAttribute("aria-checked")).toBe(
    "true",
  );
  // NOTHING IS CREATED until the person starts it; then the draft open is
  // the builder's, with the template as its one change.
  expect(engine.requests.filter(isWrite)).toHaveLength(0);
  fireEvent.click(screen.getByRole("button", { name: "Start the company" }));
  await waitFor(() => expect(field("changes")?.value).not.toBe(""));
  expect(open().draft).toMatchObject({ kind: "client", step: "visualization" });
  expect(open().draft!.help).toMatch(/org\.draft\.apply_ops/);
});
