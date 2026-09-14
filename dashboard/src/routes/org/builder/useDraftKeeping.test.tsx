/**
 * The Builder keeps the operation log across a reload, offers it back
 * against the company it meets, and forgets it whenever the tab may have
 * changed hands.
 */

import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { storeToken } from "~/protocol/index.ts";
import { fromDocument } from "./model/document.ts";
import { EMPTY_DRAFT } from "./model/draft.ts";
import { OPERATIONS_VERSION, record, type Intent, type Operation } from "./model/operations.ts";
import { DRAFT_STORAGE_KEY, type DraftStorage, type KeptDraft } from "./model/persistence.ts";
import { templateIntent } from "./model/templates.ts";
import { countingKeys, fixtureDerived } from "./model/testkit.ts";
import { company, Engine, json, mountBuilder } from "./testkit.tsx";

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
  const doc = company();
  const result = record(fromDocument(doc, fixtureDerived(doc)), intent);
  if (!result.ok) throw new Error(result.message);
  return result.op;
}

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
    ops: [editCeo()],
    undone: [],
    savedAt: 1_000,
    ...draft,
  };
  sessionStorage.setItem(DRAFT_STORAGE_KEY, JSON.stringify(kept));
}

const kept = () => sessionStorage.getItem(DRAFT_STORAGE_KEY);

// ONLY THE LOG. The document holds contact identities, emails and policies,
// and kept in storage it would outlive the operator's token.
test("an edit is kept as its operation log and nothing of the document", async () => {
  const engine = new Engine(company());
  mountBuilder({ engine });
  await screen.findByText("No problems");
  fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
  await waitFor(() => expect(kept()).not.toBeNull());
  const value = JSON.parse(kept()!) as Record<string, unknown>;
  expect(Object.keys(value).sort()).toEqual(
    ["baseRevision", "mode", "ops", "savedAt", "undone", "v"].sort(),
  );
  expect(value.baseRevision).toBe("r1");
  expect(value.ops).toHaveLength(1);
  expect(kept()).not.toContain("Designer");
  expect(kept()).not.toContain("anthropic");
});

test("a kept draft of the same revision waits for Keep or Discard, and Keep restores it", async () => {
  keep({});
  const engine = new Engine(company());
  mountBuilder({ engine });
  expect(await screen.findByText(/This tab kept a draft with 1 change/)).toBeDefined();
  // Nothing is recorded, and nothing is cleared, while the offer stands.
  expect(screen.getByText("read only")).toBeDefined();
  expect(kept()).not.toBeNull();

  fireEvent.click(screen.getByRole("button", { name: "Keep the draft" }));
  await waitFor(() =>
    expect(JSON.stringify(engine.checks().at(-1)!.body)).toContain("Lead and more"),
  );
  expect(screen.getByText("editable")).toBeDefined();
  expect(kept()).not.toBeNull();
});

// A NEWER REVISION WHILE THE OFFER STANDS. The offer was made against the
// base on screen, so the base is not moved under it, and keeping the draft
// then leads into the update flow rather than a lens that is paused with no
// way forward.
test("a kept draft offered as a colleague saves is kept, then offered as an update", async () => {
  keep({});
  const engine = new Engine(company());
  const { store } = mountBuilder({ engine });
  await screen.findByText(/This tab kept a draft with 1 change/);
  await waitFor(() => expect(engine.checks()).toHaveLength(1));
  const next = company();
  next.roles![1]!.goal = "Design things";
  engine.document = next;
  engine.revision = "r2";
  const reads = engine.sent("GET").length;
  act(() => store.applyOrg({ name: "Acme", roles: [], units: [] }));
  expect(await screen.findByText("The configuration changed")).toBeDefined();
  // The base under the offer is not read again: a Keep pressed while a newer
  // base waited for its first check would be refused, and the kept draft
  // cleared with the refusal.
  await new Promise((r) => setTimeout(r, 100));
  expect(engine.sent("GET")).toHaveLength(reads);

  fireEvent.click(screen.getByRole("button", { name: "Keep the draft" }));
  fireEvent.click(await screen.findByRole("button", { name: "Update my draft" }));
  const dialog = await screen.findByRole("dialog", { name: "Update my draft and review" });
  fireEvent.click(within(dialog).getByRole("button", { name: "Update my draft" }));
  await waitFor(() => expect(engine.checks().at(-1)!.headers["If-Match"]).toBe('"r2"'));
  expect(JSON.stringify(engine.checks().at(-1)!.body)).toContain("Lead and more");
  await waitFor(() => expect(JSON.parse(kept()!).baseRevision).toBe("r2"));
});

test("discarding a kept draft removes it and starts from the saved configuration", async () => {
  keep({});
  const engine = new Engine(company());
  mountBuilder({ engine });
  fireEvent.click(await screen.findByRole("button", { name: "Discard it" }));
  await waitFor(() => expect(kept()).toBeNull());
  expect(screen.getByText("editable")).toBeDefined();
  expect(engine.checks().at(-1)!.body).toEqual({});
});

// A DRAFT MUST SURVIVE A NEWER REVISION, which is exactly when it matters:
// somebody saved while the operator was away.
test("a kept draft of an older revision is restored through the update flow", async () => {
  keep({ baseRevision: "r0" });
  const engine = new Engine(company());
  mountBuilder({ engine });
  const dialog = await screen.findByRole("dialog", { name: "Restore the kept draft" });
  expect(within(dialog).getByText("Every change still applies.")).toBeDefined();
  fireEvent.click(within(dialog).getByRole("button", { name: "Restore the draft" }));
  await waitFor(() =>
    expect(JSON.stringify(engine.checks().at(-1)!.body)).toContain("Lead and more"),
  );
  // Kept again, now against the revision it was restored onto.
  await waitFor(() => expect(JSON.parse(kept()!).baseRevision).toBe("r1"));
});

test("declining to restore a kept draft onto a newer revision discards it", async () => {
  keep({ baseRevision: "r0" });
  const engine = new Engine(company());
  mountBuilder({ engine });
  const dialog = await screen.findByRole("dialog", { name: "Restore the kept draft" });
  fireEvent.click(within(dialog).getByRole("button", { name: "Discard the kept draft" }));
  await waitFor(() => expect(kept()).toBeNull());
  expect(engine.checks().at(-1)!.body).toEqual({});
});

test("a draft kept for creating a company is discarded, with a word, when a company exists", async () => {
  const built = templateIntent({ template: "empty", charter: { name: "Acme" } }, countingKeys("t"));
  if (!built.ok) throw new Error(built.message);
  const op = record(EMPTY_DRAFT, built.intent);
  if (!op.ok) throw new Error(op.message);
  keep({ mode: "create", baseRevision: null, ops: [op.op] });
  const engine = new Engine(company());
  mountBuilder({ engine });
  expect(
    await screen.findByText(
      "A draft for creating a company was discarded, because this engine now has a company.",
    ),
  ).toBeDefined();
  expect(kept()).toBeNull();
});

test("a token change forgets the kept draft and keeps the one on screen", async () => {
  const engine = new Engine(company());
  mountBuilder({ engine });
  await screen.findByText("No problems");
  fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
  await waitFor(() => expect(kept()).not.toBeNull());
  act(() => {
    storeToken("someone-else");
  });
  await waitFor(() => expect(kept()).toBeNull());
  await waitFor(() =>
    expect(engine.checks().at(-1)!.headers.Authorization).toBe("Bearer someone-else"),
  );
  expect(JSON.stringify(engine.checks().at(-1)!.body)).toContain("Lead and more");
});

test("a token change while a kept draft is offered withdraws the offer", async () => {
  keep({});
  const engine = new Engine(company());
  mountBuilder({ engine });
  await screen.findByText(/This tab kept a draft with 1 change/);
  act(() => {
    storeToken("someone-else");
  });
  await waitFor(() => expect(screen.queryByText(/This tab kept a draft/)).toBeNull());
  expect(kept()).toBeNull();
  expect(screen.getByText("editable")).toBeDefined();
});

test("a refused read forgets a kept draft rather than offering it to the next reader", async () => {
  keep({});
  const engine = new Engine(company());
  engine.script = () => json({ error: "unauthorized" }, 401);
  mountBuilder({ engine });
  await screen.findByText("Editing the organization needs an operator token.");
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
  mountBuilder({ engine, storage: refusing });
  await screen.findByText("No problems");
  fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
  expect(
    await screen.findByText(
      "This browser refuses to keep a draft, so unsaved changes will not survive a reload.",
    ),
  ).toBeDefined();
  // The work goes on.
  await waitFor(() =>
    expect(JSON.stringify(engine.checks().at(-1)!.body)).toContain("Lead and more"),
  );
});

test("coming back to the lens restores this page's own draft without asking", async () => {
  const engine = new Engine(company());
  const first = mountBuilder({ engine });
  await screen.findByText("No problems");
  fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
  await waitFor(() => expect(kept()).not.toBeNull());
  first.view.unmount();

  mountBuilder({ engine });
  await waitFor(() =>
    expect(JSON.stringify(engine.checks().at(-1)!.body)).toContain("Lead and more"),
  );
  expect(screen.queryByText(/This tab kept a draft/)).toBeNull();
  expect(screen.getByText("editable")).toBeDefined();
});
