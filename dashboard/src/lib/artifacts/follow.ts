/**
 * Who is in an artifact's room, one entry a seat, and what following one of
 * them moves (plan 05, 5.1 and 5.5).
 *
 * PURE: [followTarget] picks a seat's entry by [bySeat], and the artifact page applies
 * [followMoves] to its own address, so what following does is held by tests
 * rather than by a screen.
 *
 * ONE ENTRY A SEAT. Two tabs of one person are two sessions in the engine's
 * roster and one person on the rail: the most active tab wins (editing over
 * viewing over idle), then the most recent.
 *
 * WHAT FOLLOWS is what a member's presence says: the version they have open,
 * for a native sheet the collection they are on, for a deck the slide they
 * are on (the region `slide:<id>`), which is how a phone follows a presenter,
 * and for a doc the tab they are on (the region `tab:<path>`). A page's own regions are the page's to scroll to, so any other
 * region is said in words beside the follow rather than moved to.
 */

import type { ArtifactKind, RoomMember, PresenceActivity } from "~/protocol/index.ts";
import { tabOfRegion } from "./doc.ts";

const RANK: Readonly<Record<PresenceActivity, number>> = { editing: 0, viewing: 1, idle: 2 };

/** The room's members, one a seat, the most active tab of each, in the order they arrived. */
export function bySeat(members: readonly RoomMember[]): RoomMember[] {
  const best = new Map<string, RoomMember>();
  for (const m of members) {
    const seat = m.seat || m.session;
    const held = best.get(seat);
    if (!held || better(m, held)) best.set(seat, m);
  }
  return [...best.values()];
}

function better(a: RoomMember, b: RoomMember): boolean {
  const rank = RANK[a.activity] - RANK[b.activity];
  if (rank !== 0) return rank < 0;
  return a.at > b.at;
}

/** The member a reader following seat follows: that seat's most active tab, or none once they left. */
export function followTarget(members: readonly RoomMember[], seat: string): RoomMember | null {
  return bySeat(members.filter((m) => m.seat === seat))[0] ?? null;
}

/** What the page shows now, as far as following can move it. */
export interface FollowView {
  kind: ArtifactKind;
  /** The newest version. */
  head: number;
  /** The version on show; the head when none is addressed. */
  showing: number;
  tab: string;
  /** For a sheet, the collection on show. */
  sheet: string;
  /** For a deck, the slide on show ("" for its first). */
  slide: string;
  /** For a doc, the tab on show ("" for its first). */
  doctab: string;
}

/** What to move so the reader's view is the followed member's: each field only where it differs. */
export interface FollowMove {
  /** The `version=` to address: "" for the head. */
  version?: string;
  tab?: "view";
  /** The `sheet=` to address. */
  sheet?: string;
  /** The `slide=` to address. */
  slide?: string;
  /** The `doctab=` to address. */
  doctab?: string;
}

/** The slide a presence region names, or "" for a region that names none. */
export function slideOfRegion(region: string | undefined): string {
  return region?.startsWith("slide:") ? region.slice("slide:".length) : "";
}

/** The moves that bring view to where target is. */
export function followMoves(target: RoomMember, view: FollowView): FollowMove {
  const out: FollowMove = {};
  const f = target.focus;
  if (f.version && f.version > 0 && f.version <= view.head && f.version !== view.showing) {
    out.version = f.version === view.head ? "" : String(f.version);
  }
  const slide = slideOfRegion(f.region);
  const doctab = tabOfRegion(f.region);
  if (view.kind === "sheet" && f.collection) {
    if (view.tab !== "view") out.tab = "view";
    if (f.collection !== view.sheet) out.sheet = f.collection;
  } else if (slide) {
    if (view.tab !== "view") out.tab = "view";
    if (slide !== view.slide) out.slide = slide;
  } else if (doctab) {
    if (view.tab !== "view") out.tab = "view";
    if (doctab !== view.doctab) out.doctab = doctab;
  } else if ((out.version !== undefined || f.version) && view.tab !== "view") {
    out.tab = "view";
  }
  return out;
}

/** Whether a move moves anything. */
export function moves(m: FollowMove): boolean {
  return (
    m.version !== undefined ||
    m.tab !== undefined ||
    m.sheet !== undefined ||
    m.slide !== undefined ||
    m.doctab !== undefined
  );
}
