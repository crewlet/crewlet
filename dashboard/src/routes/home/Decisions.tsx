/**
 * "Needs your decision" — the first few things only this reader can settle,
 * each answerable where it stands.
 *
 * WHAT IT HOLDS: the open asks put to this person and the coding runs parked
 * on a question to them (the engine's `decisions`), and the seats the engine
 * stopped for a spent token budget that THIS reader can resolve, by raising
 * the ceiling or handing the item on (`seatDecisionsFor` decides which).
 * Newest first, three of them, and the Inbox for the rest.
 *
 * NEVER A ZERO FOR NOBODY: an anonymous reader has no record and no
 * decisions, and the card says what would give them some rather than drawing
 * an empty list that claims to have looked. An UNBOUND reader is somebody
 * (their record is their login) and is asked like anybody.
 */

import { Card, EmptyState, Tag } from "@crewlethq/ui";
import { TriangleAlertGlyph } from "@crewlethq/icons/glyphs";
import { QueryState } from "~/components/common.tsx";
import { DecisionList, decisionsHref } from "~/components/DecisionRow.tsx";
import { decisionSubjects, type SeatCondition } from "~/lib/decisions.ts";
import type { ViewerState } from "~/lib/viewer.ts";
import type { QueryErrorCode } from "~/contract/errors.ts";
import type { DecisionsAnswer, LogRefusal, QueryRefusal } from "~/protocol/index.ts";

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

  return (
    <Card padding="none" className="home-card">
      <Card.Header
        icon={<TriangleAlertGlyph size="sm" className="home-decisions-icon" />}
        actions={
          // THE INBOX IS WORKLET'S: an asked person without Worklet
          // answers here and on the task, and a link to a screen that would
          // only say it is closed to them is not a way in.
          viewer.worklet ? (
            <a className="t-link" href={decisionsHref()}>
              Open inbox
            </a>
          ) : undefined
        }
      >
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
        <QueryState error={error} refusal={refusal} loading={loading && !answer}>
          {subjects.length === 0 ? (
            // WHAT THE CARD IS FOR, not "nothing" a third time: the greeting
            // and the figure above already say none is waiting.
            <EmptyState
              size="compact"
              headingLevel="none"
              title="No decisions waiting"
              description="When an agent asks you something, it appears here with the way to answer."
            />
          ) : (
            <DecisionList
              subjects={subjects}
              decider={{ handle: viewer.owner }}
              now={now}
              limit={HOME_DECISIONS}
            />
          )}
        </QueryState>
      )}
    </Card>
  );
}
