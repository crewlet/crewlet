// @vitest-environment node

/**
 * The org index every screen consumes, and the seat-state rules.
 *
 * THE HIERARCHY IS THE ENGINE'S. The projection fixture is written out field
 * for field from `internal/api`'s `OrgProjection` with its public `derived`
 * block, because every reporting line, inherited lead and placement pinned
 * here is a READING of that block rather than a rule this client applies: the
 * TypeScript copy that used to apply them had drifted from Go on four counts,
 * and nothing compared the two.
 *
 * It is resolved ONCE, into an index, because doing it per screen is how the
 * previous dashboard came to walk the whole roster once per rendered row: its
 * `managerOf` was a linear scan called per seat AND again per row, roughly
 * 80,000 array scans per event push on a 200-seat company.
 */

import { describe, expect, test } from "vitest";
import {
  indexOrg,
  llmChain,
  mcpEnvOf,
  runState,
  seatPath,
  seatSettings,
  seatTone,
  statusLine,
  staleness,
  STALE_MS,
  STALLED_MS,
  unitDirectLabel,
  unitSeatsLabel,
  unitSettings,
  unitTally,
  UNIT_TOTAL_HINT,
} from "./seats.ts";
import type {
  CompanyDocument,
  DerivedSeat,
  OrgProjection,
  SandboxEntry,
} from "~/protocol/index.ts";

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

const org: OrgProjection = {
  name: "Acme",
  roles: [
    { name: "Jane Founder", kind: "human", manages: ["CEO"] },
    { name: "CEO", handle: "ceo", manages: ["Engineering"] },
    // A root seat whose `unit:` reference the ENGINE moved into Backend. The
    // document writes it above every unit; only the derived block says where
    // it actually sits.
    { name: "Designer" },
  ],
  units: [
    {
      name: "Engineering",
      type: "department",
      lead: "VP Engineering",
      roles: [{ name: "VP Engineering", handle: "vpe" }],
      children: [
        {
          name: "Backend",
          type: "team",
          // No lead: it inherits VP Engineering from the parent.
          roles: [{ name: "Dev A" }, { name: "Dev B" }],
        },
      ],
    },
  ],
  derived: {
    seats: [
      seat({ handle: "jane-founder", name: "Jane Founder", kind: "human", reports: ["ceo"] }),
      seat({
        handle: "ceo",
        name: "CEO",
        manager: "jane-founder",
        managers: ["jane-founder"],
        reports: ["vpe", "dev-a", "dev-b"],
      }),
      seat({ handle: "vpe", name: "VP Engineering", manager: "ceo", managers: ["ceo"] }),
      seat({ handle: "dev-a", name: "Dev A", manager: "ceo", managers: ["ceo"] }),
      seat({ handle: "dev-b", name: "Dev B", manager: "ceo", managers: ["ceo"] }),
      seat({
        handle: "designer",
        name: "Designer",
        placed_by_ref: true,
        manager: "vpe",
        managers: ["vpe"],
        auto_reports: null,
      }),
    ],
    units: [
      {
        name: "Engineering",
        type: "department",
        lead: "vpe",
        lead_inherited: false,
        channel: "",
        channel_inherited: false,
        seats: ["vpe"],
      },
      {
        name: "Backend",
        type: "team",
        lead: "vpe",
        lead_inherited: true,
        channel: "",
        channel_inherited: false,
        seats: ["dev-a", "dev-b", "designer"],
      },
    ],
  },
};

const index = indexOrg(org);

/** The same company, from an engine that reports no derived hierarchy. */
const { derived: _omitted, ...older } = org;
const authored = indexOrg(older);

describe("the engine's hierarchy", () => {
  test("every role becomes a seat, wherever the document wrote it", () => {
    expect(index.seats.map((s) => s.name).sort()).toEqual([
      "CEO",
      "Designer",
      "Dev A",
      "Dev B",
      "Jane Founder",
      "VP Engineering",
    ]);
    expect(index.hierarchy).toBe(true);
  });

  // THE HANDLE IS THE ENGINE'S AND IS NEVER DERIVED HERE. The copy this
  // replaced lower-cased "İlker" into `i-lker` where Go derives `ilker`, and
  // the handle keys a seat's memory — so a link pinning the client's version
  // pointed at nothing.
  test("a handle is the engine's, and is unknown where it did not say", () => {
    expect(index.byName.get("Dev A")?.handle).toBe("dev-a");
    expect(index.byName.get("CEO")?.handle).toBe("ceo");
    // Without the block, only a DECLARED handle is known.
    expect(authored.byName.get("CEO")?.handle).toBe("ceo");
    expect(authored.byName.get("Dev A")?.handle).toBe("");
    expect(authored.hierarchy).toBe(false);
  });

  // A LINK STILL REACHES A SEAT WITH NO REPORTED HANDLE: the seat screen
  // resolves a name as well as a handle, and the rule lives in one place.
  test("a seat with no reported handle is addressed by name", () => {
    expect(seatPath(index.byName.get("Dev A")!)).toEqual(["company", "people", "dev-a"]);
    expect(seatPath(authored.byName.get("Dev A")!)).toEqual(["company", "people", "Dev A"]);
  });

  // ONLY THE ENGINE KNOWS WHERE A ROOT SEAT SITS. The document wrote Designer
  // above every unit; its `unit:` reference put it in Backend.
  test("a root seat the engine placed sits in its unit, and says it was placed", () => {
    const designer = index.byName.get("Designer")!;
    expect(designer.unit?.name).toBe("Backend");
    expect(designer.placedByRef).toBe(true);
    expect(index.rootSeats.map((s) => s.name).sort()).toEqual(["CEO", "Jane Founder"]);
    // Without the block it sits where it was WRITTEN, and nothing claims more.
    expect(authored.byName.get("Designer")!.unit).toBeNull();
    expect(authored.byName.get("Designer")!.placedByRef).toBe(false);
  });

  test("a unit with no lead of its own takes the inherited one, marked", () => {
    // It behaves identically to an explicit lead everywhere in the engine, so
    // hiding the difference is how an operator comes to think a unit is
    // unmanaged.
    const backend = index.units.find((u) => u.name === "Backend")!;
    expect(backend.effectiveLead?.name).toBe("VP Engineering");
    expect(backend.leadInherited).toBe(true);
    expect(index.units.find((u) => u.name === "Engineering")!.leadInherited).toBe(false);
    expect(index.byName.get("Dev A")?.unitLead).toBe("VP Engineering");
    expect(index.byName.get("Dev A")?.unitChain.map((u) => u.name)).toEqual([
      "Engineering",
      "Backend",
    ]);
    // An INHERITED lead is the engine's conclusion, so without the block the
    // unit has none rather than one this client cascaded.
    expect(authored.units.find((u) => u.name === "Backend")!.effectiveLead).toBeNull();
    expect(authored.units.find((u) => u.name === "Engineering")!.effectiveLead?.name).toBe(
      "VP Engineering",
    );
  });

  test("reporting lines are the engine's, and unknown without them", () => {
    expect(
      index.byName
        .get("CEO")!
        .reports.map((r) => r.name)
        .sort(),
    ).toEqual(["Dev A", "Dev B", "VP Engineering"]);
    expect(index.byName.get("Dev A")!.manager?.name).toBe("CEO");
    expect(index.byName.get("CEO")!.manager?.name).toBe("Jane Founder");
    // NOT NOBODY: the engine did not say.
    expect(authored.byName.get("Dev A")!.manager).toBeNull();
    expect(authored.byName.get("CEO")!.reports).toEqual([]);
  });

  // A BLOCK THAT DOES NOT DESCRIBE THIS TREE IS NOT HALF A HIERARCHY. A chart
  // drawn from one that disagrees with its own seats is a chart that lies.
  test("a derived block that does not match the tree is refused whole", () => {
    const mismatched = indexOrg({
      ...org,
      derived: { ...org.derived!, seats: (org.derived!.seats ?? []).slice(0, 2) },
    });
    expect(mismatched.hierarchy).toBe(false);
    expect(mismatched.byName.get("Dev A")!.manager).toBeNull();

    // And one naming a handle that belongs to no seat in it.
    const dangling = indexOrg({
      ...org,
      derived: {
        ...org.derived!,
        seats: (org.derived!.seats ?? []).map((d) =>
          d.handle === "dev-a" ? { ...d, manager: "nobody-here" } : d,
        ),
      },
    });
    expect(dangling.hierarchy).toBe(false);

    // A UNIT'S MEMBERSHIP IS THE SAME KIND OF CLAIM, and it has its own guard:
    // a unit naming a handle no seat in the block carries would otherwise
    // leave that unit a member short and every seat placed from it wrong,
    // silently.
    const phantom = indexOrg({
      ...org,
      derived: {
        ...org.derived!,
        units: (org.derived!.units ?? []).map((u) =>
          u.name === "Backend" ? { ...u, seats: [...(u.seats ?? []), "ghost"] } : u,
        ),
      },
    });
    expect(phantom.hierarchy).toBe(false);
  });

  test("a human seat holds a place in the hierarchy", () => {
    // Addressable-only: no runtime, no inbox, no LLM — but escalation has to
    // terminate at a person.
    const founder = index.byName.get("Jane Founder");
    expect(founder?.kind).toBe("human");
    expect(founder?.reports.map((r) => r.name)).toEqual(["CEO"]);
  });

  test("nobody manages themselves", () => {
    for (const s of index.seats) {
      expect(s.reports.map((r) => r.name)).not.toContain(s.name);
    }
  });
});

// ---------------------------------------------------------------------------
// The guarded half
// ---------------------------------------------------------------------------

// EVERYTHING BELOW IS OFF THE COMPANY DOCUMENT, not the projection. `/org` is
// anonymously readable, so email, the model chain, the token budget, contact
// identities, `mcp_env`, `space:` and `id:` are not on it at all.
const doc: CompanyDocument = {
  name: "Acme",
  roles: [
    { name: "Jane Founder", kind: "human", contact: { slack_user_id: "U0FOUNDER" } },
    { name: "CEO", handle: "ceo", email: "ceo@example.com", token_budget: 250000 },
  ],
  units: [
    {
      name: "Engineering",
      id: "eng",
      mcp_env: { github: { GITHUB_HOST: "example.com" } },
      roles: [{ name: "VP Engineering", handle: "vpe" }],
      children: [
        {
          name: "Backend",
          space: "ENG",
          mcp_env: { github: { GITHUB_TOKEN: "${BACKEND_TOKEN}" } },
          roles: [
            { name: "Dev A", mcp_env: { github: { GITHUB_TOKEN: "${DEV_A_TOKEN}" } } },
            { name: "Dev B" },
          ],
        },
      ],
    },
  ],
};

describe("what only the company document says", () => {
  test("a seat is found by the name the document addresses it by", () => {
    const found = seatSettings(doc, index.byName.get("CEO")!);
    expect(found.state).toBe("found");
    expect(found.state === "found" && found.role.email).toBe("ceo@example.com");
    expect(found.state === "found" && found.role.token_budget).toBe(250000);
  });

  // THE PROJECTION AND THE DOCUMENT CAN DISAGREE for a moment either side of
  // an apply, and a seat that is in one and not the other is a state to say
  // rather than a blank panel.
  test("a seat the document does not hold is missing, not empty", () => {
    expect(seatSettings(doc, index.byName.get("Designer")!).state).toBe("missing");
    expect(seatSettings(null, index.byName.get("CEO")!).state).toBe("missing");
  });

  // TWO SEATS WITH ONE NAME can only come from a revision stored before names
  // had to be unique, and attributing either one's settings to the page would
  // be a guess.
  test("a name held by two seats is ambiguous rather than the first match", () => {
    const twice: CompanyDocument = { ...doc, roles: [...(doc.roles ?? []), { name: "CEO" }] };
    expect(seatSettings(twice, index.byName.get("CEO")!).state).toBe("ambiguous");
  });

  test("mcp_env merges DOWN the unit chain with the seat's own winning", () => {
    const env = mcpEnvOf(seatSettings(doc, index.byName.get("Dev A")!), "agent").github;
    expect(env?.GITHUB_HOST).toBeUndefined();
    expect(env?.GITHUB_TOKEN).toBe("${DEV_A_TOKEN}");
    const inherited = mcpEnvOf(seatSettings(doc, index.byName.get("Dev B")!), "agent").github;
    expect(inherited?.GITHUB_TOKEN).toBe("${BACKEND_TOKEN}");
  });

  // A HUMAN SEAT RUNS NO TOOLS, so it inherits none of its unit's credentials.
  test("a human seat inherits no tool credentials", () => {
    const found = seatSettings(doc, index.byName.get("Dev B")!);
    expect(Object.keys(mcpEnvOf(found, "human"))).toEqual([]);
  });

  test("a unit's guarded fields are read from the document, and only from it", () => {
    expect(unitSettings(doc, { name: "Engineering" })?.id).toBe("eng");
    expect(unitSettings(doc, { name: "Backend" })?.space).toBe("ENG");
    expect(unitSettings(null, { name: "Backend" })).toBeNull();
  });
});

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
    //
    // THE ENGINE'S OWN WORD. This asserted `awaiting_input`, which
    // `sandbox.PendingRun` cannot write, so the case passed against a fixture
    // no engine produces while the real state reached no tone at all.
    expect(
      seatTone({ id: "a", role: "Dev A" }, [{ ...box, status: "awaiting_clarification" }]),
    ).toBe("needs");
    // A box reaped past its pause TTL is the same fact one step worse.
    expect(seatTone({ id: "a", role: "Dev A" }, [{ ...box, status: "reseed" }])).toBe("needs");
    // And a running one is not waiting on anybody — without this the rule
    // could be "any sandbox at all" and still pass.
    expect(
      seatTone({ id: "a", role: "Dev A", state: "idle" }, [{ ...box, status: "running" }]),
    ).toBe("working");
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
    expect(statusLine(null, { seat: index.byName.get("Jane Founder")! })).toContain("human");
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

// ---------------------------------------------------------------------------
// The model chain, in every shape the config accepts
// ---------------------------------------------------------------------------

// `config.PhaseLLM` marshals as a STRING for one provider, an ARRAY for a
// fallback chain, and an OBJECT keyed on phase for a per-phase mapping. The
// client declared a string, so an array rendered as `fast,backup` and a
// mapping as `[object Object]`, and any consumer calling a string method on
// one threw on a config the engine accepts.
describe("llmChain", () => {
  test("reads all three shapes the engine marshals", () => {
    expect(llmChain("fast")).toEqual(["fast"]);
    expect(llmChain(["fast", "backup"])).toEqual(["fast", "backup"]);
    expect(llmChain({ default: "big", judge: "tiny" })).toEqual(["big", "tiny"]);
  });

  // A SEAT THAT SAYS NOTHING takes the default provider, and that is not the
  // same as a seat pinned to a provider called "".
  test("an unset field is an empty chain, not a chain of one empty key", () => {
    expect(llmChain(undefined)).toEqual([]);
    expect(llmChain("")).toEqual([]);
    expect(llmChain([])).toEqual([]);
    expect(llmChain({})).toEqual([]);
  });

  // THE SAME KEY REACHED THROUGH TWO PHASES IS NOT TWO MODELS. Without this
  // the common mapping — one strong model for most phases, a cheap one for the
  // judge — reads as five models on the seat page.
  test("one key named by several phases is listed once", () => {
    expect(
      llmChain({ default: "big", review: "big", judge: "tiny", sandbox: ["big", "tiny"] }),
    ).toEqual(["big", "tiny"]);
  });

  // A PHASE FALLS BACK TO `default`, exactly as the engine's own resolution
  // does — so asking for the judge of a seat that never named one answers the
  // model the judge will actually run on.
  test("a named phase falls back to default", () => {
    const llm = { default: "big", judge: "tiny" };
    expect(llmChain(llm, "judge")).toEqual(["tiny"]);
    expect(llmChain(llm, "review")).toEqual(["big"]);
    // And a flat chain answers the same for every phase, because it is one.
    expect(llmChain(["fast", "backup"], "judge")).toEqual(["fast", "backup"]);
  });
});

// ---------------------------------------------------------------------------
// A unit's headcount
// ---------------------------------------------------------------------------

/**
 * TWO HONEST NUMBERS, AND ONE PLACE THAT DECIDES WHICH.
 *
 * Every surface used to count for itself and draw the answer bare: the
 * workspace rail summed the whole subtree, the org chart's block counted the
 * unit's own members, the roster's group head counted whatever was on screen,
 * and the unit page printed three figures over two different trees. So
 * "Leadership" carried 5, 2 and a third number across one product, with
 * nothing anywhere saying which question any of them had answered.
 *
 * The fixture is the shape that makes the difference visible: Engineering
 * holds one seat of its own and one sub-unit holding three.
 */
describe("a unit's headcount", () => {
  const engineering = index.units.find((u) => u.name === "Engineering")!;
  const backend = index.units.find((u) => u.name === "Backend")!;

  test("counts its own members and its subtree separately", () => {
    expect(unitTally(engineering)).toEqual({ direct: 1, total: 4, subUnits: 1 });
    // A LEAF'S TWO ANSWERS AGREE, which is why most units never showed the bug.
    expect(unitTally(backend)).toEqual({ direct: 3, total: 3, subUnits: 0 });
  });

  // THE SUBTREE INCLUDES A SEAT THE DOCUMENT PUT SOMEWHERE ELSE. `Designer` is
  // written above every unit and the ENGINE placed it in Backend, so a count
  // derived from the document's own nesting would miss it — and did, on the
  // one surface that walked `org.units` instead of the index.
  test("the subtree is the engine's placement, not the document's nesting", () => {
    expect(engineering.allSeats.map((s) => s.handle).sort()).toEqual([
      "designer",
      "dev-a",
      "dev-b",
      "vpe",
    ]);
  });

  // ONE SENTENCE PER SURFACE, and the appended clause only where the two
  // numbers differ: "3 seats, 3 directly" reads as two facts about a team that
  // has one.
  test("says the subtree first and names the direct count only when it differs", () => {
    expect(unitSeatsLabel(unitTally(engineering))).toBe("4 seats, 1 directly");
    expect(unitSeatsLabel(unitTally(backend))).toBe("3 seats");
  });

  // THE OTHER HALF, for a surface that can only ever show direct members: a
  // seat sits in exactly one group, so a roster grouped by unit is the unit's
  // own members and nothing under it.
  test("a roster group says that it is the direct members", () => {
    expect(unitDirectLabel(3)).toBe("3 seats directly in it");
    expect(unitDirectLabel(1)).toBe("1 seat directly in it");
  });

  // AND THE HEADLINE NUMBER CARRIES ITS OWN SENTENCE. A bare number beside a
  // name is read as "how many there are"; this is what the rail's badge and
  // the unit page's fact both hand a reader instead.
  test("the total's hint names what it counted", () => {
    expect(UNIT_TOTAL_HINT).toContain("everything under it");
  });
});
