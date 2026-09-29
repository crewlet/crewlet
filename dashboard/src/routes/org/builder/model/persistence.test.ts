// @vitest-environment node
/**
 * Keeping the operation log across a reload.
 *
 * What these protect: only the log is kept, under one key, beside the
 * fingerprint of the chart it was made on and never the chart; everything this
 * build records reads back, and anything else (a wrong version, an unknown or
 * reshaped operation, an edit written around its own operation's rules, a
 * template in edit mode, a fingerprint that is not one) is discarded whole;
 * storage that refuses is reported rather than thrown; a draft over the cap is
 * not kept; and what a restored draft offers depends on the mode, the revision
 * and the chart it meets.
 */

import { describe, expect, test } from "vitest";
import { EMPTY_DRAFT, type Draft } from "./draft.ts";
import { chartPrint, fingerprint, fromChart } from "./document.ts";
import { apply, record, OPERATIONS_VERSION, type Intent, type Operation } from "./operations.ts";
import { EMPTY_LOG } from "./history.ts";
import {
  DRAFT_STORAGE_KEY,
  MAX_KEPT_OPERATIONS,
  clearDraft,
  isOperation,
  keepDraft,
  markPendingWrite,
  parseKeptDraft,
  persistencePlan,
  restoreDraft,
  restoreOffer,
  type DraftStorage,
  type KeptDraft,
} from "./persistence.ts";
import { templateIntent } from "./templates.ts";
import { countingKeys, fixtureChart, fixtureSettings } from "./testkit.ts";

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

const PRINT = fingerprint(chartPrint(fixtureChart()));

/** One operation of every type this build records, from the fixture company. */
function everyOperation(): Operation[] {
  let draft: Draft = fromChart(fixtureSettings(), fixtureChart());
  const ops: Operation[] = [];
  const intents: Intent[] = [
    {
      type: "addUnit",
      key: "new:u1",
      placement: { parent: "company" },
      data: { key: "legal", name: "Legal" },
    },
    {
      type: "addSeat",
      key: "new:s1",
      placement: { parent: "new:u1" },
      data: { handle: "counsel", name: "Counsel" },
    },
    { type: "renameSeat", target: "seat:dev", name: "Developer" },
    { type: "renameUnit", target: "unit:sales", name: "Revenue" },
    { type: "move", target: "seat:designer", to: { parent: "unit:engineering" }, clearLeads: [] },
    {
      type: "updateSeat",
      target: "seat:sre",
      set: [{ path: ["goal"], value: "Automate" }],
      accessLevel: "developer",
    },
    { type: "updateUnit", target: "unit:platform", set: [{ path: ["purpose"], value: "Run it" }] },
    { type: "setLead", target: "unit:sales", lead: "account-executive" },
    { type: "setManages", target: "seat:ceo", manages: ["engineering"] },
    {
      type: "changeKind",
      target: "seat:account-executive",
      kind: "human",
      contact: { slack_user_id: "U1" },
    },
    { type: "setScheduleEnabled", target: "unit:engineering", schedule: "standup", enabled: false },
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
    { type: "remove", target: "unit:sales" },
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
  basePrint: PRINT,
  ops: [],
  undone: [],
  savedAt: 1_700_000_000_000,
  reader: "p-1",
  ...overrides,
});

describe("isOperation", () => {
  test("accepts every operation type this build records, after a trip through JSON", () => {
    const ops = [...everyOperation(), templateOperation()];
    const types = new Set(ops.map((op) => op.type));
    expect(types.size).toBe(16);
    for (const op of ops) expect(isOperation(JSON.parse(JSON.stringify(op))), op.type).toBe(true);
  });

  test("refuses anything reshaped", () => {
    const [addUnit, addSeat] = everyOperation();
    expect(isOperation({ ...addUnit, extra: 1 })).toBe(false);
    expect(isOperation({ ...addSeat, key: "seat:not-minted" })).toBe(false);
    // A placement is a parent and nothing else: the chart keeps no sibling order.
    expect(isOperation({ ...addSeat, placement: { parent: "company", after: null } })).toBe(false);
    // A created node states its address.
    expect(isOperation({ ...addSeat, data: { name: "Counsel" } })).toBe(false);
    expect(isOperation({ ...addUnit, data: { name: "Legal" } })).toBe(false);
    expect(isOperation({ type: "reorder", target: "seat:dev", to: { parent: "company" } })).toBe(
      false,
    );
    // An edit holds only the changes an editor makes, each in its own shape.
    const edit = everyOperation().find((op) => op.type === "edit")!;
    const remove = everyOperation().find((op) => op.type === "remove")!;
    expect(isOperation({ ...edit, ops: [remove, remove] })).toBe(false);
    expect(isOperation({ ...edit, ops: [edit, edit] })).toBe(false);
    expect(isOperation(null)).toBe(false);
  });
});

describe("keeping and restoring", () => {
  test("only the log is kept, under one key, with the chart's fingerprint and never the chart", () => {
    const storage = new MemoryStorage();
    const ops = everyOperation();
    const value = kept({ ops: ops.slice(0, 5), undone: ops.slice(5, 7) });
    expect(keepDraft(storage, value)).toBe("kept");
    expect([...storage.items.keys()]).toEqual([DRAFT_STORAGE_KEY]);
    expect(Object.keys(JSON.parse(storage.items.get(DRAFT_STORAGE_KEY)!)).sort()).toEqual([
      "basePrint",
      "baseRevision",
      "mode",
      "ops",
      "reader",
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
      ["no fingerprint", { ...kept(), basePrint: undefined }],
      // A DRAFT NOBODY KEPT is a draft nobody can be offered.
      ["no reader", { ...kept(), reader: undefined }],
      ["an empty reader", kept({ reader: "" })],
      ["a chart where a fingerprint goes", kept({ basePrint: JSON.stringify(fixtureChart()) })],
      ["an extra key", { ...kept(), chart: { seats: [] } }],
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
      [
        "creations without a write",
        kept({ creates: [{ key: "new:a", kind: "seat", address: "a" }] }),
      ],
    ];
    for (const [label, value] of cases) {
      const storage = new MemoryStorage();
      storage.setItem(DRAFT_STORAGE_KEY, typeof value === "string" ? value : JSON.stringify(value));
      expect(restoreDraft(storage), label).toEqual({ kind: "discarded" });
      expect(storage.items.size, label).toBe(0);
    }
    // Control: a template in create mode is what a create draft keeps.
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
  const loaded = (
    revision: string | null,
    print = PRINT,
    mode: "edit" | "create" = "edit",
    reader = "p-1",
  ) => ({ mode, revision, print, reader });

  test("keep or discard on the same company, update when its settings or its chart moved", () => {
    expect(restoreOffer(kept(), loaded("rev-1"))).toEqual({ kind: "keep_or_discard" });
    expect(restoreOffer(kept(), loaded("rev-2"))).toEqual({ kind: "update" });
    // The chart's rows changed while the settings did not: still somebody else's save.
    expect(restoreOffer(kept(), loaded("rev-1", "0123456789abcdef"))).toEqual({ kind: "update" });
    // A save of the draft was out: whatever landed is the rebase's to meet.
    expect(restoreOffer(kept({ write: "write-0001" }), loaded("rev-1"))).toEqual({
      kind: "update",
    });
  });

  // A DRAFT IS KEPT FOR SOMEBODY. A session that ended with the builder closed
  // routed the tab to the sign-in, and the next person to sign in was offered
  // the last one's unsaved company edits as their own — a save of which would
  // be recorded as theirs. Whatever else the draft could be carried onto.
  test("a draft kept for another reader is discarded, before anything it could be carried onto", () => {
    const other = loaded("rev-1", PRINT, "edit", "p-2");
    expect(restoreOffer(kept(), other)).toEqual({ kind: "discard_other_reader" });
    expect(restoreOffer(kept({ write: "write-0001" }), other)).toEqual({
      kind: "discard_other_reader",
    });
    expect(restoreOffer(kept(), loaded(null, PRINT, "create", "p-2"))).toEqual({
      kind: "discard_other_reader",
    });
  });

  test("a draft made for another mode is discarded, unless it is a create whose save was out", () => {
    expect(restoreOffer(kept(), loaded(null, PRINT, "create"))).toEqual({
      kind: "discard_mode_changed",
      kept: "edit",
      loaded: "create",
    });
    const create = kept({ mode: "create", baseRevision: null });
    expect(restoreOffer(create, loaded("rev-1"))).toMatchObject({ kind: "discard_mode_changed" });
    expect(restoreOffer({ ...create, write: "write-0001" }, loaded("rev-1"))).toEqual({
      kind: "update",
    });
  });
});

describe("persistencePlan", () => {
  test("keeps a log, and clears when there is none or the tab may have changed hands", () => {
    const ops = everyOperation().slice(0, 2);
    const state = {
      mode: "edit" as const,
      baseRevision: "rev-1",
      basePrint: PRINT,
      log: { ops, undone: [] },
      keep: true,
      pending: null,
      reader: "p-1",
    };
    expect(persistencePlan(state, 42)).toEqual({
      action: "keep",
      kept: kept({ ops, savedAt: 42 }),
    });
    expect(persistencePlan({ ...state, keep: false }, 42)).toEqual({ action: "clear" });
    expect(persistencePlan({ ...state, log: EMPTY_LOG }, 42)).toEqual({ action: "clear" });
    // A save of the log that is out travels with it, and what it creates.
    const creates = [{ key: "new:u1", kind: "unit" as const, address: "legal" }];
    expect(persistencePlan({ ...state, pending: { write: "write-0001", creates } }, 42)).toEqual({
      action: "keep",
      kept: kept({ ops, savedAt: 42, write: "write-0001", creates }),
    });
  });
});

describe("a save of the kept log whose answer may be lost", () => {
  const creates = [{ key: "new:s1", kind: "seat" as const, address: "counsel" }];

  test("is marked on the kept log before it goes, with what it creates, and the mark is cleared once it is known", () => {
    const storage = new MemoryStorage();
    const draft = kept({ ops: everyOperation().slice(0, 2) });
    expect(keepDraft(storage, draft)).toBe("kept");
    expect(markPendingWrite(storage, { write: "write-0001", creates })).toBe("kept");
    expect(restoreDraft(storage)).toEqual({
      kind: "restored",
      kept: { ...draft, write: "write-0001", creates },
    });
    expect(markPendingWrite(storage, null)).toBe("kept");
    expect(restoreDraft(storage)).toEqual({ kind: "restored", kept: draft });
  });

  // A log no storage holds is never offered again, so no lost answer could
  // replay it; storage that refuses says so rather than pretending.
  test("marks nothing where no draft is kept, and says when storage refuses", () => {
    const pending = { write: "write-0001", creates };
    expect(markPendingWrite(new MemoryStorage(), pending)).toBe("cleared");
    expect(markPendingWrite(null, pending)).toBe("unavailable");
    expect(markPendingWrite(new RefusingStorage(), pending)).toBe("refused");
  });

  test("a mark that is not a write id, or a creation that is not one, is not a draft this build kept", () => {
    for (const bad of [
      { write: "not one" },
      { write: "write-0001", creates: [{ key: "seat:x", kind: "seat", address: "x" }] },
      { write: "write-0001", creates: [{ key: "new:x", kind: "role", address: "x" }] },
    ]) {
      const storage = new MemoryStorage();
      storage.setItem(
        DRAFT_STORAGE_KEY,
        JSON.stringify({ ...kept({ ops: everyOperation().slice(0, 1) }), ...bad }),
      );
      expect(restoreDraft(storage), JSON.stringify(bad)).toEqual({ kind: "discarded" });
    }
  });
});
