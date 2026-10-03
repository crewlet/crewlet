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
 * of 142" in its header, the same words on all four (`components/memory.tsx`,
 * whose diary and episode cards Knowledge › Agent diaries draws too), so the
 * two cannot silently disagree.
 *
 * # Read with `audit:read`, and asked of nobody else
 *
 * A seat's memory and its conversation ledger are its TRAIL, which the engine
 * answers on the audit grant whoever's seat it is — not by the owner-or-lead
 * rule a person's queue takes. So the tab asks nothing of a reader who does
 * not hold it and says which grant reads it ([GrantRequired]), rather than
 * sending two questions to be refused and drawing the refusals as a seat's
 * memory.
 */

import { Callout, Card, CodeBlock, EmptyState, Skeleton, Tag } from "@crewlethq/ui";
import {
  CircleAlertGlyph,
  ClockGlyph,
  MessageSquareGlyph,
  UsersGlyph,
  ZapGlyph,
} from "@crewlethq/icons/glyphs";
import { useLayoutEffect, useRef } from "react";
import { href, useParam } from "~/app/router.tsx";
import { ClockText } from "~/app/frame/cells.tsx";
import { GrantRequired } from "~/app/frame/GrantRequired.tsx";
import { reveal } from "~/lib/scroller.ts";
import { QueryState, RECORD_MAX_HEIGHT } from "~/components/common.tsx";
import { cutThen, DiaryCard, EpisodesCard, heldWords, pageWords } from "~/components/memory.tsx";
import { conversationLabel, fmtDateTime, plural, relTime, tsKey } from "~/lib/format.ts";
import { plainText } from "~/lib/markdown.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import { seatPath, type Seat } from "~/lib/seats.ts";
import { AUDIT_GRANT } from "./Overview.tsx";
import type { CounterpartyProfile } from "~/contract/memory.ts";
import type { ConversationEntry } from "~/protocol/index.ts";

export function Memory({ seat }: { seat: Seat }) {
  const viewer = useViewer();
  // THE FIRST VIEWER READ IS STILL OUT: nothing is asked yet. Mounting the
  // body here sent both questions before the grants arrived, so a reader
  // without the audit grant was asked of — and refused — anyway.
  if (viewer.asking) {
    return <Skeleton variant="text" rows={5} label="Loading this seat's memory" />;
  }
  // ANSWERED, AND WITHOUT THE GRANT: nothing is asked — see the file's doc.
  // A viewer read that FAILED is not refused either: waiting on it would wait
  // for ever, so the body's reads say what they find.
  if (!viewer.loading && !viewer.grants.includes(AUDIT_GRANT)) {
    return <GrantRequired what={`${seat.name}'s memory`} grants={[AUDIT_GRANT]} />;
  }
  return <MemoryBody seat={seat} />;
}

function MemoryBody({ seat }: { seat: Seat }) {
  const handle = seat.handle;
  const memory = useQuery("agent_memory", { id: handle }, { enabled: handle !== "" });
  const data = memory.data;
  return (
    <div className="col gap-4">
      {memory.loading && !data && (
        <Skeleton variant="text" rows={5} label="Loading this seat's memory" />
      )}
      <QueryState error={memory.error} refusal={memory.refusal} loading={memory.loading && !data}>
        {data && (
          <>
            <p className="t-caption">{heldWords(data.held_by)}</p>
            <DiaryCard memory={data} />
            <EpisodesCard memory={data} />

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
                    <CounterpartyRow key={`${counterpartyKey(c)}-${i}`} profile={c} />
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
      <Conversations seat={seat} />
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
 * into view and moves focus to its heading ([reveal]) — the row's
 * highlight was the only feedback, 1,900px above what it had opened. The rule
 * is width-free on purpose: the detail is revealed whenever it is not on
 * screen, which is never on a wide column and always on a phone.
 */
function Conversations({ seat }: { seat: Seat }) {
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
    reveal(document.getElementById(THREAD_DETAIL_ID), { focus: true });
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
      <QueryState
        error={threads.error}
        refusal={threads.refusal}
        loading={threads.loading && !data}
      >
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
                      <span title={fmtDateTime(row.last_at)}>
                        <ClockText read={(now) => relTime(row.last_at, now)} />
                      </span>
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

/** The open thread's heading, which a press [reveal]s and focuses. */
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
export function CounterpartyRow({ profile }: { profile: CounterpartyProfile }) {
  const traits = Object.entries(profile.traits ?? {});
  const stale =
    profile.last_corroborated_at &&
    profile.last_updated_at &&
    tsKey(profile.last_updated_at) - tsKey(profile.last_corroborated_at) > STALE_TRAIT_MS;
  return (
    <div className="thread-entry">
      <div className="row gap-1">
        {profile.subject.handle ? (
          <a
            className="t-cell"
            href={href(seatPath({ handle: profile.subject.handle, name: profile.subject.name }))}
          >
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
            <ClockText read={(now) => relTime(profile.last_updated_at, now)} />
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
