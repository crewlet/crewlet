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
 *
 * # A room is a row of the kit's grid
 *
 * Its facts were one prose line under a hand-built sort menu; as columns they
 * sort from their heads (the most recent activity first), give way in order at
 * a narrow width, and say which went. The row is one link to the room, Join or
 * Open stands at its end as a control of its own, and what the reader's
 * standing means (a team's room, a private one) or what a refused join came to
 * is a line under the row. Search keeps the list row (`BrowseRow`), where a
 * room is one result among messages.
 */

import { useMemo, useState } from "react";

import {
  Button,
  ButtonLink,
  Callout,
  EmptyState,
  EmptyValue,
  IconButton,
  Input,
  RoomRow,
  SegmentedControl,
  TableDateCell,
  TableNumberCell,
  Tag,
} from "@crewlethq/ui";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { CompassGlyph, PlusGlyph, SearchGlyph } from "@crewlethq/icons/glyphs";
import { NamedGlyph } from "@crewlethq/icons/glyphs/registry";

import { usePageCoverage } from "~/app/Shell.tsx";
import { QueryState } from "~/components/common.tsx";
import { WriteButton, pressable } from "~/components/WriteButton.tsx";
import { useNow } from "~/lib/clock.ts";
import { fmtCount, plural, relTime } from "~/lib/format.ts";
import { useAct } from "~/lib/useAct.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useWriteAccess } from "~/lib/useWriteAccess.ts";
import type { ChatDirectoryRoom } from "~/protocol/index.ts";

import { canJoin, membershipNote } from "./directory.ts";
import { useChatNav } from "./nav.tsx";
import { coverageOf } from "./page.ts";
import { roomMark } from "./rooms.ts";
import { reportFor, type StartReport } from "./start.ts";
import { StartNote } from "./StartNote.tsx";
import { ViewHead, useViewBarTight } from "./ViewHead.tsx";

type Kind = "all" | "public" | "unit" | "archived";

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

/** The rooms by name: the order a tie under any head keeps. */
export function byRoomName(rooms: readonly ChatDirectoryRoom[]): ChatDirectoryRoom[] {
  const name = (room: ChatDirectoryRoom) => room.channel.name ?? "";
  return [...rooms].sort((a, b) => name(a).localeCompare(name(b)));
}

export function BrowseView() {
  const nav = useChatNav();
  const create = useWriteAccess("create_channel");
  const tight = useViewBarTight();
  const answer = useQuery("chat_rooms", {});
  // THE DIRECTORY'S OWN COVERAGE, since it is the answer this view is drawn
  // from and the rail the frame also holds is not.
  usePageCoverage(coverageOf(answer.data));
  const [q, setQ] = useState("");
  const [kind, setKind] = useState<Kind>("all");
  const now = useNow();
  // BY NAME UNDER ANY HEAD: the grid's sort is stable, so a tie keeps this.
  const rooms = useMemo(
    () =>
      byRoomName(
        (answer.data?.rooms ?? []).filter((room) => keptByKind(kind, room) && matchesRoom(room, q)),
      ),
    [answer.data, kind, q],
  );
  // WHAT A PRESSED JOIN CAME TO, per room, said under its row.
  const [reports, setReports] = useState<ReadonlyMap<string, StartReport>>(new Map());
  const report = (id: string, r: StartReport | null) =>
    setReports((was) => {
      const next = new Map(was);
      if (r) next.set(id, r);
      else next.delete(id);
      return next;
    });

  return (
    <section className="chat-view">
      <ViewHead>
        {/* A TIGHT BAR DRAWS THE GLYPH ALONE, named and hinted by its words. */}
        {tight ? (
          <IconButton
            variant="primary"
            size="header"
            label="New room"
            icon={<PlusGlyph />}
            tooltip
            disabledReason={create.can ? undefined : create.reason}
            onClick={() => nav.openDialog({ kind: "new-room" })}
          />
        ) : (
          <Button
            size="medium"
            variant="primary"
            leadingIcon={<PlusGlyph size="sm" />}
            tooltip
            disabledReason={create.can ? undefined : create.reason}
            onClick={() => nav.openDialog({ kind: "new-room" })}
          >
            New room
          </Button>
        )}
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
            <DataGrid<ChatDirectoryRoom>
              aria-label="Rooms"
              rows={rooms}
              rowKey={(r) => r.channel.id}
              rowHref={(r) => nav.hrefOf({ kind: "room", id: r.channel.id })}
              defaultSort="-last"
              phoneRows="compact"
              rowAccessory={(r) => {
                const done = reports.get(r.channel.id);
                if (done) return <StartNote report={done} />;
                return roomNote(r) || null;
              }}
              columns={[
                {
                  key: "name",
                  header: "Room",
                  floor: "10rem",
                  phoneLead: true,
                  sortValue: (r) => r.channel.name ?? "",
                  cell: (r) => <RoomName room={r} />,
                },
                {
                  key: "topic",
                  header: "Topic",
                  overflow: "truncate",
                  drop: 2,
                  title: (r) => r.channel.topic,
                  cell: (r) =>
                    r.channel.topic ? (
                      <span>{r.channel.topic}</span>
                    ) : (
                      <EmptyValue label="No topic" />
                    ),
                },
                {
                  key: "members",
                  header: "Members",
                  align: "right",
                  shrink: true,
                  drop: 1,
                  sortValue: (r) => r.members,
                  cell: (r) => <TableNumberCell value={r.members} format={fmtCount} />,
                },
                {
                  // A ROOM NOBODY HAS SPOKEN IN sorts after every room somebody
                  // has, in both directions: it has no instant to sort by.
                  key: "last",
                  header: "Last message",
                  align: "right",
                  shrink: true,
                  sortValue: (r) => (r.last_at ? Date.parse(r.last_at) : null),
                  cell: (r) =>
                    r.last_at ? (
                      <TableDateCell value={r.last_at} now={now} />
                    ) : (
                      <EmptyValue label="Nobody has spoken here" />
                    ),
                },
                {
                  key: "action",
                  header: "",
                  label: "Join or open",
                  action: true,
                  shrink: true,
                  cell: (r) => (
                    <BrowseAction room={r} onReport={(done) => report(r.channel.id, done)} />
                  ),
                },
              ]}
            />
          )}
        </QueryState>
      </div>
    </section>
  );
}

/** A room's mark, its name, and Joined or Archived beside it. */
function RoomName({ room }: { room: ChatDirectoryRoom }) {
  const { channel } = room;
  return (
    <span className="row gap-1">
      <NamedGlyph name={roomMark(channel.kind)} size="sm" aria-hidden="true" />
      <span className="truncate" title={channel.name || channel.id}>
        {channel.name || channel.id}
      </span>
      {room.member && (
        <Tag size="xs" variant="success" dot>
          Joined
        </Tag>
      )}
      {channel.archived_at && <Tag size="xs">Archived</Tag>}
    </span>
  );
}

/** What the reader's standing in a room means, where it is not plain. */
function roomNote(room: ChatDirectoryRoom): string {
  const { channel } = room;
  return membershipNote({ kind: channel.kind, member: !!room.member, unit: channel.unit });
}

/**
 * Join, where the engine would take one, or Open. A join that lands takes the
 * reader into the room, which is its confirmation; anything else is reported
 * under the row (`start.ts`).
 */
function BrowseAction({
  room,
  onReport,
}: {
  room: ChatDirectoryRoom;
  onReport: (report: StartReport | null) => void;
}) {
  const nav = useChatNav();
  const join = useAct("join_channel");
  const { channel } = room;
  const archived = !!channel.archived_at;
  const joinable =
    !archived && canJoin({ kind: channel.kind, member: !!room.member, unit: channel.unit });
  const href = nav.hrefOf({ kind: "room", id: channel.id });

  async function press() {
    if (!pressable(join)) return;
    onReport(null);
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
    onReport(answer);
  }

  return joinable ? (
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
    <ButtonLink size="small" variant="ghost" href={href}>
      Open
    </ButtonLink>
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

  const href = nav.hrefOf({ kind: "room", id: channel.id });
  return (
    <RoomRow
      href={href}
      mark={<NamedGlyph name={roomMark(channel.kind)} size="sm" />}
      name={channel.name || channel.id}
      tags={
        member || archived ? (
          <>
            {member && (
              <Tag size="xs" variant="success" dot>
                Joined
              </Tag>
            )}
            {archived && <Tag size="xs">Archived</Tag>}
          </>
        ) : undefined
      }
      topic={channel.topic}
      facts={[
        plural(room.members, "member"),
        room.last_at ? `last message ${relTime(room.last_at, now)}` : "",
        note,
      ]}
      action={
        joinable ? (
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
          <ButtonLink size="small" variant="ghost" href={href}>
            Open
          </ButtonLink>
        )
      }
      note={report ? <StartNote report={report} /> : undefined}
    />
  );
}
