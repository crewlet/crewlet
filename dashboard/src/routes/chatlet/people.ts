/**
 * The company's seats, as chat draws them and offers them.
 *
 * # One reading of a seat for every chat surface
 *
 * A message row's face, a switcher's conversation, a member list, a profile,
 * the people a dialog suggests and a search's People tab all name the same
 * colleagues, and they must name each one the same way: the same name, the
 * same face, the same state. So every one of them reads a seat through
 * [useChatPeople], which joins the org chart (who a handle is, how an agent is
 * drawn, which team it sits in) with the agents roster (what the seat is doing
 * now, by the engine's own word) once.
 *
 * A HANDLE IS WHAT TRAVELS, never a display name: the engine's membership and
 * participant sets are handles, and a seat's handle is DERIVED by the engine
 * (a slug of the role name, which this client deliberately does not
 * re-implement; see `lib/seats.ts`). A seat the projection reports no handle
 * for is therefore never offered: naming it would mean inventing the one value
 * the engine owns.
 *
 * A HANDLE THE CHART DOES NOT HOLD IS STILL SOMEBODY. A message outlives the
 * seat that wrote it, so a row can name a handle nobody answers to any more;
 * it is drawn under the handle itself, with the kind the message recorded,
 * rather than dropped or guessed into a colleague.
 *
 * # What a seat is doing is the engine's word, mapped
 *
 * The state and the badge are `lib/seats.ts`' mappers over the roster row the
 * engine serves, matched by handle. Nothing here reads a live call to decide
 * whether a seat is working.
 *
 * AGENTS ARE PEOPLE HERE. A company running this engine talks to its seats, and
 * a picker that offered only the humans would make the product's premise
 * unreachable from the screen a person starts a conversation on. They are
 * ordered after the humans rather than mixed with them, because who you are
 * addressing (a colleague, or a seat that will run a turn about it) is the one
 * thing worth knowing before you press send.
 */

import { useMemo } from "react";

import type { AvatarStatus } from "@crewlethq/ui";
import type { AgentAvatar } from "~/lib/avatar.ts";
import {
  shownStateOf,
  stateLine,
  statusOf,
  useOrgIndex,
  type Seat,
  type ShownState,
} from "~/lib/seats.ts";
import { useAgents } from "~/lib/store-hooks.ts";
import type { AgentRow, ChatAuthorKind } from "~/protocol/index.ts";

/** A seat as chat draws it. */
export interface ChatPerson {
  handle: string;
  /** The chart's name for the seat, or the handle where the chart holds none. */
  name: string;
  kind: "human" | "agent";
  /** How an agent is drawn (its character and colour); null for a person. */
  avatar: AgentAvatar | null;
  /** The team the seat sits in, as the chart names it, or "". */
  team: string;
  /** The engine's word for the seat; `offline` for a person and for a seat
   *  no roster row is held for yet. */
  state: ShownState;
  /** The badge on the face's corner, or undefined for an idle seat. */
  status: AvatarStatus | undefined;
  /** What a working seat is doing, in the words the Live page uses
   *  ("Executing ENG-412"), or "". */
  doing: string;
  /** Whether the org chart holds this handle. */
  known: boolean;
}

/** Every seat chat can name, and how to read one by its handle. */
export interface ChatPeople {
  /** Every seat with a handle: people first, then agents, each by name. */
  everyone: readonly ChatPerson[];
  /**
   * The seat a handle names. `kind` is what a message recorded about its
   * author, used only where the chart holds no such handle.
   */
  person: (handle: string, kind?: ChatAuthorKind) => ChatPerson;
  /** The seat's name, or the handle itself. */
  nameOf: (handle: string) => string;
}

/** A seat off the chart, before the roster says what it is doing. */
function fromSeat(seat: Seat): ChatPerson {
  return {
    handle: seat.handle,
    name: seat.name || seat.handle,
    kind: seat.kind,
    avatar: seat.kind === "agent" ? seat.avatar : null,
    team: seat.unit?.name ?? "",
    state: "offline",
    status: undefined,
    doing: "",
    known: true,
  };
}

/**
 * A handle nobody in the chart answers to: drawn as itself.
 *
 * THE KIND IS THE MESSAGE'S where it recorded one: `human` and `operator` are
 * people (`ChatAuthorKind` says an operator is drawn as a person, never as an
 * agent), and anything else takes the agent's drawing, the kit's own default,
 * because the engine runs agents and a person is always declared.
 */
function stranger(handle: string, kind: ChatAuthorKind | undefined): ChatPerson {
  return {
    handle,
    name: handle,
    kind: kind === "human" || kind === "operator" ? "human" : "agent",
    avatar: null,
    team: "",
    state: "offline",
    status: undefined,
    doing: "",
    known: false,
  };
}

/** A person with what the roster says the seat is doing now. */
function withState(person: ChatPerson, row: AgentRow | undefined): ChatPerson {
  // A PERSON IS NOT RUN BY THE ENGINE, so the roster has no row to read; and
  // a seat with no row yet is `offline`, which draws no badge.
  if (person.kind === "human" || row === undefined) return person;
  const state = shownStateOf(row);
  return {
    ...person,
    state,
    status: statusOf(state),
    // ONLY A WORKING SEAT HAS SOMETHING TO SAY HERE: the working line names
    // the item the turn is charged to and reads no clock, where an idle
    // seat's line ("Idle · last turn 24m ago") would tick under a list that
    // is not otherwise live.
    doing: state === "working" ? stateLine(row, { now: 0 }) : "",
  };
}

/**
 * Every seat chat can name, from the chart and the roster.
 *
 * Pure, so the order and the fallbacks are testable without a store.
 */
export function chatPeople(
  index: { seats: readonly Seat[] },
  agents: readonly AgentRow[],
): ChatPeople {
  // MATCHED BY HANDLE, which is the roster row's `id`, and never by name: the
  // rule `liveRowFor` states, kept here as one lookup per seat rather than a
  // walk of the roster per seat.
  const rows = new Map(agents.map((row) => [row.id, row]));
  const byHandle = new Map<string, ChatPerson>();
  for (const seat of index.seats) {
    if (seat.handle === "") continue;
    byHandle.set(seat.handle, withState(fromSeat(seat), rows.get(seat.handle)));
  }
  // PEOPLE FIRST, THEN BY NAME: a roster somebody scans is alphabetical, where
  // the document's own order is an implementation detail of how the org chart
  // happens to be written.
  const rank = (person: ChatPerson) => (person.kind === "human" ? 0 : 1);
  const everyone = [...byHandle.values()].sort(
    (a, b) => rank(a) - rank(b) || a.name.localeCompare(b.name),
  );
  const person = (handle: string, kind?: ChatAuthorKind) =>
    byHandle.get(handle) ?? stranger(handle, kind);
  return { everyone, person, nameOf: (handle) => person(handle).name };
}

/** Every seat chat can name, for the seat this tab reads as. */
export function useChatPeople(): ChatPeople {
  const index = useOrgIndex();
  const agents = useAgents();
  return useMemo(() => chatPeople(index, agents), [index, agents]);
}

/**
 * Whether a seat answers to what somebody typed: its name, its handle, its
 * team or what it is doing now, case-insensitively.
 *
 * THE WORDS A READER CAN SEE ON THE ROW, and nothing else: a match on a field
 * the row does not draw is a result nobody can tell the reason for.
 */
export function matchesPerson(person: ChatPerson, query: string): boolean {
  const q = query.trim().toLowerCase();
  if (q === "") return true;
  return [person.name, person.handle, person.team, person.doing].some((text) =>
    text.toLowerCase().includes(q),
  );
}

/** The handles, as the write path takes them: trimmed, deduplicated, ordered. */
export function handlesOf(chosen: readonly string[]): string[] {
  return [...new Set(chosen.map((handle) => handle.trim()).filter(Boolean))];
}
