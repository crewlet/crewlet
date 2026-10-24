/**
 * Fixtures for the builder core's suites. Imported by tests only.
 *
 * THE DERIVATIONS HERE ARE FIXTURE DATA, NOT A PORT OF THE ENGINE. A suite
 * that needs the engine's `derived` block states the managers and leads it
 * wants, and [fixtureDerived] only lays them out in the wire shape with each
 * seat's authored path. A seat's handle and a unit's key are the ones it
 * declares, as every document the engine stores declares them; a fixture
 * that declares none gets the builder's own minting rule's (`identity.ts`),
 * which is what keys such a node in the draft too.
 */

import type {
  CompanyDocument,
  ConfigRole,
  ConfigUnit,
  Derived,
  DerivedSeat,
  DerivedUnit,
} from "~/protocol/index.ts";
import { mintHandle, mintUnitKey } from "./identity.ts";
import type { KeySource } from "./keys.ts";

/** A key source that counts: `k1`, `k2`, and so on, with a prefix per source. */
export function countingKeys(prefix = "k"): KeySource {
  let n = 0;
  return { next: () => `${prefix}${++n}` };
}

/** The fixture's handle for a seat: its declared handle, or the one minted from its name. */
export function fixtureHandle(role: ConfigRole): string {
  if (typeof role.handle === "string" && role.handle !== "") return role.handle;
  return mintHandle(role.name, new Set());
}

/** The fixture's key for a unit: its declared key, or the one minted from its name. */
export function fixtureUnitKey(unit: ConfigUnit): string {
  if (typeof unit.id === "string" && unit.id !== "") return unit.id;
  return mintUnitKey(unit.name, new Set());
}

/** Per-seat and per-unit overrides of a fixture derivation, by authored path. */
export interface DerivedOverrides {
  readonly seats?: Readonly<Record<string, Partial<DerivedSeat>>>;
  readonly units?: Readonly<Record<string, Partial<DerivedUnit>>>;
}

/**
 * A `derived` block for a document: every seat at its authored path with the
 * fixture handle and its structural home unit, every unit at its path with
 * the lead it declares, and nobody managing anybody unless an override says
 * so. Root seats come first, then each unit's seats depth-first, which is the
 * engine's own seat order for a document with no `unit:` references.
 */
export function fixtureDerived(doc: CompanyDocument, overrides: DerivedOverrides = {}): Derived {
  const seats: DerivedSeat[] = [];
  const units: DerivedUnit[] = [];

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
        id: fixtureUnitKey(u),
        name: u.name,
        type: u.type ?? "unit",
        lead: u.lead ?? "",
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
 * not model, and the two integration entries keyed by handle. Every seat
 * declares its handle and every unit its key, and every reference names them,
 * as in every document the engine stores.
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
      { name: "CEO", handle: "ceo", goal: "Lead", manages: ["engineering", "designer"] },
      { name: "Designer", handle: "designer", unit: "platform", goal: "Design" },
    ],
    units: [
      {
        name: "Engineering",
        id: "engineering",
        type: "department",
        lead: "vp-engineering",
        mcp_env: { tracker: { TOKEN: "${TRACKER_TOKEN}" } },
        schedules: [{ name: "standup", cron: "0 9 * * 1-5", task: "Run standup", target: "lead" }],
        roles: [
          {
            name: "VP Engineering",
            handle: "vp-engineering",
            goal: "Run engineering",
            manages: ["dev"],
          },
          { name: "Dev", handle: "dev", goal: "Build", unknown_role_key: 7 },
        ],
        children: [
          {
            name: "Platform",
            id: "platform",
            type: "team",
            roles: [{ name: "SRE", handle: "sre", goal: "Keep it up", project: "OPS" }],
          },
        ],
      },
      {
        name: "Sales",
        id: "sales",
        roles: [
          {
            name: "Account Executive",
            handle: "account-executive",
            goal: "Sell",
            schedules: [{ name: "pipeline", cron: "0 8 * * 1", task: "Review" }],
          },
        ],
      },
    ],
  };
}
