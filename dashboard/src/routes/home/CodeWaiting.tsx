/**
 * "Code waiting on a person" — the changes on Gitlet that an agent cannot
 * move: the ones asking THIS reader's review, and the ones whose readiness
 * waits on a person, whoever that is.
 *
 * TWO OF THE ENGINE'S LISTINGS, the first few changes each KEEPS:
 * `needs=review`, which names no reviewer and so asks the reader's own seat,
 * and `needs=person` — a change whose first unmet precondition waits on
 * persons alone (a person's approval, a blocker a person raised). Each row
 * is Gitlet's change row, compact, opening its change in the peek, and the card
 * publishes one peek order across both halves; its link is Gitlet › Changes
 * narrowed to what waits on a person, where the rest are.
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
 * NOTHING WAITING IS SAID ONCE. When both halves were asked and both listings
 * ended with nothing kept, the card draws one empty state in place of the two
 * halves; otherwise each half that found none says so on one line of its own.
 * The claim used to belong to the second half alone, so an empty card drew
 * the same fact two ways: a caption under "Asking your review", then a full
 * empty state with its glyph under "Waiting on a person".
 *
 * NO GAP TO SAY: Gitlet's listings carry no flag of what they could
 * not account for, since each is one estate copy's answer at the level it
 * names rather than a gather over nodes, so both halves pass an empty `gaps`
 * and the kit still decides what an empty half says.
 */

import { useMemo } from "react";
import { Card } from "@crewlethq/ui";
import { NamedGlyph } from "@crewlethq/icons/glyphs/registry";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { GITLET, needsWords } from "~/lib/access.ts";
import { useCodeQuery } from "~/lib/useCodeQuery.ts";
import { useServicesAsk } from "~/lib/services.ts";
import { useViewer } from "~/lib/viewer.ts";
import type { CodeChangeSummary } from "~/protocol/index.ts";
import { CODE_LIST_POLL_MS } from "~/routes/gitlet/ChangeList.tsx";
import { ChangeRows } from "~/routes/gitlet/ChangeRow.tsx";
import { changeObject, changesHref } from "~/routes/gitlet/paths.ts";
import { WidgetQuery } from "./board/kit.tsx";
import "~/styles/gitlet.css";

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
    { needs: "review", limit: HOME_CODE_ROWS, cards: true },
    { enabled: reads && seated, pollMs: CODE_LIST_POLL_MS, fill: HOME_CODE_ROWS },
  );
  const person = useCodeQuery(
    "code_changes",
    { needs: "person", limit: HOME_CODE_ROWS, cards: true },
    { enabled: reads, pollMs: CODE_LIST_POLL_MS, fill: HOME_CODE_ROWS },
  );
  const asked = useMemo(() => (mine.data?.changes ?? []).slice(0, HOME_CODE_ROWS), [mine.data]);
  const waiting = useMemo(
    () => (person.data?.changes ?? []).slice(0, HOME_CODE_ROWS),
    [person.data],
  );
  // WHETHER A HALF'S LISTING ENDED: the one answer that lets it say "none".
  // A half not asked (a reader bound to no seat) has nothing more to find.
  const mineEnded = !seated || (mine.data !== null && !mine.data.next);
  const personEnded = person.data !== null && !person.data.next;
  // BOTH HALVES ASKED AND BOTH ENDED WITH NOTHING: the one fact, said once. A
  // reader bound to no seat keeps the first half's sentence about why it was
  // not asked, so for them the halves stay.
  const nothing = seated && asked.length === 0 && waiting.length === 0 && mineEnded && personEnded;
  // ONE PEEK ORDER ACROSS THE CARD: the asked half, then the waiting half, each change once.
  const order = useMemo(() => {
    const seen = new Set<string>();
    return [...asked, ...waiting]
      .filter((c) => !seen.has(c.id) && Boolean(seen.add(c.id)))
      .map((c) => changeObject(c.repo, c.number));
  }, [asked, waiting]);
  usePeekNeighbours(order);
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
        ) : nothing ? (
          <WidgetQuery
            error={null}
            loading={false}
            gaps={[]}
            empty={{
              headingLevel: "none",
              title: "Nothing waits on a person",
              hint: "A change asking your review, or one whose approval or blocker only a person can settle, appears here.",
            }}
          />
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
                    label="Asking your review"
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
              >
                <WaitingRows
                  label="Waiting on a person"
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
 * A half's rows, Gitlet's compact change rows; and, where the reading stopped
 * at its bound before keeping any, where the rest are rather than a "none" it
 * cannot know (`ended` false).
 */
function WaitingRows({
  label,
  rows,
  ended,
  none,
  rest,
}: {
  label: string;
  rows: CodeChangeSummary[];
  ended: boolean;
  none: string;
  rest: string;
}) {
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
  return <ChangeRows rows={rows} label={label} density="compact" neighbours={false} />;
}
