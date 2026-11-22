/**
 * Ask Crewlet's page: the reader's conversations beside the one open, at
 * `#/ask/{id}`, or a new one at `#/ask`.
 *
 * # Anatomy
 *
 * The LIST is the reader's own history: New conversation, a search of the
 * titles, and every conversation newest first under the day it was last used
 * (Today, Yesterday, Previous 7 days, Older), each a row with its title, when
 * it was used, a mark while a turn in it runs or waits on the reader, and a
 * menu of Rename and Delete. Older ones are read a page at a time.
 *
 * The CONVERSATION is the one the drawer draws (`Conversation.tsx`), under a
 * head with its title and its actions (the model, Rename, Delete). The page
 * attaches no page to a question: it is the conversation, not a screen the
 * conversation is about.
 *
 * A conversation moved or deleted in another tab is told here (`session.ts`):
 * the list is read again, and the open one is read again or says it was deleted.
 *
 * # One conversation, two places
 *
 * The page and the drawer are views of the one store (`session.ts`), so a
 * turn streaming in one is the same turn in the other. The frame draws no
 * drawer over this page, and the drawer's Open in full page opens this one on
 * its conversation.
 *
 * # The address names the conversation
 *
 * `#/ask/{id}` opens that conversation; a conversation this page's first
 * question creates is written into the address, replacing `#/ask`, so a
 * reload and a link both come back to it. `#/ask` alone opens the one this
 * tab was on, or a new one.
 *
 * Where the screen is a phone's width the list and the conversation are one
 * pane each, as every list beside its detail is (`ListDetail`): the
 * conversation, with Back to the list.
 */

import { useEffect, useState, type MouseEvent } from "react";
import {
  Button,
  Callout,
  EmptyState,
  IconButton,
  InboxGroup,
  InboxList,
  InboxRow,
  Input,
  ListDetail,
  ListDetailBack,
  Menu,
  MessageAction,
  MessageActions,
  Skeleton,
  Tag,
  announce,
  useScreenBelow,
} from "@crewlethq/ui";
import {
  EllipsisGlyph,
  PencilGlyph,
  SearchGlyph,
  SquarePenGlyph,
  TrashGlyph,
} from "@crewlethq/icons/glyphs";
import { useFillScreen } from "~/app/fill.tsx";
import { PHONE_BREAKPOINT } from "~/app/layout.ts";
import { href, useNavigator } from "~/app/router.tsx";
import { usePageLabels } from "~/app/Shell.tsx";
import { useNow } from "~/lib/clock.ts";
import { relTime } from "~/lib/format.ts";
import { useAskReading } from "../place.ts";
import { readTab } from "../tab.ts";
import {
  ASK_PAGE_PATH,
  AskDeleteConfirm,
  AskFoot,
  AskProvider,
  AskRename,
  AskThread,
  useAskGestures,
} from "./Conversation.tsx";
import { HISTORY_QUERY_MAX, grouped, useHistory, type HistoryRow } from "./history.ts";
import {
  conversation as conversationNow,
  newConversation,
  openConversation,
  useMovedElsewhere,
} from "./session.ts";
import { isOpen, openTurn, type ConversationView } from "./turn.ts";

export function AskPage({ conversation }: { conversation?: string | undefined }) {
  const reading = useAskReading();
  // THE CONVERSATION SCROLLS INSIDE ITSELF ABOVE A COMPOSER THAT STAYS, at
  // every width, as a chat room does (`app/fill.tsx`): on a phone too, where a
  // page that grew with the thread left the composer under a short one and
  // the window's foot empty. Flush to the sheet's edges, as a conversation runs.
  useFillScreen(true, { flush: true });
  const [deleted, setDeleted] = useState("");
  return (
    <AskProvider place={{ ...reading, withPage: false, onDeleted: setDeleted }}>
      <Page conversation={conversation} deleted={deleted} />
    </AskProvider>
  );
}

/** What a conversation is called on the page: its title, or what it is while it has none. */
export function titleOf(view: Pick<ConversationView, "title" | "turns">): string {
  if (view.title.trim() !== "") return view.title;
  return view.turns.length === 0 ? "New conversation" : "Untitled conversation";
}

function Page({ conversation, deleted }: { conversation: string | undefined; deleted: string }) {
  const nav = useNavigator();
  const { view, askDelete, askRename, resetPage, deletable, composer } = useAskGestures();
  const reading = useAskReading();
  const offline = reading.unavailable !== null;
  const now = useNow();
  const [query, setQuery] = useState("");
  // ON ONE PANE, the list is shown once the reader asks for it (Back), and
  // the conversation otherwise: asking is what the page is for.
  const single = useScreenBelow(PHONE_BREAKPOINT);
  const [listing, setListing] = useState(false);

  // THE LIST IS READ AGAIN WHEN THE OPEN CONVERSATION MOVES (`history.ts`):
  // it was made, it was titled, a turn in it ended, or one was deleted; and
  // whenever another tab tells of one that moved.
  const ended = view.turns.filter((turn) => !isOpen(turn.phase)).length;
  const elsewhere = useMovedElsewhere();
  const history = useHistory(
    query,
    `${view.id ?? ""}\u0000${view.title}\u0000${ended}\u0000${deleted}\u0000${elsewhere}`,
  );

  // THE ADDRESS NAMES THE CONVERSATION: it is opened where the store holds
  // another, and is not opened again while it is being read. What came of
  // the read is the store's to say (`turn.ts`'s `Opening`).
  useEffect(() => {
    const held = conversationNow();
    const opening = held.opening;
    const reading = opening !== null && opening.id === conversation && opening.state === "reading";
    if (!conversation || held.id === conversation || reading) return;
    openConversation(conversation).catch(() => {});
  }, [conversation]);

  // NONE NAMED: the one this tab was on, else a new one.
  useEffect(() => {
    if (conversation) return;
    const held = conversationNow().id ?? readTab().conversation;
    if (held) nav.replace([...ASK_PAGE_PATH, held]);
  }, [conversation, nav]);

  // A CONVERSATION THIS PAGE'S QUESTION MADE is written into the address.
  // Keyed on the store's id alone: a row just chosen names its conversation
  // before the store holds it, and must not be sent back to the last one.
  useEffect(() => {
    if (view.id !== null && view.id !== conversation) nav.replace([...ASK_PAGE_PATH, view.id]);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [view.id]);

  const opening = view.opening !== null && view.opening.id === conversation ? view.opening : null;
  const known = history.rows?.find((row) => row.id === conversation)?.title?.trim() || null;
  const title =
    opening === null
      ? titleOf(view)
      : opening.state === "missing"
        ? "Conversation not found"
        : (known ?? (opening.state === "reading" ? "Opening conversation" : "Conversation"));
  usePageLabels(conversation ? { [conversation]: title } : {});

  const startNew = () => {
    newConversation();
    resetPage();
    setListing(false);
    nav.to([...ASK_PAGE_PATH]);
    announce("New conversation");
    requestAnimationFrame(() => composer.current?.focus());
  };
  const choose = (id: string) => {
    setListing(false);
    if (id !== conversation) nav.to([...ASK_PAGE_PATH, id]);
  };
  // THE CONVERSATION DELETED WAS THE ONE OPEN: the page goes to a new one.
  useEffect(() => {
    if (deleted !== "" && deleted === conversation) nav.replace([...ASK_PAGE_PATH]);
    if (deleted !== "") history.drop(deleted);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [deleted]);

  const list = (
    <div className="ask-page__list">
      <div className="ask-page__list-top">
        <div className="ask-page__list-head">
          <h2 className="ask-page__list-title">Conversations</h2>
          <Button
            size="small"
            variant="secondary"
            leadingIcon={<SquarePenGlyph size="sm" />}
            disabledReason={reading.unavailable?.short}
            onClick={startNew}
          >
            New conversation
          </Button>
        </div>
        <Input
          inputSize="md"
          leading={<SearchGlyph size="sm" />}
          placeholder="Find a conversation"
          aria-label="Find a conversation"
          maxLength={HISTORY_QUERY_MAX}
          value={query}
          onChange={(event) => setQuery(event.target.value)}
          onClear={() => setQuery("")}
        />
      </div>
      <HistoryList
        history={history}
        query={query}
        open={view.id ?? opening?.id ?? null}
        view={view}
        now={now}
        onOpen={choose}
        onRename={(row) => askRename(row.id, row.title ?? "")}
        onDelete={(row) => askDelete(row.id)}
      />
    </div>
  );

  return (
    <section className="ask-page" aria-label="Ask Crewlet">
      <ListDetail
        className="ask-page__split"
        list={list}
        showing={single && listing ? "list" : "detail"}
        flush
      >
        <div className="ask-page__open">
          <header className="ask-page__head">
            <ListDetailBack label="Back to conversations" onClick={() => setListing(true)} />
            <h2 className="ask-page__title">{title}</h2>
            <span className="ask-page__actions">
              {deletable ? (
                <>
                  <IconButton
                    label="Rename this conversation"
                    size="header"
                    variant="ghost"
                    icon={<PencilGlyph />}
                    tooltip
                    onClick={() => askRename()}
                  />
                  <IconButton
                    label="Delete this conversation"
                    size="header"
                    variant="ghost"
                    icon={<TrashGlyph />}
                    tooltip
                    onClick={() => askDelete()}
                  />
                </>
              ) : null}
            </span>
          </header>
          <div
            className={offline ? "ask-page__thread ask-page__thread--offline" : "ask-page__thread"}
          >
            <AskThread column onNew={startNew} />
          </div>
          {offline ? null : (
            <footer className="ask-page__foot">
              <AskFoot />
            </footer>
          )}
        </div>
      </ListDetail>
      <AskDeleteConfirm />
      <AskRename />
    </section>
  );
}

/** Where a conversation's turn stands, as its row says it: answering, waiting on the reader, or nothing. */
export function rowState(row: HistoryRow, view: ConversationView): string | null {
  // THE OPEN CONVERSATION'S OWN TURN is read off the store, which streams it;
  // the list's answer is as old as its last read.
  if (row.id === view.id) {
    const running = openTurn(view);
    if (running === null) return null;
    return running.phase === "awaiting_approval" ? "Waiting for you" : "Answering";
  }
  if (row.last_turn_state === "running") return "Answering";
  if (row.last_turn_state === "waiting_for_approval") return "Waiting for you";
  return null;
}

function HistoryList({
  history,
  query,
  open,
  view,
  now,
  onOpen,
  onRename,
  onDelete,
}: {
  history: ReturnType<typeof useHistory>;
  query: string;
  open: string | null;
  view: ConversationView;
  now: number;
  onOpen: (id: string) => void;
  onRename: (row: HistoryRow) => void;
  onDelete: (row: HistoryRow) => void;
}) {
  if (history.rows === null) {
    return history.failed ? (
      <div className="ask-page__list-note">
        <Callout variant="warning" title="Couldn't read your conversations">
          {history.failed}
        </Callout>
      </div>
    ) : (
      <div className="ask-page__list-note">
        <Skeleton variant="text" rows={6} label="Reading your conversations" />
      </div>
    );
  }
  if (history.rows.length === 0) {
    const searching = query.trim() !== "";
    return (
      <div className="ask-page__list-note">
        <EmptyState
          size="compact"
          title={searching ? "No conversation matches" : "No conversations yet"}
          description={
            searching
              ? "Try other words from a conversation's title."
              : "Ask a question, and the conversation is kept here for 30 days."
          }
        />
      </div>
    );
  }
  return (
    <>
      <InboxList label="Your conversations" density="chat">
        {grouped(history.rows, now).map(({ group, rows }) => (
          <InboxGroup key={group} label={group}>
            {rows.map((row) => {
              const name = row.title?.trim() || "Untitled conversation";
              const state = rowState(row, view);
              return (
                <InboxRow
                  key={row.id}
                  data-conversation={row.id}
                  title={name}
                  meta={
                    <time dateTime={row.last_activity_at}>
                      {relTime(row.last_activity_at, now)}
                    </time>
                  }
                  status={
                    state === null ? undefined : (
                      <Tag size="xs" variant={state === "Answering" ? "info" : "warning"}>
                        {state}
                      </Tag>
                    )
                  }
                  selected={row.id === open}
                  href={href([...ASK_PAGE_PATH, row.id])}
                  onClick={(event: MouseEvent<HTMLElement>) => {
                    if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
                    event.preventDefault();
                    onOpen(row.id);
                  }}
                  actions={
                    // ONE TOOL, A MENU (Rename, Delete): the row keeps room on
                    // its first line for its tray, so each tool more is a word
                    // fewer of the title beside it.
                    <MessageActions label={`Actions for ${name}`}>
                      <Menu
                        label={`More actions for ${name}`}
                        align="end"
                        renderTrigger={(trigger) => (
                          <MessageAction
                            {...trigger}
                            label="More actions"
                            icon={<EllipsisGlyph size="md" />}
                          />
                        )}
                        items={[
                          {
                            key: "rename",
                            label: "Rename",
                            icon: <PencilGlyph size="sm" />,
                            onSelect: () => onRename(row),
                          },
                          {
                            key: "delete",
                            label: "Delete",
                            icon: <TrashGlyph size="sm" />,
                            danger: true,
                            onSelect: () => onDelete(row),
                          },
                        ]}
                      />
                    </MessageActions>
                  }
                />
              );
            })}
          </InboxGroup>
        ))}
      </InboxList>
      {history.failed ? (
        <div className="ask-page__list-note">
          <Callout variant="warning">{history.failed}</Callout>
        </div>
      ) : null}
      {history.more ? (
        <div className="ask-page__list-more">
          <Button
            size="small"
            variant="ghost"
            disabledReason={history.loading ? "Reading older conversations" : undefined}
            onClick={history.older}
          >
            Show older conversations
          </Button>
        </div>
      ) : null}
    </>
  );
}
