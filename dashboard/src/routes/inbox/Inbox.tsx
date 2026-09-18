/**
 * The Inbox — the landing screen, and the one this product was missing.
 *
 * # The first fold is the company, not the absence of problems
 *
 * A queue-shaped home renders a healthy company as a blank page, and a reader
 * cannot tell that from a broken one. So the screen opens with the PULSE
 * STRIP — eight facts that are true whatever the queue holds — and the bands
 * sit under it. An empty band then means something: nothing is waiting on you,
 * on a company that is visibly running. See `Pulse.tsx` for the argument in
 * full.
 *
 * # Two bands on one screen, never two tabs
 *
 * **Needs a decision** is what the engine derived — every condition in
 * `lib/attention.ts`, over the subjects it declares. **Notices** is the
 * person's own inbox — what reached them, and why.
 *
 * NEITHER LIST IS RESTATED HERE, and the quiet band's sentence is why. The band
 * named three of twelve conditions as though they were all of them, and this
 * comment named four — one of which, "a node past its lease", the queue has
 * never raised at all. The band draws `WATCHED` now, which is that declaration
 * as one clause.
 *
 * They are STACKED rather than toggled. A tab hides the engine's state behind
 * a control the reader has to press, which is exactly what the landing screen
 * cannot afford: the founder's first glance is the whole of what this screen
 * is for, and a band behind a tab is a band they do not see. They are also
 * never interleaved, because one fused list ordered by time would eventually
 * rank a backup-age alarm above the CEO seat asking whether to hold a release.
 *
 * # Two panes, and the right one is always open
 *
 * Reading one row IS the activity here, so the detail is beside the list
 * rather than behind a navigation: the list keeps its place, and stepping down
 * it REPLACES history — four rows read through one open pane are one place the
 * reader has been, which is the rule the frame's own peek follows.
 *
 * # The reason is the opening fact of every notice
 *
 * The applier records, per change and per recipient, the ONE reason of twenty
 * under which that person heard about it. Nothing has ever drawn it, and it is
 * the fact no commercial tracker keeps: Linear, Jira and ClickUp can all tell
 * you that you were notified, and none can tell you why.
 *
 * # A quiet band is six different facts
 *
 * Zero rows means: nothing has answered yet, the answer was a refusal, a reason
 * chip took them all, the page holds none and more pages exist, the reader is
 * caught up, or nothing has ever reached them. The band inferred one sentence
 * from the row count and the facet, so a person the applier has never written a
 * row for was told "everything has been marked read" and sent to a facet that
 * was just as empty. See `noticeQuiet`.
 *
 * # Read-only, like everything else
 *
 * Marking a notice read is a WRITE, and it belongs to the person whose inbox
 * it is — through their own assistant, with `mark_inbox`, attributed to them.
 * A button here would write as "the dashboard", which is not a person and
 * cannot be asked why.
 */

import { useMemo } from "react";
import { href, useParam } from "~/app/router.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { usePageCoverage } from "~/app/Shell.tsx";
import { QueryState } from "~/components/common.tsx";
import { Callout, EmptyState, EmptyValue, Skeleton, Tag } from "@crewlethq/ui";
import { ArrowForwardGlyph, InboxGlyph } from "@crewlethq/icons/glyphs";
// A CONDITION'S MARK IS DATA — `lib/attention.ts` names it, and that name is
// still one of ours. Resolving it to a glyph is the other half of this port and
// belongs in that file; see the report.
import { Mark } from "~/ui/glyph.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import { reasonPhrase, reasonWhy } from "~/lib/reasons.ts";
import { plainText } from "~/lib/markdown.ts";
import {
  useAgents,
  useConnection,
  useOrgBudget,
  useSandboxes,
  useTokens,
} from "~/lib/store-hooks.ts";
import { attentionQueue, SUBJECTS, WATCHED, type Attention } from "~/lib/attention.ts";
import { ToolCallBlock } from "~/components/ToolCall.tsx";
import { runState } from "~/lib/seats.ts";
import { useNow } from "~/lib/clock.ts";
import { fmtDateTime, relTime } from "~/lib/format.ts";
import type {
  AgentRow,
  Rollup,
  SandboxEntry,
  WorkInboxAnswer,
  WorkInboxNotice,
  WorkProjectRow,
  WorkloadRow,
} from "~/protocol/index.ts";
import { FacetRail } from "~/ui/FacetRail.tsx";
// OURS, AND DELIBERATELY. `SegmentedControl` welds keyboard ACTIVATION to its
// `semantics`: `radio` commits the option the arrows land on, and this control
// drives a `useParam` that re-runs `work_inbox` — so arrowing across three
// scopes is three queries for a reader who wanted one.
import { Segmented } from "~/ui/primitives.tsx";
import { Pulse, PULSE_GLYPHS, type PulseFact } from "./Pulse.tsx";

/**
 * The three scopes the notices band can ask for.
 *
 * QUESTIONS PUT TO THE ENGINE, not facets of the page on screen: `unread` and
 * `include_snoozed` are `work_inbox` parameters, so each is a different page of
 * notices rather than a narrowing of the loaded one. Two of the three therefore
 * name rows this page does not hold, which is why the control carries no counts
 * and why it is a `Segmented` rather than a `FacetRail` — the same control the
 * work toolbar's open/closed/everything is, for the same reason.
 */
const SCOPES = ["unread", "all", "snoozed"] as const;
type Scope = (typeof SCOPES)[number];

function isScope(value: string): value is Scope {
  return (SCOPES as readonly string[]).includes(value);
}

/** A row in either band, as the detail pane addresses it. */
type Selected =
  { band: "alarm"; item: Attention } | { band: "notice"; item: WorkInboxNotice } | null;

export function Inbox() {
  const viewer = useViewer();
  const now = useNow();
  const agents = useAgents();
  const sandboxes = useSandboxes();
  const budget = useOrgBudget();
  const tokens = useTokens();
  const { connected, authRejected } = useConnection();
  const { data: engine } = useQuery("stream", undefined, { pollMs: 15_000 });
  // THE DURABLE CODING RUNS, because a parked one is the longest-lived item
  // this queue has by construction: it is waiting for a person. The live
  // projection sweeps a sandbox entry after twelve hours, so reading it here
  // dropped the row exactly when it had been ignored long enough to matter.
  // Slow, like the runs board's own poll: a run's lifetime is minutes.
  const { data: runs } = useQuery("sandbox_runs", undefined, { pollMs: 30_000 });
  // THE TWO THE PULSE STRIP NEEDS AND NOTHING ELSE ON THIS SCREEN DOES. Both
  // are slow polls: an open count and a workload are facts about a fortnight,
  // and asking them at the socket's own cadence would be eight reads a minute
  // for a strip nobody is watching change.
  const projects = useQuery("work_projects", undefined, { pollMs: 60_000 });
  const workload = useQuery("work_workload", undefined, { pollMs: 60_000 });

  // THE STATE IS IN THE URL, like every other screen: which reasons are being
  // looked at, whether read notices are shown, and which row the pane is on.
  const [stateParam, setState] = useParam("state", "unread");
  // A STRING OFF A URL IS NOT A SCOPE, so it is resolved against the three that
  // exist — the rule `useTab` follows for `tab=`. `?state=banana` left every
  // option unchosen and ran the query with both flags false, which is "all"
  // under a fourth name nothing on screen gives.
  const state: Scope = isScope(stateParam) ? stateParam : "unread";
  const [reason, setReason] = useParam("reason", "");
  const [open, setOpen] = useParam("row", "");

  const inbox = useQuery(
    "work_inbox",
    viewer.handle
      ? {
          handle: viewer.handle,
          limit: 50,
          unread: state === "unread",
          include_snoozed: state === "snoozed",
        }
      : undefined,
    { enabled: viewer.handle !== "", pollMs: 30_000 },
  );
  usePageCoverage(inbox.data);

  const loaded = inbox.data?.notices ?? [];
  // THE REASON NARROWS CLIENT-SIDE, which is what makes the chips honest. Sent
  // to the engine it narrowed the ANSWER, so the page the facet counts were
  // derived from became the page one facet had selected: every other chip's
  // count went to nothing and the rail unmounted under the pointer. Over the
  // loaded page the counts and the rows are the same set by construction, and
  // `over="loaded"` on the rail is already the sentence that says so.
  const notices = reason ? loaded.filter((n) => n.reason === reason) : loaded;

  // THE ENGINE'S OWN CONDITIONS. An alarm the engine raised is a claim on a
  // person exactly as a notice is, and it used to live in a popover behind a
  // pill in the sidebar's foot.
  const attention = useMemo(
    () =>
      attentionQueue({
        agents,
        sandboxes,
        runs: runs?.runs ?? [],
        budget,
        engine: engine ?? null,
        connected,
        authRejected,
        now,
      }),
    [agents, sandboxes, runs, budget, engine, connected, authRejected, now],
  );

  // EVERY REASON THAT IS ACTUALLY ON THE PAGE, so the filter offers what the
  // person has rather than the whole vocabulary of twenty.
  //
  // TWO RULES, AND BOTH ARE ABOUT THE CHIPS NOT MOVING:
  //
  //  1. IT IS DERIVED FROM THE UNFILTERED PAGE. Reading it off `notices` meant
  //     that picking a reason narrowed the answer to that reason, which left
  //     exactly one chip on the page and unmounted the whole rail — so the chip
  //     a reader had just pressed vanished from under the pointer and the only
  //     way back to the others was a clear button somewhere else.
  //  2. IT IS ORDERED BY NAME, never by count. Sorting by count reorders the
  //     row whenever a poll changes one, so the chip a reader is reaching for
  //     moves between the decision to press it and the press.
  const reasons = useMemo(() => {
    const seen = new Map<string, number>();
    for (const n of loaded) seen.set(n.reason, (seen.get(n.reason) ?? 0) + 1);
    // The selected reason always has a chip, even on a page where it now
    // matches nothing: a filter you cannot see is a filter you cannot lift.
    if (reason && !seen.has(reason)) seen.set(reason, 0);
    return [...seen.entries()].sort((a, b) => a[0].localeCompare(b[0]));
  }, [loaded, reason]);

  const selected: Selected = useMemo(() => {
    if (!open) return null;
    const alarm = attention.find((a) => a.id === open);
    if (alarm) return { band: "alarm", item: alarm };
    const notice = notices.find((n) => n.record_id === open);
    return notice ? { band: "notice", item: notice } : null;
  }, [open, attention, notices]);

  const facts = usePulse({
    agents,
    sandboxes,
    attention,
    projects: projects.data?.projects,
    workload: workload.data?.rows,
    tokens,
  });

  return (
    <>
      <PageActions>
        {viewer.handle && (
          <a className="t-link" href={href(["me"])}>
            My work →
          </a>
        )}
      </PageActions>

      <Pulse facts={facts} />

      {/* THREE VIEWER STATES, three different sentences — and only one of them
          is anybody's fault. The engine half of this screen works for all
          three, which is why the page is not simply locked. */}
      {viewer.anonymous ? (
        <Callout variant="warning">
          No API token is presented, so this browser is nobody. The engine's own conditions are
          below; a person's notices need a credential bound to their seat.
        </Callout>
      ) : viewer.unbound ? (
        <Callout variant="warning">
          This token is <code className="inline">{viewer.operatorID}</code> and no seat claims it.
          Give a human seat <code className="inline">contact.crewlet_operator_id</code> with that
          value and this becomes their inbox.
        </Callout>
      ) : null}

      <div className="inbox-panes" data-open={selected ? "true" : undefined}>
        <div className="inbox-list">
          <Band
            title="Needs a decision"
            count={attention.length}
            note="Conditions the engine raised — each names what it costs to leave it."
            empty={
              attention.length === 0
                ? {
                    title: "Nothing needs a decision",
                    // THE SCOPE COMES FROM THE QUEUE, never from this file. A
                    // sentence written here names whatever was true the day it
                    // was typed; `WATCHED` names what the queue watches today.
                    hint: `Checked and clear: ${WATCHED}. The figures above are what the company is doing meanwhile.`,
                  }
                : null
            }
          >
            {attention.map((item) => (
              <AttentionRow
                key={item.id}
                item={item}
                now={now}
                selected={open === item.id}
                onOpen={() => setOpen(item.id)}
              />
            ))}
          </Band>

          {viewer.handle && (
            <Band
              title="Notices"
              count={inbox.data ? notices.length : null}
              note="What reached you, and the one reason of twenty it reached you under."
              controls={
                /* THREE SCOPES, EXACTLY ONE CHOSEN. As a facet rail this could
                   carry no count on any data — "All" and "Snoozed" are rows this
                   page does not hold — and still drew the caption that qualifies
                   counts; and its all-chip, the chip that means "no filter", had
                   to be labelled `Unread`, the NARROWEST of the three, so
                   clearing the filter made the list smaller and a second press
                   on `All` silently returned to it. `size="sm"` because this
                   sits in a band head beside a `t-label` and a count. */
                <Segmented<Scope>
                  value={state}
                  onChange={setState}
                  ariaLabel="Which notices"
                  size="sm"
                  options={[
                    { value: "unread", label: "Unread", title: "What you have not read yet." },
                    {
                      value: "all",
                      label: "All",
                      title: "Everything that reached you, read or not.",
                    },
                    {
                      value: "snoozed",
                      label: "Snoozed",
                      title: "Notices you put off, until they come back.",
                    },
                  ]}
                />
              }
              filters={
                // THE RAIL SURVIVES AN EMPTY LIST. A reason matching nothing is
                // exactly when its chip is the only way back, and `reasons`
                // already keeps a sticky zero-count entry for the selected
                // value — which the emptiness it explains was hiding. The second
                // clause is the same rule for a page that came back empty with a
                // reason still in the URL, where the sticky entry is the ONLY
                // entry and `length > 1` fails.
                (reasons.length > 1 || reason !== "") && (
                  <FacetRail
                    name="Reason"
                    value={reason}
                    onChange={setReason}
                    over="loaded"
                    facets={reasons.map(([name, count]) => ({
                      value: name,
                      label: reasonPhrase(name),
                      count,
                      title:
                        count === 0
                          ? "Filtering on this; nothing else on the page carries it."
                          : undefined,
                    }))}
                  />
                )
              }
              empty={noticeQuiet({
                answer: inbox.data,
                error: inbox.error,
                state,
                reason,
                shown: notices.length,
              })}
            >
              {inbox.loading && !inbox.data && (
                <Skeleton variant="text" rows={5} label="Loading the inbox" />
              )}
              <QueryState error={inbox.error} loading={inbox.loading}>
                {notices.map((notice) => (
                  <NoticeRow
                    key={notice.record_id}
                    notice={notice}
                    now={now}
                    selected={open === notice.record_id}
                    onOpen={() => setOpen(notice.record_id)}
                  />
                ))}
              </QueryState>
              {inbox.data?.next_cursor && (
                <p className="t-caption">
                  More notices exist beyond this page. The engine returns at most 50 at a time.
                </p>
              )}
            </Band>
          )}

          {!viewer.handle && !viewer.loading && (
            <PageNote>
              With a credential bound to a seat, this screen also shows what reached that person.
            </PageNote>
          )}
        </div>

        {/* THE PANE, ALWAYS PRESENT. It holds its own prompt rather than
            collapsing when nothing is selected: a column that appears on the
            first click reflows the list under the pointer, so the row a reader
            clicked is no longer the row they are looking at. */}
        <aside className="inbox-detail" aria-label="The selected row">
          <Detail selected={selected} viewer={viewer.name} now={now} />
        </aside>
      </div>
    </>
  );
}

/**
 * What the Notices band says when it draws no rows — and `null` where it must
 * say nothing at all.
 *
 * THE SENTENCE IS DERIVED FROM THE ANSWER, NEVER FROM THE ROW COUNT. Six facts
 * share a row count of zero, and the band used to branch on the facet alone: a
 * person who has never received a notice — nothing under either facet, nothing
 * ever marked — read "Everything the company told you about has been marked
 * read. Switch to All to read back through it.", which asserts a history that
 * does not exist and points at a facet that is just as empty.
 *
 * `seen_through` IS THE EVIDENCE, and it is the only evidence in this answer.
 * It is the person's own read watermark, written by `mark_inbox` alone
 * (`tracker.Writer.WriteInbox`), and the Go side omits the whole object when it
 * is zero — so its absence is "this person has never recorded how far they have
 * read". A zero-sequence object is the same fact and must read the same way,
 * which is why this tests the sequence and not the key. The copy therefore
 * claims only the watermark: the EXACT question — has anything ever reached you
 * — is a second `work_inbox` read on the empty path, and its own loading and
 * error states would rewrite this sentence under the reader a beat after they
 * read it. The answer already in hand cannot flicker.
 *
 * THE BRANCHES MIRROR THE QUERY'S OWN PREDICATES (`unread: state === "unread"`,
 * `include_snoozed: state === "snoozed"`), because a sentence that disagrees
 * with the question that was asked is this same defect one layer down.
 */
export function noticeQuiet(input: {
  answer: WorkInboxAnswer | null;
  error: string | null;
  /** The `state` facet, straight off the URL: it is not a closed set here. */
  state: string;
  /** The reason chip, empty when none is pressed. */
  reason: string;
  /** How many rows survive the reason filter. */
  shown: number;
}): { title: string; hint: string } | null {
  const { answer, error, state, reason, shown } = input;
  // NOTHING IS KNOWN. The skeleton and the refusal are this band's children and
  // they are what belongs here; a sentence would be a claim about a company
  // nobody has read. A failed poll keeps its last answer, so an error beside
  // data is still a band that cannot speak for itself.
  if (error || !answer) return null;
  if (shown > 0) return null;

  const arrives =
    "A notice arrives when something you are on the hook for moves — a mention, a question, work assigned to you, a task you watch.";

  // THE ROWS ARE NOT ABSENT, THEY ARE FILTERED — and the chip that did it is on
  // screen, because this band draws its filters whether or not anything
  // survives them.
  if (reason && answer.notices.length > 0) {
    const held = answer.notices.length;
    return {
      title: "Nothing on this page carries that reason",
      hint: `The page holds ${held} notice${held === 1 ? "" : "s"} under other reasons. Press the chip again for all of them.`,
    };
  }

  // A PAGE WHOSE EVERY ROW WAS DROPPED AFTER IT WAS READ. `unread` and the
  // snooze filter are applied to the page rather than to the scan
  // (`tracker.InboxQuery.Unread`), while the cursor is computed from the
  // unfiltered page — so no rows and more pages is an ordinary answer, and "you
  // are caught up" over it is a claim about rows nobody looked at.
  if (answer.next_cursor) {
    return {
      title: "None on this page",
      hint: "The engine answers 50 notices at a time and every one on this page was filtered out here. There are more beyond it.",
    };
  }

  if (state === "snoozed") {
    return {
      title: "Nothing is snoozed",
      hint: "A snooze means not now — the notice leaves this list until the time set on it, and comes back when that time arrives.",
    };
  }
  if (state !== "unread") {
    return { title: "Nothing has reached you", hint: arrives };
  }
  return (answer.seen_through?.seq ?? 0) > 0
    ? {
        title: "You are caught up",
        hint: "Everything that reached you has been marked read. Switch to All to read back through it.",
      }
    : {
        title: "Nothing has reached you yet",
        hint: `${arrives} Nothing has been marked read either, so All holds this same empty list.`,
      };
}

/**
 * One band: a heading that always draws, and its rows or its reason for having
 * none.
 *
 * THE HEADING IS UNCONDITIONAL, which is the whole point of the component. A
 * band that disappears when it is empty takes its own name with it, so a
 * reader cannot tell "nothing is waiting on you" from "this screen does not
 * have that". Both bands say which they are, always. What it says when it is
 * quiet is the caller's, because only the caller knows whether the list is
 * empty or merely unanswered.
 */
function Band({
  title,
  count,
  note,
  controls,
  filters,
  empty,
  children,
}: {
  title: string;
  /** How many rows are drawn, or null where nothing has answered — the head
   *  draws an em dash for that, never a `0`. "No notices" is a claim and it is
   *  a false one for as long as the read is in flight, which is the rule the
   *  pulse strip's own figures follow one component up. */
  count: number | null;
  note: string;
  controls?: React.ReactNode;
  /** Controls over this band's OWN rows, drawn above the list and drawn
   *  ALWAYS. A chip that narrows a list to nothing is hidden by the very
   *  emptiness it caused otherwise, and a filter you cannot see is a filter you
   *  cannot lift. */
  filters?: React.ReactNode;
  /** What to say instead of rows, or null where this band cannot say anything
   *  — nothing has answered, or the answer was a refusal. It is the CALLER's
   *  derivation: a band that inferred it from `count` told a reader whose query
   *  had failed that their company was quiet. */
  empty: { title: string; hint: string } | null;
  children: React.ReactNode;
}) {
  return (
    <section className="inbox-band">
      <header className="inbox-band-head">
        <h2 className="t-label">{title}</h2>
        {/* THE COUNT IS ITS OWN ELEMENT, not part of the heading's text: it
            changes on every poll, and inside the heading it would resize the
            heading and shift whatever sits beside it. */}
        <span className="inbox-band-count">
          {count ?? <EmptyValue label="Not counted: this read did not answer" />}
        </span>
        <span className="spacer" />
        {controls}
      </header>
      <p className="t-caption">{note}</p>
      {filters}
      {count === 0 && empty ? (
        <div className="inbox-quiet">
          <strong className="t-cell">{empty.title}</strong>
          <span className="t-caption">{empty.hint}</span>
        </div>
      ) : (
        <div className="list">{children}</div>
      )}
    </section>
  );
}

/** One engine condition. */
function AttentionRow({
  item,
  now,
  selected,
  onOpen,
}: {
  item: Attention;
  now: number;
  selected: boolean;
  onOpen: () => void;
}) {
  return (
    <button
      type="button"
      className="inbox-row"
      aria-current={selected || undefined}
      onClick={onOpen}
    >
      <span className="attention-icon" data-severity={item.severity}>
        <Mark name={item.icon} size="sm" />
      </span>
      <span className="col inbox-row-body">
        <strong className="t-cell truncate">{item.title}</strong>
        <span className="t-caption truncate">{item.detail}</span>
      </span>
      {item.who && <Tag appearance="outline">{item.who}</Tag>}
      {item.at && <span className="t-caption">{relTime(item.at, now)}</span>}
    </button>
  );
}

/** One notice: the reason first, then what changed. */
function NoticeRow({
  notice,
  now,
  selected,
  onOpen,
}: {
  notice: WorkInboxNotice;
  now: number;
  selected: boolean;
  onOpen: () => void;
}) {
  return (
    <button
      type="button"
      className="inbox-row"
      data-read={notice.read || undefined}
      aria-current={selected || undefined}
      onClick={onOpen}
    >
      {/* THE UNREAD MARK IS A FIXED CELL, filled or not. Drawn only when unread
          it was a dot that appeared and disappeared as a poll landed, and
          every row's text stepped sideways with it. */}
      <i className="inbox-unread" data-on={!notice.read || undefined} aria-hidden="true" />
      <span className="col inbox-row-body">
        <span className="row gap-1">
          {/* THE REASON, FIRST. It is the fact nothing else on this row
              carries, and the one no other tracker records. */}
          <Tag appearance="outline" title={reasonWhy(notice.reason)}>
            {reasonPhrase(notice.reason)}
          </Tag>
          {notice.addressed && (
            <Tag variant="warning" title="this asks something of you">
              asks
            </Tag>
          )}
          {notice.subject_key && <span className="mono t-caption">{notice.subject_key}</span>}
        </span>
        {/* THE PROSE THE BODY RENDERS TO, not its source. An excerpt is a
            cut of a comment or a description — markdown by contract — and this
            cell is one line, so an unflattened one printed `## Understanding
            the work` with the hashes in it. */}
        <span className="t-cell truncate">
          {plainText(notice.excerpt ?? "") || notice.kind.replace(/_/g, " ")}
        </span>
      </span>
      {notice.at && <span className="t-caption">{relTime(notice.at, now)}</span>}
    </button>
  );
}

/** The right-hand pane: one row, in full. */
function Detail({ selected, viewer, now }: { selected: Selected; viewer?: string; now: number }) {
  if (!selected) {
    return (
      <EmptyState
        icon={<InboxGlyph size={28} />}
        title="Pick a row"
        description="Its reason, what changed and the call that would mark it are shown here."
      />
    );
  }
  if (selected.band === "alarm") {
    const item = selected.item;
    return (
      <div className="col gap-3">
        <div className="row gap-2">
          <span className="attention-icon" data-severity={item.severity}>
            <Mark name={item.icon} size="sm" />
          </span>
          <strong className="t-cell">{item.title}</strong>
        </div>
        <p className="t-body">{item.detail}</p>
        {item.at && <p className="t-caption">Since {fmtDateTime(item.at)}</p>}
        {item.path && (
          <a className="t-link row gap-1" href={href(item.path, item.query)}>
            Go to it <ArrowForwardGlyph size="sm" />
          </a>
        )}
        <p className="t-caption">
          Computed from the live state — nothing here is stored or marked. It clears when the
          condition does.
        </p>
      </div>
    );
  }
  const notice = selected.item;
  return (
    <div className="col gap-3">
      <div className="row gap-1">
        <Tag appearance="outline">{reasonPhrase(notice.reason)}</Tag>
        {notice.addressed && <Tag variant="warning">asks</Tag>}
        {notice.fallback && (
          <Tag appearance="outline" title="nobody better was found for this">
            fallback
          </Tag>
        )}
      </div>
      <div className="col gap-1">
        <h3 className="t-label">Why you are seeing this</h3>
        <p className="t-body">{reasonWhy(notice.reason)}</p>
        {notice.fallback && (
          <p className="t-caption">
            You were the fallback: nobody better was found for it, so it came to you.
          </p>
        )}
      </div>
      {notice.subject_key && (
        <a className="t-link mono" href={href(["work", notice.subject_key])}>
          {notice.subject_key} →
        </a>
      )}
      {/* FLATTENED EVEN HERE, where there is room for blocks: this is an
          EXCERPT, cut mid-construct by the engine, and `<p>` cannot legally hold
          the h2 or the table a render of one would produce. The item page is
          where the whole body is rendered. */}
      {plainText(notice.excerpt ?? "") && (
        <div className="col gap-1">
          <h3 className="t-label">What changed</h3>
          <p className="t-body">{plainText(notice.excerpt ?? "")}</p>
        </div>
      )}
      <p className="t-caption">
        {notice.actor ? `${notice.actor} · ` : ""}
        {fmtDateTime(notice.at)} · {relTime(notice.at, now)}
      </p>
      {/* THE CALL THAT WOULD MARK IT, rather than a control that pretends to.
          This screen only reads — every write here is attributed to somebody,
          and a button in a browser would write as "the dashboard", which is
          nobody — so what it offers is the `mark_inbox` an assistant would
          make, pre-filled with this entry's own record and POSITION. The two
          travel together: a position from a recreated stream compares as
          current. */}
      <ToolCallBlock
        subject={{ kind: "notice", id: notice.record_id, version: notice.log_seq }}
        viewer={viewer}
      />
      <p className="t-caption">
        Your assistant marks these read with <code className="inline">mark_inbox</code>, which is
        attributed to you. Nothing on this screen writes.
      </p>
    </div>
  );
}

/**
 * The eight facts, assembled.
 *
 * SEPARATED FROM THE SCREEN because every one of them is a small honest
 * decision about what a number covers, and those are worth reading in one
 * place: which seat states count as working, that `open` is over the projects
 * this answer returned rather than over the company, that the sprint shown is
 * the one closing SOONEST when several run at once, and that a window the
 * engine chose is never labelled "today".
 */
function usePulse(input: {
  agents: AgentRow[];
  sandboxes: SandboxEntry[];
  attention: Attention[];
  projects?: WorkProjectRow[];
  workload?: WorkloadRow[];
  tokens: Rollup | null;
}): PulseFact[] {
  const { agents, sandboxes, attention, projects, workload, tokens } = input;
  return useMemo(() => {
    const working = agents.filter((a) => {
      const state = runState(a, sandboxes);
      return state === "working" || state === "awaiting_sandbox";
    }).length;
    // A RUN WAITING FOR A PERSON, which the attention queue already derived
    // from the DURABLE rows: counting the live projection here would drop a
    // run at twelve hours, which is the point at which it most needs counting.
    // THE SUBJECT, NOT THE ID'S SPELLING. This matched `id.startsWith("sandbox-")`,
    // so renaming that id would have taken a headline figure silently to zero —
    // the same drift one layer down from the sentence below it.
    const parked = attention.filter((a) => a.subject === "run").length;
    const open = projects ? projects.reduce((n, p) => n + p.task_counts.open, 0) : null;
    const overdue = workload ? workload.reduce((n, r) => n + r.overdue, 0) : null;
    const blocked = workload ? workload.reduce((n, r) => n + r.blocked, 0) : null;
    // THE ONE CLOSING SOONEST. A company running four sprints has four
    // answers to "how long left", and the earliest deadline is the one a
    // person is actually up against.
    const sprint = projects
      ?.map((p) => p.sprints?.active)
      .filter((s): s is NonNullable<typeof s> => Boolean(s))
      .sort((a, b) => a.days_remaining - b.days_remaining)[0];
    const critical = attention.filter((a) => a.severity === "critical").length;
    const facts: PulseFact[] = [
      {
        key: "seats",
        icon: PULSE_GLYPHS.seats,
        value: working,
        label: "working",
        title: "Agent seats running a turn or waiting on a sandbox, from the live projection.",
        path: ["company", "people"],
      },
      {
        key: "parked",
        icon: PULSE_GLYPHS.parked,
        value: parked,
        label: "parked",
        title:
          "Coding runs stopped on a question, from the durable rows rather than the live push.",
        path: ["activity", "runs"],
        tone: parked > 0 ? "caution" : undefined,
      },
      {
        key: "open",
        icon: PULSE_GLYPHS.open,
        value: open,
        label: "open",
        title: "Open work items, summed over the projects this answer returned.",
        path: ["work"],
      },
      {
        key: "overdue",
        icon: PULSE_GLYPHS.overdue,
        value: overdue,
        label: "overdue",
        title: "Open items past their due date, summed over every person with a workload.",
        path: ["work"],
        query: { due: "overdue" },
        tone: overdue ? "caution" : undefined,
      },
      {
        key: "blocked",
        icon: PULSE_GLYPHS.blocked,
        value: blocked,
        label: "blocked",
        title: "Open items waiting on another item, summed over every person with a workload.",
        path: ["work"],
        query: { blocked: "1" },
        tone: blocked ? "caution" : undefined,
      },
      {
        key: "sprint",
        icon: PULSE_GLYPHS.sprint,
        value: sprint ? sprint.days_remaining : null,
        label: sprint ? `days left · ${sprint.name}` : "no sprint running",
        title: sprint
          ? `The active sprint closing soonest: ${sprint.name}.`
          : "No project has an active sprint.",
        path: ["work"],
      },
      {
        key: "tokens",
        icon: PULSE_GLYPHS.tokens,
        value: tokens ? tokens.totals.total_tokens : null,
        label: "tokens",
        // NOT "today". The window is the engine's and this screen was not
        // given one, so the strip names the figure and puts the window it
        // actually covers where a reader can read it.
        title: tokens
          ? `Tokens over the pushed window, ${fmtDateTime(tokens.since)} to ${fmtDateTime(tokens.until)}.`
          : "Spend has not been pushed yet.",
        path: ["cost"],
      },
      {
        key: "alarms",
        icon: PULSE_GLYPHS.alarms,
        value: critical,
        label: "alarms",
        title: "Conditions the engine raised at critical severity. They are the first band below.",
        path: ["inbox"],
        tone: critical > 0 ? "critical" : undefined,
      },
    ];
    return facts;
  }, [agents, sandboxes, attention, projects, workload, tokens]);
}
