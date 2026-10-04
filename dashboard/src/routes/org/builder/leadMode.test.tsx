/**
 * A lead's draft: a person without `config:write` who leads a unit edits that
 * unit in the builder, read and written at its own address.
 *
 * What these protect: a lead's builder never reads the company document,
 * which they may not; it opens the unit they lead (the one an address names,
 * else the first), checks it — unchanged too, so the engine's derivation of
 * the unit is the draft's from the start — and saves it whole at
 * `/config/units/{key}` under the revision it read, and places the engine's
 * answer — about the WHOLE company — on the unit's own nodes; what reaches
 * outside the unit is refused
 * before the engine is asked, with the reason; and a lead of several units
 * opens them one draft at a time.
 */

import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import type { CompanyDocument, OrgProjection } from "~/protocol/index.ts";
import { Engine, json, mountBuilder, type SentRequest } from "./testkit.tsx";
import { DRAFT_STORAGE_KEY } from "./model/persistence.ts";
import { fixtureDerived } from "./model/testkit.ts";

beforeEach(() => {
  localStorage.clear();
  sessionStorage.clear();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  localStorage.clear();
  sessionStorage.clear();
  location.hash = "#/";
});

/** Engineering and Ops are Dev's to lead; Sales and the CEO are not. */
function leadCompany(): CompanyDocument {
  return {
    name: "Acme",
    roles: [{ name: "CEO", handle: "ceo", goal: "Lead" }],
    units: [
      { name: "Sales", id: "sales", roles: [{ name: "Seller", handle: "seller" }] },
      {
        name: "Engineering",
        id: "engineering",
        lead: "dev",
        roles: [
          { name: "Dev", handle: "dev", goal: "Build" },
          { name: "QA", handle: "qa", goal: "Test" },
        ],
      },
      { name: "Ops", id: "ops", lead: "dev", roles: [{ name: "SRE", handle: "sre", goal: "Run" }] },
    ],
  };
}

/** The org push as the engine derives it from [leadCompany]. */
function leadOrg(): OrgProjection {
  const document = leadCompany();
  const { seats, units } = fixtureDerived(document);
  return {
    name: document.name,
    roles: [{ name: "CEO", handle: "ceo" }],
    units: (document.units ?? []).map((u) => ({ id: u.id!, name: u.name })),
    derived: {
      seats: (seats ?? []).map(({ path: _path, unit_path: _unit, ...seat }) => seat),
      units: (units ?? []).map(({ path: _path, ...unit }) => unit),
    },
  };
}

/** Dev, signed in, bound to the seat that leads Engineering and Ops, holding no configuration grant. */
const LEAD = { login: "dev.person", owner: "dev", handle: "dev", grants: ["state:read"] };

const isDryRun = (r: SentRequest) => r.query.get("dry_run") === "true";
/** The goal a dry run of Engineering sends for its seat at `index`. */
const goalSent = (r: SentRequest, index: number) =>
  (r.body as { roles?: { goal?: string }[] }).roles?.[index]?.goal;
const unitWrites = (engine: Engine, key: string) =>
  engine.requests.filter((r) => r.method === "PUT" && r.path === `/config/units/${key}`);

function mountLead(engine: Engine, hash?: string) {
  return mountBuilder({
    engine,
    org: leadOrg(),
    viewer: () => LEAD,
    ...(hash ? { hash } : {}),
  });
}

describe("a lead's draft", () => {
  test("opens the first unit they lead, read and checked at its own address", async () => {
    const engine = new Engine(leadCompany());
    mountLead(engine);
    expect(await screen.findByText("Dev")).toBeDefined();
    expect(screen.queryByText("CEO")).toBeNull();
    expect(screen.queryByText("Seller")).toBeNull();
    expect(screen.queryByText("SRE")).toBeNull();
    // The company document is never asked for: a lead may not read it.
    expect(engine.sent("GET", "/config")).toHaveLength(0);
    expect(engine.sent("GET", "/config/units/engineering").length).toBeGreaterThan(0);
    // An unchanged unit is checked at its own address as well — storing it
    // as it was is no lead's write, but checking it is — so the engine's
    // derivation of it describes the draft before anything is edited.
    expect(await screen.findByText("No problems")).toBeDefined();
    expect(engine.checks()).toHaveLength(1);
    expect(engine.checks()[0]).toMatchObject({
      method: "PUT",
      path: "/config/units/engineering",
    });
    expect(screen.getByTestId("derivation").textContent).toBe("described");
    // The unit is read once to open it, and checked once: the transport its
    // scope brings starts the check, and nothing asks for it again.
    expect(engine.sent("GET", "/config/units/engineering")).toHaveLength(1);
    expect(screen.getByRole("button", { name: "Editing Engineering as its lead" })).toBeDefined();
    // The draft holds no settings, so it says nothing about the providers.
    expect(screen.queryByText(/No model provider is configured/)).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Edit Dev" }));
    await waitFor(() => expect(engine.checks()).toHaveLength(2));
    const check = engine.checks()[1]!;
    expect(check.method).toBe("PUT");
    expect(check.path).toBe("/config/units/engineering");
    expect(check.headers["If-Match"]).toBe('"r1"');
    const sent = check.body as { id: string; roles: { goal: string }[] };
    expect(sent.id).toBe("engineering");
    expect(sent.roles[0]!.goal).toBe("Build and more");
    expect(await screen.findByText("No problems")).toBeDefined();
  });

  test("opens the unit holding the seat the address names", async () => {
    const engine = new Engine(leadCompany());
    mountLead(engine, "#/agents/edit?view=visualization&seat=sre");
    expect(await screen.findByText("SRE")).toBeDefined();
    expect(screen.queryByText("Dev")).toBeNull();
    expect(engine.sent("GET", "/config/units/engineering")).toHaveLength(0);
  });

  test("refuses what reaches outside the unit before the engine is asked", async () => {
    const engine = new Engine(leadCompany());
    mountLead(engine);
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Rename the company" }));
    expect(
      await screen.findByText(
        "The company's charter and settings are not part of Engineering: changing them takes the config:write grant.",
      ),
    ).toBeDefined();
    fireEvent.click(screen.getByRole("button", { name: "Add an analyst" }));
    expect(
      await screen.findByText(
        "Only Engineering and what is inside it can be changed here: the company's top level takes the config:write grant.",
      ),
    ).toBeDefined();
    // The unit as it stands is the one check sent.
    expect(engine.checks()).toHaveLength(1);

    // The toolbar's Add at the company's top level is drawn unavailable.
    fireEvent.click(screen.getByRole("button", { name: "Add" }));
    const menu = await screen.findByRole("menu", { name: "Add to the organization" });
    expect(
      within(menu)
        .getByRole("menuitem", { name: /^Add unit/ })
        .getAttribute("aria-disabled"),
    ).toBe("true");
  });

  test("places the engine's answer about the whole company on the unit's own nodes", async () => {
    const engine = new Engine(leadCompany());
    engine.script = (r) => {
      if (!isDryRun(r) || goalSent(r, 0) === "Build") return null;
      const result = engine.result(r);
      return json(
        {
          error: "validation_error",
          detail: "two problems",
          derived: fixtureDerived(result),
          problems: [
            {
              // QA, as the company document places it: Engineering is its
              // second unit, and the lead's draft holds it first.
              path: "units[1].roles[1].goal",
              segments: ["units", 1, "roles", 1, "goal"],
              kind: "invalid",
              message: "units[1].roles[1].goal: too long",
            },
            {
              // Sales' seller, which the draft does not hold: said about the
              // whole draft rather than put on whichever node sits there.
              path: "units[0].roles[0].goal",
              segments: ["units", 0, "roles", 0, "goal"],
              kind: "invalid",
              message: "units[0].roles[0].goal: missing",
            },
          ],
        },
        400,
      );
    };
    mountLead(engine);
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Edit Dev" }));
    await waitFor(() => expect(screen.getByTestId("problems QA").textContent).toBe("1"));
    expect(screen.getByTestId("problems Dev").textContent).toBe("0");
    expect(await screen.findByText("units[0].roles[0].goal: missing")).toBeDefined();
  });

  test("names what the engine refused a lead, on the part it refused", async () => {
    const engine = new Engine(leadCompany());
    engine.script = (r) =>
      isDryRun(r) && goalSent(r, 1) !== "Test"
        ? json(
            {
              error: "unauthorized",
              refused: [
                {
                  kind: "seat",
                  id: "qa",
                  op: "changed",
                  side: "after",
                  place: "sales",
                  why: "manages",
                  value: "seller",
                  reason: "not_lead",
                },
              ],
            },
            403,
          )
        : null;
    mountLead(engine);
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Edit QA" }));
    await waitFor(() => expect(screen.getByTestId("problems QA").textContent).toBe("1"));
    // A problem with the draft, not a refusal of the person: editing goes on.
    expect(screen.getByText("editable")).toBeDefined();
  });

  test("saves the unit whole under the revision it read", async () => {
    const engine = new Engine(leadCompany());
    mountLead(engine);
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Edit Dev" }));
    await waitFor(() => expect(engine.checks()).toHaveLength(2));
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Review and save" }));
    const dialog = await screen.findByRole("dialog", { name: "Review and save" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() =>
      expect(unitWrites(engine, "engineering").filter((r) => !isDryRun(r))).toHaveLength(1),
    );
    const write = unitWrites(engine, "engineering").find((r) => !isDryRun(r))!;
    expect(write.headers["If-Match"]).toBe('"r1"');
    const { _summary, ...unit } = write.body as { _summary: string; roles: { goal: string }[] };
    expect(_summary).toMatch(/\(write [0-9a-f]{32}\)$/);
    expect(unit.roles[0]!.goal).toBe("Build and more");
    // The rest of the company is untouched, and never sent.
    expect(engine.document!.units![0]!.roles![0]!.name).toBe("Seller");
    expect(engine.sent("PATCH")).toHaveLength(0);
    // What the strip offers that reads the company document is not offered.
    expect(await screen.findByText(/Saved revision/)).toBeDefined();
    expect(screen.queryByRole("link", { name: "View changes" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Copy as YAML" })).toBeNull();
  });

  test("a colleague's save is updated onto from the unit, never from the company's history", async () => {
    const engine = new Engine(leadCompany());
    mountLead(engine);
    await screen.findByText("No problems");
    const next = leadCompany();
    next.units![1]!.roles![1]!.goal = "Test well";
    engine.document = next;
    engine.revision = "r2";
    fireEvent.click(screen.getByRole("button", { name: "Edit Dev" }));
    expect(await screen.findByText("The configuration changed")).toBeDefined();
    // What changed is a diff of the company, which a lead may not read.
    expect(screen.queryByRole("link", { name: "Show what changed" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Update my draft" }));
    const dialog = await screen.findByRole("dialog", { name: "Update my draft and review" });
    // The newer unit was described by its own dry run before the draft was
    // updated onto it, as a whole company's is.
    expect(
      engine.checks().some((c) => c.headers["If-Match"] === '"r2"' && goalSent(c, 0) === "Build"),
    ).toBe(true);
    fireEvent.click(within(dialog).getByRole("button", { name: "Update my draft" }));
    await waitFor(() => {
      const last = engine.checks().at(-1)!;
      expect(last.headers["If-Match"]).toBe('"r2"');
      expect(goalSent(last, 0)).toBe("Build and more");
    });
    const unit = engine.checks().at(-1)!.body as { roles: { goal: string }[] };
    expect(unit.roles.map((r) => r.goal)).toEqual(["Build and more", "Test well"]);
    expect(engine.requests.some((r) => r.path.startsWith("/config/revisions/"))).toBe(false);
    expect(engine.sent("GET", "/config")).toHaveLength(0);
  });

  test("a lead of two units opens the other only while the draft has nothing to lose", async () => {
    const engine = new Engine(leadCompany());
    mountLead(engine);
    await screen.findByText("No problems");

    fireEvent.click(screen.getByRole("button", { name: "Edit Dev" }));
    fireEvent.click(screen.getByRole("button", { name: "Editing Engineering as its lead" }));
    let menu = await screen.findByRole("menu", { name: "Units you lead" });
    expect(
      within(menu).getByRole("menuitemradio", { name: /Ops/ }).getAttribute("aria-disabled"),
    ).toBe("true");
    act(() => fireEvent.keyDown(menu, { key: "Escape" }));

    // With the change undone there is nothing to lose, and Ops opens.
    fireEvent.click(screen.getByRole("button", { name: "Undo" }));
    fireEvent.click(screen.getByRole("button", { name: "Editing Engineering as its lead" }));
    menu = await screen.findByRole("menu", { name: "Units you lead" });
    fireEvent.click(within(menu).getByRole("menuitemradio", { name: /Ops/ }));
    expect(await screen.findByText("SRE")).toBeDefined();
    expect(screen.queryByText("Dev")).toBeNull();
    expect(screen.getByRole("button", { name: "Editing Ops as its lead" })).toBeDefined();
  });

  // A NEW SEAT AVOIDS EVERY HANDLE IN THE COMPANY, not only the unit's: the
  // engine counts Sales' seller as well, and offered `seller` the lead would
  // have learned of it only as a refusal at the company's top level.
  test("a seat a lead adds is offered a handle nobody in the company holds", async () => {
    const engine = new Engine(leadCompany());
    mountLead(engine, "#/agents/edit?view=table");
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Select Engineering" }));
    fireEvent.click(await screen.findByRole("button", { name: "Engineering" }));
    const menu = await screen.findByRole("menu", { name: "Actions for Engineering" });
    fireEvent.click(within(menu).getByRole("menuitem", { name: "Add agent seat" }));
    const dialog = await screen.findByRole("dialog", { name: "Add to Engineering" });
    fireEvent.change(within(dialog).getByLabelText("Name"), { target: { value: "Seller" } });
    expect((within(dialog).getByLabelText("Handle") as HTMLInputElement).value).toBe("seller-2");
    fireEvent.change(within(dialog).getByLabelText("Handle"), { target: { value: "seller" } });
    expect(within(dialog).getByText("seller already names a seat or a unit.")).toBeDefined();
  });

  // A RELOAD REOPENS THE UNIT THE KEPT DRAFT WAS MADE OF. Which unit a lead
  // chose is in no address, so the second unit's work would otherwise meet the
  // first unit on reload and be discarded as another unit's.
  test("a reload reopens the unit a kept draft was made of, and offers it", async () => {
    const engine = new Engine(leadCompany());
    mountLead(engine);
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Editing Engineering as its lead" }));
    const menu = await screen.findByRole("menu", { name: "Units you lead" });
    fireEvent.click(within(menu).getByRole("menuitemradio", { name: /Ops/ }));
    await screen.findByText("SRE");
    fireEvent.click(screen.getByRole("button", { name: "Edit SRE" }));
    await waitFor(() =>
      expect(sessionStorage.getItem(DRAFT_STORAGE_KEY)).toContain('"scope":"ops"'),
    );

    // A reload starts the page afresh, which no longer knows the draft as the
    // one it kept a moment ago, so the draft is offered rather than restored.
    cleanup();
    const kept = JSON.parse(sessionStorage.getItem(DRAFT_STORAGE_KEY)!) as { savedAt: number };
    sessionStorage.setItem(
      DRAFT_STORAGE_KEY,
      JSON.stringify({ ...kept, savedAt: kept.savedAt - 1 }),
    );
    mountLead(engine);
    expect(await screen.findByText(/This tab kept a draft with 1 change/)).toBeDefined();
    expect(screen.getByText("SRE")).toBeDefined();
    expect(screen.queryByText("Dev")).toBeNull();
    expect(sessionStorage.getItem(DRAFT_STORAGE_KEY)).toContain('"scope":"ops"');
  });

  // WHAT A LEAD LEADS IS KNOWN FROM THE SNAPSHOT, which follows the socket
  // opening. Read in between, the builder had no scope, read the company
  // document as a whole-company draft, and its refusal forgot the draft the
  // lead had kept through the reload.
  test("a reload waits for the org snapshot before reading, and keeps the draft", async () => {
    const engine = new Engine(leadCompany());
    mountLead(engine);
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Edit Dev" }));
    await waitFor(() =>
      expect(sessionStorage.getItem(DRAFT_STORAGE_KEY)).toContain('"scope":"engineering"'),
    );
    cleanup();
    const kept = JSON.parse(sessionStorage.getItem(DRAFT_STORAGE_KEY)!) as { savedAt: number };
    sessionStorage.setItem(
      DRAFT_STORAGE_KEY,
      JSON.stringify({ ...kept, savedAt: kept.savedAt - 1 }),
    );

    // The socket is open and its snapshot has not arrived.
    engine.script = (r) => (r.path === "/config" ? json({ error: "unauthorized" }, 403) : null);
    const { store } = mountBuilder({ engine, org: null, viewer: () => LEAD });
    await new Promise((r) => setTimeout(r, 50));
    expect(engine.sent("GET", "/config")).toHaveLength(0);
    expect(sessionStorage.getItem(DRAFT_STORAGE_KEY)).toContain('"scope":"engineering"');

    act(() => store.applySnapshot({ org: leadOrg() }));
    expect(await screen.findByText(/This tab kept a draft with 1 change/)).toBeDefined();
    expect(engine.sent("GET", "/config")).toHaveLength(0);
  });

  test("a reader who leads nothing is told the grant, and that a lead edits here", async () => {
    const engine = new Engine(leadCompany());
    engine.script = (r) => (r.path === "/config" ? json({ error: "unauthorized" }, 403) : null);
    mountBuilder({
      engine,
      org: leadOrg(),
      viewer: () => ({ ...LEAD, owner: "seller", handle: "seller" }),
    });
    expect(await screen.findByText("Reading the configuration needs config:read.")).toBeDefined();
    expect(
      screen.getByText(
        "The configuration is guarded, reads included. A unit's lead edits the units they lead here without it.",
      ),
    ).toBeDefined();
    expect(engine.sent("GET", "/config/units/engineering")).toHaveLength(0);
  });
});
