/**
 * Every message that named this person, across every room.
 *
 * THE ONE LIST THAT SPANS THE ROOMS, and the product's whole away story: the
 * engine sends no email, no push and no SMS — it has never sent anything as
 * itself — so a person learns they were named when they next open this.
 *
 * A MENTION IS THE MOMENT YOU WERE NAMED, and the feed lists those moments
 * newest first, each once — the engine decides it and this screen draws it. An
 * edit that still names you leaves the mention where it was, with the new words
 * and marked edited: fixing a typo is not naming you again, and lifted to the
 * top it would show you as news what you had already read. An edit that names
 * you where the words before it did not IS naming you, so it arrives at the
 * top, dated by the edit — the row's time is always when you were named, which
 * is what its place is ordered by. A delete takes the mention away, as an erase
 * and a prune do: the room keeps a tombstone because a thread hangs off it, and
 * a message taken back asks nothing and has no words left to show here.
 *
 * ITS CURSOR IS A LOG POSITION where a transcript's is a room's own message
 * number, and the two are not interchangeable. A mention's position is the
 * record that named this handle and it never moves, so paging through this feed
 * spans a reanchor with no gap and no repeat. It is carried back unchanged and
 * this screen has an opinion about neither.
 *
 * WHERE A MENTION IS NEVER MOVES; WHAT IT SAYS DOES, AND WHETHER IT IS THERE.
 * A page the reader paged back to is read again when a live frame reaches it
 * ([pagesTouched]) — kept as first read, it drew an edited message's old words,
 * a deleted message's, or a message the company erased, for as long as the feed
 * was open; and a room joined makes the mentions said in it visible anywhere in
 * the feed.
 */

import { useCallback } from "react";

import { href } from "~/app/router.tsx";
import { usePageCoverage } from "~/app/Shell.tsx";
import { QueryState } from "~/components/common.tsx";
import { Button, Skeleton, Tag } from "@crewlethq/ui";
import { BellGlyph } from "@crewlethq/icons/glyphs";
import { NamedGlyph } from "@crewlethq/icons/glyphs/registry";
import { useClient } from "~/lib/store-hooks.ts";
import { usePaged, type PagesReached } from "~/lib/usePaged.ts";
import { useQuery, withFloor } from "~/lib/useQuery.ts";
import { fmtDateTime, relTime } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import type { ChatChange, ChatMention, ChatMentionsAnswer } from "~/protocol/index.ts";

import { useChatFrames } from "./live.ts";
import { coverageOf, PAGE } from "./page.ts";
import { roomMark } from "./rooms.ts";

/** The question the feed is, newest page first. */
const ASKED = { limit: PAGE };

const mentionsOf = (answer: ChatMentionsAnswer) => answer.mentions ?? [];
const idOf = (mention: ChatMention) => mention.message_id;
const cursorOf = (answer: ChatMentionsAnswer) => answer.next_cursor ?? "";

/**
 * Which held pages of the feed a run of frames changed.
 *
 * A FRAME NAMING A MESSAGE ON ONE — an edit rewrites the excerpt and marks it
 * edited, or takes the mention away when it no longer names this person, and a
 * delete takes it away; a reaction is not drawn here, and a post, a room's
 * creation or its settings change no mention already held — a new one, an
 * edit's included, arrives at the top. AN
 * ERASE OR A PRUNE takes rows from its own room, so it reaches a page holding
 * any mention said there. A MEMBERSHIP CHANGE can move which private rooms'
 * mentions this person may see at all, anywhere in the feed — a page read
 * again after it can hold mentions it never held, which `usePaged` reads the
 * range under before drawing — and an op this build does not know has a reach
 * it cannot say: both reach every page, as frames let go of (`null`) do.
 *
 * NEVER THE FIRST PAGE, which the feed reads again on every frame itself.
 */
function pagesTouched(frames: readonly ChatChange[] | null): PagesReached<ChatMentionsAnswer> {
  return (answer, place) => {
    if (place.first) return false;
    if (frames === null) return true;
    const held = mentionsOf(answer);
    return frames.some((frame) => {
      switch (frame.op) {
        case "post":
        case "create":
        case "patch":
        case "react":
          return false;
        case "edit":
        case "delete":
          return (
            !frame.message_id || held.some((mention) => mention.message_id === frame.message_id)
          );
        case "erase":
        case "prune":
          return held.some((mention) => mention.channel_id === frame.channel_id);
        default:
          return true;
      }
    });
  };
}

export function Mentions({ nameOf }: { nameOf: (handle: string) => string }) {
  const { socket } = useClient();
  const now = useNow();

  // THE HISTORY EVERY SCREEN PAGES (`usePaged`): the newest page is an
  // ordinary question, re-read live, and every older one is asked when the
  // reader asks for it, at this tab's read floor. Kept across a re-read of
  // the newest rather than thrown away with it: a mention's position never
  // moves, so a page below the newest is still the page it was — and a feed
  // that dropped what the reader had paged back to on every frame anywhere in
  // the company was one nobody could read the bottom of. What a new mention
  // pushes off the newest page's foot is read back into the gap before the
  // feed moves, so the list stays whole (`usePaged`'s join).
  const first = useQuery("chat_mentions", ASKED);
  // THE FEED'S OWN COVERAGE, as every view publishes its answer's.
  usePageCoverage(coverageOf(first.data));
  const page = useCallback(
    (cursor: string) =>
      socket.query(
        "chat_mentions",
        withFloor("chat_mentions", JSON.stringify({ ...ASKED, cursor })),
      ) as Promise<ChatMentionsAnswer>,
    [socket],
  );
  const feed = usePaged(first, ASKED, page, mentionsOf, idOf, cursorOf);
  // A MENTION ANYWHERE IS A FRAME SOMEWHERE, so the newest page re-reads on
  // every frame in the company, coalesced — a burst in one room is one change
  // here. A frame's `mentions` cannot narrow that to the frames naming this
  // person, for two reasons. It is copied from the record's routing snapshot,
  // and a system line's post writes its mention rows with no snapshot at all —
  // as a delete, an erase, a prune and a membership change carry none. And
  // the newest page changes through frames that do not name this person: an
  // edit that drops them takes the mention away, a delete, an erase or a prune
  // takes it with the words, and a membership change moves which private
  // rooms' mentions they may see.
  // A page held below it is read again only where a frame names something on
  // it ([pagesTouched]).
  useChatFrames(null, (frames) => {
    first.refetch();
    feed.refresh(pagesTouched(frames));
  });

  return (
    <div className="chat-list">
      <header className="chat-head">
        <div className="row gap-2">
          <BellGlyph size="sm" />
          <strong className="t-cell">Mentions</strong>
        </div>
        <p className="t-caption muted">
          Where you were named. The engine sends no email, push or text message, so this is where to
          look.
        </p>
      </header>

      <div className="chat-list-body">
        {feed.loading && !first.data && <Skeleton variant="text" rows={6} label="Reading" />}

        <QueryState
          error={feed.error}
          refusal={feed.refusal}
          detail={first.detail}
          loading={feed.loading}
          empty={
            feed.items.length
              ? undefined
              : {
                  title: "Nobody has named you",
                  hint: "A mention is a message that names your handle, like @you. A message to a whole room, or a reply in your thread, counts only when it names you.",
                }
          }
        >
          <div className="list">
            {feed.items.map((mention) => (
              <MentionRow key={mention.message_id} mention={mention} nameOf={nameOf} now={now} />
            ))}
          </div>
          {feed.pageError && <QueryState error={feed.pageError} refusal={null} loading={false} />}
          {feed.more && (
            <Button variant="ghost" onClick={feed.next} loading={feed.paging}>
              Older mentions
            </Button>
          )}
        </QueryState>
      </div>
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
    ["chatlet", mention.channel_id],
    mention.thread_root ? { thread: mention.thread_root } : undefined,
  );
  return (
    <a className="chat-hit" href={to}>
      <span className="row gap-2">
        <NamedGlyph name={roomMark(mention.channel_kind)} size="xs" />
        <strong className="truncate t-cell">
          {mention.channel_name ? `#${mention.channel_name}` : "a direct conversation"}
        </strong>
        {mention.thread_root && <Tag appearance="outline">in a thread</Tag>}
        <span className="spacer" />
        {/* WHEN YOU WERE NAMED, which is what the feed is ordered by — the
            edit's instant for a mention an edit made. The mark says the words
            are not the ones first said, as the room's own row does. */}
        {mention.edited_at && (
          <span className="t-caption muted" title={fmtDateTime(mention.edited_at)}>
            edited
          </span>
        )}
        <span className="t-caption muted" title={fmtDateTime(mention.at)}>
          {relTime(mention.at, now)}
        </span>
      </span>
      <span className="t-caption">
        {mention.author ? `${nameOf(mention.author)}: ` : ""}
        {mention.excerpt || "…"}
      </span>
    </a>
  );
}
