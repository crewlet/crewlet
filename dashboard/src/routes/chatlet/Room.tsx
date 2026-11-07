/**
 * One room: what was said in it, and the box to say something.
 *
 * Its name and its actions are the top bar's (`Chat.tsx`), its thread and
 * details are panes beside it, and its detail is read once by the frame for
 * all of them (`nav.tsx`'s `useOpenRoom`). What is here is the conversation:
 * the transcript, and at its foot the composer, or a bar saying why the
 * reader cannot post with what they can do about it.
 *
 * # A live frame is a reason to re-read, never something to render
 *
 * The socket announces every committed record in the rooms this reader may
 * see, and the frame deliberately carries no body. So the transcript is the
 * one rendering path for a conversation: a frame makes the newest page
 * re-read, and the page is what lands on screen. Drawing the frame's excerpt
 * as a message would be a second rendering path that disagrees with the
 * transcript the first time an edit races a reload.
 *
 * AND WHAT THE NEWEST PAGE DOES NOT REACH IS READ IN ITS OWN RANGE. A reader
 * who has paged back holds rows no newest page carries, and an edit, a delete
 * or a reaction there (or an erase or a prune, which may take any of them)
 * has to reach the screen as surely as one at the top. So once the newest
 * page has landed, a touched row below it is read again in the page that ends
 * at it (`rangesToReread`), and only that.
 *
 * # A write is read back, never drawn ahead
 *
 * Every change lands on the chat log and raises this tab's read floor for it
 * (`protocol/session.ts`), so the room is asked again at that floor and
 * redrawn from an answer that includes the change. Nothing here patches a
 * row or refetches by hand.
 *
 * # Its stylesheet arrives with it
 *
 * The room's rules are `styles/chatlet-room.css`, imported here rather than by
 * `main.tsx`, so they ship beside Chatlet's chunk and a reader who never opens
 * a room never downloads them.
 */

import "~/styles/chatlet-room.css";
import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";

import { DraftBoxes, DraftBoxesContext, boxesDraft, useOpenBoxes } from "~/app/assistant/boxes.ts";
import { useAssistantDraft } from "~/app/assistant/draft.ts";
import { usePageCoverage, usePageInView } from "~/app/Shell.tsx";
import { QueryState } from "~/components/common.tsx";
import { RefusalNote, WriteButton, pressable } from "~/components/WriteButton.tsx";
import { ActivityLine, Button, Callout, EmptyState } from "@crewlethq/ui";
import { CompassGlyph, StarGlyph } from "@crewlethq/icons/glyphs";
import { useChatRead } from "~/lib/chatLive.ts";
import { useStableList } from "~/lib/useStableList.ts";
import { useClient, useConnection } from "~/lib/store-hooks.ts";
import { useAct } from "~/lib/useAct.ts";
import { useQuery, withFloor } from "~/lib/useQuery.ts";
import type { QueryErrorCode } from "~/contract/errors.ts";
import { queryErrorCode, type ChatMessageView, type ChatMessagesAnswer } from "~/protocol/index.ts";

import { useArrivals } from "./arrivals.ts";
import { Composer } from "./Composer.tsx";
import { packPosition } from "./cursor.ts";
import { canJoin, membershipNote } from "./directory.ts";
import { typingLine, useChatFocus, useChatFrames, useChatPresence, workingAgents } from "./live.ts";
import {
  answersFor,
  EMPTY_TAIL,
  JUMP_PAGE,
  mergeNewest,
  mergeOlder,
  mergeRange,
  rangesToReread,
  touchesOf,
  type Reread,
  type Tail,
  type Touch,
} from "./messages.ts";
import { MISSING_ROOM_TITLE, roomMissing, useChatFrame, useChatNav, useOpenRoom } from "./nav.tsx";
import { usePendingRows } from "./outbox.ts";
import { coverageOf, PAGE } from "./page.ts";
import { useChatNames } from "./people.ts";
import { ReadFailed, ReadPending } from "./ReadState.tsx";
import { ARCHIVED, RoomScopeProvider, useRoomScope } from "./scope.tsx";
import { useRoomStar } from "./star.ts";
import { reportFor, type StartReport } from "./start.ts";
import { StartNote } from "./StartNote.tsx";
import { Transcript, type EntryUnread, type TranscriptHandle } from "./Transcript.tsx";

const NO_PARTICIPANTS: readonly string[] = [];

export function Room({ channelID }: { channelID: string }) {
  // A ROOM THE ENGINE SAYS IS NOT THERE is one sentence with a way on, never
  // a conversation drawn around a refusal said twice and a box that cannot post.
  if (roomMissing(useOpenRoom())) return <MissingRoom />;
  return (
    <RoomScopeProvider channelID={channelID}>
      <RoomFrame>
        <Conversation channelID={channelID} />
      </RoomFrame>
    </RoomScopeProvider>
  );
}

function Conversation({ channelID }: { channelID: string }) {
  const { socket } = useClient();
  const { connected } = useConnection();
  const frame = useChatFrame();
  const open = useOpenRoom();
  const nav = useChatNav();
  const scope = useRoomScope();
  // THE CHART'S NAMES, which do not move with the roster: read live, every
  // agent's push redrew the room and its composer.
  const people = useChatNames();
  const { cursors, outbox } = frame;

  const newest = useQuery("chat_messages", { channel_id: channelID, limit: PAGE });
  // THE STATE BAR DESCRIBES THE TRANSCRIPT, the answer this room is drawn
  // from: a node behind the log is a fact about the messages on screen.
  usePageCoverage(coverageOf(newest.data));
  const [tail, setTail] = useState(EMPTY_TAIL);
  const [paging, setPaging] = useState(false);
  const [pageError, setPageError] = useState<QueryErrorCode | null>(null);

  // WHAT FRAMES HAVE TOUCHED that the next newest page must be checked
  // against, each with where it sits on the log (`answersFor`).
  const touched = useRef<Touch[]>([]);
  // THE TAIL AS LAST DRAWN, for the effects and the jumps to read: the state
  // itself is only ever moved functionally.
  const drawn = useRef(tail);
  drawn.current = tail;
  const mounted = useRef(false);
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);

  /** One page of the room at this tab's read floor, as every ask is (`withFloor`). */
  const page = useCallback(
    async (cursor: string, limit: number) =>
      (await socket.query(
        "chat_messages",
        withFloor("chat_messages", JSON.stringify({ channel_id: channelID, cursor, limit })),
      )) as ChatMessagesAnswer,
    [socket, channelID],
  );

  /** Read each range again, ONE AT A TIME: the transcript, the rail and the
   *  panes share the socket's four in-flight queries with them. */
  const reread = useCallback(
    async (ranges: readonly Reread[]) => {
      for (const range of ranges) {
        let answer: ChatMessagesAnswer;
        try {
          answer = await page(range.cursor, PAGE);
        } catch {
          // A RANGE THAT COULD NOT BE READ AGAIN keeps what it last said and
          // is asked for again with the next newest page, rather than never.
          for (const message of range.messages) touched.current.push({ position: null, message });
          continue;
        }
        if (!mounted.current) return;
        setTail((held) => mergeRange(held, range.cursor, answer));
      }
    },
    [page],
  );

  useEffect(() => {
    const answer = newest.data;
    if (!answer) return;
    setTail((held) => mergeNewest(held, answer));
    const due = touched.current.filter((touch) => answersFor(answer.position, touch));
    if (due.length === 0) return;
    touched.current = touched.current.filter((touch) => !due.includes(touch));
    const ranges = rangesToReread(mergeNewest(drawn.current, answer), answer, due, PAGE);
    if (ranges.length > 0) void reread(ranges);
  }, [newest.data, reread]);

  // EVERY FRAME IN THIS ROOM IS A RE-READ OF THE NEWEST PAGE, coalesced. The
  // room's own detail is the frame's to read again (`nav.tsx`).
  useChatFrames(channelID, (frames) => {
    touched.current.push(...touchesOf(frames));
    newest.refetch();
  });

  // WHERE THIS TAB IS LOOKING: it joins this socket to the room's presence,
  // which puts this reader in the room on everybody else's screen.
  const typing = useChatFocus(channelID);
  const presence = useChatPresence(channelID);

  // LEAVING A ROOM WRITES THE CURSOR AT ONCE rather than waiting out the
  // interval, which is one of the moments the next flush might never happen.
  useEffect(() => () => cursors.flushNow(), [cursors, channelID]);

  // A MESSAGE IS READ WHEN IT HAS BEEN ON SCREEN WHILE THE ROOM WAS IN VIEW:
  // the tab in front, and nothing standing over the page (`usePageInView`).
  // Marking rows read under a dialog or in a background tab is how a person
  // comes back to a cleared badge over messages they never saw.
  const inView = usePageInView();
  const showing = useRef(inView);
  showing.current = inView;
  const newestSeen = useRef<ChatMessageView | null>(null);
  const onSeen = useCallback(
    (view: ChatMessageView) => {
      newestSeen.current = view;
      if (showing.current) cursors.see(channelID, packPosition(view.position));
    },
    [cursors, channelID],
  );
  useEffect(() => {
    const view = newestSeen.current;
    if (inView && view) cursors.see(channelID, packPosition(view.position));
  }, [inView, cursors, channelID]);

  const loadOlder = useCallback(async () => {
    if (!tail.older || paging) return;
    setPaging(true);
    setPageError(null);
    try {
      const answer = await page(tail.older, PAGE);
      setTail((held) => mergeOlder(held, answer));
    } catch (err) {
      setPageError(queryErrorCode(err instanceof Error ? err.message : null) ?? "query_failed");
    } finally {
      setPaging(false);
    }
  }, [page, tail.older, paging]);

  /**
   * Read back until `enough` holds or the room's start is reached: a jump to
   * a date, the very beginning, the New line or a linked message further
   * back than what is held. In the engine's largest page ([JUMP_PAGE]), since
   * the reader asked to go there.
   */
  const readBack = useCallback(
    async (enough: (held: Tail) => boolean): Promise<Tail> => {
      let held = drawn.current;
      if (!held.loaded || enough(held) || !held.older) return held;
      setPaging(true);
      setPageError(null);
      try {
        while (held.older && !enough(held) && mounted.current) {
          const answer = await page(held.older, JUMP_PAGE);
          held = mergeOlder(held, answer);
          setTail((now) => mergeOlder(now, answer));
        }
      } catch (err) {
        setPageError(queryErrorCode(err instanceof Error ? err.message : null) ?? "query_failed");
      } finally {
        if (mounted.current) setPaging(false);
      }
      return held;
    },
    [page],
  );

  // THE NEW LINE IS WHERE THE READER LEFT OFF WHEN THEY CAME IN, read once
  // the reader's state is known and kept for the visit. A room the reader is
  // not in has no such place: with no read mark it would draw the line over
  // every message in it.
  const read = useChatRead();
  const [since, setSince] = useState<{ at: number | null; unread: EntryUnread | null } | null>(
    null,
  );
  const member = open?.detail.data?.member;
  useEffect(() => {
    if (since !== null || member === undefined || read === null) return;
    const summary = open?.summary;
    setSince(
      member
        ? {
            at: read.cursors?.[channelID] ?? 0,
            unread: summary ? { count: summary.unread, capped: !!summary.unread_capped } : null,
          }
        : { at: null, unread: null },
    );
  }, [since, member, read, open?.summary, channelID]);

  const transcript = useRef<TranscriptHandle | null>(null);
  const channel = open?.channel;
  const title = open?.title ?? channelID;
  const where = open?.where ?? title;
  // THE TRANSCRIPT'S PROPS STAY THE SAME VALUES WHILE THEY SAY THE SAME THING,
  // so a render of the room (a presence change, the reach of a draft) does
  // not redraw the transcript, which is memoized on them. The room's own
  // posts only: a reply's row is drawn and settled by its thread pane.
  const pending = usePendingRows(outbox, channelID, "", tail.messages, scope.viewer);
  const lastMine = tail.messages.find(
    (view) =>
      view.message.author === scope.viewer && !view.message.deleted_at && !view.message.event,
  );
  useArrivals(channelID, tail.messages, tail.loaded, scope.viewer, people.nameOf);
  const working = useStableList(
    presence.working ?? [],
    (seat) => `${seat.handle} ${seat.thread ?? ""}`,
  );
  const onOlder = useCallback(() => void loadOlder(), [loadOlder]);
  const { markRead } = frame;
  const newestPosition = tail.messages[0]?.position ?? null;
  const onMarkRead = useCallback(
    () => markRead(channelID, newestPosition),
    [markRead, channelID, newestPosition],
  );
  const typingNow = (presence.typing ?? []).filter((handle) => handle !== scope.viewer);
  // THE SEATS ANSWERING IN THE ROOM ITSELF; one answering under a message is
  // drawn under it.
  const answering = working.filter((seat) => !seat.thread);
  const placeholder = `Message ${where}`;

  return (
    <section className="chat-room" aria-label={where}>
      {/* WHY THE ROOM IS NOT DESCRIBED, in the words every read's refusal is
          drawn in, only where nothing else describes it. */}
      {!channel && open?.detail.error && (
        <QueryState
          error={open.detail.error}
          refusal={open.detail.refusal}
          detail={open.detail.detail}
          loading={false}
        />
      )}
      {/* WHAT BECAME OF AN EDIT OR A REACTION that was refused, above the
          transcript the pressed row sits in: the engine's sentence names what
          was wrong, and a toast is gone before it is read. */}
      <RefusalNote write={scope.writes.edit} />
      <RefusalNote write={scope.writes.react} />
      {pageError && <QueryState error={pageError} refusal={null} loading={false} />}
      {tail.unreadable > 0 && (
        <Callout variant="warning">
          {tail.unreadable} message{tail.unreadable === 1 ? "" : "s"} on this page came from a newer
          version of the engine, so {tail.unreadable === 1 ? "it isn’t" : "they aren’t"} shown yet.
          Upgrading this node shows {tail.unreadable === 1 ? "it" : "them"}.
        </Callout>
      )}
      {/* THE TRANSCRIPT IS DRAWN FROM A TAIL THAT HAS BEEN READ, never from
          the query's flag: the page answers a commit before the tail merges
          it, and a transcript mounted over the empty tail measured an empty
          list, then painted a frame of spacer under its day pill. */}
      {!tail.loaded ? (
        newest.error ? (
          <ReadFailed query={newest} />
        ) : (
          <ReadPending label="Reading the room" rows={8} />
        )
      ) : (
        <Transcript
          ref={transcript}
          channel={channel}
          title={title}
          where={where}
          participants={open?.summary?.participants ?? NO_PARTICIPANTS}
          tail={tail}
          pending={pending}
          working={working}
          since={since?.at ?? null}
          sinceUnread={since?.unread ?? null}
          paging={paging}
          onOlder={onOlder}
          readBack={readBack}
          onSeen={onSeen}
          onMarkRead={onMarkRead}
          flash={nav.place.kind === "room" ? (nav.place.message ?? "") : ""}
          onLanded={nav.landed}
        />
      )}

      <footer className="chat-room-foot">
        {/*
          THE BOX IS OFFERED WHEREVER THE ENGINE TAKES THE POST, by the one
          rule the room reads (`scope.tsx`'s `roomRules`, the engine's write
          gate mirrored): an archived room, a private room or a direct
          conversation the reader is not in, and a kind this build cannot
          classify refuse it, and nothing else does. Somebody outside a
          public room or a team's room may say something there, and a team's
          lead answers a person nobody else is following; the bar above the
          box says where they stand and what they can do about it, and never
          stands in for the box.
        */}
        {scope.rules.post ? (
          <JoinBar channelID={channelID} where={where} />
        ) : (
          <>
            {channel && !scope.member && <JoinBar channelID={channelID} where={where} />}
            <Composer
              key={channelID}
              channelID={channelID}
              label={placeholder}
              placeholder={placeholder}
              onTyping={typing}
              onSent={() => transcript.current?.scrollToLatest()}
              {...(lastMine ? { onEditLast: () => scope.setEditing(lastMine.message.id) } : {})}
            />
          </>
        )}
        <ActivityLine
          typing={typingLine(typingNow.map(people.nameOf))}
          agents={workingAgents(answering, people.person)}
        />
        {/* WHAT OFFLINE MEANS FOR READING. What it means for saying something
            is the composer's own sentence, beside the Send it holds. */}
        {!connected && (
          <p className="t-caption muted">
            The live connection is down, so this room shows what it last read. It catches up when
            the connection is back.
          </p>
        )}
      </footer>
    </section>
  );
}

/**
 * A room the engine says is not there for this reader, in one sentence for
 * every reason `not_found` covers (removed, never made, a mistyped address, a
 * private room they are not in), with the two ways on: the rooms they may
 * open, and Activity.
 */
function MissingRoom() {
  const nav = useChatNav();
  return (
    <section className="chat-room" aria-label={MISSING_ROOM_TITLE}>
      <EmptyState
        icon={<CompassGlyph size="xl" />}
        title="This room doesn’t exist, or you can’t see it"
        description="It may have been removed or the link mistyped, or it’s a private room you’re not in."
        action={
          <span className="chat-missing-actions">
            <Button
              variant="primary"
              leadingIcon={<CompassGlyph size="sm" />}
              onClick={() => nav.go({ kind: "browse" })}
            >
              Browse rooms
            </Button>
            <Button variant="secondary" onClick={() => nav.go({ kind: "activity" })}>
              Back to Activity
            </Button>
          </span>
        }
      />
    </section>
  );
}

/**
 * Where the reader stands in the room. In place of the composer where the
 * engine takes no post from them: why, and what they can do about it (an
 * archived room is read for ever and offers the rooms that take messages; a
 * room not yet read, or one this build cannot classify, says that). Above the
 * composer where they may post and are not in the room: a room that takes a
 * join offers one, and says what joining adds, since posting does not need it.
 *
 * A TEAM'S ROOM SOMEBODY OUTSIDE THE TEAM IS READING offers a star in place of
 * the join it cannot take: its membership is the org chart's, and a starred
 * room the reader may read stays in their Rooms menu, which is how they come
 * back to it without Browse rooms.
 */
function JoinBar({ channelID, where }: { channelID: string; where: string }) {
  const scope = useRoomScope();
  const nav = useChatNav();
  const open = useOpenRoom();
  const star = useRoomStar(channelID);
  const join = useAct("join_channel");
  const [report, setReport] = useState<StartReport | null>(null);
  const channel = scope.channel;
  const standing = open?.detail.data
    ? {
        kind: open.detail.data.channel.kind,
        member: open.detail.data.member,
        unit: open.detail.data.channel.unit,
      }
    : null;

  const onJoin = async () => {
    if (!pressable(join)) return;
    setReport(null);
    // QUIET: what the press came to is the room's own note (`start.ts`),
    // rather than a toast saying "joined" over a room this node has not
    // applied the join to yet. A join that landed is confirmed by the room,
    // read again at the join's floor: the bar goes and the composer stays.
    const result = await join.run({ channel: channelID }, { done: "Joined the room", quiet: true });
    if (result === null) return;
    const answer = reportFor("join", result);
    if (answer.tone !== "success") setReport(answer);
  };

  let words: ReactNode;
  let actions: ReactNode = null;
  if (channel?.archived_at) {
    words = (
      <>
        <strong>{where}</strong> is archived. You can read it, but it takes no new messages.
      </>
    );
    actions = (
      <Button
        size="small"
        variant="secondary"
        leadingIcon={<CompassGlyph size="sm" />}
        onClick={() => nav.go({ kind: "browse" })}
      >
        Browse rooms
      </Button>
    );
  } else if (standing && !standing.member && !scope.rules.post && canJoin(standing)) {
    words = (
      <>
        You’re not in <strong>{where}</strong>. Join it to keep it in your Rooms menu.
      </>
    );
    actions = (
      <>
        <Button
          size="small"
          variant="ghost"
          onClick={() => nav.openPane({ kind: "details", room: channelID, tab: "about" })}
        >
          Details
        </Button>
        <WriteButton
          size="small"
          variant="primary"
          write={join}
          showRefusal={false}
          onPress={() => void onJoin()}
        >
          {join.busy ? "Joining" : "Join room"}
        </WriteButton>
      </>
    );
  } else if (standing && !standing.member && standing.kind === "unit" && !scope.rules.post) {
    words = (
      <>
        {membershipNote(standing)}{" "}
        {star.starred ? "It’s in your Starred rooms." : "Star it to keep it in your Rooms menu."}
      </>
    );
    actions = star.starred ? null : (
      <Button
        size="small"
        variant="secondary"
        leadingIcon={<StarGlyph size="sm" />}
        disabledReason={star.held || undefined}
        title={star.held || undefined}
        onClick={() => star.set(true)}
      >
        Add to Starred
      </Button>
    );
  } else {
    // WHY THERE IS NO BUTTON, said rather than left to an absence: a person
    // who cannot see the rule reads the missing control as a permission they
    // lack.
    words =
      scope.rules.post && scope.rules.post !== ARCHIVED
        ? scope.rules.post
        : standing
          ? membershipNote(standing) || scope.rules.post
          : scope.rules.post;
  }
  // A STANDING NOT YET READ IS NOT "OUTSIDE": until the room's detail
  // answers there is nothing to say, and an empty bar is a box saying nothing.
  if (!words && !actions) return null;
  return (
    <div className="chat-join-bar" role="status">
      <span className="chat-join-bar-words">{words}</span>
      {actions && <span className="chat-join-bar-actions">{actions}</span>}
      <StartNote report={report} />
      <RefusalNote write={join} />
    </div>
  );
}

/**
 * The room's outer frame: the ONE draft for the person's own assistant that
 * every box open in it is a field of (`app/assistant/boxes.ts`): the message
 * box, a thread's reply box and each message open for a rewrite.
 */
function RoomFrame({ children }: { children: ReactNode }) {
  const [boxes] = useState(() => new DraftBoxes());
  const where = useOpenRoom()?.where ?? "this room";
  return (
    <DraftBoxesContext.Provider value={boxes}>
      {children}
      <RoomDraft boxes={boxes} where={where} />
    </DraftBoxesContext.Provider>
  );
}

/**
 * The room's draft, declared from the boxes open on it. A child of its own,
 * so a keystroke in a box renders the box and this, never the transcript.
 */
function RoomDraft({ boxes, where }: { boxes: DraftBoxes; where: string }) {
  const open = useOpenBoxes(boxes);
  useAssistantDraft(
    boxesDraft(open, {
      scope: `What you are writing in ${where} (not sent)`,
      help:
        `Every box open in ${where} is a field: the message box, the reply box of the ` +
        "thread open beside it, and each of the person's own messages open for a " +
        "rewrite. You can propose text for any of them; sending, replying and saving " +
        "stay the person's.",
    }),
  );
  return null;
}
