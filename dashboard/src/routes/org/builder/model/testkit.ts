/**
 * Fixtures for the builder core's suites. Imported by tests only.
 *
 * THE DERIVATIONS HERE ARE FIXTURE DATA, NOT A PORT OF THE ENGINE. A suite
 * that needs the engine's `derived` block states the managers and leads it
 * wants, and [fixtureDerived] only lays them out in the wire shape, each seat
 * by its handle and each unit by its key.
 */

import type {
  ChartRead,
  ChartSeat,
  ChartUnit,
  CompanyDocument,
  Derived,
  DerivedSeat,
  DerivedUnit,
} from "~/protocol/index.ts";
import { COMPANY_KEY, handleOfKey, unitKeyOf, type KeySource } from "./keys.ts";
import { allSeats, allUnits, type Draft } from "./draft.ts";

/** A key source that counts: `k1`, `k2`, and so on, with a prefix per source. */
export function countingKeys(prefix = "k"): KeySource {
  let n = 0;
  return { next: () => `${prefix}${++n}` };
}

/** A chart answer holding these rows, as `GET /chart?runtime=true` serves one. */
export function chartOf(
  rows: {
    readonly units?: readonly ChartUnit[];
    readonly seats?: readonly ChartSeat[];
    readonly manages?: Readonly<Record<string, readonly string[]>> | null;
  },
  runtime = true,
): ChartRead {
  const units = [...(rows.units ?? [])];
  const leads: Record<string, string> = {};
  for (const unit of units) if (unit.lead) leads[unit.key] = unit.lead;
  const manages: Record<string, string[]> = {};
  for (const [handle, list] of Object.entries(rows.manages ?? {})) manages[handle] = [...list];
  return {
    units,
    seats: [...(rows.seats ?? [])],
    manages,
    leads,
    answer: { level: "linearizable", position: "CREWLET_CHART_LOG@1:10" },
    runtime,
  };
}

/**
 * The chart as `GET /chart?runtime=true` serves it to a reader WITHOUT the
 * grant that reads the runtime half: every row's half stripped, and the
 * answer saying it was withheld. Both, because the engine does both, and a
 * suite that only flipped the flag would be handing the builder a runtime
 * half no reader of that kind is ever shown.
 */
export function strippedChart(chart: ChartRead): ChartRead {
  return {
    ...chart,
    units: chart.units.map(({ runtime: _runtime, ...unit }) => unit),
    seats: chart.seats.map(({ runtime: _runtime, ...seat }) => seat),
    runtime: false,
  };
}

/**
 * The chart a draft describes, as `GET /chart` would serve it once saved:
 * every unit with its parent, every seat with its unit, the `manages:` lists
 * beside them, and — for a node the chart already held under another address —
 * the identity its key carries. What a suite needs to hand a draft to anything
 * that reads a chart: a derivation, the component kit's engine.
 */
export function chartOfDraft(draft: Draft, runtime = true): ChartRead {
  const keyOf = new Map([...allUnits(draft)].map(({ unit }) => [unit.key, unit.data.key]));
  const units: ChartUnit[] = [];
  for (const { unit, parent } of allUnits(draft)) {
    const { key, ...rest } = unit.data;
    const origin = unitKeyOf(unit.key);
    units.push({
      key,
      ...rest,
      ...(parent !== COMPANY_KEY ? { parent: keyOf.get(parent) } : {}),
      ...(origin !== undefined && origin !== key ? { origin_key: origin } : {}),
    } as ChartUnit);
  }
  const seats: ChartSeat[] = [];
  const manages: Record<string, string[]> = {};
  for (const { seat, parent } of allSeats(draft)) {
    const { handle, manages: list, ...rest } = seat.data;
    const origin = handleOfKey(seat.key);
    seats.push({
      handle,
      ...rest,
      ...(parent !== COMPANY_KEY ? { unit: keyOf.get(parent) } : {}),
      ...(origin !== undefined && origin !== handle ? { origin_handle: origin } : {}),
    } as ChartSeat);
    if (list && list.length > 0) manages[handle] = [...list];
  }
  return chartOf({ units, seats, manages }, runtime);
}

/**
 * A small company exercising what the operations touch: root seats, nested
 * units, a lead, `manages` naming a seat and a unit, schedules, a unit's tool
 * credentials, a runtime key this build does not model, and the two
 * integration entries keyed by handle.
 */
export function fixtureChart(): ChartRead {
  return chartOf({
    units: [
      {
        key: "engineering",
        name: "Engineering",
        type: "department",
        lead: "vp-engineering",
        runtime: {
          mcp_env: { tracker: { TOKEN: "${TRACKER_TOKEN}" } },
          schedules: [
            { name: "standup", cron: "0 9 * * 1-5", task: "Run standup", target: "lead" },
          ],
        },
      },
      { key: "platform", name: "Platform", type: "team", parent: "engineering" },
      { key: "sales", name: "Sales" },
    ],
    seats: [
      { handle: "ceo", name: "CEO", goal: "Lead" },
      { handle: "designer", name: "Designer", unit: "platform", goal: "Design" },
      {
        handle: "vp-engineering",
        name: "VP Engineering",
        unit: "engineering",
        goal: "Run engineering",
      },
      {
        handle: "dev",
        name: "Dev",
        unit: "engineering",
        goal: "Build",
        runtime: { llm: "fast", future_runtime_key: 7 },
      },
      { handle: "sre", name: "SRE", unit: "platform", goal: "Keep it up", project: "OPS" },
      {
        handle: "account-executive",
        name: "Account Executive",
        unit: "sales",
        goal: "Sell",
        runtime: { schedules: [{ name: "pipeline", cron: "0 8 * * 1", task: "Review" }] },
      },
    ],
    manages: { ceo: ["engineering", "designer"], "vp-engineering": ["dev"] },
  });
}

/** The settings revision beside [fixtureChart]: the charter and the two integration entries. */
export function fixtureSettings(): CompanyDocument {
  return {
    name: "Acme",
    mission: "Make things.",
    policies: ["Write it down"],
    future_setting: { kept: true },
    integrations: {
      datadog: { enabled: true, route_to: "sre" },
      gitlab: { provisioning: { access_levels: { dev: "developer", sre: "maintainer" } } },
    },
  };
}

/** Per-seat and per-unit overrides of a fixture derivation, by handle and by key. */
export interface DerivedOverrides {
  readonly seats?: Readonly<Record<string, Partial<DerivedSeat>>>;
  readonly units?: Readonly<Record<string, Partial<DerivedUnit>>>;
}

/**
 * A `derived` block for a chart: every seat by its handle in its unit, every
 * unit by its key with its authored lead, and nobody managing anybody unless
 * an override says so.
 */
export function fixtureDerived(chart: ChartRead, overrides: DerivedOverrides = {}): Derived {
  const byKey = new Map(chart.units.map((u) => [u.key, u]));
  const chainOf = (key: string | undefined): string[] => {
    const out: string[] = [];
    for (
      let at = key ? byKey.get(key) : undefined;
      at;
      at = at.parent ? byKey.get(at.parent) : undefined
    ) {
      out.unshift(at.name ?? at.key);
    }
    return out;
  };
  const seats: DerivedSeat[] = chart.seats.map((seat) => ({
    path: "",
    handle: seat.handle,
    name: seat.name ?? "",
    kind: seat.kind === "human" ? "human" : "agent",
    unit_path: seat.unit ?? "",
    placed_by_ref: false,
    manager: "",
    managers: null,
    reports: null,
    auto_reports: null,
    onboarding_chain: seat.unit ? chainOf(seat.unit) : null,
    ...overrides.seats?.[seat.handle],
  }));
  const units: DerivedUnit[] = chart.units.map((unit) => ({
    path: "",
    id: unit.key,
    name: unit.name ?? "",
    type: unit.type ?? "unit",
    lead: unit.lead ?? "",
    lead_inherited: false,
    channel: unit.channel ?? "",
    channel_inherited: false,
    seats: chart.seats.filter((s) => s.unit === unit.key).map((s) => s.handle),
    ...overrides.units?.[unit.key],
  }));
  return { seats, units };
}
