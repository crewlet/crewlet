/**
 * "Live now" — every seat the engine says is working, what it is on, how far
 * through its turn it is and the last thing it called.
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
import { indexOrg, workingLongestFirst } from "~/lib/seats.ts";
import { LiveDot, LiveTurnRow } from "~/components/LiveTurnRow.tsx";
import type { AgentRow } from "~/protocol/index.ts";

/** How many working seats the card lists before "Open live view". */
export const LIVE_ROWS = 4;

export function LiveNow({ agents, now }: { agents: readonly AgentRow[]; now: number }) {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const working = workingLongestFirst(agents);
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
      ) : (
        <ul className="live-list">
          {working.slice(0, LIVE_ROWS).map((row) => (
            <LiveTurnRow key={row.id} row={row} index={index} now={now} />
          ))}
        </ul>
      )}
    </Card>
  );
}
