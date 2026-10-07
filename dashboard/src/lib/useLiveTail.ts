/**
 * A running coding run's live output, as a hook: what the view holds, grown by
 * what the run's owner says it lacks.
 *
 * # Why not `useQuery`
 *
 * The question's parameters are the view's own CURSOR — the reading it holds
 * (`epoch`), how far (`after`) and the owner's digest there — and they move
 * with every answer. `useQuery` keys its effect on its parameters, so a cursor
 * in them re-runs the effect the moment an answer lands: the poll becomes a
 * tight loop asking again before the three seconds are up. The cursor lives in
 * a ref here, read by the one chained poll this hook runs.
 *
 * # What the view holds
 *
 * At most what the run's record will hold (`LIVE_OUTPUT_MAX_BYTES`, the
 * engine's `sandbox.MaxRunTextBytes`): every answer is appended to it — or, on
 * a reset, replaces it — and once it is past that, its FRONT is dropped on a
 * line and counted, so the screen says how much earlier output it no longer
 * holds. An owner on a build that reads no cursor answers a WINDOW instead,
 * which replaces what is shown each time, exactly as the screen did before.
 *
 * # When it stops
 *
 * Only on `not_running`: the job finished, parked, or a later one replaced it,
 * and its output is on its record from here on. `launching` (a box still being
 * made), `box_paused`, a silent owner and a failed read all go on asking, and
 * keep what the view holds on screen.
 */

import { useEffect, useRef, useState } from "react";
import { useClient, useConnection } from "./store-hooks.ts";
import { retryDelayMs } from "./useQuery.ts";
import { utf8Bytes } from "./format.ts";
import { LIVE_OUTPUT_MAX_BYTES, SANDBOX_TAIL_POLL_MS } from "~/contract/sandbox.ts";
import type { QueryErrorCode } from "~/contract/errors.ts";
import { queryErrorCode, type SandboxTailAnswer } from "~/protocol/index.ts";

/** What a live view holds, and how it asks for the rest. */
export interface LiveView {
  /** The last answer, its text left out (the view holds that), or null. */
  answer: SandboxTailAnswer | null;
  /** What the view shows. */
  text: string;
  /** "cursor" while the owner sends what the view lacks, "window" while an
   *  older owner sends its window to replace it, null before an answer. */
  mode: "cursor" | "window" | null;
  /** The cursor: the reading the view holds, how far, and the owner's digest
   *  there. An empty epoch asks for a reset. */
  epoch: string;
  end: number;
  digest: string;
  /** Bytes of the reading before `text` the view does not hold: never sent
   *  (a reset began after them) or dropped from its front. */
  dropped: number;
  /** The owner's reading began after the job's own start. */
  front: boolean;
  /** Bytes written but not shown yet, until their redaction is settled. */
  held: number;
  /** The most the last answer's shape carries. */
  windowBytes: number;
  /** The job is not running: the view has stopped asking. */
  final: boolean;
}

export const EMPTY_VIEW: LiveView = {
  answer: null,
  text: "",
  mode: null,
  epoch: "",
  end: 0,
  digest: "",
  dropped: 0,
  front: false,
  held: 0,
  windowBytes: 0,
  final: false,
};

/** What a view asks with: its cursor, or a cursor at nothing for a reset. */
export function cursorParams(view: LiveView): Record<string, unknown> {
  if (view.mode !== "cursor" || !view.epoch) return { cursor: true };
  return { cursor: true, epoch: view.epoch, after: view.end, digest: view.digest };
}

/**
 * One answer folded into what a view holds.
 *
 * A DELTA THAT DOES NOT FOLLOW WHAT THE VIEW HOLDS IS NOT SPLICED: the owner
 * only sends one that starts where the view holds through, so one that does not
 * is a view out of step with its owner, and the honest move is to keep what is
 * on screen and ask for a reset next time — never to show a fragment as though
 * it continued the text above it.
 */
export function foldTail(view: LiveView, answer: SandboxTailAnswer): LiveView {
  const out = answer.output;
  const kept: SandboxTailAnswer = out ? { ...answer, output: { ...out, text: "" } } : answer;
  const next: LiveView = { ...view, answer: kept, final: answer.outcome === "not_running" };
  if (answer.outcome !== "tail" || !out) return next;
  if (!out.cursor) {
    // A WINDOW, from an owner that reads no cursor: it replaces.
    return {
      ...next,
      text: out.text,
      mode: "window",
      epoch: "",
      end: 0,
      digest: "",
      dropped: 0,
      front: false,
      held: 0,
      windowBytes: out.window_bytes ?? 8 << 10,
    };
  }
  next.front = out.front ?? false;
  next.held = out.held ?? 0;
  // `start` IS ALWAYS A NUMBER on a cursor answer, 0 included: compared with
  // an offset left out at 0, a delta from the start of a reading that had
  // settled nothing read as one that did not follow.
  const follows =
    !out.reset && view.mode === "cursor" && out.epoch === view.epoch && out.start === view.end;
  if (!out.reset && !follows) {
    return { ...next, epoch: "", end: 0, digest: "" };
  }
  const trimmed = trimFront(follows ? view.text + out.text : out.text, LIVE_OUTPUT_MAX_BYTES);
  return {
    ...next,
    text: trimmed.text,
    mode: "cursor",
    epoch: out.epoch,
    end: out.end,
    digest: out.digest,
    dropped: (follows ? view.dropped : out.start) + trimmed.dropped,
    windowBytes: out.window_bytes ?? LIVE_OUTPUT_MAX_BYTES,
  };
}

/**
 * A text held to `max` UTF-8 bytes by dropping its FRONT on a line, and how
 * many bytes that dropped. A single line longer than `max` keeps its own end,
 * on a character, marked — the engine's rule for a stream's end.
 */
export function trimFront(text: string, max: number): { text: string; dropped: number } {
  if (text.startsWith("…")) {
    // A line already kept in part: its mark is not content, and is not
    // counted against the bound or as text dropped.
    const inner = trimFront(text.slice(1), max);
    return inner.dropped === 0 ? { text, dropped: 0 } : inner;
  }
  const total = utf8Bytes(text);
  if (total <= max) return { text, dropped: 0 };
  const over = total - max;
  // The first index past `over` bytes, on a character.
  let i = 0;
  let bytes = 0;
  while (i < text.length && bytes < over) {
    const c = text.charCodeAt(i);
    if (c < 0x80) bytes += 1;
    else if (c < 0x800) bytes += 2;
    else if (c >= 0xd800 && c <= 0xdbff) {
      bytes += 4;
      i++;
    } else bytes += 3;
    i++;
  }
  if (i > 0 && text[i - 1] === "\n") return { text: text.slice(i), dropped: bytes };
  const line = text.indexOf("\n", i);
  if (line >= 0 && line + 1 < text.length) {
    return { text: text.slice(line + 1), dropped: bytes + utf8Bytes(text.slice(i, line + 1)) };
  }
  return { text: "…" + text.slice(i), dropped: bytes };
}

export interface LiveTail extends LiveView {
  /** The last failure's code, cleared by the next answer. */
  error: QueryErrorCode | null;
}

/**
 * Asks `sandbox_tail` for one job every `pollMs`, chained after each answer, for
 * as long as it is mounted and the job has not stopped — keyed on the job, so
 * a new launch starts a view of its own.
 */
export function useLiveTail(
  turnId: string,
  launchId: string,
  pollMs: number = SANDBOX_TAIL_POLL_MS,
): LiveTail {
  const { socket } = useClient();
  const { connected } = useConnection();
  const [state, setState] = useState<LiveTail>({ ...EMPTY_VIEW, error: null });
  // The view the poll asks from: the latest fold, read by the next tick.
  const view = useRef<LiveView>(EMPTY_VIEW);

  // A NEW JOB IS A NEW VIEW, never the last job's text with the new one's
  // output appended to it.
  useEffect(() => {
    view.current = EMPTY_VIEW;
    setState({ ...EMPTY_VIEW, error: null });
  }, [turnId, launchId]);

  useEffect(() => {
    if (view.current.final) return;
    let live = true;
    let timer: ReturnType<typeof setTimeout> | 0 = 0;
    const run = async (): Promise<void> => {
      let retryMs = 0;
      try {
        const answer = await socket.query("sandbox_tail", {
          turn_id: turnId,
          launch_id: launchId,
          ...cursorParams(view.current),
        });
        if (!live) return;
        view.current = foldTail(view.current, answer);
        setState({ ...view.current, error: null });
      } catch (err) {
        if (!live) return;
        const code = queryErrorCode(err instanceof Error ? err.message : null) ?? "query_failed";
        retryMs = retryDelayMs(code, err);
        // KEEP what the view holds: one failed read is not a run that said
        // nothing.
        setState((prev) => ({ ...prev, error: code }));
      } finally {
        if (live && !view.current.final) {
          const next = retryMs ? Math.min(pollMs, retryMs) : pollMs;
          timer = setTimeout(() => void run(), next);
        }
      }
    };
    void run();
    return () => {
      live = false;
      clearTimeout(timer);
    };
  }, [socket, turnId, launchId, pollMs, connected]);

  return state;
}
