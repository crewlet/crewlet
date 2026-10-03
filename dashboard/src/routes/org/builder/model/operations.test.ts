// @vitest-environment node
/**
 * Operations: recorded with their preconditions, evaluated, applied.
 *
 * The invariants: recording captures what each change replaced; applying
 * changes only what the operation names and follows or clears the references
 * that named what it re-addressed or removed, reporting each; an operation
 * whose preconditions no longer hold is refused with every value a person
 * needs; a field another operation owns cannot be written around its rules;
 * and one address names one node.
 */

import { describe, expect, test } from "vitest";
import { cloneJson, getPath } from "./json.ts";
import { COMPANY_KEY, mintKey, type NodeKey } from "./keys.ts";
import { locate, type Draft } from "./draft.ts";
import { fromChart } from "./document.ts";
import {
  ApplyError,
  apply,
  describeOperation,
  evaluate,
  expandTemplate,
  holderOf,
  intentOf,
  isCredentialField,
  malformedReason,
  record,
  rekeyOperation,
  restateOnto,
  touchedKeys,
  type Intent,
  type Operation,
} from "./operations.ts";
import { chartOf, countingKeys, fixtureChart, fixtureSettings } from "./testkit.ts";

function fixture(): Draft {
  return fromChart(fixtureSettings(), fixtureChart());
}

function recordOk(draft: Draft, intent: Intent): Operation {
  const result = record(draft, intent);
  if (!result.ok)
    throw new Error(`expected ${intent.type} to record: ${result.refusal}: ${result.message}`);
  return result.op;
}

function run(draft: Draft, intent: Intent) {
  const op = recordOk(draft, intent);
  return { op, ...apply(draft, op) };
}

const refusal = (draft: Draft, intent: Intent) => {
  const result = record(draft, intent);
  return result.ok ? "recorded" : result.refusal;
};

const seat = (draft: Draft, key: NodeKey) => {
  const found = locate(draft, key);
  if (found?.kind !== "seat") throw new Error(`no seat ${key}`);
  return found.node.data;
};
const unit = (draft: Draft, key: NodeKey) => {
  const found = locate(draft, key);
  if (found?.kind !== "unit") throw new Error(`no unit ${key}`);
  return found.node.data;
};
const handlesIn = (draft: Draft, key: NodeKey) => {
  const found = locate(draft, key);
  if (found?.kind !== "unit") throw new Error(`no unit ${key}`);
  return found.node.roles.map((s) => s.data.handle);
};

describe("adding", () => {
  test("a seat lands in its unit where its handle sorts, under the key the handler minted", () => {
    const draft = fixture();
    const key = mintKey(countingKeys());
    const { draft: next } = run(draft, {
      type: "addSeat",
      key,
      placement: { parent: "unit:engineering" },
      data: { handle: "architect", name: "Architect" },
    });
    // The chart serves a unit's seats by handle, so the draft holds them that way.
    expect(handlesIn(next, "unit:engineering")).toEqual(["architect", "dev", "vp-engineering"]);
    expect(seat(next, key)).toEqual({ handle: "architect", name: "Architect" });
    // Control: the draft it was applied to is untouched.
    expect(handlesIn(draft, "unit:engineering")).toEqual(["dev", "vp-engineering"]);
  });

  test("a key that was not minted, or already names a node, is refused", () => {
    const draft = fixture();
    const data = { handle: "qa", name: "QA" };
    const at = { parent: COMPANY_KEY };
    expect(refusal(draft, { type: "addSeat", key: "seat:qa", placement: at, data })).toBe(
      "not_minted",
    );
    const key = mintKey(countingKeys());
    const added = run(draft, { type: "addSeat", key, placement: at, data }).draft;
    expect(
      refusal(added, { type: "addSeat", key, placement: at, data: { handle: "qa2", name: "QA" } }),
    ).toBe("key_in_use");
    // Control: a fresh key records.
    expect(
      refusal(added, {
        type: "addSeat",
        key: mintKey(countingKeys("x")),
        placement: at,
        data: { handle: "qa2", name: "QA" },
      }),
    ).toBe("recorded");
  });

  test("one address names one node: a held handle is refused, and a unit's key is a different namespace", () => {
    const draft = fixture();
    const add = (handle: string): Intent => ({
      type: "addSeat",
      key: mintKey(countingKeys(handle.replace(/[^a-z]/g, ""))),
      placement: { parent: COMPANY_KEY },
      data: { handle, name: "New" },
    });
    expect(refusal(draft, add("dev"))).toBe("address_in_use");
    expect(refusal(draft, add(" "))).toBe("no_address");
    // A unit keyed `sales` does not hold a seat's `sales`: the chart keeps
    // handles and keys apart.
    expect(refusal(draft, add("sales"))).toBe("recorded");
    expect(
      refusal(draft, {
        type: "addUnit",
        key: mintKey(countingKeys()),
        placement: { parent: COMPANY_KEY },
        data: { key: "sales", name: "More sales" },
      }),
    ).toBe("address_in_use");
  });

  test("a unit never carries lists into its own data, and a missing parent is refused", () => {
    const draft = fixture();
    const key = mintKey(countingKeys());
    const { op } = run(draft, {
      type: "addUnit",
      key,
      placement: { parent: "unit:engineering" },
      data: { key: "qa", name: "QA", roles: [{ handle: "x" }], children: [] } as never,
    });
    expect(op.type === "addUnit" && op.data).toEqual({ key: "qa", name: "QA" });
    expect(
      refusal(draft, {
        type: "addUnit",
        key: mintKey(countingKeys("y")),
        placement: { parent: "unit:nowhere" },
        data: { key: "qa", name: "QA" },
      }),
    ).toBe("missing_parent");
  });
});

describe("removing", () => {
  test("a seat's removal clears the lead and manages entries that named it, and its access level, each reported", () => {
    const draft = fixture();
    const lead = run(draft, { type: "remove", target: "seat:vp-engineering" });
    expect(unit(lead.draft, "unit:engineering").lead).toBeUndefined();
    expect(lead.report.cleared).toEqual([
      { kind: "lead", holder: "unit:engineering", from: "vp-engineering" },
    ]);

    const dev = run(draft, { type: "remove", target: "seat:dev" });
    expect(seat(dev.draft, "seat:vp-engineering").manages).toBeUndefined();
    expect(
      getPath(dev.draft.company, ["integrations", "gitlab", "provisioning", "access_levels"]),
    ).toEqual({ sre: "maintainer" });
    expect(dev.report.cleared).toEqual([
      { kind: "manages", holder: "seat:vp-engineering", from: "dev" },
      { kind: "gitlab_access_level", holder: COMPANY_KEY, from: "dev" },
    ]);
    // Control: a seat nothing names clears nothing.
    expect(run(draft, { type: "remove", target: "seat:designer" }).report.cleared).toEqual([
      { kind: "manages", holder: "seat:ceo", from: "designer" },
    ]);
    expect(run(draft, { type: "remove", target: "seat:account-executive" }).report.cleared).toEqual(
      [],
    );
  });

  test("a unit's removal takes its subtree and clears every entry naming what it held", () => {
    const draft = fixture();
    const { draft: next, report } = run(draft, { type: "remove", target: "unit:engineering" });
    expect(locate(next, "unit:platform")).toBeUndefined();
    expect(locate(next, "seat:sre")).toBeUndefined();
    expect(seat(next, "seat:ceo").manages).toBeUndefined();
    expect(report.cleared.map((c) => [c.kind, c.from])).toEqual([
      ["manages", "engineering"],
      ["manages", "designer"],
      ["gitlab_access_level", "dev"],
      ["gitlab_access_level", "sre"],
    ]);
    // Control: a sibling unit and its seats are untouched.
    expect(handlesIn(next, "unit:sales")).toEqual(["account-executive"]);
  });

  test("an entry that named a removed SEAT is cleared even when a unit of that address remains, and not the other way round", () => {
    // A `manages:` entry resolves to a seat first, so `platform` names the seat.
    let draft = run(fixture(), {
      type: "addSeat",
      key: mintKey(countingKeys()),
      placement: { parent: COMPANY_KEY },
      data: { handle: "platform", name: "Platform seat" },
    }).draft;
    draft = run(draft, {
      type: "setManages",
      target: "seat:ceo",
      manages: ["platform"],
    }).draft;
    const seatKey = holderOf(draft, "seat", "platform")!.node.key;

    const seatGone = run(draft, { type: "remove", target: seatKey }).draft;
    expect(seat(seatGone, "seat:ceo").manages).toBeUndefined();
    // Control: removing the UNIT leaves the entry, which named the seat.
    const unitGone = run(draft, { type: "remove", target: "unit:platform" }).draft;
    expect(seat(unitGone, "seat:ceo").manages).toEqual(["platform"]);
  });

  test("a reference written with a removed seat's old handle named that seat, and is cleared with it", () => {
    const draft = fromChart(
      null,
      chartOf({
        units: [{ key: "ops", name: "Ops", lead: "boss" }],
        seats: [
          { handle: "lead", name: "Lead", unit: "ops", former_handles: ["boss"] },
          { handle: "ceo", name: "CEO" },
        ],
        manages: { ceo: ["boss", "ops"] },
      }),
    );
    const { draft: next, report } = run(draft, { type: "remove", target: "seat:lead" });
    expect(unit(next, "unit:ops").lead).toBeUndefined();
    expect(seat(next, "seat:ceo").manages).toEqual(["ops"]);
    expect(report.cleared).toEqual([
      { kind: "lead", holder: "unit:ops", from: "boss" },
      { kind: "manages", holder: "seat:ceo", from: "boss" },
    ]);
    // Control: an alias somebody else has claimed since names them, and stays.
    const claimed = run(draft, {
      type: "addSeat",
      key: mintKey(countingKeys()),
      placement: { parent: COMPANY_KEY },
      data: { handle: "boss", name: "New boss" },
    }).draft;
    const kept = run(claimed, { type: "remove", target: "seat:lead" });
    expect(unit(kept.draft, "unit:ops").lead).toBe("boss");
    expect(kept.report.cleared).toEqual([]);
  });

  test("replacing the Datadog fallback travels with the removal, and only while Datadog is connected", () => {
    const draft = fixture();
    const { draft: next } = run(draft, { type: "remove", target: "seat:sre", routeTo: "ceo" });
    expect(getPath(next.company, ["integrations", "datadog", "route_to"])).toBe("ceo");
    const disconnected: Draft = { ...draft, company: { name: "Acme" } };
    expect(refusal(disconnected, { type: "remove", target: "seat:sre", routeTo: "ceo" })).toBe(
      "no_datadog",
    );
    // Control: without a replacement the removal records on either.
    expect(refusal(disconnected, { type: "remove", target: "seat:sre" })).toBe("recorded");
  });
});

describe("renaming", () => {
  test("a name is prose: renaming changes what a person reads and nothing any reference resolves", () => {
    const draft = fixture();
    const { draft: next, report } = run(draft, {
      type: "renameSeat",
      target: "seat:vp-engineering",
      name: "Head of Engineering",
    });
    expect(seat(next, "seat:vp-engineering")).toMatchObject({
      handle: "vp-engineering",
      name: "Head of Engineering",
    });
    expect(unit(next, "unit:engineering").lead).toBe("vp-engineering");
    expect(report).toEqual({ cleared: [], followed: [], stripped: [] });

    const units = run(draft, { type: "renameUnit", target: "unit:engineering", name: "R&D" });
    expect(unit(units.draft, "unit:engineering")).toMatchObject({
      key: "engineering",
      name: "R&D",
    });
    expect(seat(units.draft, "seat:ceo").manages).toEqual(["engineering", "designer"]);
    // Control: an unchanged name records nothing.
    expect(refusal(draft, { type: "renameSeat", target: "seat:dev", name: "Dev" })).toBe(
      "no_change",
    );
  });
});

describe("changing an address", () => {
  test("a seat's new handle follows the lead, the manages entries, the Datadog fallback and its access level", () => {
    const draft = fixture();
    const vp = run(draft, {
      type: "updateSeat",
      target: "seat:vp-engineering",
      set: [{ path: ["handle"], value: "vpe" }],
    });
    expect(unit(vp.draft, "unit:engineering").lead).toBe("vpe");
    expect(vp.report.followed).toEqual([
      { kind: "lead", holder: "unit:engineering", from: "vp-engineering", to: "vpe" },
    ]);

    const dev = run(draft, {
      type: "updateSeat",
      target: "seat:dev",
      set: [{ path: ["handle"], value: "zed" }],
    });
    expect(seat(dev.draft, "seat:vp-engineering").manages).toEqual(["zed"]);
    const levels = ["integrations", "gitlab", "provisioning", "access_levels"];
    expect(getPath(dev.draft.company, levels)).toEqual({ zed: "developer", sre: "maintainer" });
    // It sorts where the chart will serve it.
    expect(handlesIn(dev.draft, "unit:engineering")).toEqual(["vp-engineering", "zed"]);

    const sre = run(draft, {
      type: "updateSeat",
      target: "seat:sre",
      set: [{ path: ["handle"], value: "oncall" }],
    });
    expect(getPath(sre.draft.company, ["integrations", "datadog", "route_to"])).toBe("oncall");
    expect(sre.report.followed).toContainEqual({
      kind: "datadog_route_to",
      holder: COMPANY_KEY,
      from: "sre",
      to: "oncall",
    });
  });

  test("a held handle is refused, and a seat's own handle stated back is no change", () => {
    const draft = fixture();
    const to = (value: string): Intent => ({
      type: "updateSeat",
      target: "seat:dev",
      set: [{ path: ["handle"], value }],
    });
    expect(refusal(draft, to("sre"))).toBe("address_in_use");
    expect(refusal(draft, to("dev"))).toBe("no_change");
    expect(refusal(draft, to(""))).toBe("no_address");
    // Control: a free handle records.
    expect(refusal(draft, to("builder"))).toBe("recorded");
  });

  test("a unit's new key follows the manages entries naming it, but not one a seat of that address owns", () => {
    const draft = fixture();
    const eng = run(draft, {
      type: "updateUnit",
      target: "unit:engineering",
      set: [{ path: ["key"], value: "eng" }],
    });
    expect(seat(eng.draft, "seat:ceo").manages).toEqual(["eng", "designer"]);
    expect(eng.report.followed).toEqual([
      { kind: "manages", holder: "seat:ceo", from: "engineering", to: "eng" },
    ]);

    // A seat handled `sales` owns the entry `sales`, so re-keying the UNIT
    // leaves it: following it would silently widen a report to a whole unit.
    let owned = run(draft, {
      type: "addSeat",
      key: mintKey(countingKeys()),
      placement: { parent: COMPANY_KEY },
      data: { handle: "sales", name: "Sales seat" },
    }).draft;
    owned = run(owned, { type: "setManages", target: "seat:ceo", manages: ["sales"] }).draft;
    const rekeyed = run(owned, {
      type: "updateUnit",
      target: "unit:sales",
      set: [{ path: ["key"], value: "revenue" }],
    });
    expect(seat(rekeyed.draft, "seat:ceo").manages).toEqual(["sales"]);
    expect(rekeyed.report.followed).toEqual([]);
  });
});

describe("moving", () => {
  test("a move places the node under its new parent and records where it came from", () => {
    const draft = fixture();
    const { op, draft: next } = run(draft, {
      type: "move",
      target: "seat:dev",
      to: { parent: "unit:sales" },
    });
    expect(op).toMatchObject({
      from: { parent: "unit:engineering" },
      to: { parent: "unit:sales" },
    });
    expect(handlesIn(next, "unit:sales")).toEqual(["account-executive", "dev"]);
    // Control: a move to where it already sits is no change.
    expect(
      refusal(draft, { type: "move", target: "seat:dev", to: { parent: "unit:engineering" } }),
    ).toBe("no_change");
  });

  test("a unit cannot move into itself, and a destination that is not in the draft is refused", () => {
    const draft = fixture();
    expect(
      refusal(draft, { type: "move", target: "unit:engineering", to: { parent: "unit:platform" } }),
    ).toBe("into_itself");
    expect(refusal(draft, { type: "move", target: "seat:dev", to: { parent: "unit:gone" } })).toBe(
      "missing_parent",
    );
    // Control: a unit moves under a sibling.
    expect(
      refusal(draft, { type: "move", target: "unit:sales", to: { parent: "unit:engineering" } }),
    ).toBe("recorded");
  });

  test("clearing the leads a moving seat holds is part of the move, and conflicts once the lead changed", () => {
    const draft = fixture();
    const intent: Intent = {
      type: "move",
      target: "seat:vp-engineering",
      to: { parent: COMPANY_KEY },
      clearLeads: ["unit:engineering"],
    };
    const { op, draft: next, report } = run(draft, intent);
    expect(unit(next, "unit:engineering").lead).toBeUndefined();
    expect(report.cleared).toEqual([
      { kind: "lead", holder: "unit:engineering", from: "vp-engineering" },
    ]);
    const relead = run(draft, { type: "setLead", target: "unit:engineering", lead: "dev" }).draft;
    expect(evaluate(relead, op)).toEqual({
      kind: "conflict",
      conflicts: [{ subject: "lead", base: "vp-engineering", theirs: "dev", mine: undefined }],
    });
  });

  test("a move recorded from one parent conflicts once somebody moved the node elsewhere", () => {
    const draft = fixture();
    const op = recordOk(draft, { type: "move", target: "seat:dev", to: { parent: "unit:sales" } });
    const moved = run(draft, {
      type: "move",
      target: "seat:dev",
      to: { parent: "unit:platform" },
    }).draft;
    expect(evaluate(moved, op)).toEqual({
      kind: "conflict",
      conflicts: [
        {
          subject: "where it sits",
          shape: "parent",
          base: "unit:engineering",
          theirs: "unit:platform",
          mine: "unit:sales",
        },
      ],
    });
    // Control: the draft it was recorded on applies it.
    expect(evaluate(draft, op)).toEqual({ kind: "applies" });
  });
});

describe("editing", () => {
  test("an edit records only the fields that differ, with what they held, the runtime half included", () => {
    const draft = fixture();
    const op = recordOk(draft, {
      type: "updateSeat",
      target: "seat:dev",
      set: [
        { path: ["goal"], value: "Ship" },
        { path: ["backstory"], value: "Joined early." },
        { path: ["runtime", "llm"], value: "fast" },
        { path: ["runtime", "token_budget"], value: { day: 5000 } },
      ],
    });
    expect(op).toEqual({
      type: "updateSeat",
      target: "seat:dev",
      changes: [
        { path: ["goal"], before: "Build", after: "Ship" },
        { path: ["backstory"], after: "Joined early." },
        { path: ["runtime", "token_budget"], after: { day: 5000 } },
      ],
      accessLevels: [],
    });
    // The runtime key this build does not model survives the edit.
    const next = apply(draft, op).draft;
    expect(seat(next, "seat:dev").runtime).toEqual({
      llm: "fast",
      future_runtime_key: 7,
      token_budget: { day: 5000 },
    });
  });

  test("fields another operation owns cannot be written by an edit", () => {
    const draft = fixture();
    const seatSet = (path: string[], value: unknown): Intent => ({
      type: "updateSeat",
      target: "seat:dev",
      set: [{ path, value }],
    });
    for (const path of [["name"], ["kind"], ["manages"], ["former_handles"]]) {
      expect(refusal(draft, seatSet(path, ["x"]))).toBe("forbidden_field");
    }
    expect(refusal(draft, seatSet(["runtime", "schedules"], []))).toBe("forbidden_field");
    const unitSet = (path: string[], value: unknown): Intent => ({
      type: "updateUnit",
      target: "unit:engineering",
      set: [{ path, value }],
    });
    for (const path of [["name"], ["lead"], ["former_keys"]]) {
      expect(refusal(draft, unitSet(path, "x"))).toBe("forbidden_field");
    }
    // Control: the same seat's goal and the same unit's purpose record.
    expect(refusal(draft, seatSet(["goal"], "Ship"))).toBe("recorded");
    expect(refusal(draft, unitSet(["purpose"], "Build it"))).toBe("recorded");
  });

  test("an access level is recorded beside the seat and needs GitLab provisioning connected", () => {
    const draft = fixture();
    const op = recordOk(draft, {
      type: "updateSeat",
      target: "seat:designer",
      set: [],
      accessLevel: "reporter",
    });
    expect(op.type === "updateSeat" && op.accessLevels).toEqual([
      { handle: "designer", after: "reporter" },
    ]);
    const cleared = recordOk(draft, {
      type: "updateSeat",
      target: "seat:dev",
      set: [],
      accessLevel: null,
    });
    expect(cleared.type === "updateSeat" && cleared.accessLevels).toEqual([
      { handle: "dev", before: "developer" },
    ]);
    const disconnected: Draft = { ...draft, company: { name: "Acme" } };
    expect(
      refusal(disconnected, {
        type: "updateSeat",
        target: "seat:designer",
        set: [],
        accessLevel: "reporter",
      }),
    ).toBe("no_gitlab");
  });

  test("the charter is edited through its own operation and nothing outside it", () => {
    const draft = fixture();
    const { draft: next } = run(draft, {
      type: "updateCompany",
      set: [{ path: ["mission"], value: "Make better things." }],
    });
    expect(next.company.mission).toBe("Make better things.");
    expect(
      refusal(draft, { type: "updateCompany", set: [{ path: ["integrations"], value: {} }] }),
    ).toBe("forbidden_field");
    expect(
      refusal(draft, {
        type: "updateCompany",
        set: [{ path: ["mission"], value: "Make things." }],
      }),
    ).toBe("no_change");
  });

  test("a schedule toggle writes only a change the engine would act on", () => {
    const draft = fixture();
    const toggle = (enabled: boolean): Intent => ({
      type: "setScheduleEnabled",
      target: "unit:engineering",
      schedule: "standup",
      enabled,
    });
    // Unset runs the schedule, so enabling it changes nothing.
    expect(refusal(draft, toggle(true))).toBe("no_change");
    const { draft: off } = run(draft, toggle(false));
    expect(getPath(unit(off, "unit:engineering"), ["runtime", "schedules"])).toEqual([
      { name: "standup", cron: "0 9 * * 1-5", task: "Run standup", target: "lead", enabled: false },
    ]);
    // The runtime's other keys are kept.
    expect(getPath(unit(off, "unit:engineering"), ["runtime", "mcp_env"])).toEqual({
      tracker: { TOKEN: "${TRACKER_TOKEN}" },
    });
    expect(
      refusal(draft, {
        type: "setScheduleEnabled",
        target: "unit:engineering",
        schedule: "nothing",
        enabled: false,
      }),
    ).toBe("no_schedule");
  });

  test("the Datadog fallback is set only while Datadog is connected", () => {
    const draft = fixture();
    const { draft: next } = run(draft, { type: "setDatadogRouteTo", routeTo: "ceo" });
    expect(getPath(next.company, ["integrations", "datadog"])).toEqual({
      enabled: true,
      route_to: "ceo",
    });
    const disconnected: Draft = { ...draft, company: { name: "Acme" } };
    expect(refusal(disconnected, { type: "setDatadogRouteTo", routeTo: "ceo" })).toBe("no_datadog");
  });
});

describe("integration blocks", () => {
  // A block exists because somebody connected the integration. A seat edit
  // reaches one value inside it and never creates or deletes the block.
  test("removing the last access level or the fallback leaves the connected block standing", () => {
    let draft = fixture();
    draft = run(draft, {
      type: "updateSeat",
      target: "seat:dev",
      set: [],
      accessLevel: null,
    }).draft;
    draft = run(draft, {
      type: "updateSeat",
      target: "seat:sre",
      set: [],
      accessLevel: null,
    }).draft;
    draft = run(draft, { type: "setDatadogRouteTo" }).draft;
    expect(draft.company.integrations).toEqual({
      datadog: { enabled: true },
      gitlab: { provisioning: {} },
    });
  });

  test("an access level recorded before GitLab was disconnected upstream is gone, not written back", () => {
    const draft = fixture();
    const op = recordOk(draft, {
      type: "updateSeat",
      target: "seat:designer",
      set: [],
      accessLevel: "reporter",
    });
    const disconnected: Draft = { ...draft, company: { name: "Acme" } };
    expect(evaluate(disconnected, op)).toEqual({
      kind: "gone",
      reason: "GitLab provisioning is no longer connected.",
    });
  });
});

describe("changing kind", () => {
  test("becoming human strips every field a human seat may not carry, each recorded, and sets the contact", () => {
    const draft = fixture();
    const {
      op,
      draft: next,
      report,
    } = run(draft, {
      type: "changeKind",
      target: "seat:sre",
      kind: "human",
      contact: { slack_user_id: "U0SRE" },
    });
    expect(op).toMatchObject({
      after: "human",
      stripped: [{ path: ["project"], before: "OPS" }],
      contact: { slack_user_id: "U0SRE" },
    });
    expect(seat(next, "seat:sre")).toEqual({
      handle: "sre",
      name: "SRE",
      kind: "human",
      goal: "Keep it up",
      runtime: { contact: { slack_user_id: "U0SRE" } },
    });
    expect(report.stripped).toEqual(["project"]);

    const dev = run(draft, { type: "changeKind", target: "seat:dev", kind: "human" });
    // The runtime key this build does not model is not a forbidden field.
    expect(seat(dev.draft, "seat:dev").runtime).toEqual({ future_runtime_key: 7 });
    expect(dev.report.stripped).toEqual(["llm"]);
  });

  test("the seat's own GitHub App and its tool credentials are credentials a kind change removes", () => {
    expect(isCredentialField(["runtime", "github"])).toBe(true);
    expect(isCredentialField(["runtime", "mcp_env"])).toBe(true);
    expect(isCredentialField(["runtime", "slack"])).toBe(true);
    // Control: a model chain is not a credential.
    expect(isCredentialField(["runtime", "llm"])).toBe(false);
  });

  test("becoming an agent strips the contact and the availability", () => {
    let draft = run(fixture(), {
      type: "changeKind",
      target: "seat:designer",
      kind: "human",
      contact: { email: "d@example.com" },
    }).draft;
    draft = run(draft, {
      type: "updateSeat",
      target: "seat:designer",
      set: [{ path: ["runtime", "availability"], value: "weekdays" }],
    }).draft;
    const { draft: next, report } = run(draft, {
      type: "changeKind",
      target: "seat:designer",
      kind: "agent",
    });
    expect(seat(next, "seat:designer")).toEqual({
      handle: "designer",
      name: "Designer",
      goal: "Design",
    });
    expect(report.stripped).toEqual(["contact", "availability"]);
    // Control: an agent seat already an agent is no change.
    expect(refusal(next, { type: "changeKind", target: "seat:designer", kind: "agent" })).toBe(
      "no_change",
    );
  });
});

describe("evaluating", () => {
  test("a changed precondition is a conflict carrying the base value, their value and this operation's", () => {
    const draft = fixture();
    const op = recordOk(draft, {
      type: "updateSeat",
      target: "seat:dev",
      set: [{ path: ["goal"], value: "Ship" }],
    });
    const theirs = run(draft, {
      type: "updateSeat",
      target: "seat:dev",
      set: [{ path: ["goal"], value: "Refactor" }],
    }).draft;
    expect(evaluate(theirs, op)).toEqual({
      kind: "conflict",
      conflicts: [{ subject: "goal", base: "Build", theirs: "Refactor", mine: "Ship" }],
    });
    expect(() => apply(theirs, op)).toThrow(ApplyError);
  });

  // A CONFLICT'S VALUES ARE NOT ALWAYS A FIELD'S. A removal's whole node, a
  // placement and a kind change's stripped fields are the builder's own
  // structures, so each says which it is and a view can name what it is about
  // rather than print keys and credential references.
  test("a conflict over the builder's own structures says which structure it holds", () => {
    const draft = fixture();
    const shapes = (theirs: Draft, op: Operation) => {
      const outcome = evaluate(theirs, op);
      if (outcome.kind !== "conflict") throw new Error(`expected a conflict, got ${outcome.kind}`);
      return outcome.conflicts.map((c) => [c.subject, c.shape]);
    };
    const editDev: Intent = {
      type: "updateSeat",
      target: "seat:dev",
      set: [{ path: ["goal"], value: "Ship" }],
    };
    const removal = recordOk(draft, { type: "remove", target: "seat:dev" });
    expect(shapes(run(draft, editDev).draft, removal)).toEqual([["the whole seat", "snapshot"]]);

    const toHuman = recordOk(draft, { type: "changeKind", target: "seat:sre", kind: "human" });
    const trackerMoved = run(draft, {
      type: "updateSeat",
      target: "seat:sre",
      set: [{ path: ["project"], value: "SUP" }],
    }).draft;
    expect(shapes(trackerMoved, toHuman)).toContainEqual(["fields the new kind removes", "fields"]);

    // A field's own values carry no shape: they are what the chart stores.
    const refactored = run(draft, {
      type: "updateSeat",
      target: "seat:dev",
      set: [{ path: ["goal"], value: "Refactor" }],
    }).draft;
    expect(shapes(refactored, recordOk(draft, editDev))).toEqual([["goal", undefined]]);
  });

  test("an address somebody else took since the operation was recorded is a conflict naming it", () => {
    const draft = fixture();
    const mine = recordOk(draft, {
      type: "addSeat",
      key: mintKey(countingKeys("m")),
      placement: { parent: COMPANY_KEY },
      data: { handle: "qa", name: "Mine" },
    });
    const theirs = run(draft, {
      type: "addSeat",
      key: mintKey(countingKeys("t")),
      placement: { parent: "unit:sales" },
      data: { handle: "qa", name: "Theirs" },
    }).draft;
    const outcome = evaluate(theirs, mine);
    expect(outcome).toEqual({
      kind: "conflict",
      conflicts: [
        {
          subject: "the handle qa",
          shape: "snapshot",
          address: "qa",
          base: undefined,
          theirs: { handle: "qa", name: "Theirs" },
          mine: { handle: "qa", name: "Mine" },
        },
      ],
    });
    // Control: a different address applies.
    expect(evaluate(draft, mine)).toEqual({ kind: "applies" });
  });

  test("a missing target is gone, and an edit to one field is untouched by an upstream edit of another", () => {
    const draft = fixture();
    const lead = recordOk(draft, {
      type: "setLead",
      target: "unit:sales",
      lead: "account-executive",
    });
    const removed = run(draft, { type: "remove", target: "unit:sales" }).draft;
    expect(evaluate(removed, lead).kind).toBe("gone");

    const goal = recordOk(draft, {
      type: "updateSeat",
      target: "seat:dev",
      set: [{ path: ["goal"], value: "Ship" }],
    });
    const backstory = run(draft, {
      type: "updateSeat",
      target: "seat:dev",
      set: [{ path: ["backstory"], value: "Joined early." }],
    }).draft;
    expect(evaluate(backstory, goal)).toEqual({ kind: "applies" });
  });
});

describe("an operation as data", () => {
  test("recording again from its intent reproduces it on the same draft", () => {
    const draft = fixture();
    const intents: Intent[] = [
      { type: "remove", target: "seat:sre", routeTo: "ceo" },
      {
        type: "move",
        target: "seat:vp-engineering",
        to: { parent: COMPANY_KEY },
        clearLeads: ["unit:engineering"],
      },
      { type: "updateSeat", target: "seat:dev", set: [{ path: ["handle"], value: "zed" }] },
      { type: "updateSeat", target: "seat:designer", set: [], accessLevel: "reporter" },
      { type: "setManages", target: "seat:ceo", manages: ["dev"] },
      {
        type: "changeKind",
        target: "seat:sre",
        kind: "human",
        contact: { email: "s@example.com" },
      },
      {
        type: "setScheduleEnabled",
        target: "unit:engineering",
        schedule: "standup",
        enabled: false,
      },
      { type: "setDatadogRouteTo" },
    ];
    for (const intent of intents) {
      const op = recordOk(draft, intent);
      expect(recordOk(draft, intentOf(op))).toEqual(op);
      // It survives the storage round trip unchanged.
      expect(JSON.parse(JSON.stringify(op))).toEqual(op);
    }
  });

  test("an operation that could never have been recorded is malformed", () => {
    expect(
      malformedReason({
        type: "updateSeat",
        target: "seat:dev",
        changes: [{ path: ["kind"], after: "human" }],
        accessLevels: [],
      }),
    ).toBe("a seat edit writes a field its own action owns");
    expect(
      malformedReason({
        type: "addSeat",
        key: "seat:dev",
        placement: { parent: COMPANY_KEY },
        data: { handle: "dev", name: "Dev" },
      }),
    ).toBe("a created node has no minted key");
    expect(
      malformedReason({
        type: "applyTemplate",
        template: "new_company",
        charter: { name: "Acme" },
        roles: [
          { key: "new:a", data: { handle: "ceo", name: "CEO" } },
          { key: "new:b", data: { handle: "ceo", name: "Chief" } },
        ],
        units: [],
      }),
    ).toBe("a template names an address twice");
    expect(
      malformedReason({
        type: "updateCompany",
        changes: [{ path: ["integrations"], after: {} }],
      }),
    ).toBe("a company edit writes outside the charter");
    // Control: what recording produces is well formed.
    const op = recordOk(fixture(), {
      type: "updateSeat",
      target: "seat:dev",
      set: [{ path: ["goal"], value: "Ship" }],
    });
    expect(malformedReason(op)).toBeNull();
  });

  test("describes itself in one sentence and names what to focus", () => {
    const draft = fixture();
    const say = (intent: Intent) => describeOperation(recordOk(draft, intent), draft);
    expect(say({ type: "remove", target: "unit:engineering" })).toBe(
      "Removed unit Engineering and 4 seats in it.",
    );
    expect(say({ type: "setLead", target: "unit:sales", lead: "account-executive" })).toBe(
      "Set the lead of Sales to Account Executive.",
    );
    expect(say({ type: "move", target: "seat:dev", to: { parent: "unit:sales" } })).toBe(
      "Moved Dev to Sales.",
    );
    expect(
      say({
        type: "updateSeat",
        target: "seat:dev",
        set: [{ path: ["runtime", "llm"], value: "slow" }],
      }),
    ).toBe("Edited Dev: llm.");
    const move = recordOk(draft, {
      type: "move",
      target: "seat:dev",
      to: { parent: "unit:sales" },
    });
    expect(touchedKeys(move)).toEqual(["seat:dev", "unit:sales", "unit:engineering"]);
  });

  test("applying never mutates the draft it was given", () => {
    const draft = fixture();
    const before = cloneJson(draft);
    run(draft, { type: "remove", target: "unit:engineering" });
    run(draft, {
      type: "updateSeat",
      target: "seat:dev",
      set: [{ path: ["handle"], value: "zed" }],
    });
    run(draft, { type: "changeKind", target: "seat:sre", kind: "human" });
    expect(draft).toEqual(before);
  });
});

describe("rebasing an add the chart already holds", () => {
  test("re-keying names the node by its address from then on, and leaves an operation naming none of it alone", () => {
    const key = mintKey(countingKeys());
    const keys = new Map([[key, "seat:qa"]]);
    const edit: Operation = {
      type: "updateSeat",
      target: key,
      changes: [{ path: ["goal"], after: "Test" }],
      accessLevels: [],
    };
    expect(rekeyOperation(edit, keys)).toEqual({ ...edit, target: "seat:qa" });
    const other: Operation = { ...edit, target: "seat:dev" };
    expect(rekeyOperation(other, keys)).toEqual(other);
  });

  test("restating writes this draft's node over the one holding its address, as recorded edits", () => {
    const draft = fixture();
    // An add recorded on a draft where `dev` was free, met by one that holds it.
    const add: Operation = {
      type: "addSeat",
      key: mintKey(countingKeys()),
      placement: { parent: "unit:sales" },
      data: { handle: "dev", name: "Developer", goal: "Sell", kind: "human" },
    };
    expect(evaluate(draft, add).kind).toBe("conflict");
    // "Keep mine" restates it onto the holder.
    const ops = restateOnto(draft, add, "seat:dev");
    expect(ops.map((op) => op.type)).toEqual(["move", "changeKind", "edit"]);
    let next = draft;
    for (const op of ops) next = apply(next, op).draft;
    expect(seat(next, "seat:dev")).toMatchObject({
      name: "Developer",
      goal: "Sell",
      kind: "human",
    });
    expect(handlesIn(next, "unit:sales")).toEqual(["account-executive", "dev"]);
  });

  test("a template becomes a charter edit and one add per node once its company exists", () => {
    const draft = fromChart({}, null);
    const template: Operation = {
      type: "applyTemplate",
      template: "new_company",
      charter: { name: "Acme", mission: "Make things." },
      roles: [{ key: "new:ceo", data: { handle: "ceo", name: "CEO" } }],
      units: [
        {
          key: "new:eng",
          data: { key: "engineering", name: "Engineering" },
          roles: [{ key: "new:dev", data: { handle: "dev", name: "Dev" } }],
          children: [],
        },
      ],
    };
    expect(expandTemplate(draft, template).map((op) => op.type)).toEqual([
      "updateCompany",
      "addSeat",
      "addUnit",
      "addSeat",
    ]);
  });
});

describe("editing a node in one operation", () => {
  test("a form's changes to one seat record as one edit that applies every part and follows references", () => {
    const draft = fixture();
    const {
      op,
      draft: next,
      report,
    } = run(draft, {
      type: "edit",
      target: "seat:vp-engineering",
      intents: [
        { type: "renameSeat", target: "seat:vp-engineering", name: "VP" },
        {
          type: "updateSeat",
          target: "seat:vp-engineering",
          set: [
            { path: ["handle"], value: "vp" },
            { path: ["goal"], value: "Grow the team" },
          ],
        },
        { type: "setManages", target: "seat:vp-engineering", manages: ["dev", "sre"] },
      ],
    });
    expect(op.type).toBe("edit");
    expect(seat(next, "seat:vp-engineering")).toMatchObject({
      handle: "vp",
      name: "VP",
      goal: "Grow the team",
      manages: ["dev", "sre"],
    });
    expect(unit(next, "unit:engineering").lead).toBe("vp");
    expect(report.followed).toEqual([
      { kind: "lead", holder: "unit:engineering", from: "vp-engineering", to: "vp" },
    ]);
  });

  test("a refused part refuses the whole edit, and an unchanged part drops out", () => {
    const draft = fixture();
    expect(
      refusal(draft, {
        type: "edit",
        target: "seat:dev",
        intents: [
          { type: "renameSeat", target: "seat:dev", name: "Developer" },
          { type: "updateSeat", target: "seat:dev", set: [{ path: ["handle"], value: "sre" }] },
        ],
      }),
    ).toBe("address_in_use");
    const single = recordOk(draft, {
      type: "edit",
      target: "seat:dev",
      intents: [
        { type: "renameSeat", target: "seat:dev", name: "Dev" },
        { type: "updateSeat", target: "seat:dev", set: [{ path: ["goal"], value: "Ship" }] },
      ],
    });
    // A single change records as itself.
    expect(single.type).toBe("updateSeat");
    expect(
      refusal(draft, {
        type: "edit",
        target: "seat:dev",
        intents: [{ type: "renameSeat", target: "seat:dev", name: "Dev" }],
      }),
    ).toBe("no_change");
  });

  test("a part for another node, or a change no editor makes, is refused", () => {
    const draft = fixture();
    expect(
      refusal(draft, {
        type: "edit",
        target: "seat:dev",
        intents: [
          { type: "renameSeat", target: "seat:dev", name: "Developer" },
          { type: "renameSeat", target: "seat:sre", name: "Ops" },
        ],
      }),
    ).toBe("not_editable");
    expect(
      refusal(draft, {
        type: "edit",
        target: "seat:dev",
        intents: [{ type: "move", target: "seat:dev", to: { parent: COMPANY_KEY } } as never],
      }),
    ).toBe("not_editable");
  });

  test("every conflicting part of an edit is reported together, and a removed node is gone", () => {
    const draft = fixture();
    const op = recordOk(draft, {
      type: "edit",
      target: "seat:dev",
      intents: [
        { type: "renameSeat", target: "seat:dev", name: "Developer" },
        { type: "updateSeat", target: "seat:dev", set: [{ path: ["goal"], value: "Ship" }] },
      ],
    });
    let theirs = run(draft, { type: "renameSeat", target: "seat:dev", name: "Engineer" }).draft;
    theirs = run(theirs, {
      type: "updateSeat",
      target: "seat:dev",
      set: [{ path: ["goal"], value: "Refactor" }],
    }).draft;
    const outcome = evaluate(theirs, op);
    expect(outcome.kind === "conflict" && outcome.conflicts.map((c) => c.subject)).toEqual([
      "name",
      "goal",
    ]);
    const gone = run(draft, { type: "remove", target: "seat:dev" }).draft;
    expect(evaluate(gone, op).kind).toBe("gone");
  });
});
