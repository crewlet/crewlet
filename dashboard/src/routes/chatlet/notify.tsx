/**
 * How a room reaches its reader: every message, mentions (the default), or
 * nothing.
 *
 * ONE READING AND ONE WRITE for the two places a reader chooses it, the top
 * bar's bell and the details pane's Notifications tab, so the two can never
 * name the same room's level two ways. The level is the reader's own mark on
 * their read record (`every` and `muted` in `ChatReadState`), written through
 * `mark_chat_read`'s `notify`, whose answer is the record as it left it and is
 * applied as the push it raises would be (`cursor.ts`' `markRead`).
 *
 * A MARK, NOT A MEMBERSHIP. It wakes nobody and changes nothing for anybody
 * else: a person is never woken by a message, so the level says what counts
 * as unread for them and what reaches their Activity, nothing more.
 */

import { useCallback } from "react";

import { SegmentedControl } from "@crewlethq/ui";
import { BellGlyph } from "@crewlethq/icons/glyphs";

import { useClient } from "~/lib/store-hooks.ts";
import { useAct } from "~/lib/useAct.ts";
import type { ChatNotifyLevel, ChatReadState } from "~/protocol/index.ts";

import { markRead } from "./cursor.ts";
import { useChatRead } from "~/lib/chatLive.ts";

/** What each level is called, the bell's menu and the tab alike. */
export const NOTIFY_LABEL: Record<ChatNotifyLevel, string> = {
  all: "Every message",
  mentions: "Mentions",
  muted: "Muted",
};

/** What each level means, in a sentence under the choice. */
export const NOTIFY_MEANS: Record<ChatNotifyLevel, string> = {
  all: "Each new message here counts as unread for you and shows in Activity.",
  mentions:
    "Messages that name you, @channel and direct messages reach you. Everything else is marked unread in the list until you read it.",
  muted:
    "Nothing here reaches you. The room stays in your list, dimmed, and a mention of you still shows in Activity.",
};

/** The level a read state holds for a room. */
export function levelOf(
  read: ChatReadState | null | undefined,
  channelID: string,
): ChatNotifyLevel {
  if (read?.muted?.includes(channelID)) return "muted";
  if (read?.every?.includes(channelID)) return "all";
  return "mentions";
}

/** A room's level for this reader, and the one write that changes it. */
export function useNotifyLevel(channelID: string): {
  level: ChatNotifyLevel;
  /** False while the reader's read state has not been read: the level shown is the default. */
  known: boolean;
  set: (level: ChatNotifyLevel) => void;
  busy: boolean;
  /** Why the write is held, or "". */
  held: string;
} {
  const read = useChatRead();
  const { store } = useClient();
  const mark = useAct("mark_chat_read");
  const level = levelOf(read, channelID);
  const set = useCallback(
    (next: ChatNotifyLevel) => {
      if (next === level || !mark.access.can) return;
      void markRead(
        mark,
        store,
        { notify: { [channelID]: next } },
        {
          done: `Set this room to ${NOTIFY_LABEL[next].toLowerCase()}`,
        },
      );
    },
    [mark, store, channelID, level],
  );
  return {
    level,
    known: read !== null,
    set,
    busy: mark.busy,
    held: mark.access.can ? "" : mark.access.reason,
  };
}

/** The Notifications tab of a room's details. */
export function NotifyLevels({ channelID }: { channelID: string }) {
  const notify = useNotifyLevel(channelID);
  return (
    <div className="chat-pane-section">
      <SegmentedControl
        label="What reaches you"
        semantics="radio"
        value={notify.level}
        fill
        onValueChange={(value) => notify.set(value)}
        options={(["all", "mentions", "muted"] as const).map((value) => ({
          value,
          label: NOTIFY_LABEL[value],
          disabled: notify.held !== "" || notify.busy,
          // HELD WITH WHY, as every write control is (`app/writeGate.test.tsx`).
          ...(notify.held ? { title: notify.held } : {}),
        }))}
      />
      <p className="chat-pane-note">
        {notify.known
          ? NOTIFY_MEANS[notify.level]
          : "Your choices couldn’t be read just now, so this shows the default."}
      </p>
      {notify.held && <p className="chat-pane-note">{notify.held}</p>}
      <p className="chat-pane-note chat-pane-note--lead">
        <BellGlyph size="xs" />
        <span>
          The engine sends no email, push or text message. Activity is where everything that reaches
          you is listed.
        </span>
      </p>
    </div>
  );
}
