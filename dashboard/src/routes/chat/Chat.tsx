/**
 * Chat — the company's own conversation, for a person.
 *
 * # Who is asking is the server's answer, always
 *
 * Every question this screen asks resolves the reader from their own
 * credential: there is no handle parameter anywhere in the chat family and no
 * operator arm that reads somebody else's rooms. A token bound to no seat gets
 * NOTHING here, reads included, which is stricter than every other personal
 * surface in this dashboard and deliberately so — a transcript is the most
 * sensitive thing a deployment holds, and a pipeline's credential is not a
 * person.
 *
 * So the screen has three shapes before it has any rooms: anonymous (no token
 * at all), unbound (a token no seat names), and a reader. The middle one is
 * the one worth writing carefully — it is an ordinary state, not a fault, and
 * the remedy is one line of company configuration. The engine refuses with
 * that sentence, but the socket reduces a refusal to its CODE, so the screen
 * says it from the one fact it holds independently: the viewer answer with an
 * operator id and no handle.
 *
 * # The rail is the whole navigation
 *
 * A room is a destination with its own address (`#/chat/{id}`), and the two
 * lists beside the rooms — the mention feed and search — are the same. The
 * thread pane is NOT: it is a `thread=` on the room you are already in, which
 * is what the information architecture calls a tab, because a thread is one
 * level deep and has no life away from its room.
 *
 * # Nothing here decides who may see what
 *
 * Every room in the rail, every message in a transcript and every live frame
 * on the socket has already been through the engine's own visibility rule. A
 * client-side filter would be a second rule that could disagree with it, and
 * the disagreement would be silent in whichever direction it went.
 */

import { useMemo, useState } from "react";

import { href } from "~/app/router.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { useFillScreen } from "~/app/fill.tsx";
import { usePageCoverage } from "~/app/Shell.tsx";
import { QueryState } from "~/components/common.tsx";
import { Button, Callout, Count, EmptyState, Skeleton } from "@crewlethq/ui";
import {
  AddGlyph,
  ChatGlyph,
  ExploreGlyph,
  NotificationsGlyph,
  PersonAddGlyph,
  PersonGlyph,
  SearchGlyph,
} from "@crewlethq/icons/glyphs";
import { Mark } from "~/ui/glyph.tsx";
import { useConnection, useOrg } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import { indexOrg } from "~/lib/seats.ts";
import { relTime } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import type { ChatChannelSummary } from "~/protocol/index.ts";

import { Browse } from "./Browse.tsx";
import { useReadCursors } from "./cursor.ts";
import { DEGRADED_POLL_MS, useChatActivity, useCoalesced } from "./live.ts";
import { Mentions } from "./Mentions.tsx";
import { peopleOptions } from "./people.ts";
import { Room } from "./Room.tsx";
import { Search } from "./Search.tsx";
import { StartDirect } from "./StartDirect.tsx";
import { StartRoom } from "./StartRoom.tsx";
import { badgesAreMeasured, isDirect, roomMark, roomTitle, unreadLabel } from "./rooms.ts";

/** Which of the three lists the main column is showing. */
export type ChatView = "room" | "mentions" | "search";

/**
 * The gesture a person is in the middle of, if any.
 *
 * A DIALOG RATHER THAN AN ADDRESS, unlike every list on this screen. A room,
 * the mention feed and search are places somebody can be — each has a URL and
 * each is worth handing to a colleague — while making a room is a gesture that
 * is either finished or abandoned. `#/chat/new-room` would also collide with
 * the one segment under `chat` that carries an id: a room is addressed by the
 * uuid the engine minted for it, and a reserved word there is a room somebody
 * can never reach.
 */
type StartGesture = "" | "room" | "direct" | "browse";

export function Chat({ channel = "", view = "room" }: { channel?: string; view?: ChatView }) {
  const viewer = useViewer();
  const { connected } = useConnection();
  // THE SCREEN TAKES THE WINDOW'S HEIGHT rather than growing the page. A
  // transcript inside a page that also scrolls is two scrollers under one
  // wheel, and which one moves is whichever the pointer happens to be over —
  // so the reader loses their place in a conversation by looking at it.
  useFillScreen(true);
  const org = useOrg();
  const now = useNow();
  const index = useMemo(() => indexOrg(org), [org]);
  const nameOf = useMemo(
    () => (handle: string) => index.byHandle.get(handle)?.name ?? handle,
    [index],
  );
  // EVERYBODY THE COMPANY HAS, for the two gestures that name people. Built
  // once here rather than inside each dialog: the roster is the same roster,
  // and two derivations of it would order the same colleague two ways.
  const people = useMemo(() => peopleOptions(index.seats, viewer.handle), [index, viewer.handle]);
  const [gesture, setGesture] = useState<StartGesture>("");

  // ONE CURSOR WRITER FOR THE TAB, above the room, so switching rooms is a
  // flush rather than a new writer that has forgotten what the last one had
  // already written.
  const cursors = useReadCursors();

  // THE RAIL IS THE VIEWER'S OWN ROOMS, which is narrower than what they may
  // read: a public room they have not joined is searchable and openable and
  // does not sit in their list. `enabled` keeps the question off the wire for
  // a caller the engine would refuse anyway.
  const rail = useQuery(
    "chat_channels",
    {},
    {
      enabled: !viewer.loading && !viewer.anonymous && !viewer.unbound,
      pollMs: connected ? undefined : DEGRADED_POLL_MS,
    },
  );

  // A LIVE FRAME IN ANY ROOM MOVES THIS LIST — a preview, an unread count, the
  // order itself — so the rail re-reads on what the socket has seen rather
  // than on a timer. Coalesced, because a burst in one room is one change to
  // this list.
  useCoalesced(useChatActivity(), rail.refetch);

  usePageCoverage(
    rail.data
      ? {
          read_level: rail.data.read_level,
          complete: rail.data.complete,
          applied_through: rail.data.position?.seq,
        }
      : null,
  );

  if (viewer.loading) {
    return <Skeleton variant="text" rows={6} label="Working out who you are" />;
  }
  if (viewer.anonymous) {
    return (
      <EmptyState
        icon={<ChatGlyph size="xl" />}
        title="Chat needs a credential"
        description="The company's conversation is read as a person, so this screen has no anonymous form at all — not even for a public room. Present an operator token and this becomes the rooms the seat it is bound to is in."
      />
    );
  }
  if (viewer.unbound) {
    return (
      <EmptyState
        icon={<PersonGlyph size="xl" />}
        title="This credential is not bound to a seat"
        description="Chat is read as a person: the server resolves your token to a `kind: human` seat on every call, and a token no seat names reads nothing here — a pipeline's credential is not a person. Give a human seat a contact.crewlet_operator_id matching this token's id in the company configuration, and this screen becomes their rooms."
      />
    );
  }

  const channels = rail.data?.channels ?? [];
  const measured = badgesAreMeasured(rail.data);

  return (
    <>
      <PageNote>
        The company's own chat: every message is a record on the shared log, and every node derives
        the same rooms from it. You are reading as <strong>{viewer.name || viewer.handle}</strong>.
      </PageNote>

      {rail.data?.truncated && (
        <Callout variant="warning">
          You are in more rooms than this list holds ({channels.length}). The rest are still
          readable by address and still searchable — the list is bounded because it is sorted by
          activity, which cannot be paged without re-sorting each page against itself.
        </Callout>
      )}
      {rail.data && !measured && (
        <Callout variant="warning">
          The unread counts could not be read. Where you have got to in each room lives in the
          coordination store rather than on the chat log, and this node could not reach it — so the
          badges below are absent rather than zero, which is not the same as being caught up.
        </Callout>
      )}

      <div className="chat-frame">
        <nav className="chat-rail" aria-label="Your rooms">
          {/* THE HALF A PERSON DOES FIRST. Everything else on this screen
              reads a conversation that already exists; these three start one,
              and they sit above the lists because a reader with an empty rail
              has nothing below them to look at. */}
          <div className="row wrap gap-1">
            <Button
              variant="tertiary"
              size="small"
              leadingIcon={<AddGlyph size="sm" />}
              onClick={() => setGesture("room")}
            >
              New room
            </Button>
            <Button
              variant="tertiary"
              size="small"
              leadingIcon={<PersonAddGlyph size="sm" />}
              onClick={() => setGesture("direct")}
            >
              Message
            </Button>
            <Button
              variant="tertiary"
              size="small"
              leadingIcon={<ExploreGlyph size="sm" />}
              onClick={() => setGesture("browse")}
            >
              Find a room
            </Button>
          </div>
          <ChatRailLink
            path={["chat", "mentions"]}
            current={view === "mentions"}
            icon={<NotificationsGlyph size="sm" />}
            label="Mentions"
          />
          <ChatRailLink
            path={["chat", "search"]}
            current={view === "search"}
            icon={<SearchGlyph size="sm" />}
            label="Search"
          />
          <div className="chat-rail-label t-label">Rooms</div>
          <QueryState
            error={rail.error}
            loading={rail.loading}
            empty={
              channels.length
                ? undefined
                : {
                    title: "You are in no rooms yet",
                    hint: "A unit that names a channel in the org chart gets that room created for it, with the unit's seats as members. Somebody can also open a direct conversation with you — which needs no invitation, because its identity is derived from who is in it.",
                  }
            }
          >
            {channels.map((room) => (
              <RailRoom
                key={room.channel.id}
                room={room}
                current={view === "room" && room.channel.id === channel}
                measured={measured}
                viewer={viewer.handle}
                nameOf={nameOf}
                now={now}
              />
            ))}
          </QueryState>
        </nav>

        <div className="chat-main">
          {view === "mentions" ? (
            <Mentions nameOf={nameOf} />
          ) : view === "search" ? (
            <Search channels={channels} viewer={viewer.handle} nameOf={nameOf} />
          ) : channel ? (
            <Room
              key={channel}
              channelID={channel}
              viewer={viewer.handle}
              nameOf={nameOf}
              summary={channels.find((room) => room.channel.id === channel)}
              cursors={cursors}
              onWrote={rail.refetch}
            />
          ) : (
            <EmptyState
              size="compact"
              icon={<ChatGlyph size="xl" />}
              title="Pick a room"
              description="Rooms are on the left, newest activity first. Your mentions are the one list that spans all of them."
            />
          )}
        </div>
      </div>

      {/* THE RAIL IS ASKED AGAIN WHEN ONE OF THESE WRITES, rather than left to
          the frame the record's own applier will raise: the frame is what
          makes this list correct eventually, and a person who has just made a
          room is looking at the rail now. */}
      {gesture === "room" && (
        <StartRoom people={people} onClose={() => setGesture("")} onWrote={rail.refetch} />
      )}
      {gesture === "direct" && (
        <StartDirect people={people} onClose={() => setGesture("")} onWrote={rail.refetch} />
      )}
      {gesture === "browse" && <Browse onClose={() => setGesture("")} onWrote={rail.refetch} />}
    </>
  );
}

/** One of the two lists that are not a room. */
function ChatRailLink({
  path,
  current,
  icon,
  label,
}: {
  path: string[];
  current: boolean;
  icon: React.ReactNode;
  label: string;
}) {
  return (
    <a
      className={`chat-rail-row${current ? " selected" : ""}`}
      href={href(path)}
      aria-current={current ? "page" : undefined}
    >
      {icon}
      <span className="chat-rail-title truncate">{label}</span>
    </a>
  );
}

/** One room in the rail. */
function RailRoom({
  room,
  current,
  measured,
  viewer,
  nameOf,
  now,
}: {
  room: ChatChannelSummary;
  current: boolean;
  measured: boolean;
  viewer: string;
  nameOf: (handle: string) => string;
  now: number;
}) {
  const { channel } = room;
  const title = roomTitle(channel, { participants: room.participants, viewer, nameOf });
  // THE BADGE IS ONLY DRAWN WHERE IT MEANS SOMETHING. With no read state the
  // engine zeroes every count and says so; drawing that as a silent row is
  // right, and drawing it as "nothing unread" in words would not be.
  const badge = measured ? unreadLabel(room.unread, room.unread_capped) : "";
  return (
    <a
      className={`chat-rail-row${current ? " selected" : ""}`}
      href={href(["chat", channel.id])}
      aria-current={current ? "page" : undefined}
    >
      <Mark name={roomMark(channel.kind)} size="sm" />
      <span className="col" style={{ gap: 0, flex: 1, minWidth: 0 }}>
        <span className="chat-rail-title truncate">
          {isDirect(channel.kind) ? title : `#${title}`}
        </span>
        {room.last && (
          <span className="chat-rail-preview truncate t-caption">
            {room.last.author ? `${nameOf(room.last.author)}: ` : ""}
            {room.last.deleted ? "(deleted)" : room.last.excerpt || "…"}
          </span>
        )}
        <span className="row gap-2 t-caption muted">
          {room.last && <span>{relTime(room.last.at, now)}</span>}
          {room.muted && <span>muted</span>}
          {room.follow_all && <span>following</span>}
        </span>
      </span>
      {badge && (
        <span className={room.muted ? "muted" : undefined}>
          <Count value={badge} label={`unread in ${title}`} />
        </span>
      )}
    </a>
  );
}
