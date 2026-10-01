/**
 * The Builder keeps the operation log across a reload, offers it back
 * against the company it meets, and forgets it whenever the tab may have
 * changed hands.
 */

import { act, cleanup, fireEvent, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { chartPrint, fingerprint, fromChart } from "./model/document.ts";
import { EMPTY_DRAFT } from "./model/draft.ts";
import { OPERATIONS_VERSION, record, type Intent, type Operation } from "./model/operations.ts";
import { DRAFT_STORAGE_KEY, type DraftStorage, type KeptDraft } from "./model/persistence.ts";
import { templateIntent } from "./model/templates.ts";
import { countingKeys } from "./model/testkit.ts";
import {
  asReader,
  company,
  Engine,
  json,
  lensToolbar,
  mountBuilder,
  pressInView,
  pressOnBanner,
  rereadViewer,
} from "./testkit.tsx";
import { noteReader } from "~/lib/reader.ts";

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

/** An operation recorded against the fixture company, as a kept draft holds it. */
function recorded(intent: Intent): Operation {
  const { settings, chart } = company();
  const result = record(fromChart(settings, chart), intent);
  if (!result.ok) throw new Error(result.message);
  return result.op;
}

/** The fingerprint of the fixture company's chart, which a kept draft records beside the revision. */
const fixturePrint = () => fingerprint(chartPrint(company().chart));

const editCeo = (): Operation =>
  recorded({
    type: "updateSeat",
    target: "seat:ceo",
    set: [{ path: ["goal"], value: "Lead and more" }],
  });

function keep(draft: Partial<KeptDraft>): void {
  const kept: KeptDraft = {
    v: OPERATIONS_VERSION,
    mode: "edit",
    baseRevision: "r1",
    basePrint: fixturePrint(),
    ops: [editCeo()],
    undone: [],
    savedAt: 1_000,
    reader: "p-1",
    ...draft,
  };
  sessionStorage.setItem(DRAFT_STORAGE_KEY, JSON.stringify(kept));
}

const kept = () => sessionStorage.getItem(DRAFT_STORAGE_KEY);

/** Whether the draft on screen holds changes: Review and save opens only then. */
const holdsChanges = () =>
  !(within(lensToolbar()).getByRole("button", { name: "Review and save" }) as HTMLButtonElement)
    .disabled;

/** The Builder's one polite live region. */
const liveRegion = () => document.querySelector("[data-live-region]")!;

/** The sentence offering this tab's kept draft back, if it is on screen. */
const OFFER = /This tab kept a draft with 1 change/;

/** Presses one of the buttons the offer of a kept draft carries. */
const answerTheOffer = (name: "Keep the draft" | "Discard it") => pressOnBanner(OFFER, name);

// ONLY THE LOG. The company holds contact identities, emails and policies,
// and kept in storage they would outlive the operator's session.
test("an edit is kept as its operation log and nothing of the document", async () => {
  const engine = new Engine(company());
  const { settle, checked } = mountBuilder({ engine });
  await checked();
  pressInView("Edit CEO");
  await settle();
  expect(kept()).not.toBeNull();
  const value = JSON.parse(kept()!) as Record<string, unknown>;
  expect(Object.keys(value).sort()).toEqual(
    ["baseRevision", "basePrint", "mode", "ops", "reader", "savedAt", "undone", "v"].sort(),
  );
  expect(value.baseRevision).toBe("r1");
  expect(value.basePrint).toBe(fixturePrint());
  expect(value.ops).toHaveLength(1);
  expect(kept()).not.toContain("Designer");
  expect(kept()).not.toContain("anthropic");
});

test("a kept draft of the same revision waits for Keep or Discard, and Keep restores it", async () => {
  keep({});
  const engine = new Engine(company());
  const { settle } = mountBuilder({ engine });
  await settle();
  expect(screen.getByText(OFFER)).toBeDefined();
  // Nothing is recorded, and nothing is cleared, while the offer stands.
  expect(screen.getByText("read only")).toBeDefined();
  expect(kept()).not.toBeNull();

  answerTheOffer("Keep the draft");
  await settle();
  expect(holdsChanges()).toBe(true);
  expect(screen.getByText("editable")).toBeDefined();
  expect(kept()).not.toBeNull();
  expect(engine.chartWrites()).toHaveLength(0);
});

// A NEWER REVISION WHILE THE OFFER STANDS. The offer was made against the
// base on screen, so the base is not moved under it, and keeping the draft
// then leads into the update flow rather than a lens that is paused with no
// way forward.
test("a kept draft offered as a colleague saves is kept, then offered as an update", async () => {
  keep({});
  const engine = new Engine(company());
  const { store, settle, checked } = mountBuilder({ engine });
  await checked();
  expect(screen.getByText(OFFER)).toBeDefined();
  engine.seats.find((s) => s.handle === "designer")!.goal = "Design things";
  const reads = engine.chartReads().length;
  act(() => store.applyOrg(engine.orgPush()));
  await settle();
  expect(within(lensToolbar()).getByText("The company changed")).toBeDefined();
  // The base under the offer is not read again — only the check's read went
  // out: a Keep pressed while a newer base waited for its first check would be
  // refused, and the kept draft cleared with the refusal. Counted once the
  // lens has settled, so a read it was going to make has been made.
  expect(engine.chartReads()).toHaveLength(reads + 1);

  answerTheOffer("Keep the draft");
  await settle();
  pressOnBanner("Somebody changed the org chart since you started editing.", "Update my draft");
  await settle();
  const dialog = screen.getByRole("dialog", { name: "Update my draft and review" });
  fireEvent.click(within(dialog).getByRole("button", { name: "Update my draft" }));
  await checked();
  expect(holdsChanges()).toBe(true);
  // Kept again, now against the chart the colleague saved.
  const moved = fingerprint(chartPrint(engine.chart()));
  expect(JSON.parse(kept()!).basePrint).toBe(moved);
});

test("discarding a kept draft removes it and starts from the saved configuration", async () => {
  keep({});
  const engine = new Engine(company());
  const { settle } = mountBuilder({ engine });
  await settle();
  answerTheOffer("Discard it");
  await settle();
  expect(kept()).toBeNull();
  expect(screen.getByText("editable")).toBeDefined();
  expect(holdsChanges()).toBe(false);
  expect(engine.chartWrites()).toHaveLength(0);
});

// A DRAFT MUST SURVIVE A NEWER COMPANY, which is exactly when it matters:
// somebody saved while the operator was away — the settings, or the chart.
test.each([
  ["settings revision", { baseRevision: "r0" }],
  ["chart", { basePrint: "0000000000000000" }],
] as const)("a kept draft of an older %s is restored through the update flow", async (_, older) => {
  keep(older);
  const engine = new Engine(company());
  const { settle } = mountBuilder({ engine });
  await settle();
  const dialog = screen.getByRole("dialog", { name: "Restore the kept draft" });
  expect(within(dialog).getByText("Every change still applies.")).toBeDefined();
  fireEvent.click(within(dialog).getByRole("button", { name: "Restore the draft" }));
  await settle();
  expect(holdsChanges()).toBe(true);
  // Kept again, now against the company it was restored onto.
  const value = JSON.parse(kept()!) as KeptDraft;
  expect([value.baseRevision, value.basePrint]).toEqual(["r1", fixturePrint()]);
});

test("declining to restore a kept draft onto a newer company discards it", async () => {
  keep({ baseRevision: "r0" });
  const engine = new Engine(company());
  const { settle } = mountBuilder({ engine });
  await settle();
  const dialog = screen.getByRole("dialog", { name: "Restore the kept draft" });
  fireEvent.click(within(dialog).getByRole("button", { name: "Discard the kept draft" }));
  await settle();
  expect(kept()).toBeNull();
  expect(holdsChanges()).toBe(false);
});

test("a draft kept for creating a company is discarded, with a word, when a company exists", async () => {
  const built = templateIntent({ template: "empty", charter: { name: "Acme" } }, countingKeys("t"));
  if (!built.ok) throw new Error(built.message);
  const op = record(EMPTY_DRAFT, built.intent);
  if (!op.ok) throw new Error(op.message);
  keep({
    mode: "create",
    baseRevision: null,
    basePrint: fingerprint(chartPrint(null)),
    ops: [op.op],
  });
  const engine = new Engine(company());
  const { settle } = mountBuilder({ engine });
  await settle();
  expect(
    screen.getByText(
      "A draft for creating a company was discarded, because this engine now has a company.",
    ),
  ).toBeDefined();
  expect(kept()).toBeNull();
});

// ANOTHER TAB SIGNED IN AS SOMEBODY ELSE, and the cookie is the browser's:
// this tab now writes as them, so what it kept for the last reader is not
// theirs to be offered.
test("a change of reader forgets the kept draft and keeps the one on screen", async () => {
  let who = "jane.doe";
  const engine = new Engine(company());
  const { store, settle, checked } = mountBuilder({ engine, query: asReader(() => who) });
  await checked();
  pressInView("Edit CEO");
  await settle();
  expect(kept()).not.toBeNull();
  const reads = engine.chartReads().length;
  who = "sam.lee";
  rereadViewer(store);
  await checked();
  expect(kept()).toBeNull();
  // Checked again as the new reader, with the edit still in the draft.
  expect(engine.chartReads().length).toBeGreaterThan(reads);
  expect(holdsChanges()).toBe(true);
});

test("a change of reader while a kept draft is offered withdraws the offer", async () => {
  keep({});
  let who = "jane.doe";
  const engine = new Engine(company());
  const { store, settle } = mountBuilder({ engine, query: asReader(() => who) });
  await settle();
  expect(screen.getByText(OFFER)).toBeDefined();
  who = "sam.lee";
  rereadViewer(store);
  await settle();
  expect(screen.queryByText(/This tab kept a draft/)).toBeNull();
  expect(kept()).toBeNull();
  expect(screen.getByText("editable")).toBeDefined();
});

// THE CONTROL: the same reader read again is not somebody else, and a kept
// draft offered to them stays offered.
test("the same reader read again keeps the offer standing", async () => {
  keep({});
  const engine = new Engine(company());
  const { store, settle } = mountBuilder({ engine, query: asReader(() => "jane.doe") });
  await settle();
  expect(screen.getByText(OFFER)).toBeDefined();
  rereadViewer(store);
  await settle();
  expect(screen.getByText(OFFER)).toBeDefined();
  expect(kept()).not.toBeNull();
});

test("a refused read forgets a kept draft rather than offering it to the next reader", async () => {
  keep({});
  const engine = new Engine(company());
  engine.script = () => json({ error: "unauthorized" }, 401);
  const { settle } = mountBuilder({ engine });
  // SETTLED, NOT READ ON THE SPOT. The refusal is rendered from state and the
  // draft is dropped by the EFFECT that state schedules, so the message is in
  // the DOM one commit before the storage is cleared; a settled lens has run
  // both. The claim is that the draft is forgotten, and it still fails if it
  // never is.
  await settle();
  expect(
    screen.getByText(/^Editing the organization needs a credential the engine accepts\./),
  ).toBeDefined();
  expect(kept()).toBeNull();
});

test("storage that refuses says the draft will not survive a reload", async () => {
  const refusing: DraftStorage = {
    getItem: () => null,
    setItem: () => {
      throw new DOMException("quota", "QuotaExceededError");
    },
    removeItem: () => {},
  };
  const engine = new Engine(company());
  const { checked } = mountBuilder({ engine, storage: refusing });
  await checked();
  pressInView("Edit CEO");
  // The work goes on.
  await checked();
  expect(
    screen.getByText(
      "This browser refuses to keep a draft, so unsaved changes will not survive a reload or leaving the builder.",
    ),
  ).toBeDefined();
  expect(holdsChanges()).toBe(true);
  expect(engine.chartReads().length).toBeGreaterThan(1);
});

// A tab whose storage accessor throws has no storage at all, and loses the
// draft on a reload exactly as a refusing one does, so it says so too.
test("no storage at all says the draft will not survive a reload", async () => {
  const engine = new Engine(company());
  const { checked } = mountBuilder({ engine, storage: null });
  await checked();
  pressInView("Edit CEO");
  await checked();
  expect(liveRegion().textContent).toBe("Edited CEO: goal.");
  expect(holdsChanges()).toBe(true);
  expect(engine.chartReads().length).toBeGreaterThan(1);
  expect(
    screen.getByText(
      "This browser refuses to keep a draft, so unsaved changes will not survive a reload or leaving the builder.",
    ),
  ).toBeDefined();
});

// A CLOSED TAB TAKES THE DRAFT WITH IT: session storage survives a reload and
// a trip to another screen, never the tab itself.
test("a draft with changes asks before the tab goes, and one without does not", async () => {
  const unload = () => {
    const event = new Event("beforeunload", { cancelable: true });
    window.dispatchEvent(event);
    return event.defaultPrevented;
  };
  const { settle, checked } = mountBuilder({ engine: new Engine(company()) });
  await checked();
  expect(unload()).toBe(false);
  pressInView("Edit CEO");
  await settle();
  expect(unload()).toBe(true);
});

// WHERE NOTHING KEEPS THE DRAFT, leaving the lens loses it like a reload, so
// that move is asked about first; a move within the lens keeps the Builder
// and the draft, and is not.
test("a draft this browser cannot keep asks before the lens is left, not before a view changes", async () => {
  const refusing: DraftStorage = {
    getItem: () => null,
    setItem: () => {
      throw new DOMException("quota", "QuotaExceededError");
    },
    removeItem: () => {},
  };
  const { settle, checked, navigate } = mountBuilder({
    engine: new Engine(company()),
    storage: refusing,
  });
  await checked();
  pressInView("Edit CEO");
  await settle();
  expect(screen.getByText(/This browser refuses to keep a draft/)).toBeDefined();
  const start = location.hash;
  const onTable = "#/company?lens=builder&view=table";

  await navigate(() => {
    location.hash = onTable;
  }, onTable);
  expect(screen.queryByRole("dialog", { name: "Leave the builder?" })).toBeNull();

  // Held, so the browser is put back on the table once it has gone.
  await navigate(() => {
    location.hash = "#/people";
  }, onTable);
  const asked = screen.getByRole("dialog", { name: "Leave the builder?" });
  fireEvent.click(within(asked).getByRole("button", { name: "Stay" }));
  expect(screen.queryByRole("dialog", { name: "Leave the builder?" })).toBeNull();

  await navigate(() => {
    location.hash = "#/people";
  }, onTable);
  const again = screen.getByRole("dialog", { name: "Leave the builder?" });
  await navigate(
    () => fireEvent.click(within(again).getByRole("button", { name: "Leave without the draft" })),
    "#/people",
  );
  expect(start).toContain("lens=builder");
});

test("coming back to the lens restores this page's own draft without asking", async () => {
  const engine = new Engine(company());
  const first = mountBuilder({ engine });
  await first.checked();
  pressInView("Edit CEO");
  await first.settle();
  expect(kept()).not.toBeNull();
  first.view.unmount();

  const second = mountBuilder({ engine });
  await second.checked();
  expect(holdsChanges()).toBe(true);
  expect(screen.queryByText(/This tab kept a draft/)).toBeNull();
  expect(screen.getByText("editable")).toBeDefined();
});

// A DRAFT IS KEPT FOR SOMEBODY. A session that ended while the builder was
// closed routes the tab to the sign-in, and the builder the next person opens
// is a fresh mount — whose first viewer answer is not a change — so the last
// person's unsaved company edits were offered to them as their own, a save of
// which would have been recorded as theirs.
test("a draft kept for another reader is discarded and never offered", async () => {
  keep({ reader: "p-2" });
  const engine = new Engine(company());
  const { checked } = mountBuilder({ engine, reader: "p-1" });
  await checked();
  expect(kept()).toBeNull();
  expect(screen.queryByText(/This tab kept a draft/)).toBeNull();
  expect(screen.getByText("editable")).toBeDefined();
});

// AND NOTHING IS DECIDED UNTIL THE TAB KNOWS WHO READS IT: a draft neither
// offered to a reader who may not be its own, nor erased for one who may be.
test("a kept draft waits for the tab's reader before it is offered or cleared", async () => {
  keep({});
  const engine = new Engine(company());
  const { settle, checked } = mountBuilder({ engine, reader: null });
  await checked();
  expect(screen.queryByText(/This tab kept a draft/)).toBeNull();
  expect(kept()).not.toBeNull();

  act(() => noteReader("p-1"));
  await settle();
  expect(screen.getByText(OFFER)).toBeDefined();
});
