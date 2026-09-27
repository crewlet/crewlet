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
import { delegatedWorkers, indexOrg, stateLine, type OrgIndex } from "~/lib/seats.ts";
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

/** The three phases a turn walks, in order. */
const PHASES = ["context", "execute", "review"] as const;

function LiveRow({ row, index, now }: { row: AgentRow; index: OrgIndex; now: number }) {
  const handle = row.handle ?? "";
  const seat = index.byHandle.get(handle) ?? null;
  const name = seat?.name ?? row.role;
  const call = row.live_call;
  const parked = row.turn?.stage === "parked";
  const phase = parked ? "execute" : (call?.phase ?? row.current_phase ?? "context");
  const current = phase === "review" ? "review" : phase === "execute" ? "execute" : "context";
  const started = Date.parse(row.turn?.started_at ?? call?.started_at ?? "");
  const turnId = row.turn?.turn_id ?? call?.turn_id ?? "";
  const workers = delegatedWorkers(call);
  const round = call ? Math.max(call.rounds_used, (call.round_num ?? -1) + 1) : 0;
  const executeLabel =
    current !== "execute"
      ? "Execute"
      : parked
        ? "Execute · coding run"
        : workers > 0
          ? "Execute · workers"
          : round > 0
            ? `Execute · round ${round}${call?.max_rounds ? ` of ${call.max_rounds}` : ""}`
            : "Execute";
  const steps = PHASES.map((id) => ({
    id,
    label:
      id === "context"
        ? phase === "onboarding"
          ? "Onboarding"
          : "Context"
        : id === "execute"
          ? executeLabel
          : "Review",
  }));
  const last = lastCall(row);
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

/**
 * The call the seat is making now, or the last one it made this phase:
 * `sandbox.run go test ./...` — the tool's name and its arguments' values, as
 * one line the card cuts at its edge.
 */
function lastCall(row: AgentRow): string {
  const call = row.live_call;
  if (!call) return "";
  if (call.running_call)
    return `${call.running_call.name} ${argWords(call.running_call.arguments)}`.trim();
  const done = call.tool_executions?.at(-1);
  if (!done) return "";
  const name = done.name ?? done.tool ?? "";
  return `${name} ${argWords(done.arguments ?? done.args)}`.trim();
}

/** A call's arguments as their values, space-separated — what a person reads
 *  a call by — or the raw text where they are not an object. */
function argWords(args: unknown): string {
  let value = args;
  if (typeof value === "string") {
    try {
      value = JSON.parse(value);
    } catch {
      return value as string;
    }
  }
  if (value && typeof value === "object" && !Array.isArray(value)) {
    return Object.values(value as Record<string, unknown>)
      .map((v) => (typeof v === "string" ? v : JSON.stringify(v)))
      .join(" ");
  }
  return value == null ? "" : JSON.stringify(value);
}
