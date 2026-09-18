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
 * **Needs a decision** is what the engine derived: a run parked on a question,
 * a seat it stopped, a budget refusing, a node past its lease. **Notices** is
 * the person's own inbox — what reached them, and why.
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
import { Callout, EmptyState, Skeleton, Tag } from "@crewlethq/ui";
import { ArrowForwardGlyph, InboxGlyph } from "@crewlethq/icons/glyphs";
// A CONDITION'S MARK IS DATA — `lib/attention.ts` names it, and that name is
// still one of ours. Resolving it to a glyph is the other half of this port and
// belongs in that file; see the report.
import { Mark } from "~/ui/glyph.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import { reasonPhrase, reasonWhy } from "~/lib/reasons.ts";
import {
  useAgents,
  useConnection,
  useOrg,
  useOrgBudget,
  useSandboxes,
  useTokens,
} from "~/lib/store-hooks.ts";
import { attentionQueue, type Attention } from "~/lib/attention.ts";
import { ToolCallBlock } from "~/components/ToolCall.tsx";
import { indexOrg, runState } from "~/lib/seats.ts";
import { useNow } from "~/lib/clock.ts";
import { fmtDateTime, relTime } from "~/lib/format.ts";
import type {
  AgentRow,
  Rollup,
  SandboxEntry,
  WorkInboxNotice,
  WorkProjectRow,
  WorkloadRow,
} from "~/protocol/index.ts";
import { FacetRail } from "~/ui/FacetRail.tsx";
import { Pulse, PULSE_GLYPHS, type PulseFact } from "./Pulse.tsx";

/** A row in either band, as the detail pane addresses it. */
type Selected =
  { band: "alarm"; item: Attention } | { band: "notice"; item: WorkInboxNotice } | null;

export function Inbox() {
  const viewer = useViewer();
  const now = useNow();
  const org = useOrg();
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
  const [state, setState] = useParam("state", "unread");
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
  const index = useMemo(() => indexOrg(org), [org]);
  const attention = useMemo(
    () =>
      attentionQueue({
        agents,
        sandboxes,
        runs: runs?.runs ?? [],
        budget,
        engine: engine ?? null,
        seats: index.seats,
        connected,
        authRejected,
        now,
      }),
    [agents, sandboxes, runs, budget, engine, index.seats, connected, authRejected, now],
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
            empty={{
              title: "Nothing needs a decision",
              hint: "No seat is stopped, no run is parked on a question, and no budget is refusing. The figures above are what the company is doing meanwhile.",
            }}
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
              count={notices.length}
              note="What reached you, and the one reason of twenty it reached you under."
              controls={
                <FacetRail
                  name="State"
                  value={state === "unread" ? "" : state}
                  onChange={(next) => setState(next || "unread")}
                  over="loaded"
                  allLabel="Unread"
                  facets={[
                    { value: "all", label: "All", count: null },
                    { value: "snoozed", label: "Snoozed", count: null },
                  ]}
                />
              }
              empty={{
                title: state === "unread" ? "Nothing unread" : "Nothing reached you",
                hint:
                  state === "unread"
                    ? "Everything the company told you about has been marked read. Switch to All to read back through it."
                    : "A notice arrives when something you are on the hook for moves — a mention, a question, work assigned to you, a task you watch.",
              }}
            >
              {reasons.length > 1 && (
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
              )}
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
 * One band: a heading that always draws, and its rows or its reason for having
 * none.
 *
 * THE HEADING IS UNCONDITIONAL, which is the whole point of the component. A
 * band that disappears when it is empty takes its own name with it, so a
 * reader cannot tell "nothing is waiting on you" from "this screen does not
 * have that". Both bands say which they are, always.
 */
function Band({
  title,
  count,
  note,
  controls,
  empty,
  children,
}: {
  title: string;
  count: number;
  note: string;
  controls?: React.ReactNode;
  empty: { title: string; hint: string };
  children: React.ReactNode;
}) {
  return (
    <section className="inbox-band">
      <header className="inbox-band-head">
        <h2 className="t-label">{title}</h2>
        {/* THE COUNT IS ITS OWN ELEMENT, not part of the heading's text: it
            changes on every poll, and inside the heading it would resize the
            heading and shift whatever sits beside it. */}
        <span className="inbox-band-count">{count}</span>
        <span className="spacer" />
        {controls}
      </header>
      <p className="t-caption">{note}</p>
      {count === 0 ? (
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
        <span className="t-cell truncate">{notice.excerpt || notice.kind.replace(/_/g, " ")}</span>
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
      {notice.excerpt && (
        <div className="col gap-1">
          <h3 className="t-label">What changed</h3>
          <p className="t-body">{notice.excerpt}</p>
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
    const parked = attention.filter((a) => a.id.startsWith("sandbox-")).length;
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
