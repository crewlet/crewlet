/**
 * "Live now" — how many seats the engine says are working, and the longest-
 * running four of their turns: what each is on, how far through its turn it
 * is and the last thing it called. The rest are the live view's.
 *
 * THE ENGINE'S WORD, from the agents push: a seat is here exactly when its
 * `activity` is `working`, so this card, the header's faces and the first tile
 * can never disagree about who is working. What a seat is doing is
 * `lib/seats.ts`'s `stateLine` — the item is the one the turn is CHARGED to,
 * never a work key a prompt happened to mention.
 */

import { useMemo } from "react";
import { Card, Tag } from "@crewlethq/ui";
import { href } from "~/app/router.tsx";
import { useOrg } from "~/lib/store-hooks.ts";
import { plural } from "~/lib/format.ts";
import { indexOrg, seatPath } from "~/lib/seats.ts";
import { runningShortList } from "~/lib/turns.ts";
import { LiveDot, LiveRoom, LiveTurnRow } from "~/components/LiveTurnRow.tsx";
import type { AgentRow } from "~/protocol/index.ts";
import { WidgetQuery } from "./board/kit.tsx";

/** How many working seats with no named turn the card names before it counts the rest. */
const NAMED_SEATS = 3;

export function LiveNow({ agents, now }: { agents: readonly AgentRow[]; now: number }) {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  // THE SIDEBAR'S RUNNING GROUP'S OWN SELECTION, so the two name the same
  // seats in the same order: the count is every working seat, and the rows
  // are the first four whose turn has an id — a seat whose turn has none yet
  // is counted here and listed on Live › Now running, which "Open live view"
  // goes to.
  const { working, shown } = runningShortList(agents);
  return (
    <Card padding="none" className="home-card">
      <Card.Header
        icon={<LiveDot running={working.length} />}
        actions={
          <a className="t-link" href={href(["live"])}>
            Open live view
          </a>
        }
      >
        <Card.Title as="h3">
          Live now
          {working.length > 0 && (
            <Tag size="xs" variant="info" className="home-card-count">
              {working.length.toLocaleString()}
            </Tag>
          )}
        </Card.Title>
      </Card.Header>
      <LiveRoom>
        {working.length === 0 ? (
          // THE KIT'S CLAIM, saying what the card is for: the greeting already
          // says nobody is working. Drawn through WidgetQuery, as every Home
          // card's claim of nothing is; the agents push says nothing it could
          // not account for, so there is no gap to say.
          <WidgetQuery
            error={null}
            loading={false}
            gaps={[]}
            empty={{
              headingLevel: "none",
              title: "No agent is working",
              hint: "A running turn appears here with its steps and the way to watch it.",
            }}
          />
        ) : shown.length === 0 ? (
          // WORKING, AND ON NO TURN THE ENGINE HAS NAMED: the seats are said by
          // name, each a way to its own page, which says what it is doing.
          // It promised the turns would appear here "once they are" named,
          // which a seat whose turn ended without its record reaching the
          // projection never is, so the promise stood for hours.
          <WidgetQuery
            error={null}
            loading={false}
            gaps={[]}
            empty={{
              headingLevel: "none",
              title: `${plural(working.length, "seat")} working`,
              hint: (
                <>
                  {"The engine reports no turn for "}
                  {working.slice(0, NAMED_SEATS).map((row, i) => {
                    const name = index.byHandle.get(row.handle ?? "")?.name ?? row.role;
                    return (
                      <span key={row.id}>
                        {i > 0
                          ? i === Math.min(working.length, NAMED_SEATS) - 1
                            ? " and "
                            : ", "
                          : ""}
                        <a
                          className="t-link"
                          href={href(seatPath({ handle: row.handle ?? "", name }))}
                        >
                          {name}
                        </a>
                      </span>
                    );
                  })}
                  {working.length > NAMED_SEATS ? ` and ${working.length - NAMED_SEATS} more` : ""}
                  {working.length === 1
                    ? ". Its page says what it is doing."
                    : ". Each one's page says what it is doing."}
                </>
              ),
            }}
          />
        ) : (
          <ul className="live-list">
            {shown.map((row) => (
              <LiveTurnRow key={row.id} row={row} index={index} now={now} />
            ))}
          </ul>
        )}
      </LiveRoom>
    </Card>
  );
}
