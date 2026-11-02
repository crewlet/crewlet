/**
 * A page action the engine sent, carried out exactly once: claimed on the
 * server, applied by the screen, and reported back.
 *
 * # Exactly once, across tabs
 *
 * Every tab open on the conversation reads the same stream, so every one of
 * them hears the action. Before any screen applies it, this tab CLAIMS it
 * (`PUT /assistant/actions/{id}` with `applying`), and the engine answers
 * `409 action_already_claimed` to every claim after the first. Each request
 * is sent under ONE operation key, minted for it and sent again unchanged on
 * a retry, so a claim whose answer was lost is asked again as the same claim
 * rather than read as another tab's. A claim nobody ever answered is not a
 * claim: nothing is applied, because the one thing worse than a change that
 * did not happen is a change that happened twice. Inside one tab, an action
 * is taken once however often its frame is read (a resumed stream, a second
 * listener).
 *
 * # Which tab claims
 *
 * The tab the change was meant for: the one that approved it or asked the
 * question, and otherwise only a tab whose page is the very one the change
 * was prepared against (its revision equal to the action's). A tab on
 * another page leaves it alone rather than claim it and answer `stale` for
 * the tab that would have applied it.
 *
 * # Apply and report
 *
 * The screen applies it through its own parser, gate and setters, reached
 * over the bus (`../bus.ts`, answered by `../context.ts`), with a fresh look
 * at the page's revision at that moment. The outcome goes back on the same
 * claim, so the model hears it as the action's result and the turn goes on;
 * a report nobody answered is sent again a few times, since the engine keys
 * it on the action and a second copy changes nothing.
 */

import {
  ASSISTANT_ACTION_CONTEXT_INVALIDATE,
  type AssistantActionEvent,
  type AssistantActionReport,
  type AssistantPageContext,
} from "~/contract/assistant.ts";
import { call, versioned } from "~/protocol/assistant.ts";
import { newActOpID } from "~/protocol/act.ts";
import { RestError } from "~/protocol/rest.ts";
import { emit, on, type AssistantBusEvents } from "../bus.ts";
import { actionRevision, settledPageContext } from "../context.ts";

/** What became of a page action in this tab, as its receipt says it. */
export type ActionLocalState =
  /** The screen applied it. */
  | "applied"
  /** The page moved since the change was prepared; nothing changed. */
  | "stale"
  /** The screen's own gate refused it; nothing changed. */
  | "blocked"
  /** The screen could not read the change; nothing changed. */
  | "invalid"
  /** The page no longer takes this change; nothing changed. */
  | "no_handler"
  /** Another tab claimed it first, and applies it there. */
  | "elsewhere"
  /** Crewlet did not answer the claim, so nothing was applied. */
  | "unconfirmed"
  /** The person undid it from its receipt. */
  | "undone";

/** A page action's outcome in this tab. */
export interface ActionLocal {
  state: ActionLocalState;
  /**
   * What happened, in the screen's or the engine's words: the receipt of an
   * applied change, or why nothing changed. Never the consequence itself
   * ("Nothing changed."), which each place that draws it adds in its own way.
   */
  words: string;
  /** The receipt may offer Undo. */
  undoable: boolean;
}

/** The answers the flow is told about, and the requests it makes. */
export interface ActionDeps {
  /** Record what became of the action, for the drawer to draw. */
  settle: (local: ActionLocal) => void;
  /** Send a report on the action's claim, under `opKey`; the session's `call`, replaced in a test. */
  report?: (actionId: string, body: AssistantActionReport, opKey: string) => Promise<unknown>;
  /** The screen once it settled after the action; the page's own, replaced in a test. */
  screen?: () => Promise<AssistantPageContext | null>;
}

/** The action ids this tab has taken, so a frame read twice is taken once. */
const taken = new Set<string>();

/** How many times a claim or a report nobody answered is sent. */
const REPORT_ATTEMPTS = 3;

/** How long to wait before sending an unanswered request again, doubled each time. */
const REPORT_BACKOFF_MS = 1_000;

function sendReport(
  actionId: string,
  body: AssistantActionReport,
  opKey: string,
): Promise<unknown> {
  return call("PUT /assistant/actions/{id}", body, { id: actionId, opKey });
}

/** Whether a report's failure is one a second copy may get past: no answer, or the engine unavailable. */
function retryable(err: unknown): boolean {
  return !(err instanceof RestError) || err.unanswered || err.status >= 500;
}

/** The screen's own answer, over the bus. */
function applyOnPage(action: AssistantActionEvent): AssistantBusEvents["assistant.action.result"] {
  let answer: AssistantBusEvents["assistant.action.result"] | null = null;
  const off = on("assistant.action.result", (result) => {
    if (result.correlation_id === action.action_id) answer = result;
  });
  try {
    emit("assistant.action.apply", {
      correlation_id: action.action_id,
      action: action.name,
      payload: action.payload,
      source_revision: action.source_revision ?? "",
    });
  } finally {
    off();
  }
  return (
    answer ?? {
      correlation_id: action.action_id,
      outcome: "no_handler",
      reason: "This page does not take that change.",
      revision: actionRevision(action.name),
    }
  );
}

/** What an outcome says, in the screen's words where it gave some. */
function wordsOf(result: AssistantBusEvents["assistant.action.result"]): string {
  switch (result.outcome) {
    case "applied":
      return result.receipt ?? "Applied to this page.";
    case "stale":
      return "The page changed since this was prepared.";
    case "no_handler":
      return "This page no longer takes that change.";
    case "blocked":
    case "invalid":
      return result.reason ?? "The page refused it.";
  }
}

/** Whether an outcome left the page as it was. */
export function changedNothing(state: ActionLocalState): boolean {
  return state !== "applied" && state !== "elsewhere";
}

/**
 * Send a claim or a report under one operation key, sending it again while
 * nobody answers; a refusal the engine wrote is thrown at once, and the last
 * unanswered attempt's error once the attempts are spent.
 */
async function sendOnce(
  actionId: string,
  body: AssistantActionReport,
  report: NonNullable<ActionDeps["report"]>,
  wait: (ms: number) => Promise<void>,
): Promise<void> {
  const opKey = newActOpID();
  for (let attempt = 0; ; attempt++) {
    try {
      await report(actionId, body, opKey);
      return;
    } catch (err) {
      if (!retryable(err) || attempt === REPORT_ATTEMPTS - 1) throw err;
      await wait(REPORT_BACKOFF_MS * 2 ** attempt);
    }
  }
}

/**
 * Take a page action the stream carried: claim it, apply it, report it, and
 * answer whether this tab took it and reported what it came to (false where
 * it was not this tab's to take, was taken already, or its claim was
 * refused). `mine`
 * says this tab approved the change or asked the question it answers.
 * Resolves once the outcome is reported, or once it is clear this tab will
 * not apply it.
 */
export async function takeAction(
  action: AssistantActionEvent,
  mine: boolean,
  deps: ActionDeps,
  wait: (ms: number) => Promise<void> = (ms) => new Promise((done) => setTimeout(done, ms)),
): Promise<boolean> {
  if (action.name === ASSISTANT_ACTION_CONTEXT_INVALIDATE || taken.has(action.action_id)) {
    return false;
  }
  // THE ACTION'S OWN REVISION: what it depends on, never the whole page.
  const here = actionRevision(action.name);
  if (!mine && action.source_revision !== here) return false;
  taken.add(action.action_id);
  const report = deps.report ?? sendReport;
  try {
    await sendOnce(
      action.action_id,
      { ...versioned, state: "applying", revision: here },
      report,
      wait,
    );
  } catch (err) {
    if (err instanceof RestError && err.code === "action_already_claimed") {
      deps.settle({
        state: "elsewhere",
        words: "Applied in another tab of Crewlet.",
        undoable: false,
      });
      return false;
    }
    // A REFUSAL IS THE ENGINE'S SENTENCE; a claim nobody answered is not a
    // claim, so neither applies anything.
    const refused = err instanceof RestError && !err.unanswered;
    deps.settle({
      state: "unconfirmed",
      words: refused
        ? err.sentence || "Crewlet refused this change."
        : "Crewlet did not confirm this change, so it was not applied.",
      undoable: false,
    });
    return false;
  }
  const result = applyOnPage(action);
  deps.settle({
    state: result.outcome,
    words: wordsOf(result),
    undoable: result.outcome === "applied" && result.undoable === true,
  });
  // THE SCREEN AS THE ACTION LEFT IT travels with the report, so the turn it
  // resumes reads the draft the change made and offers the actions the
  // screen accepts now, not the ones it had when the question was asked.
  const screen = await (deps.screen ?? settledPageContext)();
  // THE OUTCOME IS THE PAGE'S OWN STATEMENT and already drawn here; a report
  // the engine never heard leaves the turn waiting on it, which the engine's
  // own expiry settles, and is not a reason to say the change did not apply.
  await sendOnce(
    action.action_id,
    {
      ...versioned,
      state: result.outcome,
      revision: result.revision,
      ...(result.reason === undefined ? {} : { reason: result.reason }),
      ...(result.receipt === undefined ? {} : { receipt: result.receipt }),
      ...(screen === null ? {} : { page_context: screen }),
    },
    report,
    wait,
  ).catch(() => {});
  return true;
}

/** Test seam: a tab that has taken nothing. */
export function resetPageActionsForTest(): void {
  taken.clear();
}
