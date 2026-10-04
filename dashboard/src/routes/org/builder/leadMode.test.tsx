/**
 * A lead's draft: a person without `config:write` who leads a unit edits that
 * unit in the builder, read and written at its own address.
 *
 * What these protect: a lead's builder never reads the company document,
 * which they may not; it opens the unit they lead (the one an address names,
 * else the first), checks and saves it whole at `/config/units/{key}` under
 * the revision it read, and places the engine's answer — about the WHOLE
 * company — on the unit's own nodes; what reaches outside the unit is refused
 * before the engine is asked, with the reason; and a lead of several units
 * opens them one draft at a time.
 */

import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import type { CompanyDocument, OrgProjection } from "~/protocol/index.ts";
import { Engine, json, mountBuilder, type SentRequest } from "./testkit.tsx";
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
  const { units } = fixtureDerived(document);
  return {
    name: document.name,
    roles: [{ name: "CEO", handle: "ceo" }],
    units: (document.units ?? []).map((u) => ({ id: u.id!, name: u.name })),
    derived: { seats: [], units: (units ?? []).map(({ path: _path, ...unit }) => unit) },
  };
}

/** Dev, signed in, bound to the seat that leads Engineering and Ops, holding no configuration grant. */
const LEAD = { login: "dev.person", owner: "dev", handle: "dev", grants: ["state:read"] };

const isDryRun = (r: SentRequest) => r.query.get("dry_run") === "true";
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
    // An unchanged unit is read rather than checked, since storing it as it
    // was is no lead's write.
    expect(await screen.findByText("No problems")).toBeDefined();
    expect(engine.checks()).toHaveLength(0);
    // The unit is read once to open it and once as its check: the transport
    // its scope brings starts the check, and nothing asks for it again.
    expect(engine.sent("GET", "/config/units/engineering")).toHaveLength(2);
    expect(screen.getByRole("button", { name: "Editing Engineering as its lead" })).toBeDefined();
    // The draft holds no settings, so it says nothing about the providers.
    expect(screen.queryByText(/No model provider is configured/)).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Edit Dev" }));
    await waitFor(() => expect(engine.checks()).toHaveLength(1));
    const check = engine.checks()[0]!;
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
    expect(engine.checks()).toHaveLength(0);

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
      if (!isDryRun(r)) return null;
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
      isDryRun(r)
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
    await waitFor(() => expect(engine.checks()).toHaveLength(1));
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
    fireEvent.click(within(dialog).getByRole("button", { name: "Update my draft" }));
    await waitFor(() => expect(engine.checks().at(-1)!.headers["If-Match"]).toBe('"r2"'));
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
