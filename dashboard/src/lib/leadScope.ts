/**
 * The part of the organization a person may change without `config:write`:
 * the units they lead, and everything inside them.
 *
 * THE RULE IS THE ENGINE'S (`authz`' subtree class, `configapi`'s admission):
 * the EFFECTIVE lead of a unit — the seat it declares, or the one it inherits
 * from the unit above when it declares none — may change that unit and every
 * seat and unit inside it, and nothing else. The company's root seats and its
 * top-level placement are nobody's, and so are the unit's own lead and its
 * place, which would hand the subtree to somebody else. The engine decides
 * every write; this reads the same organization to say, before a write, where
 * a person may make one, so a control outside it is drawn disabled with the
 * reason rather than refused after a round trip.
 *
 * FROM THE ORG PUSH, never a field of its own: `derived.units[].lead` is the
 * effective lead the engine resolved, `derived.units[].seats` a unit's direct
 * members (root seats placed in it by their `unit:` reference included), and
 * the authored tree each unit's children. A viewer bound to no seat leads
 * nothing, and so does every viewer while the push carries no derivation.
 */

import { useMemo } from "react";
import type { OrgProjection, OrgUnit } from "~/protocol/index.ts";
import { useOrg } from "./store-hooks.ts";
import { useViewer } from "./viewer.ts";

/** A unit a person leads whose parent they do not: what a lead's draft is opened on. */
export interface LedUnit {
  readonly key: string;
  readonly name: string;
}

export interface LeadScope {
  /**
   * Every unit the person may change — those they lead and every unit inside
   * them — each to the key of the TOP unit it falls under: the one of [tops]
   * a draft of it is opened on.
   */
  readonly units: ReadonlyMap<string, string>;
  /** The units they lead whose parent they do not, in the company's order. */
  readonly tops: readonly LedUnit[];
  /** The handles of the seats directly in any of those units, each to its top unit's key. */
  readonly seats: ReadonlyMap<string, string>;
}

export const NO_SCOPE: LeadScope = { units: new Map(), tops: [], seats: new Map() };

/** What `handle` leads in `org`. */
export function leadScope(org: OrgProjection | null | undefined, handle: string): LeadScope {
  const derived = org?.derived?.units ?? [];
  if (handle === "" || derived.length === 0) return NO_SCOPE;
  const leads = new Map(derived.map((u) => [u.id, u.lead]));
  const members = new Map(derived.map((u) => [u.id, u.seats ?? []]));
  const units = new Map<string, string>();
  const tops: LedUnit[] = [];
  const seats = new Map<string, string>();
  // `top` is the key of the led unit this walk is inside, or null outside one.
  const walk = (list: readonly OrgUnit[] | undefined, top: string | null) => {
    for (const unit of list ?? []) {
      const under = top ?? (leads.get(unit.id) === handle ? unit.id : null);
      if (under !== null) {
        units.set(unit.id, under);
        if (top === null) tops.push({ key: unit.id, name: unit.name });
        for (const seat of members.get(unit.id) ?? []) seats.set(seat, under);
      }
      walk(unit.children, under);
    }
  };
  walk(org?.units, null);
  return units.size === 0 ? NO_SCOPE : { units, tops, seats };
}

/** What the person reading leads, recomputed only when the org push or the reader moves. */
export function useLeadScope(): LeadScope {
  const org = useOrg();
  const { handle } = useViewer();
  return useMemo(() => leadScope(org, handle), [org, handle]);
}
