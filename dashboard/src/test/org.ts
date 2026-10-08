/**
 * An org projection in the shape the engine sends it: the authored tree AND
 * the `derived` block beside it.
 *
 * The engine puts that block on every projection that carries a seat, and
 * `lib/seats.ts` reads every seat, handle and unit membership off it — so a
 * fixture holding only the tree is a projection no engine sends, and indexes
 * as a company with nobody in it.
 *
 * FOR A SUITE THAT PINS NO HIERARCHY. Every seat runs under the handle its
 * entry DECLARES — a fixture must declare one, because this derives nothing —
 * sits in the unit the tree writes it under, and manages nobody; a unit's lead
 * is the seat its `lead:` names. A suite whose subject IS a reporting line, an
 * inherited lead or a `unit:` placement writes its `derived` block out in
 * full, as `test/orgchart.ts` does.
 */

import type { Derived, DerivedSeat, DerivedUnit, OrgSeat, OrgUnit } from "~/protocol/types.ts";

interface Tree {
  roles?: OrgSeat[] | null;
  units?: OrgUnit[] | null;
  derived?: Derived;
}

/** The fixture with its derived block; one that writes its own keeps it. */
export function withDerived<T>(fixture: T): T & { derived: Derived } {
  const org = fixture as Tree;
  if (org.derived) return fixture as T & { derived: Derived };
  const seats: DerivedSeat[] = [];
  const units: DerivedUnit[] = [];
  const byName = new Map<string, string>();
  const add = (raw: OrgSeat): string => {
    if (!raw.handle) {
      throw new Error(`withDerived: the fixture seat "${raw.name}" declares no handle`);
    }
    byName.set(raw.name, raw.handle);
    seats.push({
      handle: raw.handle,
      name: raw.name,
      kind: raw.kind === "human" ? "human" : "agent",
      placed_by_ref: false,
      manager: "",
      managers: null,
      reports: null,
      auto_reports: null,
      onboarding_chain: null,
    });
    return raw.handle;
  };
  for (const raw of org.roles ?? []) add(raw);
  const leads: [DerivedUnit, string][] = [];
  const visit = (raw: OrgUnit): void => {
    const unit: DerivedUnit = {
      name: raw.name,
      type: raw.type ?? "",
      lead: "",
      lead_inherited: false,
      channel: raw.channel ?? "",
      channel_inherited: false,
      seats: (raw.roles ?? []).map(add),
    };
    units.push(unit);
    if (raw.lead) leads.push([unit, raw.lead]);
    for (const child of raw.children ?? []) visit(child);
  };
  for (const raw of org.units ?? []) visit(raw);
  for (const [unit, lead] of leads) unit.lead = byName.get(lead) ?? "";
  return { ...org, derived: { seats, units } } as T & { derived: Derived };
}
