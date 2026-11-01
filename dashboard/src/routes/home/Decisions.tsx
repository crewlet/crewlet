/**
 * "Needs your decision" — the first few things only this reader can settle,
 * each answerable where it stands.
 *
 * WHAT IT HOLDS: the open asks put to this person and the coding runs parked
 * on a question to them (the engine's `decisions`), and the seats the engine
 * stopped for a spent token budget that THIS reader can resolve, by raising
 * the ceiling or handing the item on (`seatDecisionsFor` decides which).
 * Newest first, three of them, and the Inbox for the rest: the card's foot
 * says how many more wait there, under the last decision it shows, where a
 * reader who has answered these looks for the next.
 *
 * NEVER A ZERO FOR NOBODY: an anonymous reader has no record and no
 * decisions, and the card says what would give them some rather than drawing
 * an empty list that claims to have looked. An UNBOUND reader is somebody
 * (their record is their login) and is asked like anybody.
 */

import { Card, Tag } from "@crewlethq/ui";
import { TriangleAlertGlyph } from "@crewlethq/icons/glyphs";
import { DecisionList, decisionsHref } from "~/components/DecisionRow.tsx";
import { decisionSubjects, type SeatCondition } from "~/lib/decisions.ts";
import type { ViewerState } from "~/lib/viewer.ts";
import type { QueryErrorCode } from "~/contract/errors.ts";
import type { DecisionsAnswer, LogRefusal, QueryRefusal } from "~/protocol/index.ts";
import { WidgetQuery, gapsOf } from "./board/kit.tsx";

/** What a reader signed in as nobody is told: a decision is a person's, so
 *  there is nothing to list until the browser is somebody. */
export const NOBODY_SENTENCE = "Sign in to see your decisions.";

/** How many decisions the landing screen shows before "Open inbox". */
export const HOME_DECISIONS = 3;

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
  const subjects = decisionSubjects(answer, seatConditions);
  const total = typeof answer?.total === "number" ? answer.total + seatConditions.length : null;
  const more = total === null ? 0 : total - Math.min(subjects.length, HOME_DECISIONS);
  // THE INBOX IS WORKLET'S: an asked person without Worklet answers here and
  // on the task, and a link to a screen that would only say it is closed to
  // them is not a way in. Nothing listed, nothing to go on to.
  const foot = viewer.worklet && Boolean(viewer.owner) && subjects.length > 0;

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
            subjects.length === 0
              ? {
                  headingLevel: "none",
                  title: "No decisions waiting",
                  hint: "When an agent asks you something, it appears here with the way to answer.",
                }
              : undefined
          }
        >
          <DecisionList
            subjects={subjects}
            decider={{ handle: viewer.owner }}
            now={now}
            limit={HOME_DECISIONS}
            clampTitles
          />
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
