/**
 * A write to the identity directory (`/iam`), and what came of it.
 *
 * Every gesture People & access makes — an invitation, a service account, an
 * edit, a suspension, a reset, a revocation, a removal — is one REST write
 * whose answer is one of four things a person acts on differently:
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
 */

import { useCallback, useState } from "react";
import { isAbort, newActOpID, rest, RestError, type QueryValue } from "~/protocol/index.ts";
import { refusalText } from "./refusal.ts";

/** What one `/iam` write came to. */
export type IamAnswer =
  | {
      kind: "done";
      /** `202`: durable, and this node has not applied it yet. */
      pending: boolean;
      /** The engine's answer, verbatim — a link, a token, an id. */
      body: Record<string, unknown>;
    }
  | {
      kind: "unknown";
      /** The key a retry sends, or "" on a route that reads none. */
      key: string;
      text: string;
    }
  | {
      kind: "refused";
      text: string;
      /**
       * `409 stale`: what the gesture was about has moved on, so sending it
       * again is refused again — a dialog offers only a way out.
       */
      stale: boolean;
    };

export interface IamRequest {
  method: "POST" | "PATCH" | "DELETE";
  path: string;
  body?: unknown;
  query?: Record<string, QueryValue>;
  /**
   * The gesture's operation key, sent as `Idempotency-Key`; absent on the two
   * routes that read none.
   */
  key?: string;
}

/** The steps of a sequence that landed before it was refused, as words. */
const LANDED: Record<string, string> = {
  identity: "the login and seat",
  stage: "the stage",
};

/**
 * One `/iam` write, answered as a value — it rejects only for the caller's own
 * abort, which is not an answer.
 */
export async function iamWrite(req: IamRequest): Promise<IamAnswer> {
  try {
    const answer = await rest.request(req.method, req.path, {
      ...(req.body === undefined ? {} : { body: req.body }),
      ...(req.query ? { query: req.query } : {}),
      ...(req.key ? { headers: { "Idempotency-Key": req.key } } : {}),
    });
    const body =
      answer.body && typeof answer.body === "object"
        ? (answer.body as Record<string, unknown>)
        : {};
    return { kind: "done", pending: answer.status === 202, body };
  } catch (err) {
    if (isAbort(err)) throw err;
    return refusalOf(err, req.key ?? "");
  }
}

/** What a failed request means: a refusal, or an outcome nobody knows. */
function refusalOf(err: unknown, sent: string): IamAnswer {
  if (!(err instanceof RestError)) {
    return { kind: "refused", text: refusalText(err), stale: false };
  }
  // WHAT AN EDIT HAD ALREADY CHANGED when a later step was refused or went
  // unconfirmed — said either way, or a half-landed edit reads as one that
  // changed nothing.
  const landed = Array.isArray(err.body.landed)
    ? err.body.landed.filter((s): s is string => typeof s === "string")
    : [];
  const changed =
    landed.length > 0
      ? ` What did change: ${landed.map((s) => LANDED[s] ?? s).join(" and ")}.`
      : "";
  const unknown = err.unanswered || (err.status === 503 && err.body.outcome === "unknown");
  if (unknown) {
    const key = sent ? (typeof err.body.op_id === "string" ? err.body.op_id : sent) : "";
    const said = err.unanswered
      ? "No answer came back, so nothing here knows whether this landed."
      : "The engine could not confirm whether this landed.";
    const next = key
      ? "Try again as it is: it sends the same operation, which lands once. Changed, it is a new one."
      : asked(err.detail) || "Try again: this makes a new one.";
    return { kind: "unknown", key, text: `${said}${changed} ${next}` };
  }
  const parts = [refusalText(err)];
  // ONLY A RULE'S REFUSAL names grants the caller lacks. A step-up refusal
  // names the grants that ADMITTED them, so said here it told somebody holding
  // people:manage that people:manage would admit them.
  if (err.code === "unauthorized" && err.grants.length > 0) {
    parts.push(`Any one of ${err.grants.join(", ")} would admit you.`);
  }
  return {
    kind: "refused",
    text: parts.join(" ") + changed,
    stale: err.status === 409 && err.code === "stale",
  };
}

/** The engine's own sentence, capitalised, or "". */
function asked(detail: string): string {
  const trimmed = detail.trim();
  return trimmed ? trimmed[0]!.toUpperCase() + trimmed.slice(1) : "";
}

/** One gesture's write: its key, its answer, and whether it is out. */
export interface IamGesture {
  busy: boolean;
  /** The last answer, or null before one. */
  answer: IamAnswer | null;
  /**
   * Send the write. `keyed: false` for the two routes that read no key. The
   * key is the unknown answer's where the last one was unknown and this is
   * the request it was for — the retry it asked for — and a fresh one
   * otherwise.
   */
  run: (req: Omit<IamRequest, "key">, keyed?: boolean) => Promise<IamAnswer | null>;
  /** Forget the last answer, for a dialog that opens again. */
  reset: () => void;
}

export function useIamGesture(): IamGesture {
  const [busy, setBusy] = useState(false);
  const [answer, setAnswer] = useState<IamAnswer | null>(null);
  // THE REQUEST THE LAST ANSWER WAS FOR, which an unknown answer's key names.
  const [sent, setSent] = useState("");
  const run = useCallback(
    async (req: Omit<IamRequest, "key">, keyed = true) => {
      if (busy) return null;
      const request = JSON.stringify([req.method, req.path, req.query ?? null, req.body ?? null]);
      const key = keyed
        ? answer?.kind === "unknown" && answer.key && sent === request
          ? answer.key
          : newActOpID()
        : undefined;
      setBusy(true);
      try {
        const next = await iamWrite({ ...req, ...(key ? { key } : {}) });
        setSent(request);
        setAnswer(next);
        return next;
      } finally {
        setBusy(false);
      }
    },
    [answer, busy, sent],
  );
  const reset = useCallback(() => {
    setSent("");
    setAnswer(null);
  }, []);
  return { busy, answer, run, reset };
}
