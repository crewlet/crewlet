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
import type { ArtifactAudience, RoomMember, OrgProjection } from "~/protocol/index.ts";
import type { BridgePerson } from "~/runtime/bridge.ts";
import { loginKind, tierATokenOf } from "../login.ts";
import { indexOrg, nameOfIn, principalOfSeat, type OrgIndex, type Unit } from "../seats.ts";
import { useOrg } from "../store-hooks.ts";
import { plural } from "../format.ts";
import { tabOfRegion } from "./doc.ts";
import { slideOfRegion } from "./follow.ts";

/** The chart, indexed, with the lookups an artifact's surfaces make. */
export interface ArtifactPeople {
  index: OrgIndex;
  /** The company's name, as the share dialog's company row says it. */
  company: string;
  /**
   * A seat's name by handle. A name the chart holds no seat for is a LOGIN
   * (an owner who acts as no seat, a Tier A token) and is said in its words
   * ([loginWords]), else the handle as written.
   */
  seatName: (handle: string) => string;
  /** A unit's name by key, or the key as written. */
  unitName: (key: string) => string;
  /** A seat as a principal: a person or an agent with their face; a login as a person, by its words. */
  seatPrincipal: (handle: string) => Principal;
  /** A unit as a principal: a team, with how many it holds. */
  unitPrincipal: (key: string) => Principal;
  /** Every seat as a person cell offers it. */
  sheetPeople: SheetPerson[];
  /** A seat as an artifact page is told it: display facts only. `kind` is the engine's where it said one. */
  pagePerson: (handle: string, kind?: "human" | "agent") => BridgePerson;
  /** Every seat as an artifact page is told it. */
  pagePeople: BridgePerson[];
}

/**
 * A unit as a principal: everybody in it and below it.
 *
 * A COUNT OF NONE IS LEFT OUT. "0 people, 2 agents" spends half its line on
 * what the team does not hold; "2 agents" says the same.
 */
export function principalOfUnit(unit: Unit | undefined, key: string): Principal {
  if (!unit) return { id: key, kind: "team", name: key };
  const people = unit.allSeats.filter((s) => s.kind === "human").length;
  const agents = unit.allSeats.length - people;
  const counts = [
    people > 0 ? plural(people, "person", "people") : "",
    agents > 0 ? plural(agents, "agent") : "",
  ].filter(Boolean);
  return {
    id: unit.id,
    kind: "team",
    name: unit.name,
    detail: counts.length > 0 ? counts.join(", ") : "No seats yet",
  };
}

/**
 * The turn an agent in the room is working in, for the People rail's Open
 * turn: the roster is keyed by session, which is the drawn member's `id`. A
 * person, or an agent between turns, has none.
 */
export function memberTurn(
  roster: ReadonlyMap<string, RoomMember>,
  member: Pick<PresenceMember, "id" | "kind">,
): string {
  return member.kind === "agent" ? (roster.get(member.id)?.turn ?? "") : "";
}

/** The chart's lookups for an artifact's surfaces, over an org projection. */
export function artifactPeople(org: OrgProjection | null | undefined): ArtifactPeople {
  const index = indexOrg(org);
  const units = new Map(index.units.map((u) => [u.id, u]));
  const nameOf = nameOfIn(index);
  const pagePerson = (handle: string, kind?: "human" | "agent"): BridgePerson => {
    const seat = index.byHandle.get(handle);
    const person: BridgePerson = {
      seat: handle,
      kind: kind ?? (seat?.kind === "agent" ? "agent" : "human"),
      displayName: handle ? nameOf(handle) : "Somebody",
    };
    if (seat?.kind === "agent" && seat.avatar) {
      person.avatar = { character: seat.avatar.character, hue: seat.avatar.color };
    }
    return person;
  };
  return {
    index,
    company: org?.name?.trim() || "the company",
    seatName: nameOf,
    unitName: (key) => units.get(key)?.name ?? key,
    // A LOGIN IS NOT A SEAT, and drawn as the chart's default it wore an
    // agent's squircle with the credential's first letters ("TO" for
    // `token:review`). It is a person's circle and its words, as the Audit
    // trail draws a login.
    seatPrincipal: (handle) => {
      const seat = index.byHandle.get(handle);
      if (!seat && loginKind(handle) !== null) {
        // A TOKEN'S ID IS ITS NAME and "API token" the line under it, so a
        // badge draws the id's initials: "review (API token)" drew "R(".
        const token = tierATokenOf(handle);
        return token
          ? { id: handle, kind: "person", name: token, detail: "API token" }
          : { id: handle, kind: "person", name: handle };
      }
      return principalOfSeat(seat, handle);
    },
    unitPrincipal: (key) => principalOfUnit(units.get(key), key),
    sheetPeople: index.seats.map((seat) => {
      const person: SheetPerson = { id: seat.handle, name: seat.name, kind: seat.kind };
      if (seat.kind === "agent" && seat.avatar) {
        person.character = seat.avatar.character;
        if (isNodeHue(seat.avatar.color)) person.hue = seat.avatar.color;
      }
      return person;
    }),
    pagePerson,
    pagePeople: index.seats.map((seat) => pagePerson(seat.handle)),
  };
}

/** The chart's lookups, from the org this tab holds. */
export function useArtifactPeople(): ArtifactPeople {
  const org = useOrg();
  return useMemo(() => artifactPeople(org), [org]);
}

/**
 * A share's audience as the engine spells it in a history row (`seat:pm`,
 * `unit:ops`, `company`), named the way a reader knows it, for
 * [historyWords]' `audience`.
 */
export function audienceName(raw: string, people: ArtifactPeople): string {
  if (raw === "company") return `everyone in ${people.company}`;
  // AT THE FIRST COLON ONLY: a seat handle may carry its own (`token:maya`).
  const at = raw.indexOf(":");
  const kind = raw.slice(0, at);
  const id = raw.slice(at + 1);
  return kind === "unit" ? people.unitName(id) : people.seatName(id);
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
 * How a place in an artifact is named to a reader: a row by its number as the
 * sheet draws it, a column by its label. A raw document id or field key is
 * never said; a place the caller cannot name is "a row" or "a column".
 */
export interface PresencePlace {
  /** "row 4". */
  row: (collection: string, doc: string) => string | null;
  /** "Gate". */
  field: (collection: string, field: string) => string | null;
  /** A view's own region by the name its view gives it: "Overview", "slide 3". */
  region: (region: string) => string | null;
}

const NO_PLACE: PresencePlace = { row: () => null, field: () => null, region: () => null };

/**
 * Where a member is, in words, after the verb: "row 4, Gate", "version 3".
 * What the presence rail and a badge's name say.
 */
export function memberLocation(member: RoomMember, place: PresencePlace = NO_PLACE): string {
  const f = member.focus;
  if (f.collection && f.doc) {
    const row = place.row(f.collection, f.doc) ?? "a row";
    if (!f.field) return row;
    return `${row}, ${place.field(f.collection, f.field) ?? "a column"}`;
  }
  if (f.region) {
    const named = place.region(f.region);
    if (named) return named;
    // A DOC'S TAB OR A DECK'S SLIDE IS AN ADDRESS ("tab:index.md"), never
    // words: unnamed, it is left unsaid and the version stands in for it. A
    // page's own region is the page author's words, and is said as written.
    if (!isViewRegion(f.region)) return `in ${f.region}`;
  }
  if (f.version) return `version ${f.version}`;
  return "";
}

/** Whether a region is one of the views' own addresses: a doc's tab or a deck's slide. */
export function isViewRegion(region: string): boolean {
  return slideOfRegion(region) !== "" || tabOfRegion(region) !== "";
}

/**
 * The reader's own other tabs, apart from everybody else: a second tab of the
 * reader's own seat is not another person, and following it moves nothing, so
 * it is counted beside "You" rather than drawn as a collaborator.
 */
export function splitOwnSessions<M extends { seat: string }>(
  members: readonly M[],
  handle: string,
): { others: M[]; ownTabs: number } {
  if (!handle) return { others: [...members], ownTabs: 0 };
  const others = members.filter((m) => m.seat !== handle);
  return { others, ownTabs: members.length - others.length };
}

/** The reader's own row on the People rail: "You", or "You, 2 tabs" with another tab open. */
export function selfWords(ownTabs: number): string {
  return ownTabs > 0 ? `You, ${ownTabs + 1} tabs` : "You";
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
  members: readonly RoomMember[],
  people: ArtifactPeople,
  held: ReadonlyMap<string, NodeHue>,
  place: PresencePlace = NO_PLACE,
): { members: PresenceMember[]; hues: Map<string, NodeHue> } {
  const drawn: PresenceMember[] = members.map((m) => {
    const seat = people.index.byHandle.get(m.seat);
    const kind = m.kind === "agent" ? "agent" : "human";
    const out: PresenceMember = {
      id: m.session,
      seat: m.seat || m.session,
      name: m.seat ? people.seatName(m.seat) : "Somebody",
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
    const where = memberLocation(m, place);
    if (where) out.location = `${PRESENCE_LABELS.activity(m.activity, kind)} ${where}`;
    return out;
  });
  const hues = assignPresenceHues(drawn, held);
  return { members: drawn.map((m) => ({ ...m, hue: hues.get(m.id) ?? m.hue })), hues };
}

/** The members on cells of one collection, as the sheet outlines them. */
export function cellPresence(
  members: readonly RoomMember[],
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
