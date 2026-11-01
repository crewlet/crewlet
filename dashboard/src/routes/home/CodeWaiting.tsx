/**
 * "Code waiting on a person" — the changes on the company's own code host
 * that an agent cannot move: the ones asking THIS reader's review, and the
 * ones whose readiness waits on a person, whoever that is.
 *
 * TWO OF THE ENGINE'S LISTINGS, their first few rows each: `needs=review`,
 * which names no reviewer and so asks the reader's own seat, and
 * `needs=person` — a change whose first unmet precondition waits on persons
 * alone (a person's approval, a blocker a person raised). Each row opens its
 * change in the peek; the card's link is Code › Changes narrowed to what
 * waits on a person, where the rest are.
 *
 * A READER BOUND TO NO SEAT is asked no review — the engine has nobody to ask
 * it of — so the card lists what waits on a person and says why the first
 * half is not there.
 */

import { Card, EmptyState, Tag } from "@crewlethq/ui";
import { NamedGlyph } from "@crewlethq/icons/glyphs/registry";
import { peekHref, usePeekControls } from "~/app/frame/DetailRail.tsx";
import { QueryState } from "~/components/common.tsx";
import { useCodeQuery } from "~/lib/useCodeQuery.ts";
import { useServicesAsk } from "~/lib/services.ts";
import { useViewer } from "~/lib/viewer.ts";
import type { CodeChangeSummary } from "~/protocol/index.ts";
import { CODE_LIST_POLL_MS } from "~/routes/code/ChangeList.tsx";
import { changeObject, changesHref } from "~/routes/code/paths.ts";

/**
 * How many changes each half lists: THREE, the count "Needs your decision"
 * draws beside it (`HOME_DECISIONS`), so the two cards a reader reads first
 * hold the same weight; the link is where the rest are.
 */
export const HOME_CODE_ROWS = 3;

export function CodeWaiting() {
  const viewer = useViewer();
  const seated = viewer.handle !== "";
  const ask = useServicesAsk().code;
  const mine = useCodeQuery(
    "code_changes",
    { needs: "review", limit: HOME_CODE_ROWS },
    { enabled: ask && seated, pollMs: CODE_LIST_POLL_MS },
  );
  const person = useCodeQuery(
    "code_changes",
    { needs: "person", limit: HOME_CODE_ROWS },
    { enabled: ask, pollMs: CODE_LIST_POLL_MS },
  );
  const asked = mine.data?.changes ?? [];
  const waiting = person.data?.changes ?? [];
  return (
    <Card padding="none" className="home-card">
      <Card.Header
        icon={<NamedGlyph name="diff" size="sm" />}
        actions={
          <a className="t-link" href={changesHref({ needs: "person" })}>
            Open Code
          </a>
        }
      >
        <Card.Title as="h3">Code waiting on a person</Card.Title>
      </Card.Header>
      <Card.Body padding="md" className="col gap-3">
        <section aria-label="Asking your review" className="col gap-1">
          <span className="t-label">Asking your review</span>
          {seated ? (
            <QueryState
              error={mine.error}
              refusal={mine.refusal}
              loading={mine.loading && !mine.data}
            >
              <WaitingRows rows={asked} none="No change is asking your review." />
            </QueryState>
          ) : (
            <span className="t-caption">
              A review is asked of a seat, and you are signed in as {viewer.login || "nobody"},
              bound to none.
            </span>
          )}
        </section>
        <section aria-label="Waiting on a person" className="col gap-1">
          <span className="t-label">Waiting on a person</span>
          <QueryState
            error={person.error}
            refusal={person.refusal}
            loading={person.loading && !person.data}
          >
            {waiting.length === 0 && asked.length === 0 ? (
              <EmptyState
                size="compact"
                headingLevel="none"
                title="Nothing waits on a person"
                description="A change whose approval or blocker only a person can settle appears here."
              />
            ) : (
              <WaitingRows rows={waiting} none="No change waits on a person." />
            )}
          </QueryState>
        </section>
      </Card.Body>
    </Card>
  );
}

/** A half's rows: each change's reference, title and what it waits on, opening its peek. */
function WaitingRows({ rows, none }: { rows: CodeChangeSummary[]; none: string }) {
  const { open } = usePeekControls();
  if (rows.length === 0) return <span className="t-caption">{none}</span>;
  return (
    <ul className="code-person-band__list">
      {rows.map((c) => (
        <li key={c.id}>
          <a
            className="mono"
            href={peekHref(changeObject(c.repo, c.number))}
            onClick={(e) => {
              if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
              e.preventDefault();
              open(changeObject(c.repo, c.number));
            }}
          >
            {c.ref}
          </a>{" "}
          {c.title}
          {c.readiness && <span className="muted"> — {c.readiness.first}</span>}
          {c.state === "draft" && (
            <>
              {" "}
              <Tag appearance="outline">Draft</Tag>
            </>
          )}
        </li>
      ))}
    </ul>
  );
}
