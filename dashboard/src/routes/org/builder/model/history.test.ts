// @vitest-environment node
/**
 * The log: undo, redo, replay and rebase.
 *
 * What these protect. A log replays to the same draft, with the same keys,
 * however many times and after a trip through JSON (which is how it survives a
 * reload). A rebase onto a newer revision applies what still holds, reports
 * what is gone, and holds every conflict with both values rather than
 * overwriting the other operator's change; an edit to a field this log never
 * touched survives it; and an entity somebody recreated under the same name
 * is never the target of an operation recorded against the original.
 *
 * The random cases come from a seeded PRNG in this file. A failure names its
 * seed, and `SEEDS` replays it.
 */

import { describe, expect, test } from "vitest";
import type { CompanyDocument } from "~/protocol/index.ts";
import { cloneJson } from "./json.ts";
import { COMPANY_KEY, mintKey, type KeySource, type NodeKey } from "./keys.ts";
import { allKeys, allSeats, allUnits, locate, type Draft } from "./draft.ts";
import { fromDocument, toDocument } from "./document.ts";
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
import { fixtureCompany, fixtureDerived, fixtureHandle } from "./testkit.ts";

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

function keyed(doc: CompanyDocument): Draft {
  return fromDocument(doc, fixtureDerived(doc));
}

/** A random intent against a draft, from what the draft holds now. */
function randomIntent(draft: Draft, rand: () => number, keys: KeySource, label: string): Intent {
  const pick = <T>(list: readonly T[]): T | undefined =>
    list.length === 0 ? undefined : list[Math.floor(rand() * list.length)];
  const seats = [...allSeats(draft)].map(({ seat }) => seat);
  const units = [...allUnits(draft)].map(({ unit }) => unit);
  const parents: NodeKey[] = [COMPANY_KEY, ...units.map((u) => u.key)];
  const placementIn = (parent: NodeKey, kind: "seat" | "unit", self?: NodeKey) => {
    const found = parent === COMPANY_KEY ? undefined : locate(draft, parent);
    const list =
      parent === COMPANY_KEY
        ? kind === "seat"
          ? draft.roles
          : draft.units
        : found?.kind === "unit"
          ? kind === "seat"
            ? found.node.roles
            : found.node.children
          : [];
    const options = list.filter((n) => n.key !== self);
    const after = rand() < 0.3 ? null : (pick(options)?.key ?? null);
    return { parent, after };
  };
  const seat = pick(seats);
  const unitNode = pick(units);
  const field = pick(["goal", "backstory", "email"] as const)!;
  switch (Math.floor(rand() * 11)) {
    case 0: {
      const parent = pick(parents)!;
      return {
        type: "addSeat",
        key: mintKey(keys),
        placement: placementIn(parent, "seat"),
        data: { name: `Seat ${label}`, goal: label },
      };
    }
    case 1: {
      const parent = pick(parents)!;
      return {
        type: "addUnit",
        key: mintKey(keys),
        placement: placementIn(parent, "unit"),
        data: { name: `Unit ${label}` },
      };
    }
    case 2:
      return seat
        ? { type: "remove", target: seat.key }
        : { type: "updateCompany", set: [{ path: ["mission"], value: label }] };
    case 3:
      return seat
        ? { type: "renameSeat", target: seat.key, name: `${seat.data.name} ${label}` }
        : { type: "updateCompany", set: [{ path: ["vision"], value: label }] };
    case 4:
      return unitNode
        ? { type: "renameUnit", target: unitNode.key, name: `${unitNode.data.name} ${label}` }
        : { type: "updateCompany", set: [{ path: ["mission"], value: label }] };
    case 5: {
      if (!seat) return { type: "updateCompany", set: [{ path: ["vision"], value: label }] };
      const parent = pick(parents)!;
      return { type: "move", target: seat.key, to: placementIn(parent, "seat", seat.key) };
    }
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
        : { type: "updateCompany", set: [{ path: ["mission"], value: label }] };
    case 7:
      return unitNode
        ? {
            type: "setLead",
            target: unitNode.key,
            ...(rand() < 0.3 || !seat ? {} : { lead: seat.data.name }),
          }
        : { type: "updateCompany", set: [{ path: ["mission"], value: label }] };
    case 8:
      return seat
        ? {
            type: "setManages",
            target: seat.key,
            manages: seats.filter(() => rand() < 0.3).map((s) => s.data.name),
          }
        : { type: "updateCompany", set: [{ path: ["vision"], value: label }] };
    case 9:
      return seat
        ? {
            type: "changeKind",
            target: seat.key,
            kind: seat.data.kind === "human" ? "agent" : "human",
            contact: { slack_user_id: `U${label}` },
          }
        : { type: "updateCompany", set: [{ path: ["mission"], value: label }] };
    default:
      return unitNode
        ? {
            type: "updateUnit",
            target: unitNode.key,
            set: [{ path: ["purpose"], value: `purpose ${label}` }],
          }
        : { type: "updateCompany", set: [{ path: ["vision"], value: label }] };
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
    const intent = randomIntent(draft, rand, keys, `${tag}${attempt}`);
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

describe("undo and redo", () => {
  test("undo returns to the draft before the operation and redo to the one after", () => {
    const base = keyed(fixtureCompany());
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
    const base = keyed(fixtureCompany());
    const { ops } = randomLog(base, 7, 3, "r");
    let log = EMPTY_LOG;
    for (const op of ops) log = pushOperation(log, op);
    log = undoOperation(log)!.log;
    expect(log.undone).toHaveLength(1);
    log = pushOperation(log, ops[0]!);
    expect(log.undone).toEqual([]);
  });
});

describe("replay", () => {
  test("replaying the same operations twice gives identical keys and drafts", () => {
    const base = keyed(fixtureCompany());
    const failures = failuresFor(SEEDS, (seed) => {
      const { ops, drafts } = randomLog(base, seed, 12, "p");
      const first = replay(base, ops).draft;
      const second = replay(keyed(fixtureCompany()), ops).draft;
      if ([...allKeys(first)].join() !== [...allKeys(second)].join()) return "keys differ";
      if (JSON.stringify(first) !== JSON.stringify(second)) return "drafts differ";
      return JSON.stringify(first) === JSON.stringify(drafts[drafts.length - 1])
        ? null
        : "replay differs from recording";
    });
    expect(failures).toEqual([]);
  });

  test("a log that went through JSON replays identically", () => {
    const base = keyed(fixtureCompany());
    const failures = failuresFor(SEEDS, (seed) => {
      const { ops } = randomLog(base, seed, 12, "j");
      const roundTripped = JSON.parse(JSON.stringify({ ops })).ops as Operation[];
      const a = toDocument(replay(base, ops).draft);
      const b = toDocument(replay(base, roundTripped).draft);
      if (JSON.stringify(a.document) !== JSON.stringify(b.document)) return "documents differ";
      return JSON.stringify([...a.index.byPath]) === JSON.stringify([...b.index.byPath])
        ? null
        : "path indexes differ";
    });
    expect(failures).toEqual([]);
  });

  test("an operation that does not apply is a defect and names its position", () => {
    const base = keyed(fixtureCompany());
    const op = record(base, { type: "setLead", target: "unit:Sales", lead: "Account Executive" });
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
  const intentOk = (draft: Draft, intent: Intent): { op: Operation; draft: Draft } => {
    const result = record(draft, intent);
    if (!result.ok) throw new Error(`${intent.type}: ${result.message}`);
    return { op: result.op, draft: apply(draft, result.op).draft };
  };

  test("onto the base it was recorded on, everything applies to the same draft", () => {
    const base = keyed(fixtureCompany());
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

  test("onto a document with a unit inserted before the targets, every key still resolves", () => {
    const doc = fixtureCompany();
    const base = keyed(doc);
    let draft = base;
    const ops: Operation[] = [];
    for (const intent of [
      { type: "updateSeat", target: "seat:sre", set: [{ path: ["goal"], value: "Automate" }] },
      { type: "setLead", target: "unit:Sales", lead: "Account Executive" },
      { type: "move", target: "seat:dev", to: { parent: "unit:Platform", after: "seat:sre" } },
    ] as Intent[]) {
      const next = intentOk(draft, intent);
      ops.push(next.op);
      draft = next.draft;
    }

    const theirs = cloneJson(doc);
    theirs.units!.unshift({ name: "Legal", roles: [{ name: "Counsel" }] });
    const result = rebase(keyed(theirs), ops);
    expect(result.entries.map((e) => e.outcome)).toEqual(["applies", "applies", "applies"]);
    const out = toDocument(result.draft).document;
    expect(out.units!.map((u) => u.name)).toEqual(["Legal", "Engineering", "Sales"]);
    expect(out.units![1]!.children![0]!.roles!.map((r) => [r.name, r.goal])).toEqual([
      ["SRE", "Automate"],
      ["Dev", "Build"],
    ]);
    expect(out.units![2]!.lead).toBe("Account Executive");
  });

  test("a value changed upstream is held as a conflict with both values, then kept mine or theirs", () => {
    const doc = fixtureCompany();
    const { op } = intentOk(keyed(doc), {
      type: "updateSeat",
      target: "seat:dev",
      set: [{ path: ["goal"], value: "Mine" }],
    });
    const theirs = cloneJson(doc);
    theirs.units![0]!.roles![1]!.goal = "Theirs";
    const newBase = keyed(theirs);

    const held = rebase(newBase, [op]);
    expect(held.pending).toBe(1);
    expect(held.entries[0]).toMatchObject({
      outcome: "conflict",
      conflicts: [{ subject: "goal", base: "Build", theirs: "Theirs", mine: "Mine" }],
    });
    expect(toDocument(held.draft).document.units![0]!.roles![1]!.goal).toBe("Theirs");

    const mine = rebase(newBase, [op], new Map([[0, "mine"]]));
    expect(mine.pending).toBe(0);
    expect(toDocument(mine.draft).document.units![0]!.roles![1]!.goal).toBe("Mine");
    expect(mine.ops[0]).toMatchObject({
      changes: [{ path: ["goal"], before: "Theirs", after: "Mine" }],
    });

    const kept = rebase(newBase, [op], new Map([[0, "theirs"]]));
    expect(kept.pending).toBe(0);
    expect(kept.ops).toEqual([]);
    expect(toDocument(kept.draft).document.units![0]!.roles![1]!.goal).toBe("Theirs");
  });

  test("an operation on something removed upstream is gone, and so is one that depended on a held conflict", () => {
    const doc = fixtureCompany();
    let draft = keyed(doc);
    const ops: Operation[] = [];
    for (const intent of [
      {
        type: "updateUnit",
        target: "unit:Sales",
        set: [{ path: ["purpose"], value: "Sell more" }],
      },
      {
        type: "addSeat",
        key: "new:a",
        placement: { parent: "unit:Engineering", after: "seat:dev" },
        data: { name: "Tester" },
      },
      { type: "updateSeat", target: "new:a", set: [{ path: ["goal"], value: "Test" }] },
    ] as Intent[]) {
      const next = intentOk(draft, intent);
      ops.push(next.op);
      draft = next.draft;
    }
    const theirs = cloneJson(doc);
    theirs.units!.splice(1, 1);
    theirs.units![0]!.roles!.splice(1, 1);
    const result = rebase(keyed(theirs), ops);
    expect(result.entries.map((e) => e.outcome)).toEqual(["gone", "conflict", "gone"]);

    const resolved = rebase(keyed(theirs), ops, new Map([[1, "mine"]]));
    expect(resolved.entries.map((e) => e.outcome)).toEqual(["gone", "conflict", "applies"]);
    expect(toDocument(resolved.draft).document.units![0]!.roles!.map((r) => r.name)).toEqual([
      "VP Engineering",
      "Tester",
    ]);
  });

  test("a concurrent edit to a field this log never touched survives", () => {
    const doc = fixtureCompany();
    const base = keyed(doc);
    const failures = failuresFor(SEEDS, (seed) => {
      const theirs = randomLog(base, seed, 6, "t");
      const theirsDoc = toDocument(theirs.drafts[theirs.drafts.length - 1]!).document;
      const mine = randomLog(base, seed + 10_000, 6, "m");
      const newBase = keyed(theirsDoc);
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

  test("an entity recreated upstream under the same name is never modified by an operation on the original", () => {
    const doc = fixtureCompany();
    const base = keyed(doc);
    const failures = failuresFor(SEEDS, (seed) => {
      const rand = prng(seed + 20_000);
      const seats = [...allSeats(base)].map(({ seat, parent }) => ({ seat, parent }));
      const victim = seats[Math.floor(rand() * seats.length)]!;
      const mine = randomLog(base, seed, 10, "c");

      // Upstream removes the seat and creates a different one under its name:
      // a different seat, so the engine gives it a different handle.
      const removed = record(base, {
        type: "remove",
        target: victim.seat.key,
        placedSeats: "keep",
      });
      if (!removed.ok) return `could not remove ${victim.seat.key}`;
      let upstream = apply(base, removed.op).draft;
      const parent = locate(upstream, victim.parent) ? victim.parent : COMPANY_KEY;
      const recreatedData = {
        name: victim.seat.data.name,
        handle: `${fixtureHandle(victim.seat.data)}-recreated`,
        goal: "Recreated",
      };
      const added = record(upstream, {
        type: "addSeat",
        key: "new:recreated",
        placement: { parent, after: null },
        data: recreatedData,
      });
      if (!added.ok) return `could not recreate: ${added.message}`;
      upstream = apply(upstream, added.op).draft;
      const theirsDoc = toDocument(upstream).document;
      const newBase = keyed(theirsDoc);
      const recreatedKey = `seat:${recreatedData.handle}`;
      if (locate(newBase, recreatedKey)?.kind !== "seat")
        return "the recreated seat is not keyed by its own handle";

      for (const choice of ["mine", "theirs"] as const) {
        const rebased = rebase(newBase, mine.ops, new Map(mine.ops.map((_, i) => [i, choice])));
        for (const op of rebased.ops) {
          if ("target" in op && op.target === recreatedKey)
            return `${op.type} was applied to the recreated seat`;
        }
        const found = locate(rebased.draft, recreatedKey);
        if (found?.kind === "seat") {
          const data = found.node.data;
          if (
            data.goal !== "Recreated" ||
            data.handle !== recreatedData.handle ||
            data.name !== recreatedData.name
          ) {
            return `the recreated seat changed under "${choice}": ${JSON.stringify(data)}`;
          }
        }
      }
      return null;
    });
    expect(failures).toEqual([]);
  });

  test("never throws, and an adopted rebase replays to its own draft", () => {
    const base = keyed(fixtureCompany());
    const failures = failuresFor(SEEDS, (seed) => {
      const theirs = randomLog(base, seed, 8, "x");
      const mine = randomLog(base, seed + 50_000, 8, "y");
      const newBase = keyed(toDocument(theirs.drafts[theirs.drafts.length - 1]!).document);
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
