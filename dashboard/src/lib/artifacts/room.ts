/**
 * An artifact's room, as a screen holds it: whether the engine put this tab in
 * it and at what role, who else is there, and the pushes it hears.
 *
 * ONE ROOM PER ARTIFACT PER TAB. The socket joins on the first listener and
 * leaves on the last (`LiveSocket.room`), so the viewer's header, its sheet and
 * its frame's bridge each listen without three joins; this hook is the React
 * half, holding the roster the joins and the presence pushes describe.
 *
 * PRESENCE IS TOLD, NEVER STORED (ADR-0038): the roster here is what the
 * engine's hub last said, merged frame by frame, and nothing about it outlives
 * the page. The reader's own session is in the engine's roster too; it is
 * kept out of what the chrome draws (`others`), which is how "You and 3
 * others" counts.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type {
  ArtifactChangedPush,
  ArtifactDataPush,
  ArtifactJoined,
  RoomMember,
  ArtifactPresencePush,
  ArtifactRole,
  PresenceActivity,
  PresenceFocus,
  QueryError,
} from "~/protocol/index.ts";
import { PRESENCE_FRAMES_PER_SECOND } from "~/contract/presence.ts";
import { useClient } from "../store-hooks.ts";

/** Where the tab stands in the room. */
export type RoomState =
  /** Asked, and the engine has not answered (or the socket is reconnecting). */
  | { kind: "joining" }
  /** In the room, at this role. */
  | { kind: "joined"; role: ArtifactRole; session: string }
  /** The engine refused the join, or withdrew the room: the reader's access ended. */
  | { kind: "refused"; code: string };

/** Who is in the room, as the engine last described it. */
export interface Roster {
  /** Every member presented, the reader's own session included, by session. */
  members: ReadonlyMap<string, RoomMember>;
  /** Past fifty members the room presents the most recent and counts the rest. */
  counts: { human: number; agent: number } | null;
  truncated: boolean;
}

const EMPTY_ROSTER: Roster = { members: new Map(), counts: null, truncated: false };

/** What a screen in the room hears beside the roster. */
export interface RoomHandlers {
  /** Every join's answer: the first, and each a reconnect makes. */
  onJoined?: (answer: ArtifactJoined) => void;
  onChanged?: (push: ArtifactChangedPush) => void;
  onData?: (push: ArtifactDataPush) => void;
}

export interface ArtifactRoom {
  state: RoomState;
  roster: Roster;
  /** The members the chrome draws: everybody but the reader's own session. */
  others: RoomMember[];
  /** Say what the reader is doing; coalesced to the engine's per-socket rate. */
  say: (activity: PresenceActivity, focus: PresenceFocus) => void;
}

/** Merges one presence push into a roster: members upserted by session, sessions left removed. */
export function mergeRoster(roster: Roster, push: ArtifactPresencePush): Roster {
  const members = new Map(roster.members);
  for (const session of push.leave ?? []) members.delete(session);
  for (const member of push.upsert ?? []) members.set(member.session, member);
  return { members, counts: push.counts ?? null, truncated: push.truncated };
}

/** A join's roster as the map a room keeps. */
export function rosterOf(answer: ArtifactJoined): Roster {
  const members = new Map<string, RoomMember>();
  for (const member of answer.roster.members ?? []) members.set(member.session, member);
  return {
    members,
    counts: answer.roster.counts ?? null,
    truncated: answer.roster.truncated ?? false,
  };
}

/**
 * Be in artifact `id`'s room while the screen is mounted and `enabled`.
 *
 * `handlers` are read through a ref, so a screen passing new closures each
 * render does not leave and rejoin the room.
 */
export function useArtifactRoom(
  id: string,
  enabled: boolean,
  handlers: RoomHandlers = {},
): ArtifactRoom {
  const { socket } = useClient();
  const [state, setState] = useState<RoomState>({ kind: "joining" });
  const [roster, setRoster] = useState<Roster>(EMPTY_ROSTER);
  const latest = useRef(handlers);
  latest.current = handlers;

  useEffect(() => {
    if (!enabled || id === "") return undefined;
    setState({ kind: "joining" });
    setRoster(EMPTY_ROSTER);
    return socket.room(id, {
      joined: (answer) => {
        setState({ kind: "joined", role: answer.role, session: answer.session });
        setRoster(rosterOf(answer));
        latest.current.onJoined?.(answer);
      },
      refused: (error: QueryError) => {
        // UNAVAILABLE IS A NODE THAT COULD NOT DECIDE, which the socket asks
        // again on the engine's clock: the room is still being joined.
        if (error.message === "unavailable") setState({ kind: "joining" });
        else setState({ kind: "refused", code: error.message });
        setRoster(EMPTY_ROSTER);
      },
      changed: (push) => latest.current.onChanged?.(push),
      data: (push) => latest.current.onData?.(push),
      presence: (push) => setRoster((was) => mergeRoster(was, push)),
    });
  }, [socket, id, enabled]);

  const session = state.kind === "joined" ? state.session : "";
  const others = useMemo(
    () => [...roster.members.values()].filter((m) => m.session !== session),
    [roster, session],
  );

  // COALESCED TO THE ENGINE'S RATE: the socket's room drops frames past
  // PRESENCE_FRAMES_PER_SECOND, so a burst (an arrow key held down a sheet) is
  // folded into the last frame of each interval rather than sent and dropped.
  const pending = useRef<{ activity: PresenceActivity; focus: PresenceFocus } | null>(null);
  const timer = useRef<ReturnType<typeof setTimeout> | 0>(0);
  const lastSent = useRef(0);
  useEffect(
    () => () => {
      clearTimeout(timer.current);
      timer.current = 0;
    },
    [],
  );
  const say = useCallback(
    (activity: PresenceActivity, focus: PresenceFocus) => {
      pending.current = { activity, focus };
      if (timer.current) return;
      const interval = 1000 / PRESENCE_FRAMES_PER_SECOND;
      const wait = Math.max(0, lastSent.current + interval - Date.now());
      timer.current = setTimeout(() => {
        timer.current = 0;
        const frame = pending.current;
        pending.current = null;
        if (!frame) return;
        lastSent.current = Date.now();
        socket.presence(id, frame.activity, frame.focus);
      }, wait);
    },
    [socket, id],
  );

  return { state, roster, others, say };
}
