/**
 * The rooms you are not in.
 *
 * # The rail is membership; this is the rest of what you may read
 *
 * A public room and a unit's room are readable by every seat in the company,
 * joined or not, so a person's rail is narrower than their company. This is
 * the other list — a public room to join, and a unit's room to read, whose
 * membership is the org chart's.
 *
 * WHAT IT READS, all of it the engine's own visibility rather than a filter
 * invented on this side:
 *
 *   - `chat_search`, which ranks over every room the viewer may READ and says
 *     which room each hit was said in. It spans the whole corpus.
 *   - the live frames this tab has already received, which arrive for every
 *     room the viewer may read and carry the room's name and kind.
 *   - the names of every room the viewer may read (`chat_room_names`), which
 *     the frame reads once (`lib/roomNames.ts`), so a room nobody has spoken
 *     in is offered while nothing is searched for.
 *
 * `directory.ts` holds the selection and the reasoning.
 *
 * # Nothing here decides who may join
 *
 * A private room refuses a join outright (its membership is the only way in),
 * so does a unit's room (its membership is the org chart's, and a seat is in
 * it by being in the unit), a direct conversation refuses one (its id IS its
 * participant set), and a room whose kind this build cannot classify refuses
 * every write. Those are the engine's rules and `canJoin` mirrors them so the
 * screen does not offer a button whose only outcome is a refusal — never so
 * that it can decide anything the server has not already decided. A private
 * room the viewer is not in never reaches this list at all: the read that
 * would describe it answers "no such room", exactly as a room that does not
 * exist.
 */

import { useMemo, useState } from "react";

import { useNavigator } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { WriteButton, pressable } from "~/components/WriteButton.tsx";
import { Button, Callout, EmptyState, Input, Modal, Skeleton, Tag } from "@crewlethq/ui";
import { CompassGlyph, SearchGlyph } from "@crewlethq/icons/glyphs";
import { NamedGlyph } from "@crewlethq/icons/glyphs/registry";
import { useRoomNames } from "~/lib/roomNames.ts";
import { useSlice } from "~/lib/store-hooks.ts";
import { useAct } from "~/lib/useAct.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useSearchQuery } from "~/lib/useSearchQuery.ts";
import type { ChatChannelDetail, StoreState } from "~/protocol/index.ts";
import { CHAT_SEARCH_PAGE } from "~/contract/chat.ts";

import {
  canJoin,
  membershipNote,
  roomsToOffer,
  useRoomDetails,
  type Sighting,
} from "./directory.ts";
import { roomKindLabel, roomMark } from "./rooms.ts";
import { reportFor, type StartReport } from "./start.ts";
import { StartNote } from "./StartNote.tsx";

/** Every room this tab has seen a frame for, newest frame first. */
function sightings(state: StoreState): Sighting[] {
  return Object.entries(state.chat.rooms).map(([id, live]) => {
    const newest = live.changes[0];
    return {
      id,
      name: newest?.channel_name ?? "",
      kind: newest?.channel_kind ?? "",
      position: live.position,
    };
  });
}

export function Browse({ onClose }: { onClose: () => void }) {
  const nav = useNavigator();
  const join = useAct("join_channel");
  const [draft, setDraft] = useState("");
  const [q, setQ] = useState("");
  const [joining, setJoining] = useState("");
  const [report, setReport] = useState<StartReport | null>(null);

  // ON SUBMIT rather than per keystroke, for the reason the search screen
  // gives: each of these is a real ranking over this node's own index. A
  // phrase past the engine's bound is not sent, and the dialog says why
  // rather than reporting that no room came back — see `useSearchQuery`.
  // THE ENGINE'S OWN PAGE ([CHAT_SEARCH_PAGE]): the rooms offered are the
  // rooms its hits are in, so a page it would have clamped is rooms this
  // dialog would have believed it had found every one of.
  const found = useSearchQuery(
    "chat_search",
    { q, limit: CHAT_SEARCH_PAGE },
    { enabled: q.trim() !== "" },
  );

  // THE RAIL, ARCHIVED ROOMS INCLUDED — which the screen's own rail is not.
  // A room somebody is in and has archived would otherwise be offered back to
  // them here as a room to join, and joining it would write nothing at all.
  const mineAnswer = useQuery("chat_channels", { include_archived: true });
  const mine = useMemo(
    () => new Set((mineAnswer.data?.channels ?? []).map((row) => row.channel.id)),
    [mineAnswer.data],
  );

  const seen = useSlice(["chat"], sightings);
  // EVERY ROOM THE VIEWER MAY READ, by the frame's one read of their names,
  // offered only while nothing is searched for (`roomsToOffer`).
  const rooms = useRoomNames();
  const named = useMemo(() => (q === "" ? [...rooms.values()] : []), [q, rooms]);
  // A SEARCH THAT FAILED FOUND NOTHING — not even what the last one found,
  // which `useQuery` keeps on screen for a poll and which here would be
  // another phrase's rooms offered under this one.
  const answered = q !== "" && found.error === null ? found.data : null;
  const offered = useMemo(
    () => roomsToOffer({ hits: answered?.hits ?? [], seen, named, mine }),
    [answered, seen, named, mine],
  );
  const details = useRoomDetails(offered.ids);

  // THE DETAIL IS THE AUTHORITY ON MEMBERSHIP, and it is checked again here:
  // the rail is bounded at 256 rooms and says when it truncated, so a person
  // in more rooms than that has a `mine` that is genuinely incomplete.
  const rows = offered.ids
    .map((id) => details.rooms.get(id))
    .filter((detail): detail is ChatChannelDetail => detail !== undefined && !detail.member);

  async function joinRoom(channelID: string) {
    if (!pressable(join)) return;
    setJoining(channelID);
    setReport(null);
    try {
      // QUIET: the answer is this dialog's own note (`start.ts`), and a join
      // that landed takes the reader to the room, which is its confirmation.
      // The rail is asked again at the join's floor, so it needs no nudge.
      const result = await join.run(
        { channel: channelID },
        { done: "Joined the room", quiet: true },
      );
      if (result === null) return;
      const answer = reportFor("join", result);
      if (answer.go) {
        nav.to(["chatlet", answer.go]);
        onClose();
        return;
      }
      setReport(answer);
    } finally {
      setJoining("");
    }
  }

  return (
    <Modal
      open
      title="Find a room"
      icon={<CompassGlyph size="md" />}
      onClose={onClose}
      dismissable={!join.busy}
      closeDisabledReason="Waiting for the engine to answer."
      size="lg"
      stackBody
      onSubmit={() => setQ(draft.trim())}
    >
      <div className="toolbar">
        <div style={{ flex: 1, minWidth: 0 }}>
          <Input
            type="search"
            width="full"
            value={draft}
            autoFocus
            onChange={(event) => setDraft(event.target.value)}
            aria-label="Find a room by what is said in it"
            leading={<SearchGlyph size="sm" />}
            placeholder="What’s talked about there, not the room’s name"
          />
        </div>
        <Button variant="primary" onClick={() => setQ(draft.trim())}>
          Search
        </Button>
      </div>

      {mineAnswer.data?.truncated && (
        <Callout variant="warning">
          You&rsquo;re in more rooms than one list holds, so a room you&rsquo;re already in may show
          up below. Joining it again changes nothing.
        </Callout>
      )}
      {found.error === "unknown_query" ? (
        <Callout variant="neutral">
          Chat search isn&rsquo;t available on this node. The rooms below are the ones you can read
          and aren&rsquo;t in.
        </Callout>
      ) : (
        // ANY OTHER FAILURE IS NO ANSWER, said in the words every refused read
        // is drawn in — never "no room came back", which is an answer.
        q &&
        found.error && (
          <QueryState
            error={found.error}
            refusal={found.refusal}
            detail={found.detail}
            loading={false}
          />
        )
      )}

      {found.tooLong && <Callout variant="warning">{found.tooLong}</Callout>}
      {found.withheld && <Callout variant="info">{found.withheld}</Callout>}
      {answered && answered.hits.length >= CHAT_SEARCH_PAGE && (
        // A FULL PAGE IS NOT EVERY ROOM THAT MATCHED: the rooms listed are
        // the rooms the best [CHAT_SEARCH_PAGE] hits are in, and the engine
        // has no next page — so a room found further down is not offered.
        <p className="t-caption muted">{FULL_PAGE_ROOMS}</p>
      )}
      {q && found.data?.building && (
        // A PARTIAL INDEX FINDS PART OF WHAT HAS BEEN SAID, so a room missing
        // here may simply not be indexed yet — said rather than drawn as "no
        // room came back".
        <Callout variant="warning">
          Still indexing, so a room may be missing. Try again shortly.
        </Callout>
      )}

      {details.unreadable.size > 0 && (
        // COULD NOT SAY IS NOT "NOT THERE". A room this node could not read
        // just now is not a room to drop from the list for good: it is asked
        // again when the engine said to, when the socket is back, and here.
        <Callout
          variant="warning"
          action={
            <Button size="small" variant="secondary" onClick={details.retry}>
              Ask again
            </Button>
          }
        >
          {details.unreadable.size} room{details.unreadable.size === 1 ? "" : "s"} couldn&rsquo;t be
          read just now, so {details.unreadable.size === 1 ? "it isn’t" : "they aren’t"} listed.
        </Callout>
      )}

      <StartNote report={report} />

      {(details.loading || (q !== "" && found.loading)) && rows.length === 0 && (
        <Skeleton variant="text" rows={3} label="Reading the rooms" />
      )}

      {rows.length === 0 && !details.loading ? (
        // NOTHING WAS ASKED for a phrase past the bound, so "no room came
        // back" would report a search that never ran; the warning above is
        // the whole answer. And a search that FAILED, or a room that could
        // not be read, is not an answer that nothing is there: the verdict
        // is drawn only when an answer came back and every room in it was
        // read.
        found.tooLong ||
        found.withheld ||
        (q && !answered) ||
        found.data?.building ||
        details.unreadable.size > 0 ? null : (
          <EmptyState
            size="compact"
            icon={<CompassGlyph size="xl" />}
            title={q ? "No room to join" : "Search for a room"}
            description={
              q
                ? "You’re in every room that matched, or nothing matched. A room is found by what’s said in it, so a quiet room won’t show up."
                : "Type what a room talks about. This searches every room you can read, including public and team rooms you haven’t joined, and lists the ones you’re not in."
            }
          />
        )
      ) : (
        <div className="list">
          {rows.map((detail) => {
            const room = detail.channel;
            const standing = { kind: room.kind, member: detail.member, unit: room.unit };
            const note = membershipNote(standing);
            return (
              <div key={room.id} className="list-row">
                <NamedGlyph name={roomMark(room.kind)} size="sm" />
                <span className="col" style={{ gap: 0, flex: 1, minWidth: 0 }}>
                  <span className="row gap-2">
                    <strong className="truncate t-cell">#{room.name || room.id}</strong>
                    <Tag appearance="outline">{roomKindLabel(room.kind)}</Tag>
                    {room.archived_at && <Tag appearance="outline">archived</Tag>}
                  </span>
                  {room.topic && <span className="t-caption truncate">{room.topic}</span>}
                  <span className="t-caption muted">
                    {(detail.members ?? []).length} member
                    {(detail.members ?? []).length === 1 ? "" : "s"}
                    {note ? ` · ${note}` : ""}
                  </span>
                </span>
                {canJoin(standing) && (
                  <WriteButton
                    variant="secondary"
                    write={join}
                    showRefusal={false}
                    pressing={joining === room.id}
                    onPress={() => void joinRoom(room.id)}
                  >
                    {joining === room.id ? "Joining" : "Join"}
                  </WriteButton>
                )}
              </div>
            );
          })}
        </div>
      )}

      {offered.more > 0 && (
        <p className="t-caption muted">
          {offered.more} more room{offered.more === 1 ? "" : "s"}{" "}
          {q
            ? "matched than fit here. Narrow the search to find the one you want."
            : "than fit here. Search for what’s said in the one you want."}
        </p>
      )}

      <p className="t-caption muted measure">
        These are the rooms you can read and aren&rsquo;t in, the busiest first. A search finds a
        room by what&rsquo;s been said in it.
      </p>
    </Modal>
  );
}

/** What a full page of hits says above the rooms it offers. */
export const FULL_PAGE_ROOMS = `These are the rooms in the ${CHAT_SEARCH_PAGE} best matches, and more may match. Add a word to narrow it.`;
