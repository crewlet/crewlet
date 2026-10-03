/**
 * A newer company found by a check is offered as an update of the draft,
 * never replayed over silently: what applies is replayed onto the chart and
 * the settings revision the node serves now, and a value somebody else
 * changed waits for a choice.
 */

import { act, cleanup, fireEvent, screen, within } from "~/test/inCase.ts";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { COMPANY_KEY } from "./model/keys.ts";
import type { Conflict } from "./model/operations.ts";
import { clearSavedChanges } from "./savedChanges.ts";
import {
  company,
  Engine,
  json,
  builderToolbar,
  mountBuilder,
  type MountedBuilder,
  pressInToolbar,
  pressInView,
  pressOnBanner,
} from "./testkit.tsx";
import { conflictCells } from "./UpdateDraftDialog.tsx";

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

const CHART_MOVED = "Somebody changed the org chart since you started editing.";
const SETTINGS_MOVED = "The settings changed since you started editing.";
const NO_LONGER_ACTIVE =
  "The configuration this draft edits is no longer active on this engine, so the draft cannot be saved.";

/** The Builder's one polite live region. */
const liveRegion = () => document.querySelector("[data-live-region]")!;

/** Opens the builder, lets a colleague change the chart, and edits the CEO. */
async function editAfterColleague(
  change: (engine: Engine) => void,
): Promise<{ engine: Engine; builder: MountedBuilder }> {
  const engine = new Engine(company());
  const builder = mountBuilder({ engine });
  await builder.checked();
  change(engine);
  pressInView("Edit CEO");
  await builder.settle();
  expect(screen.getByText(CHART_MOVED)).toBeDefined();
  return { engine, builder };
}

/** Presses the conflict's Update my draft, and settles on the dialog it opens. */
async function updateMyDraft(
  builder: MountedBuilder,
  conflict: string = CHART_MOVED,
): Promise<HTMLElement> {
  pressOnBanner(conflict, "Update my draft");
  await builder.settle();
  return screen.getByRole("dialog", { name: "Update my draft and review" });
}

const seat = (engine: Engine, handle: string) => engine.seats.find((s) => s.handle === handle)!;

/** Whether Review and save opens. */
const reviewOpens = () =>
  !(within(builderToolbar()).getByRole("button", { name: "Review and save" }) as HTMLButtonElement)
    .disabled;

test("an edit that still applies is replayed onto the newer chart, and saving keeps the colleague's change", async () => {
  const { engine, builder } = await editAfterColleague((e) => {
    seat(e, "designer").goal = "Design things";
  });
  // A chart has no revision to diff against, so no link to one is offered.
  expect(screen.queryByRole("link", { name: "Show what changed" })).toBeNull();
  const dialog = await updateMyDraft(builder);
  expect(within(dialog).getByText("Every change still applies.")).toBeDefined();
  fireEvent.click(within(dialog).getByRole("button", { name: "Update my draft" }));
  await builder.checked();

  pressInToolbar("Review and save");
  const review = screen.getByRole("dialog", { name: "Review and save" });
  expect(within(review).getByText("Edits CEO: goal.")).toBeDefined();
  // The colleague's change is part of the base now, and not a change of this draft's.
  expect(within(review).queryByText(/Designer/)).toBeNull();
  fireEvent.click(within(review).getByRole("button", { name: "Save" }));
  await builder.settle();
  expect(seat(engine, "ceo").goal).toBe("Lead and more");
  expect(seat(engine, "designer").goal).toBe("Design things");
});

test("a value somebody else changed waits for a choice, and keeping theirs drops mine", async () => {
  const { engine, builder } = await editAfterColleague((e) => {
    seat(e, "ceo").goal = "Lead well";
  });
  const dialog = await updateMyDraft(builder);
  const row = within(dialog).getByRole("row", { name: /goal/ });
  expect(within(row).getByText("Lead")).toBeDefined();
  expect(within(row).getByText("Lead well")).toBeDefined();
  expect(within(row).getByText("Lead and more")).toBeDefined();
  const confirm = within(dialog).getByRole("button", { name: "Update my draft" });
  expect((confirm as HTMLButtonElement).disabled).toBe(true);

  fireEvent.click(within(dialog).getByRole("radio", { name: "Keep theirs" }));
  expect((confirm as HTMLButtonElement).disabled).toBe(false);
  fireEvent.click(confirm);

  // Nothing of the draft is left: the chart's value is the one kept.
  await builder.checked();
  expect(reviewOpens()).toBe(false);
  expect(engine.chartWrites()).toHaveLength(0);
});

describe("a settings revision saved by somebody else", () => {
  /** Opens the builder, renames the company, and lets a colleague save r2 first. */
  async function renameAfterColleague(): Promise<{ engine: Engine; builder: MountedBuilder }> {
    const engine = new Engine(company());
    const builder = mountBuilder({ engine });
    await builder.checked();
    engine.settings = { ...engine.settings, mission: "Make better things" };
    engine.revision = "r2";
    pressInView("Rename the company");
    await builder.settle();
    expect(screen.getByText(SETTINGS_MOVED)).toBeDefined();
    return { engine, builder };
  }

  test("is offered with a link to what changed, and replayed onto that revision", async () => {
    const { engine, builder } = await renameAfterColleague();
    // The newer revision against the draft's base, read forwards.
    expect(screen.getByRole("link", { name: "Show what changed" }).getAttribute("href")).toBe(
      "#/settings/config?lens=diff&revision=r2&against=r1",
    );
    const dialog = await updateMyDraft(builder, SETTINGS_MOVED);
    fireEvent.click(within(dialog).getByRole("button", { name: "Update my draft" }));
    await builder.settle();
    expect(engine.checks().at(-1)!.headers["If-Match"]).toBe('"r2"');
    expect(engine.checks().at(-1)!.body).toEqual({ name: "Acme Labs" });
  });

  test("a node still serving the draft's base is not updated onto", async () => {
    const { engine, builder } = await renameAfterColleague();
    // This node answers reads with the old revision while the conflict named r2.
    engine.script = (r) =>
      r.method === "GET" && r.path === "/config"
        ? json(company().settings, 200, { ETag: '"r1"' })
        : null;
    pressOnBanner(SETTINGS_MOVED, "Update my draft");
    await builder.settle();
    expect(
      screen.getByText(
        "This node has not caught up with the newer settings revision yet. Try again in a moment.",
      ),
    ).toBeDefined();
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});

test("a draft of a configuration that is no longer active offers to discard and reload", async () => {
  const engine = new Engine(company());
  const builder = mountBuilder({ engine });
  await builder.checked();
  engine.script = (r) =>
    r.query.get("dry_run") === "true" ? json({ error: "no_active_revision" }, 412) : null;
  pressInView("Rename the company");
  await builder.settle();
  expect(screen.getByText(NO_LONGER_ACTIVE)).toBeDefined();
  engine.script = () => null;
  const reads = engine.sent("GET").length;
  pressOnBanner(NO_LONGER_ACTIVE, "Discard and reload");
  await builder.checked();
  expect(engine.sent("GET").length).toBeGreaterThan(reads);
  // Discarded: nothing is left to dry-run.
  expect(engine.checks()).toHaveLength(1);
});

// A CONFLICT IS SHOWN AS A PERSON READS IT. The builder's own structures
// (node keys, a removed node, a kind change's stripped fields, an address
// somebody else took) were printed as JSON, which put internal keys and
// credential masks in a table an operator decides from.
describe("a conflict's values", () => {
  const names: Record<string, string> = {
    "seat:dev": "Dev",
    "seat:qa": "QA",
    "unit:engineering": "Engineering",
    "unit:sales": "Sales",
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

  test("where a node sits names the unit it is in, never its key", () => {
    expect(
      cells({
        subject: "where it sits",
        shape: "parent",
        base: "unit:engineering",
        theirs: COMPANY_KEY,
        mine: "unit:sales",
      }),
    ).toEqual({ base: "Engineering", theirs: "The top of the organization", mine: "Sales" });
    expect(
      cells({ subject: "where it sits", shape: "parent", base: "new:gone", theirs: "unit:sales" }),
    ).toEqual({
      base: "a node no longer in the organization",
      theirs: "Sales",
      mine: "Not set",
    });
  });

  test("an address somebody else took names both holders, never the address's key", () => {
    expect(
      cells({
        subject: "the address qa",
        shape: "snapshot",
        address: "qa",
        base: undefined,
        theirs: { handle: "qa", name: "Quality" },
        mine: { handle: "qa", name: "QA" },
      }),
    ).toEqual({ base: "Free", theirs: "Held by Quality", mine: "This draft's QA" });
  });

  test("a removed node reads as the fields that changed, and stripped fields by name only", () => {
    const removal = cells({
      subject: "the whole seat",
      shape: "snapshot",
      base: {
        name: "Dev",
        goal: "Build",
        runtime: { mcp_env: { tracker: { TOKEN: "__redacted__" } } },
      },
      theirs: {
        name: "Dev",
        goal: "Ship",
        runtime: { mcp_env: { tracker: { TOKEN: "__redacted__" } } },
      },
    });
    expect(removal).toEqual({ base: "As it was", theirs: "Changed: goal", mine: "Removed" });

    const stripped = cells({
      subject: "fields the new kind removes",
      shape: "fields",
      base: [
        { path: ["runtime", "mcp_env"], before: { tracker: { TOKEN: "__redacted__" } } },
        { path: ["project"], before: "OPS" },
      ],
      theirs: [
        { path: ["runtime", "mcp_env"], before: { tracker: { TOKEN: "__redacted__" } } },
        { path: ["project"], before: "SUP" },
        { path: ["email"], before: "dev@example.com" },
      ],
    });
    expect(stripped).toEqual({
      base: "mcp_env, project",
      theirs: "mcp_env, project (changed), email (added)",
      mine: "Removed",
    });
    const shown = JSON.stringify([removal, stripped]);
    expect(shown).not.toContain("__redacted__");
    expect(shown).not.toContain("OPS");
  });
});

test("a removal somebody else edited first names what they changed", async () => {
  const engine = new Engine(company());
  const builder = mountBuilder({ engine });
  await builder.checked();
  pressInView("Remove Designer");
  await builder.checked();
  expect(liveRegion().textContent).toContain("Designer");
  seat(engine, "designer").goal = "Design things";
  act(() => builder.store.applyOrg(engine.orgPush()));
  await builder.settle();
  const dialog = await updateMyDraft(builder);
  const row = within(dialog).getByRole("row", { name: /the whole seat/ });
  expect(within(row).getByText("As it was")).toBeDefined();
  expect(within(row).getByText("Changed: goal")).toBeDefined();
  expect(within(row).getByText("Removed")).toBeDefined();
  expect(dialog.textContent).not.toContain("{");
});
