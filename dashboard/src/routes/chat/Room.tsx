/**
 * One room: who is in it, what was said, who is here now, and the composer.
 *
 * # A live frame is a reason to re-read, never something to render
 *
 * The socket announces every committed record in the rooms this reader may
 * see, and the frame deliberately carries no body. So the transcript is the
 * one rendering path for a conversation: a frame moves this room's activity
 * counter, the counter makes the newest page re-read, and the page is what
 * lands on screen. The alternative — drawing the frame's excerpt as a message
 * — is a second rendering path that disagrees with the transcript the first
 * time an edit races a reload.
 *
 * # What this room refuses, and why it says so rather than greying out
 *
 * Three things close the composer, and they are different facts: the room is
 * archived (readable for ever, writable by nobody), the reader is not a member
 * of a private room, or this build cannot classify the room's kind at all —
 * which the engine treats as refusing every write, because a kind read
 * two-valued falls out as "not private".
 */

import { useCallback, useEffect, useMemo, useState } from "react";

import { useParam } from "~/app/router.tsx";
import { usePageLabels } from "~/app/Shell.tsx";
import { QueryState, SeatChip } from "~/components/common.tsx";
import { Button, Callout, Skeleton, Tag } from "@crewlethq/ui";
import { GroupGlyph } from "@crewlethq/icons/glyphs";
import { Mark } from "~/ui/glyph.tsx";
import { useClient, useConnection } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import type { ChatChannelSummary, ChatMessageView, ChatMessagesAnswer } from "~/protocol/index.ts";

import { Composer } from "./Composer.tsx";
import { ReadCursors, packPosition } from "./cursor.ts";
import {
  DEGRADED_POLL_MS,
  useChatFocus,
  useChatLive,
  useChatPresence,
  useChatRead,
  useCoalesced,
  useSettledIDs,
} from "./live.ts";
import { EMPTY_TAIL, mergeNewest, mergeOlder } from "./messages.ts";
import { useOutbox } from "./outbox.ts";
import { PAGE } from "./page.ts";
import { isDirect, knownKind, roomKindLabel, roomMark, roomTitle } from "./rooms.ts";
import { ThreadPane } from "./Thread.tsx";
import { Transcript } from "./Transcript.tsx";
import { deleteMessage, editMessage, flushRead, react as sendReaction } from "./writes.ts";

const NO_REPLIES: ChatMessageView[] = [];

export interface RoomProps {
  channelID: string;
  viewer: string;
  nameOf: (handle: string) => string;
  /** This room's row in the rail, when it is one of the reader's own. Absent
   *  for a public room they are reading without having joined. */
  summary?: ChatChannelSummary;
  cursors: ReadCursors;
}

export function Room({ channelID, viewer, nameOf, summary, cursors }: RoomProps) {
  const { socket } = useClient();
  const { connected } = useConnection();
  const [threadRoot, setThreadRoot] = useParam("thread", "", "section");

  // WHILE THE SOCKET IS DOWN THESE POLL, because nothing will arrive on its
  // own: the frames are the live path, and a room with neither is a screen
  // that quietly stops being a conversation.
  const degraded = connected ? undefined : { pollMs: DEGRADED_POLL_MS };
  const detail = useQuery("chat_channel", { channel_id: channelID }, degraded);
  const newest = useQuery("chat_messages", { channel_id: channelID, limit: PAGE }, degraded);
  const [tail, setTail] = useState(EMPTY_TAIL);
  const [paging, setPaging] = useState(false);
  const [pageError, setPageError] = useState<string | null>(null);
  const [replies, setReplies] = useState<ChatMessageView[]>(NO_REPLIES);

  useEffect(() => {
    if (!newest.data) return;
    setTail((held) => mergeNewest(held, newest.data!));
  }, [newest.data]);

  // EVERY FRAME IN THIS ROOM IS A RE-READ, coalesced: a burst of six messages
  // is one query rather than six.
  const live = useChatLive(channelID);
  const activity = live?.changes.length ?? 0;
  useCoalesced(activity, () => {
    newest.refetch();
    detail.refetch();
  });

  // WHERE THIS TAB IS LOOKING. It bounds the fleet-wide presence probe to the
  // rooms somebody actually has open, and it is what puts this reader in the
  // "who is here" line on everybody else's screen.
  const typing = useChatFocus(channelID);
  const presence = useChatPresence(channelID);

  // LEAVING A ROOM WRITES THE CURSOR AT ONCE rather than waiting out the
  // interval, which is one of the three moments where the next flush might
  // never happen.
  useEffect(() => () => cursors.flushNow(), [cursors, channelID]);

  const onSeen = useCallback(
    (view: ChatMessageView) => {
      // A MESSAGE IS READ WHEN IT HAS BEEN ON SCREEN AND THE TAB IS IN FRONT.
      // A tab in the background is rendering to nobody, and marking those read
      // is how a person comes back to a cleared badge over messages they never
      // saw.
      if (typeof document !== "undefined" && document.visibilityState === "hidden") return;
      cursors.see(channelID, packPosition(view.position));
    },
    [cursors, channelID],
  );

  const loadOlder = useCallback(async () => {
    if (!tail.older || paging) return;
    setPaging(true);
    setPageError(null);
    try {
      const page = (await socket.query("chat_messages", {
        channel_id: channelID,
        cursor: tail.older,
        limit: PAGE,
      })) as ChatMessagesAnswer;
      setTail((held) => mergeOlder(held, page));
    } catch (err) {
      setPageError(err instanceof Error ? err.message : "query_failed");
    } finally {
      setPaging(false);
    }
  }, [socket, channelID, tail.older, paging]);

  const settled = useSettledIDs(tail.messages, replies);
  const outbox = useOutbox(settled);

  const channel = detail.data?.channel ?? summary?.channel;
  const members = detail.data?.members ?? [];
  const handles = useMemo(() => members.map((member) => member.handle), [members]);
  const title = channel
    ? roomTitle(channel, { participants: summary?.participants, viewer, nameOf })
    : channelID;

  // THREE DIFFERENT REFUSALS, told apart. Each is a fact about the room rather
  // than about the reader's credential, which the server has already settled.
  const archived = !!channel?.archived_at;
  const unknown = !!channel && !knownKind(channel.kind);
  const outsider = !!channel && channel.kind === "private" && detail.data?.member === false;
  const writable = !!channel && !archived && !unknown && !outsider;
  const refusal = archived
    ? "This room is archived. It stays readable for ever and takes no new messages."
    : unknown
      ? "This room was created by a newer build than this node runs, which refuses every write to it rather than guessing what kind of room it is."
      : outsider
        ? "This is a private room you are not in. A private room refuses every write from outside it, a reaction included."
        : "";

  const post = useCallback(
    (message: { body: string; mentions: string[]; collective: boolean }) => {
      void outbox.send({ channelID, ...message });
    },
    [outbox, channelID],
  );
  const reply = useCallback(
    (message: { body: string; mentions: string[]; collective: boolean }) => {
      void outbox.send({ channelID, threadRoot, ...message });
    },
    [outbox, channelID, threadRoot],
  );
  // THE THREE WRITES THAT ARE NOT A MESSAGE, and each re-reads the room
  // rather than patching a row: the engine's answer says the record landed,
  // and what the room LOOKS like afterwards is the transcript's to say. An
  // edit that rewrote the row here would be a second rendering of a message.
  const onReact = useCallback(
    (messageID: string, emoji: string, remove: boolean) => {
      void sendReaction(channelID, messageID, emoji, remove).then(() => newest.refetch());
    },
    [channelID, newest],
  );
  const onEdit = useCallback(
    (messageID: string, body: string, mentions: string[]) => {
      void editMessage(channelID, messageID, { body, mentions }).then(() => newest.refetch());
    },
    [channelID, newest],
  );
  const onDelete = useCallback(
    (messageID: string) => {
      void deleteMessage(channelID, messageID).then(() => newest.refetch());
    },
    [channelID, newest],
  );

  // THE TRAIL AND THE TAB TITLE READ THE ROOM'S NAME, not its uuid. Published
  // rather than derived by the frame, because the name is an answer this
  // screen has and the router does not — and it is withdrawn when the screen
  // goes, so the next screen's identically shaped segment is not titled with
  // a room.
  usePageLabels(channel ? { [channelID]: isDirect(channel.kind) ? title : `#${title}` } : {});

  const readState = useChatRead();
  const [muting, setMuting] = useState(false);
  const muted = readState?.muted?.includes(channelID) ?? summary?.muted ?? false;
  const toggleMute = useCallback(async () => {
    setMuting(true);
    try {
      // THE WHOLE LIST IS WHAT A FLUSH REPLACES, so it has to be known before
      // it is written: muting one room by sending a list built from the rail
      // alone would silently unmute a room that is not in it. A tab that has
      // not seen the record yet learns it from an EMPTY flush, which changes
      // nothing and answers the state — there is no read question for it on
      // this socket.
      const state = readState ?? (await flushRead({}));
      const next = new Set(state.muted ?? []);
      if (next.has(channelID)) next.delete(channelID);
      else next.add(channelID);
      await flushRead({ muted: [...next] });
    } finally {
      setMuting(false);
    }
  }, [readState, channelID]);

  const pending = outbox.rows.filter((row) => row.channelID === channelID && !row.threadRoot);
  const threadPending = outbox.rows.filter((row) => row.threadRoot === threadRoot && !!threadRoot);

  return (
    <div className={`chat-room${threadRoot ? " with-thread" : ""}`}>
      <div className="chat-room-main">
        <header className="chat-head">
          <div className="row gap-2">
            {channel && <Mark name={roomMark(channel.kind)} size="sm" />}
            <strong className="truncate t-cell">
              {channel && !isDirect(channel.kind) ? `#${title}` : title}
            </strong>
            {channel && <Tag appearance="outline">{roomKindLabel(channel.kind)}</Tag>}
            {muted && <Tag appearance="outline">muted</Tag>}
            <span className="spacer" />
            <Button variant="tertiary" onClick={() => void toggleMute()} disabled={muting}>
              {muted ? "Unmute" : "Mute"}
            </Button>
          </div>
          {channel?.topic && <p className="t-caption truncate">{channel.topic}</p>}
          <div className="row wrap gap-2 t-caption muted">
            <span className="row gap-1">
              <GroupGlyph size="xs" />
              {members.length} member{members.length === 1 ? "" : "s"}
            </span>
            {channel?.unit && <span>the {channel.unit} unit's own room</span>}
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
        </header>

        {!writable && refusal && <Callout variant="neutral">{refusal}</Callout>}
        {pageError && (
          <Callout variant="warning">
            The older messages could not be read ({pageError}). What is on screen is still what this
            node holds.
          </Callout>
        )}
        {tail.unreadable > 0 && (
          <Callout variant="warning">
            {tail.unreadable} message{tail.unreadable === 1 ? "" : "s"} on this page were written by
            a newer build than this node runs, so they are not drawn. Another node can render them,
            and this one can once it is upgraded.
          </Callout>
        )}

        {newest.loading && !tail.loaded && (
          <Skeleton variant="text" rows={8} label="Reading the room" />
        )}
        <QueryState
          error={newest.error}
          loading={newest.loading}
          empty={
            tail.loaded && tail.messages.length === 0 && pending.length === 0
              ? {
                  title: "Nothing has been said here yet",
                  hint: "A room exists from the moment it is created — say something, and every node in the fleet derives the same first line from it.",
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
            onDelete={onDelete}
            handles={handles}
            onRetry={(id) => void outbox.retry(id)}
            onDiscard={outbox.discard}
            onSeen={onSeen}
            viewer={viewer}
            nameOf={nameOf}
            writable={writable}
            working={presence.working ?? []}
          />
        </QueryState>

        <Composer
          what={channel && !isDirect(channel.kind) ? `#${title}` : title}
          handles={handles}
          send={post}
          onTyping={typing}
          disabled={!writable}
          disabledReason={refusal}
        />
        {!connected && (
          <p className="t-caption muted">
            The socket is down, so nothing new will arrive on its own — this room re-reads every few
            seconds until it is back. What you send still goes over its own request.
          </p>
        )}
      </div>

      {threadRoot && (
        <ThreadPane
          connected={connected}
          channelID={channelID}
          rootID={threadRoot}
          activity={activity}
          onClose={() => setThreadRoot("")}
          onReplies={setReplies}
          send={reply}
          handles={handles}
          nameOf={nameOf}
          writable={writable}
          disabledReason={refusal}
          pending={threadPending}
          onRetry={(id) => void outbox.retry(id)}
          onDiscard={outbox.discard}
          viewer={viewer}
        />
      )}
    </div>
  );
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
            <SeatChip key={handle} name={nameOf(handle)} handle={handle} />
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
        <span key={`${seat.handle}:${seat.thread ?? ""}`} className="row gap-1 chat-working">
          <span className="dot working" aria-hidden="true" />
          <span>
            {nameOf(seat.handle)} {seat.status || "is working on a reply"}
          </span>
        </span>
      ))}
    </div>
  );
}
