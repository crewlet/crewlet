/**
 * Who may change the atlas, said in a sentence a reader can act on.
 *
 * EVERY ATLAS CHANGE IS MADE AS A SEAT. A claim names the person who made it
 * by the seat they hold, so the engine offers none of these changes to a
 * sign-in that holds no seat: an API token, or an account the directory binds
 * to none. It says so by leaving them out of what the reader may do, and the
 * shared write access (`lib/useWriteAccess.ts`) can only read that as "this
 * can't be changed from here", which names neither the cause nor the way
 * past it. An entry's More menu then said it six times over.
 *
 * So the atlas, which knows its changes are a seat's, says the cause where
 * that is the cause: one sentence on each held control, and beside the
 * controls one note with the place a person joins the company as a seat.
 * Every other hold (offline, no Atlas access, a retired entry) keeps its own
 * sentence, since those are checked first and are not this.
 */

import { useMemo } from "react";
import { Link, Text } from "@crewlethq/ui";
import { href } from "~/app/router.tsx";
import type { ActionTool } from "~/protocol/act.ts";
import type { Act } from "~/lib/useAct.ts";
import type { WriteAccess } from "~/lib/useWriteAccess.ts";
import { useViewer } from "~/lib/viewer.ts";

/** Why a sign-in with no seat changes nothing in the atlas. */
export const SEATLESS =
  "The atlas records a change under the seat of whoever made it, and this sign-in holds no seat.";

/** Where a person joins the company as a seat. */
export const SEAT_PLACE = ["company", "org"] as const;

/**
 * `access` as the atlas says it: where the engine offers the change to nobody
 * signed in like this reader and the reader holds no seat, that is the reason.
 */
export function seatedAccess(access: WriteAccess, viewer: { handle: string }): WriteAccess {
  return !access.can && access.block === "not_served" && viewer.handle === ""
    ? { can: false, block: "held", reason: SEATLESS }
    : access;
}

/**
 * An atlas change's access in the atlas's own words: see [seatedAccess].
 *
 * HANDED THE ACCESS, NOT THE TOOL'S NAME: the call that names the tool
 * (`useWriteAccess("atlas_correct")`) stays at the control that presses it, as
 * a string literal, which is where the source gates read which screen makes
 * which change.
 */
export function useSeatedAccess(access: WriteAccess): WriteAccess {
  const { handle } = useViewer();
  return useMemo(() => seatedAccess(access, { handle }), [access, handle]);
}

/** One change to the atlas, held in the atlas's own words: see [useSeatedAccess]. */
export function useSeated<T extends ActionTool>(act: Act<T>): Act<T> {
  const access = useSeatedAccess(act.access);
  return useMemo(() => ({ ...act, access }), [act, access]);
}

/**
 * The one note beside an entry's controls for a reader they are all held for
 * by [SEATLESS]: the cause once, and where to go, rather than the same
 * sentence under every control with nowhere to take it.
 */
export function SeatlessNote({ access }: { access: WriteAccess }) {
  if (access.can || access.reason !== SEATLESS) return null;
  return (
    <Text as="p" variant="caption" tone="secondary" className="atlas-actions__note">
      {SEATLESS} To change the atlas, sign in as a person who holds one. People join and take a seat
      in{" "}
      <Link href={href(SEAT_PLACE)} size="inherit">
        Company › Structure
      </Link>
      .
    </Text>
  );
}
