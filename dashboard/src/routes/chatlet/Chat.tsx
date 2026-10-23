/**
 * Chatlet — the company's own conversation, for a person.
 *
 * # Who is asking is the server's answer, always
 *
 * Every question this screen asks is answered for the principal the SESSION
 * resolves to (ADR-0024), as the seat it holds: there is no handle parameter
 * anywhere in the chat family and no arm — an admin's included — that reads
 * somebody else's rooms. A credential acting as no seat gets NOTHING here,
 * reads included, which is stricter than every other personal surface in this
 * dashboard and deliberately so: a transcript is the most sensitive thing a
 * deployment holds, and a pipeline's credential is not a person.
 *
 * So a reader arrives in one of three shapes before there are any rooms: one
 * without the Chatlet app, a credential acting as no seat, and a person. The
 * FIRST IS THE FRAME'S: the chat row names its app (`app/nav.ts`), so a reader
 * the engine says holds none is drawn the one refusal every app's workspace
 * draws (`GrantRequired`) in place of every chat page — never a second,
 * weaker one here. The SECOND is this screen's, because no other surface has
 * it: a credential the engine resolves and the identity directory binds to no
 * seat — a deployment's own token, a service account. It is an ordinary
 * state, not a fault, and the remedy is one binding (`crewlet iam bind`), so
 * the screen says that from the one fact it holds independently — the viewer
 * answer with a login and no seat.
 *
 * A VIEWER READ THAT FAILED IS NO ANSWER and holds nothing: the screen asks,
 * and the engine's own answer or refusal says what is true. Waiting on it
 * would hold the screen until the viewer's next poll, minutes away, while
 * every other screen worked — which is the failure `lib/viewer.ts` names.
 *
 * # The rail is the whole navigation
 *
 * A room is a destination with its own address (`#/chatlet/{id}`), and the two
 * lists beside the rooms — the mention feed and search — are the same. The
 * thread pane is NOT: it is a `thread=` on the room you are already in, which
 * is what the information architecture calls a tab, because a thread is one
 * level deep and has no life away from its room.
 *
 * THE RAIL IS THE CHATLET ROW'S ONE SECTION RENDERER (`app/nav.ts`), drawn from
 * that row rather than beside it: the page header draws section tabs on a
 * section's own page and never on an object's, and a room is an object page —
 * the one a reader spends their time on — so a strip there would leave the
 * mention feed two presses from every room. The row says `renderer: "rail"`,
 * which the frame draws nothing for, and this rail draws every section the
 * row declares, under the names it declares and with the lock a closed one
 * carries, so the trail, the palette and the rail cannot come to disagree
 * about what chat's lists are called.
 *
 * # Nothing here decides who may see what
 *
 * Every room in the rail, every message in a transcript and every live frame
 * on the socket has already been through the engine's own visibility rule. A
 * client-side filter would be a second rule that could disagree with it, and
 * the disagreement would be silent in whichever direction it went.
 */

import { useEffect, useMemo, useRef, useState, type RefObject } from "react";

import { href, samePath, useIsCurrent, useNavigator, useParam } from "~/app/router.tsx";
import { grantsMissing, workspaceRow, type Section } from "~/app/nav.ts";
import { useFillScreen } from "~/app/fill.tsx";
import { usePageCoverage } from "~/app/Shell.tsx";
import { QueryState } from "~/components/common.tsx";
import { UnboundRemedy } from "~/components/people.tsx";
import { Button, Callout, Count, EmptyState, InlineCode, PageNote, Skeleton } from "@crewlethq/ui";
import {
  PlusGlyph,
  MessageSquareGlyph,
  CompassGlyph,
  KeyGlyph,
  UserPlusGlyph,
  UserGlyph,
} from "@crewlethq/icons/glyphs";
import { NamedGlyph } from "@crewlethq/icons/glyphs/registry";
import { useClient, useOrg } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { readerName, useHeldGrants, useViewer } from "~/lib/viewer.ts";
import { useFrameSession } from "~/lib/frameSession.ts";
import { accessNames } from "~/lib/access.ts";
import { useWriteAccess } from "~/lib/useWriteAccess.ts";
import { indexOrg } from "~/lib/seats.ts";
import { relTime } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import type { ChatChannelSummary, ChatChannelsAnswer } from "~/protocol/index.ts";

import { Browse } from "./Browse.tsx";
import { useReadCursors } from "./cursor.ts";
import { useChatActivity, useChatRead, useCoalesced } from "./live.ts";
import { Mentions } from "./Mentions.tsx";
import { coverageOf } from "./page.ts";
import { useOutbox } from "./outbox.ts";
import { peopleOptions } from "./people.ts";
import { Room } from "./Room.tsx";
import { Search } from "./Search.tsx";
import { StartDirect } from "./StartDirect.tsx";
import { StartRoom } from "./StartRoom.tsx";
import { badgesAreMeasured, isDirect, roomMark, roomTitle, unreadLabel } from "./rooms.ts";

/** Which of the three lists the main column is showing. */
export type ChatView = "room" | "mentions" | "search";

const CHATLET = workspaceRow("chatlet");

/**
 * The chat row's LANDING section, whose label heads the room list: the rooms
 * are what `#/chatlet` lists, so they sit under that section's name.
 */
const ROOMS = CHATLET?.sections.find((s) => samePath(s.path, CHATLET.path));

/** Every other section of the chat row — the lists that are not a room. */
const LISTS = CHATLET?.sections.filter((s) => !samePath(s.path, CHATLET.path)) ?? [];

/**
 * The gesture a person is in the middle of, if any.
 *
 * A DIALOG RATHER THAN AN ADDRESS, unlike every list on this screen. A room,
 * the mention feed and search are places somebody can be — each has a URL and
 * each is worth handing to a colleague — while making a room is a gesture that
 * is either finished or abandoned. `#/chatlet/new-room` would also collide with
 * the one segment under `chatlet` that carries an id: a room is addressed by the
 * uuid the engine minted for it, and a reserved word there is a room somebody
 * can never reach.
 */
type StartGesture = "" | "room" | "direct" | "browse";

/** Where the screen is: a room's id, or one of the two lists. */
interface ChatPlace {
  channel?: string;
  view?: ChatView;
}

/**
 * The chat screen, for the seat this tab reads as now.
 *
 * EVERYTHING BELOW IT IS ONE SEAT'S, and it is MOUNTED UNDER THAT SEAT
 * (`viewer.owner`) so a seat that moves remounts it rather than handing it on:
 * the read cursor writer, what a room has paged in, the messages still being
 * sent from every room with their Retry (the outbox, held here so it outlives
 * the room it was said in), a half-typed message, an open thread, a dialog
 * half filled in, a refusal drawn beside a control. Kept, each is the last
 * seat's under the next one's name, and the ones that write would write as the
 * next one — the engine resolves the seat when a press arrives, so a Retry
 * would post the last seat's words as the next.
 *
 * TWO THINGS MOVE THE SEAT, and only one is in this tab. The identity
 * directory binding the person elsewhere changes `owner` without a reload —
 * the engine closes the socket and the frame asks the viewer again — and that
 * is what this key follows. A different PERSON signing in never reaches here:
 * it hands the whole tab over by a reload with its storage emptied
 * (`lib/session.ts`), which is a remount of everything. Nothing else remounts
 * this: the viewer read keeps its last answer on screen while it asks again,
 * so the screen is never between readers.
 *
 * `seat` IS THE SEAT NOW, read by whatever outlives a render — the writer's
 * timer, the flush a remount's cleanup makes — and written here, in the render
 * that moves it, so the screen being taken down already knows it is no longer
 * the tab's.
 */
export function Chat(place: ChatPlace) {
  const viewer = useViewer();
  const seat = useRef(viewer.owner);
  seat.current = viewer.owner;
  return <ChatAs key={viewer.owner} {...place} seat={seat} />;
}

function ChatAs({ channel = "", view = "room", seat }: ChatPlace & { seat: RefObject<string> }) {
  const viewer = useViewer();
  const session = useFrameSession();
  // ONE CURSOR WRITER FOR THE SEAT, above the room, so switching rooms is a
  // flush rather than a new writer that has forgotten what the last one had
  // already written ([useReadCursors]).
  const cursors = useReadCursors(seat);
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
  // WHO A DIRECT MESSAGE ASKED FOR BY ADDRESS IS WITH (`#/chatlet?dm=<handle>`,
  // from a seat's page and peek): the New direct message opened ONCE with
  // that seat chosen, and the address left at once — the builder's `add=`
  // idiom — so a reload or Back does not open it again. Read once the chart
  // and the viewer have answered, since who may be named is the chart's and
  // the reader is never one of them; a handle nobody in the chart answers to
  // opens the dialog with nobody chosen rather than a name it cannot send.
  const [dm] = useParam("dm", "", "filter");
  const nav = useNavigator();
  const [directWith, setDirectWith] = useState<readonly string[]>([]);
  const create = useWriteAccess("create_channel");
  const direct = useWriteAccess("open_dm");
  // WHAT THIS PERSON IS STILL TRYING TO SAY, in every room — held here rather
  // than in the room, which is remounted for each room opened: an `unknown`
  // row's same-id Retry has to survive a look at another room, or the person
  // who comes back and types it again says it twice (`outbox.ts`).
  const outbox = useOutbox();

  // THE RAIL IS THE VIEWER'S OWN ROOMS, which is narrower than what they may
  // read: a public room they have not joined is searchable and openable and
  // does not sit in their list. `enabled` keeps the question off the wire
  // while the FIRST viewer read is out — the frame waits that out for every
  // app's workspace, and the screen states the same precondition rather than
  // inheriting it — and for a credential the engine has SAID acts as no
  // seat, which it would only refuse. `unbound` is an answer by construction:
  // a login with no seat exists only in a viewer that came back.
  //
  // A company whose chat is somebody else's, or nobody's, is served no chat
  // question, and never sees this screen once the engine has said so: the
  // frame draws where its chat is in its place (`app/ServiceNotice.tsx`), as
  // it does for Worklet on a company running Jira.
  useEffect(() => {
    if (dm === "" || viewer.asking || org === null) return;
    nav.filter({ dm: null });
    // NO DIALOG WHERE NOTHING COULD BE SAID: a credential acting as no seat
    // gets this screen's own sentence instead.
    if (viewer.unbound) return;
    setDirectWith(people.some((person) => person.value === dm) ? [dm] : []);
    setGesture("direct");
  }, [dm, viewer.asking, viewer.unbound, org, people, nav]);
  const rail = useQuery("chat_channels", {}, { enabled: !viewer.asking && !viewer.unbound });

  // THE RAIL SEEDS THIS TAB'S READ STATE, which nothing else would until a
  // flush landed somewhere: the live channel pushes it only when it moves.
  // Through the store's own ordering, so a push that arrived while this answer
  // was in flight — newer by its revision — is kept over it.
  const { store } = useClient();
  useEffect(() => store.applyChatRead(rail.data?.read), [store, rail.data]);

  // A LIVE FRAME IN ANY ROOM MOVES THIS LIST — a preview, an unread count, the
  // order itself — so the rail re-reads on what the socket has seen rather
  // than on a timer. Coalesced, because a burst in one room is one change to
  // this list.
  useCoalesced(useChatActivity(), rail.refetch);

  // AND SO DOES THIS PERSON'S OWN READ STATE: every unread count and every
  // mute in the rail is answered from it. This tab's own flush or mute moves
  // it through the write's answer (`markRead`), and every tab's — this one's
  // included — through the `chat_cursor_moved` push; the revision makes those
  // one move, so the rail re-reads once for it. KEYED ON THE RECORD'S
  // REVISION, never on the slice: `applyChatRead` re-emits a state of the
  // revision it holds, and the effect above re-applies the rail's own answer,
  // so a key on the slice would re-read the rail on every answer of its own.
  // A revision the rail's answer already carries is one it has drawn — which
  // is what keeps the seed above from costing every mount a second read.
  const readRevision = useChatRead()?.revision ?? 0;
  const drawnRevision = rail.data?.read?.revision ?? 0;
  useCoalesced(readRevision, () => {
    if (readRevision > drawnRevision) rail.refetch();
  });

  if (viewer.asking) {
    return <Skeleton variant="text" rows={6} label="Working out who you are" />;
  }
  if (viewer.unbound) {
    return (
      <EmptyState
        icon={<UserGlyph size="xl" />}
        title="Chatlet is read as the seat you hold"
        description={
          <>
            You&rsquo;re signed in as <InlineCode>{viewer.login}</InlineCode>, which holds no seat.
            Chatlet needs a seat, so no room is open to you.{" "}
            <UnboundRemedy to="read and write here" />
          </>
        }
      />
    );
  }

  const channels = rail.data?.channels ?? [];
  const measured = badgesAreMeasured(rail.data);

  return (
    <>
      <PageNote>
        Your company&rsquo;s own rooms and conversations.
        {/* ONLY A SEAT THE VIEWER NAMED: after a viewer read that failed
            nobody has said who this is, and the rooms below are the engine's
            answer about the session rather than a name this screen knows.
            THE READER AND THE SEAT ARE TWO NAMES (`readerName`): the seat's
            is the chart's name for a place, never what the person is
            called. */}
        {viewer.handle && (
          <>
            {" "}
            You&rsquo;re <strong>{readerName(session.answer, viewer.login)}</strong>, reading as the
            seat <strong>{viewer.name || viewer.handle}</strong>.
          </>
        )}
      </PageNote>

      {rail.data?.truncated && (
        <Callout variant="warning">
          You&rsquo;re in more rooms than this list holds ({channels.length}). The rest still open
          from a link and still show up in search.
        </Callout>
      )}
      {rail.data && !measured && (
        <Callout variant="warning">
          Unread counts couldn&rsquo;t be read, so the badges are missing. That doesn&rsquo;t mean
          you&rsquo;re caught up.
        </Callout>
      )}

      <div className="chat-frame">
        <nav className="chat-rail" aria-label="Your rooms">
          {/* THE HALF A PERSON DOES FIRST. Everything else on this screen
              reads a conversation that already exists; these three start one,
              and they sit above the lists because a reader with an empty rail
              has nothing below them to look at. The two that open a CHANGE
              are held as the change would be — by the browser's reason for
              the tool their dialog presses — and drawn for everybody, as
              every screen draws the button that opens a change. Finding a
              room is a read, and opens for anybody. */}
          <div className="row wrap gap-1">
            <Button
              variant="ghost"
              size="small"
              leadingIcon={<PlusGlyph size="sm" />}
              disabledReason={create.can ? undefined : create.reason}
              title={create.can ? undefined : create.reason}
              onClick={() => setGesture("room")}
            >
              New room
            </Button>
            <Button
              variant="ghost"
              size="small"
              leadingIcon={<UserPlusGlyph size="sm" />}
              disabledReason={direct.can ? undefined : direct.reason}
              title={direct.can ? undefined : direct.reason}
              onClick={() => setGesture("direct")}
            >
              Message
            </Button>
            <Button
              variant="ghost"
              size="small"
              leadingIcon={<CompassGlyph size="sm" />}
              onClick={() => setGesture("browse")}
            >
              Find a room
            </Button>
          </div>
          {LISTS.map((section) => (
            <ChatRailLink key={section.key} section={section} />
          ))}
          <div className="chat-rail-label t-label">{ROOMS?.label}</div>
          <QueryState
            error={rail.error}
            refusal={rail.refusal}
            detail={rail.detail}
            loading={rail.loading}
            empty={
              channels.length
                ? undefined
                : {
                    title: "You’re in no rooms yet",
                    hint: "Give a team a channel in the org chart and its room appears here. Anyone can also message you directly.",
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
              outbox={outbox}
            />
          ) : (
            <Landing rail={rail.data} />
          )}
        </div>
      </div>

      {/* THE RAIL IS ASKED AGAIN WHEN ONE OF THESE WRITES, at the write's own
          floor (`protocol/session.ts`) rather than whenever the frame the
          record's applier raises arrives: a person who has just made a room is
          looking at the rail now, and a read at the floor is one that already
          has it. */}
      {gesture === "room" && (
        <StartRoom
          people={people}
          defaultPrivate={rail.data?.default_private ?? false}
          onClose={() => setGesture("")}
        />
      )}
      {gesture === "direct" && (
        <StartDirect
          people={people}
          initial={directWith}
          onClose={() => {
            setGesture("");
            setDirectWith([]);
          }}
        />
      )}
      {gesture === "browse" && <Browse onClose={() => setGesture("")} />}
    </>
  );
}

/**
 * The bare `#/chatlet`: the rail, and nothing picked.
 *
 * THE ONE VIEW DRAWN FROM THE RAIL'S ANSWER, so it is the one that publishes
 * the rail's coverage. Every other view publishes its own — a room its
 * transcript's, the mention feed its feed's, search none (it reads this
 * node's own index, as the work search does) — because the state bar
 * describes the answer the reader is looking at, and a frame holds one.
 * Published from a child for the same reason: a parent's effect runs after
 * its child's, so the screen publishing the rail's would overwrite the room's.
 */
function Landing({ rail }: { rail: ChatChannelsAnswer | null }) {
  usePageCoverage(coverageOf(rail));
  return (
    <EmptyState
      size="compact"
      icon={<MessageSquareGlyph size="xl" />}
      title="Pick a room"
      description="Rooms are on the left, newest activity first. Your mentions are the one list that spans all of them."
    />
  );
}

/**
 * One section of the chat row that is not a room, drawn as the row declares
 * it. CURRENT BY THE ADDRESS, the rule the header's tabs use, rather than by
 * which list the screen happens to be showing.
 */
function ChatRailLink({ section }: { section: Section }) {
  const current = useIsCurrent(section.path);
  // THE LOCK THE SIDEBAR DRAWS for a section the reader cannot open, by the
  // one rule (`grantsMissing`), and for the same reason: the row stays, and
  // says what it needs before a press rather than as the page a press lands
  // on. Every chat section is open to whoever this screen is drawn for — the
  // frame refuses the whole workspace first — so this draws for a section
  // the table someday gives a grant of its own, and is the one rule rather
  // than a special case of it.
  const held = useHeldGrants();
  const needs = held === null ? [] : grantsMissing(section, held);
  return (
    <a
      className={`chat-rail-row${current ? " selected" : ""}`}
      href={href(section.path)}
      aria-current={current ? "page" : undefined}
      title={needs.length > 0 ? `Needs ${accessNames(needs)} access` : undefined}
    >
      <NamedGlyph name={section.icon} size="sm" />
      <span className="chat-rail-title truncate">{section.label}</span>
      {needs.length > 0 && (
        <>
          <KeyGlyph size="xs" aria-hidden="true" />
          <span className="sr-only">, needs {accessNames(needs)} access</span>
        </>
      )}
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
      href={href(["chatlet", channel.id])}
      aria-current={current ? "page" : undefined}
    >
      <NamedGlyph name={roomMark(channel.kind)} size="sm" />
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
