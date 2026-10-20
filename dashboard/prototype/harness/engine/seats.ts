/**
 * A seat's live state: the one a seat starts with, and the ones a pause and a
 * resume put it in.
 *
 * The STATE IS THE ENGINE'S WORD. `activity` and `stopped_reason` are what
 * `internal/api/livestate/activity.go` computes, from the pause among other
 * things; a screen maps them and derives nothing, so the demo engine sets
 * them as the engine would rather than leaving a screen to guess.
 */

import type { Overlay, SeatActivityDay } from "~/protocol/types.ts";
import { documentOf, type DemoCompany, type DemoSeat } from "./company.ts";
import { companyZone, roster } from "./derive.ts";
import type { DemoActor } from "./principal.ts";
import { addDays, dateIn } from "./spend.ts";

/** A seat doing nothing, on no turn, paused by nobody. */
export function idleOverlay(): Overlay {
  return {
    activity: "idle",
    stopped_reason: null,
    current_phase: null,
    current_iteration: 0,
    live_call: null,
    live_call_seq: 0,
    last_error: null,
    turn: null,
    last_turn: null,
    paused: null,
  };
}

/** `days` company days ending today, as quiet days. */
export function quietDays(c: DemoCompany, days: number, now = new Date(c.now)): SeatActivityDay[] {
  const today = dateIn(now, companyZone(documentOf(c)));
  return Array.from({ length: days }, (_, i) => ({
    day: addDays(today, i - (days - 1)),
    turns: 0,
    failed: 0,
    tokens: 0,
  }));
}

/** The state a seat added by an org edit starts in: onboarded now, idle, remembering nothing. */
export function blankSeat(c: DemoCompany, handle: string, now: string): DemoSeat {
  const row = roster(documentOf(c)).find((r) => r.handle === handle);
  const node = c.engine.health.node;
  return {
    overlay: idleOverlay(),
    activity: {
      handle,
      role: row?.role ?? handle,
      agent_id: row?.agent_id ?? "",
      in_chart: true,
      turns: 0,
      failed: 0,
      reviewed: 0,
      first_pass: 0,
      sent_back: 0,
      tokens: 0,
      per_day: quietDays(c, 14, new Date(now)),
    },
    memory: {
      id: handle,
      diary: [],
      diary_total: 0,
      episodes: [],
      episodes_total: 0,
      skills: [],
      skills_total: 0,
      counterparties: [],
      counterparties_total: 0,
      latest_reflection: null,
      onboarded_at: now,
      held_by: node,
    },
    episode: null,
    history: [],
    conversations: { rows: [], entries: [] },
    overview: {
      agent_id: row?.agent_id ?? "",
      diary_total: 0,
      episodes_total: 0,
      skills_total: 0,
      last_reflection_at: "",
      latest_reflection: null,
      held_by: node,
      unavailable: "",
    },
  };
}

/**
 * Pause a seat: stopped, for the reason a person gave, by that person in the
 * three halves the pause record keeps (`coord.SeatPause`): the author, its
 * kind and the credential it came through.
 */
export function pause(
  seat: DemoSeat,
  by: DemoActor,
  at: string,
  reason: string,
  stopRunning: boolean,
): void {
  const paused: NonNullable<Overlay["paused"]> = {
    by: by.name,
    by_kind: by.kind,
    at,
    stop_running: stopRunning,
  };
  if (by.operator_id) paused.operator_id = by.operator_id;
  if (reason) paused.reason = reason;
  seat.overlay.paused = paused;
  seat.overlay.activity = "stopped";
  seat.overlay.stopped_reason = "paused";
  if (stopRunning) {
    seat.overlay.turn = null;
    seat.overlay.live_call = null;
    seat.overlay.current_phase = null;
  }
}

/** Resume a seat: what waited is delivered, and with nothing waiting it is idle. */
export function resume(seat: DemoSeat): void {
  seat.overlay.paused = null;
  seat.overlay.stopped_reason = null;
  seat.overlay.activity = seat.overlay.turn ? "working" : "idle";
}
