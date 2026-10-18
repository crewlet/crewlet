/**
 * Who an artifact names, as the reader knows them: seats and units to names,
 * faces and principals, a share's audience to its row in the share dialog, and
 * a room's members to the presence the chrome draws.
 *
 * FROM THE CHART THIS TAB HOLDS, never a second lookup: the engine sends seat
 * handles only (attribution is the handle, and display names are the
 * dashboard's to resolve), and the org it pushes is what every other screen
 * draws a seat from, so one person reads as one name everywhere.
 */

import { useMemo } from "react";
import {
  PRESENCE_LABELS,
  assignPresenceHues,
  isNodeHue,
  type NodeHue,
  type PresenceMember,
  type Principal,
  type SheetCellPresence,
  type SheetPerson,
} from "@crewlethq/ui";
import type { ArtifactAudience, ArtifactPresenceMember, OrgProjection } from "~/protocol/index.ts";
import { indexOrg, principalOfSeat, type OrgIndex, type Unit } from "../seats.ts";
import { useOrg } from "../store-hooks.ts";
import { plural } from "../format.ts";

/** The chart, indexed, with the lookups an artifact's surfaces make. */
export interface ArtifactPeople {
  index: OrgIndex;
  /** The company's name, as the share dialog's company row says it. */
  company: string;
  /** A seat's name by handle, or the handle as written where the chart holds no such seat. */
  seatName: (handle: string) => string;
  /** A unit's name by key, or the key as written. */
  unitName: (key: string) => string;
  /** A seat as a principal: a person or an agent with their face. */
  seatPrincipal: (handle: string) => Principal;
  /** A unit as a principal: a team, with how many it holds. */
  unitPrincipal: (key: string) => Principal;
  /** Every seat as a person cell offers it. */
  sheetPeople: SheetPerson[];
}

/** A unit as a principal: everybody in it and below it. */
export function principalOfUnit(unit: Unit | undefined, key: string): Principal {
  if (!unit) return { id: key, kind: "team", name: key };
  const people = unit.allSeats.filter((s) => s.kind === "human").length;
  const agents = unit.allSeats.length - people;
  return {
    id: unit.id,
    kind: "team",
    name: unit.name,
    detail: `${plural(people, "person", "people")}, ${plural(agents, "agent")}`,
  };
}

/**
 * The turn an agent in the room is working in, for the People rail's Open
 * turn: the roster is keyed by session, which is the drawn member's `id`. A
 * person, or an agent between turns, has none.
 */
export function memberTurn(
  roster: ReadonlyMap<string, ArtifactPresenceMember>,
  member: Pick<PresenceMember, "id" | "kind">,
): string {
  return member.kind === "agent" ? (roster.get(member.id)?.turn ?? "") : "";
}

/** The chart's lookups for an artifact's surfaces, over an org projection. */
export function artifactPeople(org: OrgProjection | null | undefined): ArtifactPeople {
  const index = indexOrg(org);
  const units = new Map(index.units.map((u) => [u.id, u]));
  return {
    index,
    company: org?.name?.trim() || "the company",
    seatName: (handle) => index.byHandle.get(handle)?.name ?? handle,
    unitName: (key) => units.get(key)?.name ?? key,
    seatPrincipal: (handle) => principalOfSeat(index.byHandle.get(handle), handle),
    unitPrincipal: (key) => principalOfUnit(units.get(key), key),
    sheetPeople: index.seats.map((seat) => {
      const person: SheetPerson = { id: seat.handle, name: seat.name, kind: seat.kind };
      if (seat.kind === "agent" && seat.avatar) {
        person.character = seat.avatar.character;
        if (isNodeHue(seat.avatar.color)) person.hue = seat.avatar.color;
      }
      return person;
    }),
  };
}

/** The chart's lookups, from the org this tab holds. */
export function useArtifactPeople(): ArtifactPeople {
  const org = useOrg();
  return useMemo(() => artifactPeople(org), [org]);
}

/** A share's audience as a principal, for the share dialog's row. */
export function audiencePrincipal(audience: ArtifactAudience, people: ArtifactPeople): Principal {
  switch (audience.kind) {
    case "seat":
      return people.seatPrincipal(audience.id ?? "");
    case "unit":
      return people.unitPrincipal(audience.id ?? "");
    case "company":
      return { id: "company", kind: "team", name: `Everyone in ${people.company}` };
  }
}

/** A principal picked in the share dialog as the audience a share names. */
export function principalAudience(principal: Principal): ArtifactAudience {
  return principal.kind === "team"
    ? { kind: "unit", id: principal.id }
    : { kind: "seat", id: principal.id };
}

/**
 * Where a member is, in words: what the presence rail and a badge's name say.
 * An agent reading is "Reading", as a turn reads; a person viewing is
 * "Viewing".
 */
export function memberLocation(
  member: ArtifactPresenceMember,
  title: (doc: string) => string,
): string {
  const f = member.focus;
  if (f.collection && f.doc && f.field) return `on ${title(f.doc)}, ${f.field}`;
  if (f.collection && f.doc) return `on ${title(f.doc)}`;
  if (f.region) return `in ${f.region}`;
  if (f.version) return `on version ${f.version}`;
  return "";
}

/**
 * A room's members as the presence stack draws them: each with the name the
 * chart gives their seat, the kind the engine says they are, the activity,
 * and the hue their marks are drawn in (D22). An agent wears the hue its
 * operator chose; a person is given one of the six node hues for the session,
 * in join order, skipping hues in use (`assignPresenceHues`), and keeps it
 * while present (`held`).
 */
export function presenceMembers(
  members: readonly ArtifactPresenceMember[],
  people: ArtifactPeople,
  held: ReadonlyMap<string, NodeHue>,
  title: (doc: string) => string = (doc) => doc,
): { members: PresenceMember[]; hues: Map<string, NodeHue> } {
  const drawn: PresenceMember[] = members.map((m) => {
    const seat = people.index.byHandle.get(m.seat);
    const kind = m.kind === "agent" ? "agent" : "human";
    const out: PresenceMember = {
      id: m.session,
      seat: m.seat || m.session,
      name: seat?.name ?? (m.seat || "Somebody"),
      kind,
      activity: m.activity,
    };
    if (kind === "agent" && seat?.avatar) {
      out.character = seat.avatar.character;
      if (isNodeHue(seat.avatar.color)) out.hue = seat.avatar.color;
    }
    // THE WHOLE PHRASE, VERB FIRST, which is what uilet's `PresenceMember.location`
    // means: the People rail prints it in place of the activity, and the
    // presence stack reads it out in place of the activity too.
    const where = memberLocation(m, title);
    if (where) out.location = `${PRESENCE_LABELS.activity(m.activity, kind)} ${where}`;
    return out;
  });
  const hues = assignPresenceHues(drawn, held);
  return { members: drawn.map((m) => ({ ...m, hue: hues.get(m.id) ?? m.hue })), hues };
}

/** The members on cells of one collection, as the sheet outlines them. */
export function cellPresence(
  members: readonly ArtifactPresenceMember[],
  drawn: readonly PresenceMember[],
  collection: string,
): SheetCellPresence[] {
  const byId = new Map(drawn.map((m) => [m.id, m]));
  const out: SheetCellPresence[] = [];
  for (const m of members) {
    const f = m.focus;
    if (f.collection !== collection || !f.doc || !f.field) continue;
    const face = byId.get(m.session);
    if (!face) continue;
    out.push({
      rowId: f.doc,
      field: f.field,
      id: m.session,
      name: face.name,
      kind: face.kind,
      hue: face.hue ?? "purple",
      editing: m.activity === "editing",
      idle: m.activity === "idle",
    });
  }
  return out;
}
