/**
 * The company as the builder draws it for a reader the configuration is
 * guarded from: the org projection every signed-in reader holds, as a
 * document and the derivation the engine made of it.
 *
 * WHY THE BUILDER DRAWS IT AT ALL. Structure is the one page the org is read
 * on, and its two tabs are the builder. `GET /config` answers an admin and a
 * unit's lead, so everybody else would meet a refusal where the org chart
 * used to be. The projection carries what the chart needs (the seats, the
 * units, who leads and who manages whom), so such a reader reads the same
 * chart and table, held read-only, with nothing to check or save.
 *
 * MEMBERSHIP IS THE ENGINE'S. The projection carries the seats in the
 * positions the document wrote them, but not a root seat's `unit:` reference
 * or any authored path, so a seat the engine moved into a unit would be drawn
 * at the company's root. The document built here places every seat where the
 * engine says it sits ([DerivedUnit.seats]), and the derivation is given the
 * paths of that document, so the builder places it as it places a check's.
 */

import type {
  CompanyDocument,
  Derived,
  OrgProjection,
  OrgSeat,
  OrgUnit,
} from "~/protocol/index.ts";
import { fromDocument, toDocument } from "./document.ts";
import { seatKey, unitKey } from "./keys.ts";

/**
 * The projection-only keys a seat carries that are no field of the document:
 * its resolved model chains and its tool sources, both read in other shapes
 * there, and so never handed to the builder as if they were written.
 */
const RESOLVED_ONLY: ReadonlySet<string> = new Set(["llm", "tool_sources"]);

/** A seat as a document's role: the projection's own keys, less the resolved ones. */
function roleOf(seat: OrgSeat): Record<string, unknown> {
  return Object.fromEntries(Object.entries(seat).filter(([key]) => !RESOLVED_ONLY.has(key)));
}

/** The projection's read: the document to draw and the derivation keyed to its paths. */
export interface ProjectionRead {
  readonly document: CompanyDocument;
  readonly derived: Derived | null;
}

export function projectionRead(org: OrgProjection): ProjectionRead {
  const seats = new Map<string, OrgSeat>();
  const collect = (list: readonly OrgSeat[] | undefined) => {
    for (const seat of list ?? []) if (seat.handle) seats.set(seat.handle, seat);
  };
  collect(org.roles);
  const walk = (units: readonly OrgUnit[] | undefined) => {
    for (const unit of units ?? []) {
      collect(unit.roles);
      walk(unit.children);
    }
  };
  walk(org.units);

  const members = new Map<string, readonly string[]>();
  for (const unit of org.derived?.units ?? []) members.set(unit.id, unit.seats ?? []);
  const placed = new Set<string>();
  const unitOf = (unit: OrgUnit): Record<string, unknown> => {
    const { roles, children, ...rest } = unit;
    // A UNIT THE DERIVATION DOES NOT LIST keeps the seats the document wrote in it.
    const handles = members.get(unit.id) ?? (roles ?? []).map((r) => r.handle ?? "");
    const own = handles.flatMap((h) => {
      const seat = seats.get(h);
      if (!seat) return [];
      placed.add(h);
      return [roleOf(seat)];
    });
    return {
      ...rest,
      ...(own.length ? { roles: own } : {}),
      ...(children?.length ? { children: children.map(unitOf) } : {}),
    };
  };
  const units = (org.units ?? []).map(unitOf);
  const root = (org.roles ?? []).filter((seat) => !seat.handle || !placed.has(seat.handle));
  const document = {
    ...(org.name ? { name: org.name } : {}),
    ...(root.length ? { roles: root.map(roleOf) } : {}),
    ...(units.length ? { units } : {}),
  } as CompanyDocument;

  if (!org.derived) return { document, derived: null };
  const { index } = toDocument(fromDocument(document));
  const pathOf = (key: ReturnType<typeof seatKey>) => index.pathOf.get(key);
  const unitPathOfSeat = new Map<string, string>();
  for (const unit of org.derived.units ?? []) {
    const path = pathOf(unitKey(unit.id));
    for (const handle of unit.seats ?? []) if (path !== undefined) unitPathOfSeat.set(handle, path);
  }
  return {
    document,
    derived: {
      seats: (org.derived.seats ?? []).map((seat) => {
        const path = pathOf(seatKey(seat.handle));
        return path === undefined
          ? seat
          : { ...seat, path, unit_path: unitPathOfSeat.get(seat.handle) ?? "" };
      }),
      units: (org.derived.units ?? []).map((unit) => {
        const path = pathOf(unitKey(unit.id));
        return path === undefined ? unit : { ...unit, path };
      }),
    },
  };
}
