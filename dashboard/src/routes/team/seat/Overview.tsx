/**
 * A seat's Overview in the person shell: its open work, who reports to it,
 * what it is for and the recurring work that wakes it — and, for a person,
 * their day, where the reader may read it.
 *
 * # What a teammate reads, and nothing the machine processed
 *
 * Everything here is the company's published facts: the org projection a
 * member is sent (the seat's goal, its responsibilities, who reports to it),
 * the `schedules` push, the work assigned to it, and a person's own record
 * where the chart lets the reader read it. The week's turn figures, the turn
 * in flight, the model chain, the node holding the seat and its memory are
 * how the machine RUNS the seat — the console's (`routes/admin/agents/seat/
 * Overview.tsx`), which draws these same panels beside its own, so the two
 * pages can never describe a seat's work or its charter two ways.
 *
 * # A person runs no turn
 *
 * A human seat's Overview asks nothing a runtime answers: the engine never
 * spawns a person. What it shows is their day (theirs, and their leads', to
 * read), the work assigned to them and who they are.
 */

import { useId, useMemo, useRef, useState } from "react";
import { Card, EmptyValue, StatCard, StatGroup, Tag } from "@crewlethq/ui";
import { ClockGlyph } from "@crewlethq/icons/glyphs";
import { href, useShell } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { DueMark, PriorityMark, StatusMark } from "~/components/work.tsx";
import { describe as describeCron } from "~/lib/cron.ts";
import { fmtDate, inTime, plural, relTime } from "~/lib/format.ts";
import { reportsCaption, seatPath, type NameOf, type OrgIndex, type Seat } from "~/lib/seats.ts";
import { renderInline } from "~/lib/markdown.ts";
import { useSchedules } from "~/lib/store-hooks.ts";
import { useClipped } from "~/lib/useClipped.ts";
import { inboxFigure, useInboxCountsOf, waitingWords } from "~/lib/useInboxCounts.ts";
import { useQuery, type QueryResult } from "~/lib/useQuery.ts";
import { readsPersonOf, useViewer } from "~/lib/viewer.ts";
import type { AgentRow, WorkItemsAnswer, WorkSummary } from "~/protocol/index.ts";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import { seatSchedules } from "./profile.ts";

/** How many assigned tasks the Overview lists before "All n". */
export const ASSIGNED_ROWS = 5;

/** How many schedules a card that has somewhere to list the rest draws first. */
const SCHEDULE_ROWS = 3;

/**
 * How many responsibilities About lists before "Show all" — the artboard's
 * three, which keep the rest of the side column beside the main one rather
 * than a screen further down. A charter is written for the seat's prompt, not
 * for this card, and the example company's run to a dozen bullets of four
 * lines each: drawn whole, About stood 1,700px tall and pushed the rest of
 * the side off the page.
 */
export const ABOUT_ROWS = 3;

export interface OverviewProps {
  seat: Seat;
  agent: AgentRow | undefined;
  index: OrgIndex;
  /** The seat's open work — the one answer the tab strip counts. */
  work: QueryResult<WorkItemsAnswer>;
  nameOf: NameOf;
  now: number;
}

export function Overview(props: OverviewProps) {
  return props.seat.kind === "human" ? (
    <HumanOverview {...props} />
  ) : (
    <AgentOverview {...props} />
  );
}

// ---------------------------------------------------------------------------
// An agent
// ---------------------------------------------------------------------------

function AgentOverview({ seat, work, now }: OverviewProps) {
  return (
    <div className="prof-overview">
      <div className="prof-main">
        <AssignedWork seat={seat} work={work} now={now} />
        <Reports seat={seat} />
      </div>
      <aside className="prof-side" aria-label={`About ${seat.name}`}>
        <About seat={seat} />
        <SchedulesCard seat={seat} now={now} />
      </aside>
    </div>
  );
}

/** The open work on the seat, most urgent first, with the way to all of it. */
export function AssignedWork({
  seat,
  work,
  now,
}: {
  seat: Seat;
  work: QueryResult<WorkItemsAnswer>;
  now: number;
}) {
  const rows = work.data?.items ?? [];
  const total = work.data?.total_hint ?? rows.length;
  return (
    <Card padding="none" className="prof-card">
      <Card.Header
        actions={
          total > 0 ? (
            <a className="t-link" href={href(["work"], { assignee: seat.handle })}>
              All {total.toLocaleString()}
              {work.data?.total_capped ? "+" : ""}
            </a>
          ) : undefined
        }
      >
        <Card.Title as="h3">Assigned work</Card.Title>
      </Card.Header>
      <QueryState
        error={work.error}
        loading={work.loading && !work.data}
        empty={
          work.data && rows.length === 0
            ? {
                title: "Nothing open is assigned to them",
                hint: "Work reaches a seat by assignment; closed work is not counted here.",
              }
            : undefined
        }
      >
        <ul className="prof-rows">
          {rows.slice(0, ASSIGNED_ROWS).map((row) => (
            <AssignedRow key={row.key} row={row} now={now} />
          ))}
        </ul>
      </QueryState>
    </Card>
  );
}

function AssignedRow({ row, now }: { row: WorkSummary; now: number }) {
  return (
    <li>
      <a className="prof-row" href={href(["work", row.key])}>
        <StatusMark status={row.status} />
        <span className="work-key mono prof-row-key" title={row.key}>
          {row.key}
        </span>
        <span className="prof-row-title">{row.title}</span>
        <PriorityMark priority={row.priority} />
        <span className="prof-row-due">
          <DueMark due={row.due} overdue={row.overdue} now={now} />
        </span>
      </a>
    </li>
  );
}

/** Who reports to this seat, when anybody does. */
export function Reports({ seat }: { seat: Seat }) {
  const shell = useShell();
  if (seat.reports.length === 0) return null;
  return (
    <Card padding="none" className="prof-card">
      <Card.Header count={seat.reports.length} subtitle={reportsCaption(seat)}>
        <Card.Title as="h3">Direct reports</Card.Title>
      </Card.Header>
      <div className="seat-grid prof-reports">
        {seat.reports.map((r) => {
          const about = r.goal || r.unit?.name || "";
          return (
            <a key={r.key} className="seat-card" href={href(seatPath(r, shell))}>
              <div className="row">
                <SeatAvatar
                  name={r.name}
                  size="sm"
                  kind={r.kind === "human" ? "human" : "agent"}
                  decorative
                />
                <span className="truncate t-cell prof-report-name">{r.name}</span>
              </div>
              {/* A GOAL IS PROSE, SO IT GETS THE CARD'S OWN WIDTH and two
                  lines of it, cut at a line rather than at whichever pixel
                  came next; the whole of it is on the title. */}
              {about && (
                <span className="clamp t-caption" title={about}>
                  {renderInline(about, r.key)}
                </span>
              )}
            </a>
          );
        })}
      </div>
    </Card>
  );
}

/**
 * The seat's goal and what it is responsible for — its charter.
 *
 * THE ARTBOARD'S SHAPE, THEN THE WHOLE OF IT ON REQUEST: the goal to three
 * lines and the first [ABOUT_ROWS] responsibilities to two each, and a
 * disclosure where that cut anything — more responsibilities than three, or a
 * clamp that measured short ([useClipped]). The charter is the seat's prompt
 * text, so nothing in it is summarised: "Show all" draws every word.
 *
 * INLINE MARKDOWN, because that is what a prompt is written in: a
 * responsibility naming ``nimbuscore`` meant a code span, and drawn as plain
 * text it read as two pairs of stray backticks.
 */
export function About({ seat, onboardedAt }: { seat: Seat; onboardedAt?: string | undefined }) {
  const human = seat.kind === "human";
  const empty = !seat.goal && seat.responsibilities.length === 0;
  const [whole, setWhole] = useState(false);
  const bodyId = useId();
  const body = useRef<HTMLDivElement>(null);
  const clipped = useClipped(body, `${whole}|${seat.goal}|${seat.responsibilities.join("\n")}`);
  const more = seat.responsibilities.length - ABOUT_ROWS;
  const shown = whole ? seat.responsibilities : seat.responsibilities.slice(0, ABOUT_ROWS);
  const offer = whole || more > 0 || clipped;
  return (
    <Card padding="none" className="prof-card">
      <Card.Header divided={false}>
        <Card.Title as="h3">About</Card.Title>
      </Card.Header>
      <div className="prof-about" id={bodyId} ref={body} data-whole={whole || undefined}>
        {seat.goal && (
          <p className="prof-about-goal">
            <span className={whole ? undefined : "clamp"}>{renderInline(seat.goal, "goal")}</span>
          </p>
        )}
        {shown.length > 0 && (
          <ul className="prof-about-list">
            {shown.map((r, i) => (
              // THE CLAMP IS ON A SPAN INSIDE THE ITEM: on the `li` itself
              // its `display` would take the bullet with it.
              <li key={i}>
                <span className={whole ? undefined : "clamp"}>{renderInline(r, `resp-${i}`)}</span>
              </li>
            ))}
          </ul>
        )}
        {offer && (
          // THE CARD-LINK REGISTER, flush with the text above it: a ghost
          // button's own inline padding stood it nine pixels in from the goal
          // and the list it opens, and it was the one action on the side
          // column not drawn the way "Edit" and "Open" are.
          <button
            type="button"
            className="t-link prof-about-more"
            aria-expanded={whole}
            aria-controls={bodyId}
            onClick={() => setWhole(!whole)}
          >
            {whole
              ? "Show less"
              : more > 0
                ? `Show all ${seat.responsibilities.length} responsibilities`
                : "Show the whole charter"}
          </button>
        )}
        {empty && (
          <p className="t-caption">
            {human
              ? "No goal or responsibilities are written for this person."
              : "No goal or responsibilities are written for this seat — both render straight into its prompt."}
          </p>
        )}
        {human && seat.availability && <p className="t-caption">Available {seat.availability}</p>}
        {onboardedAt && <p className="t-caption">Onboarded {fmtDate(onboardedAt)}</p>}
      </div>
    </Card>
  );
}

/**
 * The recurring work that wakes the seat, soonest first.
 *
 * `all` is where every one of them is listed, when a page has such a place —
 * the console's Schedules tab. Without one the card draws every row, because
 * a card that stopped at three with nowhere to go for the rest would be a
 * list that silently ends.
 */
export function SchedulesCard({ seat, now, all }: { seat: Seat; now: number; all?: string }) {
  const pushed = useSchedules();
  const rows = useMemo(() => seatSchedules(pushed, seat.handle), [pushed, seat.handle]);
  return (
    <Card padding="none" className="prof-card">
      <Card.Header
        divided={false}
        actions={
          all && rows.length > SCHEDULE_ROWS ? (
            <a className="t-link" href={all}>
              All {rows.length}
            </a>
          ) : undefined
        }
      >
        <Card.Title as="h3">Schedules</Card.Title>
      </Card.Header>
      {rows.length === 0 ? (
        <p className="prof-note">Nothing recurring reaches {seat.name}.</p>
      ) : (
        <ul className="prof-sched">
          {(all ? rows.slice(0, SCHEDULE_ROWS) : rows).map((row) => (
            <li key={`${row.scope_type}/${row.scope_id}/${row.name}`}>
              {/* THE ARTBOARD'S CLOCK: each row is a recurring time. */}
              <ClockGlyph size="sm" className="prof-sched-glyph" aria-hidden="true" />
              <span className="prof-sched-name">{row.name}</span>
              <span className="prof-sched-when">
                {row.problem ? (
                  <Tag size="xs" variant="danger" title={row.problem}>
                    cannot fire
                  </Tag>
                ) : !row.enabled ? (
                  "disabled"
                ) : (
                  <span title={row.next_run ? `next ${inTime(row.next_run, now)}` : undefined}>
                    {describeCron(row.cron) ?? row.cron}
                  </span>
                )}
              </span>
            </li>
          ))}
        </ul>
      )}
    </Card>
  );
}

// ---------------------------------------------------------------------------
// A person
// ---------------------------------------------------------------------------

export function HumanOverview({ seat, index, work, nameOf, now }: OverviewProps) {
  const viewer = useViewer();
  // A PERSON RECORD IS THEIRS: their unread notices, the order they mean to
  // work in and who set it. The engine answers it to them and to the leads
  // in their line, and to nobody else, an admin included (`readsPersonOf`)
  // — so it is asked only where it can be answered, and WITHHELD by a
  // sentence elsewhere rather than drawn as a person with nothing to do.
  const self = viewer.handle !== "" && viewer.handle === seat.handle;
  const mayRead = readsPersonOf(viewer, seat.handle);
  const person = useQuery(
    "work_person",
    { handle: seat.handle },
    { enabled: mayRead && seat.handle !== "", pollMs: 60_000 },
  );
  // WHAT IS WAITING ON THEM is the badge's count asked of their inbox — the
  // sidebar badge's own reading when it is the viewer's own day — never the
  // person record's `unread`, which holds only the EXCEPTIONS below their read
  // mark.
  const inbox = useInboxCountsOf(seat.handle, mayRead);
  // WHAT THEY MEAN TO DO FIRST, COUNTED BY THE ENGINE: the resolved list's
  // total, the figure My work and this profile's Work tab both read. The
  // record's `priorities` is the list AS STORED — a person's own object, which
  // a read never rewrites — so it still names a task they finished until
  // their next reorder drops it, and a tile counting it said 3 beside a Work
  // tab listing 2.
  const mine = useQuery(
    "work_my_work",
    { handle: seat.handle },
    { enabled: mayRead && seat.handle !== "", pollMs: 60_000 },
  );
  const priorities = mine.data?.totals.priorities;
  const who = self ? "you" : "them";
  return (
    <div className="prof-overview">
      <div className="prof-main">
        {mayRead ? (
          // A SECTION RATHER THAN A CARD: the tiles draw their own surface,
          // and a card around them was a frame inside a frame.
          <section className="prof-day" aria-labelledby="prof-day-title">
            <div className="prof-day-head">
              <h3 id="prof-day-title" className="prof-section-title">
                {self ? "Your day" : "Their day"}
              </h3>
              {self ? (
                // YOURS IS MOVED ON WHERE YOU WORK IT, so this says where.
                <span className="t-caption">
                  <a className="t-link" href={href(["inbox"])}>
                    Inbox
                  </a>
                  {" · "}
                  <a className="t-link" href={href(["me"])}>
                    My work
                  </a>
                </span>
              ) : (
                <span className="t-caption">
                  Read-only here: an inbox is moved on by the person whose it is.
                </span>
              )}
            </div>
            <QueryState error={person.error} loading={person.loading && !person.data}>
              {person.data && (
                <StatGroup columns={3}>
                  <StatCard
                    // WAITING, NEVER "UNREAD": on the Inbox, Unread is every
                    // unread notice, a larger number, and this is the badge's
                    // figure under the badge's words (`waitingWords`).
                    label="Waiting"
                    loading={inbox.waiting === null}
                    value={inboxFigure(inbox)}
                    sub={
                      person.data.due?.length
                        ? `${waitingWords(inbox, who)} · ${plural(person.data.due.length, "snoozed notice")} now due`
                        : waitingWords(inbox, who)
                    }
                  />
                  <StatCard
                    // THE LIST SOMEBODY WROTE, called what My work calls it —
                    // "Queue" there is the open work ASSIGNED to them.
                    label="Priorities"
                    loading={!mine.data && !mine.error}
                    value={
                      priorities ? (
                        `${priorities.total.toLocaleString()}${priorities.capped ? "+" : ""}`
                      ) : (
                        <EmptyValue
                          label={
                            mine.error
                              ? "Their queue did not answer"
                              : "This engine counts no queue"
                          }
                        />
                      )
                    }
                    sub={
                      person.data.priorities_set_by
                        ? `set by ${nameOf(person.data.priorities_set_by)}${
                            person.data.priorities_set_at
                              ? ` ${relTime(person.data.priorities_set_at, now)}`
                              : ""
                          }`
                        : `${self ? "your" : "their"} own order`
                    }
                  />
                  <StatCard
                    label="Pinned views"
                    value={(person.data.pinned_views?.length ?? 0).toLocaleString()}
                    sub={`${person.data.favorites?.length ?? 0} starred`}
                  />
                </StatGroup>
              )}
            </QueryState>
          </section>
        ) : (
          <Card padding="none" className="prof-card">
            <Card.Header divided={false}>
              <Card.Title as="h3">Their day</Card.Title>
            </Card.Header>
            <p className="prof-note">
              Their inbox, their priorities and their pinned views are theirs, and their
              leads&rsquo; to read — nobody else&rsquo;s.
            </p>
          </Card>
        )}
        <AssignedWork seat={seat} work={work} now={now} />
        <Reports seat={seat} />
      </div>
      <aside className="prof-side" aria-label={`About ${seat.name}`}>
        <About seat={seat} />
      </aside>
    </div>
  );
}
