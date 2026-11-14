/**
 * "Needs your decision" — the first few things only this reader can settle,
 * each answerable where it stands.
 *
 * WHAT IT HOLDS: the open asks put to this person and the executors parked
 * on a question to them (the engine's `decisions`), and the seats the engine
 * stopped for a spent token budget that THIS reader can resolve, by raising
 * the ceiling or handing the item on (`seatDecisionsFor` decides which).
 * Newest first, three of them, and the Inbox for the rest: the card's foot
 * says how many more wait there, under the last decision it shows, where a
 * reader who has answered these looks for the next.
 *
 * AND WHAT THEY MAY UNBLOCK, under it: the parked runs this reader may
 * answer, as the seat's lead or an administrator, that wait on somebody else
 * (a delegate, a colleague). The engine lists them on the reader's own
 * record and says which they are (`lib/decisions.ts`), so they are drawn as a
 * second group, "Waiting on others, you can answer", with the same review and
 * controls, and counted by neither the card's figure nor the badge: nobody has
 * asked the reader yet. One its delegates escalated, or left past their
 * deadline, is the reader's to decide and is in the first group.
 *
 * NEVER A ZERO FOR NOBODY: an anonymous reader has no record and no
 * decisions, and the card says what would give them some rather than drawing
 * an empty list that claims to have looked. An UNBOUND reader is somebody
 * (their record is their login) and is asked like anybody; but no decision is
 * put to a sign-in that holds no seat, so where nothing is listed the card
 * says that plainly, and how to sign in as somebody who is asked, rather
 * than "when an agent asks you something", which no agent ever will.
 */

import { Card, Tag } from "@crewlethq/ui";
import { TriangleAlertGlyph } from "@crewlethq/icons/glyphs";
import { DecisionList, decisionsHref } from "~/components/DecisionRow.tsx";
import { UnboundRemedy } from "~/components/people.tsx";
import { decisionSubjects, splitDecisions, type SeatCondition } from "~/lib/decisions.ts";
import type { ViewerState } from "~/lib/viewer.ts";
import type { QueryErrorCode } from "~/contract/errors.ts";
import type { DecisionsAnswer, LogRefusal, QueryRefusal } from "~/protocol/index.ts";
import { WidgetQuery, gapsOf } from "./board/kit.tsx";

/** What a reader signed in as nobody is told: a decision is a person's, so
 *  there is nothing to list until the browser is somebody. */
export const NOBODY_SENTENCE = "Sign in to see your decisions.";

/** How many decisions the landing screen shows before "Open inbox". */
export const HOME_DECISIONS = 3;

/** The heading over what the reader may answer that waits on somebody else. */
export const OTHERS_HEADING = "Waiting on others, you can answer";

/** What a sign-in holding no seat is told where nothing is listed. */
export const NO_SEAT_SENTENCE = "No decision is put to this sign-in: it holds no seat.";

export function Decisions({
  viewer,
  answer,
  error,
  refusal,
  loading,
  seatConditions,
  now,
}: {
  viewer: ViewerState;
  answer: DecisionsAnswer | null;
  error: QueryErrorCode | null;
  /** Why the read was refused, beside `error`: what `QueryState` names. */
  refusal: QueryRefusal | LogRefusal | null;
  loading: boolean;
  seatConditions: readonly SeatCondition[];
  now: number;
}) {
  const { yours: subjects, others } = splitDecisions(decisionSubjects(answer, seatConditions));
  const total = typeof answer?.total === "number" ? answer.total + seatConditions.length : null;
  const more = total === null ? 0 : total - Math.min(subjects.length, HOME_DECISIONS);
  const otherMore = Math.max(0, (answer?.answerable ?? others.length) - HOME_DECISIONS);
  // THE INBOX IS WORKLET'S: an asked person without Worklet answers here and
  // on the task, and a link to a screen that would only say it is closed to
  // them is not a way in. Nothing listed, nothing to go on to.
  const foot = viewer.worklet && Boolean(viewer.owner) && subjects.length + others.length > 0;

  return (
    <Card padding="none" className="home-card">
      <Card.Header icon={<TriangleAlertGlyph size="sm" className="home-decisions-icon" />}>
        <Card.Title as="h3">
          Needs your decision
          {total !== null && total > 0 && (
            <Tag size="xs" variant="warning" className="home-card-count">
              {`${total.toLocaleString()}${answer?.capped ? "+" : ""}`}
            </Tag>
          )}
        </Card.Title>
      </Card.Header>
      {!viewer.owner && !viewer.loading ? (
        <p className="home-card-note">{NOBODY_SENTENCE}</p>
      ) : (
        <WidgetQuery
          error={error}
          refusal={refusal}
          loading={loading && !answer}
          gaps={gapsOf([answer])}
          // WHAT THE CARD IS FOR, not "nothing" a third time: the greeting
          // and the figure above already say none is waiting.
          empty={
            subjects.length === 0 && others.length === 0
              ? viewer.unbound
                ? {
                    headingLevel: "none",
                    title: NO_SEAT_SENTENCE,
                    hint: <UnboundRemedy to="be asked here" />,
                  }
                : {
                    headingLevel: "none",
                    title: "No decisions waiting",
                    hint: "When an agent asks you something, it appears here with the way to answer.",
                  }
              : undefined
          }
        >
          {subjects.length > 0 ? (
            <DecisionList
              subjects={subjects}
              decider={{ handle: viewer.owner }}
              now={now}
              limit={HOME_DECISIONS}
              clampTitles
            />
          ) : (
            <p className="home-card-note home-card-note--lead">
              {viewer.unbound ? NO_SEAT_SENTENCE : "Nothing waits on you."}
            </p>
          )}
          {others.length > 0 && (
            <section aria-label={OTHERS_HEADING} className="home-decisions-others">
              <h4 className="home-decisions-others-head t-label">
                {OTHERS_HEADING}
                <Tag size="xs" variant="neutral" className="home-card-count">
                  {(answer?.answerable ?? others.length).toLocaleString()}
                </Tag>
              </h4>
              <DecisionList
                subjects={others}
                decider={{ handle: viewer.owner }}
                now={now}
                limit={HOME_DECISIONS}
                clampTitles
              />
              {otherMore > 0 && foot && (
                <a className="t-link home-card-more" href={decisionsHref()}>
                  {`${otherMore.toLocaleString()} more in Inbox`}
                </a>
              )}
            </section>
          )}
        </WidgetQuery>
      )}
      {foot && (
        <Card.Footer variant="meta">
          <a className="t-link" href={decisionsHref()}>
            {more > 0
              ? `Show ${more.toLocaleString()}${answer?.capped ? "+" : ""} more in Inbox`
              : "Open inbox"}
          </a>
        </Card.Footer>
      )}
    </Card>
  );
}
