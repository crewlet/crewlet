/**
 * What is on a seat's plate — an agent's or a person's.
 *
 * TWO HALVES, AND THEY ARE NOT THE SAME READ. The list is `work_items
 * {assignee}`, which is ungated: every reader of this page gets it, and it is
 * the floor. The blocks under it are `work_my_work`, which the engine scopes
 * to the caller's own record, so they are asked for a person reading their
 * own page and for a `fleet:operate` holder. One tab, because the question a
 * reader has is "what is this seat doing", and the answer is simply fuller
 * when they are entitled to more of it.
 *
 * THE TRACKER'S OWN ROW AND THE PERSON'S OWN BLOCKS, imported rather than
 * redrawn: `components/work.tsx` states the rule — one renderer, screens pick
 * the density — and a per-seat list that drew its own columns is exactly how
 * the board came to know a task could be blocked while the personal page did
 * not.
 */

import { Card, Skeleton } from "@crewlethq/ui";
import { href } from "~/app/router.tsx";
import { AskList } from "~/components/DecisionRow.tsx";
import { QueryState } from "~/components/common.tsx";
import {
  ChecklistClaims,
  Coverage,
  RowList,
  TaskBlock,
  type RowChrome,
} from "~/components/work.tsx";
import { HoldWrites } from "~/lib/useWriteAccess.ts";
import { useQuery, type QueryResult } from "~/lib/useQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import type { Seat } from "~/lib/seats.ts";
import type { WorkItemsAnswer } from "~/protocol/index.ts";

export function Work({
  seat,
  work,
  chrome,
  now,
}: {
  seat: Seat;
  work: QueryResult<WorkItemsAnswer>;
  chrome: RowChrome;
  now: number;
}) {
  const handle = seat.handle;
  const viewer = useViewer();
  // THE SEVEN CLAIMS, and only where the reader may have them — the same
  // rule the person record follows: its owner, and `fleet:operate`, the
  // admin path of the owner-or-lead rule (never `people:manage`, which opens
  // directory rows and nobody's work). A lead is answered by the engine.
  const mayRead = viewer.operatesFleet || (viewer.handle !== "" && viewer.handle === handle);
  const mine = useQuery(
    "work_my_work",
    { handle },
    { enabled: mayRead && handle !== "", pollMs: 30_000 },
  );
  const rows = work.data?.items ?? [];
  return (
    <div className="col gap-4">
      <Coverage answer={work.data} />
      {work.loading && !work.data && (
        <Skeleton variant="text" rows={4} rowHeight={44} label="Loading this seat's work" />
      )}
      <QueryState
        error={work.error}
        refusal={work.refusal}
        loading={work.loading}
        empty={
          work.data && rows.length === 0
            ? {
                title: "Nothing open is assigned to them",
                hint: "Work reaches a seat by assignment, and closed work is not counted here.",
              }
            : undefined
        }
      >
        <Card padding="none">
          <Card.Header
            // THE ENGINE'S COUNT, which is the set's — the list is its first
            // page, and a card counting its own rows would say fifty of a
            // seat holding two hundred.
            count={
              work.data ? `${work.data.total_hint}${work.data.total_capped ? "+" : ""}` : undefined
            }
            actions={
              <a className="t-link" href={href(["work"], { assignee: handle })}>
                Open in Work
              </a>
            }
          >
            <Card.Title as="h3">Assigned and open</Card.Title>
          </Card.Header>
          <RowList
            rows={rows}
            now={now}
            chrome={chrome}
            hrefOf={(row) => href(["work", row.key])}
          />
        </Card>
      </QueryState>

      {/* THE SCOPED HALF, and its absence is a sentence rather than a gap: a
          reader without the credential is not missing a feature, they are
          reading somebody else's queue. */}
      {mayRead ? (
        <>
          {/* THE QUESTIONS PUT TO THIS SEAT, as the row Home and My work draw
              — answerable only by the seat they are put to, which the engine
              enforces: on anybody else's page the answers are HELD with that
              sentence, rather than offered and refused. */}
          {(mine.data?.asked_of_me.length ?? 0) > 0 && (
            <Card padding="none">
              <Card.Header count={mine.data?.totals?.asked_of_me.total}>
                <Card.Title as="h3">Waiting on their answer</Card.Title>
              </Card.Header>
              <HoldWrites
                reason={
                  viewer.handle === handle
                    ? null
                    : `Asked of ${seat.name} — only they can answer it.`
                }
              >
                <AskList
                  rows={mine.data?.asked_of_me ?? []}
                  decider={{ handle, name: viewer.handle === handle ? undefined : seat.name }}
                  now={now}
                />
              </HoldWrites>
            </Card>
          )}
          {/* EACH BLOCK COUNTS ITS WHOLE CLAIM, the engine's total: a
              block is a page of at most twenty rows, and its own length
              would say twenty of a queue of forty. */}
          <TaskBlock
            title="What they mean to do first"
            hint="Their own order, as they set it."
            total={mine.data?.totals?.priorities}
            rows={mine.data?.priorities ?? []}
            now={now}
            chrome={chrome}
            hrefOf={(row) => href(["work", row.key])}
          />
          <TaskBlock
            title="Collaborating"
            hint="Tasks they are named on without owning."
            total={mine.data?.totals?.collaborating}
            rows={mine.data?.collaborating ?? []}
            now={now}
            chrome={chrome}
            hrefOf={(row) => href(["work", row.key])}
          />
          <ChecklistClaims
            rows={mine.data?.checklist_items ?? []}
            hrefOf={(key) => href(["work", key])}
          />
        </>
      ) : (
        <p className="t-caption">
          Their own queue — what they mean to do first, the questions put to them and their
          checklist items on other seats&apos; tasks — is theirs to read. Signing in as them, or as
          somebody holding fleet:operate, shows it here.
        </p>
      )}
    </div>
  );
}
