/**
 * One person's day — the seven claims on their attention, and the feed.
 *
 * # Seven claims, and the feed that reached them
 *
 * A person who saw only their assignments would miss six other things asking
 * for their time: what a lead put at the top of their list, the questions
 * waiting on an answer, the sub-items they claimed on somebody else's task,
 * the work they were brought onto without owning, what moved on what they
 * follow, and what became workable while they were not looking. Each is a
 * different claim, and folding them into one list is how six of them go
 * unnoticed. The eighth block is not a claim at all: `work_inbox` is what
 * REACHED them, and the one reason of twenty it reached them under.
 *
 * # Every block is bounded the same
 *
 * Twenty rows each, so no block can crowd out another — the same rule the
 * engine's own answer follows, for the same reason: this is read as one page
 * and a person with two hundred assignments would otherwise never see their
 * asks. The inbox block takes that same bound — see [INBOX_ROWS] — and not the
 * engine's own inbox ceiling of fifty, which belongs to the screen that IS
 * somebody's inbox.
 *
 * # Every number here is over the page it is on
 *
 * `work_inbox` says so on the field: `unread` and `primary` are counts over the
 * rows the answer returned, never totals. So a figure drawn here carries a word
 * saying what it counts. The count pill is the ROWS UNDER IT, the way that slot
 * reads on every other card in this product, and the unread half is said in
 * prose beside it. It was the other way round — the pill held the unread tally
 * and the subtitle opened with the row count — which is two bare digits side by
 * side, each denying the other's meaning, and on a quiet day both of them `0`.
 *
 * # Whose day it is decides the pronoun, everywhere
 *
 * Including the wake reasons. `reasonPhrase` is the second person — "assigned
 * to you" — and on an operator reading a report's day that is a sentence about
 * the reader, who is not on the list. That is the exact failure `reasonAbout`
 * was written for, and this is the only screen in the product that renders
 * somebody else's notices, so it is the only one that has to choose.
 *
 * # Read-only, like every other work screen
 *
 * Nothing here writes. A priority list is reordered through the engine's own
 * write path, attributed to whoever did it; a button here would write as "the
 * dashboard", which is not a person and cannot be asked why.
 */

import { useMemo } from "react";
import { plainText, renderMarkdown } from "~/lib/markdown.ts";
import { href, useParam } from "~/app/router.tsx";
import { QueryState, SeatChip } from "~/components/common.tsx";
import { AsksTag, Coverage, RowList, type RowChrome } from "~/components/work.tsx";
import { Callout, Card, Count, EmptyState, Select, StatCard, StatGroup, Tag } from "@crewlethq/ui";
import { ErrorGlyph, HelpGlyph, InboxGlyph, KeyGlyph } from "@crewlethq/icons/glyphs";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg } from "~/lib/seats.ts";
import { relTime } from "~/lib/format.ts";
import { useViewer } from "~/lib/viewer.ts";
import { reasonAbout, reasonPhrase } from "~/lib/reasons.ts";
import { useNow } from "~/lib/clock.ts";
import type { WorkAskRow, WorkChecklistRow, WorkSummary } from "~/protocol/index.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";

/**
 * How many notices the inbox block carries.
 *
 * TWENTY, which is the bound the engine puts on each of the seven blocks beside
 * it (`tracker.MyWorkRows`). It was twelve, a number with no anchor at the call
 * site and none anywhere else — and this page's whole rule is that no block may
 * crowd out another, which a block bounded below its seven neighbours breaks in
 * the one direction nobody notices: it runs out first, and a truncated feed
 * reads as a quiet one.
 *
 * NOT the engine's own ceiling of fifty (`tracker.MaxInboxRows`). That is what
 * the Inbox screen asks for, because that screen IS somebody's inbox; this is
 * one card on somebody's day, and fifty notices here would be most of the page.
 * Anything above fifty is clamped by the reader, so this is a request it can
 * always serve whole.
 */
const INBOX_ROWS = 20;

export function MyWork() {
  const org = useOrg();
  const now = useNow();
  const [handle, setHandle] = useParam("handle", "");
  // EVERY SEAT AND EVERY PERSON the chart names, so the screen can be
  // reached with nobody chosen and still offer somebody.
  const index = useMemo(() => indexOrg(org), [org]);
  // THE PICKER OFFERS NAMES AND SENDS HANDLES. A list of slugs is the
  // database's vocabulary; the person choosing knows their colleagues by name.
  const handles = useMemo(
    () =>
      index.seats
        .map((s) => ({ value: s.handle, label: s.name }))
        .sort((a, b) => a.label.localeCompare(b.label)),
    [index],
  );
  const chrome: RowChrome = {
    seatName: (handle) => index.byHandle.get(handle)?.name ?? handle,
  };
  // WHOSE DAY THIS IS, resolved rather than guessed.
  //
  // This fell back to `handles[0]` — the ALPHABETICALLY FIRST SEAT — so a
  // screen titled "My work" rendered a stranger's day to everybody, and the
  // engine had no way to tell it otherwise because `work_my_work` demanded a
  // handle and was registered operator-only. The `viewer` question answers it:
  // the engine resolves this browser to a principal, and the identity
  // directory binds that principal to a seat.
  //
  // An explicit choice still wins — an operator reading a report's day is a
  // real thing to do, and the header says whose day it is either way.
  //
  // YOURS IS `owner`, NOT `handle`: the one name your own record is kept
  // under, which is your seat when the directory binds you to one and your
  // login when it does not. Read by `handle`, an unbound reader had no day at
  // all here while their assistant wrote their priorities under their login.
  const viewer = useViewer();
  const whose = handle || viewer.owner;
  // WHOSE DAY DECIDES THE PRONOUN. This screen is read two ways — a person
  // reading their own day, and an operator reading a report's — and one
  // wording cannot serve both: "nothing has reached them" on your own inbox
  // reads as a screen describing somebody else, which is exactly the
  // confusion the `viewer` question exists to end.
  const ownDay = whose !== "" && whose === viewer.owner;
  const they = ownDay ? "you" : "them";
  // NOT UNTIL SOMEBODY IS CHOSEN — the same guard the board takes. `whose`
  // is empty until the chart has loaded, and the engine
  // refuses this question without a handle.
  //
  // PUSHED on the reader's own day: the record that hands somebody work is the
  // record that writes them a notice, so the frame that moves their inbox is
  // the one that moves this. On a report's day no frame arrives — the shell
  // watches the viewer's own seat — and the poll is what keeps it current.
  const state = useQuery("work_my_work", whose ? { handle: whose } : undefined, {
    enabled: whose !== "",
    pollMs: 30_000,
    refetchOnInboxOf: whose,
  });
  const mine = state.data;

  // AND WHAT REACHED THEM, which is a different question from what is on
  // them. `work_inbox` names the ONE reason of twenty under which each change
  // found this person — the fact no commercial tracker records — and the
  // reader behind it has existed, tested and swept on a 365-day retention,
  // since the tracker did.
  const inbox = useQuery("work_inbox", whose ? { handle: whose, limit: INBOX_ROWS } : undefined, {
    enabled: whose !== "",
    pollMs: 30_000,
    refetchOnInboxOf: whose,
  });
  const notices = inbox.data?.notices ?? [];
  // THE REASON VOCABULARY, IN THE VOICE OF WHOSE DAY THIS IS. `reasonPhrase` is
  // the second person — "assigned to you" — which on a report's day is a
  // sentence about the reader, who is not on the list.
  const reasonWord = ownDay ? reasonPhrase : reasonAbout;
  // AND THE CARD'S FOOTER AS ONE STRING. `Card.Footer` is a flex row that does
  // not wrap, so two element children sit on one line and squeeze each other;
  // one text node is a single anonymous flex item and wraps like prose. The
  // primary split is ABSENT rather than half-written when the engine has not
  // stated one — "counting as primary: ." is worse than saying nothing.
  const primary = (inbox.data?.primary_reasons ?? []).map(reasonWord);
  const inboxNote = [
    primary.length
      ? `Of the reasons below, ${primary.join(" · ")} count as primary for ${ownDay ? "you" : "them"}.`
      : "",
    `Read and snoozed marks are written by ${ownDay ? "your" : "this person’s"} own assistant, through mark_inbox — the dashboard shows what it recorded.`,
  ]
    .filter(Boolean)
    .join(" ");

  return (
    <>
      {/* WHOSE DAY, said out loud. An operator reading a report's day is a
          real thing to do, and a screen called "My work" showing somebody
          else's without saying so is how a reader acts on work that is not
          theirs. */}
      <PageActions>
        {whose ? (
          // ONE PILL, WHATEVER IT SAYS. This took `success` — the POSITIVE
          // STATUS hue, a soft green tint under green ink — when the day was
          // your own, and the neutral outline when it was somebody else's, so
          // the only thing either the hue or the fill separated was two people.
          // Colour carries STATE here and never IDENTITY (`styles/tokens.css`;
          // uilet's own tone doc: "a tone says what a thing IS, never who it
          // is"), and a green pill in the page bar reads as a verdict on the day
          // rather than a label on the page.
          //
          // IT WAS ALSO THE WRONG WAY ROUND. The reader who must not miss this
          // tag is the operator on somebody ELSE'S day — acting on work that is
          // not theirs is the failure the tag exists to prevent — and that was
          // the branch wearing the quiet outline (`crewlet-tag--neutral` under
          // `--outline` drops to the tertiary ink) while the harmless branch
          // wore the green. So both take the same pill, at the louder of the two
          // registers, and the WORDS carry whose day it is: a name is stable,
          // legible, and does not run out at eight.
          //
          // `ownDay` rather than a fourth spelling of `whose === viewer.owner`
          // — inside this guard `whose` is non-empty, so the two are the same
          // question, and one question spelled per prop is how one prop came to
          // disagree with the rule while its neighbours did not.
          <Tag>{ownDay ? "yours" : `${index.byHandle.get(whose)?.name ?? whose}’s day`}</Tag>
        ) : undefined}
        {
          <a className="t-link" href={href(["work"])}>
            Tracker →
          </a>
        }
      </PageActions>
      <PageNote>
        Everything one person is expected to look at — their priorities, what they hold, the
        questions waiting on them, and what became workable while they were away.
      </PageNote>

      <div className="toolbar">
        {/* THE EMPTY OPTION IS A REAL ROW, not their `placeholder`: choosing
            nobody is a state this screen has — it is how a reader gets back to
            "pick somebody" — and a placeholder is only ever the label over an
            unset value, with nothing to select. */}
        <Select
          // A PICKER IN A TOOLBAR, not a field on a form. uilet's Select is
          // `width: 100%` unless told otherwise, and its own doc says why that
          // is wrong here: "a filter row of full-width selects is one question
          // per line, which is not what a filter bar is".
          width="auto"
          value={whose}
          onChange={(value) => setHandle(String(value))}
          ariaLabel="Whose"
          placeholder="Pick somebody"
          active={whose !== ""}
          options={[{ value: "", label: "Pick somebody" }, ...handles]}
        />
        <span className="spacer" />
        <Coverage answer={mine} />
      </div>

      {/* THREE STATES, and they are not one empty state. A reader nobody
          has signed in, a reader the directory binds to no seat, and a reader
          who simply has not chosen somebody need three different sentences.
          The unbound reader is not an EMPTY state any more: they have a day
          of their own, kept under their login, and what they lack is the
          work the chart addresses to a seat — which is what the note says,
          above the day rather than instead of it. */}
      {ownDay && viewer.unbound && (
        <Callout variant="info">
          The directory binds <code className="inline">{viewer.login}</code> to no seat, so this is
          the day kept under that login: what you follow, what names you, and your own priorities.
          Work the org chart hands to a seat reaches you once{" "}
          <code className="inline">crewlet iam bind</code> binds you to one.
        </Callout>
      )}
      {!whose &&
        (viewer.anonymous ? (
          <EmptyState
            icon={<KeyGlyph size={32} />}
            title="Nobody is signed in"
            description="A day belongs to a person, and this browser has not said who it is. Sign in, or pick somebody below to read their day."
          />
        ) : (
          <EmptyState
            icon={<InboxGlyph size={32} />}
            title="Nobody chosen"
            description="A day belongs to somebody."
          />
        ))}

      {whose && (
        <QueryState error={state.error} loading={state.loading}>
          {mine && (
            <>
              {/* COVERAGE IS A BANNER, never swallowed: rows may be missing,
                  and a day rendered as complete when it is not is a person
                  who thinks they are done. */}
              {mine.complete === false && (
                <Callout variant="warning">
                  This node could not account for every change yet, so a block may be short.
                </Callout>
              )}
              <StatGroup columns={4}>
                <StatCard
                  label="Priorities"
                  value={mine.priorities.length}
                  sub="in the stored order"
                />
                <StatCard label="Assigned" value={mine.assigned.length} />
                <StatCard
                  label="Asked"
                  value={mine.asked_of_me.length}
                  icon={mine.asked_of_me.length ? <ErrorGlyph size="xs" /> : undefined}
                  sub="waiting on an answer"
                />
                <StatCard
                  label="Unblocked"
                  value={mine.unblocked_recent.length}
                  sub="newly workable"
                />
              </StatGroup>

              {/* WHAT REACHED THEM, and WHY. Every other block on this
                  screen answers "what is on you"; this one answers "what
                  happened that you were told about", which is the question
                  an inbox is. The reason is the row's opening fact because
                  it is the one nothing else in this category records. */}
              <Card padding="none">
                <Card.Header
                  icon={<InboxGlyph size="sm" />}
                  // THE SUBTITLE CARRIES THE FIGURE THE PILL CANNOT, WITH A WORD
                  // ON IT. Both numbers are over the PAGE — the answer says so
                  // on the field — so neither is a total, and two bare digits
                  // beside each other said nothing about which was which. The
                  // bound is stated unconditionally rather than only when the
                  // page fills, which is the rule `FacetRail` already keeps for
                  // the same reason: a number is read as "how many there are"
                  // until something says otherwise, and a sentence that appears
                  // only sometimes is one nobody learns to look for.
                  //
                  // GUARDED ON THE ANSWER, NEVER ON THE FIELD. `unread` is sent
                  // on every answer, so `?? 0` there could only ever substitute
                  // a made-up figure for a missing one.
                  subtitle={
                    inbox.data
                      ? `${inbox.data.unread} unread · the ${INBOX_ROWS} most recent`
                      : undefined
                  }
                >
                  <Card.Title>{ownDay ? "Reached you" : "Reached them"}</Card.Title>
                  {/* THE COUNT IS RENDERED HERE rather than through
                      `Card.Header`'s own `count`, which passes no `label` to
                      `Count`: a reader on a screen reader then hears "Reached
                      you, 12" with nothing to say what twelve is. It draws in
                      the same place — the header puts `children` into the name
                      block immediately before its own pill — and it is the ROWS
                      UNDER IT, which is what that slot means on every other card
                      here. A pill disagreeing with what the reader can count is
                      the defect this replaced.
                      DRAWN ONLY ONCE THERE IS AN ANSWER: this card is gated on
                      `work_my_work` and the inbox is a second round trip, so a
                      pill built from `notices.length` would have said `0` —
                      "nothing reached you" — before anything had been asked. */}
                  {inbox.data && <Count value={notices.length} label="notices on this page" />}
                </Card.Header>
                <QueryState
                  error={inbox.error}
                  loading={inbox.loading}
                  empty={
                    notices.length
                      ? undefined
                      : {
                          title: `Nothing has reached ${they}`,
                          hint: `A notice is written when a change names somebody — as an assignee, a mention, a question, a watcher. ${ownDay ? "Your" : "This person's"} record has none.`,
                        }
                  }
                >
                  <div className="list">
                    {notices.map((notice) => (
                      <div
                        key={notice.record_id}
                        className="thread-entry"
                        style={{ opacity: notice.read ? 0.72 : 1 }}
                      >
                        <div className="row gap-1">
                          {notice.subject_key && (
                            <a className="mono t-link" href={href(["work", notice.subject_key])}>
                              {notice.subject_key}
                            </a>
                          )}
                          <span className="truncate t-cell" style={{ flex: 1 }}>
                            {plainText(notice.excerpt ?? "") || notice.kind.replace(/_/g, " ")}
                          </span>
                          {/* THE ONE MARK, shared with the item page's Woke
                              panel — which drew the same fact as a TINT on the
                              reason chip instead, in the accent ground a pressed
                              filter chip takes. Two marks for one engine fact is
                              two facts to a reader. */}
                          {notice.addressed && <AsksTag />}
                          <Tag
                            appearance="outline"
                            title={notice.fallback ? "nobody better was found" : undefined}
                          >
                            {reasonWord(notice.reason)}
                          </Tag>
                          <span className="t-caption">{relTime(notice.at, now)}</span>
                        </div>
                        {notice.actor && (
                          <span className="t-caption">
                            by {chrome.seatName?.(notice.actor) ?? notice.actor}
                            {notice.actor_kind && notice.actor_kind !== "seat"
                              ? ` (${notice.actor_kind})`
                              : ""}
                          </span>
                        )}
                      </div>
                    ))}
                  </div>
                </QueryState>
                <Card.Footer variant="meta">
                  {/* THE PRIMARY SPLIT BELONGS HERE, not on the header line. The
                      subtitle is one row that truncates — `flex-shrink: 100`,
                      `text-overflow: ellipsis` — and the shipped default is
                      eight reasons, so the sentence explaining the card was cut
                      to two words on every render it ever made. A footer wraps,
                      and it is the only place a reason list can be read beside
                      the tags it describes. */}
                  {inboxNote}
                </Card.Footer>
              </Card>

              <Asks rows={mine.asked_of_me} now={now} chrome={chrome} ownDay={ownDay} />
              <TaskBlock
                title="Priorities"
                hint="What somebody put at the top of this list, in the order they put it."
                rows={mine.priorities}
                now={now}
                chrome={chrome}
              />
              <TaskBlock title="Assigned" rows={mine.assigned} now={now} chrome={chrome} />
              <TaskBlock
                title="Unblocked"
                hint="Work whose blockers have all finished — the one block about a change rather than a state."
                rows={mine.unblocked_recent}
                now={now}
                chrome={chrome}
              />
              <Checklist rows={mine.checklist_items} />
              <TaskBlock
                title="Collaborating"
                hint="Brought on without owning."
                rows={mine.collaborating}
                now={now}
                chrome={chrome}
              />
              <TaskBlock title="Watching" rows={mine.watching_recent} now={now} chrome={chrome} />
            </>
          )}
        </QueryState>
      )}
    </>
  );
}

/** A block of tasks, ABSENT when it is empty rather than drawn as a shrug: a
 *  page of seven "nothing here" panels buries the one that has something. */
export function TaskBlock({
  title,
  hint,
  rows,
  now,
  chrome,
}: {
  title: string;
  hint?: string;
  rows: WorkSummary[];
  now: number;
  chrome?: RowChrome;
}) {
  if (rows.length === 0) return null;
  // THE TRACKER'S OWN ROW, so a task looks the same here as it does on the
  // board it came from: this page used to render four of a row's facts and
  // the board six, and only one of the two knew a task could be blocked.
  return (
    <Card padding="none">
      <Card.Header subtitle={hint} count={rows.length}>
        <Card.Title>{title}</Card.Title>
      </Card.Header>
      <RowList rows={rows} now={now} chrome={chrome} hrefOf={(row) => href(["work", row.key])} />
    </Card>
  );
}

/** The questions waiting on this person.
 *
 *  FIRST on the page, because an unanswered question is the only block where
 *  somebody else is blocked on THIS person rather than the other way round. */
export function Asks({
  rows,
  now,
  chrome,
  // WHOSE QUESTIONS THESE ARE, and the DEFAULT IS SOMEBODY ELSE'S. This block
  // is rendered on a report's day here and on every seat page, which is written
  // in the third person throughout — so a caller that says nothing is a caller
  // that has not claimed the rows are the reader's, and the safe reading of an
  // unstated owner is "not yours". That is the same lesson this screen's own
  // handle resolution records: it guessed, and told everybody they were reading
  // their own day.
  ownDay = false,
}: {
  rows: WorkAskRow[];
  now: number;
  chrome?: RowChrome;
  ownDay?: boolean;
}) {
  if (rows.length === 0) return null;
  return (
    <Card>
      <Card.Header icon={<HelpGlyph size="sm" />} count={rows.length}>
        <Card.Title>{ownDay ? "Asked of you" : "Asked of them"}</Card.Title>
      </Card.Header>
      <div className="col gap-3">
        {rows.map((ask) => (
          // THE CAUTION RAIL, the same mark a question wears in a thread:
          // this is the one block where somebody else is blocked on THIS
          // person rather than the other way round.
          <div key={ask.comment} className="comment work-ask">
            <div className="row gap-2 wrap">
              <a className="mono t-link" href={href(["work", ask.key])}>
                {ask.key}
              </a>
              <span className="truncate">{ask.title}</span>
              <span className="spacer" />
              <SeatChip
                name={chrome?.seatName?.(ask.asked_by) ?? ask.asked_by}
                handle={ask.asked_by}
              />
              <span className="muted">{relTime(ask.asked_at, now)}</span>
            </div>
            <div className="prose md">{renderMarkdown(ask.body)}</div>
          </div>
        ))}
      </div>
    </Card>
  );
}

/** Sub-items claimed on somebody else's task.
 *
 *  Its own block because no assignee filter over tasks reaches one: a person
 *  holding six checklist items and no assignment reads their queue as empty. */
export function Checklist({ rows }: { rows: WorkChecklistRow[] }) {
  if (rows.length === 0) return null;
  return (
    <Card>
      <Card.Header count={rows.length} subtitle="On other people's tasks.">
        <Card.Title>Checklist items</Card.Title>
      </Card.Header>
      {rows.map((item) => (
        <div key={`${item.task}:${item.item}`} className={`work-check${item.done ? " done" : ""}`}>
          <a className="mono t-link" href={href(["work", item.task_key])}>
            {item.task_key}
          </a>
          <span className="work-check-name">{item.name}</span>
          <span className="spacer" />
          <span className="muted truncate">{item.task_title}</span>
        </div>
      ))}
    </Card>
  );
}
