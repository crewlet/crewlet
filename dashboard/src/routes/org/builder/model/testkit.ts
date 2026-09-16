/**
 * Fixtures for the builder core's suites. Imported by tests only.
 *
 * THE DERIVATIONS HERE ARE FIXTURE DATA, NOT A PORT OF THE ENGINE. A suite
 * that needs the engine's `derived` block states the handles, managers and
 * leads it wants, and [fixtureDerived] only lays them out in the wire shape
 * with each seat's authored path. Its default handle is a plain ASCII
 * lower-case-and-hyphen form of the name, which is correct for the ASCII
 * names these fixtures use and is never used outside a test: the dashboard
 * does not derive handles (see `keys.ts`).
 */

import type {
  CompanyDocument,
  ConfigRole,
  ConfigUnit,
  Derived,
  DerivedSeat,
  DerivedUnit,
} from "~/protocol/index.ts";
import type { KeySource } from "./keys.ts";

/** A key source that counts: `k1`, `k2`, and so on, with a prefix per source. */
export function countingKeys(prefix = "k"): KeySource {
  let n = 0;
  return { next: () => `${prefix}${++n}` };
}

/** The fixture's handle for a seat: its declared handle, or its ASCII name in lower case with hyphens. */
export function fixtureHandle(role: ConfigRole): string {
  if (typeof role.handle === "string" && role.handle !== "") return role.handle;
  return role.name
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");
}

/** Per-seat and per-unit overrides of a fixture derivation, by authored path. */
export interface DerivedOverrides {
  readonly seats?: Readonly<Record<string, Partial<DerivedSeat>>>;
  readonly units?: Readonly<Record<string, Partial<DerivedUnit>>>;
}

/**
 * A `derived` block for a document: every seat at its authored path with the
 * fixture handle and its structural home unit, every unit at its path with
 * its declared lead's handle, and nobody managing anybody unless an override
 * says so. Root seats come first, then each unit's seats depth-first, which
 * is the engine's own seat order for a document with no `unit:` references.
 */
export function fixtureDerived(doc: CompanyDocument, overrides: DerivedOverrides = {}): Derived {
  const seats: DerivedSeat[] = [];
  const units: DerivedUnit[] = [];
  const handleByName = new Map<string, string>();
  const collect = (roles: readonly ConfigRole[] | undefined) => {
    for (const r of roles ?? [])
      if (!handleByName.has(r.name)) handleByName.set(r.name, fixtureHandle(r));
  };
  collect(doc.roles);
  const collectUnits = (list: readonly ConfigUnit[] | undefined) => {
    for (const u of list ?? []) {
      collect(u.roles);
      collectUnits(u.children);
    }
  };
  collectUnits(doc.units);

  const seat = (role: ConfigRole, path: string, unitPath: string, chain: string[]) => {
    seats.push({
      path,
      handle: fixtureHandle(role),
      name: role.name,
      kind: role.kind === "human" ? "human" : "agent",
      unit_path: unitPath,
      placed_by_ref: false,
      manager: "",
      managers: null,
      reports: null,
      auto_reports: null,
      onboarding_chain: chain.length > 0 ? [...chain] : null,
      ...overrides.seats?.[path],
    });
  };
  (doc.roles ?? []).forEach((r, i) => seat(r, `roles[${i}]`, "", []));
  const walk = (list: readonly ConfigUnit[] | undefined, prefix: string, chain: string[]) => {
    (list ?? []).forEach((u, i) => {
      const path = `${prefix}[${i}]`;
      const here = [...chain, u.name];
      units.push({
        path,
        name: u.name,
        type: u.type ?? "unit",
        lead: u.lead ? (handleByName.get(u.lead) ?? "") : "",
        lead_inherited: false,
        channel: u.channel ?? "",
        channel_inherited: false,
        seats: (u.roles ?? []).map(fixtureHandle),
        ...overrides.units?.[path],
      });
      (u.roles ?? []).forEach((r, j) => seat(r, `${path}.roles[${j}]`, path, here));
      walk(u.children, `${path}.children`, here);
    });
  };
  walk(doc.units, "units", []);
  return { seats, units };
}

/**
 * A small company exercising what the operations touch: root seats (one
 * placed in a unit by reference), nested units, a lead, `manages` naming a
 * seat and a unit, schedules, a unit's tool credentials, a key this build does
 * not model, and the two integration entries keyed by handle.
 */
export function fixtureCompany(): CompanyDocument {
  return {
    name: "Acme",
    mission: "Make things.",
    policies: ["Write it down"],
    future_setting: { kept: true },
    integrations: {
      datadog: { enabled: true, route_to: "sre" },
      gitlab: { provisioning: { access_levels: { dev: "developer", sre: "maintainer" } } },
    },
    roles: [
      { name: "CEO", goal: "Lead", manages: ["Engineering", "Designer"] },
      { name: "Designer", unit: "Platform", goal: "Design" },
    ],
    units: [
      {
        name: "Engineering",
        type: "department",
        lead: "VP Engineering",
        mcp_env: { tracker: { TOKEN: "${TRACKER_TOKEN}" } },
        schedules: [{ name: "standup", cron: "0 9 * * 1-5", task: "Run standup", target: "lead" }],
        roles: [
          { name: "VP Engineering", goal: "Run engineering", manages: ["Dev"] },
          { name: "Dev", goal: "Build", unknown_role_key: 7 },
        ],
        children: [
          {
            name: "Platform",
            type: "team",
            roles: [
              { name: "SRE", goal: "Keep it up", integrations: { jira: { project: "OPS" } } },
            ],
          },
        ],
      },
      {
        name: "Sales",
        roles: [
          {
            name: "Account Executive",
            goal: "Sell",
            schedules: [{ name: "pipeline", cron: "0 8 * * 1", task: "Review" }],
          },
        ],
      },
    ],
  };
}
