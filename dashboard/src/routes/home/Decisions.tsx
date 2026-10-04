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
 * an empty list that claims to have looked. An UNBOUND reader is somebody —
 * their record is their login — and is asked like anybody.
 */

import { Card, Tag } from "@crewlethq/ui";
import { InfoGlyph, TriangleAlertGlyph } from "@crewlethq/icons/glyphs";
import { QueryState } from "~/components/common.tsx";
import {
  DecisionRow,
  decisionSubjects,
  decisionsHref,
  subjectKey,
  type SeatCondition,
} from "~/components/DecisionRow.tsx";
import type { ViewerState } from "~/lib/viewer.ts";
import type { QueryErrorCode } from "~/contract/errors.ts";
import type { DecisionsAnswer, LogRefusal, QueryRefusal } from "~/protocol/index.ts";

/** What a reader signed in as nobody is told: a decision is a person's, so
 *  there is nothing to list until the browser is somebody. */
export const NOBODY_SENTENCE = "Sign in to see what waits on your decision.";

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
  /** Why the read was refused, beside `error` — what `QueryState` names. */
  refusal: QueryRefusal | LogRefusal | null;
  loading: boolean;
  seatConditions: readonly SeatCondition[];
  now: number;
}) {
  const subjects = decisionSubjects(answer, seatConditions);
  const total = typeof answer?.total === "number" ? answer.total + seatConditions.length : null;
  const shown = subjects.slice(0, HOME_DECISIONS);

  return (
    <Card padding="none" className="home-card">
      <Card.Header
        icon={<TriangleAlertGlyph size="sm" className="home-decisions-icon" />}
        actions={
          <a className="t-link" href={decisionsHref()}>
            Open inbox
          </a>
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
          {shown.length === 0 ? (
            <div className="home-quiet">
              <strong className="t-cell">Nothing needs your decision</strong>
              <span className="t-caption">
                No question is put to you, no coding run is waiting on your answer, and no seat is
                stopped on its budget.
              </span>
            </div>
          ) : (
            <ul className="decision-list">
              {shown.map((subject) => (
                <DecisionRow
                  key={subjectKey(subject)}
                  subject={subject}
                  decider={{ handle: viewer.handle }}
                  now={now}
                />
              ))}
            </ul>
          )}
        </QueryState>
      )}
      <p className="home-card-foot">
        <InfoGlyph size="sm" aria-hidden="true" />
        <span>
          Agents only reach you for what their own authority cannot decide. Everything else is in
          the activity below.
        </span>
      </p>
    </Card>
  );
}
