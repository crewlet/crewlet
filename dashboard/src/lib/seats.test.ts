// @vitest-environment node

/**
 * The org derivations every screen consumes, and the seat-state rules.
 *
 * These are resolved ONCE, into an index, because doing them per screen is how
 * the previous dashboard came to walk the whole roster once per rendered row:
 * its `managerOf` was a linear scan called per seat AND again per row, roughly
 * 80,000 array scans per event push on a 200-seat company.
 */

import { describe, expect, test } from "vitest";
import {
  indexOrg,
  runState,
  seatTone,
  slugify,
  statusLine,
  staleness,
  STALE_MS,
  STALLED_MS,
} from "./seats.ts";
import type { OrgTree, SandboxEntry } from "~/protocol/index.ts";

const org: OrgTree = {
  name: "Acme",
  roles: [
    { name: "Jane Founder", kind: "human", manages: ["CEO"], contact: { slack_user_id: "U1" } },
    { name: "CEO", handle: "ceo", manages: ["Engineering"] },
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
          mcp_env: { github: { GITHUB_TOKEN: "${BACKEND_TOKEN}" } },
        },
      ],
      mcp_env: { github: { GITHUB_HOST: "example.com" } },
    },
  ],
};

const index = indexOrg(org);

describe("flattening", () => {
  test("every role becomes a seat, wherever it sits", () => {
    expect(index.seats.map((s) => s.name).sort()).toEqual([
      "CEO",
      "Dev A",
      "Dev B",
      "Jane Founder",
      "VP Engineering",
    ]);
  });

  test("a handle is derived exactly as the engine derives it", () => {
    // A handle that differs from the seat's real one is a cross-link to a
    // page that does not exist — and the seat's durable id is derived from
    // it, so the two must agree.
    expect(index.byName.get("Dev A")?.handle).toBe("dev-a");
    expect(slugify("Sarah Chen")).toBe("sarah-chen");
    expect(index.byName.get("CEO")?.handle).toBe("ceo");
  });

  test("a unit with no lead INHERITS the nearest ancestor's", () => {
    // It behaves identically to an explicit lead everywhere in the engine, so
    // hiding the difference is how an operator comes to think a unit is
    // unmanaged.
    expect(index.byName.get("Dev A")?.unitLead).toBe("VP Engineering");
    expect(index.byName.get("Dev A")?.unit?.name).toBe("Backend");
    expect(index.byName.get("Dev A")?.unitChain.map((u) => u.name)).toEqual([
      "Engineering",
      "Backend",
    ]);
  });

  test("mcp_env merges DOWN the unit chain with the seat's own winning", () => {
    const env = index.byName.get("Dev A")?.mcpEnv.github;
    expect(env?.GITHUB_HOST).toBe("example.com");
    expect(env?.GITHUB_TOKEN).toBe("${BACKEND_TOKEN}");
  });
});

describe("the management graph", () => {
  test("a unit name in `manages` expands to every role beneath it", () => {
    // Including descendants: the CEO manages Engineering, which is the VP and
    // both Backend devs.
    const reports = index.reportsOf
      .get("CEO")
      ?.map((s) => s.name)
      .sort();
    expect(reports).toEqual(["Dev A", "Dev B", "VP Engineering"]);
  });

  test("a unit lead auto-manages anyone nobody else already does", () => {
    expect(index.managerOf.get("Dev A")?.name).toBe("CEO");
    expect(index.managerOf.get("CEO")?.name).toBe("Jane Founder");
  });

  test("a human seat holds a place in the hierarchy", () => {
    // Addressable-only: no runtime, no inbox, no LLM — but escalation has to
    // terminate at a person.
    const founder = index.byName.get("Jane Founder");
    expect(founder?.kind).toBe("human");
    expect(founder?.contact.slack_user_id).toBe("U1");
  });

  test("nobody manages themselves", () => {
    for (const [name, reports] of index.reportsOf) {
      expect(reports.map((r) => r.name)).not.toContain(name);
    }
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
