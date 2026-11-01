/**
 * Searching Chatlet: messages, people and rooms, as the reader types.
 *
 * # Messages run over the rooms the reader may READ, which is wider than their rail
 *
 * A public room and a unit's room are readable by any seat, joined or not, so
 * a search driven off somebody's membership would answer nothing from exactly
 * the rooms a company does most of its talking in, silently, because an empty
 * result reads as "nobody said that". The set is computed by the SERVER from
 * the viewer it resolved, and a request may only NARROW it: to one room (In)
 * and to one author (From), both answered by the engine over its whole index
 * rather than by filtering a page here.
 *
 * So the answer carries the DENOMINATOR: how many rooms it actually ran over.
 * "Nothing matched" over three rooms and over three hundred are different
 * answers, and this screen says which it got.
 *
 * # As the reader types, once they pause
 *
 * Each question is a real ranking over this node's own index, so it is asked
 * once the words have rested (`useQuiet`), not on every key: a pause asks and
 * a word being typed does not. The settled words go into the address by
 * replacement, so a search is a link somebody can hand on and the back button
 * is not filled with every prefix of the phrase. A phrase past the engine's
 * bound is not sent, and the screen says why (`useSearchQuery`).
 *
 * # People and rooms are what the reader already holds
 *
 * People are the org chart's seats (`people.ts`), and rooms are every room the
 * reader may read (`chat_rooms`, the list Browse rooms draws): both are
 * matched here on the words a row draws, so they need no ranking.
 *
 * # Keyword, and deliberately nothing else
 *
 * Chatlet has its own lexical index and no semantic search: a message is a
 * remark about work rather than a statement of it, and mixing the corpora
 * would let one busy conversation outrank every page written on purpose.
 */

import { useEffect, useId, useMemo, useRef, useState } from "react";

import {
  Callout,
  EmptyState,
  Highlight,
  Input,
  SegmentedControl,
  Select,
  TabPanel,
  Tabs,
  Tag,
  highlightTerms,
  Skeleton,
} from "@crewlethq/ui";
import { SearchGlyph } from "@crewlethq/icons/glyphs";

import { useSearchTarget } from "~/app/searchTarget.ts";
import { usePageCoverage } from "~/app/Shell.tsx";
import { QueryState } from "~/components/common.tsx";
import { CHAT_SEARCH_PAGE } from "~/contract/chat.ts";
import { useNow } from "~/lib/clock.ts";
import { relTime } from "~/lib/format.ts";
import { useQuiet } from "~/lib/useQuiet.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useSearchQuery } from "~/lib/useSearchQuery.ts";
import type { ChatHit } from "~/protocol/index.ts";

import { BrowseRow, matchesRoom } from "./Browse.tsx";
import { ChatFace } from "./Face.tsx";
import { useChatFrame, useChatNav } from "./nav.tsx";
import { matchesPerson, useChatPeople, type ChatPerson } from "./people.ts";
import { roomTitle, roomWhere } from "./rooms.ts";
import { SuggestedPerson } from "./Dialogs.tsx";
import { ViewHead } from "./Views.tsx";

type Scope = "messages" | "people" | "rooms";
type Who = "all" | "people" | "agents";

const SCOPE_LABEL: Record<Scope, string> = {
  messages: "Messages",
  people: "People",
  rooms: "Rooms",
};

/**
 * What a full page of hits says beneath them. A RANKING CUT AT ITS PAGE IS NOT
 * "EVERYTHING THAT MATCHED": the engine answers at most [CHAT_SEARCH_PAGE] hits
 * and has no next page, so a full one may have more behind it.
 */
export const FULL_PAGE = `These are the ${CHAT_SEARCH_PAGE} best matches, and there may be more. Add a word, or pick a room or a person, to narrow it.`;

/** Whether a seat is one the Who filter keeps. */
export function keptBy(who: Who, person: Pick<ChatPerson, "kind">): boolean {
  return who === "all" || (who === "agents") === (person.kind === "agent");
}

export function SearchView() {
  const nav = useChatNav();
  const frame = useChatFrame();
  const people = useChatPeople();
  const now = useNow();
  const panel = useId();
  const box = useRef<HTMLInputElement>(null);
  // `/` SEARCHES WHAT THE READER IS LOOKING AT, which here is this box.
  useSearchTarget(box);
  // NOTHING ON THE STATE BAR: a ranking over THIS NODE'S OWN index has no
  // read level to state.
  usePageCoverage(undefined);

  const place = nav.place.kind === "search" ? nav.place : { q: undefined, room: undefined };
  const [draft, setDraft] = useState(place.q ?? "");
  const [room, setRoom] = useState(place.room ?? "");
  const [from, setFrom] = useState("");
  const [who, setWho] = useState<Who>("all");
  const [scope, setScope] = useState<Scope>("messages");
  const q = useQuiet(draft.trim());

  // THE SETTLED SEARCH IS THE ADDRESS, replaced rather than pushed.
  useEffect(() => {
    if ((place.q ?? "") === q && (place.room ?? "") === room) return;
    nav.go({ kind: "search", q: q || undefined, room: room || undefined }, { replace: true });
  }, [q, room, place.q, place.room, nav]);

  const answer = useSearchQuery(
    "chat_search",
    {
      q,
      limit: CHAT_SEARCH_PAGE,
      ...(room ? { channel_id: room } : {}),
      ...(from ? { author: from } : {}),
    },
    { enabled: q.length > 0 },
  );
  const terms = highlightTerms(q);
  // THE WHO FILTER NARROWS THE PAGE THE ENGINE RANKED, by the kind of each
  // hit's author: the engine takes one author, not a kind, so the page is
  // said to be the best matches it is (FULL_PAGE) rather than all of them.
  const hits = (answer.data?.hits ?? []).filter(
    (hit) => !hit.author || keptBy(who, people.person(hit.author)),
  );

  // EVERY ROOM THE READER MAY READ, which on this screen is wider than their
  // rail: a search runs over public and team rooms they have not joined, and
  // a hit in one of them is named from this list, as the Rooms tab is drawn.
  const directory = useQuery("chat_rooms", {}, { enabled: q !== "" });
  const rooms = useMemo(
    () => new Map((directory.data?.rooms ?? []).map((room) => [room.channel.id, room])),
    [directory.data],
  );
  const whereOf = (id: string) => {
    const found = frame.channels.find((summary) => summary.channel.id === id);
    const channel = found?.channel ?? rooms.get(id)?.channel;
    // A ROOM NEITHER LIST HOLDS IS STILL A HIT: the engine ranked it.
    if (!channel) return "a room";
    const title = roomTitle(channel, {
      participants: found?.participants,
      viewer: frame.viewer,
      nameOf: people.nameOf,
    });
    return roomWhere(channel.kind, title);
  };

  const peopleHits = q
    ? people.everyone.filter((person) => keptBy(who, person) && matchesPerson(person, q))
    : [];
  const roomHits = q ? [...rooms.values()].filter((room) => matchesRoom(room, q)) : [];
  const counts: Record<Scope, number> = {
    messages: hits.length,
    people: peopleHits.length,
    rooms: roomHits.length,
  };
  const inRoom = room ? whereOf(room) : "";

  const open = (hit: ChatHit) =>
    hit.thread_root
      ? nav.openThread(hit.channel_id, hit.thread_root)
      : nav.openRoom(hit.channel_id, { message: hit.message_id });

  return (
    <section className="chat-view">
      <ViewHead title="Search" />
      <div className="chat-search-bar">
        <Input
          ref={box}
          type="search"
          width="full"
          autoFocus
          leading={<SearchGlyph size="sm" />}
          placeholder={room ? `Search in ${inRoom}` : "Search messages, people and rooms"}
          aria-label="Search Chatlet"
          value={draft}
          onChange={(event) => setDraft(event.target.value)}
          onClear={() => setDraft("")}
        />
        <div className="chat-search-filters">
          {scope === "messages" && (
            <>
              <Select
                size="sm"
                width="auto"
                ariaLabel="In"
                value={room}
                onChange={(value) => setRoom(String(value))}
                options={[
                  { value: "", label: "In: any room" },
                  ...frame.channels.map((summary) => ({
                    value: summary.channel.id,
                    label: `In: ${whereOf(summary.channel.id)}`,
                  })),
                ]}
              />
              <Select
                size="sm"
                width="auto"
                ariaLabel="From"
                value={from}
                onChange={(value) => setFrom(String(value))}
                options={[
                  { value: "", label: "From: anyone" },
                  ...people.everyone.map((person) => ({
                    value: person.handle,
                    label: `From: ${person.name}`,
                    leading: <ChatFace person={person} size="xs" status={false} />,
                  })),
                ]}
              />
            </>
          )}
          {scope !== "rooms" && (
            <SegmentedControl
              label="Who"
              semantics="radio"
              size="sm"
              value={who}
              onValueChange={(value) => setWho(value)}
              options={[
                { value: "all", label: "Everyone" },
                { value: "people", label: "People" },
                { value: "agents", label: "Agents" },
              ]}
            />
          )}
        </div>
      </div>
      <Tabs
        className="chat-view-tabs"
        variant="underline"
        size="sm"
        ariaLabel="Results"
        panelId={panel}
        value={scope}
        onValueChange={(value) => setScope(value as Scope)}
        items={(["messages", "people", "rooms"] as const).map((value) => ({
          value,
          label: SCOPE_LABEL[value],
          count: q ? counts[value] : null,
        }))}
      />
      <TabPanel id={panel} value={scope}>
        <div className="chat-view-scroll">
          {!q ? (
            <p className="chat-pane-note">
              Search covers every room you can read, including public and team rooms you
              haven&rsquo;t joined. Results appear as you type.
            </p>
          ) : scope === "messages" ? (
            <Messages
              answer={answer}
              hits={hits}
              terms={terms}
              room={room}
              whereOf={whereOf}
              open={open}
              now={now}
            />
          ) : counts[scope] === 0 ? (
            <EmptyState
              size="compact"
              icon={<SearchGlyph size="xl" />}
              title={`No ${SCOPE_LABEL[scope].toLowerCase()} match`}
              description="Try fewer words, or another spelling."
            />
          ) : scope === "people" ? (
            <ul className="chat-pick-list">
              {peopleHits.map((person) => (
                <li key={person.handle}>
                  <SuggestedPerson
                    person={person}
                    onSelect={() => nav.openPane({ kind: "profile", handle: person.handle })}
                  />
                </li>
              ))}
            </ul>
          ) : (
            <ul className="chat-browse-list">
              {roomHits.map((found) => (
                <BrowseRow key={found.channel.id} room={found} />
              ))}
            </ul>
          )}
        </div>
      </TabPanel>
    </section>
  );
}

/** The Messages tab: the engine's ranking, said honestly. */
function Messages({
  answer,
  hits,
  terms,
  room,
  whereOf,
  open,
  now,
}: {
  answer: ReturnType<typeof useSearchQuery<"chat_search">>;
  hits: ChatHit[];
  terms: string[];
  room: string;
  whereOf: (id: string) => string;
  open: (hit: ChatHit) => void;
  now: number;
}) {
  const people = useChatPeople();
  if (answer.tooLong) return <Callout variant="warning">{answer.tooLong}</Callout>;
  if (answer.withheld) return <Callout variant="info">{answer.withheld}</Callout>;
  const data = answer.data;
  return (
    <>
      {data?.building && (
        // A PARTIAL INDEX IS NOT "NOBODY SAID THAT".
        <Callout variant="warning">
          Still indexing, so these results may be missing some messages. Search again shortly.
        </Callout>
      )}
      {answer.loading && !data && <Skeleton variant="text" rows={5} label="Ranking" />}
      {data && data.searched === 0 && (
        // NO ROOMS RAN, which is not "nothing matched".
        <Callout variant="neutral">
          {room
            ? "You can’t read the room this search is narrowed to, so nothing was searched."
            : "There are no rooms you can read, so nothing was searched."}
        </Callout>
      )}
      <QueryState
        error={answer.error}
        refusal={answer.refusal}
        detail={answer.detail}
        loading={answer.loading}
        empty={
          data && hits.length === 0 && data.searched > 0 && !data.building
            ? {
                title: "No messages match",
                hint: `Searched ${data.searched} room${data.searched === 1 ? "" : "s"} you can read. Try fewer words, or widen In and From.`,
              }
            : undefined
        }
      >
        {hits.map((hit) => {
          const author = hit.author ? people.person(hit.author) : null;
          return (
            <button
              key={hit.message_id}
              type="button"
              className="chat-hit"
              onClick={() => open(hit)}
            >
              {author ? <ChatFace person={author} size={32} status={false} /> : <span />}
              <span className="chat-hit__text">
                <span className="chat-hit__head">
                  <span className="chat-hit__name">{author?.name ?? "Somebody"}</span>
                  <span className="chat-muted-text">
                    {whereOf(hit.channel_id)}
                    {hit.thread_root ? " · in a thread" : ""} · {relTime(hit.at, now)}
                  </span>
                </span>
                <Highlight className="chat-hit__body" text={hit.snippet ?? ""} terms={terms} />
              </span>
            </button>
          );
        })}
        {data && hits.length > 0 && (
          <p className="chat-pane-note">
            Searched {data.searched} room{data.searched === 1 ? "" : "s"}.
            {data.hits.length >= CHAT_SEARCH_PAGE && ` ${FULL_PAGE}`}
          </p>
        )}
      </QueryState>
      {data && hits.length < data.hits.length && (
        <p className="chat-pane-note">
          <Tag size="xs">Filtered</Tag> Showing the {hits.length} of the best {data.hits.length}{" "}
          matches the Who filter keeps.
        </p>
      )}
    </>
  );
}
