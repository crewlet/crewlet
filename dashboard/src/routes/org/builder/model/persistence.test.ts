// @vitest-environment node
/**
 * Keeping the operation log across a reload.
 *
 * What these protect: only the log is kept, under one key; everything this
 * build records reads back, and anything else (a wrong version, an unknown or
 * reshaped operation, an edit written around its own operation's rules, a
 * template in edit mode) is discarded whole; storage that refuses is reported
 * rather than thrown; a draft over the cap is not kept; and what a restored
 * draft offers depends on the mode and revision it meets.
 */

import { describe, expect, test } from "vitest";
import { EMPTY_DRAFT, type Draft } from "./draft.ts";
import { fromDocument } from "./document.ts";
import { apply, record, OPERATIONS_VERSION, type Intent, type Operation } from "./operations.ts";
import { EMPTY_LOG } from "./history.ts";
import {
  DRAFT_STORAGE_KEY,
  MAX_KEPT_OPERATIONS,
  clearDraft,
  isOperation,
  keepDraft,
  parseKeptDraft,
  persistencePlan,
  restoreDraft,
  restoreOffer,
  type DraftStorage,
  type KeptDraft,
} from "./persistence.ts";
import { templateIntent } from "./templates.ts";
import { countingKeys, fixtureCompany, fixtureDerived } from "./testkit.ts";

class MemoryStorage implements DraftStorage {
  readonly items = new Map<string, string>();
  getItem(key: string) {
    return this.items.get(key) ?? null;
  }
  setItem(key: string, value: string) {
    this.items.set(key, value);
  }
  removeItem(key: string) {
    this.items.delete(key);
  }
}

class RefusingStorage implements DraftStorage {
  getItem(): string | null {
    throw new DOMException("The operation is insecure.", "SecurityError");
  }
  setItem(): void {
    throw new DOMException("The quota has been exceeded.", "QuotaExceededError");
  }
  removeItem(): void {
    throw new DOMException("The operation is insecure.", "SecurityError");
  }
}

/**
 * One operation of every type this build records, from the fixture company.
 * A renamed node keeps its key, so the later operations on `seat:dev` and
 * `unit:Sales` address the renamed seat and unit.
 */
function everyOperation(): Operation[] {
  const doc = fixtureCompany();
  let draft: Draft = fromDocument(doc, fixtureDerived(doc));
  const ops: Operation[] = [];
  const intents: Intent[] = [
    {
      type: "addUnit",
      key: "new:u1",
      placement: { parent: "company", after: null },
      data: { name: "Legal" },
    },
    {
      type: "addSeat",
      key: "new:s1",
      placement: { parent: "new:u1", after: null },
      data: { name: "Counsel" },
    },
    { type: "renameSeat", target: "seat:dev", name: "Developer" },
    { type: "renameUnit", target: "unit:Sales", name: "Revenue" },
    {
      type: "move",
      target: "seat:designer",
      to: { parent: "unit:Platform", after: null },
      clearLeads: [],
    },
    { type: "reorder", target: "seat:dev", to: { parent: "unit:Engineering", after: null } },
    {
      type: "updateSeat",
      target: "seat:sre",
      set: [{ path: ["goal"], value: "Automate" }],
      accessLevel: "developer",
    },
    { type: "updateUnit", target: "unit:Platform", set: [{ path: ["purpose"], value: "Run it" }] },
    { type: "setLead", target: "unit:Sales", lead: "Account Executive" },
    { type: "setManages", target: "seat:ceo", manages: ["Engineering"] },
    {
      type: "changeKind",
      target: "seat:account-executive",
      kind: "human",
      contact: { slack_user_id: "U1" },
    },
    { type: "setScheduleEnabled", target: "unit:Engineering", schedule: "standup", enabled: false },
    { type: "setDatadogRouteTo", routeTo: "dev" },
    { type: "updateCompany", set: [{ path: ["vision"], value: "Everywhere" }] },
    {
      type: "edit",
      target: "seat:sre",
      intents: [
        { type: "renameSeat", target: "seat:sre", name: "Reliability Engineer" },
        { type: "updateSeat", target: "seat:sre", set: [{ path: ["goal"], value: "Automate it" }] },
      ],
    },
    { type: "remove", target: "unit:Sales" },
  ];
  for (const intent of intents) {
    const result = record(draft, intent);
    if (!result.ok) throw new Error(`${intent.type}: ${result.message}`);
    ops.push(result.op);
    draft = apply(draft, result.op).draft;
  }
  return ops;
}

function templateOperation(): Operation {
  const built = templateIntent(
    { template: "new_company", charter: { name: "Acme" } },
    countingKeys(),
  );
  if (!built.ok) throw new Error(built.message);
  const result = record(EMPTY_DRAFT, built.intent);
  if (!result.ok) throw new Error(result.message);
  return result.op;
}

const charterEdit = everyOperation().find((op) => op.type === "updateCompany")!;

const kept = (overrides: Partial<KeptDraft> = {}): KeptDraft => ({
  v: OPERATIONS_VERSION,
  mode: "edit",
  baseRevision: "rev-1",
  ops: [],
  undone: [],
  savedAt: 1_700_000_000_000,
  ...overrides,
});

describe("isOperation", () => {
  test("accepts every operation type this build records, after a trip through JSON", () => {
    const ops = [...everyOperation(), templateOperation()];
    const types = new Set(ops.map((op) => op.type));
    expect(types.size).toBe(17);
    for (const op of ops) expect(isOperation(JSON.parse(JSON.stringify(op))), op.type).toBe(true);
  });

  test("refuses anything reshaped", () => {
    const [addUnit, addSeat] = everyOperation();
    expect(isOperation({ ...addUnit, extra: 1 })).toBe(false);
    expect(isOperation({ ...addSeat, key: "seat:not-minted" })).toBe(false);
    expect(isOperation({ ...addSeat, placement: { parent: "company" } })).toBe(false);
    expect(isOperation({ type: "renameEverything" })).toBe(false);
    // An edit holds only the changes an editor makes, each in its own shape.
    const edit = everyOperation().find((op) => op.type === "edit")!;
    const remove = everyOperation().find((op) => op.type === "remove")!;
    expect(isOperation({ ...edit, ops: [remove, remove] })).toBe(false);
    expect(isOperation({ ...edit, ops: [edit, edit] })).toBe(false);
    expect(isOperation({ ...edit, ops: [{ ...addSeat, key: "seat:x" }] })).toBe(false);
    expect(isOperation(null)).toBe(false);
  });
});

describe("keeping and restoring", () => {
  test("only the log is kept, under one key, and reads back whole", () => {
    const storage = new MemoryStorage();
    const ops = everyOperation();
    const value = kept({ ops: ops.slice(0, 5), undone: ops.slice(5, 7) });
    expect(keepDraft(storage, value)).toBe("kept");
    expect([...storage.items.keys()]).toEqual([DRAFT_STORAGE_KEY]);
    expect(Object.keys(JSON.parse(storage.items.get(DRAFT_STORAGE_KEY)!)).sort()).toEqual([
      "baseRevision",
      "mode",
      "ops",
      "savedAt",
      "undone",
      "v",
    ]);
    expect(restoreDraft(storage)).toEqual({ kind: "restored", kept: value });
    expect(clearDraft(storage)).toBe("cleared");
    expect(restoreDraft(storage)).toEqual({ kind: "none" });
  });

  test("anything this build cannot replay is discarded whole and removed", () => {
    const cases: [string, unknown][] = [
      ["not JSON", "{"],
      ["another version", kept({ v: OPERATIONS_VERSION + 1 })],
      ["an unknown mode", { ...kept(), mode: "merge" }],
      ["an edit without its revision", kept({ baseRevision: null })],
      ["a create with a revision", kept({ mode: "create", baseRevision: "rev-1" })],
      ["an extra key", { ...kept(), document: { name: "leaked" } }],
      ["a template in edit mode", kept({ ops: [templateOperation()] })],
      [
        "a rename written as an edit",
        kept({
          ops: [
            {
              type: "updateSeat",
              target: "seat:dev",
              changes: [{ path: ["name"], after: "X" }],
              accessLevels: [],
            },
          ],
        }),
      ],
      [
        "over the cap",
        kept({ ops: Array.from({ length: MAX_KEPT_OPERATIONS + 1 }, () => charterEdit) }),
      ],
    ];
    for (const [label, value] of cases) {
      const storage = new MemoryStorage();
      storage.setItem(DRAFT_STORAGE_KEY, typeof value === "string" ? value : JSON.stringify(value));
      expect(restoreDraft(storage), label).toEqual({ kind: "discarded" });
      expect(storage.items.size, label).toBe(0);
    }
    expect(
      parseKeptDraft(kept({ mode: "create", baseRevision: null, ops: [templateOperation()] })),
    ).toBeDefined();
  });

  test("a draft over the cap is not kept, and a kept one it replaces is removed", () => {
    const storage = new MemoryStorage();
    keepDraft(storage, kept({ ops: everyOperation().slice(0, 1) }));
    const big = kept({ ops: Array.from({ length: MAX_KEPT_OPERATIONS + 1 }, () => charterEdit) });
    expect(keepDraft(storage, big)).toBe("too_large");
    expect(storage.items.size).toBe(0);
  });

  test("storage that refuses is reported, never thrown, and missing storage says so", () => {
    const refusing = new RefusingStorage();
    expect(keepDraft(refusing, kept())).toBe("refused");
    expect(restoreDraft(refusing)).toEqual({ kind: "refused" });
    expect(clearDraft(refusing)).toBe("refused");
    expect(keepDraft(null, kept())).toBe("unavailable");
    expect(restoreDraft(null)).toEqual({ kind: "unavailable" });
  });
});

describe("restoreOffer", () => {
  test("keep or discard on the same revision, update when it moved, discard when the mode changed", () => {
    expect(restoreOffer(kept(), { mode: "edit", revision: "rev-1" })).toEqual({
      kind: "keep_or_discard",
    });
    expect(restoreOffer(kept(), { mode: "edit", revision: "rev-2" })).toEqual({
      kind: "update",
      from: "rev-1",
      to: "rev-2",
    });
    expect(restoreOffer(kept(), { mode: "create", revision: null })).toEqual({
      kind: "discard_mode_changed",
      kept: "edit",
      loaded: "create",
    });
    expect(
      restoreOffer(kept({ mode: "create", baseRevision: null }), {
        mode: "edit",
        revision: "rev-1",
      }),
    ).toMatchObject({
      kind: "discard_mode_changed",
    });
  });
});

describe("persistencePlan", () => {
  test("keeps a log, and clears when there is none or the tab may have changed hands", () => {
    const ops = everyOperation().slice(0, 2);
    const state = {
      mode: "edit" as const,
      baseRevision: "rev-1",
      log: { ops, undone: [] },
      keep: true,
    };
    expect(persistencePlan(state, 42)).toEqual({
      action: "keep",
      kept: kept({ ops, savedAt: 42 }),
    });
    expect(persistencePlan({ ...state, keep: false }, 42)).toEqual({ action: "clear" });
    expect(persistencePlan({ ...state, log: EMPTY_LOG }, 42)).toEqual({ action: "clear" });
  });
});
