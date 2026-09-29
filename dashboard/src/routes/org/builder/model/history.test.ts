// @vitest-environment node
/**
 * The log: undo, redo, replay and rebase.
 *
 * What these protect. A log replays to the same draft, with the same keys,
 * however many times and after a trip through JSON (which is how it survives a
 * reload). A rebase onto a newer chart applies what still holds, reports what
 * is gone, and holds every conflict with both values rather than overwriting
 * the other operator's change; an edit to a field this log never touched
 * survives it; an operation follows its seat through a rename somebody else
 * made; and a seat somebody created under an address another seat used to
 * answer to is never the target of an operation recorded against that other.
 *
 * The random cases come from a seeded PRNG in this file. A failure names its
 * seed, and `SEEDS` replays it.
 */

import { describe, expect, test } from "vitest";
import { COMPANY_KEY, mintKey, type KeySource, type NodeKey } from "./keys.ts";
import { allKeys, allSeats, allUnits, locate, type Draft } from "./draft.ts";
import { fromChart } from "./document.ts";
import { apply, record, type Intent, type Operation } from "./operations.ts";
import {
  EMPTY_LOG,
  pushOperation,
  rebase,
  redoOperation,
  replay,
  undoOperation,
  ReplayError,
} from "./history.ts";
import { chartOfDraft, fixtureChart, fixtureSettings } from "./testkit.ts";

/** mulberry32: a seeded PRNG, so a failing property names the run that found it. */
function prng(seed: number): () => number {
  let s = seed >>> 0;
  return () => {
    s = (s + 0x6d2b79f5) >>> 0;
    let t = s;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

const SEEDS = Array.from({ length: 150 }, (_, i) => i + 1);

const fixture = (): Draft => fromChart(fixtureSettings(), fixtureChart());

/** What the chart holds once a draft is saved, read back: the next base. */
const savedAs = (draft: Draft): Draft => fromChart(draft.company, chartOfDraft(draft));

/** A random intent against a draft, from what the draft holds now. */
function randomIntent(draft: Draft, rand: () => number, keys: KeySource, label: string): Intent {
  const pick = <T>(list: readonly T[]): T | undefined =>
    list.length === 0 ? undefined : list[Math.floor(rand() * list.length)];
  const seats = [...allSeats(draft)].map(({ seat }) => seat);
  const units = [...allUnits(draft)].map(({ unit }) => unit);
  const parents: NodeKey[] = [COMPANY_KEY, ...units.map((u) => u.key)];
  const seat = pick(seats);
  const unitNode = pick(units);
  const field = pick(["goal", "backstory", "email"] as const)!;
  const charter = (f: "mission" | "vision"): Intent => ({
    type: "updateCompany",
    set: [{ path: [f], value: label }],
  });
  switch (Math.floor(rand() * 12)) {
    case 0:
      return {
        type: "addSeat",
        key: mintKey(keys),
        placement: { parent: pick(parents)! },
        data: { handle: `s-${label}`, name: `Seat ${label}`, goal: label },
      };
    case 1:
      return {
        type: "addUnit",
        key: mintKey(keys),
        placement: { parent: pick(parents)! },
        data: { key: `u-${label}`, name: `Unit ${label}` },
      };
    case 2:
      return seat ? { type: "remove", target: seat.key } : charter("mission");
    case 3:
      return seat
        ? { type: "renameSeat", target: seat.key, name: `${seat.data.name} ${label}` }
        : charter("vision");
    case 4:
      return unitNode
        ? { type: "renameUnit", target: unitNode.key, name: `${unitNode.data.name} ${label}` }
        : charter("mission");
    case 5:
      return seat
        ? { type: "move", target: seat.key, to: { parent: pick(parents)! } }
        : charter("vision");
    case 6:
      // An editor submits the whole form: the field the operator changed, and
      // the others at the values the form opened with. Only the first may
      // reach the operation.
      return seat
        ? {
            type: "updateSeat",
            target: seat.key,
            set: (["goal", "backstory", "email"] as const).map((f) =>
              f === field
                ? { path: [f], value: rand() < 0.2 ? undefined : `${f} ${label}` }
                : { path: [f], value: seat.data[f] },
            ),
          }
        : charter("mission");
    case 7:
      return unitNode
        ? {
            type: "setLead",
            target: unitNode.key,
            ...(rand() < 0.3 || !seat ? {} : { lead: seat.data.handle }),
          }
        : charter("mission");
    case 8:
      return seat
        ? {
            type: "setManages",
            target: seat.key,
            manages: seats.filter(() => rand() < 0.3).map((s) => s.data.handle),
          }
        : charter("vision");
    case 9:
      return seat
        ? {
            type: "changeKind",
            target: seat.key,
            kind: seat.data.kind === "human" ? "agent" : "human",
            contact: { slack_user_id: `U${label}` },
          }
        : charter("mission");
    case 10:
      // A new address: the chart's rename, which keeps the seat.
      return seat
        ? { type: "updateSeat", target: seat.key, set: [{ path: ["handle"], value: `h-${label}` }] }
        : charter("vision");
    default:
      return unitNode
        ? {
            type: "updateUnit",
            target: unitNode.key,
            set: [{ path: ["purpose"], value: `purpose ${label}` }],
          }
        : charter("vision");
  }
}

/**
 * Records and applies `count` random operations, returning the log, each draft
 * along the way, and which seat fields the operator INTENDED to change (read
 * from the intents, not from the operations recorded from them, so a recording
 * that captured more than was asked cannot hide itself).
 */
function randomLog(
  base: Draft,
  seed: number,
  count: number,
  tag: string,
): { ops: Operation[]; drafts: Draft[]; intended: Set<string> } {
  const rand = prng(seed);
  let n = 0;
  const keys: KeySource = { next: () => `${tag}${seed}x${++n}` };
  const ops: Operation[] = [];
  const drafts: Draft[] = [base];
  const intended = new Set<string>();
  let draft = base;
  for (let attempt = 0; ops.length < count && attempt < count * 6; attempt++) {
    const intent = randomIntent(draft, rand, keys, `${tag}${seed}-${attempt}`);
    const result = record(draft, intent);
    if (!result.ok) continue;
    if (intent.type === "updateSeat") {
      const current = locate(draft, intent.target);
      for (const set of intent.set) {
        const was = current?.kind === "seat" ? current.node.data[set.path[0]!] : undefined;
        if (JSON.stringify(was) !== JSON.stringify(set.value))
          intended.add(JSON.stringify([intent.target, set.path[0]]));
      }
    }
    draft = apply(draft, result.op).draft;
    ops.push(result.op);
    drafts.push(draft);
  }
  return { ops, drafts, intended };
}

function failuresFor(seeds: readonly number[], check: (seed: number) => string | null): string[] {
  const failures: string[] = [];
  for (const seed of seeds) {
    try {
      const failure = check(seed);
      if (failure) failures.push(`seed ${seed}: ${failure}`);
    } catch (err) {
      failures.push(`seed ${seed}: threw ${err instanceof Error ? err.message : String(err)}`);
    }
  }
  return failures;
}

const intentOk = (draft: Draft, intent: Intent): { op: Operation; draft: Draft } => {
  const result = record(draft, intent);
  if (!result.ok) throw new Error(`${intent.type}: ${result.message}`);
  return { op: result.op, draft: apply(draft, result.op).draft };
};

function logOf(base: Draft, intents: readonly Intent[]): { ops: Operation[]; draft: Draft } {
  let draft = base;
  const ops: Operation[] = [];
  for (const intent of intents) {
    const next = intentOk(draft, intent);
    ops.push(next.op);
    draft = next.draft;
  }
  return { ops, draft };
}

const seatData = (draft: Draft, key: NodeKey) => {
  const found = locate(draft, key);
  return found?.kind === "seat" ? found.node.data : undefined;
};

describe("undo and redo", () => {
  test("undo returns to the draft before the operation and redo to the one after", () => {
    const base = fixture();
    const failures = failuresFor(SEEDS.slice(0, 60), (seed) => {
      const { ops, drafts } = randomLog(base, seed, 8, "u");
      let log = EMPTY_LOG;
      for (const op of ops) log = pushOperation(log, op);
      for (let i = ops.length; i > 0; i--) {
        const undone = undoOperation(log)!;
        log = undone.log;
        if (JSON.stringify(replay(base, log.ops).draft) !== JSON.stringify(drafts[i - 1]))
          return `undo to ${i - 1} differs`;
      }
      if (undoOperation(log) !== undefined) return "undid past the base";
      let draft = base;
      for (let i = 0; i < ops.length; i++) {
        const redone = redoOperation(log)!;
        log = redone.log;
        draft = apply(draft, redone.op).draft;
        if (JSON.stringify(draft) !== JSON.stringify(drafts[i + 1]))
          return `redo to ${i + 1} differs`;
      }
      return redoOperation(log) === undefined ? null : "redid past the end";
    });
    expect(failures).toEqual([]);
  });

  test("recording after an undo forgets what was undone", () => {
    const { ops } = randomLog(fixture(), 7, 3, "r");
    let log = EMPTY_LOG;
    for (const op of ops) log = pushOperation(log, op);
    log = undoOperation(log)!.log;
    expect(log.undone).toHaveLength(1);
    log = pushOperation(log, ops[0]!);
    expect(log.undone).toEqual([]);
  });
});

describe("replay", () => {
  test("replaying the same operations twice gives identical keys and drafts, and survives JSON", () => {
    const base = fixture();
    const failures = failuresFor(SEEDS, (seed) => {
      const { ops, drafts } = randomLog(base, seed, 12, "p");
      const first = replay(base, ops).draft;
      const second = replay(fixture(), ops).draft;
      if ([...allKeys(first)].join() !== [...allKeys(second)].join()) return "keys differ";
      if (JSON.stringify(first) !== JSON.stringify(second)) return "drafts differ";
      const roundTripped = JSON.parse(JSON.stringify({ ops })).ops as Operation[];
      if (JSON.stringify(replay(base, roundTripped).draft) !== JSON.stringify(first))
        return "a log that went through JSON replays differently";
      return JSON.stringify(first) === JSON.stringify(drafts[drafts.length - 1])
        ? null
        : "replay differs from recording";
    });
    expect(failures).toEqual([]);
  });

  test("an operation that does not apply is a defect and names its position", () => {
    const base = fixture();
    const op = record(base, { type: "setLead", target: "unit:sales", lead: "account-executive" });
    if (!op.ok) throw new Error(op.message);
    expect(() => replay(base, [op.op, op.op])).toThrow(ReplayError);
    try {
      replay(base, [op.op, op.op]);
    } catch (err) {
      expect((err as ReplayError).index).toBe(1);
    }
  });
});

describe("rebase", () => {
  test("onto the base it was recorded on, everything applies to the same draft", () => {
    const base = fixture();
    const failures = failuresFor(SEEDS.slice(0, 60), (seed) => {
      const { ops, drafts } = randomLog(base, seed, 10, "s");
      const result = rebase(base, ops);
      if (result.pending !== 0 || result.entries.some((e) => e.outcome !== "applies"))
        return "an entry did not apply";
      return JSON.stringify(result.draft) === JSON.stringify(drafts[drafts.length - 1])
        ? null
        : "draft differs";
    });
    expect(failures).toEqual([]);
  });

  test("onto a chart that gained a unit, every key still resolves", () => {
    const base = fixture();
    const { ops } = logOf(base, [
      { type: "updateSeat", target: "seat:sre", set: [{ path: ["goal"], value: "Automate" }] },
      { type: "setLead", target: "unit:sales", lead: "account-executive" },
      { type: "move", target: "seat:dev", to: { parent: "unit:platform" } },
    ]);
    const theirs = logOf(base, [
      {
        type: "addUnit",
        key: "new:l",
        placement: { parent: COMPANY_KEY },
        data: { key: "legal", name: "Legal" },
      },
    ]).draft;
    const result = rebase(savedAs(theirs), ops);
    expect(result.entries.map((e) => e.outcome)).toEqual(["applies", "applies", "applies"]);
    expect(result.draft.units.map((u) => u.data.key)).toEqual(["engineering", "legal", "sales"]);
    expect(seatData(result.draft, "seat:sre")?.goal).toBe("Automate");
    expect(locate(result.draft, "seat:dev")?.parent).toBe("unit:platform");
  });

  test("a value changed upstream is held as a conflict with both values, then kept mine or theirs", () => {
    const base = fixture();
    const { ops } = logOf(base, [
      { type: "updateSeat", target: "seat:dev", set: [{ path: ["goal"], value: "Mine" }] },
    ]);
    const newBase = savedAs(
      logOf(base, [
        { type: "updateSeat", target: "seat:dev", set: [{ path: ["goal"], value: "Theirs" }] },
      ]).draft,
    );

    const held = rebase(newBase, ops);
    expect(held.pending).toBe(1);
    expect(held.entries[0]).toMatchObject({
      outcome: "conflict",
      conflicts: [{ subject: "goal", base: "Build", theirs: "Theirs", mine: "Mine" }],
    });
    expect(seatData(held.draft, "seat:dev")?.goal).toBe("Theirs");

    const mine = rebase(newBase, ops, new Map([[0, "mine"]]));
    expect(mine.pending).toBe(0);
    expect(seatData(mine.draft, "seat:dev")?.goal).toBe("Mine");
    expect(mine.ops[0]).toMatchObject({
      changes: [{ path: ["goal"], before: "Theirs", after: "Mine" }],
    });

    const kept = rebase(newBase, ops, new Map([[0, "theirs"]]));
    expect(kept.pending).toBe(0);
    expect(kept.ops).toEqual([]);
    expect(seatData(kept.draft, "seat:dev")?.goal).toBe("Theirs");
  });

  test("the base already holding this operation's own value is not a conflict", () => {
    const base = fixture();
    const change: Intent = {
      type: "updateSeat",
      target: "seat:dev",
      set: [{ path: ["goal"], value: "Ship" }],
    };
    const { ops } = logOf(base, [change]);
    const result = rebase(savedAs(logOf(base, [change]).draft), ops);
    expect(result.pending).toBe(0);
    expect(result.entries.map((e) => e.outcome)).toEqual(["already"]);
  });

  test("an operation on something removed upstream is gone, and so is one that depended on a held conflict", () => {
    const base = fixture();
    const { ops } = logOf(base, [
      {
        type: "updateUnit",
        target: "unit:sales",
        set: [{ path: ["purpose"], value: "Sell more" }],
      },
      {
        type: "addSeat",
        key: "new:a",
        placement: { parent: "unit:engineering" },
        data: { handle: "tester", name: "Tester" },
      },
      { type: "updateSeat", target: "new:a", set: [{ path: ["goal"], value: "Test" }] },
    ]);
    // Upstream removed Sales and created a seat of its own under `tester`.
    const theirs = savedAs(
      logOf(base, [
        { type: "remove", target: "unit:sales" },
        {
          type: "addSeat",
          key: "new:t",
          placement: { parent: COMPANY_KEY },
          data: { handle: "tester", name: "Their tester" },
        },
      ]).draft,
    );
    const result = rebase(theirs, ops);
    expect(result.entries.map((e) => e.outcome)).toEqual(["gone", "conflict", "gone"]);

    // Keep mine of the add writes this draft's seat over theirs, and the edit
    // of the seat this draft created lands on it.
    const resolved = rebase(theirs, ops, new Map([[1, "mine"]]));
    expect(resolved.entries.map((e) => e.outcome)).toEqual(["gone", "conflict", "applies"]);
    expect(seatData(resolved.draft, "seat:tester")).toMatchObject({ name: "Tester", goal: "Test" });
    expect(locate(resolved.draft, "seat:tester")?.parent).toBe("unit:engineering");
    expect(resolved.rekeyed.get("new:a")).toBe("seat:tester");
  });

  test("an operation follows its seat through a rename somebody else made", () => {
    const base = fixture();
    const { ops } = logOf(base, [
      { type: "updateSeat", target: "seat:dev", set: [{ path: ["goal"], value: "Ship" }] },
    ]);
    const renamed = savedAs(
      logOf(base, [
        { type: "updateSeat", target: "seat:dev", set: [{ path: ["handle"], value: "developer" }] },
      ]).draft,
    );
    const result = rebase(renamed, ops);
    expect(result.entries.map((e) => e.outcome)).toEqual(["applies"]);
    expect(seatData(result.draft, "seat:dev")).toMatchObject({ handle: "developer", goal: "Ship" });
  });

  // THE CHART LETS A NEW SEAT TAKE A RETIRED ALIAS, and a draft keyed by address
  // would then carry an edit of the renamed seat onto the newcomer. Keyed by
  // identity, the newcomer is another node however it is addressed.
  test("a seat created under an address another seat used to answer to never receives that seat's operations", () => {
    // `zed` was created as `dev` and renamed; the draft holds it under `seat:dev`.
    const base = savedAs(
      logOf(fixture(), [
        { type: "updateSeat", target: "seat:dev", set: [{ path: ["handle"], value: "zed" }] },
      ]).draft,
    );
    const { ops } = logOf(base, [
      { type: "updateSeat", target: "seat:dev", set: [{ path: ["goal"], value: "Mine" }] },
      { type: "remove", target: "seat:dev" },
    ]);
    // Upstream renamed it again and gave `zed` to a new seat.
    const theirs = savedAs(
      logOf(base, [
        { type: "updateSeat", target: "seat:dev", set: [{ path: ["handle"], value: "yan" }] },
        {
          type: "addSeat",
          key: "new:z",
          placement: { parent: COMPANY_KEY },
          data: { handle: "zed", name: "Newcomer", goal: "Theirs" },
        },
      ]).draft,
    );
    for (const choice of ["mine", "theirs"] as const) {
      const rebased = rebase(theirs, ops, new Map(ops.map((_, i) => [i, choice])));
      const newcomer = seatData(rebased.draft, "seat:zed");
      expect(newcomer, choice).toMatchObject({ handle: "zed", name: "Newcomer", goal: "Theirs" });
      for (const op of rebased.ops) {
        expect("target" in op && op.target, `${op.type} under ${choice}`).not.toBe("seat:zed");
      }
    }
    // Control: the renamed seat itself is what they reach — the edit applies to
    // it, and the removal is held because the seat it removes changed since.
    const applied = rebase(theirs, ops);
    expect(applied.entries.map((e) => e.outcome)).toEqual(["applies", "conflict"]);
    expect(seatData(applied.draft, "seat:dev")).toMatchObject({ handle: "yan", goal: "Mine" });
    const removed = rebase(theirs, ops, new Map([[1, "mine"]]));
    expect(locate(removed.draft, "seat:dev")).toBeUndefined();
    expect(seatData(removed.draft, "seat:zed")?.name).toBe("Newcomer");
  });

  test("a concurrent edit to a field this log never touched survives", () => {
    const base = fixture();
    const failures = failuresFor(SEEDS, (seed) => {
      const theirs = randomLog(base, seed, 6, "t");
      const mine = randomLog(base, seed + 10_000, 6, "m");
      const newBase = savedAs(theirs.drafts[theirs.drafts.length - 1]!);
      // Keep mine on every conflict: the harshest choice for the other
      // operator's work, and still it may win only where this operator
      // actually changed something.
      const rebased = rebase(
        newBase,
        mine.ops,
        new Map(mine.ops.map((_, i) => [i, "mine" as const])),
      );
      for (const { seat } of allSeats(rebased.draft)) {
        const upstream = locate(newBase, seat.key);
        if (upstream?.kind !== "seat") continue;
        for (const field of ["goal", "backstory", "email"]) {
          if (mine.intended.has(JSON.stringify([seat.key, field]))) continue;
          if (JSON.stringify(seat.data[field]) !== JSON.stringify(upstream.node.data[field])) {
            return `${seat.key}.${field} lost the upstream value`;
          }
        }
      }
      return null;
    });
    expect(failures).toEqual([]);
  });

  test("never throws, and an adopted rebase replays to its own draft", () => {
    const base = fixture();
    const failures = failuresFor(SEEDS, (seed) => {
      const theirs = randomLog(base, seed, 8, "x");
      const mine = randomLog(base, seed + 50_000, 8, "y");
      const newBase = savedAs(theirs.drafts[theirs.drafts.length - 1]!);
      const choices = new Map(
        mine.ops.map((_, i) => [i, seed % 2 === 0 ? ("mine" as const) : ("theirs" as const)]),
      );
      const result = rebase(newBase, mine.ops, choices);
      if (result.pending !== 0) return "a chosen conflict is still pending";
      if (result.reports.length !== result.ops.length)
        return "reports are not aligned with operations";
      return JSON.stringify(replay(newBase, result.ops).draft) === JSON.stringify(result.draft)
        ? null
        : "adopted log replays differently";
    });
    expect(failures).toEqual([]);
  });
});
