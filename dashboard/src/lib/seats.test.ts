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
  activityOf,
  activityWord,
  handleLabel,
  indexOrg,
  labelOf,
  leadsInLine,
  liveOnItems,
  liveRowFor,
  llmChain,
  mcpEnvOf,
  ringOf,
  roundLabel,
  roundOf,
  sandboxFor,
  seatAddress,
  seatByAddress,
  seatFilter,
  seatPath,
  seatReading,
  stateLine,
  stoppedLine,
  staleness,
  toneOf,
  STALE_MS,
  STALLED_MS,
  unitByKey,
  unitDirectLabel,
  unitPath,
  unitSeatsLabel,
  unitTally,
  UNIT_TOTAL_HINT,
  type OrgIndex,
  type Seat,
} from "./seats.ts";
import type { ChartReading } from "./chartReads.ts";
import type {
  AgentRow,
  ChartAnswer,
  ChartSeat,
  ChartSeatRead,
  ChartUnit,
  ChartUnitRead,
  DerivedSeat,
  LiveCall,
  OrgProjection,
  SandboxEntry,
} from "~/protocol/index.ts";
import {
  type Lang,
  type Node,
  lineOf,
  memberName,
  modules,
  parse,
  stringValue,
  walk,
} from "~/test/source.ts";

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
      id: "engineering",
      name: "Engineering",
      type: "department",
      lead: "VP Engineering",
      roles: [{ name: "VP Engineering", handle: "vpe" }],
      children: [
        {
          id: "backend",
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
        id: "engineering",
        name: "Engineering",
        type: "department",
        lead: "vpe",
        lead_inherited: false,
        channel: "",
        channel_inherited: false,
        seats: ["vpe"],
      },
      {
        id: "backend",
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

/**
 * The first seat carrying a name. A TEST convenience over fixtures whose names
 * are unique: nothing in the product resolves a seat by name, because two
 * seats may share one.
 */
const named = (i: OrgIndex, name: string): Seat | undefined => i.seats.find((s) => s.name === name);

/** The same company, from an engine that reports no derived hierarchy. */
const { derived: _omitted, ...older } = org;
const authored = indexOrg(older);

// A LEAD IS ANYBODY ABOVE IN THE CHART, as the engine reads "somebody in
// their line" (`leadsOf` walks every ancestor): a founder leads everybody. And
// WITHOUT THE ENGINE'S HIERARCHY THE ANSWER IS UNKNOWN, never "no" — a screen
// that turned an undescribed chart into a refusal would tell a founder they do
// not lead their own company.
describe("who leads whom", () => {
  test("a direct manager and every manager above are in the line", () => {
    expect(leadsInLine(index, "ceo", "dev-a")).toBe(true);
    expect(leadsInLine(index, "jane-founder", "dev-a")).toBe(true);
    expect(leadsInLine(index, "jane-founder", "designer")).toBe(true);
  });

  test("a peer, a report and the person themselves are not", () => {
    expect(leadsInLine(index, "dev-a", "dev-b")).toBe(false);
    expect(leadsInLine(index, "dev-a", "ceo")).toBe(false);
    expect(leadsInLine(index, "ceo", "ceo")).toBe(false);
  });

  test("a chart the engine did not derive answers unknown, not no", () => {
    expect(leadsInLine(authored, "jane-founder", "dev-a")).toBeNull();
  });

  // A CONFIG CAN EXPRESS A CYCLE, which the engine's own walk ends; so does
  // this one, rather than hanging the tab.
  test("a management cycle ends the walk", () => {
    const loop = indexOrg({
      name: "Loop",
      roles: [
        { name: "A", handle: "a" },
        { name: "B", handle: "b" },
        { name: "C", handle: "c" },
      ],
      units: [],
      derived: {
        units: [],
        seats: [
          seat({ handle: "a", name: "A", managers: ["b"], reports: ["b"] }),
          seat({ handle: "b", name: "B", managers: ["a"], reports: ["a"] }),
          seat({ handle: "c", name: "C" }),
        ],
      },
    });
    expect(loop.hierarchy).toBe(true);
    expect(leadsInLine(loop, "c", "a")).toBe(false);
    expect(leadsInLine(loop, "b", "a")).toBe(true);
  });
});

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
    expect(named(index, "Dev A")?.handle).toBe("dev-a");
    expect(named(index, "CEO")?.handle).toBe("ceo");
    // Without the block, only a DECLARED handle is known.
    expect(named(authored, "CEO")?.handle).toBe("ceo");
    expect(named(authored, "Dev A")?.handle).toBe("");
    expect(authored.hierarchy).toBe(false);
  });

  // A LINK STILL REACHES A SEAT WITH NO REPORTED HANDLE: the seat screen
  // resolves a name as well as a handle, and the rule lives in one place.
  test("a seat with no reported handle is addressed by name", () => {
    expect(seatPath(named(index, "Dev A")!)).toEqual(["agents", "seats", "dev-a"]);
    expect(seatPath(named(authored, "Dev A")!)).toEqual(["agents", "seats", "Dev A"]);
  });

  // ONLY THE ENGINE KNOWS WHERE A ROOT SEAT SITS. The document wrote Designer
  // above every unit; its `unit:` reference put it in Backend.
  test("a root seat the engine placed sits in its unit, and says it was placed", () => {
    const designer = named(index, "Designer")!;
    expect(designer.unit?.name).toBe("Backend");
    expect(designer.placedByRef).toBe(true);
    expect(index.rootSeats.map((s) => s.name).sort()).toEqual(["CEO", "Jane Founder"]);
    // Without the block it sits where it was WRITTEN, and nothing claims more.
    expect(named(authored, "Designer")!.unit).toBeNull();
    expect(named(authored, "Designer")!.placedByRef).toBe(false);
  });

  test("a unit with no lead of its own takes the inherited one, marked", () => {
    // It behaves identically to an explicit lead everywhere in the engine, so
    // hiding the difference is how an operator comes to think a unit is
    // unmanaged.
    const backend = index.units.find((u) => u.name === "Backend")!;
    expect(backend.effectiveLead?.name).toBe("VP Engineering");
    expect(backend.leadInherited).toBe(true);
    expect(index.units.find((u) => u.name === "Engineering")!.leadInherited).toBe(false);
    expect(named(index, "Dev A")?.unitLead).toBe("VP Engineering");
    expect(named(index, "Dev A")?.unitChain.map((u) => u.name)).toEqual(["Engineering", "Backend"]);
    // An INHERITED lead is the engine's conclusion, so without the block the
    // unit has none rather than one this client cascaded.
    expect(authored.units.find((u) => u.name === "Backend")!.effectiveLead).toBeNull();
    expect(authored.units.find((u) => u.name === "Engineering")!.effectiveLead?.name).toBe(
      "VP Engineering",
    );
  });

  test("reporting lines are the engine's, and unknown without them", () => {
    expect(
      named(index, "CEO")!
        .reports.map((r) => r.name)
        .sort(),
    ).toEqual(["Dev A", "Dev B", "VP Engineering"]);
    expect(named(index, "Dev A")!.manager?.name).toBe("CEO");
    expect(named(index, "CEO")!.manager?.name).toBe("Jane Founder");
    // NOT NOBODY: the engine did not say.
    expect(named(authored, "Dev A")!.manager).toBeNull();
    expect(named(authored, "CEO")!.reports).toEqual([]);
  });

  // A BLOCK THAT DOES NOT DESCRIBE THIS TREE IS NOT HALF A HIERARCHY. A chart
  // drawn from one that disagrees with its own seats is a chart that lies.
  test("a derived block that does not match the tree is refused whole", () => {
    const mismatched = indexOrg({
      ...org,
      derived: { ...org.derived!, seats: (org.derived!.seats ?? []).slice(0, 2) },
    });
    expect(mismatched.hierarchy).toBe(false);
    expect(named(mismatched, "Dev A")!.manager).toBeNull();

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

    // AND A UNIT PAIRS BY ITS KEY as well as its name: two units may share a
    // name, so a block naming another unit in this one's place is not this
    // unit's hierarchy however it is spelled.
    const swapped = indexOrg({
      ...org,
      derived: {
        ...org.derived!,
        units: (org.derived!.units ?? []).map((u) =>
          u.name === "Backend" ? { ...u, id: "backend-2" } : u,
        ),
      },
    });
    expect(swapped.hierarchy).toBe(false);
  });

  test("a human seat holds a place in the hierarchy", () => {
    // Addressable-only: no runtime, no inbox, no LLM — but escalation has to
    // terminate at a person.
    const founder = named(index, "Jane Founder");
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

// EVERYTHING BELOW IS OFF THE ORG CHART'S OWN READ, not the projection. `/org`
// is anonymously readable, so the model chain, the token budget, contact
// identities and `mcp_env` are not on it at all; the chart serves them as a
// row's RUNTIME half, and only to a reader who may read the configuration.
const answer: ChartAnswer = { level: "consistent_prefix", position: "CREWLET_CHART_LOG@1:10" };

const backend: ChartUnit = {
  key: "backend",
  name: "Backend",
  parent: "engineering",
  runtime: {
    mcp_env: { github: { GITHUB_HOST: "example.com", GITHUB_TOKEN: "${BACKEND_TOKEN}" } },
  },
};
const devA: ChartSeat = {
  handle: "dev-a",
  name: "Dev A",
  unit: "backend",
  runtime: {
    token_budget: { week: 250000 },
    mcp_env: { github: { GITHUB_TOKEN: "${DEV_A_TOKEN}" } },
  },
};
const ceo: ChartSeat = { handle: "ceo", name: "CEO", runtime: { llm: "fast" } };

const seatRead = (row: ChartSeat, runtime = true): ChartReading<ChartSeatRead> => ({
  state: "read",
  value: { seat: row, manages: null, answer, runtime },
});
const unitRead = (row: ChartUnit): ChartReading<ChartUnitRead> => ({
  state: "read",
  value: { unit: row, children: [], seats: [], answer, runtime: true },
});

// A UNIT IS ADDRESSED BY ITS KEY. Every route to one was built from its NAME,
// which is prose: two units may share one, and a unit that declares an `id:`
// is named by that id wherever the engine names it — a schedule's scope, the
// org chart's findings — so a page looked up by name opened the first unit of
// that name, and a link carrying the id opened none.
describe("addressing a unit", () => {
  /** Two teams called Platform, one of them declaring an id. */
  const twins: OrgProjection = {
    name: "Acme",
    roles: [],
    units: [
      { id: "platform", name: "Platform", purpose: "the web", roles: [{ name: "Web" }] },
      { id: "infra", name: "Platform", purpose: "the metal", roles: [{ name: "Metal" }] },
    ],
    derived: {
      seats: [seat({ handle: "web", name: "Web" }), seat({ handle: "metal", name: "Metal" })],
      units: [
        {
          id: "platform",
          name: "Platform",
          type: "team",
          lead: "",
          lead_inherited: false,
          channel: "",
          channel_inherited: false,
          seats: ["web"],
        },
        {
          id: "infra",
          name: "Platform",
          type: "team",
          lead: "",
          lead_inherited: false,
          channel: "",
          channel_inherited: false,
          seats: ["metal"],
        },
      ],
    },
  };
  const twinIndex = indexOrg(twins);

  test("each unit's path carries its own key", () => {
    expect(twinIndex.hierarchy).toBe(true);
    expect(twinIndex.units.map(unitPath)).toEqual([
      ["agents", "teams", "platform"],
      ["agents", "teams", "infra"],
    ]);
  });

  test("a key opens its own unit, whatever name it shares", () => {
    expect(unitByKey(twinIndex, "infra")?.purpose).toBe("the metal");
    expect(unitByKey(twinIndex, "platform")?.purpose).toBe("the web");
  });

  test("a name is not an address", () => {
    // "Platform" is the key of the first unit and the name of both; the id
    // one declares is not reachable under the name it shares.
    expect(unitByKey(twinIndex, "Platform")).toBeNull();
    expect(unitByKey(twinIndex, "")).toBeNull();
  });
});

describe("a renamed seat or unit keeps its old addresses", () => {
  /**
   * `web` was created as `frontend` and answered to `site` in between; the
   * `edge` unit was created as `platform`. A second seat now HOLDS `site` as
   * its current handle — a live handle must win over a retired alias.
   */
  const renamed: OrgProjection = {
    name: "Acme",
    roles: [
      { name: "Web", handle: "web" },
      { name: "Site Reliability", handle: "site" },
    ],
    units: [{ id: "edge", name: "Edge", roles: [{ name: "Metal", handle: "metal" }] }],
    derived: {
      seats: [
        seat({
          handle: "web",
          name: "Web",
          origin_handle: "frontend",
          former_handles: ["site", "frontend"],
        }),
        seat({ handle: "site", name: "Site Reliability" }),
        seat({ handle: "metal", name: "Metal" }),
      ],
      units: [
        {
          id: "edge",
          name: "Edge",
          type: "team",
          lead: "",
          lead_inherited: false,
          channel: "",
          channel_inherited: false,
          seats: ["metal"],
          origin_key: "platform",
          former_keys: ["platform"],
        },
      ],
    },
  };
  const renamedIndex = indexOrg(renamed);

  test("a link kept before a rename opens the seat that holds the address now", () => {
    expect(renamedIndex.hierarchy).toBe(true);
    expect(seatByAddress(renamedIndex, "frontend")?.handle).toBe("web");
    expect(seatByAddress(renamedIndex, "web")?.handle).toBe("web");
    // THE LIVE HANDLE WINS, however recently another seat gave it up.
    expect(seatByAddress(renamedIndex, "site")?.handle).toBe("site");
    // Typed with capitals, a handle is still the handle it spells.
    expect(seatByAddress(renamedIndex, "FRONTEND")?.handle).toBe("web");
    expect(seatByAddress(renamedIndex, "nobody")).toBeNull();
  });

  test("a seat's NAME is not its address", () => {
    // Every seat here has a handle, so no name addresses one — the arm that
    // resolved a name opened whichever namesake came first.
    expect(seatByAddress(renamedIndex, "Site Reliability")).toBeNull();
    // The one seat a name IS the address of: one the engine gave no handle,
    // which `seatPath` links to by name.
    const handleless = indexOrg({ name: "Acme", roles: [{ name: "Dev A" }] });
    expect(seatByAddress(handleless, "Dev A")?.name).toBe("Dev A");
  });

  test("a link kept before a unit was re-keyed opens that unit", () => {
    expect(unitByKey(renamedIndex, "platform")?.id).toBe("edge");
    expect(unitByKey(renamedIndex, "edge")?.id).toBe("edge");
  });
});

describe("what only the org chart's own read says", () => {
  test("a seat read with its runtime half is read, together with its home unit", () => {
    const reading = seatReading(seatRead(devA), unitRead(backend));
    expect(reading.state).toBe("read");
    expect(reading.state === "read" && reading.seat.runtime?.token_budget).toEqual({
      week: 250000,
    });
    expect(reading.state === "read" && reading.unit?.key).toBe("backend");
  });

  // A RUNTIME HALF WITHHELD IS NOT A REFUSAL: the chart served the rows and
  // said it left the half out, so the reading keeps the seat and says so.
  test("a seat served without its runtime half is stripped, not refused and not empty", () => {
    const reading = seatReading(
      seatRead({ handle: "dev-a", name: "Dev A", unit: "backend" }, false),
      unitRead(backend),
    );
    expect(reading).toEqual({
      state: "stripped",
      seat: { handle: "dev-a", name: "Dev A", unit: "backend" },
    });
    // Its credentials are unknown to this reader, never an empty set it was shown.
    expect(mcpEnvOf(reading, "agent")).toEqual({});
    // The control: the same seat served whole is read.
    expect(seatReading(seatRead(devA), unitRead(backend)).state).toBe("read");
  });

  // FIVE FACTS THE CHART CAN ANSWER, and a reading carries each as itself:
  // folded into one, a reader who lacked a grant was told the seat had
  // nothing to show.
  test("an absent, refused, failed or unread seat read is that answer, whatever the unit said", () => {
    expect(seatReading({ state: "absent" }, unitRead(backend))).toEqual({ state: "absent" });
    expect(
      seatReading({ state: "refused", grants: ["state:read"], reason: "needs a grant" }, null),
    ).toEqual({ state: "refused", grants: ["state:read"], reason: "needs a grant" });
    const failed = { state: "failed", failure: { error: "unanswered", refusal: null } } as const;
    expect(seatReading(failed, null)).toEqual(failed);
    expect(seatReading({ state: "unread" }, unitRead(backend))).toEqual({ state: "unread" });
  });

  // THE UNIT IS PART OF THE ANSWER: a seat's credentials drawn before its
  // unit's arrive would be a list that grows when the second read lands.
  test("a unit read still out, refused or failed is the seat reading's own answer", () => {
    expect(seatReading(seatRead(devA), { state: "unread" }).state).toBe("unread");
    expect(
      seatReading(seatRead(devA), {
        state: "failed",
        failure: { error: "query_failed", refusal: null },
      }).state,
    ).toBe("failed");
    expect(
      seatReading(seatRead(devA), { state: "refused", grants: ["config:read"], reason: "" }),
    ).toEqual({ state: "refused", grants: ["config:read"], reason: "" });
    // A unit the chart no longer holds is no unit rather than a failure.
    expect(seatReading(seatRead(devA), { state: "absent" })).toEqual({
      state: "read",
      seat: devA,
      unit: null,
    });
  });

  test("a seat at the root reads no unit, and needs none", () => {
    expect(seatReading(seatRead(ceo), null)).toEqual({ state: "read", seat: ceo, unit: null });
  });

  test("mcp_env merges the home unit's DOWN, the seat's own winning per variable", () => {
    const env = mcpEnvOf(seatReading(seatRead(devA), unitRead(backend)), "agent").github;
    // Per VARIABLE: the seat overrides the token and keeps the unit's host.
    expect(env?.GITHUB_TOKEN).toBe("${DEV_A_TOKEN}");
    expect(env?.GITHUB_HOST).toBe("example.com");
    const inherited = mcpEnvOf(
      seatReading(seatRead({ handle: "dev-b", name: "Dev B", unit: "backend" }), unitRead(backend)),
      "agent",
    ).github;
    expect(inherited?.GITHUB_TOKEN).toBe("${BACKEND_TOKEN}");
  });

  // A HUMAN SEAT RUNS NO TOOLS, so it inherits none of its unit's credentials.
  test("a human seat inherits no tool credentials", () => {
    const human = { handle: "dev-b", name: "Dev B", unit: "backend", kind: "human" };
    expect(mcpEnvOf(seatReading(seatRead(human), unitRead(backend)), "human")).toEqual({});
    // The control: the same unit hands an agent seat its credentials.
    expect(Object.keys(mcpEnvOf(seatReading(seatRead(human), unitRead(backend)), "agent"))).toEqual(
      ["github"],
    );
  });
});

describe("what a seat is doing", () => {
  const box: SandboxEntry = {
    turn_id: "t1",
    role: "Dev A",
    agent_handle: "dev-a",
    agent_id: "id-a",
    coding_agent: "claude-code",
    sandbox_id: "s1",
    task: "",
    status: "running",
    started_at: "",
  };

  const now = Date.parse("2026-01-01T12:00:00Z");

  test("a run belongs to its seat by agent id, never to a namesake", () => {
    // Two seats may share a name. Paired by the run's role name, the second
    // "Dev A" read as coding whenever the first one was.
    const namesake = { id: "b", agent_id: "id-b", role: "Dev A", activity: "idle" as const };
    expect(sandboxFor([box], namesake)).toBeNull();
    expect(sandboxFor([box], { agent_id: "id-a" })).toBe(box);
    // An empty id pairs with nothing, not with every run that carries none.
    expect(sandboxFor([{ ...box, agent_id: "" }], { agent_id: "" })).toBeNull();
  });

  test("a seat pairs with its live row by handle, never by name", () => {
    const ada: AgentRow = {
      id: "ada",
      agent_id: "id-ada",
      role: "Engineer",
      handle: "ada",
      activity: "working",
    };
    const bob: AgentRow = {
      id: "bob",
      agent_id: "id-bob",
      role: "Engineer",
      handle: "bob",
      activity: "idle",
    };
    expect(liveRowFor([ada, bob], { handle: "bob" })).toBe(bob);
    expect(liveRowFor([ada, bob], { handle: "ada" })).toBe(ada);
    // A seat whose handle nobody reported pairs with nothing rather than with
    // whichever namesake comes first.
    expect(liveRowFor([ada, bob], { handle: "" })).toBeUndefined();
  });

  test("a spend row opens its seat by handle, and a retired one by its id — never by name", () => {
    expect(seatAddress({ handle: "dev-a", agent_id: "id-a" })).toBe("dev-a");
    // A row the chart no longer holds carries no handle. Its name is somebody
    // else's address by now; its id answers the honest "no such seat".
    expect(seatAddress({ handle: "", agent_id: "id-gone" })).toBe("id-gone");
  });

  test("a one-seat filter asks by the agent id its handle pairs with", () => {
    const rows = [
      { id: "dev-a", agent_id: "id-a", role: "Dev A", handle: "dev-a" },
      { id: "dev-b", agent_id: "id-b", role: "Dev A", handle: "dev-b" },
    ];
    // No filter: no seat and no id, which a caller reads as "every seat".
    expect(seatFilter(index, rows, "")).toEqual({ seat: null, agentId: "" });
    // A filter on one of two namesakes narrows to that one alone.
    const b = seatFilter(index, rows, "dev-b");
    expect(b.seat?.handle).toBe("dev-b");
    expect(b.agentId).toBe("id-b");
    // A handle nothing answers to is NOT "every seat": the seat is null and a
    // caller must say so rather than widen the list.
    expect(seatFilter(index, rows, "nobody")).toEqual({ seat: null, agentId: "" });
    // A person's seat has no roster row, so it narrows to no agent id at all.
    const human = seatFilter(index, rows, "jane-founder");
    expect(human.seat?.handle).toBe("jane-founder");
    expect(human.agentId).toBe("");
  });

  test("a seat's state is the ENGINE'S word, and nothing is folded in here", () => {
    // The client used to fold the running-runs panel into the seat's state
    // itself, three different ways on three screens. The engine now serves
    // one word per seat — its runs included — and this reads it unchanged.
    expect(activityOf({ id: "a", agent_id: "id-a", role: "Dev A", activity: "working" })).toBe(
      "working",
    );
    expect(activityOf({ id: "a", agent_id: "id-a", role: "Dev A", activity: "needs" })).toBe(
      "needs",
    );
    expect(activityOf({ id: "a", agent_id: "id-a", role: "Dev A", activity: "idle" })).toBe("idle");
    // No row from the engine yet is its own answer, never a guess.
    expect(activityOf(undefined)).toBe("offline");
    expect(activityOf({ id: "a", agent_id: "id-a", role: "Dev A" })).toBe("offline");
  });

  test("colour is STATE and an idle seat gets none", () => {
    // An idle seat drawn as a green pill read as activity. The fix for that
    // is not a duller hue, it is none.
    expect(ringOf("idle")).toBeUndefined();
    expect(ringOf("offline")).toBeUndefined();
    expect(toneOf("idle")).toBe("neutral");
    expect(ringOf("working")).toBe("info");
    expect(toneOf("working")).toBe("info");
  });

  test("waiting on a person and being stopped are DIFFERENT tones", () => {
    // Both have stopped, and only one is a failure of the seat. Red is
    // reserved for a stop, amber for the one state that asks for a person.
    expect(ringOf("needs")).toBe("warning");
    expect(ringOf("stopped")).toBe("danger");
    expect(activityWord("needs")).toBe("needs you");
  });

  test("a label is the engine's words, with the reason and the pauser", () => {
    expect(labelOf({ id: "a", agent_id: "id-a", role: "Dev A", activity: "working" }, now)).toBe(
      "Working",
    );
    expect(labelOf({ id: "a", agent_id: "id-a", role: "Dev A", activity: "idle" }, now)).toBe(
      "Idle",
    );
    expect(labelOf(undefined, now)).toBe("No state from the engine yet");
    expect(
      labelOf(
        { id: "a", agent_id: "id-a", role: "Dev A", activity: "stopped", stopped_reason: "budget" },
        now,
      ),
    ).toBe("Stopped · budget");
    expect(
      labelOf(
        {
          id: "a",
          agent_id: "id-a",
          role: "Dev A",
          activity: "stopped",
          stopped_reason: "paused",
          paused: { by: "jane", by_kind: "human", at: "2026-01-01T11:48:00Z", stop_running: false },
        },
        now,
      ),
    ).toBe("Paused by jane · 12m");
    // THE PAUSER BY NAME where the chart knows them: `paused.by` is the name
    // the engine records them under — a bound person's seat handle — and
    // "Paused by jane-founder" names an address where a person is meant. A
    // key the chart does not hold — the login of somebody bound to no seat —
    // is drawn as it came.
    const paused = {
      id: "a",
      agent_id: "id-a",
      role: "Dev A",
      activity: "stopped" as const,
      stopped_reason: "paused" as const,
      paused: {
        by: "jane-founder",
        by_kind: "human",
        at: "2026-01-01T11:48:00Z",
        stop_running: false,
      },
    };
    const nameOf = (key: string) => (key === "jane-founder" ? "Jane Founder" : key);
    expect(labelOf(paused, now, nameOf)).toBe("Paused by Jane Founder · 12m");
    expect(stoppedLine(paused, nameOf)).toBe("paused by Jane Founder");
    expect(stateLine(paused, { now, nameOf })).toBe("Paused by Jane Founder · 12m");
    expect(
      labelOf(
        { ...paused, paused: { ...paused.paused, by: "ops.lead", by_kind: "operator" } },
        now,
        nameOf,
      ),
    ).toBe("Paused by ops.lead · 12m");
    // A reason this build does not know draws the word, never a guess.
    expect(
      labelOf(
        {
          id: "a",
          agent_id: "id-a",
          role: "Dev A",
          activity: "stopped",
          stopped_reason: "later" as never,
        },
        now,
      ),
    ).toBe("Stopped");
  });

  test("a state line describes the engine's state and never invents one", () => {
    expect(
      stateLine(
        {
          id: "a",
          agent_id: "id-a",
          role: "Dev A",
          activity: "working",
          current_phase: "execute",
          turn: { stage: "running", work_item: { id: "t", key: "ENG-412" } } as never,
        },
        { now },
      ),
    ).toBe("Executing ENG-412");
    // A FAN-OUT IN FLIGHT is the call the seat is waiting on: a `delegate`
    // with three tasks is three workers, on the turn's own item.
    const delegating = (args: string, name = "delegate") =>
      stateLine(
        {
          id: "a",
          agent_id: "id-a",
          role: "Dev A",
          activity: "working",
          current_phase: "execute",
          live_call: {
            phase: "execute",
            work_item: { backend: "native", id: "t", key: "ENG-405", project: "ENG" },
            running_call: { round: 2, name, arguments: args, started_at: "" },
          } as never,
        },
        { now },
      );
    const three = JSON.stringify({ tasks: [{ id: "a" }, { id: "b" }, { id: "c" }] });
    expect(delegating(three)).toBe("3 workers on ENG-405");
    // Any other call is the phase; arguments that are not the schema count
    // nothing rather than a guess.
    expect(delegating(three, "search_work_items")).toBe("Executing ENG-405");
    expect(delegating("not json")).toBe("Executing ENG-405");
    expect(delegating('{"tasks": "three"}')).toBe("Executing ENG-405");
    expect(
      stateLine(
        {
          id: "a",
          agent_id: "id-a",
          role: "Dev A",
          activity: "stopped",
          stopped_reason: "unplaced",
        },
        { now },
      ),
    ).toBe("Not placed on any node");
    expect(
      stateLine(
        {
          id: "a",
          agent_id: "id-a",
          role: "Dev A",
          activity: "idle",
          last_turn: { ended_at: "2026-01-01T11:36:00Z" } as never,
        },
        { now },
      ),
    ).toBe("Idle · last turn 24m ago");
    expect(stateLine(undefined, { now })).toBe("No state from the engine yet");
    expect(stateLine(null, { now, seat: named(index, "Jane Founder")! })).toMatch(/human|Human/);
  });

  test("a parked run is a coding run while working, and says so", () => {
    // A detached run parks the turn while the box works: no model call is in
    // flight, so the phase has nothing to say and the line names the run.
    expect(
      stateLine(
        {
          id: "a",
          agent_id: "id-a",
          role: "Dev A",
          activity: "working",
          current_phase: "execute",
          turn: { stage: "parked", work_item: { id: "t", key: "ENG-9" } } as never,
        },
        { now },
      ),
    ).toBe("Coding run on ENG-9");
    expect(
      labelOf(
        {
          id: "a",
          agent_id: "id-a",
          role: "Dev A",
          activity: "needs",
          turn: { stage: "parked" } as never,
        },
        now,
      ),
    ).toBe("Needs you · run parked");
  });

  test("a seat with no handle prints none, rather than a bare @", () => {
    expect(handleLabel("pm")).toBe("@pm");
    expect(handleLabel("")).toBe("");
    expect(handleLabel(undefined)).toBe("");
    expect(handleLabel("  ")).toBe("");
  });
});

/**
 * THE MAPPER IS THE ONLY READER, held over the whole tree.
 *
 * A screen that decides a seat is working because a live call is in flight,
 * or a run is attached, is the fold `activity` replaced — and it drifts from
 * the engine's own word on exactly the cases that matter (a parked run, a
 * run past its age-out). So no module but this one may turn a live call's or
 * a run's fields into one of the state words.
 */
describe("no screen derives a seat's state", () => {
  const WORDS = new Set(["working", "idle", "needs", "stopped", "offline"]);
  const SOURCES = /^(in_progress|live_call|sandboxes|runs|running_call|stage)$/;

  /** Whether an expression reads one of the fields a state could be folded from. */
  const reads = (node: Node | null | undefined): boolean => {
    if (!node) return false;
    let found = false;
    walk(node, (n) => {
      if (found) return false;
      const name = memberName(n) ?? (n.type === "Identifier" ? String(n.name) : null);
      if (name !== null && SOURCES.test(name)) found = true;
    });
    return found;
  };

  /** The expression under a type assertion: `{…} as const` is still `{…}`. */
  const bare = (node: Node | null | undefined): Node | null => {
    let n = node ?? null;
    while (n && /^TS(As|Satisfies|NonNull)Expression$|^TSTypeAssertion$/.test(n.type)) {
      n = n.expression as Node;
    }
    return n;
  };

  /** Whether an object literal maps anything to a state word. */
  const mapsToWord = (node: Node | null | undefined): boolean => {
    const obj = bare(node);
    if (obj?.type !== "ObjectExpression") return false;
    return (obj.properties as Node[]).some((p) => {
      const word = p.type === "Property" ? stringValue(bare(p.value as Node) ?? undefined) : null;
      return word !== null && WORDS.has(word);
    });
  };

  /**
   * Whether a branch HANDS BACK a state word: is one, returns one, or assigns
   * one — in any statement under it, but not inside a function it declares,
   * whose returns are that function's own.
   */
  const yieldsWord = (node: Node | null | undefined): boolean => {
    if (!node) return false;
    const direct = stringValue(bare(node) ?? undefined);
    if (direct !== null) return WORDS.has(direct);
    let found = false;
    walk(node, (n) => {
      if (found) return false;
      if (n !== node && /Function/.test(n.type)) return false;
      const value =
        n.type === "ReturnStatement"
          ? (n.argument as Node | null)
          : n.type === "AssignmentExpression"
            ? (n.right as Node)
            : n.type === "VariableDeclarator"
              ? (n.init as Node | null)
              : null;
      const word = value ? stringValue(bare(value) ?? undefined) : null;
      if (word !== null && WORDS.has(word)) found = true;
    });
    return found;
  };

  /**
   * Every place in one module's text that decides a state word from a call's
   * or a run's fields — in each shape a decision is written in: a ternary, an
   * `if`, a `switch`, a `&&`/`||`/`??`, and a LOOKUP (an object literal mapping
   * to a word, indexed by such a field, written inline or declared once and
   * indexed later).
   *
   * THE TERNARY WAS THE ONLY ONE THIS READ, which is the gate claiming more than
   * it held: `if (call.in_progress) return "working"` is the same fold in a
   * different spelling, and so is `STATE[run.status]`.
   */
  const offences = (text: string, lang: Lang): number[] => {
    const tree = parse(text, lang);
    const wordMaps = new Set<string>();
    walk(tree, (n) => {
      if (n.type !== "VariableDeclarator") return;
      const id = n.id as Node;
      if (id.type === "Identifier" && mapsToWord(n.init as Node)) wordMaps.add(String(id.name));
    });
    const at: number[] = [];
    walk(tree, (n) => {
      switch (n.type) {
        case "ConditionalExpression":
        case "IfStatement":
          if (
            reads(n.test as Node) &&
            (yieldsWord(n.consequent as Node) || yieldsWord(n.alternate as Node))
          ) {
            at.push(n.start);
          }
          return;
        case "SwitchStatement":
          if (
            reads(n.discriminant as Node) &&
            (n.cases as Node[]).some((c) => (c.consequent as Node[]).some(yieldsWord))
          ) {
            at.push(n.start);
          }
          return;
        case "LogicalExpression":
          if (reads(n.left as Node) && yieldsWord(n.right as Node)) at.push(n.start);
          return;
        case "MemberExpression": {
          if (!n.computed || !reads(n.property as Node)) return;
          const object = bare(n.object as Node);
          const named = object?.type === "Identifier" && wordMaps.has(String(object.name));
          if (named || mapsToWord(object)) at.push(n.start);
          return;
        }
      }
    });
    return at;
  };

  test("nothing but the mapper turns a call's or a run's fields into a state word", () => {
    const offenders: string[] = [];
    for (const mod of modules()) {
      if (mod.path === "lib/seats.ts" || mod.lang === "dts") continue;
      const line = lineOf(mod.text);
      for (const start of offences(mod.text, mod.lang))
        offenders.push(`${mod.path}:${line(start)}`);
    }
    expect(offenders, "a seat's state is `activity`, read through lib/seats.ts").toEqual([]);
  });

  // EVERY SHAPE IS CAUGHT, and the ordinary reads are not: a gate that goes
  // quiet on a spelling is one that certifies less than it says.
  test("the rule can tell, in every spelling a decision is written in", () => {
    const caught = (text: string) => offences(text, "ts").length > 0;
    expect(caught('const s = agent.live_call?.in_progress ? "working" : "idle";')).toBe(true);
    expect(caught('function f(c) { if (c.in_progress) return "working"; return "x"; }')).toBe(true);
    expect(
      caught('function f(c) { let s = "x"; if (c.live_call) { s = "working"; } return s; }'),
    ).toBe(true);
    expect(caught('function f(t) { switch (t.stage) { case "parked": return "needs"; } }')).toBe(
      true,
    );
    expect(caught('const s = agent.live_call && "working";')).toBe(true);
    expect(caught('const s = { parked: "needs", phase: "working" }[turn.stage];')).toBe(true);
    expect(caught('const BY = { parked: "needs" } as const; const s = BY[agent.turn.stage];')).toBe(
      true,
    );
    // …and the reads that are not a state at all.
    expect(caught('const s = agent.activity === "working" ? "Working" : "";')).toBe(false);
    expect(caught('const n = call.in_progress ? "running" : "done";')).toBe(false);
    expect(
      caught('function f(c) { if (c.in_progress) { const g = () => "idle"; return g; } }'),
    ).toBe(false);
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

  test("a parked turn is silent on purpose, and is never called stalled", () => {
    // A detached coding run stops the round by design for as long as the run
    // takes; the alarm keyed on "no update" fired on every one of them.
    const old = new Date(now - STALLED_MS - 1).toISOString();
    expect(staleness(old, now, "parked")).toBe("");
    expect(staleness(old, now, "running")).toBe("stalled");
  });
});

// ---------------------------------------------------------------------------
// The model chain, in both shapes the chart serves
// ---------------------------------------------------------------------------

// `org.ProviderKeys` marshals as a STRING for one provider and an ARRAY for a
// fallback chain. The client declared a string, so an array rendered as
// `fast,backup` and any consumer calling a string method on one threw on a
// seat the engine accepts.
describe("llmChain", () => {
  test("reads both shapes the engine marshals, first choice first", () => {
    expect(llmChain("fast")).toEqual(["fast"]);
    expect(llmChain(["fast", "backup"])).toEqual(["fast", "backup"]);
  });

  // A SEAT THAT SAYS NOTHING takes the default provider, and that is not the
  // same as a seat pinned to a provider called "".
  test("an unset field is an empty chain, not a chain of one empty key", () => {
    expect(llmChain(undefined)).toEqual([]);
    expect(llmChain("")).toEqual([]);
    expect(llmChain([])).toEqual([]);
    expect(llmChain(["", "fast"])).toEqual(["fast"]);
  });

  test("one key listed twice is one model", () => {
    expect(llmChain(["big", "tiny", "big"])).toEqual(["big", "tiny"]);
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
  // name is read as "how many there are"; this is what the sidebar's badge and
  // the unit page's fact both hand a reader instead.
  test("the total's hint names what it counted", () => {
    expect(UNIT_TOTAL_HINT).toContain("everything under it");
  });
});

// A TASK CARD'S STRIP NAMES THE ROUND THE STEPPER NAMES. It read the engine's
// zero-based `round_num` raw — "round 6 of 25" on a card beside "round 7 of
// 25" on the seat's own profile — and read the first round, 0, as no round.
test("a working seat's strip on a task counts its round from one", () => {
  const working = (round_num: number, rounds_used: number): AgentRow =>
    ({
      role: "SWE",
      handle: "swe",
      activity: "working",
      live_call: {
        phase: "execute",
        round_num,
        rounds_used,
        max_rounds: 25,
        work_item: { backend: "native", id: "i", key: "ENG-412", project: "ENG" },
      },
    }) as unknown as AgentRow;
  expect(liveOnItems([working(6, 6)]).get("i")?.doing).toMatch(/round 7 of 25$/);
  expect(liveOnItems([working(0, 0)]).get("i")?.doing).toMatch(/round 1 of 25$/);
});

// A TURN IS ON ONE TASK, AND A KEY CAN BE TWO. A key another task claimed
// first is flagged `key_collision` and stays on the task that did not claim
// it, so two cards can carry one key — and a map keyed on it drew the working
// seat's strip on both of them. Keyed on the task's id, only the task the turn
// is charged to finds one.
test("of two tasks sharing a key, only the one being worked on draws the strip", () => {
  const row = {
    role: "SWE",
    handle: "swe",
    activity: "working",
    live_call: {
      phase: "execute",
      round_num: 2,
      rounds_used: 2,
      max_rounds: 25,
      work_item: { backend: "native", id: "t-duplicate", key: "ENG-7", project: "ENG" },
    },
  } as unknown as AgentRow;
  const live = liveOnItems([row]);
  expect(live.get("t-duplicate")?.handle).toBe("swe");
  expect(live.has("t-claimant")).toBe(false);
  expect(live.has("ENG-7")).toBe(false);
});

// ONE READING OF THE ROUND, on the roster's card and in the attention queue
// too. `roundLabel` decoded `round_num` itself — a second reading beside the
// one the stepper, the peek and a task's strips share — so a call whose
// counters disagreed was "starting" on the card and "round 3" on the profile.
test("the roster's round label names the round the rest of the product names", () => {
  const calls = [
    { round_num: -1, rounds_used: 0 },
    { round_num: 0, rounds_used: 0 },
    { round_num: 6, rounds_used: 6 },
    { round_num: -1, rounds_used: 3 },
  ] as LiveCall[];
  for (const call of calls) {
    expect(roundLabel(call).text).toBe(`round ${roundOf(call)}`);
  }
  expect(roundLabel({ round_num: -1, rounds_used: 3 } as LiveCall).text).toBe("round 3");
  expect(roundLabel(null).text).toBe("starting");
});

// THE OPENING FRAME IS ROUND ONE. It is published immediately before the
// phase's first provider call and carries the cap so a row can say "round 1 of
// 24" before the model answers; read as round zero, a slow first answer drew
// Execute with no round at all while the task strip said "round 1".
test("a call whose first round has not come back is on round one", () => {
  const opening = { round_num: -1, rounds_used: 0, max_rounds: 24 } as LiveCall;
  expect(roundOf(opening)).toBe(1);
  expect(roundLabel(opening)).toEqual({
    text: "round 1",
    hint: "the first model round is in flight and has not come back",
  });
  expect(roundOf(null)).toBe(0);
});
