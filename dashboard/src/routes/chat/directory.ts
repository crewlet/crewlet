/**
 * The rooms that are NOT in the rail.
 *
 * # What this has to work with, and what does not exist
 *
 * The rail is the viewer's own membership. What a person needs before they
 * have a rail is the other list — the public and unit rooms of the company
 * they have not joined — and **the engine serves no directory read**: the six
 * chat questions are the viewer's rooms, one room by id, a transcript, a
 * thread, the mention feed and a keyword search. There is no "every room I may
 * read" among them, and inventing one is not this screen's to do.
 *
 * So the candidates are derived from the two reads that genuinely reach past
 * the rail, and both of them are the engine's own visibility rather than a
 * guess made here:
 *
 *   - **SEARCH.** `chat_search` ranks over every room the viewer may READ,
 *     which is wider than the rail by exactly the set this list is about, and
 *     the answer carries the room each hit was said in. It spans the whole
 *     corpus rather than this session, which is what makes it the primary
 *     source — but it only ever finds a room somebody has SAID something in.
 *   - **THE LIVE FRAMES.** Every committed record reaches this socket for
 *     every room the viewer may read, filtered per socket by the engine's own
 *     `Visible` rule, and the frame carries the room's name and kind. So a
 *     room that is merely busy is discoverable without anybody searching for
 *     it — but only while this tab has been connected.
 *
 * Neither finds a room that is silent and was created before this tab opened.
 * That is a real gap and it is stated on the screen rather than papered over:
 * the honest fix is a read the engine does not have.
 *
 * # Why the exclusion is the rail rather than a flag on the room
 *
 * "Rooms I am not in" is a question about membership, and membership is what
 * the rail IS. A room's own detail answers `member` too, and the two agree —
 * the detail is the authority and is what a row is finally drawn from — but
 * the rail is one read for every room, and a browse list that asked the server
 * "am I in this one" per row would be the same answer bought N times.
 */

import { useEffect, useMemo, useRef, useState } from "react";

import { useClient } from "~/lib/store-hooks.ts";
import type { ChatChannelDetail, ChatKind } from "~/protocol/index.ts";

import { isDirect, knownKind } from "./rooms.ts";

/**
 * How many candidate rooms one pass resolves.
 *
 * EIGHT — `stream.MaxInFlightQueries`, the number of queries one socket may
 * have running at once. Each row here costs one `chat_channel` read, and the
 * engine runs eight concurrently and QUEUES the rest, so a ninth lookup does
 * not arrive sooner for being asked sooner: it waits behind the first eight
 * while the rail, the transcript and the mention feed wait behind it. A list
 * bounded to one wave is a list that never delays the screen around it, and a
 * list longer than this is one somebody narrows rather than reads.
 *
 * What is cut is REPORTED rather than dropped, because a truncated list of
 * rooms and a company with few of them look identical from outside.
 */
export const MAX_BROWSE_ROOMS = 8;

/** A room this tab has seen a live frame for. */
export interface Sighting {
  id: string;
  /** The name the frame carried. Empty for a direct conversation, which has
   *  none, and for a frame from a build that did not send one. */
  name: string;
  kind: ChatKind;
  /** The broker's instant on the newest frame seen for it. */
  at: string;
}

/** What one browse pass should look up, and what it had to leave out. */
export interface Offered {
  ids: string[];
  /** Candidates past [MAX_BROWSE_ROOMS] — how many more there were to look at,
   *  never how many rooms the company has. */
  more: number;
}

/**
 * The rooms to offer: what the two sources found, minus what the viewer is
 * already in.
 *
 * SEARCH FIRST, IN RANK ORDER, because those rooms answer the question
 * somebody actually typed; the sightings follow, newest activity first, as
 * what the company happens to be doing right now. Deduplicated across both —
 * a room that was both searched and seen is one room — and every id the viewer
 * already holds is dropped, which is the whole point of the list.
 *
 * PURE, so the exclusion can be exercised without a socket. It is the rule
 * most likely to rot: a browse list that quietly includes the rooms somebody
 * is in reads as a directory that does not work, and nothing else in the
 * screen would notice.
 */
export function roomsToOffer(input: {
  hits: readonly { channel_id: string }[];
  seen: readonly Sighting[];
  /** The channel ids the viewer is a member of — their rail, ARCHIVED ROOMS
   *  INCLUDED, or a room they are in but have closed would be offered back to
   *  them as one to join. */
  mine: ReadonlySet<string>;
  limit?: number;
}): Offered {
  const { hits, seen, mine, limit = MAX_BROWSE_ROOMS } = input;
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
  for (const room of [...seen].sort((a, b) => (a.at < b.at ? 1 : a.at > b.at ? -1 : 0))) {
    take(room.id);
  }
  return { ids: order.slice(0, limit), more: Math.max(0, order.length - limit) };
}

/** Where the viewer stands in one room, as a join or a leave needs to know. */
export interface RoomStanding {
  kind: ChatKind;
  member: boolean;
}

/**
 * Whether this viewer may JOIN this room.
 *
 * THE ENGINE'S OWN REFUSALS, mirrored so the screen does not offer a button
 * that cannot work: a **private** room refuses a join, because its membership
 * is the only way into it and somebody already inside is who adds you; a
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
    standing.kind !== "private"
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
 * than about the reader's credential, and a disabled button with no sentence
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
  return "";
}

/** One room's detail, or why this pass has none. */
export interface RoomDetails {
  /** Keyed by channel id. A room absent here is one still being read, or one
   *  the read refused. */
  rooms: Map<string, ChatChannelDetail>;
  /**
   * The ids the engine would not describe.
   *
   * NOT AN ERROR TO RENDER. "No such room" and "not one you may read" are one
   * answer by design, so an id that came back missing is simply a room this
   * person has no business being offered — it leaves the list.
   */
  missing: Set<string>;
  loading: boolean;
}

/**
 * Read each of these rooms, once.
 *
 * ONE QUERY PER ROOM, which is the shape the engine's reads leave: there is no
 * question that describes several rooms the viewer may not be in. They are
 * bounded by [MAX_BROWSE_ROOMS] at the caller and CACHED for the life of the
 * component, so widening a search does not re-ask about the rooms the last one
 * already resolved.
 *
 * THE ANSWER IS FILED UNDER THE ID THAT WAS ASKED FOR. An in-flight read is
 * abandoned when the component goes, because an answer applied to a screen
 * that has been replaced is a render of somebody else's question.
 */
export function useRoomDetails(ids: readonly string[]): RoomDetails {
  const { socket } = useClient();
  const cache = useRef(new Map<string, ChatChannelDetail>());
  const refused = useRef(new Set<string>());
  // A COUNTER RATHER THAN THE MAPS THEMSELVES: they are mutated in place, so
  // their identity says nothing about whether an answer has arrived. It is
  // never read — only depended on — which is what makes it the one piece of
  // state here that cannot get out of step with what has actually landed.
  const [settled, setSettled] = useState(0);
  const key = ids.join("|");

  const wanted = useMemo(() => (key === "" ? [] : key.split("|")), [key]);

  useEffect(() => {
    const missing = wanted.filter((id) => !cache.current.has(id) && !refused.current.has(id));
    if (missing.length === 0) return;
    // A LOOKUP IS NEVER ABANDONED, ONLY THE RE-RENDER IT WOULD HAVE CAUSED.
    // An answer that arrives after the wanted set moved is still the truth
    // about that room, so it is filed; what the disposed flag stops is asking
    // a component that has gone to draw it.
    let live = true;
    for (const id of missing) {
      void socket
        .query("chat_channel", { channel_id: id })
        .then((answer) => {
          cache.current.set(id, answer);
        })
        .catch(() => {
          // EVERY FAILURE IS THE SAME ANSWER HERE. A room that is not there,
          // one this viewer may not read, and a node that could not say are
          // all "this row cannot be offered", and guessing between them would
          // be the client deciding what somebody may know about a room.
          refused.current.add(id);
        })
        .finally(() => {
          if (live) setSettled((n) => n + 1);
        });
    }
    return () => {
      live = false;
    };
  }, [socket, wanted]);

  // `settled` is a dependency that is never read: it is what MOVES when an
  // answer lands, and the two maps it moves are mutated in place, so their
  // identity would never tell this memo that anything had arrived.
  //
  // LOADING IS DERIVED FROM THE ROWS rather than counted. A counter has to be
  // decremented by every path a request can end on, and the one that is easy
  // to miss is the request SUPERSEDED by a new search: its answer lands after
  // the wanted set moved, and a count that never came down leaves a panel
  // showing a skeleton over a list it already has. An id that is neither held
  // nor refused is one still out there, by construction.
  return useMemo(() => {
    const rooms = new Map<string, ChatChannelDetail>();
    const missing = new Set<string>();
    let waiting = false;
    for (const id of wanted) {
      const held = cache.current.get(id);
      if (held) rooms.set(id, held);
      else if (refused.current.has(id)) missing.add(id);
      else waiting = true;
    }
    return { rooms, missing, loading: waiting };
  }, [wanted, settled]);
}
