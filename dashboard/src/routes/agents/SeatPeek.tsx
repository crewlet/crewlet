/**
 * One seat, beside the chart or the list it was opened from.
 *
 * # What a peek answers
 *
 * "Who is this, and what is it doing right now?" — the state card first (the
 * engine's state line, how long the turn has run, which turn on its task and
 * which round of how many), then the handful of facts a reader asks next:
 * who it reports to, which model it runs on and which one is serving the call
 * in flight, each capped budget window, which node holds it, how much work it
 * holds, where its tools come from, and what it is for. Everything else is
 * the profile's, one press away.
 *
 * # At most three reads, and none a reader will be refused
 *
 * The seat, its state, its model chain and tool sources, and its budget
 * windows all arrive with the org projection and the `agents` push, so they
 * cost nothing. What does cost a read:
 *
 *   * `work_workload` — the seat's open work, for every reader;
 *   * `fleet` — which node holds the seat and since when — OPERATOR-ONLY, so
 *     it is asked only of an operator; every other reader is told what the
 *     public health push says (`heldBy`: this node, by name, or another),
 *     exactly as the profile's Setup card tells them, rather than being sent
 *     a refusal to draw or told a fact already on their screen is withheld;
 *   * `work_item_turns` — which turn this is on its task ("Turn 2") — asked
 *     only while the seat is working on one.
 *
 * The company document is NOT read: the model chain a turn resolves is on the
 * public projection, so a per-peek read of the whole guarded configuration
 * (on every `[`/`]` step through a list) would buy nothing.
 *
 * # Message is a task that asks
 *
 * There is no person-to-seat chat channel. Message opens the one New task
 * sheet with the seat as assignee and `ask` set, so the question is filed as
 * work the seat owes an answer on, and the answer lands in the asker's Inbox.
 */

import { useMemo } from "react";
import { ButtonLink, Callout, EmptyState, EmptyValue, Meter, StatusDot, Tag } from "@crewlethq/ui";
import { UserGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { MessageSeatButton } from "~/components/writes.tsx";
import { useNow } from "~/lib/clock.ts";
import { fmtCount, fmtElapsed, fmtMinute, fmtTime, plural, readerDay } from "~/lib/format.ts";
import { useAgents, useEngineHealth, useOrg } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import {
  activityOf,
  awaitingPerson,
  handleLabel,
  heldBy,
  indexOrg,
  nameOfIn,
  ringOf,
  roundOf,
  seatPath,
  stateLine,
  toneOf,
  type Seat,
} from "~/lib/seats.ts";
import { unitPath } from "~/lib/orgchart.ts";
import type { AgentRow, BudgetWindow, LiveCall, LiveTurn } from "~/protocol/types.ts";
import { useSandboxes } from "~/lib/store-hooks.ts";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import { PERIOD_WORDS } from "~/lib/budget.ts";
import { renderInline } from "~/lib/markdown.ts";
import { companyCeilings } from "./seat/profile.ts";

/** How many tool sources the peek names before "+n". */
const TOOL_CHIPS = 3;

/** A tool source in the words a reader uses: `mcp:gitlab` is "gitlab". */
export function toolSourceLabel(source: string): string {
  if (source === "builtin") return "built-in";
  return source.startsWith("mcp:") ? source.slice(4) : source;
}

/**
 * The label and figure of one capped window: "Budget this week" and
 * "630k of 1M · resets Mon 00:00".
 */
export function budgetLine(w: BudgetWindow): { label: string; figure: string } {
  return {
    label: `Budget ${PERIOD_WORDS[w.period]}`,
    figure: `${fmtCount(w.used)} of ${fmtCount(w.limit ?? 0)} · resets ${fmtMinute(w.resets_at)}`,
  };
}

/**
 * Which turn this is on the task it is charged to, from the task's own turn
 * list (newest first): the running turn's own ordinal once a segment of it is
 * recorded (a coding run parks the turn and resumes it), else the one after
 * the newest recorded — the running turn is written when it ends. Null when
 * the list is not in hand.
 */
export function turnOrdinal(
  turnId: string,
  newest: { turn_id: string; ordinal: number } | undefined,
  answered: boolean,
): number | null {
  if (!answered) return null;
  if (!newest) return 1;
  if (newest.turn_id === turnId) return newest.ordinal || null;
  return newest.ordinal + 1;
}

/** "Turn 2 · round 7 of 25 · coding run in progress". */
function turnFacts(
  turn: LiveTurn | null | undefined,
  call: LiveCall | null | undefined,
  ordinal: number | null,
): string {
  const parts: string[] = [];
  if (ordinal) parts.push(`Turn ${ordinal}`);
  // ONE-BASED, through the one reading every running-turn row shares
  // (`lib/turnsteps.ts`): `round_num` is the ZERO-based round in flight, and
  // read raw it named a round one lower than the stepper on the profile the
  // peek opens.
  const round = roundOf(call);
  if (round > 0) {
    parts.push(call?.max_rounds ? `round ${round} of ${call.max_rounds}` : `round ${round}`);
  }
  if (turn?.stage === "parked") parts.push("coding run in progress");
  return parts.join(" · ");
}

export function SeatPeek({ handle }: { handle: string }) {
  const org = useOrg();
  const agents = useAgents();
  const index = useMemo(() => indexOrg(org), [org]);
  const seat = index.byHandle.get(handle) ?? index.byName.get(handle);
  if (!seat) {
    // NOT AN EMPTY RAIL. A `peek=seat:` reaches this from a pasted or
    // hand-edited URL as often as from a card, so the honest answer names the
    // handle that resolved to nothing rather than a header over no seat.
    return (
      <EmptyState
        size="compact"
        icon={<UserGlyph size={32} />}
        title={`No seat called “${handle}”`}
        description="Seats are addressed by handle. A company revision may have renamed or removed this one."
      />
    );
  }
  return (
    <SeatPeekBody
      seat={seat}
      agent={agents.find((a) => a.role === seat.name)}
      hierarchy={index.hierarchy}
      nameOf={nameOfIn(index)}
    />
  );
}

function SeatPeekBody({
  seat,
  agent,
  hierarchy,
  nameOf,
}: {
  seat: Seat;
  agent: AgentRow | undefined;
  hierarchy: boolean;
  nameOf: (key: string) => string;
}) {
  const now = useNow();
  const viewer = useViewer();
  const org = useOrg();
  const health = useEngineHealth();
  const sandboxes = useSandboxes();
  const human = seat.kind === "human";
  const state = human ? undefined : activityOf(agent);
  const ring = human ? undefined : ringOf(state);
  const turn = agent?.turn ?? null;
  const call = agent?.live_call ?? null;
  const item = call?.work_item?.key || turn?.work_item?.key || "";

  // THE THREE READS — see the file's doc.
  const workload = useQuery("work_workload", undefined, { pollMs: 60_000 });
  const fleet = useQuery("fleet", undefined, { enabled: viewer.operator, pollMs: 60_000 });
  const turns = useQuery(
    "work_item_turns",
    { id: item, limit: 1 },
    { enabled: state === "working" && item !== "" },
  );

  const ordinal =
    turn && item
      ? turnOrdinal(turn.turn_id, turns.data?.turns?.[0], !!turns.data && turns.data.key === item)
      : null;
  const started = turn?.started_at ?? call?.started_at;
  const elapsed = started ? now - Date.parse(started) : null;
  const facts = turnFacts(turn, call, ordinal);
  const load = workload.data?.rows?.find((r) => r.handle === seat.handle);
  const lease = fleet.data?.seats?.find((s) => s.handle === seat.handle);
  const windows = agent?.budget?.windows ?? [];
  const chain = seat.raw.llm?.["execute"] ?? [];
  const tools = seat.raw.tool_sources ?? [];
  const sandbox = sandboxes.find((s) => s.role === seat.name) ?? null;
  const place = unitPath(seat.unit);

  return (
    <div className="seat-peek">
      <header className="seat-peek-head">
        <SeatAvatar
          name={seat.name}
          size="lg"
          kind={human ? "human" : "agent"}
          {...(ring ? { ring } : {})}
          decorative
        />
        <div className="seat-peek-who">
          <h2 className="seat-peek-name">{seat.name}</h2>
          <span className="seat-peek-sub">
            {[handleLabel(seat.handle), place || "Above every unit"].filter(Boolean).join(" · ")}
          </span>
        </div>
      </header>

      {/* THE STATE CARD: the engine's line, how long, and which turn and
          round. Tinted by the state, which is the one hue a seat carries. */}
      <section className="seat-peek-state" data-state={state ?? "human"} aria-label="Doing now">
        <span className="seat-peek-line">
          <StatusDot tone={human ? "neutral" : toneOf(state)} pulse={state === "working"} />
          <span>
            {stateLine(agent, { now, seat, nameOf })}
            {state === "working" && elapsed !== null && ` · ${fmtElapsed(elapsed)}`}
          </span>
        </span>
        {state === "working" && facts && <span className="seat-peek-facts">{facts}</span>}
        {human && (
          <span className="seat-peek-facts">
            The engine never runs a human seat, so there is no turn, model or token budget here.
          </span>
        )}
      </section>

      {agent?.last_error && (
        <Callout variant="danger">
          <strong>{agent.last_error.kind || "error"}</strong> — {agent.last_error.message}
        </Callout>
      )}
      {sandbox && awaitingPerson(sandbox.status) && (
        // THE ONE THING A READER CAN ACT ON from here: a run parked on a
        // question holds the seat until somebody answers it.
        <Callout variant="warning">
          A coding run is waiting on a question: {sandbox.question || "(no question recorded)"}
        </Callout>
      )}

      <dl className="seat-peek-facts-grid">
        <dt>Reports to</dt>
        <dd>
          {seat.manager ? (
            <a className="seat-peek-person" href={href(seatPath(seat.manager))}>
              <SeatAvatar
                name={seat.manager.name}
                size="xs"
                kind={seat.manager.kind === "human" ? "human" : "agent"}
                decorative
              />
              {seat.manager.name}
            </a>
          ) : hierarchy ? (
            "Nobody — the top of the chart"
          ) : (
            <EmptyValue label="Not reported by this engine" />
          )}
        </dd>

        {!human && (
          <>
            <dt>Model</dt>
            <dd>
              {chain.length ? (
                <span>
                  <span className="mono">{chain[0]}</span>
                  {chain.length > 1 && (
                    <span className="muted"> +{plural(chain.length - 1, "fallback")}</span>
                  )}
                  {call?.model && (
                    <span className="seat-peek-serving">serving now: {call.model}</span>
                  )}
                </span>
              ) : (
                <EmptyValue label="No provider configured" />
              )}
            </dd>

            {windows.length ? (
              windows.map((w) => {
                const line = budgetLine(w);
                return (
                  <BudgetFact key={w.period} window={w} label={line.label} figure={line.figure} />
                );
              })
            ) : (
              <>
                <dt>Budget</dt>
                <dd>
                  {/* THE COMPANY'S CEILINGS BIND EVERY SEAT, so a seat with
                      none of its own is capped by those where there are any. */}
                  {companyCeilings(org?.token_budget)
                    ? `No seat budget — ${companyCeilings(org?.token_budget)}`
                    : "No budget — nothing caps this seat's tokens"}
                </dd>
              </>
            )}

            <dt>Running on</dt>
            <dd>
              {!viewer.operator ? (
                // WHAT THE PUBLIC PUSH SAYS, as the profile says it: this
                // node by name, or another — never which peer, which is the
                // operator's fleet read.
                <span className="muted">{heldBy(seat.handle, agent, health)}</span>
              ) : lease ? (
                <span className="seat-peek-node">
                  <StatusDot tone="success" />
                  {lease.node}
                  {lease.acquired_at && ` · since ${sinceWords(lease.acquired_at, now)}`}
                </span>
              ) : fleet.data ? (
                "No node holds it"
              ) : (
                <EmptyValue label="Reading the fleet" />
              )}
            </dd>
          </>
        )}

        <dt>Open work</dt>
        <dd>
          {workload.error ? (
            <EmptyValue label="The tracker did not answer" />
          ) : !workload.data ? (
            <EmptyValue label="Reading the tracker" />
          ) : load && load.open > 0 ? (
            <a className="t-link" href={href(["work"], { assignee: seat.handle })}>
              {plural(load.open, "task")}
              {load.blocked > 0 && ` · ${load.blocked} blocked`}
              {load.overdue > 0 && ` · ${load.overdue} overdue`}
            </a>
          ) : (
            "Nothing open"
          )}
        </dd>

        {!human && tools.length > 0 && (
          <>
            <dt>Tools</dt>
            <dd className="seat-peek-tools">
              {tools.slice(0, TOOL_CHIPS).map((t) => (
                <Tag key={t} size="xs" appearance="outline">
                  {toolSourceLabel(t)}
                </Tag>
              ))}
              {tools.length > TOOL_CHIPS && (
                <Tag size="xs" appearance="outline" title={tools.slice(TOOL_CHIPS).join(", ")}>
                  +{tools.length - TOOL_CHIPS}
                </Tag>
              )}
            </dd>
          </>
        )}
      </dl>

      {seat.goal && (
        <section className="seat-peek-goal" aria-label="Goal">
          <h3>Goal</h3>
          {/* PROMPT TEXT, which is markdown: a ``code`` span is one. */}
          <p>{renderInline(seat.goal, "goal")}</p>
        </section>
      )}

      <footer className="seat-peek-foot">
        <ButtonLink variant="primary" size="small" href={href(seatPath(seat))}>
          Open profile
        </ButtonLink>
        <MessageSeatButton handle={seat.handle} />
      </footer>
    </div>
  );
}

/** One capped window: its label, its figure and a meter in the engine's state. */
function BudgetFact({
  window: w,
  label,
  figure,
}: {
  window: BudgetWindow;
  label: string;
  figure: string;
}) {
  return (
    <>
      <dt>{label}</dt>
      <dd className="seat-peek-budget">
        <span className="t-num">{figure}</span>
        {/* THE ENGINE'S STATE, never a threshold of ours (`lib/budget.ts`). */}
        <Meter
          hideLabel
          label={`${label}, ${w.window}`}
          value={w.used}
          max={w.limit ?? 0}
          state={w.state}
          valueText={`${w.used.toLocaleString()} of ${(w.limit ?? 0).toLocaleString()} tokens`}
        />
      </dd>
    </>
  );
}

/** "08:02" for an instant today, the date and minute for an older one. */
function sinceWords(at: string, now: number): string {
  const t = Date.parse(at);
  if (!Number.isFinite(t)) return "";
  return readerDay(t) === readerDay(now) ? fmtTime(at).slice(0, 5) : fmtMinute(at);
}
