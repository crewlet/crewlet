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
 * NEVER A ZERO FOR NOBODY: an anonymous reader or a token bound to no seat has
 * no decisions, and the card says what would give them some rather than
 * drawing an empty list that claims to have looked.
 */

import { Card, Tag } from "@crewlethq/ui";
import { TriangleAlertGlyph } from "@crewlethq/icons/glyphs";
import { QueryState } from "~/components/common.tsx";
import {
  DecisionList,
  decisionSubjects,
  decisionsHref,
  type SeatCondition,
} from "~/components/DecisionRow.tsx";
import type { ViewerState } from "~/lib/viewer.ts";
import type { QueryErrorCode } from "~/contract/errors.ts";
import type { DecisionsAnswer } from "~/protocol/index.ts";

/** What a reader signed in as nobody is told: a decision is a person's, so
 *  there is nothing to list until the browser is somebody. */
export const NOBODY_SENTENCE = {
  anonymous: "Set an API token bound to your seat to see what waits on your decision.",
  unbound:
    "This token is not bound to a person — set contact.crewlet_operator_id on your seat to see what waits on your decision.",
} as const;

/** How many decisions the landing screen shows before "Open inbox". */
export const HOME_DECISIONS = 3;

export function Decisions({
  viewer,
  answer,
  error,
  loading,
  seatConditions,
  now,
}: {
  viewer: ViewerState;
  answer: DecisionsAnswer | null;
  error: QueryErrorCode | null;
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
      {!viewer.handle && !viewer.loading ? (
        <p className="home-card-note">
          {NOBODY_SENTENCE[viewer.anonymous ? "anonymous" : "unbound"]}
        </p>
      ) : (
        <QueryState error={error} loading={loading && !answer}>
          {subjects.length === 0 ? (
            <div className="home-quiet">
              <strong className="t-cell">Nothing needs your decision</strong>
              <span className="t-caption">
                No question is put to you, no coding run is waiting on your answer, and no seat is
                stopped on its budget.
              </span>
            </div>
          ) : (
            <DecisionList
              subjects={subjects}
              decider={{ handle: viewer.handle }}
              now={now}
              limit={HOME_DECISIONS}
            />
          )}
        </QueryState>
      )}
      <p className="home-card-foot">
        Agents only bring you what their own authority can't decide. Everything else is in the
        activity below.
      </p>
    </Card>
  );
}
