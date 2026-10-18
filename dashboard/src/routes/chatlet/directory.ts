/**
 * The rooms that are NOT in the rail.
 *
 * # What this has to work with
 *
 * The rail is the viewer's own membership. What a person needs before they
 * have a rail is the other list — the public and unit rooms of the company
 * they have not joined — and the candidates come from the three reads that
 * reach past the rail, each of them the engine's own visibility rather than a
 * guess made here:
 *
 *   - **SEARCH.** `chat_search` ranks over every room the viewer may READ,
 *     which is wider than the rail by exactly the set this list is about, and
 *     the answer carries the room each hit was said in. It is what somebody
 *     typed, so it comes first — but it only ever finds a room somebody has
 *     SAID something in.
 *   - **THE LIVE FRAMES.** Every committed record reaches this socket for
 *     every room the viewer may read, filtered per socket by the engine's own
 *     `Visible` rule, and the frame carries the room's name and kind. So a
 *     room that is merely busy is discoverable without anybody searching for
 *     it — but only while this tab has been connected.
 *   - **THE NAMES.** `chat_room_names`, which the frame reads once
 *     (`lib/roomNames.ts`), names every room the viewer may read, so a room
 *     nobody has spoken in is offered too. Only while nothing is searched
 *     for: a search narrows the list to what it found, and every room the
 *     viewer may read after its hits would undo that.
 *
 * # Why the exclusion is the rail rather than a flag on the room
 *
 * "Rooms I am not in" is a question about membership, and membership is what
 * the rail IS. A room's own detail answers `member` too, and the two agree —
 * the detail is the authority and is what a row is finally drawn from — but
 * the rail is one read for every room, and a browse list that asked the server
 * "am I in this one" per row would be the same answer bought N times.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import type { QueryErrorCode } from "~/contract/errors.ts";
import { MAX_IN_FLIGHT_QUERIES } from "~/contract/wire.ts";
import { useClient, useConnection } from "~/lib/store-hooks.ts";
import { retryDelayMs, withFloor } from "~/lib/useQuery.ts";
import {
  queryErrorCode,
  type ChatChannelDetail,
  type ChatKind,
  type LogPosition,
} from "~/protocol/index.ts";
import { comparePositions } from "~/protocol/session.ts";

import { isDirect, knownKind } from "./rooms.ts";

/**
 * How many candidate rooms one pass resolves.
 *
 * ONE WAVE OF THE SOCKET — it IS `MAX_IN_FLIGHT_QUERIES`, the engine's
 * `stream.MaxInFlightQueries`, which is four: the number of queries one socket
 * may have running at once. Each row here costs one `chat_channel` read, and
 * past that number the engine stops reading the socket until one finishes, so
 * a fifth lookup does not arrive sooner for being asked sooner: it waits
 * behind the first four while the rail, the transcript and the mention feed
 * wait behind it. A list bounded to one wave delays the screen around it by
 * one read at most, and a list longer than this is one somebody narrows
 * rather than reads.
 *
 * DERIVED rather than spelled, so the two cannot part: the contract's copy is
 * held to the engine's by a Go gate, and a bound written here as its own
 * number was eight for as long as the socket ran four — two waves, holding
 * the screen behind its own lookups, under a comment saying it never would.
 *
 * What is cut is REPORTED rather than dropped, because a truncated list of
 * rooms and a company with few of them look identical from outside.
 */
export const MAX_BROWSE_ROOMS = MAX_IN_FLIGHT_QUERIES;

/** A room this tab has seen a live frame for. */
export interface Sighting {
  id: string;
  /** The name the frame carried. Empty for a direct conversation, which has
   *  none, and for a frame from a build that did not send one. */
  name: string;
  kind: ChatKind;
  /** Where on the chat log the newest frame seen for it sits — the LOG'S
   *  order, never the broker's instant, for the reason `comparePositions`
   *  gives — or null when no frame named one. */
  position: LogPosition | null;
}

/** What one browse pass should look up, and what it had to leave out. */
export interface Offered {
  ids: string[];
  /** Candidates past [MAX_BROWSE_ROOMS] — how many more there were to look at,
   *  never how many rooms the company has. */
  more: number;
}

/**
 * The rooms to offer: what the three sources found, minus what the viewer is
 * already in.
 *
 * SEARCH FIRST, IN RANK ORDER, because those rooms answer the question
 * somebody actually typed; the sightings follow, newest activity first, as
 * what the company happens to be doing right now; then every other room the
 * names read holds, in its order. Deduplicated across all three — a room that
 * was both searched and seen is one room — and every id the viewer already
 * holds is dropped, which is the whole point of the list.
 *
 * PURE, so the exclusion can be exercised without a socket. It is the rule
 * most likely to rot: a browse list that quietly includes the rooms somebody
 * is in reads as a directory that does not work, and nothing else in the
 * screen would notice.
 */
export function roomsToOffer(input: {
  hits: readonly { channel_id: string }[];
  seen: readonly Sighting[];
  /** The ids of the rooms the names read holds (`lib/roomNames.ts`), offered
   *  after the others; none while a search narrows the list. */
  named?: readonly string[];
  /** The channel ids the viewer is a member of — their rail, ARCHIVED ROOMS
   *  INCLUDED, or a room they are in but have closed would be offered back to
   *  them as one to join. */
  mine: ReadonlySet<string>;
  limit?: number;
}): Offered {
  const { hits, seen, named = [], mine, limit = MAX_BROWSE_ROOMS } = input;
  const order: string[] = [];
  const held = new Set<string>();
  const take = (id: string) => {
    if (!id || held.has(id) || mine.has(id)) return;
    held.add(id);
    order.push(id);
  };
  for (const hit of hits) take(hit.channel_id);
  // A COPY BEFORE THE SORT: the caller's array is the store's own derivation
  // and sorting it in place would reorder what the next render reads.
  for (const room of [...seen].sort((a, b) => comparePositions(b.position, a.position))) {
    take(room.id);
  }
  for (const id of named) take(id);
  return { ids: order.slice(0, limit), more: Math.max(0, order.length - limit) };
}

/** Where the viewer stands in one room, as a join or a leave needs to know. */
export interface RoomStanding {
  kind: ChatKind;
  member: boolean;
  /** The unit whose room it is, for `kind: unit` — named in the sentence
   *  that says why its membership is not the viewer's to move. */
  unit?: string;
}

/**
 * Whether this viewer may JOIN this room.
 *
 * THE ENGINE'S OWN REFUSALS, mirrored so the screen does not offer a button
 * that cannot work: a **private** room refuses a join, because its membership
 * is the only way into it and somebody already inside is who adds you; a
 * **unit's** room refuses one, because its membership is the org chart's
 * alone — readable by every seat, joined only by being in the unit — and a
 * member the chart did not put there would be taken out at the next apply; a
 * **direct** conversation refuses one, because its id IS its participant set,
 * so joining would not widen that room but name a different one; and a room
 * whose kind this build cannot classify refuses every write, because a kind
 * read two-valued falls out as "not private" and that is the one direction
 * that cannot be walked back.
 *
 * A private room the viewer is not in never reaches this: the read that would
 * describe it answers "no such room", exactly as a room that does not exist,
 * because a private room's EXISTENCE is information.
 */
export function canJoin(standing: RoomStanding): boolean {
  return (
    !standing.member &&
    knownKind(standing.kind) &&
    !isDirect(standing.kind) &&
    standing.kind !== "private" &&
    standing.kind !== "unit"
  );
}

/**
 * Whether this viewer may LEAVE it.
 *
 * A UNIT'S ROOM REFUSES A LEAVE, which looks like a restriction and is the
 * opposite: that membership is the org chart's own, so the next apply would
 * put it back — and a gesture that silently reverts is worse than one that is
 * refused, because nothing reports it. A direct conversation refuses one for
 * [canJoin]'s reason, and an unclassifiable room refuses everything.
 */
export function canLeave(standing: RoomStanding): boolean {
  return (
    standing.member &&
    knownKind(standing.kind) &&
    !isDirect(standing.kind) &&
    standing.kind !== "unit"
  );
}

/**
 * Why a room's membership is not this viewer's to move, or "" where it is.
 *
 * SAID RATHER THAN GREYED OUT. Each of these is a rule about the room rather
 * than about who is reading, and a disabled button with no sentence
 * beside it reads as a permission somebody lacks.
 */
export function membershipNote(standing: RoomStanding): string {
  if (!knownKind(standing.kind)) {
    return "This room was created by a newer build than this node runs, which refuses every write to it rather than guessing what kind of room it is.";
  }
  if (isDirect(standing.kind)) {
    return "A direct conversation is the people in it — its id is derived from their handles — so there is nobody to add and nothing to join. A different set of people is a different conversation.";
  }
  if (standing.kind === "private" && !standing.member) {
    return "A private room's membership is the only way into it, so it is joined by being added rather than by asking.";
  }
  if (standing.kind === "unit" && standing.member) {
    return "You are in this room because the org chart puts you there. Its membership is maintained by the unit, so leaving would be undone by the next apply.";
  }
  if (standing.kind === "unit") {
    const whose = standing.unit ? `the ${standing.unit} unit's` : "a unit's";
    return `This is ${whose} own room. Every seat can read it, and its membership is the org chart's: a seat is in it by being in the unit, so it is joined by moving into the unit in the company configuration rather than from here.`;
  }
  return "";
}

/** One room's detail, or why this pass has none. */
export interface RoomDetails {
  /** Keyed by channel id. A room absent here is one still being read, one
   *  the engine would not describe, or one that could not be read just now. */
  rooms: Map<string, ChatChannelDetail>;
  /**
   * The ids the engine would not describe.
   *
   * NOT AN ERROR TO RENDER. "No such room" and "not one you may read" are one
   * answer by design, so an id that came back missing is simply a room this
   * person has no business being offered — it leaves the list.
   */
  missing: Set<string>;
  /**
   * The ids this node COULD NOT SAY anything about just now — it was behind,
   * the read failed, the socket went away. Not [missing]: "not there or not
   * yours" and "could not say" are different answers, and folding the second
   * into the first made a room vanish from the list for the life of the
   * dialog over one blip. Said on screen, and asked again.
   */
  unreadable: Set<string>;
  loading: boolean;
  /** Ask about every unreadable room again now. */
  retry: () => void;
}

/**
 * The answers about a room that are FINAL: the engine read the question and
 * said the room cannot be described to this person — not there, not theirs
 * (one answer by design), not a room id at all — or that this reader may ask
 * no chat question at all: one without the Chatlet app, or a credential acting
 * as no seat, refused on authority. Waiting changes none of them, so they are
 * kept; every other failure is a fact about the node or the connection at
 * that moment, and is asked again.
 */
const FINAL: ReadonlySet<QueryErrorCode> = new Set<QueryErrorCode>([
  "not_found",
  "bad_params",
  "unauthorized",
]);

/**
 * Read each of these rooms, once — or again, where the last read could not
 * say.
 *
 * ONE QUERY PER ROOM, which is the shape the engine's reads leave: there is no
 * question that describes several rooms the viewer may not be in. They are
 * bounded by [MAX_BROWSE_ROOMS] at the caller, and an ANSWER is CACHED for the
 * life of the component — a description, or the engine's final word that
 * there is none — so widening a search does not re-ask about the rooms the
 * last one already resolved.
 *
 * A FAILURE THAT IS NOT AN ANSWER IS NOT CACHED. It is reported as
 * [RoomDetails.unreadable] and asked again: when the engine named a wait
 * (`unavailable`, as `useQuery` honours it), when the socket comes back, when
 * the caller asks ([RoomDetails.retry]), and on the next pass that wants it.
 *
 * ONE READ PER ID AT A TIME, AND IT BELONGS TO THE COMPONENT RATHER THAN TO
 * THE PASS THAT ASKED IT. A pass skips every id whose read is still out, so a
 * read that lands after its pass was superseded — the viewer's own rooms
 * answered and narrowed the list, a frame reordered the rooms seen, a search
 * kept a room the last one found — is the ONLY read that id will get. Its
 * answer is filed and redrawn whenever the component is still mounted, and
 * its wait is armed the same way; only a component that has gone draws
 * nothing.
 */
export function useRoomDetails(ids: readonly string[]): RoomDetails {
  const { socket } = useClient();
  const { connected } = useConnection();
  const cache = useRef(new Map<string, ChatChannelDetail>());
  const refused = useRef(new Set<string>());
  // WHAT COULD NOT BE SAID, by id — never consulted to skip a read, only to
  // report one.
  const failed = useRef(new Set<string>());
  const asking = useRef(new Set<string>());
  // A COUNTER RATHER THAN THE MAPS THEMSELVES: they are mutated in place, so
  // their identity says nothing about whether an answer has arrived. It is
  // never read — only depended on — which is what makes it the one piece of
  // state here that cannot get out of step with what has actually landed.
  const [settled, setSettled] = useState(0);
  // EACH BUMP IS ANOTHER PASS over whatever could not be read.
  const [attempt, setAttempt] = useState(0);
  const retry = useCallback(() => setAttempt((n) => n + 1), []);
  const key = ids.join("|");

  const wanted = useMemo(() => (key === "" ? [] : key.split("|")), [key]);

  // THE SOCKET COMING BACK is the moment a read that failed on its way out
  // can be answered, as every socket question is asked again then.
  const wasConnected = useRef(connected);
  useEffect(() => {
    if (connected && !wasConnected.current) retry();
    wasConnected.current = connected;
  }, [connected, retry]);

  // THE ENGINE'S OWN WAIT, where it named one, and nothing invented where it
  // did not (`retryDelayMs`).
  const timer = useRef<ReturnType<typeof setTimeout> | 0>(0);

  // WHETHER THERE IS STILL A SCREEN TO DRAW AN ANSWER ON — the component's
  // lifetime, never one pass's. Set by the mount rather than at the ref's
  // creation, so StrictMode's rehearsed unmount and remount leaves it true.
  const mounted = useRef(false);
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
      clearTimeout(timer.current);
      timer.current = 0;
    };
  }, []);

  useEffect(() => {
    // AN ID ALREADY BEING READ IS NOT ASKED AGAIN: its answer redraws this
    // component whichever pass asked for it, so a second read would only be
    // a second query for one room on a socket that runs four at once.
    const ask = wanted.filter(
      (id) => !cache.current.has(id) && !refused.current.has(id) && !asking.current.has(id),
    );
    for (const id of ask) {
      asking.current.add(id);
      failed.current.delete(id);
      // AT THIS TAB'S READ FLOOR, as every ask is (`withFloor`): a room this
      // tab has just joined, described by a node that has not applied the
      // join yet, would be offered back to it as a room to join.
      void socket
        .query("chat_channel", withFloor("chat_channel", JSON.stringify({ channel_id: id })))
        .then((answer) => {
          cache.current.set(id, answer);
        })
        .catch((err: unknown) => {
          const code = queryErrorCode(err instanceof Error ? err.message : null) ?? "query_failed";
          if (FINAL.has(code)) {
            refused.current.add(id);
            return;
          }
          failed.current.add(id);
          const wait = retryDelayMs(code, err);
          if (wait && mounted.current && !timer.current) {
            timer.current = setTimeout(() => {
              timer.current = 0;
              retry();
            }, wait);
          }
        })
        .finally(() => {
          asking.current.delete(id);
          // NEVER GATED ON THE PASS. Gated on it, a read whose pass had been
          // superseded filed its answer and redrew nothing — and since the
          // next pass skipped it for being out, nothing else ever would: the
          // room stayed "being read" for the life of the dialog.
          if (mounted.current) setSettled((n) => n + 1);
        });
    }
    // `attempt` is a dependency that is never read: it is what makes a pass
    // re-run over the ids whose last read could not say.
  }, [socket, wanted, attempt, retry]);

  // `settled` is a dependency that is never read: it is what MOVES when an
  // answer lands, and the maps it moves are mutated in place, so their
  // identity would never tell this memo that anything had arrived.
  //
  // LOADING IS DERIVED FROM THE ROWS rather than counted. A counter has to be
  // decremented by every path a request can end on, and the one that is easy
  // to miss is the request SUPERSEDED by a new search: its answer lands after
  // the wanted set moved, and a count that never came down leaves a panel
  // showing a skeleton over a list it already has. An id that is neither held
  // nor refused nor failed is one still out there, by construction.
  return useMemo(() => {
    const rooms = new Map<string, ChatChannelDetail>();
    const missing = new Set<string>();
    const unreadable = new Set<string>();
    let waiting = false;
    for (const id of wanted) {
      const held = cache.current.get(id);
      if (held) rooms.set(id, held);
      else if (refused.current.has(id)) missing.add(id);
      else if (failed.current.has(id)) unreadable.add(id);
      else waiting = true;
    }
    return { rooms, missing, unreadable, loading: waiting, retry };
  }, [wanted, settled, retry]);
}
