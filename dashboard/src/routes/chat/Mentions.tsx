/**
 * Every message that named this person, across every room.
 *
 * THE ONE LIST THAT SPANS THE ROOMS, and the product's whole away story: the
 * engine sends no email, no push and no SMS — it has never sent anything as
 * itself — so a person learns they were named when they next open this.
 *
 * ITS CURSOR IS A LOG POSITION where a transcript's is a room's own message
 * number, and the two are not interchangeable. A mention's position is the one
 * that FIRST named this handle and it never moves — a later edit that renames
 * nobody new leaves it exactly where it was — so paging through this feed
 * spans a reanchor with no gap and no repeat. It is carried back unchanged and
 * this screen has an opinion about neither.
 */

import { useCallback, useEffect, useState } from "react";

import { href } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { Button, Callout, Skeleton, Tag } from "@crewlethq/ui";
import { NotificationsGlyph } from "@crewlethq/icons/glyphs";
import { Mark } from "~/ui/glyph.tsx";
import { useClient, useConnection } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { relTime } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import type { ChatMention, ChatMentionsAnswer } from "~/protocol/index.ts";

import { DEGRADED_POLL_MS, useChatActivity, useCoalesced } from "./live.ts";
import { PAGE } from "./page.ts";
import { roomMark } from "./rooms.ts";

export function Mentions({ nameOf }: { nameOf: (handle: string) => string }) {
  const { socket } = useClient();
  const now = useNow();
  const [older, setOlder] = useState<ChatMention[]>([]);
  const [cursor, setCursor] = useState("");
  const [paging, setPaging] = useState(false);
  const [pageError, setPageError] = useState<string | null>(null);

  const { connected } = useConnection();
  const feed = useQuery(
    "chat_mentions",
    { limit: PAGE },
    connected ? undefined : { pollMs: DEGRADED_POLL_MS },
  );
  // A MENTION ANYWHERE IS A FRAME SOMEWHERE, so this re-reads on the same
  // signal the rail does. Coalesced: a burst in one room is one change here.
  useCoalesced(useChatActivity(), feed.refetch);

  // The newest page is re-read live, so everything below it is discarded when
  // it moves: a page built on a cursor from an answer that has been replaced
  // would interleave two readings of the same feed.
  useEffect(() => {
    setOlder([]);
    setCursor(feed.data?.next_cursor ?? "");
  }, [feed.data]);

  const loadOlder = useCallback(async () => {
    if (!cursor || paging) return;
    setPaging(true);
    setPageError(null);
    try {
      const page = (await socket.query("chat_mentions", {
        cursor,
        limit: PAGE,
      })) as ChatMentionsAnswer;
      setOlder((held) => [...held, ...(page.mentions ?? [])]);
      setCursor(page.next_cursor ?? "");
    } catch (err) {
      setPageError(err instanceof Error ? err.message : "query_failed");
    } finally {
      setPaging(false);
    }
  }, [socket, cursor, paging]);

  const rows = [...(feed.data?.mentions ?? []), ...older];

  return (
    <div className="chat-list">
      <header className="chat-head">
        <div className="row gap-2">
          <NotificationsGlyph size="sm" />
          <strong className="t-cell">Mentions</strong>
        </div>
        <p className="t-caption muted">
          Where you were named. There is no notification away from this screen — the engine sends no
          email, no push and no SMS.
        </p>
      </header>

      {feed.loading && !feed.data && <Skeleton variant="text" rows={6} label="Reading" />}
      {pageError && (
        <Callout variant="warning">The older mentions could not be read ({pageError}).</Callout>
      )}

      <QueryState
        error={feed.error}
        loading={feed.loading}
        empty={
          rows.length
            ? undefined
            : {
                title: "Nobody has named you",
                hint: "A mention is a message that named your handle — @you — or a reply in a thread you started. A broadcast to a whole room is not one.",
              }
        }
      >
        <div className="list">
          {rows.map((mention) => (
            <MentionRow key={mention.message_id} mention={mention} nameOf={nameOf} now={now} />
          ))}
        </div>
        {cursor && (
          <Button variant="tertiary" onClick={() => void loadOlder()} disabled={paging}>
            {paging ? "Reading…" : "Older mentions"}
          </Button>
        )}
      </QueryState>
    </div>
  );
}

function MentionRow({
  mention,
  nameOf,
  now,
}: {
  mention: ChatMention;
  nameOf: (handle: string) => string;
  now: number;
}) {
  // A MENTION IN A THREAD OPENS THE THREAD, not the room: the words were said
  // under a message, and the room's own transcript is not where their context
  // is.
  const to = href(
    ["chat", mention.channel_id],
    mention.thread_root ? { thread: mention.thread_root } : undefined,
  );
  return (
    <a className="hit" href={to}>
      <span className="row gap-2">
        <Mark name={roomMark(mention.channel_kind)} size="xs" />
        <strong className="truncate t-cell">
          {mention.channel_name ? `#${mention.channel_name}` : "a direct conversation"}
        </strong>
        {mention.thread_root && <Tag appearance="outline">in a thread</Tag>}
        <span className="spacer" />
        <span className="t-caption muted">{relTime(mention.at, now)}</span>
      </span>
      <span className="t-caption">
        {mention.author ? `${nameOf(mention.author)}: ` : ""}
        {mention.deleted ? "(deleted)" : mention.excerpt || "…"}
      </span>
    </a>
  );
}
