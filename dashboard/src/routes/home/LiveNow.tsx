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
import { indexOrg } from "~/lib/seats.ts";
import { runningShortList } from "~/lib/turns.ts";
import { LiveDot, LiveTurnRow } from "~/components/LiveTurnRow.tsx";
import type { AgentRow } from "~/protocol/index.ts";

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
      {working.length === 0 ? (
        <div className="home-quiet">
          <strong className="t-cell">No agent is working right now</strong>
          <span className="t-caption">A seat appears here the moment a turn starts.</span>
        </div>
      ) : shown.length === 0 ? (
        // WORKING, AND NOT YET ON A TURN THIS VIEW CAN NAME: a coding run is
        // out for a seat whose turn record has not reached the projection. An
        // empty list under a count of them would read as a list that failed.
        <div className="home-quiet">
          <strong className="t-cell">
            {plural(working.length, "seat")} working, on turns not named yet
          </strong>
          <span className="t-caption">The live view lists each one.</span>
        </div>
      ) : (
        <ul className="live-list">
          {shown.map((row) => (
            <LiveTurnRow key={row.id} row={row} index={index} now={now} />
          ))}
        </ul>
      )}
    </Card>
  );
}
