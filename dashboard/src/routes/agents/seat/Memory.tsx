/**
 * What a seat remembers, and what it has said on surfaces the engine does not
 * own.
 *
 * # Read from the node that holds the seat
 *
 * A seat's memory is written to the store of the node running it, and a
 * compacted changelog carries it to whichever node holds the seat next — so
 * the holder's copy is the one kept current, and any other node's may be a
 * placement behind. Both answers here are the HOLDER'S (`held_by`), asked
 * across the fleet; a seat no node holds answers `none`, with nothing in it,
 * and the tab says why rather than drawing a seat that remembers nothing.
 *
 * # A header counts the whole, a list shows the newest
 *
 * Every header is the TOTAL the holder counted — never the length of the page
 * it sent, which is fifty at most — and every list cut short says "Latest 50
 * of 142" in its header, the same words on all four, so the two cannot
 * silently disagree.
 */

import { Callout, Card, CodeBlock, EmptyState, EmptyValue, Skeleton, Tag } from "@crewlethq/ui";
import {
  BookOpenGlyph,
  CircleAlertGlyph,
  ClockGlyph,
  LayersGlyph,
  MessageSquareGlyph,
  UsersGlyph,
  ZapGlyph,
} from "@crewlethq/icons/glyphs";
import { useLayoutEffect, useRef } from "react";
import { href, useParam } from "~/app/router.tsx";
import { screenScroller } from "~/lib/scroller.ts";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { DateCell, DurationCell, KeyCell, TextCell } from "~/app/frame/cells.tsx";
import { QueryState, RECORD_MAX_HEIGHT } from "~/components/common.tsx";
import { conversationLabel, fmtDateTime, plural, relTime, tsKey } from "~/lib/format.ts";
import { decisionLabel, decisionTone } from "~/lib/phases.ts";
import { plainText } from "~/lib/markdown.ts";
import { useQuery } from "~/lib/useQuery.ts";
import type { Seat } from "~/lib/seats.ts";
import { uiletTone } from "~/ui/primitives.tsx";
import type { CounterpartyProfile } from "~/contract/memory.ts";
import type { ConversationEntry } from "~/protocol/index.ts";

/**
 * The header's line for a list the holder cut at its page — "Latest 50 of
 * 142" — or null where the page holds every row, which the header's count
 * already says.
 */
export function pageWords(shown: number, total: number): string | null {
  if (total <= shown) return null;
  return `Latest ${shown.toLocaleString()} of ${total.toLocaleString()}`;
}

/** A card's subtitle: the cut, where there is one, then what the list is. */
function cutThen(cut: string | null, what: string): string {
  return cut ? `${cut} — ${what}` : what;
}

/** Who answered, as the sentence under the tab's head. */
export function heldWords(heldBy: string | undefined): string {
  if (!heldBy) return "";
  if (heldBy === "none") {
    return "No node holds this seat, so no copy of its memory is current and nothing is shown.";
  }
  return `Read from ${heldBy}, the node holding this seat — the copy kept current.`;
}

export function Memory({ seat, now }: { seat: Seat; now: number }) {
  const handle = seat.handle;
  const memory = useQuery("agent_memory", { id: handle }, { enabled: handle !== "" });
  const data = memory.data;
  return (
    <div className="col gap-4">
      {memory.loading && !data && (
        <Skeleton variant="text" rows={5} label="Loading this seat's memory" />
      )}
      <QueryState error={memory.error} loading={memory.loading && !data}>
        {data && (
          <>
            <p className="t-caption">{heldWords(data.held_by)}</p>
            <Card padding="none">
              <Card.Header
                icon={<BookOpenGlyph size="sm" />}
                count={data.diary_total}
                subtitle={cutThen(
                  pageWords(data.diary.length, data.diary_total),
                  "what it chose to remember",
                )}
              >
                <Card.Title as="h3">Diary</Card.Title>
              </Card.Header>
              {data.diary.length ? (
                <div className="list">
                  {data.diary.map((d) => (
                    <div key={d.id} className="thread-entry">
                      <div className="row gap-1">
                        <Tag appearance="outline">
                          {d.retention === "diary_short" ? "short-lived" : "kept"}
                        </Tag>
                        {d.retrievals > 0 && (
                          <span className="t-caption">recalled {plural(d.retrievals, "time")}</span>
                        )}
                        <span className="spacer" />
                        <span className="t-caption">{fmtDateTime(d.created_at)}</span>
                      </div>
                      <p className="t-body">{d.content}</p>
                    </div>
                  ))}
                </div>
              ) : (
                <EmptyState
                  size="compact"
                  icon={<BookOpenGlyph size={32} />}
                  title="Nothing written yet"
                  description="A seat keeps a note by calling reflect_and_persist during a turn, and the learning loop keeps what it judges worth remembering."
                />
              )}
            </Card>

            <Card padding="none">
              <Card.Header
                icon={<LayersGlyph size="sm" />}
                count={data.episodes_total}
                subtitle={cutThen(
                  pageWords(data.episodes.length, data.episodes_total),
                  "one per completed turn, recalled by similarity at turn start",
                )}
              >
                <Card.Title as="h3">Episodes</Card.Title>
              </Card.Header>
              <DataGrid
                name="episodes"
                rows={data.episodes}
                rowKey={(e) => e.id || e.turn_id || e.created_at}
                defaultSort="-at"
                empty={{
                  title: "No episodes recorded",
                  hint: "An episode is written when a turn completes.",
                }}
                columns={[
                  {
                    key: "at",
                    header: "When",
                    shrink: true,
                    // THROUGH `tsKey`, never `<` on the string: the engine
                    // trims trailing zeros, so a raw compare puts `:07Z`
                    // before `:07.42Z`.
                    sortValue: (e) => tsKey(e.created_at),
                    cell: (e) => <DateCell at={e.created_at} now={now} />,
                  },
                  {
                    key: "task",
                    header: "What it did",
                    cell: (e) =>
                      e.task_summary ? (
                        <TextCell>{e.task_summary}</TextCell>
                      ) : (
                        <EmptyValue label="The episode recorded no summary" />
                      ),
                  },
                  {
                    key: "outcome",
                    header: "Outcome",
                    shrink: true,
                    sortValue: (e) => e.review_outcome || null,
                    // THE REVIEWER'S DECISION, in the one table that tones
                    // it (`decisionTone`): `failed` is red here as it is on
                    // the turn it came from, where a two-way done-or-warning
                    // drew it amber.
                    cell: (e) =>
                      e.review_outcome ? (
                        <Tag
                          variant={uiletTone(decisionTone("review", e.review_outcome))}
                          title={decisionLabel("review", e.review_outcome)}
                        >
                          {e.review_outcome}
                        </Tag>
                      ) : (
                        <EmptyValue label="The turn ended without a review outcome" />
                      ),
                  },
                  {
                    key: "dur",
                    header: "Took",
                    align: "right",
                    shrink: true,
                    sortValue: (e) => e.duration_ms ?? null,
                    cell: (e) => <DurationCell ms={e.duration_ms} />,
                  },
                  {
                    key: "conv",
                    header: "Conversation",
                    // CONTENT-SIZED AND CAPPED, with a uuid cut to its head:
                    // flexible, a native task's `work:task:<uuid>` took the
                    // width and "What it did" — the column a reader scans —
                    // was cut at a third of the grid.
                    shrink: true,
                    cell: (e) =>
                      e.conversation_key ? (
                        <KeyCell
                          value={e.conversation_key}
                          text={conversationLabel(e.conversation_key)}
                        />
                      ) : (
                        <EmptyValue label="Not part of a conversation" />
                      ),
                  },
                ]}
              />
            </Card>

            <Card padding="none">
              <Card.Header
                icon={<ZapGlyph size="sm" />}
                count={data.skills_total}
                subtitle={cutThen(
                  pageWords(data.skills.length, data.skills_total),
                  "drafted from its own repeated work, loadable mid-turn",
                )}
              >
                <Card.Title as="h3">Skills it taught itself</Card.Title>
              </Card.Header>
              {data.skills.length ? (
                <div className="list">
                  {data.skills.map((s) => (
                    <div key={s.id || s.key} className="thread-entry">
                      <div className="row gap-1">
                        <strong className="t-body">{s.title}</strong>
                        <Tag appearance="outline">v{s.version}</Tag>
                        {s.uses > 0 && (
                          <span className="t-caption">used {plural(s.uses, "time")}</span>
                        )}
                        <span className="spacer" />
                        {s.updated_at && (
                          <span className="t-caption">{fmtDateTime(s.updated_at)}</span>
                        )}
                      </div>
                      {s.summary && <p className="t-caption">{s.summary}</p>}
                    </div>
                  ))}
                </div>
              ) : (
                <EmptyState
                  size="compact"
                  icon={<ZapGlyph size={32} />}
                  title="No learned skills"
                  description="The learning loop drafts these from repeated work. A young seat has none."
                />
              )}
            </Card>

            <Card padding="none">
              <Card.Header
                icon={<UsersGlyph size="sm" />}
                count={data.counterparties_total}
                subtitle={cutThen(
                  pageWords(data.counterparties.length, data.counterparties_total),
                  "what it believes about the people and seats it works with",
                )}
              >
                <Card.Title as="h3">Who it has worked with</Card.Title>
              </Card.Header>
              {data.counterparties.length ? (
                <div className="list">
                  {data.counterparties.map((c, i) => (
                    <CounterpartyRow key={`${counterpartyKey(c)}-${i}`} profile={c} now={now} />
                  ))}
                </div>
              ) : (
                <EmptyState
                  size="compact"
                  icon={<UsersGlyph size={32} />}
                  title="No counterparty profiles"
                  description="Built up from observed interactions with the people and seats it works with."
                />
              )}
            </Card>
          </>
        )}
      </QueryState>
      <Conversations seat={seat} now={now} />
    </div>
  );
}

/**
 * Every thread this seat holds a record in, and what it said there.
 *
 * The conversation ledger is what stops a seat replying twice in one thread —
 * the engine's only account of what a seat said on a surface it does not own.
 * WHICH THREAD IS OPEN is a filter (`conversation=`) rather than a section:
 * opening one replaces the history entry, so Back leaves the seat rather than
 * walking every thread the reader glanced at.
 *
 * # Choosing a thread shows it
 *
 * The list is a seat's every thread — twenty-odd rows, 1,700px drawn whole —
 * and choosing one is the section's only interaction, so what it opens has to
 * be on screen when it opens. Beside the list (a wide column) the list is
 * BOUNDED to the viewport and scrolls inside itself, and the detail is STICKY
 * under the page's top, so a thread chosen near the list's end opens beside
 * the row that was pressed rather than a screen above it. Stacked (a phone),
 * the detail is below the whole list, so choosing a thread SCROLLS the detail
 * into view and moves focus to its heading ([revealDetail]) — the row's
 * highlight was the only feedback, 1,900px above what it had opened. The rule
 * is width-free on purpose: the detail is revealed whenever it is not on
 * screen, which is never on a wide column and always on a phone.
 */
function Conversations({ seat, now }: { seat: Seat; now: number }) {
  const [thread, setThread] = useParam("conversation", "", "filter");
  // SET BY A PRESS, read once the chosen thread has rendered: a thread named in
  // the address the page was opened on is not a reader's gesture, and the page
  // opens at its top rather than jumping to it.
  const chose = useRef(false);
  const list = useRef<HTMLDivElement>(null);
  useLayoutEffect(() => {
    if (!thread) return;
    // THE CHOSEN ROW STAYS IN ITS LIST'S VIEW, scrolled within the list alone
    // — `scrollIntoView` would move the page too.
    const box = list.current;
    const row = box?.querySelector<HTMLElement>('[aria-pressed="true"]');
    if (box && row) keepInView(box, row);
    if (!chose.current) return;
    chose.current = false;
    revealDetail(document.getElementById(THREAD_DETAIL_ID));
  }, [thread]);
  const threads = useQuery(
    "conversations",
    { handle: seat.handle, ...(thread ? { conversation: thread } : {}) },
    { enabled: seat.handle !== "" },
  );
  const data = threads.data;
  const rows = data?.conversations ?? [];
  return (
    <section className="col gap-2" aria-labelledby="prof-conversations">
      <h3 id="prof-conversations" className="prof-section-title">
        Conversations
      </h3>
      <QueryState error={threads.error} loading={threads.loading && !data}>
        <div className="split thread-split">
          <Card padding="none">
            <Card.Header
              icon={<MessageSquareGlyph size="sm" />}
              count={data?.conversations_total}
              subtitle={
                data ? (pageWords(rows.length, data.conversations_total) ?? undefined) : undefined
              }
            >
              <Card.Title as="h4">Threads</Card.Title>
            </Card.Header>
            {rows.length ? (
              <div className="list thread-list" ref={list}>
                {rows.map((row) => (
                  // THE KEY HAS THE ROW: it is the thread's only identity, so
                  // it takes the whole width and what is known about the
                  // thread is the caption under it. Beside a tag and a full
                  // timestamp it kept 78px of a 300px list and every row read
                  // `work:ENG-…` — five threads nobody could tell apart.
                  <button
                    key={row.key}
                    type="button"
                    className={`thread-entry as-row${row.key === thread ? " selected" : ""}`}
                    aria-pressed={row.key === thread}
                    title={row.key}
                    onClick={() => {
                      chose.current = row.key !== thread;
                      setThread(row.key === thread ? "" : row.key);
                    }}
                  >
                    <span className="mono t-cell key-cell">{conversationLabel(row.key)}</span>
                    <span className="t-caption">
                      {plural(row.turns, "turn")} ·{" "}
                      <span title={fmtDateTime(row.last_at)}>{relTime(row.last_at, now)}</span>
                    </span>
                  </button>
                ))}
              </div>
            ) : (
              <EmptyState
                size="compact"
                icon={<MessageSquareGlyph size={32} />}
                title="No conversations recorded"
                description={
                  data?.held_by === "none"
                    ? "No node holds this seat, so no copy of its ledger is current."
                    : "A seat writes one entry per turn that took part in a thread — a chat message, an issue comment, a page discussion."
                }
              />
            )}
          </Card>

          <Card padding="none" className="thread-detail-card">
            <Card.Header
              icon={<ClockGlyph size="sm" />}
              // A COUNT IS A FACT ABOUT THE THREAD THE READER OPENED: with
              // none open the answer carries no entries, and a 0 there would
              // be a quantity stated about a thread nobody had named.
              count={thread ? data?.entries?.length : undefined}
              // WHICH THREAD, named where the turns are: on a phone the list
              // that says it is a screen above.
              subtitle={thread ? conversationLabel(thread) : undefined}
            >
              <Card.Title as="h4" id={THREAD_DETAIL_ID} tabIndex={-1}>
                Thread turns
              </Card.Title>
            </Card.Header>
            {!thread ? (
              <EmptyState
                size="compact"
                icon={<ClockGlyph size={32} />}
                title="Nothing selected"
                description="Choose a thread to see the turns this seat recorded in it."
              />
            ) : data?.entries?.length ? (
              <div className="list">
                {data.entries.map((entry, i) => (
                  <ThreadTurn key={entry.turn_id || i} entry={entry} />
                ))}
              </div>
            ) : (
              <EmptyState
                size="compact"
                icon={<ClockGlyph size={32} />}
                title="No turns in this thread"
                description="The ledger is trimmed per conversation, so an old thread can list a count it no longer carries the turns for."
              />
            )}
          </Card>
        </div>
      </QueryState>
    </section>
  );
}

/** The open thread's heading, which [revealDetail] scrolls to and focuses. */
const THREAD_DETAIL_ID = "prof-thread-turns";

/**
 * Scroll `row` into its list's own view, moving nothing outside the list.
 *
 * NOT `scrollIntoView`, which scrolls every scrollable ancestor to reach the
 * row — the page included, so reopening a profile on a thread near the list's
 * end would open the page there.
 */
export function keepInView(box: HTMLElement, row: HTMLElement): void {
  const top = row.getBoundingClientRect().top - box.getBoundingClientRect().top + box.scrollTop;
  const bottom = top + row.offsetHeight;
  if (top < box.scrollTop) box.scrollTop = top;
  else if (bottom > box.scrollTop + box.clientHeight) box.scrollTop = bottom - box.clientHeight;
}

/**
 * Bring a chosen thread's heading on screen and put focus on it — when it is
 * not on screen already.
 *
 * On a wide column the sticky detail is always in view and this does nothing,
 * focus included: the reader is still in the list, arrowing to the next
 * thread. Stacked, the detail is below every thread and the heading is where a
 * keyboard or a screen reader has to land for the press to have done anything
 * they can perceive.
 */
export function revealDetail(heading: HTMLElement | null): void {
  if (!heading) return;
  const box = heading.getBoundingClientRect();
  const view = screenScroller()?.getBoundingClientRect();
  const top = view ? view.top : 0;
  const bottom = view ? view.bottom : window.innerHeight;
  if (box.top >= top && box.bottom <= bottom) return;
  heading.scrollIntoView({ block: "start" });
  heading.focus({ preventScroll: true });
}

/** A counterparty's stable identity, for a key and for a link.
 *
 *  THE NAME IS NOT IT. A profile's identity is the seat handle, or the
 *  platform and external id for somebody this company has not mapped — the
 *  display name is deliberately excluded, because a person renaming
 *  themselves on a chat surface must not look like a different colleague.
 */
export function counterpartyKey(profile: CounterpartyProfile): string {
  const { handle, platform, external_id } = profile.subject;
  return handle || `${platform ?? "?"}:${external_id ?? "?"}`;
}

/** How far `last_updated_at` may run ahead of `last_corroborated_at` before
 *  the profile is called stale.
 *
 *  THIRTY DAYS, which is the shortest inbox retention this engine allows and
 *  therefore the shortest span over which "still working together" is a fact
 *  the company still holds evidence for. Shorter and every colleague seen
 *  twice in a week reads as stale; longer and a profile nobody has
 *  corroborated since last quarter looks current. */
const STALE_TRAIT_MS = 30 * 24 * 60 * 60 * 1000;

/** One colleague this seat has learned about.
 *
 *  BOTH INSTANTS: `last_updated_at` moves on every interaction and
 *  `last_corroborated_at` only when the traits changed. A colleague seen daily
 *  whose profile has not moved in months is one this seat has STOPPED
 *  learning about — the gap the prompt's own prefetch demotes stale traits on.
 *
 *  THE TRAITS ARE A BAG: the model invents the keys, so they are listed as
 *  they come rather than drawn as whichever three fields appeared first.
 */
export function CounterpartyRow({ profile, now }: { profile: CounterpartyProfile; now: number }) {
  const traits = Object.entries(profile.traits ?? {});
  const stale =
    profile.last_corroborated_at &&
    profile.last_updated_at &&
    tsKey(profile.last_updated_at) - tsKey(profile.last_corroborated_at) > STALE_TRAIT_MS;
  return (
    <div className="thread-entry">
      <div className="row gap-1">
        {profile.subject.handle ? (
          <a className="t-cell" href={href(["agents", "seats", profile.subject.handle])}>
            <strong>{profile.subject.name || profile.subject.handle}</strong>
          </a>
        ) : (
          <strong className="t-cell">{profile.subject.name || counterpartyKey(profile)}</strong>
        )}
        {!profile.resolved && (
          <Tag appearance="outline" title="not mapped to a seat in this company">
            {profile.subject.platform || "external"}
          </Tag>
        )}
        <span className="spacer" />
        <span className="t-caption">{plural(profile.interactions, "interaction")}</span>
        {/* RELATIVE, like every other list on the profile (Episodes,
            Threads), the instant on hover: a full timestamp with seconds was
            the one row that read differently. */}
        {profile.last_updated_at && (
          <span className="t-caption" title={fmtDateTime(profile.last_updated_at)}>
            {relTime(profile.last_updated_at, now)}
          </span>
        )}
      </div>
      {traits.length > 0 ? (
        <div className="row gap-1 wrap">
          {traits.map(([key, value]) => (
            <Tag key={key} appearance="outline" title={key}>
              {key}: {typeof value === "string" ? value : JSON.stringify(value)}
            </Tag>
          ))}
        </div>
      ) : (
        <p className="t-caption">Seen, and nothing believed about them yet.</p>
      )}
      {stale && (
        <p className="t-caption">
          Last corroborated {fmtDateTime(profile.last_corroborated_at)} — this seat is still working
          with them and has stopped learning about them.
        </p>
      )}
    </div>
  );
}

/** One recorded turn in one conversation.
 *
 *  `reply` AND `unsent` ARE NOT THE SAME FIELD RENDERED TWICE. Both carry the
 *  turn's final artifact, and which one holds it is the whole record of
 *  whether anybody received it: a turn can end with real work done and no way
 *  to say so, and a panel that drew the two alike would show work announced
 *  to nobody as announced — the confusion that made a seat answer a follow-up
 *  against a message it had never sent.
 */
export function ThreadTurn({ entry }: { entry: ConversationEntry }) {
  const trigger = entry.trigger ? plainText(entry.trigger) : "";
  return (
    <div className="thread-entry">
      <div className="row gap-1 wrap">
        {entry.decision && <Tag appearance="outline">{entry.decision}</Tag>}
        <span className="spacer" />
        {entry.turn_id && (
          <a className="t-link mono t-caption" href={href(["live", "turns", entry.turn_id])}>
            turn
          </a>
        )}
        <span className="t-caption">{entry.at ? fmtDateTime(entry.at) : ""}</span>
      </div>
      {/* WHAT WOKE IT, as a line of prose rather than a chip: the trigger is
          the brief the engine wrote the seat — markdown, often a paragraph
          ("## Coalesced updates (5 events)…") — and a chip drew the syntax
          raw and cut the second tag off at the card's edge. Stripped to its
          words (`plainText`), two lines, the whole on hover. */}
      {trigger && (
        <p className="t-caption clamp" title={trigger}>
          {trigger}
        </p>
      )}
      {entry.intent && <p className="t-body">{entry.intent}</p>}
      {entry.reply && <p className="t-caption">{entry.reply}</p>}
      {entry.unsent && (
        // THE ONE THAT REACHED NOBODY, marked.
        <Callout variant="warning" icon={<CircleAlertGlyph size="md" />}>
          <strong>Nothing was delivered.</strong> {entry.unsent}
        </Callout>
      )}
      {entry.completed_work && <p className="t-caption">{entry.completed_work}</p>}
      {/* A BLOCK, NOT A BARE `pre`: a long tool log is a scroll container,
          and `focusWhenScrollable` names and focuses the ones that scroll so
          a keyboard can reach them. */}
      {entry.tool_calls && (
        <CodeBlock
          plain
          maxHeight={RECORD_MAX_HEIGHT}
          focusWhenScrollable
          label="Tool calls in this turn"
          code={entry.tool_calls}
        />
      )}
    </div>
  );
}
