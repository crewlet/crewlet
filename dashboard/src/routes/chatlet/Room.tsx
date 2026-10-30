/**
 * One room: who is in it, what was said, who is here now, and the composer.
 *
 * # A live frame is a reason to re-read, never something to render
 *
 * The socket announces every committed record in the rooms this reader may
 * see, and the frame deliberately carries no body. So the transcript is the
 * one rendering path for a conversation: a frame makes the newest page
 * re-read, and the page is what lands on screen. The alternative — drawing
 * the frame's excerpt as a message — is a second rendering path that
 * disagrees with the transcript the first time an edit races a reload.
 *
 * AND WHAT THE NEWEST PAGE DOES NOT REACH IS READ IN ITS OWN RANGE. A reader
 * who has paged back holds rows no newest page carries, and an edit, a
 * delete or a reaction there — or an erase or a prune, which may take any of
 * them — has to reach the screen as surely as one at the top: kept as first
 * read, a message the company deleted stayed on screen for as long as the
 * room was open. So once the newest page has landed, a touched row below it
 * is read again in the page that ends at it (`rangesToReread`), and only
 * that: a frame touches one message, and the room is not read whole for it.
 *
 * # What this room refuses, and why it says so rather than greying out
 *
 * Three things close the composer, and they are different facts: the room is
 * archived (readable for ever, taking no new messages), the reader is not a
 * member of a private room, or this build cannot classify the room's kind at
 * all — which the engine treats as refusing every write, because a kind read
 * two-valued falls out as "not private". A fourth is about the BROWSER rather
 * than the room: every write here is a change made through the act transport
 * as the session's principal (ADR-0024), so offline, or as a credential
 * acting as no seat, nothing can be sent — and the sentence is
 * `useWriteAccess`'s for the tool a control presses, the same one every other
 * control in the dashboard is held with. A room's refusal closes the box; the
 * browser's holds the button and leaves the box open (`Composer.tsx`).
 *
 * THOSE ARE THE RULES FOR A MESSAGE, and a message's own changes have rules
 * of their own, mirrored from the engine's write path one per tool: a
 * reaction is refused outside a private room as a post is; an edit is its
 * author's and a delete its author's or an admin's, both reaching past
 * membership; an archive closes a room to new messages and to nothing else;
 * and a kind this build cannot classify takes none of them. Each control is
 * drawn and held with its own sentence, never hidden.
 *
 * # A write is read back, never drawn ahead
 *
 * Every change here lands on the chat log and raises this tab's read floor
 * for it (`protocol/session.ts`), so the room, its transcript and the rail
 * are asked again at that floor and redrawn from an answer that includes the
 * change. Nothing here patches a row or refetches by hand: an edit that
 * rewrote the row on screen would be a second rendering of a message.
 */

import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";

import { useNavigator, useParam } from "~/app/router.tsx";
import { DraftBoxes, DraftBoxesContext, boxesDraft, useOpenBoxes } from "~/app/assistant/boxes.ts";
import { useAssistantDraft } from "~/app/assistant/draft.ts";
import { usePageCoverage, usePageInView, usePageLabels } from "~/app/Shell.tsx";
import { QueryState, SeatChip } from "~/components/common.tsx";
import { RefusalNote, WriteButton, pressable } from "~/components/WriteButton.tsx";
import { Button, Callout, Skeleton, StatusDot, Tag } from "@crewlethq/ui";
import { PencilGlyph, UsersGlyph } from "@crewlethq/icons/glyphs";
import { NamedGlyph } from "@crewlethq/icons/glyphs/registry";
import { useClient, useConnection } from "~/lib/store-hooks.ts";
import { useAct } from "~/lib/useAct.ts";
import { useWriteAccess } from "~/lib/useWriteAccess.ts";
import { useViewer } from "~/lib/viewer.ts";
import { useQuery, withFloor } from "~/lib/useQuery.ts";
import { useSeatBadgeOf } from "~/lib/seats.ts";
import type { QueryErrorCode } from "~/contract/errors.ts";
import {
  queryErrorCode,
  type ChatChannelSummary,
  type ChatMessageView,
  type ChatMessagesAnswer,
} from "~/protocol/index.ts";

import { Composer } from "./Composer.tsx";
import { markRead, packPosition, type ReadCursors } from "./cursor.ts";
import { canJoin, canLeave, membershipNote } from "./directory.ts";
import {
  useChatFocus,
  useChatFrames,
  useChatPresence,
  useChatRead,
  useSettledIDs,
} from "./live.ts";
import {
  answersFor,
  EMPTY_TAIL,
  mergeNewest,
  mergeOlder,
  mergeRange,
  rangesToReread,
  touchesOf,
  type Reread,
  type Touch,
} from "./messages.ts";
import type { OutboxHandle } from "./outbox.ts";
import { coverageOf, PAGE } from "./page.ts";
import { RoomSettings } from "./RoomSettings.tsx";
import { isDirect, knownKind, NEWER_ROOM, roomKindLabel, roomMark, roomTitle } from "./rooms.ts";
import { reportFor, type StartReport } from "./start.ts";
import { StartNote } from "./StartNote.tsx";
import { ThreadPane } from "./Thread.tsx";
import { Transcript, type MessageRules } from "./Transcript.tsx";

const NO_REPLIES: ChatMessageView[] = [];

/** The three refusals a room makes of a reader, each a fact about the room. */
const ARCHIVED =
  "This room is archived. You can still read it and edit, delete or react to what’s in it, but it takes no new messages.";
const UNKNOWN_KIND = NEWER_ROOM;
const OUTSIDER =
  "This is a private room you’re not in, so you can’t post or react here. An admin can still take a message down.";

/** What every control says before the room's own answer has come. */
const READING = "Reading the room before anything in it can change.";

/** What every control says when the room's own answer did not come. */
const UNREAD = "This room couldn’t be read, so nothing in it can change. The reason is at the top.";

export interface RoomProps {
  channelID: string;
  viewer: string;
  nameOf: (handle: string) => string;
  /** This room's row in the rail, when it is one of the reader's own. Absent
   *  for a public room they are reading without having joined. */
  summary?: ChatChannelSummary;
  cursors: ReadCursors;
  /** The screen's outbox, which outlives this room (`outbox.ts`). */
  outbox: OutboxHandle;
}

export function Room({ channelID, viewer, nameOf, summary, cursors, outbox }: RoomProps) {
  const { socket, store } = useClient();
  const { connected } = useConnection();
  // AN ADMIN MODERATES — may take anybody's message down — and nobody else:
  // the authority table's rule for `delete_message`, which the engine judges
  // against the stored author ([Transcript]'s `moderates`).
  const moderates = useViewer().admin;
  const [threadRoot, setThreadRoot] = useParam("thread", "", "section");

  const detail = useQuery("chat_channel", { channel_id: channelID });
  const newest = useQuery("chat_messages", { channel_id: channelID, limit: PAGE });
  // THE STATE BAR DESCRIBES THE TRANSCRIPT, which is the answer this room is
  // drawn from: a node behind the log, or a deferred record that met the
  // read, is a fact about the messages on screen (`coverageOf`).
  usePageCoverage(coverageOf(newest.data));
  const [tail, setTail] = useState(EMPTY_TAIL);
  const [paging, setPaging] = useState(false);
  const [pageError, setPageError] = useState<QueryErrorCode | null>(null);
  const [replies, setReplies] = useState<ChatMessageView[]>(NO_REPLIES);

  // WHAT FRAMES HAVE TOUCHED that the next newest page must be checked
  // against, each with where it sits on the log (`answersFor`).
  const touched = useRef<Touch[]>([]);
  // THE TAIL AS LAST DRAWN, for the merge's effect to find the rows below
  // the newest page in — the state itself is only ever moved functionally.
  const drawn = useRef(tail);
  drawn.current = tail;
  // WHETHER THERE IS STILL A ROOM TO MERGE A RANGE INTO.
  const mounted = useRef(false);
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);

  /** Read each range again, ONE AT A TIME: a reader who paged back through
   *  an afternoon holds many, and the transcript, the rail and the thread
   *  share the socket's four in-flight queries with them. */
  const reread = useCallback(
    async (ranges: readonly Reread[]) => {
      for (const range of ranges) {
        let page: ChatMessagesAnswer;
        try {
          page = (await socket.query(
            "chat_messages",
            withFloor(
              "chat_messages",
              JSON.stringify({ channel_id: channelID, cursor: range.cursor, limit: PAGE }),
            ),
          )) as ChatMessagesAnswer;
        } catch {
          // A RANGE THAT COULD NOT BE READ AGAIN keeps what it last said and
          // is asked for again with the next newest page, rather than never.
          for (const message of range.messages) touched.current.push({ position: null, message });
          continue;
        }
        if (!mounted.current) return;
        setTail((held) => mergeRange(held, range.cursor, page));
      }
    },
    [socket, channelID],
  );

  useEffect(() => {
    const page = newest.data;
    if (!page) return;
    setTail((held) => mergeNewest(held, page));
    const due = touched.current.filter((touch) => answersFor(page.position, touch));
    if (due.length === 0) return;
    touched.current = touched.current.filter((touch) => !due.includes(touch));
    const ranges = rangesToReread(mergeNewest(drawn.current, page), page, due, PAGE);
    if (ranges.length > 0) void reread(ranges);
  }, [newest.data, reread]);

  // EVERY FRAME IN THIS ROOM IS A RE-READ OF THE NEWEST PAGE, coalesced: a
  // burst of six messages is one query rather than six. What the frames
  // touched below it is read once that page has landed, above.
  useChatFrames(channelID, (frames) => {
    touched.current.push(...touchesOf(frames));
    newest.refetch();
    detail.refetch();
  });

  // WHERE THIS TAB IS LOOKING. It joins this socket to the room's presence,
  // which is what puts this reader in the "who is here" line on everybody
  // else's screen and what brings this screen the line on theirs.
  const typing = useChatFocus(channelID);
  const presence = useChatPresence(channelID);

  // LEAVING A ROOM WRITES THE CURSOR AT ONCE rather than waiting out the
  // interval, which is one of the three moments where the next flush might
  // never happen.
  useEffect(() => () => cursors.flushNow(), [cursors, channelID]);

  // A MESSAGE IS READ WHEN IT HAS BEEN ON SCREEN WHILE THE ROOM WAS IN VIEW:
  // the tab in front, and nothing standing over the page (`usePageInView`). A
  // tab in the background, or a room under the assistant or a dialog, is
  // drawing for nobody, and marking those rows read is how a person comes back
  // to a cleared badge, on every device, over messages they never saw.
  const inView = usePageInView();
  const showing = useRef(inView);
  showing.current = inView;
  // THE NEWEST ROW THE TRANSCRIPT HAS DRAWN, seen or not, for the moment the
  // room is back in view.
  const newestDrawn = useRef<ChatMessageView | null>(null);
  const onSeen = useCallback(
    (view: ChatMessageView) => {
      newestDrawn.current = view;
      if (showing.current) cursors.see(channelID, packPosition(view.position));
    },
    [cursors, channelID],
  );
  // AND WHAT IT DREW MEANWHILE IS SEEN ONCE THE ROOM IS BACK IN VIEW. The
  // transcript reports a row as it draws it, so a row drawn under the
  // assistant is reported once, while nobody could see it; it is on screen
  // now, and nothing else would report it until the next message came.
  useEffect(() => {
    const view = newestDrawn.current;
    if (inView && view) cursors.see(channelID, packPosition(view.position));
  }, [inView, cursors, channelID]);

  const loadOlder = useCallback(async () => {
    if (!tail.older || paging) return;
    setPaging(true);
    setPageError(null);
    try {
      // AT THIS TAB'S READ FLOOR, as every ask is (`withFloor`): a page read
      // from a node that has not yet applied this tab's own write would page
      // back through a room without it.
      const page = (await socket.query(
        "chat_messages",
        withFloor(
          "chat_messages",
          JSON.stringify({ channel_id: channelID, cursor: tail.older, limit: PAGE }),
        ),
      )) as ChatMessagesAnswer;
      setTail((held) => mergeOlder(held, page));
    } catch (err) {
      setPageError(queryErrorCode(err instanceof Error ? err.message : null) ?? "query_failed");
    } finally {
      setPaging(false);
    }
  }, [socket, channelID, tail.older, paging]);

  // THIS ROOM SETTLES ITS OWN ROWS of the screen's outbox, against what its
  // transcript and its open thread now carry; every other room's rows wait
  // for the person to come back to them (`outbox.ts`).
  const settled = useSettledIDs(tail.messages, replies);
  const { settle } = outbox;
  useEffect(() => settle(channelID, settled), [settle, channelID, settled]);

  const channel = detail.data?.channel ?? summary?.channel;
  const members = detail.data?.members;
  const title = channel
    ? roomTitle(channel, { participants: summary?.participants, viewer, nameOf })
    : channelID;
  // WHAT A SENTENCE CALLS THE ROOM: `#general`, or who a direct conversation is with.
  const where = channel && !isDirect(channel.kind) ? `#${title}` : title;

  // THREE DIFFERENT REFUSALS, told apart. Each is a fact about the room rather
  // than about who is reading, which the server has already settled.
  const archived = !!channel?.archived_at;
  const unknown = !!channel && !knownKind(channel.kind);
  const outsider = !!channel && channel.kind === "private" && detail.data?.member === false;
  const roomRefusal = archived ? ARCHIVED : unknown ? UNKNOWN_KIND : outsider ? OUTSIDER : "";
  // A ROOM NOT YET READ TAKES NOTHING YET: which of the three it is, if any,
  // is the room's own answer, and a control offered before it arrived would
  // be offered on a guess. AND ONE WHOSE READ FAILED SAYS SO rather than
  // claiming to be reading — `useQuery` asks again only after `unavailable`,
  // so every other failure would hold every control "reading" for ever.
  const unread = detail.error ? UNREAD : READING;
  const ruled = (refusal: string) => (channel ? refusal : unread);
  const roomHold = ruled(roomRefusal);
  // EACH OF A MESSAGE'S CHANGES BY ITS OWN TOOL'S RULE, never the post's
  // (`internal/chatlet`'s write path): a reaction takes the room's membership
  // rule, an edit and a delete reach past membership (the author's own, and
  // an admin's moderation), an archive refuses none of the three, and a
  // kind this build cannot classify refuses all of them.
  const rules: MessageRules = {
    react: ruled(unknown ? UNKNOWN_KIND : outsider ? OUTSIDER : ""),
    edit: ruled(unknown ? UNKNOWN_KIND : ""),
    remove: ruled(unknown ? UNKNOWN_KIND : ""),
  };
  // THE ROOM'S OWN GATE, THEN THE BROWSER'S. The room's sentence is said above
  // the transcript and closes the composer; the browser's is the one every
  // control in the dashboard is held with, and it holds the composer's Send.
  const browser = outbox.post.can ? "" : outbox.post.reason;
  // WHAT A ROOM'S SETTINGS TAKE IS NOT WHAT ITS TRANSCRIPT TAKES. The engine
  // refuses a patch from outside a private room and on a kind this build
  // cannot classify, as it does a message — and takes one on an ARCHIVED
  // room, which keeps its topic and purpose for whoever reads it after.
  // Mirrored so the opener is held where the engine would refuse and nowhere
  // else, never so this screen can decide anything the engine has not.
  const settings = useWriteAccess("update_channel");
  const topicHold =
    ruled(unknown ? UNKNOWN_KIND : outsider ? OUTSIDER : "") ||
    (settings.can ? "" : settings.reason);

  const post = useCallback(
    (body: string) => void outbox.send({ channelID, body }),
    [outbox, channelID],
  );
  const reply = useCallback(
    (body: string) => void outbox.send({ channelID, threadRoot, body }),
    [outbox, channelID, threadRoot],
  );
  // THE THREE WRITES THAT ARE NOT A MESSAGE. A reaction is QUIET — a toast
  // per emoji would be louder than the emoji — and an edit and a delete say
  // what they did; a refusal of any of them is drawn above the transcript.
  const react = useAct("react_to_message");
  const edit = useAct("edit_message");
  const remove = useAct("delete_message");
  const onReact = useCallback(
    (messageID: string, emoji: string, removing: boolean) => {
      // ACCESS, NOT BUSY: two chips are two independent toggles, and one
      // still in flight is no reason to drop a press on the other.
      if (!react.access.can) return;
      void react.run(
        { channel: channelID, message_id: messageID, emoji, remove: removing },
        { done: removing ? "Took the reaction back" : "Reacted", quiet: true },
      );
    },
    [react, channelID],
  );
  const onEdit = useCallback(
    async (messageID: string, body: string) => {
      // THE BUTTON'S OWN GATE, because a form submits on its own: a second
      // submit while the first answer was out would rewrite the message
      // again under a new operation key.
      if (!pressable(edit)) return false;
      const result = await edit.run(
        { channel: channelID, message_id: messageID, body },
        { done: "Edited the message" },
      );
      return result !== null && (result.kind === "applied" || result.kind === "pending");
    },
    [edit, channelID],
  );
  // WHICH MESSAGE'S DELETE IS OUT, so that row's button shows it is sending
  // and every other row's is held with why rather than taking a press the
  // hook would drop.
  const [deleting, setDeleting] = useState("");
  const onDelete = useCallback(
    async (messageID: string) => {
      if (!pressable(remove)) return;
      setDeleting(messageID);
      try {
        await remove.run(
          { channel: channelID, message_id: messageID },
          { done: "Deleted the message" },
        );
      } finally {
        setDeleting("");
      }
    },
    [remove, channelID],
  );

  // THE TRAIL AND THE TAB TITLE READ THE ROOM'S NAME, not its uuid. Published
  // rather than derived by the frame, because the name is an answer this
  // screen has and the router does not — and it is withdrawn when the screen
  // goes, so the next screen's identically shaped segment is not titled with
  // a room.
  usePageLabels(channel ? { [channelID]: isDirect(channel.kind) ? title : `#${title}` } : {});

  const readState = useChatRead();
  const mute = useAct("mark_chat_read");
  const muted = readState?.muted?.includes(channelID) ?? summary?.muted ?? false;
  const toggleMute = useCallback(() => {
    // THIS ROOM, NEVER THE LIST: the engine merges the one room into whatever
    // the record holds, so a tab that has not seen the record yet needs
    // nothing read first, and another tab toggling a different room at the
    // same moment loses nothing. The new state is the write's own answer,
    // applied as the push it raises would be ([markRead]).
    void markRead(mute, store, muted ? { unmute: [channelID] } : { mute: [channelID] }, {
      done: muted ? "Unmuted the room" : "Muted the room",
    });
  }, [mute, store, muted, channelID]);

  // WHETHER THE READER IS IN THIS ROOM IS THE SERVER'S ANSWER, and it is a
  // different question from whether they may read it: a public room and a
  // unit's room are readable by every seat, joined or not, so arriving here
  // from a search result is an ordinary way to be standing outside a room
  // whose whole transcript is on the screen. The member flag is the room's own
  // detail rather than the rail, which is bounded and leaves archived rooms
  // out.
  const nav = useNavigator();
  const join = useAct("join_channel");
  const leave = useAct("leave_channel");
  const [membership, setMembership] = useState<StartReport | null>(null);
  const [settingTopic, setSettingTopic] = useState(false);
  const standing = detail.data
    ? { kind: detail.data.channel.kind, member: detail.data.member, unit: detail.data.channel.unit }
    : null;

  const moveMembership = useCallback(
    async (leaving: boolean) => {
      const write = leaving ? leave : join;
      if (!pressable(write)) return;
      setMembership(null);
      // QUIET: what the gesture came to is drawn as the room's own note —
      // `start.ts`, which every gesture that starts or moves a membership
      // shares — rather than a toast that would say "joined" over a room
      // this node has not applied the join to yet.
      const result = await write.run(
        { channel: channelID },
        { done: leaving ? "Left the room" : "Joined the room", quiet: true },
      );
      if (result === null) return;
      const answer = reportFor(leaving ? "leave" : "join", result);
      if (answer.tone === "success") {
        // LEAVING TAKES THE READER OUT OF THE ROOM, and a private room they
        // have just left is one they can no longer read at all — so staying
        // on it would turn into "no such room" under them a moment later.
        // The room itself, read again at the join's floor, is the
        // confirmation of a join.
        if (leaving) nav.to(["chatlet"]);
        return;
      }
      setMembership(answer);
    },
    [join, leave, channelID, nav],
  );

  const pending = outbox.rows.filter((row) => row.channelID === channelID && !row.threadRoot);
  const threadPending = outbox.rows.filter(
    (row) => row.channelID === channelID && !!threadRoot && row.threadRoot === threadRoot,
  );

  return (
    <RoomFrame where={where} withThread={threadRoot !== ""}>
      <div className="chat-room-main">
        <header className="chat-head">
          <div className="row gap-2">
            {channel && <NamedGlyph name={roomMark(channel.kind)} size="sm" />}
            <strong className="truncate t-cell">
              {channel && !isDirect(channel.kind) ? `#${title}` : title}
            </strong>
            {channel && <Tag appearance="outline">{roomKindLabel(channel.kind)}</Tag>}
            {muted && <Tag appearance="outline">muted</Tag>}
            <span className="spacer" />
            {/* WHAT THIS READER MAY DO ABOUT BEING IN THE ROOM, and nothing
                the engine would refuse: a private room admits no join, a
                unit's room neither a join nor a leave — its membership is the
                org chart's — and a direct conversation neither.
                `directory.ts` mirrors those rules rather than inventing any. */}
            {standing && canJoin(standing) && (
              <WriteButton
                variant="secondary"
                write={join}
                showRefusal={false}
                onPress={() => void moveMembership(false)}
              >
                {join.busy ? "Joining" : "Join"}
              </WriteButton>
            )}
            {standing && canLeave(standing) && (
              <WriteButton
                variant="ghost"
                write={leave}
                showRefusal={false}
                onPress={() => void moveMembership(true)}
              >
                {leave.busy ? "Leaving" : "Leave"}
              </WriteButton>
            )}
            {/* THE OPENER IS HELD AS ITS CHANGE WOULD BE — by the room's own
                rule for a patch, then by the browser's reason for
                `update_channel` — and drawn for everybody, as every other
                screen draws the button that opens a change: a dialog whose
                Save could never be pressed is a dialog nobody should have to
                open to find that out. */}
            <Button
              variant="ghost"
              leadingIcon={<PencilGlyph size="sm" />}
              disabledReason={topicHold || undefined}
              title={topicHold || undefined}
              onClick={() => setSettingTopic(true)}
            >
              Topic
            </Button>
            <WriteButton variant="ghost" write={mute} onPress={toggleMute}>
              {muted ? "Unmute" : "Mute"}
            </WriteButton>
          </div>
          {channel?.topic && <p className="t-caption truncate">{channel.topic}</p>}
          <div className="row wrap gap-2 t-caption muted">
            {/* A COUNT ONLY WHERE THERE IS ONE: before the room's answer, or
                after it failed, "0 members" would be a fact nobody stated. */}
            {members && (
              <span className="row gap-1">
                <UsersGlyph size="xs" />
                {members.length} member{members.length === 1 ? "" : "s"}
              </span>
            )}
            {channel?.unit && <span>the {channel.unit} team&rsquo;s room</span>}
            {/* STANDING OUTSIDE A ROOM YOU CAN READ IS AN ORDINARY STATE, and
                an unsaid one is what makes a rail that does not list the room
                look like a rail that lost it. */}
            {standing && !standing.member && <span>you&rsquo;re not in this room</span>}
            {summary?.follow_all && <span>every message here reaches you</span>}
            {channel?.retention_days !== undefined && channel.retention_days !== null && (
              <span>
                {channel.retention_days === 0
                  ? "kept for ever"
                  : `kept ${channel.retention_days} days`}
              </span>
            )}
          </div>
          <Presence presence={presence} viewer={viewer} nameOf={nameOf} />
          {/* WHY THE ROOM IS NOT DESCRIBED, in the words every read's refusal
              is drawn in — and only where nothing else describes it: a room
              the rail holds still has its name and kind from there. */}
          {!channel && detail.error && (
            <QueryState
              error={detail.error}
              refusal={detail.refusal}
              detail={detail.detail}
              loading={false}
            />
          )}
        </header>

        <StartNote report={membership} />
        {standing &&
          // A ROOM THIS BUILD CANNOT CLASSIFY IS NOT SAID TWICE. `refusal`
          // below already carries that sentence, and two renderings of one
          // rule are two sentences that can come to disagree.
          knownKind(standing.kind) &&
          !canJoin(standing) &&
          !canLeave(standing) &&
          membershipNote(standing) && (
            // WHY THERE IS NO BUTTON, said rather than left to an absence.
            // Each of these is a rule about the room — the org chart maintains
            // this membership, a private room is joined by being added, a
            // direct conversation is the people in it — and a person who
            // cannot see the rule reads the missing control as a permission
            // they lack.
            <p className="t-caption muted">{membershipNote(standing)}</p>
          )}
        {roomRefusal && <Callout variant="neutral">{roomRefusal}</Callout>}
        {/* WHAT BECAME OF AN EDIT, A DELETE OR A REACTION that was refused,
            above the transcript the pressed row sits in: the engine's sentence
            names what was wrong, and a toast is gone before it is read. */}
        <RefusalNote write={edit} />
        <RefusalNote write={remove} />
        <RefusalNote write={react} />
        {/* WHY THE OLDER PAGE DID NOT COME, in the words every screen's
            pager uses; what is on screen is still what this node holds. */}
        {pageError && <QueryState error={pageError} refusal={null} loading={false} />}
        {tail.unreadable > 0 && (
          <Callout variant="warning">
            {tail.unreadable} message{tail.unreadable === 1 ? "" : "s"} on this page came from a
            newer version of the engine, so {tail.unreadable === 1 ? "it isn’t" : "they aren’t"}{" "}
            shown yet. Upgrading this node shows {tail.unreadable === 1 ? "it" : "them"}.
          </Callout>
        )}

        {newest.loading && !tail.loaded && (
          <Skeleton variant="text" rows={8} label="Reading the room" />
        )}
        <QueryState
          error={newest.error}
          refusal={newest.refusal}
          detail={newest.detail}
          loading={newest.loading}
          empty={
            tail.loaded && tail.messages.length === 0 && pending.length === 0
              ? {
                  title: "Nothing has been said here yet",
                  hint: "Say something to start the conversation.",
                }
              : undefined
          }
        >
          <Transcript
            tail={tail}
            pending={pending}
            onOlder={() => void loadOlder()}
            paging={paging}
            threadRoot={threadRoot}
            onOpenThread={(rootID) => setThreadRoot(rootID === threadRoot ? "" : rootID)}
            onReact={onReact}
            onEdit={onEdit}
            onDelete={(id) => void onDelete(id)}
            deleting={deleting}
            onRetry={(id) => void outbox.retry(id)}
            onDiscard={outbox.discard}
            onSeen={onSeen}
            viewer={viewer}
            moderates={moderates}
            nameOf={nameOf}
            writes={{ react, edit, remove }}
            rules={rules}
            retryHold={roomHold || browser}
            working={presence.working ?? []}
          />
        </QueryState>

        <Composer what={where} send={post} onTyping={typing} closed={roomHold} held={browser} />
        {/* WHAT OFFLINE MEANS FOR READING. What it means for saying something
            is the composer's own sentence, beside the Send it holds. */}
        {!connected && (
          <p className="t-caption muted">
            The live connection is down, so this room shows what it last read. It catches up when
            the connection is back.
          </p>
        )}
      </div>

      {settingTopic && channel && (
        <RoomSettings
          channel={channel}
          member={standing?.member === true}
          onClose={() => setSettingTopic(false)}
        />
      )}

      {threadRoot && (
        // ONE PANE PER THREAD: what a pane has paged in belongs to its root,
        // and another root opened over it is another conversation.
        <ThreadPane
          key={threadRoot}
          channelID={channelID}
          rootID={threadRoot}
          onClose={() => setThreadRoot("")}
          onReplies={setReplies}
          send={reply}
          nameOf={nameOf}
          closed={roomHold}
          held={outbox.reply.can ? "" : outbox.reply.reason}
          pending={threadPending}
          onRetry={(id) => void outbox.retry(id)}
          onDiscard={outbox.discard}
          viewer={viewer}
        />
      )}
    </RoomFrame>
  );
}

/**
 * The room's outer frame: its grid, and the ONE draft for the person's own
 * assistant that every box open in it is a field of (`app/assistant/boxes.ts`):
 * the message box, the reply box of the thread open beside it and each of the
 * person's messages open for a rewrite. The room declares it rather than each
 * box its own, because the page context carries one draft and each box's own
 * hid the others from the assistant at its next keystroke.
 */
function RoomFrame({
  where,
  withThread,
  children,
}: {
  where: string;
  withThread: boolean;
  children: ReactNode;
}) {
  const [boxes] = useState(() => new DraftBoxes());
  return (
    <DraftBoxesContext.Provider value={boxes}>
      <div className={`chat-room${withThread ? " with-thread" : ""}`}>{children}</div>
      <RoomDraft boxes={boxes} where={where} />
    </DraftBoxesContext.Provider>
  );
}

/**
 * The room's draft, declared from the boxes open on it. A child of its own, so
 * a keystroke in a box renders the box and this, never the room and its
 * transcript.
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

/** Who else is here, and which seat is working on an answer. */
function Presence({
  presence,
  viewer,
  nameOf,
}: {
  presence: {
    viewing?: string[];
    typing?: string[];
    working?: { handle: string; thread?: string; status?: string }[];
  };
  viewer: string;
  nameOf: (handle: string) => string;
}) {
  // THE BADGE IS THE CHART'S, as on every other chip: a person here is drawn
  // as the circle and an agent as the character it chose. A chip handed only
  // a name takes the kit's default, the agent's squircle, which put every
  // person looking at a room in an agent's outline. The NAME stays this
  // screen's own `nameOf`, so the chip and the typing line beside it cannot
  // name one person two ways.
  const badgeOf = useSeatBadgeOf();
  const viewing = (presence.viewing ?? []).filter((handle) => handle !== viewer);
  const typing = (presence.typing ?? []).filter((handle) => handle !== viewer);
  // WHAT IS BEING ANSWERED UNDER A MESSAGE IS DRAWN THERE, by the transcript.
  // What is left here is a seat working on the room rather than on one thing
  // in it — which is what an empty `thread` means.
  const working = (presence.working ?? []).filter((seat) => !seat.thread);
  if (viewing.length === 0 && typing.length === 0 && working.length === 0) return null;
  return (
    <div className="row wrap gap-2 chat-presence t-caption">
      {viewing.length > 0 && (
        <span className="row gap-1">
          {viewing.slice(0, 5).map((handle) => (
            <SeatChip key={handle} {...badgeOf(handle)} name={nameOf(handle)} handle={handle} />
          ))}
          {viewing.length > 5 && <span className="muted">+{viewing.length - 5}</span>}
          <span className="muted">here</span>
        </span>
      )}
      {typing.length > 0 && (
        <span className="muted">
          {typing.map((handle) => nameOf(handle)).join(", ")} {typing.length === 1 ? "is" : "are"}{" "}
          typing…
        </span>
      )}
      {working.map((seat) => (
        // THE KIT'S WORKING MARK, as the sidebar and the org chart draw a seat
        // that is working: the info dot, breathing for as long as the turn
        // runs, beside the word that says so — the dot is never the only
        // carrier, and the kit hides it from a screen reader for that reason.
        <StatusDot key={`${seat.handle}:${seat.thread ?? ""}`} tone="info" pulse>
          {nameOf(seat.handle)} {seat.status || "is working on a reply"}
        </StatusDot>
      ))}
    </div>
  );
}
