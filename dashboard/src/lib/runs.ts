import { useMemo } from "react";
import { decisionTexts } from "./work.ts";
import { tsKey } from "./format.ts";
import { useQuery } from "./useQuery.ts";
import { useExecutors } from "./store-hooks.ts";
import type { ExecutorEntry, ExecutorRow, ExecutorStatus } from "~/protocol/index.ts";

/**
 * How often the executors' durable record is asked again.
 *
 * ONE VALUE for every reader of `executors` — the Executors board, an
 * executor's own page and rail, and the attention queue Home and the Inbox
 * draw. It was two: the board polled every 20 s and the queue every 30 s, so
 * an executor that parked on a question appeared on the board and then, up to
 * ten seconds later, as
 * the decision the Inbox asks a person to make — two screens disagreeing about
 * whether anybody was being waited on.
 *
 * TWENTY SECONDS, because nothing pushes this record: an executor's status
 * moves on its own row in the coordination store and the live projection
 * carries only what a panel of executors in flight draws. A launch lives for
 * minutes and a parked executor waits for a person, who is not answering
 * inside twenty seconds either; the
 * live projection reconciles its own panel against the same record every
 * 30 s, so a shorter poll here would be reading a record that has not moved.
 */
export const RUNS_POLL_MS = 20_000;

/**
 * Every text of an executor's question a reader reads as one message, as
 * `askTexts` is an ask's: a bare `#11` in any of them is placed by what the
 * others name (`TokenScope`).
 *
 * AN EXECUTOR'S QUESTION IS ITS TITLE AND ITS CONTEXT, each its own, on every
 * reader of it: the durable row, the live entry and the record of its answer.
 * Nothing is split out of the question (`components/RunQuestion.tsx`).
 */
export function runAskTexts(
  run: Pick<ExecutorRow, "question" | "question_context" | "decision">,
): string[] {
  return [run.question ?? "", run.question_context, ...decisionTexts(run.decision)].filter(
    (text) => text.trim() !== "",
  );
}

/**
 * Why an executor's question cannot be answered from what a screen holds, or
 * undefined where it can.
 *
 * AN ANSWER NAMES THE QUESTION IT ANSWERS (`answer_executor{launch_id}`), the one
 * the reader saw, so an executor that moved on to another question refuses it
 * rather than taking it as an answer to that one. A live entry the screen
 * read before the executor's durable row carries no launch yet, and an answer
 * pressed then would name nothing.
 */
export function unanswerable(run: Pick<ExecutorRow, "launch_id">): string | undefined {
  return run.launch_id ? undefined : "Reading the executor’s question first.";
}

/**
 * A live projection entry as the executor's row it stands for.
 *
 * The fields the projection has never held are left EMPTY rather than guessed:
 * a placement of `""` is "this node has not written the row yet", and the
 * durable row replaces the whole entry the moment the store catches up. A
 * fabricated `direct` here would be a placement a reader plans against.
 */
function fromLiveBox(box: ExecutorEntry): ExecutorRow {
  return {
    turn_id: box.turn_id,
    agent_handle: box.agent_handle,
    role: box.role,
    // The live projection types its status as a plain string because it
    // mirrors whatever the executor reported; it is the same vocabulary.
    status: box.status as ExecutorStatus,
    harness: box.harness,
    ...(box.provider_key ? { provider_key: box.provider_key } : {}),
    ...(box.model ? { model: box.model } : {}),
    placement: "",
    // THE BODY AS THE PROJECTION SENT IT: absent where the engine withheld it
    // from this reader, who is told so (`body_withheld`) rather than shown an
    // executor with no task.
    ...(box.task !== undefined ? { task_description: box.task } : {}),
    ...(box.question !== undefined ? { question: box.question } : {}),
    question_context: box.question_context,
    ...(box.decision ? { decision: box.decision } : {}),
    ...(box.body_withheld ? { body_withheld: true } : {}),
    audience: box.audience ?? "",
    // THE PROJECTION HOLDS NO RESOLUTION: who the question is put to is on
    // the durable row alone, which replaces this entry once it is read.
    audience_handles: [],
    audience_fallback: false,
    branch: "",
    trace_id: "",
    owner: "",
    box_exists: true,
    paused_at: "",
    pause_ttl_seconds: 0,
    started_at: box.started_at,
    updated_at: box.started_at,
    answerable_in_chat: false,
  };
}

/**
 * The durable record and the live projection, as one list, newest first.
 *
 * ONE ROW PER TURN and the DURABLE row wins: it carries the placement, the
 * branch, the owner and the pause deadline the projection never held, and it
 * is the only one that survives the box being reclaimed. A live entry the
 * store has not caught up with still belongs on screen, which is the other
 * half — an executor that launched two seconds ago is exactly what somebody
 * watching this screen is watching for.
 *
 * SHARED by every screen that draws a run (the Executors board, Live now, a
 * seat's page and its peek, through [useRuns]): two screens folding "what is
 * in a box" their own way is how they come to disagree about whether anything
 * is. A seat's page that read the live push alone said its executor asked to
 * call a tool, with the answer beside it, while Live, reading the durable row
 * the answer had already settled, said nobody was waiting.
 */
export function mergeRuns(durable: ExecutorRow[], live: ExecutorEntry[]): ExecutorRow[] {
  const byTurn = new Map<string, ExecutorRow>();
  for (const run of durable) byTurn.set(run.turn_id, run);
  for (const box of live) {
    if (byTurn.has(box.turn_id)) continue;
    byTurn.set(box.turn_id, fromLiveBox(box));
  }
  return [...byTurn.values()].sort(
    (a, b) => tsKey(b.updated_at || b.started_at) - tsKey(a.updated_at || a.started_at),
  );
}

/**
 * Every run, as the durable record and the live push fold them ([mergeRuns]),
 * on the board's own poll: the one reading of "what is in a box" every screen
 * that draws a run takes, with whatever the engine states about each for this
 * reader (`put_to_you`, `may_answer`, `needs_you`) on its durable row.
 */
export function useRuns(enabled = true): { runs: ExecutorRow[]; loading: boolean } {
  const live = useExecutors();
  const { data, loading } = useQuery("executors", undefined, { pollMs: RUNS_POLL_MS, enabled });
  const runs = useMemo(() => mergeRuns(data?.runs ?? [], live), [data, live]);
  return { runs, loading };
}
