/**
 * Where a parked run stands for the reader, in the words its row says.
 *
 * THE ENGINE DECIDES IT, never this module: every run row carries whom it
 * waits on (`put_to`), whether it waits on the reader (`needs_you`) and
 * whether the reader may answer it (`may_answer`, the answer route's own
 * authority), read by Home, the Inbox, Live and the badge alike. A screen that
 * decided "needs you" for itself drew every parked run in the company as the
 * reader's own, beside a Home that rightly listed none of them.
 */

import type { ExecutorRow } from "~/protocol/index.ts";
import { andList } from "./words.ts";

/** What a run's row says of it to the reader. */
export interface RunStanding {
  /** It waits on the reader: "Needs you". */
  needs: boolean;
  /** The words in place of "Needs you" where it waits on somebody else. */
  word: string;
  /** The reader may answer it, so its row offers the answer. */
  answers: boolean;
}

/** What a run row says after its state where the reader may answer a run that
 *  waits on somebody else. */
export const YOU_CAN_ANSWER = "you can answer";

/**
 * The run's standing for the reader. A row the engine has not read yet (a
 * live entry before its durable row) carries none of the facts, and is drawn
 * as waiting on whoever its audience names, with no answer offered.
 */
export function runStanding(run: ExecutorRow, nameOf: (handle: string) => string): RunStanding {
  const needs = run.needs_you === true;
  const answers = run.may_answer === true;
  if (needs) return { needs, answers, word: "Needs you" };
  const who = (run.put_to?.length ? run.put_to : (run.audience_handles ?? [])).map(nameOf);
  return {
    needs,
    answers,
    word: who.length > 0 ? `Waiting on ${andList(who)}` : "Waiting on a person",
  };
}
