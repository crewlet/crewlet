/**
 * The rooms you are not in.
 *
 * # The rail is membership; this is the rest of what you may read
 *
 * A public room and a unit's room are readable by every seat in the company,
 * joined or not, so a person's rail is narrower than their company. This is
 * the other list — and it is the one gesture in the chat screen the engine
 * offers no read for: there is no directory question among the six, so nothing
 * here can simply ask for "every room I may read".
 *
 * WHAT IT ASKS INSTEAD, and both are the engine's own visibility rather than a
 * filter invented on this side:
 *
 *   - `chat_search`, which ranks over every room the viewer may READ and says
 *     which room each hit was said in. It spans the whole corpus.
 *   - the live frames this tab has already received, which arrive for every
 *     room the viewer may read and carry the room's name and kind.
 *
 * What neither finds is a room that has been SILENT since before this tab
 * connected. That is a real hole and the screen says so out loud rather than
 * presenting a partial list as the company's rooms — see the note at the foot.
 * `directory.ts` holds the selection and the reasoning.
 *
 * # Nothing here decides who may join
 *
 * A private room refuses a join outright (its membership is the only way in),
 * a direct conversation refuses one (its id IS its participant set), and a
 * room whose kind this build cannot classify refuses every write. Those are
 * the engine's rules and `canJoin` mirrors them so the screen does not offer a
 * button whose only outcome is a refusal — never so that it can decide
 * anything the server has not already decided. A private room the viewer is
 * not in never reaches this list at all: the read that would describe it
 * answers "no such room", exactly as a room that does not exist.
 */

import { useMemo, useState } from "react";

import { useNavigator } from "~/app/router.tsx";
import { Button, Callout, EmptyState, Input, Modal, Skeleton, Tag } from "@crewlethq/ui";
import { ExploreGlyph, SearchGlyph } from "@crewlethq/icons/glyphs";
import { Mark } from "~/ui/glyph.tsx";
import { useSlice } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import type { ChatChannelDetail, StoreState } from "~/protocol/index.ts";

import {
  canJoin,
  membershipNote,
  roomsToOffer,
  useRoomDetails,
  type Sighting,
} from "./directory.ts";
import { PAGE } from "./page.ts";
import { roomKindLabel, roomMark } from "./rooms.ts";
import { reportFor, type StartReport } from "./start.ts";
import { StartNote } from "./StartNote.tsx";
import { joinRoom } from "./writes.ts";

/** Every room this tab has seen a frame for, newest frame first. */
function sightings(state: StoreState): Sighting[] {
  return Object.entries(state.chat.rooms).map(([id, live]) => {
    const newest = live.changes[0];
    return {
      id,
      name: newest?.channel_name ?? "",
      kind: newest?.channel_kind ?? "",
      at: live.at,
    };
  });
}

export function Browse({ onClose, onWrote }: { onClose: () => void; onWrote: () => void }) {
  const nav = useNavigator();
  const [draft, setDraft] = useState("");
  const [q, setQ] = useState("");
  const [joining, setJoining] = useState("");
  const [report, setReport] = useState<StartReport | null>(null);

  // ON SUBMIT rather than per keystroke, for the reason the search screen
  // gives: each of these is a real ranking over this node's own index.
  const found = useQuery("chat_search", { q, limit: PAGE }, { enabled: q.trim() !== "" });

  // THE RAIL, ARCHIVED ROOMS INCLUDED — which the screen's own rail is not.
  // A room somebody is in and has archived would otherwise be offered back to
  // them here as a room to join, and joining it would write nothing at all.
  const mineAnswer = useQuery("chat_channels", { include_archived: true });
  const mine = useMemo(
    () => new Set((mineAnswer.data?.channels ?? []).map((row) => row.channel.id)),
    [mineAnswer.data],
  );

  const seen = useSlice(["chat"], sightings);
  const offered = useMemo(
    () => roomsToOffer({ hits: found.data?.hits ?? [], seen, mine }),
    [found.data, seen, mine],
  );
  const details = useRoomDetails(offered.ids);

  // THE DETAIL IS THE AUTHORITY ON MEMBERSHIP, and it is checked again here:
  // the rail is bounded at 256 rooms and says when it truncated, so a person
  // in more rooms than that has a `mine` that is genuinely incomplete.
  const rows = offered.ids
    .map((id) => details.rooms.get(id))
    .filter((detail): detail is ChatChannelDetail => detail !== undefined && !detail.member);

  async function join(channelID: string) {
    if (joining) return;
    setJoining(channelID);
    setReport(null);
    try {
      const answer = reportFor("join", await joinRoom(channelID));
      onWrote();
      if (answer.go) {
        nav.to(["chat", answer.go]);
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
      icon={<ExploreGlyph size="md" />}
      onClose={onClose}
      dismissable={joining === ""}
      closeDisabledReason="Waiting for the log to acknowledge."
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
            placeholder="What is talked about there — words, not a room name"
          />
        </div>
        <Button variant="primary" onClick={() => setQ(draft.trim())}>
          Search
        </Button>
      </div>

      {mineAnswer.data?.truncated && (
        <Callout variant="warning">
          You are in more rooms than one list holds, so a room you are already in may appear below.
          Joining it again writes nothing — membership is a set.
        </Callout>
      )}
      {found.error === "unknown_query" && (
        <Callout variant="neutral">
          This node has no chat index, so nothing can be searched for here. The rooms below are the
          ones that have been active while this tab has been open.
        </Callout>
      )}

      <StartNote report={report} />

      {details.loading && rows.length === 0 && (
        <Skeleton variant="text" rows={3} label="Reading the rooms" />
      )}

      {rows.length === 0 && !details.loading ? (
        <EmptyState
          size="compact"
          icon={<ExploreGlyph size="xl" />}
          title={q ? "No room to join came back" : "Search for a room"}
          description={
            q
              ? "Every room that matched is one you are already in, or nothing matched at all. A room is found here by something said in it, so one nobody has spoken in is not findable this way."
              : "Type what a room talks about. This searches every room you may read — which includes the public and unit rooms you have not joined — and lists the ones you are not in."
          }
        />
      ) : (
        <div className="list">
          {rows.map((detail) => {
            const room = detail.channel;
            const standing = { kind: room.kind, member: detail.member };
            const note = membershipNote(standing);
            return (
              <div key={room.id} className="list-row">
                <Mark name={roomMark(room.kind)} size="sm" />
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
                    {note ? ` — ${note}` : ""}
                  </span>
                </span>
                {canJoin(standing) && (
                  <Button
                    variant="secondary"
                    onClick={() => void join(room.id)}
                    disabled={joining !== ""}
                  >
                    {joining === room.id ? "Joining" : "Join"}
                  </Button>
                )}
              </div>
            );
          })}
        </div>
      )}

      {offered.more > 0 && (
        <p className="t-caption muted">
          {offered.more} more room{offered.more === 1 ? "" : "s"} matched than this list reads at
          once. Narrow the search, and the ones you want will be in it.
        </p>
      )}

      <p className="t-caption muted measure">
        A room is found here by what has been said in it, or by having been busy while this tab was
        open. A room nobody has spoken in since you arrived is not in this list — the engine serves
        no directory of rooms, and a list that quietly left them out would read as the company’s
        rooms rather than as what could be found.
      </p>
    </Modal>
  );
}
