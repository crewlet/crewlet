/**
 * Where a running turn is, as the steps a stepper draws, and what it is
 * calling — read off the agents push, the same way on every screen that draws
 * a running turn (Home's Live now, a seat's Current turn card).
 *
 * ONE DERIVATION, because two stepper rows that disagree about which step a
 * turn is on — one saying Execute, one saying Review — for the same seat at
 * the same moment is the product contradicting itself on one screen.
 *
 * THREE STEPS — Context, Execute, Review — which are the stages the engine
 * reports (`turn.stage` and the live call's `phase`). There is no delivery
 * STEP: whether a turn reached anybody is decided as it closes, and a step
 * that could never be the current one would be a promise the row cannot keep.
 */

import type { StepperStep } from "@crewlethq/ui";
import { delegatedWorkers, roundOf } from "./seats.ts";
import type { AgentRow } from "~/protocol/index.ts";

/** The steps a turn walks, in order. */
export const TURN_STEPS = ["context", "execute", "review"] as const;
export type TurnStep = (typeof TURN_STEPS)[number];

/**
 * The turn's steps and which one it is on.
 *
 * A PARKED turn is in Execute — its coding run is the execute phase's work —
 * with the run named; a fan-out to workers says so; otherwise Execute carries
 * "round x of y" against the cap the phase was GRANTED (an extension raises
 * it). Context reads Onboarding on a seat's first turn.
 *
 * `detail` is that word on its own ("round 7 of 25", "coding run"), and
 * `inLabel: false` leaves it OUT of the Execute step for a row with no room
 * for it — a phone's card, where "Execute · round 7 of 25" pushed Review onto
 * a second line behind its own connector — so the caller can say it beside
 * the stepper instead. Either way it is said once.
 */
export function turnSteps(
  row: AgentRow,
  { inLabel = true }: { inLabel?: boolean } = {},
): { steps: StepperStep[]; current: TurnStep; detail: string } {
  const call = row.live_call;
  const parked = row.turn?.stage === "parked";
  const phase = parked ? "execute" : (call?.phase ?? row.current_phase ?? "context");
  const current: TurnStep =
    phase === "review" ? "review" : phase === "execute" ? "execute" : "context";
  const workers = delegatedWorkers(call);
  const round = roundOf(call);
  const detail =
    current !== "execute"
      ? ""
      : parked
        ? "coding run"
        : workers > 0
          ? "workers"
          : round > 0
            ? `round ${round}${call?.max_rounds ? ` of ${call.max_rounds}` : ""}`
            : "";
  const executeLabel = detail && inLabel ? `Execute · ${detail}` : "Execute";
  const steps = TURN_STEPS.map((id) => ({
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
  return { steps, current, detail };
}

/**
 * The arguments that SAY what a call is about, printed bare and first: the
 * thing searched for, the task or file acted on, the command run, the words
 * written. Any other argument is printed with its name, so `assignee me`
 * rather than a bare `me` nobody can place.
 */
const SUBJECT_ARGS = [
  "query",
  "q",
  "key",
  "id",
  "item",
  "cmd",
  "command",
  "path",
  "file",
  "url",
  "title",
  "body",
  "text",
  "message",
  "comment",
  "content",
] as const;

/** A name as words: `include_done` → `include done`. */
const argName = (key: string) => key.replace(/[_-]+/g, " ").trim();

/**
 * A call's arguments as a person reads them — `ENG-38 · Picked this up…`,
 * `assignee me · open`, `go test ./provisioner/...` — never the JSON, and
 * never the bare values it used to print ("me true", read off
 * `{"assignee":"me","open":true}`, placed nothing).
 *
 * THE SUBJECT FIRST ([SUBJECT_ARGS]), bare; every other argument after it
 * under its own name; a flag that is SET is its name alone and one that is
 * not set is nothing; a list is its items; a nested object is its name,
 * because its fields are the Input the turn's trace shows whole. Text that is
 * not a JSON object is printed as it came.
 */
export function callWords(args: unknown): string {
  let value = args;
  if (typeof value === "string") {
    try {
      value = JSON.parse(value);
    } catch {
      return value as string;
    }
  }
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    return value == null ? "" : typeof value === "string" ? value : JSON.stringify(value);
  }
  const entries = Object.entries(value as Record<string, unknown>);
  const rank = (key: string) => {
    const at = (SUBJECT_ARGS as readonly string[]).indexOf(key.toLowerCase());
    return at < 0 ? SUBJECT_ARGS.length : at;
  };
  const ordered = entries
    .map((entry, at) => ({ entry, at, rank: rank(entry[0]) }))
    .sort((x, y) => x.rank - y.rank || x.at - y.at);
  const parts: string[] = [];
  for (const {
    entry: [key, v],
    rank: r,
  } of ordered) {
    const subject = r < SUBJECT_ARGS.length;
    let said: string;
    if (v === null || v === undefined || v === false || v === "") continue;
    if (v === true) said = argName(key);
    else if (typeof v === "string") said = subject ? v : `${argName(key)} ${v}`;
    else if (typeof v === "number") said = subject ? String(v) : `${argName(key)} ${v}`;
    else if (Array.isArray(v)) {
      if (v.length === 0) continue;
      const items = v.map((i) =>
        typeof i === "string" || typeof i === "number" ? String(i) : "…",
      );
      said = subject ? items.join(", ") : `${argName(key)} ${items.join(", ")}`;
    } else said = argName(key);
    parts.push(said.replace(/\s+/g, " ").trim());
  }
  return parts.join(" · ");
}

/**
 * The call a seat is making now, or the last one it made this phase, as one
 * line: `sandbox.run go test ./...`.
 */
export function lastCallLine(row: AgentRow): string {
  const call = row.live_call;
  if (!call) return "";
  if (call.running_call) {
    return `${call.running_call.name} ${callWords(call.running_call.arguments)}`.trim();
  }
  const done = call.tool_executions?.at(-1);
  if (!done) return "";
  const name = done.name ?? done.tool ?? "";
  return `${name} ${callWords(done.arguments ?? done.args)}`.trim();
}
