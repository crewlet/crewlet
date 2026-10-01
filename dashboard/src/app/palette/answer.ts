/**
 * The palette's answer: one question, answered from what the company has
 * written down, as the person asking (`answer_knowledge`, ADR-0024).
 *
 * # When it is asked, because every asking spends tokens
 *
 * The engine answers with one model call charged to the company's windows, so
 * this asks only when ALL of these hold:
 *
 *  - the scope is All or Pages — a person in Tasks, Agents or Actions is
 *    looking for a row, not a paragraph;
 *  - the term reads as a question ([answerable]: three words, twelve
 *    characters);
 *  - typing has paused for [ANSWER_IDLE_MS];
 *  - this browser may make the call (`useWriteAccess`): a person, bound, on
 *    an engine that answers. An anonymous or unbound reader is told what
 *    would let them ask, and nothing is sent;
 *  - it has not been answered already this session — answers are kept per
 *    normalised question for as long as the page is open, so going back to a
 *    term, or reopening the palette on it, spends nothing.
 *
 * AT MOST ONE IS IN FLIGHT: a new term abandons the question before it, whose
 * answer would be about something the person has stopped asking. And an
 * answer is only ever drawn under the term it answers — a stale paragraph
 * under a new question reads as the answer to it.
 *
 * # Why a write
 *
 * It changes nothing, but a read is asked again on every refocus and
 * reconnect, and this one costs tokens each time. As a press it is sent once,
 * when the person has asked.
 */

import { useEffect, useRef, useState } from "react";
import { useAct } from "~/lib/useAct.ts";
import type { WriteAccess } from "~/lib/useWriteAccess.ts";
import { ANSWER_IDLE_MS, answerable, normalizeQuestion } from "./hits.ts";

/** One document an answer was written from; source [n] is the n-th. */
export interface AnswerSource {
  kind: "page" | "task" | (string & {});
  /** A page's id, a work item's key. */
  ref: string;
  title: string;
  /** An external wiki page's own link, which the dashboard has no route for. */
  url?: string;
}

/** What `answer_knowledge` answers. Tokens, never a price. */
export interface KnowledgeAnswerReceipt {
  answer_md: string;
  sources: AnswerSource[];
  /** What THIS call spent; zero on a cached answer. */
  tokens: { input: number; output: number };
  /** The model that wrote it; "" when nothing matched and no model ran. */
  model: string;
  cached: boolean;
}

/** Where the answer to the current term stands. */
export type AnswerState =
  /** Nothing to show: not a question, not this scope, or nobody may ask. */
  | { kind: "none" }
  /** A question, and this reader cannot ask it — `reason` says why. */
  | { kind: "blocked"; reason: string }
  /** A question, waiting for typing to pause: nothing is drawn yet. */
  | { kind: "waiting" }
  /** A question on the wire. */
  | { kind: "asking" }
  | { kind: "answered"; answer: KnowledgeAnswerReceipt }
  /** The engine said no — the budget, the backend — in its own sentence. */
  | { kind: "refused"; sentence: string }
  /** Nobody can say whether it was answered; nothing is retried by itself. */
  | { kind: "unknown"; reason: string };

/**
 * The answers this page has been given, per normalised question.
 *
 * FOR THE SESSION — the page's lifetime — rather than per palette: the
 * palette is opened fresh every time, and a person who closes it and opens it
 * again on the same question has asked nothing new. The engine keeps its own
 * cache keyed on the corpus position; this one saves the round trip and the
 * dispatch on top of that.
 */
const answered = new Map<string, KnowledgeAnswerReceipt>();

/** Test seam: forget every answer, as a fresh page would. */
export function forgetAnswersForTest(): void {
  answered.clear();
}

/** Whether a reader blocked for this reason is told how to ask, or told nothing. */
function explains(access: WriteAccess): string | null {
  if (access.can) return null;
  // THE TWO READERS WHO COULD ASK once they did something about it: no token,
  // or a token that is not a person. Offline and "still checking" pass by
  // themselves, and an engine that does not answer questions at all has
  // nothing for them to do — a line about it on every question is noise.
  if (access.block === "anonymous" || access.block === "unbound") {
    return "Set a token bound to your seat to get an answer from your company’s knowledge.";
  }
  return null;
}

/**
 * The answer to `term`, asked when it should be — see the file's doc.
 * `wanted` is whether the palette's scope takes an answer at all.
 */
export function useKnowledgeAnswer(term: string, wanted: boolean): AnswerState {
  const write = useAct("answer_knowledge");
  const key = normalizeQuestion(term);
  const question = wanted && answerable(term);
  const eligible = question && write.access.can;
  const [settled, setSettled] = useState<{ key: string; state: AnswerState } | null>(null);
  const [sent, setSent] = useState("");
  // THE PRESS, held rather than depended on: `run` is stable per hook, and an
  // effect keyed on the hook's whole object would re-arm on every render.
  const run = useRef(write.run);
  run.current = write.run;

  useEffect(() => {
    if (!eligible || answered.has(key)) return;
    const controller = new AbortController();
    const timer = setTimeout(() => {
      setSent(key);
      void run
        .current(
          { q: term.trim() },
          // QUIET: the answer is drawn where it was asked, and a toast
          // saying "Answered" over it is the same fact twice.
          { done: "Answered", quiet: true },
          { signal: controller.signal },
        )
        .then((result) => {
          // NULL IS ABANDONED OR NEVER SENT, and neither is an answer.
          if (result === null || controller.signal.aborted) return;
          let state: AnswerState;
          switch (result.kind) {
            case "applied":
            case "pending": {
              const answer = result.receipt as KnowledgeAnswerReceipt;
              answered.set(key, answer);
              state = { kind: "answered", answer };
              break;
            }
            case "unknown":
              state = { kind: "unknown", reason: result.reason };
              break;
            case "refused":
              state = { kind: "refused", sentence: result.sentence };
              break;
          }
          setSettled({ key, state });
        });
    }, ANSWER_IDLE_MS);
    // A NEW TERM ABANDONS THE OLD QUESTION, whether it is still waiting for
    // the pause or already on the wire.
    return () => {
      clearTimeout(timer);
      controller.abort();
    };
    // `term` IS READ THROUGH `key`: two spellings of one question are one
    // ask, and re-arming on a changed space would abandon a question that
    // did not change.
  }, [key, eligible]);

  if (!question) return { kind: "none" };
  const blocked = explains(write.access);
  if (blocked) return { kind: "blocked", reason: blocked };
  if (!write.access.can) return { kind: "none" };
  const held = answered.get(key);
  if (held) return { kind: "answered", answer: held };
  // ONLY THIS TERM'S OUTCOME: a refusal or an unknown for the question before
  // is not one for this.
  if (settled?.key === key) return settled.state;
  return sent === key ? { kind: "asking" } : { kind: "waiting" };
}
