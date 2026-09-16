// @vitest-environment node

/**
 * The org index every screen consumes, and the seat-state rules.
 *
 * The index is built from what the ENGINE derived, never from a second
 * implementation of its rules. So these cases are not about manager
 * resolution or lead inheritance, which are Go's to test: they are about
 * laying the engine's answer over the authored tree faithfully, and about
 * saying "unknown" rather than guessing when the answer is missing or does not
 * describe the tree it arrived with.
 */

import { describe, expect, test } from "vitest";
import {
  indexOrg,
  runState,
  seatPath,
  seatSettings,
  seatTone,
  statusLine,
  staleness,
  STALE_MS,
  STALLED_MS,
} from "./seats.ts";
import type {
  CompanyDocument,
  Derived,
  DerivedSeat,
  DerivedUnit,
  OrgProjection,
  OrgSeat,
  OrgUnit,
  SandboxEntry,
} from "~/protocol/index.ts";

/**
 * The anonymous projection for a small company, exactly as `internal/api`'s
 * `OrgProjection` writes it: authored positions, no credentials, and the
 * public `derived` block with its paths omitted.
 *
 * `Designer` is a ROOT seat in the document whose `unit: Backend` the engine
 * honoured, which is why the derived block has it in Backend and the
 * projection, which does not carry `unit`, has it at the root. `İlker Demir`
 * is here for his handle: the engine derives `ilker-demir`, which no
 * JavaScript lowercasing of the name produces.
 */
const seat = (over: Partial<DerivedSeat> & Pick<DerivedSeat, "handle" | "name">): DerivedSeat => ({
  kind: "agent",
  placed_by_ref: false,
  manager: "",
  managers: null,
  reports: null,
  auto_reports: null,
  onboarding_chain: null,
  ...over,
});

const derived: Derived = {
  seats: [
    seat({ handle: "jane-founder", name: "Jane Founder", kind: "human", reports: ["ceo"] }),
    seat({
      handle: "ceo",
      name: "CEO",
      manager: "jane-founder",
      managers: ["jane-founder"],
      reports: ["vpe"],
    }),
    seat({ handle: "ilker-demir", name: "İlker Demir", manager: "ceo", managers: ["ceo"] }),
    seat({
      handle: "vpe",
      name: "VP Engineering",
      manager: "ceo",
      managers: ["ceo"],
      reports: ["dev-a", "dev-b", "designer"],
      auto_reports: ["dev-a", "dev-b", "designer"],
      onboarding_chain: ["Engineering"],
    }),
    seat({
      handle: "dev-a",
      name: "Dev A",
      manager: "vpe",
      managers: ["vpe"],
      onboarding_chain: ["Engineering", "Backend"],
    }),
    seat({ handle: "dev-b", name: "Dev B", manager: "vpe", managers: ["vpe"] }),
    seat({
      handle: "designer",
      name: "Designer",
      placed_by_ref: true,
      manager: "vpe",
      managers: ["vpe"],
    }),
  ],
  units: [
    {
      name: "Engineering",
      type: "department",
      lead: "vpe",
      lead_inherited: false,
      channel: "eng",
      channel_inherited: false,
      seats: ["vpe"],
    },
    {
      name: "Backend",
      type: "team",
      lead: "vpe",
      lead_inherited: true,
      channel: "eng",
      channel_inherited: true,
      seats: ["dev-a", "dev-b", "designer"],
    },
  ],
};

const org: OrgProjection = {
  name: "Acme",
  roles: [
    { name: "Jane Founder", kind: "human", manages: ["CEO"], availability: "weekdays" },
    { name: "CEO", handle: "ceo", manages: ["VP Engineering"] },
    { name: "İlker Demir", goal: "Keep the books" },
    { name: "Designer" },
  ],
  units: [
    {
      name: "Engineering",
      type: "department",
      lead: "VP Engineering",
      channel: "eng",
      knowledge: ["Engineering handbook"],
      roles: [{ name: "VP Engineering", handle: "vpe" }],
      children: [{ name: "Backend", roles: [{ name: "Dev A" }, { name: "Dev B" }] }],
    },
  ],
  derived,
};

const index = indexOrg(org);
const byName = (name: string) => index.byName.get(name)!;

describe("the engine's derived hierarchy, laid over the authored tree", () => {
  test("every seat is indexed once, in the engine's own order", () => {
    expect(index.hierarchy).toBe(true);
    expect(index.seats.map((s) => s.name)).toEqual([
      "Jane Founder",
      "CEO",
      "İlker Demir",
      "VP Engineering",
      "Dev A",
      "Dev B",
      "Designer",
    ]);
  });

  test("a handle is the one the engine reports, never one derived here", () => {
    // A JavaScript lowercasing of "İlker" emits a dotted i the engine's does
    // not, and the handle keys the seat's memory: a client-derived handle is
    // a seat that loses everything it remembered the first time it is saved.
    expect(byName("İlker Demir").handle).toBe("ilker-demir");
    expect(index.byHandle.get("ilker-demir")?.name).toBe("İlker Demir");
    expect(byName("Dev A").handle).toBe("dev-a");
  });

  test("a root seat the engine moved into its unit sits in that unit, and says why", () => {
    const designer = byName("Designer");
    expect(designer.unit?.name).toBe("Backend");
    expect(designer.placedByRef).toBe(true);
    expect(index.rootSeats.map((s) => s.name)).toEqual(["Jane Founder", "CEO", "İlker Demir"]);
    expect(index.units[1]?.seats.map((s) => s.name)).toEqual(["Dev A", "Dev B", "Designer"]);
  });

  test("an inherited lead is the engine's, and is marked as inherited", () => {
    // It behaves identically to an explicit lead everywhere in the engine, so
    // hiding the difference is how an operator comes to think a unit is
    // unmanaged.
    const [engineering, backend] = index.units;
    expect(engineering?.lead?.name).toBe("VP Engineering");
    expect(engineering?.leadInherited).toBe(false);
    expect(backend?.lead?.name).toBe("VP Engineering");
    expect(backend?.leadInherited).toBe(true);
    expect(backend?.declaredLead).toBe("");
    expect(backend?.type).toBe("team");
    expect(backend?.channelInherited).toBe(true);
  });

  test("the unit chain follows the tree, outermost first", () => {
    expect(byName("Dev A").unitChain.map((u) => u.name)).toEqual(["Engineering", "Backend"]);
    expect(index.topUnits.map((u) => u.name)).toEqual(["Engineering"]);
    expect(index.units[1]?.parent?.name).toBe("Engineering");
  });

  test("reporting lines are the engine's, resolved to seats", () => {
    expect(byName("Dev A").manager?.name).toBe("VP Engineering");
    expect(byName("CEO").manager?.name).toBe("Jane Founder");
    expect(byName("Jane Founder").manager).toBeNull();
    expect(byName("VP Engineering").reports.map((s) => s.name)).toEqual([
      "Dev A",
      "Dev B",
      "Designer",
    ]);
    expect(byName("VP Engineering").autoReports.length).toBe(3);
  });

  test("what the document writes is kept as written", () => {
    // `manages` unexpanded: the expansion is the engine's, above.
    expect(byName("Jane Founder").manages).toEqual(["CEO"]);
    expect(byName("Jane Founder").kind).toBe("human");
    expect(index.units[0]?.knowledge).toEqual(["Engineering handbook"]);
  });

  test("a seat's page is addressed by its handle", () => {
    expect(seatPath(byName("İlker Demir"))).toEqual(["seats", "ilker-demir"]);
  });
});

describe("an engine that sends no derived hierarchy", () => {
  const { derived: _omitted, ...older } = org;
  const bare = indexOrg(older);

  test("the seats are still listed, where the document wrote them", () => {
    expect(bare.hierarchy).toBe(false);
    expect(bare.seats.map((s) => s.name).sort()).toEqual(
      ["CEO", "Designer", "Dev A", "Dev B", "Jane Founder", "VP Engineering", "İlker Demir"].sort(),
    );
    // NOT moved: whether the engine honoured `unit:` is its conclusion.
    expect(bare.byName.get("Designer")?.unit).toBeNull();
    expect(bare.byName.get("Designer")?.placedByRef).toBe(false);
  });

  test("a handle is known only where the document declares one", () => {
    expect(bare.byName.get("CEO")?.handle).toBe("ceo");
    expect(bare.byName.get("İlker Demir")?.handle).toBe("");
    // And its page is still reachable, by name.
    expect(seatPath(bare.byName.get("İlker Demir")!)).toEqual(["seats", "İlker Demir"]);
    // Keys stay unique for the seats nobody declared a handle for.
    expect(new Set(bare.seats.map((s) => s.key)).size).toBe(bare.seats.length);
  });

  test("a position key never collides with a declared handle", () => {
    // `s1` is a valid handle, and it used to be the second seat's position key
    // as well: two React siblings, and two DOM ids, with one key.
    const clash = indexOrg({
      roles: [{ name: "First", handle: "s1" }, { name: "Second" }, { name: "Third", handle: "s1" }],
    });
    expect(new Set(clash.seats.map((s) => s.key)).size).toBe(3);
    expect(clash.byName.get("First")?.key).toBe("s1");
  });

  test("nothing only the engine could conclude is invented", () => {
    for (const s of bare.seats) {
      expect(s.manager).toBeNull();
      expect(s.reports).toEqual([]);
    }
    // A declared lead is a fact of the document; an inherited one is not.
    expect(bare.units[0]?.lead?.name).toBe("VP Engineering");
    expect(bare.units[1]?.lead).toBeNull();
    expect(bare.units[1]?.leadInherited).toBe(false);
  });
});

describe("a derived block that does not describe its tree is not believed", () => {
  const variants: [string, (d: Derived) => Derived][] = [
    ["a unit missing", (d) => ({ ...d, units: d.units!.slice(1) })],
    [
      "a unit renamed",
      (d) => ({ ...d, units: d.units!.map((u, i) => (i === 1 ? { ...u, name: "Frontend" } : u)) }),
    ],
    ["a seat missing", (d) => ({ ...d, seats: d.seats!.slice(1) })],
    [
      "a seat the tree does not have",
      (d) => ({ ...d, seats: d.seats!.map((s, i) => (i === 0 ? { ...s, name: "Nobody" } : s)) }),
    ],
    [
      "a manager that is no seat",
      (d) => ({ ...d, seats: d.seats!.map((s, i) => (i === 1 ? { ...s, manager: "ghost" } : s)) }),
    ],
    [
      "a seat in two units",
      (d) => ({ ...d, units: d.units!.map((u) => ({ ...u, seats: ["vpe"] })) }),
    ],
    [
      "a repeated handle",
      (d) => ({ ...d, seats: d.seats!.map((s, i) => (i === 2 ? { ...s, handle: "ceo" } : s)) }),
    ],
  ];

  test.each(variants)("%s", (_name, mutate) => {
    const broken = indexOrg({ ...org, derived: mutate(derived) });
    expect(broken.hierarchy).toBe(false);
    // Every seat is still there once, from the authored tree.
    expect(broken.seats.length).toBe(7);
  });

  test("a repeated name pairs by the handle it declares, never by position", () => {
    // Only a revision stored before names had to be unique can hold this. The
    // engine lists the root "Designer" AFTER Backend's own "Designer", since
    // it moved the root one into the unit, which is the reverse of the
    // document's order: paired in turn, each would carry the other's goal.
    const repeated: OrgProjection = {
      roles: [{ name: "Designer", handle: "designer-root", goal: "Root goal" }],
      units: [
        {
          name: "Backend",
          roles: [{ name: "Designer", handle: "designer-unit", goal: "Unit goal" }],
        },
      ],
      derived: {
        seats: [
          seat({ handle: "designer-unit", name: "Designer" }),
          seat({ handle: "designer-root", name: "Designer", placed_by_ref: true }),
        ],
        units: [
          {
            name: "Backend",
            type: "team",
            lead: "",
            lead_inherited: false,
            channel: "",
            channel_inherited: false,
            seats: ["designer-unit", "designer-root"],
          },
        ],
      },
    };
    const built = indexOrg(repeated);
    expect(built.hierarchy).toBe(true);
    expect(built.byHandle.get("designer-root")?.goal).toBe("Root goal");
    expect(built.byHandle.get("designer-root")?.placedByRef).toBe(true);
    expect(built.byHandle.get("designer-unit")?.goal).toBe("Unit goal");

    // A handle the tree does not declare for that name is not this tree.
    const foreign = indexOrg({
      ...repeated,
      derived: {
        ...repeated.derived!,
        seats: [
          seat({ handle: "designer-unit", name: "Designer" }),
          seat({ handle: "someone-else", name: "Designer" }),
        ],
        units: [{ ...repeated.derived!.units![0]!, seats: ["designer-unit", "someone-else"] }],
      },
    });
    expect(foreign.hierarchy).toBe(false);
  });

  test("null lists are empty lists, as Go marshals a nil slice", () => {
    const empty = indexOrg({ name: "Blank", derived: { seats: null, units: null } });
    expect(empty.hierarchy).toBe(true);
    expect(empty.seats).toEqual([]);
  });
});

describe("the operator-gated half of a seat", () => {
  const doc: CompanyDocument = {
    name: "Acme",
    roles: [{ name: "CEO", email: "ceo@example.com", llm: { default: "fast", review: "big" } }],
    units: [
      {
        name: "Engineering",
        roles: [{ name: "VP Engineering" }],
        children: [
          {
            name: "Backend",
            mcp_env: { github: { GITHUB_TOKEN: "${BACKEND_TOKEN}" } },
            roles: [{ name: "Dev A", mcp_env: { tracker: { TOKEN: "__redacted__" } } }],
          },
        ],
      },
    ],
  };

  test("a seat's settings are its own entry, with its home unit beside it", () => {
    const found = seatSettings(doc, byName("Dev A"));
    expect(found.state).toBe("found");
    if (found.state !== "found") return;
    expect(found.role.mcp_env?.tracker?.TOKEN).toBe("__redacted__");
    expect(found.unit?.name).toBe("Backend");
    const ceo = seatSettings(doc, byName("CEO"));
    expect(ceo.state === "found" && ceo.unit).toBeNull();
  });

  test("a seat the document does not hold is missing, not someone else's", () => {
    expect(seatSettings(doc, byName("Designer")).state).toBe("missing");
    expect(seatSettings(null, byName("CEO")).state).toBe("missing");
  });

  test("two seats with one name are ambiguous rather than a guess", () => {
    const twice: CompanyDocument = { roles: [{ name: "CEO" }, { name: "CEO", email: "x" }] };
    expect(seatSettings(twice, byName("CEO")).state).toBe("ambiguous");
  });
});

// ---------------------------------------------------------------------------
// Properties, over companies nobody wrote by hand
// ---------------------------------------------------------------------------

/**
 * mulberry32: a seeded PRNG written here rather than a dependency, so a
 * failing case is reproducible from the seed it prints and the suite adds
 * nothing to package.json.
 */
function prng(seed: number): () => number {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

/**
 * A random company and a derived block that is consistent with it: random
 * nesting, random root seats moved into random units, random reporting lines
 * between real handles.
 */
function company(random: () => number): OrgProjection {
  const pick = <T>(items: T[]): T => items[Math.floor(random() * items.length)]!;
  let seatCount = 0;
  let unitCount = 0;
  const newOrgSeat = (): OrgSeat => ({ name: `Seat ${seatCount++}` });
  const newOrgUnit = (depth: number): OrgUnit => {
    const unit: OrgUnit = {
      name: `Unit ${unitCount++}`,
      roles: Array.from({ length: Math.floor(random() * 3) }, newOrgSeat),
    };
    if (depth < 3) {
      unit.children = Array.from({ length: Math.floor(random() * 3) }, () => newOrgUnit(depth + 1));
    }
    return unit;
  };
  const roles = Array.from({ length: Math.floor(random() * 4) }, newOrgSeat);
  const units = Array.from({ length: Math.floor(random() * 3) }, () => newOrgUnit(0));

  // The engine's view: units depth-first, root seats possibly attached.
  const flat: OrgUnit[] = [];
  const visit = (u: OrgUnit) => {
    flat.push(u);
    (u.children ?? []).forEach(visit);
  };
  units.forEach(visit);
  const members = new Map<OrgUnit, string[]>(
    flat.map((u) => [u, (u.roles ?? []).map((r) => r.name)]),
  );
  const kept: string[] = [];
  const placed = new Set<string>();
  for (const role of roles) {
    if (flat.length && random() < 0.4) {
      members.get(pick(flat))!.push(role.name);
      placed.add(role.name);
    } else {
      kept.push(role.name);
    }
  }
  const order = [...kept, ...flat.flatMap((u) => members.get(u)!)];
  const handleOf = (name: string) => name.toLowerCase().replace(/\s+/g, "-");
  const handles = order.map(handleOf);
  const seats: DerivedSeat[] = order.map((name) => {
    const managers = handles.filter(() => random() < 0.15);
    return {
      handle: handleOf(name),
      name,
      kind: random() < 0.2 ? "human" : "agent",
      placed_by_ref: placed.has(name),
      manager: managers[0] ?? "",
      managers,
      reports: handles.filter(() => random() < 0.1),
      auto_reports: null,
      onboarding_chain: null,
    };
  });
  const derivedUnits: DerivedUnit[] = flat.map((u) => ({
    name: u.name,
    type: "team",
    lead: members.get(u)!.length && random() < 0.5 ? handleOf(pick(members.get(u)!)) : "",
    lead_inherited: random() < 0.3,
    channel: "",
    channel_inherited: false,
    seats: members.get(u)!.map(handleOf),
  }));
  return { name: "Generated", roles, units, derived: { seats, units: derivedUnits } };
}

/** Every structural invariant the screens rely on, for one index. */
function invariants(built: ReturnType<typeof indexOrg>, org: OrgProjection, seed: number): void {
  const at = `seed ${seed}`;
  const total = (org.roles ?? []).length + countSeats(org.units ?? []);
  expect(built.seats.length, at).toBe(total);
  expect(new Set(built.seats).size, at).toBe(total);
  expect(new Set(built.seats.map((s) => s.key)).size, at).toBe(total);

  // Every seat is either at the root or a member of exactly the unit it names.
  const inUnits = built.units.flatMap((u) => u.seats);
  expect(inUnits.length + built.rootSeats.length, at).toBe(total);
  for (const unit of built.units) {
    for (const member of unit.seats) expect(member.unit, at).toBe(unit);
    expect(unit.chain.at(-1), at).toBe(unit);
    for (let i = 1; i < unit.chain.length; i++) {
      expect(unit.chain[i]!.parent, at).toBe(unit.chain[i - 1]);
    }
  }
  for (const s of built.seats) {
    expect(s.unitChain.at(-1) ?? null, at).toBe(s.unit);
    if (s.manager) expect(s.managers.includes(s.manager), at).toBe(true);
  }
}

function countSeats(units: OrgUnit[]): number {
  return units.reduce((n, u) => n + (u.roles ?? []).length + countSeats(u.children ?? []), 0);
}

describe("properties", () => {
  // A fixed base, so CI runs the same cases every time and a failure names
  // the seed that reproduces it.
  const BASE_SEED = 0x5eed;
  const CASES = 300;

  test("a consistent derived block is believed, and the index is well formed", () => {
    for (let i = 0; i < CASES; i++) {
      const seedValue = BASE_SEED + i;
      const generated = company(prng(seedValue));
      const built = indexOrg(generated);
      expect(built.hierarchy, `seed ${seedValue}`).toBe(true);
      invariants(built, generated, seedValue);
    }
  });

  test("a derived block missing any one seat is refused, and nothing is lost", () => {
    for (let i = 0; i < CASES; i++) {
      const seedValue = BASE_SEED + CASES + i;
      const random = prng(seedValue);
      const generated = company(random);
      const seats = generated.derived!.seats!;
      if (!seats.length) continue;
      const drop = Math.floor(random() * seats.length);
      const damaged = {
        ...generated,
        derived: { ...generated.derived!, seats: seats.filter((_, j) => j !== drop) },
      };
      const built = indexOrg(damaged);
      expect(built.hierarchy, `seed ${seedValue}`).toBe(false);
      invariants(built, damaged, seedValue);
    }
  });
});

// ---------------------------------------------------------------------------
// Live state
// ---------------------------------------------------------------------------

describe("what a seat is doing", () => {
  const box: SandboxEntry = {
    turn_id: "t1",
    role: "Dev A",
    agent_handle: "dev-a",
    agent_id: "",
    coding_agent: "claude-code",
    sandbox_id: "s1",
    task: "",
    status: "running",
    started_at: "",
  };

  test("an in-flight sandbox run keeps a seat busy", () => {
    // Its kick-off turn already completed, which the projection reads as
    // idle — so without folding the live sandbox set in, a seat writing code
    // for ten minutes renders as idle.
    expect(runState({ id: "a", role: "Dev A", state: "idle" }, [box])).toBe("awaiting_sandbox");
    expect(runState({ id: "a", role: "Dev A", state: "idle" }, [])).toBe("idle");
  });

  test("colour is STATE and an idle seat gets none", () => {
    // An idle seat used to draw a tinted, glowing tile that read as activity.
    // The fix for that is not a duller hue, it is none.
    expect(seatTone({ id: "a", role: "Dev A", state: "idle" }, [])).toBe("quiet");
    expect(seatTone({ id: "a", role: "Dev A", state: "working" }, [])).toBe("working");
  });

  test("waiting on a person and having fallen over are DIFFERENT tones", () => {
    // Both stopped, and only one is a failure. Red is reserved for failure.
    expect(seatTone({ id: "a", role: "Dev A" }, [{ ...box, status: "awaiting_input" }])).toBe(
      "needs",
    );
    expect(
      seatTone(
        {
          id: "a",
          role: "Dev A",
          last_error: { kind: "x", message: "", phase: "", turn_id: "", at: "", event_id: "" },
        },
        [],
      ),
    ).toBe("broken");
  });

  test("a status line describes live state and never invents one", () => {
    expect(
      statusLine({ id: "a", role: "Dev A", state: "working", current_phase: "execute" }),
    ).toContain("working on the task");
    expect(statusLine({ id: "a", role: "Dev A", state: "afk", afk_reason: "stall" })).toContain(
      "no forward progress",
    );
    expect(statusLine(undefined)).toBe("not running on this node");
    expect(statusLine(null, { seat: byName("Jane Founder") })).toBe("weekdays");
  });
});

describe("staleness", () => {
  const now = Date.parse("2026-01-01T12:00:00Z");

  test("a live round that has not moved is called out", () => {
    // A live row animates, which sells motion — so a turn on round 3 for
    // eleven minutes looked exactly like one that started two seconds ago.
    expect(staleness(new Date(now - 5_000).toISOString(), now)).toBe("");
    expect(staleness(new Date(now - STALE_MS - 1).toISOString(), now)).toBe("stale");
    expect(staleness(new Date(now - STALLED_MS - 1).toISOString(), now)).toBe("stalled");
  });

  test("a missing stamp makes no claim", () => {
    expect(staleness(undefined, now)).toBe("");
  });
});
