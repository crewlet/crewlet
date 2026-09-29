// @vitest-environment node
/**
 * Saving: the plan a draft becomes, and what each answer means.
 *
 * What these protect: a draft with nothing to write sends nothing; the steps
 * go in an order every one of them can land in — the settings first when they
 * create the company and last when they name the chart's handles, structure
 * before content, placements before removals, a unit's contents out before the
 * unit; a batch never exceeds the chart's cap; a content write states the
 * whole post-state of its object and the runtime half only when it changed,
 * so a lead correcting a goal is asked for nothing they do not hold and a
 * MASKED credential read from the chart goes back exactly as it came; every
 * step carries the id a retry must resend; and an answer is read as applied,
 * pending, unknown (with the id to retry), a conflict, or a refusal naming the
 * node and the fields it is about.
 */

import { describe, expect, test } from "vitest";
import type { ChartOperation } from "~/protocol/index.ts";
import { COMPANY_KEY } from "./keys.ts";
import { EMPTY_DRAFT, type Draft } from "./draft.ts";
import { fromChart } from "./document.ts";
import { apply, record, type Intent } from "./operations.ts";
import {
  MAX_BATCH_OPERATIONS,
  classifyStep,
  createdBy,
  furthestPosition,
  hasSaveSteps,
  landed,
  parsePosition,
  planSave,
  type SaveStep,
} from "./save.ts";
import { chartOf, fixtureChart, fixtureSettings } from "./testkit.ts";

const REDACTED = "__redacted__";

function run(draft: Draft, ...intents: Intent[]): Draft {
  for (const intent of intents) {
    const result = record(draft, intent);
    if (!result.ok) throw new Error(`${intent.type}: ${result.message}`);
    draft = apply(draft, result.op).draft;
  }
  return draft;
}

const base = () => fromChart(fixtureSettings(), fixtureChart());

function plan(draft: Draft, from: Draft = base(), runtimeVisible = true) {
  return planSave({
    mode: "edit",
    baseRevision: "rev-1",
    baseSettings: fixtureSettings(),
    baseDraft: from,
    draft,
    runtimeVisible,
    writeId: "write-0001",
    summary: "Saved (write write-0001)",
  }).steps;
}

const ops = (step: SaveStep) =>
  (step.request.body as { operations: ChartOperation[] }).operations.map((op) =>
    [op.kind, op.object.id, op.parent ?? op.to ?? op.lead ?? op.seat_kind ?? op.manages]
      .filter((x) => x !== undefined)
      .join(" "),
  );

const shape = (steps: readonly SaveStep[]) =>
  steps.map((s) => `${s.kind} ${s.request.method} ${s.request.path}`);

describe("what a save sends", () => {
  test("a draft with nothing to write sends nothing, and one change sends its one write", () => {
    expect(plan(base())).toEqual([]);
    expect(
      hasSaveSteps({
        mode: "edit",
        baseRevision: "rev-1",
        baseSettings: fixtureSettings(),
        baseDraft: base(),
        draft: base(),
        runtimeVisible: true,
      }),
    ).toBe(false);
    const edited = run(base(), {
      type: "updateSeat",
      target: "seat:dev",
      set: [{ path: ["goal"], value: "Ship" }],
    });
    expect(shape(plan(edited))).toEqual(["seat PATCH /chart/seats/dev"]);
  });

  test("create mode writes the settings first and whole, then the chart they hold", () => {
    const draft = run(
      EMPTY_DRAFT,
      { type: "updateCompany", set: [{ path: ["name"], value: "Acme" }] },
      {
        type: "addUnit",
        key: "new:u",
        placement: { parent: COMPANY_KEY },
        data: { key: "eng", name: "Engineering", lead: "cto" },
      },
      {
        type: "addSeat",
        key: "new:s",
        placement: { parent: "new:u" },
        data: { handle: "cto", name: "CTO", goal: "Build" },
      },
    );
    const steps = planSave({
      mode: "create",
      baseRevision: null,
      baseSettings: null,
      baseDraft: EMPTY_DRAFT,
      draft,
      runtimeVisible: true,
      writeId: "write-0001",
      summary: "Created",
    }).steps;
    expect(shape(steps)).toEqual([
      "settings PUT /config",
      "structure POST /chart/batch",
      "unit PATCH /chart/units/eng",
      "seat PATCH /chart/seats/cto",
    ]);
    expect(steps[0]!.request.headers).toEqual({ "If-None-Match": "*" });
    expect(steps[0]!.request.body).toEqual({ name: "Acme", _summary: "Created" });
    expect(ops(steps[1]!)).toEqual(["create_unit eng cto", "create_seat cto eng"]);
    expect(createdBy(steps)).toEqual([
      { key: "new:u", kind: "unit", address: "eng" },
      { key: "new:s", kind: "seat", address: "cto" },
    ]);
    // Control: a create draft holding nothing writes nothing — not even a nameless company.
    expect(
      planSave({
        mode: "create",
        baseRevision: null,
        baseSettings: null,
        baseDraft: EMPTY_DRAFT,
        draft: EMPTY_DRAFT,
        runtimeVisible: true,
        writeId: "write-0001",
        summary: "",
      }).steps,
    ).toEqual([]);
  });

  test("edit mode writes the settings last, as a merge patch on the revision it was made on", () => {
    const draft = run(
      base(),
      { type: "updateSeat", target: "seat:sre", set: [{ path: ["handle"], value: "oncall" }] },
      { type: "updateCompany", set: [{ path: ["mission"], value: "Make more." }] },
    );
    const steps = plan(draft);
    expect(shape(steps)).toEqual(["structure POST /chart/batch", "settings PATCH /config"]);
    expect(ops(steps[0]!)).toEqual(["rename sre oncall"]);
    const settings = steps[1]!.request;
    expect(settings.headers).toEqual({ "If-Match": '"rev-1"' });
    // The Datadog fallback followed the rename, and names the handle the chart holds after it.
    expect(settings.body).toMatchObject({
      mission: "Make more.",
      integrations: {
        datadog: { route_to: "oncall" },
        gitlab: { provisioning: { access_levels: { sre: null, oncall: "maintainer" } } },
      },
    });
  });
});

describe("the order of the structure", () => {
  test("renames first, units before the seats they hold, then the kinds, leads and reports", () => {
    const draft = run(
      base(),
      { type: "updateUnit", target: "unit:sales", set: [{ path: ["key"], value: "revenue" }] },
      {
        type: "addUnit",
        key: "new:u",
        placement: { parent: "unit:sales" },
        data: { key: "partners", name: "Partners" },
      },
      {
        type: "addSeat",
        key: "new:s",
        placement: { parent: "new:u" },
        data: { handle: "partner-lead", name: "Partner lead" },
      },
      { type: "move", target: "seat:designer", to: { parent: "new:u" } },
      { type: "changeKind", target: "seat:account-executive", kind: "human" },
      { type: "setLead", target: "new:u", lead: "partner-lead" },
      { type: "setLead", target: "unit:sales", lead: "account-executive" },
      { type: "setManages", target: "seat:ceo", manages: ["engineering", "designer", "revenue"] },
    );
    const [structure] = plan(draft);
    expect(ops(structure!)).toEqual([
      "rename sales revenue",
      "create_unit partners revenue",
      "move designer partners",
      "create_seat partner-lead partners",
      "set_kind account-executive human",
      "set_lead revenue account-executive",
      "set_manages ceo engineering,designer,revenue",
    ]);
    // Each operation's node, so a refusal naming operation N names its node.
    expect(structure!.nodes).toEqual([
      "unit:sales",
      "new:u",
      "seat:designer",
      "new:s",
      "seat:account-executive",
      "unit:sales",
      "seat:ceo",
    ]);
  });

  test("a move that would close a cycle waits for the move that opens it", () => {
    // Platform sits in Engineering; the draft puts Engineering in Platform.
    const draft = run(
      base(),
      { type: "move", target: "unit:platform", to: { parent: COMPANY_KEY } },
      { type: "move", target: "unit:engineering", to: { parent: "unit:platform" } },
    );
    const [structure] = plan(draft);
    expect(ops(structure!)).toEqual(["move platform", "move engineering platform"]);
  });

  test("removals go in a batch of their own after the placements, a unit's contents before it", () => {
    const draft = run(
      base(),
      { type: "move", target: "seat:sre", to: { parent: "unit:sales" } },
      { type: "remove", target: "unit:engineering" },
    );
    const steps = plan(draft);
    expect(steps.map((s) => s.kind)).toEqual(["structure", "removal", "settings"]);
    expect(ops(steps[0]!)).toEqual(["move sre sales", "set_manages ceo"]);
    expect(ops(steps[1]!)).toEqual([
      "remove dev",
      "remove vp-engineering",
      "remove designer",
      "remove platform",
      "remove engineering",
    ]);
  });

  test("no batch carries more operations than the chart takes in one record", () => {
    let draft = base();
    for (let i = 0; i < MAX_BATCH_OPERATIONS + 1; i++) {
      draft = run(draft, {
        type: "addSeat",
        key: `new:n${i}`,
        placement: { parent: COMPANY_KEY },
        data: { handle: `n${i}`, name: `N${i}` },
      });
    }
    const batches = plan(draft).filter((s) => s.kind === "structure");
    expect(batches.map((b) => b.nodes.length)).toEqual([MAX_BATCH_OPERATIONS, 1]);
  });
});

describe("a content write", () => {
  test("states the whole post-state, and the runtime half only when it changed", () => {
    const goal = plan(
      run(base(), {
        type: "updateSeat",
        target: "seat:dev",
        set: [{ path: ["goal"], value: "Ship" }],
      }),
    );
    expect(goal[0]!.request.body).toEqual({
      unit: "engineering",
      name: "Dev",
      email: "",
      backstory: "",
      goal: "Ship",
      responsibilities: [],
      behavioral_guidelines: [],
      project: "",
      space: "",
    });
    const llm = plan(
      run(base(), {
        type: "updateSeat",
        target: "seat:dev",
        set: [{ path: ["runtime", "llm"], value: "slow" }],
      }),
    );
    expect(llm[0]!.request.body).toMatchObject({
      goal: "Build",
      runtime: { llm: "slow", future_runtime_key: 7 },
    });
    const cleared = plan(
      run(base(), { type: "changeKind", target: "seat:dev", kind: "human" }),
    ).find((s) => s.kind === "seat")!;
    expect(cleared.request.body).toMatchObject({ runtime: { future_runtime_key: 7 } });
    const unit = plan(
      run(base(), {
        type: "setScheduleEnabled",
        target: "unit:engineering",
        schedule: "standup",
        enabled: false,
      }),
    );
    expect(shape(unit)).toEqual(["unit PATCH /chart/units/engineering"]);
    expect(unit[0]!.request.body).toMatchObject({
      name: "Engineering",
      runtime: { mcp_env: { tracker: { TOKEN: "${TRACKER_TOKEN}" } } },
    });
  });

  test("taking the runtime half away is its own clear, and a reader never shown it never states one", () => {
    const b = fromChart(
      null,
      chartOf({ seats: [{ handle: "x", name: "X", runtime: { llm: "fast" } }] }),
    );
    const none = run(b, {
      type: "updateSeat",
      target: "seat:x",
      set: [{ path: ["runtime", "llm"] }],
    });
    expect(plan(none, b)[0]!.request.body).toMatchObject({ clear_runtime: true });
    expect(plan(none, b)[0]!.request.body).not.toHaveProperty("runtime");

    const stripped = fromChart(null, chartOf({ seats: [{ handle: "x", name: "X" }] }, false));
    const goal = run(stripped, {
      type: "updateSeat",
      target: "seat:x",
      set: [{ path: ["goal"], value: "Ship" }],
    });
    const body = plan(goal, stripped, false)[0]!.request.body;
    expect(body).not.toHaveProperty("runtime");
    expect(body).not.toHaveProperty("clear_runtime");
  });

  test("a created node is always written, runtime included, and a moved one only when its content changed", () => {
    const draft = run(
      base(),
      {
        type: "addSeat",
        key: "new:q",
        placement: { parent: "unit:sales" },
        data: { handle: "qa", name: "QA", runtime: { llm: "fast" } },
      },
      { type: "move", target: "seat:dev", to: { parent: "unit:sales" } },
    );
    const content = plan(draft).filter((s) => s.kind === "seat");
    expect(shape(content)).toEqual(["seat PATCH /chart/seats/qa"]);
    expect(content[0]!.request.body).toMatchObject({
      unit: "sales",
      name: "QA",
      runtime: { llm: "fast" },
    });
  });

  // THE CHART SERVES EVERY CREDENTIAL MASKED — a sealed one as its `${VAR}`
  // reference, anything else as the mask, and a seat's address the same way —
  // and a write handing a mask back is restored from the row. So what the
  // builder read is what it sends back, byte for byte, until somebody edits it.
  test("a masked credential and a masked address go back exactly as they were read", () => {
    const masked = fromChart(
      fixtureSettings(),
      chartOf({
        seats: [
          {
            handle: "ops",
            name: "Ops",
            email: REDACTED,
            runtime: {
              llm: "fast",
              mcp_env: {
                tracker: { TOKEN: REDACTED, URL: "${CHART_SEAT_OPS_MCP_ENV_URL_0a1b2c3d4e}" },
              },
              slack: { bot_token: REDACTED },
            },
          },
        ],
      }),
    );
    // Nothing edited: nothing sent.
    expect(plan(masked, masked)).toEqual([]);
    // A goal edit sends the address back as the mask, and no runtime half at all.
    const goal = plan(
      run(masked, {
        type: "updateSeat",
        target: "seat:ops",
        set: [{ path: ["goal"], value: "Run" }],
      }),
      masked,
    );
    expect(goal[0]!.request.body).toMatchObject({ email: REDACTED, goal: "Run" });
    expect(goal[0]!.request.body).not.toHaveProperty("runtime");
    // A model edit sends the runtime half with every masked value as it came.
    const llm = plan(
      run(masked, {
        type: "updateSeat",
        target: "seat:ops",
        set: [{ path: ["runtime", "llm"], value: "slow" }],
      }),
      masked,
    );
    expect(llm[0]!.request.body).toMatchObject({
      email: REDACTED,
      runtime: {
        llm: "slow",
        mcp_env: { tracker: { TOKEN: REDACTED, URL: "${CHART_SEAT_OPS_MCP_ENV_URL_0a1b2c3d4e}" } },
        slack: { bot_token: REDACTED },
      },
    });
    // Control: an address the person typed goes as the literal they typed.
    const typed = plan(
      run(masked, {
        type: "updateSeat",
        target: "seat:ops",
        set: [{ path: ["email"], value: "ops@example.com" }],
      }),
      masked,
    );
    expect(typed[0]!.request.body).toMatchObject({ email: "ops@example.com" });
  });
});

describe("naming every step", () => {
  test("each step has its own id, sent as the key a retry must resend", () => {
    const draft = run(
      base(),
      { type: "updateSeat", target: "seat:dev", set: [{ path: ["handle"], value: "zed" }] },
      { type: "updateSeat", target: "seat:sre", set: [{ path: ["goal"], value: "Up" }] },
      { type: "updateCompany", set: [{ path: ["vision"], value: "Everywhere" }] },
    );
    const steps = plan(draft);
    expect(steps.map((s) => s.id)).toEqual(["write-0001-1", "write-0001-2", "write-0001-3"]);
    for (const step of steps.filter((s) => s.kind !== "settings")) {
      expect(step.request.headers).toEqual({ "Idempotency-Key": step.id });
    }
    // The settings are signed in their summary instead.
    expect(steps[2]!.request.body).toMatchObject({ _summary: "Saved (write write-0001)" });
    // A content write addresses a renamed seat by the handle the batch gave it.
    expect(shape(steps)).toContain("seat PATCH /chart/seats/sre");
  });
});

describe("what an answer means", () => {
  const seatStep: SaveStep = {
    id: "write-0001-2",
    kind: "seat",
    request: {
      method: "PATCH",
      path: "/chart/seats/dev",
      query: {},
      contentType: "application/json",
      headers: {},
      body: {},
    },
    nodes: ["seat:dev"],
    creates: [],
  };
  const batchStep: SaveStep = {
    ...seatStep,
    id: "write-0001-1",
    kind: "structure",
    nodes: ["seat:a", "unit:b"],
  };

  test("applied, pending and unknown are three answers, and unknown carries the id to retry under", () => {
    expect(
      classifyStep(
        { status: 200, body: { outcome: "applied", position: "L@1:5", op_id: "x" } },
        seatStep,
      ),
    ).toEqual({ kind: "applied", position: "L@1:5" });
    expect(
      classifyStep({ status: 202, body: { outcome: "pending", position: "L@1:6" } }, seatStep),
    ).toEqual({
      kind: "pending",
      position: "L@1:6",
    });
    expect(
      classifyStep(
        { status: 503, body: { error: "unavailable", op_id: "write-0001-2" } },
        seatStep,
      ),
    ).toMatchObject({ kind: "unknown", opId: "write-0001-2" });
    // Never answered: still the step's own id, which is always safe to resend.
    expect(classifyStep({ status: 0, body: null }, seatStep)).toMatchObject({
      kind: "unknown",
      opId: "write-0001-2",
    });
    expect(landed({ kind: "pending", position: "" })).toBe(true);
    expect(landed({ kind: "unknown", opId: "x", detail: "" })).toBe(false);
  });

  test("a refusal names the node and what would admit it; a lost race is a conflict", () => {
    expect(
      classifyStep(
        {
          status: 403,
          body: {
            error: "unauthorized",
            reason: "lead",
            grants: ["config:write"],
            fields: ["project"],
          },
        },
        seatStep,
      ),
    ).toMatchObject({
      kind: "refused",
      status: 403,
      grants: ["config:write"],
      fields: ["project"],
      node: "seat:dev",
    });
    expect(
      classifyStep(
        { status: 422, body: { error: "refused", rule: "key_taken", index: 1 } },
        batchStep,
      ),
    ).toMatchObject({ kind: "refused", rule: "key_taken", node: "unit:b" });
    // A masked value with nothing stored behind it is refused naming the field.
    expect(
      classifyStep({ status: 422, body: { error: "refused", fields: ["email"] } }, seatStep),
    ).toMatchObject({ kind: "refused", fields: ["email"], node: "seat:dev" });
    expect(classifyStep({ status: 409, body: { error: "stale" } }, seatStep)).toMatchObject({
      kind: "conflict",
    });
  });

  test("a settings answer carries the revision and the epoch it activated", () => {
    const settings: SaveStep = { ...seatStep, kind: "settings", nodes: [COMPANY_KEY] };
    expect(
      classifyStep({ status: 200, body: { revision_id: "rev-2", epoch: 7 } }, settings),
    ).toEqual({
      kind: "applied",
      revisionId: "rev-2",
      epoch: 7,
      warnings: [],
    });
    expect(
      classifyStep(
        { status: 409, body: { error: "revision_advanced", current_revision_id: "rev-3" } },
        settings,
      ),
    ).toMatchObject({ kind: "conflict", currentRevisionId: "rev-3" });
  });

  test("the furthest position a save reached is compared as numbers, never as text", () => {
    expect(parsePosition("CREWLET_CHART_LOG@2:10")).toEqual({
      stream: "CREWLET_CHART_LOG",
      generation: 2,
      seq: 10,
    });
    expect(parsePosition("nonsense")).toBeNull();
    expect(
      furthestPosition([
        { kind: "applied", position: "L@1:9" },
        { kind: "pending", position: "L@1:10" },
        { kind: "unknown", opId: "x", detail: "" },
        undefined,
      ]),
    ).toBe("L@1:10");
    expect(
      furthestPosition([
        { kind: "applied", position: "L@2:1" },
        { kind: "applied", position: "L@1:99" },
      ]),
    ).toBe("L@2:1");
    expect(furthestPosition([])).toBeNull();
  });
});
