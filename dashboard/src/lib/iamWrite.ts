/**
 * A write to the identity directory (`/iam`), and what came of it.
 *
 * Every gesture made about somebody in the directory — on a vacant human seat
 * (the org chart's card menu, the seat's peek, its page): an invitation, a
 * person's create, an invitation's cancellation; on People & access: a
 * service account, an edit, a suspension, a reset, a revocation, a removal —
 * is one REST write whose answer is one of four things a person acts on
 * differently:
 *
 *  - **done** — `200`/`201` (this node has it) or `202` (`pending`: the record
 *    is durable and this node has not applied it yet, so the lists catch up a
 *    moment later);
 *  - **unknown** — nobody can say whether it landed: the engine's `503` with
 *    `outcome: "unknown"`, or no answer at all (a dropped connection, a
 *    gateway's page). It is NEVER retried here: a retry is the person's, and
 *    it sends the SAME operation key, which the engine resolves to the first
 *    attempt's write rather than a second one — except on the two routes that
 *    read no key (a token's mint, a reset link's issue), where a retry is a new
 *    one and the sentence says so;
 *  - **refused** — the engine said no, in its own words, with the grants that
 *    would have admitted the caller where the refusal named them, and the steps
 *    of an edit that had already landed (`landed`) where it was refused part
 *    way. A `409 stale` is marked `stale`: the thing the gesture was about has
 *    moved on — an invitation redeemed meanwhile — so the same request is
 *    refused the same however often it is sent.
 *
 * A `403 step_up_required` is not among them: `protocol/rest.ts` asks the
 * person to confirm who they are and REPLAYS the same request, key included, so
 * a gesture refused for a stale proof is made once they have confirmed.
 *
 * EVERY ANSWER IS FOLLOWED BY A RE-READ of the lists the write touches, not
 * only a `done` one: a refused edit may have landed its login and seat first
 * (`landed`), and an unknown one may have landed whole — the re-read is what
 * shows which.
 *
 * ONE KEY PER REQUEST ([useIamGesture]): minted when the gesture starts, kept
 * for the retry an `unknown` asks for — the SAME request sent again — and
 * replaced after any answer that settled it, and for a request that is not the
 * one the unknown answer was for. A create's key sent again with another body
 * is refused as a reused key, in the engine's own words about operation ids:
 * an address corrected after a dropped connection and sent under the first
 * attempt's key answered that, and only the press after it issued anything.
 * A PERSON'S CREATE leans on the key hardest: the engine derives the person
 * and their first password link from the operation, so the retry an unknown
 * answer asks for hands back the same link rather than a second person.
 */

import {
  keyedWrite,
  useKeyedGesture,
  type KeyedAnswer,
  type KeyedGesture,
  type KeyedRequest,
} from "./keyedWrite.ts";

/** What one `/iam` write came to: the generic keyed answer (`keyedWrite.ts`). */
export type IamAnswer = KeyedAnswer;

export interface IamRequest extends Omit<KeyedRequest, "method" | "headers"> {
  method: "POST" | "PATCH" | "DELETE";
}

/** The steps of a sequence that landed before it was refused, as words. */
const LANDED: Record<string, string> = {
  identity: "the login and seat",
  stage: "the stage",
};

/**
 * One `/iam` write, answered as a value: it rejects only for the caller's own
 * abort, which is not an answer.
 */
export function iamWrite(req: IamRequest): Promise<IamAnswer> {
  return keyedWrite(req, { landed: LANDED });
}

/** One gesture's write: its key, its answer, and whether it is out. */
export type IamGesture = KeyedGesture<IamRequest>;

export function useIamGesture(): IamGesture {
  return useKeyedGesture(iamWrite);
}
