/**
 * Searching the company's conversation.
 *
 * # It runs over the rooms the reader may READ, which is wider than their rail
 *
 * A public room and a unit's room are readable by any seat, joined or not, so
 * a search driven off somebody's membership would answer nothing from exactly
 * the rooms a company does most of its talking in — silently, because an empty
 * result reads as "nobody said that". The set is computed by the SERVER from
 * the viewer it resolved, and a request may only NARROW it to one room.
 *
 * So the answer carries the DENOMINATOR: how many rooms it actually ran over.
 * "Nothing matched" over three rooms and over three hundred are different
 * answers, and this screen says which it got.
 *
 * # Keyword, and deliberately nothing else
 *
 * Chatlet has its own lexical index and no semantic search at all: a message is a
 * remark about work rather than a statement of it, and mixing the corpora
 * would let one busy conversation outrank every page written on purpose.
 * Ranking here is BM25 tuned for a bimodal corpus — one-line acknowledgements
 * beside pasted blocks — so this box takes words rather than a query language.
 */

import { useMemo, useRef, useState } from "react";

import { href, useParam } from "~/app/router.tsx";
import { useSearchTarget } from "~/app/searchTarget.ts";
import { usePageCoverage } from "~/app/Shell.tsx";
import { QueryState } from "~/components/common.tsx";
import { Button, Callout, Input, Select, Skeleton } from "@crewlethq/ui";
import { XGlyph, SearchGlyph } from "@crewlethq/icons/glyphs";
import { useSearchQuery } from "~/lib/useSearchQuery.ts";
import { fmtDateTime } from "~/lib/format.ts";
import type { ChatChannelSummary } from "~/protocol/index.ts";

import { CHAT_SEARCH_PAGE } from "~/contract/chat.ts";

import { MAX_BROWSE_ROOMS, useRoomDetails } from "./directory.ts";
import { isDirect } from "~/lib/chatRooms.ts";
import { roomTitle } from "./rooms.ts";

export function Search({
  channels,
  viewer,
  nameOf,
}: {
  channels: ChatChannelSummary[];
  viewer: string;
  nameOf: (handle: string) => string;
}) {
  // THE QUERY IS IN THE ADDRESS and the draft is not: a search somebody wants
  // to hand to a colleague is a URL, and a keystroke is not a place anybody
  // has been. `filter` rather than `section`, so typing does not fill the back
  // button with every prefix of the phrase.
  const [q, setQ] = useParam("q", "");
  const [room, setRoom] = useParam("in", "");
  const [draft, setDraft] = useState(q);
  // `/` SEARCHES WHAT THE READER IS LOOKING AT, which here is this box — the
  // screen's own search — rather than the palette over it.
  const searchBox = useRef<HTMLInputElement>(null);
  useSearchTarget(searchBox);

  // ON SUBMIT rather than on every keystroke: each of these is a real ranking
  // over this node's own index, and a search-as-you-type would run one per
  // letter for an answer nobody reads until they stop. A PHRASE PAST THE
  // ENGINE'S BOUND IS NOT SENT, and the screen says why in place of the
  // results — see `useSearchQuery`. THE PAGE IS THE ENGINE'S OWN
  // ([CHAT_SEARCH_PAGE]), so a full one is told from the whole answer.
  const answer = useSearchQuery(
    "chat_search",
    { q, limit: CHAT_SEARCH_PAGE, ...(room ? { channel_id: room } : {}) },
    { enabled: q.trim().length > 0 },
  );

  // THE ROOMS THE RAIL DOES NOT HOLD, which on this screen is most of them: a
  // search runs over every room the viewer may READ, and the public and unit
  // rooms they have not joined are exactly the ones a rail lookup misses. Left
  // to the rail alone every hit in one of them was titled "a room" — which is
  // this screen's own promise failing quietly, since finding something said
  // somewhere you are not is what it is for. Bounded by [MAX_BROWSE_ROOMS] for
  // the reason that constant states: one wave of the socket's in-flight
  // queries (four), so the transcript and the rail never queue behind more
  // than one of these titles.
  const outside = useMemo(() => {
    const rail = new Set(channels.map((summary) => summary.channel.id));
    const ids = new Set(
      (answer.data?.hits ?? []).map((hit) => hit.channel_id).filter((id) => !rail.has(id)),
    );
    return [...ids].slice(0, MAX_BROWSE_ROOMS);
  }, [answer.data, channels]);
  const resolved = useRoomDetails(outside);
  // NOTHING ON THE STATE BAR: a ranking over THIS NODE'S OWN index has no
  // read level to state, as the work search's has none — and the rail's, which
  // this screen also holds, describes a list the reader is not looking at.
  usePageCoverage(undefined);

  const titleOf = (id: string) => {
    const found = channels.find((summary) => summary.channel.id === id);
    const channel = found?.channel ?? resolved.rooms.get(id)?.channel;
    // A ROOM NEITHER LIST HOLDS IS STILL A HIT. The read that would name it
    // may not have come back yet, may have been refused, or may not have
    // been answered just now — none of which says anything about whether the
    // message is real: the engine ranked it, so it is.
    if (!channel) return "a room";
    const title = roomTitle(channel, { participants: found?.participants, viewer, nameOf });
    return isDirect(channel.kind) ? title : `#${title}`;
  };

  return (
    <div className="chat-list">
      <form
        className="toolbar"
        onSubmit={(event) => {
          event.preventDefault();
          setQ(draft.trim());
        }}
      >
        <div style={{ flex: 1, minWidth: 0 }}>
          <Input
            type="search"
            width="full"
            ref={searchBox}
            value={draft}
            onChange={(event) => setDraft(event.target.value)}
            aria-label="Search the company's chat"
            leading={<SearchGlyph size="sm" />}
            placeholder="Search what’s been said"
          />
        </div>
        <Select
          aria-label="Which room"
          value={room}
          onChange={(value) => setRoom(String(value))}
          options={[
            { value: "", label: "Every room you can read" },
            ...channels.map((summary) => ({
              value: summary.channel.id,
              label: titleOf(summary.channel.id),
            })),
          ]}
        />
        <Button variant="primary" type="submit" leadingIcon={<SearchGlyph size="sm" />}>
          Search
        </Button>
        {q && (
          <Button
            variant="secondary"
            leadingIcon={<XGlyph size="sm" />}
            onClick={() => {
              setDraft("");
              setQ("");
            }}
          >
            Clear
          </Button>
        )}
      </form>

      <div className="chat-list-body">
        {!q && (
          <p className="t-caption muted measure">
            This searches every room you can read, including public and team rooms you haven&rsquo;t
            joined. Chatlet is searched apart from the knowledge base, so a busy conversation never
            outranks a page.
          </p>
        )}

        {answer.tooLong && <Callout variant="warning">{answer.tooLong}</Callout>}
        {answer.withheld && <Callout variant="info">{answer.withheld}</Callout>}

        {q && answer.data?.building && (
          // A PARTIAL INDEX IS NOT "NOBODY SAID THAT". The answering node's
          // index had not caught up with its transcript, so the ranking is
          // over part of it — said once, above whatever it did find.
          <Callout variant="warning">
            Still indexing, so these results may be missing some messages. Search again shortly.
          </Callout>
        )}

        {answer.loading && !answer.data && <Skeleton variant="text" rows={5} label="Ranking" />}

        {q && answer.data && answer.data.searched === 0 && (
          // NO ROOMS RAN, which is not "nothing matched": narrowing to a room
          // this reader may not see answers over nothing at all, and so does a
          // reader who may genuinely read nothing.
          <Callout variant="neutral">
            {room
              ? "You can’t read the room this search is narrowed to, so nothing was searched."
              : "There are no rooms you can read, so nothing was searched."}
          </Callout>
        )}

        {q && !answer.tooLong && !answer.withheld && (
          <QueryState
            error={answer.error}
            refusal={answer.refusal}
            detail={answer.detail}
            loading={answer.loading}
            empty={
              answer.data &&
              answer.data.hits.length === 0 &&
              answer.data.searched > 0 &&
              !answer.data.building
                ? {
                    title: `Nothing matched “${q}”`,
                    hint: `Searched ${answer.data.searched} room${answer.data.searched === 1 ? "" : "s"} you can read.`,
                  }
                : undefined
            }
          >
            <div className="list">
              {(answer.data?.hits ?? []).map((hit, index) => (
                <a
                  key={hit.message_id}
                  className="chat-hit"
                  href={href(
                    ["chatlet", hit.channel_id],
                    hit.thread_root ? { thread: hit.thread_root } : undefined,
                  )}
                >
                  <span className="row gap-2">
                    <span className="rank-place">{index + 1}</span>
                    <strong className="truncate t-cell">{titleOf(hit.channel_id)}</strong>
                    <span className="spacer" />
                    <span className="t-caption muted">{fmtDateTime(hit.at)}</span>
                  </span>
                  <span className="t-caption">
                    {hit.author ? `${nameOf(hit.author)}: ` : ""}
                    {hit.snippet}
                  </span>
                </a>
              ))}
            </div>
            {answer.data && answer.data.hits.length > 0 && (
              <p className="t-caption muted">
                Searched {answer.data.searched} room
                {answer.data.searched === 1 ? "" : "s"}.
                {answer.data.hits.length >= CHAT_SEARCH_PAGE && ` ${FULL_PAGE}`}
              </p>
            )}
            {resolved.unreadable.size > 0 && (
              // A NAME THAT COULD NOT BE READ JUST NOW IS SAID, and asked for
              // again, rather than left as "a room" as if that were the answer.
              <p className="row wrap gap-2 t-caption muted">
                {resolved.unreadable.size} room{resolved.unreadable.size === 1 ? "’s" : "s’"} name
                couldn&rsquo;t be read just now, so{" "}
                {resolved.unreadable.size === 1 ? "it shows" : "they show"} as “a room”.
                <Button size="small" variant="ghost" onClick={resolved.retry}>
                  Ask again
                </Button>
              </p>
            )}
          </QueryState>
        )}
      </div>
    </div>
  );
}

/**
 * What a full page of hits says beneath them.
 *
 * A RANKING CUT AT ITS PAGE IS NOT "EVERYTHING THAT MATCHED": the engine
 * answers at most [CHAT_SEARCH_PAGE] hits and has no next page, so a full one
 * may have more behind it, and the move is a narrower question.
 */
export const FULL_PAGE = `These are the ${CHAT_SEARCH_PAGE} best matches, and there may be more. Add a word, or pick a room, to narrow it.`;
