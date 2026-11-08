/**
 * "Code waiting on a person" — the changes on Gitlet that an agent cannot
 * move: the ones asking THIS reader's review, and the ones whose readiness
 * waits on a person, whoever that is.
 *
 * TWO OF THE ENGINE'S LISTINGS, the first few changes each KEEPS:
 * `needs=review`, which names no reviewer and so asks the reader's own seat,
 * and `needs=person` — a change whose first unmet precondition waits on
 * persons alone (a person's approval, a blocker a person raised). Each row
 * opens its change in the peek; the card's link is Gitlet › Changes narrowed to
 * what waits on a person, where the rest are.
 *
 * KEPT, NOT READ: a page of either listing is the rows the engine read, and
 * it drops some of them after (a change of a repository the reader may not
 * read, one that has ended, one whose readiness no longer waits on a person),
 * so a first page can keep none and still carry `next`. Each half follows
 * `next` (`useCodeQuery`'s fill) until it holds its rows or the listing ends,
 * and says "none" only of a listing that ENDED; a reading that stops at its
 * bound first draws what it found and points at the rest, in Gitlet.
 *
 * A READER BOUND TO NO SEAT is asked no review — the engine has nobody to ask
 * it of — so the card lists what waits on a person and says why the first
 * half is not there.
 *
 * GITLET'S QUESTIONS, ASKED ONLY OF A READER HOLDING GITLET (ADR-0110): the
 * engine refuses both listings to anybody else, so for them the card asks
 * neither and says what would open it, in the words every lock uses, as Tasks
 * completed does for Worklet. Until the viewer has answered, neither half is
 * asked and neither says "none": a listing nobody asked found nothing.
 *
 * NO GAP TO SAY: Gitlet's listings carry no flag of what they could
 * not account for, since each is one estate copy's answer at the level it
 * names rather than a gather over nodes, so both halves pass an empty `gaps`
 * and the kit still decides what an empty half says.
 */

import { Card, Tag } from "@crewlethq/ui";
import { NamedGlyph } from "@crewlethq/icons/glyphs/registry";
import { peekHref, usePeekControls } from "~/app/frame/peekAddress.ts";
import { GITLET, needsWords } from "~/lib/access.ts";
import { useCodeQuery } from "~/lib/useCodeQuery.ts";
import { useServicesAsk } from "~/lib/services.ts";
import { useViewer } from "~/lib/viewer.ts";
import type { CodeChangeSummary } from "~/protocol/index.ts";
import { CODE_LIST_POLL_MS } from "~/routes/gitlet/ChangeList.tsx";
import { changeObject, changesHref } from "~/routes/gitlet/paths.ts";
import { WidgetQuery } from "./board/kit.tsx";

/**
 * How many changes each half lists: THREE, the count "Needs your decision"
 * draws beside it (`HOME_DECISIONS`), so the two cards a reader reads first
 * hold the same weight; the link is where the rest are. Each page is asked
 * for as many, and a half reads at most `CHANGES_FILL_PAGES` of them to keep
 * that many ([CodeChangesOptions.fill]).
 */
export const HOME_CODE_ROWS = 3;

export function CodeWaiting() {
  const viewer = useViewer();
  const seated = viewer.handle !== "";
  // ASKED ONLY WHERE THE COMPANY HOSTS CODE AND THE READER HOLDS GITLET; the
  // lock is drawn once the viewer has said they do not.
  const reads = useServicesAsk().code && viewer.gitlet;
  const locked = !viewer.loading && !viewer.gitlet;
  const mine = useCodeQuery(
    "code_changes",
    { needs: "review", limit: HOME_CODE_ROWS },
    { enabled: reads && seated, pollMs: CODE_LIST_POLL_MS, fill: HOME_CODE_ROWS },
  );
  const person = useCodeQuery(
    "code_changes",
    { needs: "person", limit: HOME_CODE_ROWS },
    { enabled: reads, pollMs: CODE_LIST_POLL_MS, fill: HOME_CODE_ROWS },
  );
  const asked = (mine.data?.changes ?? []).slice(0, HOME_CODE_ROWS);
  const waiting = (person.data?.changes ?? []).slice(0, HOME_CODE_ROWS);
  // WHETHER A HALF'S LISTING ENDED: the one answer that lets it say "none".
  // A half not asked (a reader bound to no seat) has nothing more to find.
  const mineEnded = !seated || (mine.data !== null && !mine.data.next);
  const personEnded = person.data !== null && !person.data.next;
  return (
    <Card padding="none" className="home-card">
      <Card.Header
        icon={<NamedGlyph name="diff" size="sm" />}
        actions={
          <a className="t-link" href={changesHref({ needs: "person" })}>
            Open Gitlet
          </a>
        }
      >
        <Card.Title as="h3">Code waiting on a person</Card.Title>
      </Card.Header>
      <Card.Body padding="md" className="col gap-3">
        {locked ? (
          <p className="home-card-note">{needsWords("Seeing Gitlet's changes", [GITLET])}</p>
        ) : (
          <>
            <section aria-label="Asking your review" className="col gap-1">
              <span className="t-label">Asking your review</span>
              {seated ? (
                <WidgetQuery
                  error={mine.error}
                  refusal={mine.refusal}
                  loading={(mine.loading || !reads) && !mine.data}
                  gaps={[]}
                >
                  <WaitingRows
                    rows={asked}
                    ended={mineEnded}
                    none="No change is asking your review."
                    rest={changesHref({ needs: "review" })}
                  />
                </WidgetQuery>
              ) : (
                <span className="t-caption">
                  A review is asked of a seat, and you are signed in as {viewer.login || "nobody"},
                  bound to none.
                </span>
              )}
            </section>
            <section aria-label="Waiting on a person" className="col gap-1">
              <span className="t-label">Waiting on a person</span>
              <WidgetQuery
                error={person.error}
                refusal={person.refusal}
                loading={(person.loading || !reads) && !person.data}
                gaps={[]}
                empty={
                  waiting.length === 0 && asked.length === 0 && personEnded && mineEnded
                    ? {
                        headingLevel: "none",
                        title: "Nothing waits on a person",
                        hint: "A change whose approval or blocker only a person can settle appears here.",
                      }
                    : undefined
                }
              >
                <WaitingRows
                  rows={waiting}
                  ended={personEnded}
                  none="No change waits on a person."
                  rest={changesHref({ needs: "person" })}
                />
              </WidgetQuery>
            </section>
          </>
        )}
      </Card.Body>
    </Card>
  );
}

/**
 * A half's rows: each change's reference, title and what it waits on, opening
 * its peek — and, where the reading stopped at its bound before keeping any,
 * where the rest are rather than a "none" it cannot know (`ended` false).
 */
function WaitingRows({
  rows,
  ended,
  none,
  rest,
}: {
  rows: CodeChangeSummary[];
  ended: boolean;
  none: string;
  rest: string;
}) {
  const { open } = usePeekControls();
  if (rows.length === 0) {
    return ended ? (
      <span className="t-caption">{none}</span>
    ) : (
      <span className="t-caption">
        None among the newest changes read.{" "}
        <a className="t-link prose-link" href={rest}>
          See the rest in Gitlet
        </a>
      </span>
    );
  }
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
          {c.readiness && <span className="muted"> · {c.readiness.first}</span>}
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
