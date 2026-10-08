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
 * holds, where its tools come from, and what it is for — and for a HUMAN
 * seat, who holds it. Everything else is the profile's, one press away.
 *
 * # A human seat says who holds it, and is filled from here
 *
 * The identity directory binds a person to a human seat, and every person
 * comes from one, so a human seat's peek says what holds it — its holder and
 * their stage, the open invitation that names it, or that it is vacant — and,
 * for a reader holding `people:manage`, offers what the seat's card menu and
 * its page offer: Invite and Create on a vacant seat, Cancel invitation on an
 * invited one. On a phone, whose chart is rows with no card menu, this is
 * where those gestures are.
 *
 * # At most three questions, one directory read, and none a reader will be refused
 *
 * The seat, its state, its model chain and tool sources, and its budget
 * windows all arrive with the org projection and the `agents` push, so they
 * cost nothing. What does cost a read:
 *
 *   * `work_workload` — the seat's open work, for every reader;
 *   * `fleet` — which node holds the seat and since when — on
 *     `fleet:operate`, so it is asked only of a reader holding it; every
 *     other reader is told what the health push says (`heldBy`: this node, by
 *     name, or another),
 *     exactly as the profile's Setup card tells them, rather than being sent
 *     a refusal to draw or told a fact already on their screen is withheld;
 *   * `work_item_turns` — which turn this is on its task ("Turn 2") — asked
 *     only while the seat is working on one.
 *
 * And for a HUMAN seat, one REST read of the directory (`GET /iam/seats`),
 * asked only of a reader it answers (`people:manage` or `audit:read`) and
 * never for an agent's seat, whose holder is the engine.
 *
 * The company document is NOT read: the model chain a turn resolves is on the
 * org projection for a reader holding `config:read` — the one who could read
 * the document — and everybody else is told which grant shows it
 * ([resolvedAbsence]), so a per-peek read of the whole guarded configuration
 * (on every `[`/`]` step through a list) would buy nothing.
 *
 * # Message is a task that asks
 *
 * There is no person-to-seat chat channel. Message opens the one New task
 * sheet with the seat as assignee and `ask` set, so the question is filed as
 * work the seat owes an answer on, and the answer lands in the asker's Inbox.
 */

import { useMemo, useState } from "react";
import {
  Button,
  ButtonLink,
  Callout,
  EmptyState,
  EmptyValue,
  Meter,
  StatusDot,
  Tag,
} from "@crewlethq/ui";
import { UserGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { MessageSeatButton } from "~/components/writes.tsx";
import { useNow } from "~/lib/clock.ts";
import { fmtCount, fmtElapsed, fmtMinute, fmtTime, plural, readerDay } from "~/lib/format.ts";
import { useAgents, useEngineHealth, useOrg } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import {
  canManagePeople,
  canReadDirectory,
  SEAT_GESTURE_WORDS,
  SeatGestureDialog,
  seatGestures,
  useHumanSeats,
  type SeatGesture,
} from "~/components/people.tsx";
import type { HumanSeat } from "~/contract/identity.ts";
import {
  activityOf,
  awaitingPerson,
  handleLabel,
  heldBy,
  indexOrg,
  liveRowFor,
  nameOfIn,
  resolvedAbsence,
  resolvedWithheld,
  ringOf,
  roundOf,
  seatPath,
  stateLine,
  toneOf,
  type Seat,
} from "~/lib/seats.ts";
import { unitPath } from "~/lib/orgchart.ts";
import { answersAddress, itemAddress, turnItem } from "~/lib/work.ts";
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
      agent={liveRowFor(agents, seat)}
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
  const ref = call?.work_item?.key ? call.work_item : (turn?.work_item ?? null);
  // ASKED FOR BY ITS ADDRESS ([turnItem]): a key another task claimed first
  // would number this turn on the claimant's list.
  const item = ref?.key ? itemAddress(turnItem(ref)) : "";

  // THE THREE READS — see the file's doc.
  const workload = useQuery("work_workload", undefined, { pollMs: 60_000 });
  // THE FLEET'S LEASES ARE THE DEPLOYMENT'S, read on `fleet:operate`: asked
  // only of a reader holding it, and everybody else is told what the health
  // push says.
  const fleet = useQuery("fleet", undefined, { enabled: viewer.operatesFleet, pollMs: 60_000 });
  const turns = useQuery(
    "work_item_turns",
    { id: item, limit: 1 },
    { enabled: state === "working" && item !== "" },
  );

  const ordinal =
    turn && item
      ? turnOrdinal(
          turn.turn_id,
          turns.data?.turns?.[0],
          !!turns.data && answersAddress(item, turns.data.item, turns.data.key),
        )
      : null;
  const started = turn?.started_at ?? call?.started_at;
  const elapsed = started ? now - Date.parse(started) : null;
  const facts = turnFacts(turn, call, ordinal);
  const load = workload.data?.rows?.find((r) => r.handle === seat.handle);
  const lease = fleet.data?.seats?.find((s) => s.handle === seat.handle);
  const windows = agent?.budget?.windows ?? [];
  const chain = seat.raw.llm?.["execute"] ?? [];
  const tools = seat.raw.tool_sources ?? [];
  // BY THE SEAT'S HANDLE, never the name a run was started under, which two
  // seats may share.
  const sandbox = sandboxes.find((s) => s.agent_handle === seat.handle) ?? null;
  const absent = resolvedAbsence(viewer);
  const place = unitPath(seat.unit);
  // WHO HOLDS A HUMAN SEAT — the directory read, see the file's doc.
  const readsDirectory = human && !viewer.asking && canReadDirectory(viewer.grants);
  const directory = useHumanSeats(readsDirectory);
  const holding = directory.data?.find((row) => row.handle === seat.handle);
  const gestures = canManagePeople(viewer.grants) ? seatGestures(holding) : [];
  // ABOUT THE SEAT AS IT WAS WHEN THE GESTURE STARTED (`SeatGestureDialog`).
  const [opening, setOpening] = useState<{ gesture: SeatGesture; row: HumanSeat } | null>(null);

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
              ) : absent === "none" ? (
                <EmptyValue label="No provider configured" />
              ) : absent === "withheld" ? (
                <span className="muted">{resolvedWithheld("its model chain")}</span>
              ) : (
                <EmptyValue label="Not reported yet" />
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
              {!viewer.operatesFleet ? (
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

        {readsDirectory && (
          <>
            <dt>Held by</dt>
            <dd>
              {holding ? (
                <HeldBy row={holding} />
              ) : directory.error ? (
                <EmptyValue label="The identity directory did not answer" />
              ) : directory.data ? (
                <EmptyValue label="Not in this node's directory yet" />
              ) : (
                <EmptyValue label="Reading the directory" />
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
        {holding &&
          gestures.map((gesture) => (
            <Button
              key={gesture}
              size="small"
              variant="secondary"
              leadingIcon={SEAT_GESTURE_WORDS[gesture].icon}
              onClick={() => setOpening({ gesture, row: holding })}
            >
              {SEAT_GESTURE_WORDS[gesture].button}
            </Button>
          ))}
      </footer>
      {opening && (
        <SeatGestureDialog
          gesture={opening.gesture}
          seat={opening.row}
          held={viewer.grants}
          onClose={() => setOpening(null)}
        />
      )}
    </div>
  );
}

/**
 * What holds a human seat, as the VALUE of the peek's **Held by** fact: the
 * holder's login — a service account said to be one — with their stage short
 * of active; the open invitation that names the seat; or nobody.
 *
 * NOT THE CARD'S LINE (`holdingLine` in `components/people.tsx`), which begins
 * "Held by" itself because a card has no term beside it: under this term it
 * read "Held by / Held by jane.doe (suspended)", and "Held by / Vacant".
 */
function HeldBy({ row }: { row: HumanSeat }) {
  const { holder, invitation } = row;
  if (holder) {
    const stage = holder.stage && holder.stage !== "active" ? ` (${holder.stage})` : "";
    return (
      <span>
        {holder.kind === "machine" && "The service account "}
        <code className="inline">{holder.login || holder.person}</code>
        {stage}
      </span>
    );
  }
  if (invitation) {
    return (
      <span>
        {invitation.email && !invitation.sealed
          ? `An open invitation · ${invitation.email}`
          : "An open invitation"}
      </span>
    );
  }
  return <span>Nobody — the seat is vacant</span>;
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
