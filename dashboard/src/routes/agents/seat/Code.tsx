/**
 * What a seat does on the company's code host — an agent's or a person's: the
 * changes it owns that are still open, the reviews it gave, and, for an agent,
 * what its days counted.
 *
 * THREE READS, EACH THE ENGINE'S OWN QUESTION, held to the reader's grant on
 * every repository it names (ADR-0058: no app gates code, so the tab is every
 * reader's, and a repository the reader may not read is simply not in its
 * rows):
 *
 * - its OPEN changes, `code_changes?owner=&state=open` — the owner listing's
 *   index walks one state at a time (an owner alone narrows no walk, so the
 *   engine lists an owner's changes in a state, never in every one), so the
 *   tab lists the open ones and Code › Changes, narrowed to the owner, is
 *   where the other states are;
 * - the reviews it GAVE, `code_reviews?reviewer=`, newest first;
 * - and, for an agent seat, the counters its replicated usage days carry over
 *   the Overview's week (`seat_activity`: `changes_opened`, `reviews_given`,
 *   `findings_received` — the last counted on the OWNER of the changes the
 *   findings were raised on, never on the reviewer). A person's openings and
 *   reviews count on no seat's day, so a human seat's tab draws none.
 *
 * COUNTS ARE NOT QUALITY. The three figures say how much happened, as the
 * Overview's turn tiles do; a reader judging a seat's work reads its reviews
 * and the changes' readiness, which are a click away.
 */

import { Card, EMPTY_VALUE, StatCard, StatGroup } from "@crewlethq/ui";
import { QueryState } from "~/components/common.tsx";
import { fmtCount } from "~/lib/format.ts";
import { useQuery } from "~/lib/useQuery.ts";
import type { Seat } from "~/lib/seats.ts";
import { ChangeList, CODE_LIST_POLL_MS, ReviewList } from "~/routes/code/ChangeList.tsx";
import { changesHref } from "~/routes/code/paths.ts";
import { ACTIVITY_DAYS } from "./Overview.tsx";

export function SeatCode({ seat, human }: { seat: Seat; human: boolean }) {
  const handle = seat.handle;
  return (
    <div className="col gap-4">
      {!human && <CodeCounters handle={handle} />}
      <Card padding="none">
        <Card.Header
          actions={
            <a className="t-link" href={changesHref({ owner: handle })}>
              Open in Code
            </a>
          }
        >
          <Card.Title as="h3">Open changes</Card.Title>
        </Card.Header>
        <ChangeList
          params={{ owner: handle, state: "open" }}
          enabled={handle !== ""}
          empty={{
            title: "No open change",
            hint: human
              ? "A change they open, from a push or from their assistant, is listed here until it merges or closes."
              : "A change this seat opens with code_open_change, or from a coding run, is listed here until it merges or closes.",
          }}
        />
      </Card>
      <Card padding="none">
        <Card.Header>
          <Card.Title as="h3">Reviews given</Card.Title>
        </Card.Header>
        <ReviewList
          reviewer={handle}
          empty={{
            title: "No reviews given",
            hint: "Every review this seat gives a change is listed here, newest first.",
          }}
        />
      </Card>
    </div>
  );
}

/**
 * The seat's week on the code host, from the same `seat_activity` answer the
 * Overview's tiles read — the same question with the same parameters, so the
 * two tabs share one read rather than asking twice.
 */
function CodeCounters({ handle }: { handle: string }) {
  const activity = useQuery(
    "seat_activity",
    { seat: handle, days: ACTIVITY_DAYS, previous: true },
    { enabled: handle !== "", pollMs: CODE_LIST_POLL_MS },
  );
  const row = activity.data?.seats?.find((r) => r.handle === handle);
  return (
    <QueryState error={activity.error} refusal={activity.refusal} loading={activity.loading}>
      <StatGroup columns={3}>
        <StatCard
          label="Changes opened"
          value={row ? fmtCount(row.changes_opened) : EMPTY_VALUE}
          sub={`the last ${ACTIVITY_DAYS} days`}
        />
        <StatCard
          label="Reviews given"
          value={row ? fmtCount(row.reviews_given) : EMPTY_VALUE}
          sub={`the last ${ACTIVITY_DAYS} days`}
        />
        <StatCard
          label="Findings received"
          value={row ? fmtCount(row.findings_received) : EMPTY_VALUE}
          sub="raised on the changes it owns"
        />
      </StatGroup>
    </QueryState>
  );
}
