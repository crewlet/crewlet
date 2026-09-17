/**
 * A newer revision found by a check is offered as an update of the draft,
 * never replayed over silently: what applies is replayed onto the revision
 * the node serves, and a value somebody else changed waits for a choice.
 */

import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import type { CompanyDocument } from "~/protocol/index.ts";
import { COMPANY_KEY } from "./model/keys.ts";
import type { Conflict } from "./model/operations.ts";
import { company, Engine, json, mountBuilder } from "./testkit.tsx";
import { conflictCells } from "./UpdateDraftDialog.tsx";

beforeEach(() => {
  sessionStorage.clear();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  sessionStorage.clear();
  location.hash = "#/";
});

/** The company as a colleague saved it: `change` applied to the fixture. */
function upstream(change: (doc: CompanyDocument) => void): CompanyDocument {
  const doc = company();
  change(doc);
  return doc;
}

/** Opens the builder, lets a colleague save `next` as r2, and edits the CEO. */
async function editAfterUpstreamSave(next: CompanyDocument) {
  const engine = new Engine(company());
  mountBuilder({ engine });
  await screen.findByText("No problems");
  engine.document = next;
  engine.revision = "r2";
  fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
  expect(await screen.findByText("The configuration changed")).toBeDefined();
  return engine;
}

test("an edit that still applies is replayed onto the newer revision", async () => {
  const engine = await editAfterUpstreamSave(
    upstream((doc) => {
      doc.roles![1]!.goal = "Design things";
    }),
  );
  // The banner links to what changed between the draft's base and now: the
  // newer revision against the base, read forwards.
  expect(screen.getByRole("link", { name: "Show what changed" }).getAttribute("href")).toBe(
    "#/admin/config?lens=diff&revision=r2&against=r1",
  );
  fireEvent.click(screen.getByRole("button", { name: "Update my draft" }));
  const dialog = await screen.findByRole("dialog", { name: "Update my draft and review" });
  expect(within(dialog).getByText("Every change still applies.")).toBeDefined();
  fireEvent.click(within(dialog).getByRole("button", { name: "Update my draft" }));

  await waitFor(() => {
    const last = engine.checks().at(-1)!;
    expect(last.headers["If-Match"]).toBe('"r2"');
  });
  const patch = engine.checks().at(-1)!.body as { roles: { goal: string }[] };
  expect(patch.roles[0]!.goal).toBe("Lead and more");
  // The colleague's change is part of the base now, not overwritten.
  expect(patch.roles[1]!.goal).toBe("Design things");
  expect(await screen.findByText("No problems")).toBeDefined();
});

test("a value somebody else changed waits for a choice, and keeping theirs drops mine", async () => {
  const engine = await editAfterUpstreamSave(
    upstream((doc) => {
      doc.roles![0]!.goal = "Lead well";
    }),
  );
  fireEvent.click(screen.getByRole("button", { name: "Update my draft" }));
  const dialog = await screen.findByRole("dialog", { name: "Update my draft and review" });
  const row = within(dialog).getByRole("row", { name: /goal/ });
  expect(within(row).getByText("Lead")).toBeDefined();
  expect(within(row).getByText("Lead well")).toBeDefined();
  expect(within(row).getByText("Lead and more")).toBeDefined();
  const confirm = within(dialog).getByRole("button", { name: "Update my draft" });
  expect((confirm as HTMLButtonElement).disabled).toBe(true);

  fireEvent.click(within(dialog).getByRole("radio", { name: "Keep theirs" }));
  expect((confirm as HTMLButtonElement).disabled).toBe(false);
  fireEvent.click(confirm);

  await waitFor(() => expect(engine.checks().at(-1)!.headers["If-Match"]).toBe('"r2"'));
  expect(engine.checks().at(-1)!.body).toEqual({});
});

test("a node still serving the draft's base is not updated onto", async () => {
  const engine = await editAfterUpstreamSave(
    upstream((doc) => {
      doc.roles![1]!.goal = "Design things";
    }),
  );
  // This node answers reads with the old revision while the conflict named r2.
  engine.script = (r) =>
    r.method === "GET" && r.path === "/config" ? json(company(), 200, { ETag: '"r1"' }) : null;
  fireEvent.click(screen.getByRole("button", { name: "Update my draft" }));
  expect(
    await screen.findByText(
      "This node has not caught up with the newer revision yet. Try again in a moment.",
    ),
  ).toBeDefined();
  expect(screen.queryByRole("dialog")).toBeNull();
});

test("a draft of a configuration that is no longer active offers to discard and reload", async () => {
  const engine = new Engine(company());
  mountBuilder({ engine });
  await screen.findByText("No problems");
  engine.script = (r) =>
    r.query.get("dry_run") === "true" ? json({ error: "no_active_revision" }, 412) : null;
  fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
  expect(
    await screen.findByText(
      "The configuration this draft edits is no longer active on this engine, so the draft cannot be saved.",
    ),
  ).toBeDefined();
  engine.script = () => null;
  const reads = engine.sent("GET").length;
  fireEvent.click(screen.getByRole("button", { name: "Discard and reload" }));
  await waitFor(() => expect(engine.sent("GET").length).toBe(reads + 1));
  expect(await screen.findByText("No problems")).toBeDefined();
  expect(engine.checks().at(-1)!.body).toEqual({});
});

// A CONFLICT IS SHOWN AS A PERSON READS IT. The builder's own structures
// (node keys, placements, a removed node, a kind change's stripped fields)
// were printed as JSON, which put internal keys and credential masks in a
// table an operator decides from.
describe("a conflict's values", () => {
  const names: Record<string, string> = {
    "seat:dev": "Dev",
    "seat:qa": "QA",
    "unit:Engineering": "Engineering",
    "unit:Sales": "Sales",
  };
  const nameOf = (key: string) => names[key] ?? null;
  const cells = (conflict: Conflict) => conflictCells(conflict, nameOf);

  test("a field reads as its value, and a credential's mask as a literal that is set", () => {
    expect(
      cells({
        subject: "mcp_env.TOKEN",
        base: "__redacted__",
        theirs: "${TOKEN}",
        mine: undefined,
      }),
    ).toEqual({ base: "A literal value is set (hidden)", theirs: "${TOKEN}", mine: "Not set" });
    expect(cells({ subject: "availability", base: { days: "Mon", hours: 8 }, theirs: {} })).toEqual(
      { base: "days: Mon; hours: 8", theirs: "None", mine: "Not set" },
    );
  });

  test("a position names the nodes it is between, never their keys", () => {
    expect(
      cells({
        subject: "position",
        shape: "sibling",
        base: "seat:dev",
        theirs: null,
        mine: "seat:dev",
      }),
    ).toEqual({ base: "After Dev", theirs: "Not there", mine: "After Dev" });
    expect(
      cells({
        subject: "where it sits",
        shape: "parent",
        base: "unit:Engineering",
        theirs: COMPANY_KEY,
        mine: "unit:Sales",
      }),
    ).toEqual({ base: "Engineering", theirs: "The top of the organization", mine: "Sales" });
    expect(
      cells({
        subject: "position",
        shape: "placement",
        base: { parent: "unit:Engineering", after: null },
        theirs: { parent: "unit:Engineering", after: "seat:qa" },
        mine: { parent: "unit:Engineering", after: "new:gone" },
      }),
    ).toEqual({
      base: "Engineering, first",
      theirs: "Engineering, after QA",
      mine: "Engineering, after a node no longer in the organization",
    });
  });

  test("a removed node reads as the fields that changed, and stripped fields by name only", () => {
    const removal = cells({
      subject: "the whole seat",
      shape: "snapshot",
      base: { name: "Dev", goal: "Build", mcp_env: { tracker: { TOKEN: "__redacted__" } } },
      theirs: { name: "Dev", goal: "Ship", mcp_env: { tracker: { TOKEN: "__redacted__" } } },
    });
    expect(removal).toEqual({ base: "As it was", theirs: "Changed: goal", mine: "Removed" });

    const stripped = cells({
      subject: "fields the new kind removes",
      shape: "fields",
      base: [
        { path: ["mcp_env"], before: { tracker: { TOKEN: "__redacted__" } } },
        { path: ["integrations", "jira"], before: { project: "OPS" } },
      ],
      theirs: [
        { path: ["mcp_env"], before: { tracker: { TOKEN: "__redacted__" } } },
        { path: ["integrations", "jira"], before: { project: "SUP" } },
        { path: ["email"], before: "dev@example.com" },
      ],
    });
    expect(stripped).toEqual({
      base: "mcp_env, integrations.jira",
      theirs: "mcp_env, integrations.jira (changed), email (added)",
      mine: "Removed",
    });
    const shown = JSON.stringify([removal, stripped]);
    expect(shown).not.toContain("__redacted__");
    expect(shown).not.toContain("OPS");

    expect(
      cells({
        subject: "seats placed in it by reference",
        shape: "placed",
        base: [{ key: "seat:dev", json: { name: "Dev" } }],
        theirs: [],
      }),
    ).toEqual({ base: "Dev", theirs: "None", mine: "None" });
  });
});

test("a removal somebody else edited first names what they changed", async () => {
  const engine = new Engine(company());
  const { store } = mountBuilder({ engine });
  await screen.findByText("No problems");
  fireEvent.click(screen.getByRole("button", { name: "Remove Designer" }));
  await waitFor(() => expect(engine.checks()).toHaveLength(2));
  await screen.findByText("No problems");
  engine.document = upstream((doc) => {
    doc.roles![1]!.goal = "Design things";
  });
  engine.revision = "r2";
  act(() => store.applyOrg({ name: "Acme", roles: [], units: [] }));
  fireEvent.click(await screen.findByRole("button", { name: "Update my draft" }));
  const dialog = await screen.findByRole("dialog", { name: "Update my draft and review" });
  const row = within(dialog).getByRole("row", { name: /the whole seat/ });
  expect(within(row).getByText("As it was")).toBeDefined();
  expect(within(row).getByText("Changed: goal")).toBeDefined();
  expect(within(row).getByText("Removed")).toBeDefined();
  expect(dialog.textContent).not.toContain("{");
});
