/**
 * Browse rooms: every room the reader may read, joined or not, with what each
 * is about, how many are in it and when it last spoke, and Join or Open.
 *
 * # One answer, the engine's visibility
 *
 * The list is `chat_rooms`: every named room the reader may READ (a public
 * room, a unit's room, and a private room they are in), archived ones
 * included, each with its member count, whether the reader is in it, and its
 * newest message's instant. A private room the reader is not in never reaches
 * this list, exactly as a room that does not exist. Filtering and sorting are
 * this screen's, over that one answer, so they ask nothing.
 *
 * # Nothing here decides who may join
 *
 * A private room refuses a join outright (its membership is the only way in),
 * so does a unit's room (its members are the org chart's), and a room whose
 * kind this build cannot classify refuses every write. Those are the engine's
 * rules, and `canJoin` mirrors them so the screen does not offer a button
 * whose only outcome is a refusal; Open is offered instead.
 */

import { useMemo, useState } from "react";

import {
  Button,
  ButtonLink,
  Callout,
  EmptyState,
  Input,
  SegmentedControl,
  Select,
  Tag,
} from "@crewlethq/ui";
import { CompassGlyph, PlusGlyph, SearchGlyph } from "@crewlethq/icons/glyphs";
import { NamedGlyph } from "@crewlethq/icons/glyphs/registry";

import { QueryState } from "~/components/common.tsx";
import { WriteButton, pressable } from "~/components/WriteButton.tsx";
import { useNow } from "~/lib/clock.ts";
import { plural, relTime } from "~/lib/format.ts";
import { useAct } from "~/lib/useAct.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useWriteAccess } from "~/lib/useWriteAccess.ts";
import type { ChatDirectoryRoom } from "~/protocol/index.ts";

import { canJoin, membershipNote } from "./directory.ts";
import { useChatNav } from "./nav.tsx";
import { roomMark } from "./rooms.ts";
import { reportFor, type StartReport } from "./start.ts";
import { StartNote } from "./StartNote.tsx";
import { ViewHead } from "./Views.tsx";

type Kind = "all" | "public" | "unit" | "archived";
type Sort = "activity" | "name" | "members";

/** Whether a room is one the Kind filter keeps. */
export function keptByKind(kind: Kind, room: ChatDirectoryRoom): boolean {
  const archived = !!room.channel.archived_at;
  if (kind === "archived") return archived;
  return !archived && (kind === "all" || room.channel.kind === kind);
}

/** Whether a room answers to what somebody typed: its name or its topic. */
export function matchesRoom(room: ChatDirectoryRoom, query: string): boolean {
  const q = query.trim().toLowerCase();
  if (q === "") return true;
  return [room.channel.name ?? "", room.channel.topic ?? ""].some((text) =>
    text.toLowerCase().includes(q),
  );
}

/**
 * The rooms in the order the reader asked for. A room nobody has spoken in
 * sorts after every room somebody has, by name, so a quiet room never jumps
 * ahead of a busy one for lack of an instant.
 */
export function sortRooms(rooms: readonly ChatDirectoryRoom[], sort: Sort): ChatDirectoryRoom[] {
  const name = (room: ChatDirectoryRoom) => room.channel.name ?? "";
  const byName = (a: ChatDirectoryRoom, b: ChatDirectoryRoom) => name(a).localeCompare(name(b));
  return [...rooms].sort((a, b) => {
    if (sort === "name") return byName(a, b);
    if (sort === "members") return b.members - a.members || byName(a, b);
    const at = (room: ChatDirectoryRoom) => (room.last_at ? Date.parse(room.last_at) : -Infinity);
    return at(b) === at(a) ? byName(a, b) : at(b) > at(a) ? 1 : -1;
  });
}

export function BrowseView() {
  const nav = useChatNav();
  const create = useWriteAccess("create_channel");
  const answer = useQuery("chat_rooms", {});
  const [q, setQ] = useState("");
  const [kind, setKind] = useState<Kind>("all");
  const [sort, setSort] = useState<Sort>("activity");
  const rooms = useMemo(
    () =>
      sortRooms(
        (answer.data?.rooms ?? []).filter((room) => keptByKind(kind, room) && matchesRoom(room, q)),
        sort,
      ),
    [answer.data, kind, q, sort],
  );

  return (
    <section className="chat-view">
      <ViewHead title="Browse rooms">
        <Button
          size="small"
          variant="primary"
          leadingIcon={<PlusGlyph size="sm" />}
          disabledReason={create.can ? undefined : create.reason}
          title={create.can ? undefined : create.reason}
          onClick={() => nav.openDialog({ kind: "new-room" })}
        >
          New room
        </Button>
      </ViewHead>
      <div className="chat-search-bar">
        <Input
          width="full"
          leading={<SearchGlyph size="sm" />}
          placeholder="Search by name or topic"
          aria-label="Search rooms"
          value={q}
          onChange={(event) => setQ(event.target.value)}
          onClear={() => setQ("")}
        />
        <div className="chat-search-filters">
          <SegmentedControl
            label="Kind"
            semantics="radio"
            size="sm"
            value={kind}
            onValueChange={(value) => setKind(value)}
            options={[
              { value: "all", label: "All" },
              { value: "public", label: "Public" },
              { value: "unit", label: "Team rooms" },
              { value: "archived", label: "Archived" },
            ]}
          />
          <Select
            size="sm"
            width="auto"
            ariaLabel="Sort"
            value={sort}
            onChange={(value) => setSort(value as Sort)}
            options={[
              { value: "activity", label: "Sort: most recent activity" },
              { value: "name", label: "Sort: name" },
              { value: "members", label: "Sort: most members" },
            ]}
          />
        </div>
      </div>
      <div className="chat-view-scroll">
        {answer.data?.truncated && (
          <Callout variant="warning">
            There are more rooms than one list holds, so some aren&rsquo;t listed. Search for the
            one you want by name.
          </Callout>
        )}
        {(answer.data?.unreadable ?? 0) > 0 && (
          <Callout variant="neutral">
            {plural(answer.data?.unreadable ?? 0, "room")} came from a newer version of the engine
            and {answer.data?.unreadable === 1 ? "isn’t" : "aren’t"} listed.
          </Callout>
        )}
        <QueryState
          error={answer.error}
          refusal={answer.refusal}
          detail={answer.detail}
          loading={answer.loading}
        >
          {rooms.length === 0 ? (
            <EmptyState
              size="compact"
              icon={<CompassGlyph size="xl" />}
              title="No rooms match"
              description="Try another word, or make the room yourself."
            />
          ) : (
            <ul className="chat-browse-list">
              {rooms.map((room) => (
                <BrowseRow key={room.channel.id} room={room} />
              ))}
            </ul>
          )}
        </QueryState>
      </div>
    </section>
  );
}

/**
 * A room as Browse rooms lists it: its mark, name and state, topic, size and
 * last message, then Join where the engine would take one, or Open.
 */
export function BrowseRow({ room }: { room: ChatDirectoryRoom }) {
  const nav = useChatNav();
  const now = useNow();
  const join = useAct("join_channel");
  const [report, setReport] = useState<StartReport | null>(null);
  const { channel } = room;
  const member = !!room.member;
  const standing = { kind: channel.kind, member, unit: channel.unit };
  const archived = !!channel.archived_at;
  const joinable = !archived && canJoin(standing);
  const note = membershipNote(standing);

  async function press() {
    if (!pressable(join)) return;
    setReport(null);
    // QUIET: a join that landed takes the reader to the room, which is its
    // confirmation; anything else is this row's own note (`start.ts`).
    const result = await join.run(
      { channel: channel.id },
      { done: "Joined the room", quiet: true },
    );
    if (result === null) return;
    const answer = reportFor("join", result);
    if (answer.go) {
      nav.openRoom(answer.go);
      return;
    }
    setReport(answer);
  }

  return (
    <li className="chat-browse-row">
      <a className="chat-browse-row__main" href={nav.hrefOf({ kind: "room", id: channel.id })}>
        <span className="chat-browse-row__name">
          <NamedGlyph name={roomMark(channel.kind)} size="sm" />
          {channel.name || channel.id}
          {member && (
            <Tag size="xs" variant="success" dot>
              Joined
            </Tag>
          )}
          {archived && <Tag size="xs">Archived</Tag>}
        </span>
        {channel.topic && <span className="chat-browse-row__topic">{channel.topic}</span>}
        <span className="chat-browse-row__meta">
          {plural(room.members, "member")}
          {room.last_at ? ` · last message ${relTime(room.last_at, now)}` : ""}
          {note ? ` · ${note}` : ""}
        </span>
      </a>
      {joinable ? (
        <WriteButton
          size="small"
          variant="secondary"
          write={join}
          showRefusal={false}
          onPress={() => void press()}
        >
          {join.busy ? "Joining" : "Join"}
        </WriteButton>
      ) : (
        <ButtonLink
          size="small"
          variant="ghost"
          href={nav.hrefOf({ kind: "room", id: channel.id })}
        >
          Open
        </ButtonLink>
      )}
      {report && (
        <div className="chat-browse-row__note">
          <StartNote report={report} />
        </div>
      )}
    </li>
  );
}
