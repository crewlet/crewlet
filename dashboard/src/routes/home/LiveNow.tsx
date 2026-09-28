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
import { Card, StatusDot, Stepper, Tag } from "@crewlethq/ui";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import { href } from "~/app/router.tsx";
import { fmtElapsed } from "~/lib/format.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg, stateLine, type OrgIndex } from "~/lib/seats.ts";
import { lastCallLine, turnSteps } from "~/lib/turnsteps.ts";
import type { AgentRow } from "~/protocol/index.ts";

/** How many working seats the card lists before "Open live view". */
export const LIVE_ROWS = 4;

export function LiveNow({ agents, now }: { agents: readonly AgentRow[]; now: number }) {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const working = agents
    .filter((a) => a.activity === "working")
    // LONGEST RUNNING FIRST: the turn that has been going longest is the one
    // a reader is most likely looking for.
    .sort(
      (a, b) =>
        (Date.parse(a.turn?.started_at ?? "") || 0) - (Date.parse(b.turn?.started_at ?? "") || 0),
    );
  return (
    <Card padding="none" className="home-card">
      <Card.Header
        icon={
          <StatusDot tone={working.length > 0 ? "info" : "neutral"} pulse={working.length > 0} />
        }
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
            <LiveRow key={row.id} row={row} index={index} now={now} />
          ))}
        </ul>
      )}
    </Card>
  );
}

function LiveRow({ row, index, now }: { row: AgentRow; index: OrgIndex; now: number }) {
  const handle = row.handle ?? "";
  const seat = index.byHandle.get(handle) ?? null;
  const name = seat?.name ?? row.role;
  const call = row.live_call;
  const started = Date.parse(row.turn?.started_at ?? call?.started_at ?? "");
  const turnId = row.turn?.turn_id ?? call?.turn_id ?? "";
  // THE ONE DERIVATION every running-turn row shares — see lib/turnsteps.ts.
  const { steps, current } = turnSteps(row);
  const last = lastCallLine(row);
  const body = (
    <>
      <SeatAvatar name={name} kind="agent" ring="info" size="sm" />
      <span className="live-body">
        <span className="live-head">
          <span className="live-name">{name}</span>
          <span className="live-doing">{stateLine(row, { now, seat })}</span>
          <span className="live-elapsed">
            {Number.isFinite(started) ? fmtElapsed(now - started) : ""}
          </span>
        </span>
        <Stepper label={`${name}'s turn`} steps={steps} current={current} pulse />
        {last && <span className="live-call mono">{last}</span>}
      </span>
    </>
  );
  return (
    <li>
      {turnId ? (
        <a className="live-row" href={href(["live", "turns", turnId])}>
          {body}
        </a>
      ) : (
        <span className="live-row">{body}</span>
      )}
    </li>
  );
}
