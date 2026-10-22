/**
 * One keyed REST write, and what came of it: the one shape every gesture that
 * writes over REST with an operation key is answered in.
 *
 * A write a person makes is one request whose answer is one of three things
 * they act on differently:
 *
 *  - **done**: `200`/`201` (this node has it) or `202` (`pending`: the record
 *    is durable and this node has not applied it yet, so a list catches up a
 *    moment later);
 *  - **unknown**: nobody can say whether it landed: the engine's `503` with
 *    `outcome: "unknown"`, or no answer at all (a dropped connection, a
 *    gateway's page). It is NEVER retried here: a retry is the person's, and it
 *    sends the SAME operation key, which the engine resolves to the first
 *    attempt's write rather than a second one, except on a route that reads no
 *    key, where a retry is a new one and the sentence says so. So is a write
 *    that LANDED and whose answer the node could not read back yet — a created
 *    person's first password link, an invitation's: the engine's `503` with the
 *    write's own outcome (`applied` or `pending`), in its own words. Only the
 *    same request under the same key hands back what it made, and a new key is
 *    refused by what the first attempt now holds;
 *  - **refused**: the engine said no, in its own words, with the access that
 *    would have admitted the caller where that access is the way past the
 *    rule, the steps a sequence had already landed (`landed`, worded by the
 *    caller) where it was refused part way, and the refusal itself for a
 *    caller that branches on its code. A `409 stale` is marked `stale`: what
 *    the gesture was about has moved on, so the same request is refused the
 *    same however often it is sent.
 *
 * A `403 step_up_required` is not among them: `protocol/rest.ts` asks the
 * person to confirm who they are and REPLAYS the same request, key included,
 * so a gesture refused for a stale proof is made once they have confirmed.
 *
 * ONE KEY PER REQUEST ([useKeyedGesture]): minted when the gesture starts,
 * kept for the retry an `unknown` asks for (the SAME request sent again), and
 * replaced after any answer that settled it, and for a request that is not the
 * one the unknown answer was for. A key sent again with another body is
 * refused as a reused key, in the engine's own words about operation ids.
 *
 * The identity directory's writes (`iamWrite.ts`) and the artifacts' are both
 * this; neither restates it.
 */

import { useCallback, useState } from "react";
import { isAbort, newActOpID, rest, RestError, type QueryValue } from "~/protocol/index.ts";
import { openSentence } from "./format.ts";
import { refusalText } from "./refusal.ts";
import { accessToAsk } from "./authorityRemedy.ts";

/** What one keyed write came to. */
export type KeyedAnswer =
  | {
      kind: "done";
      /** `202`: durable, and this node has not applied it yet. */
      pending: boolean;
      /** The engine's answer, verbatim: a link, a token, an id, a version. */
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
       * again is refused again; a dialog offers only a way out.
       */
      stale: boolean;
      /**
       * The refusal as the engine wrote it, for a caller that branches on its
       * code or reads a field it carries (a publish's `version_moved` names
       * the live head). Absent where no engine answered.
       */
      error?: RestError;
    };

export interface KeyedRequest {
  method: "POST" | "PUT" | "PATCH" | "DELETE";
  path: string;
  body?: unknown;
  query?: Record<string, QueryValue>;
  /** Headers the route reads beside the key: a publish's `If-Match`. */
  headers?: Record<string, string>;
  /**
   * The gesture's operation key, sent as `Idempotency-Key`; absent on a route
   * that reads none.
   */
  key?: string;
}

/** What a caller's surface says beyond the generic answer. */
export interface KeyedWriteOptions {
  /**
   * The words for each step a refused sequence had already landed (the
   * refusal's `landed` list), so a half-landed write never reads as one that
   * changed nothing.
   */
  landed?: Readonly<Record<string, string>>;
}

/**
 * One keyed write, answered as a value: it rejects only for the caller's own
 * abort, which is not an answer.
 */
export async function keyedWrite(
  req: KeyedRequest,
  options: KeyedWriteOptions = {},
): Promise<KeyedAnswer> {
  try {
    const headers = { ...req.headers, ...(req.key ? { "Idempotency-Key": req.key } : {}) };
    const answer = await rest.request(req.method, req.path, {
      ...(req.body === undefined ? {} : { body: req.body }),
      ...(req.query ? { query: req.query } : {}),
      ...(Object.keys(headers).length > 0 ? { headers } : {}),
    });
    const body =
      answer.body && typeof answer.body === "object"
        ? (answer.body as Record<string, unknown>)
        : {};
    return { kind: "done", pending: answer.status === 202, body };
  } catch (err) {
    if (isAbort(err)) throw err;
    return refusalOf(err, req.key ?? "", options.landed ?? {});
  }
}

/** What a failed request means: a refusal, or an outcome nobody knows. */
function refusalOf(
  err: unknown,
  sent: string,
  landedWords: Readonly<Record<string, string>>,
): KeyedAnswer {
  if (!(err instanceof RestError)) {
    return { kind: "refused", text: refusalText(err), stale: false };
  }
  // WHAT A SEQUENCE HAD ALREADY CHANGED when a later step was refused or went
  // unconfirmed, said either way, or a half-landed write reads as one that
  // changed nothing.
  const landed = Array.isArray(err.body.landed)
    ? err.body.landed.filter((s): s is string => typeof s === "string")
    : [];
  const changed =
    landed.length > 0
      ? ` What did change: ${landed.map((s) => landedWords[s] ?? s).join(" and ")}.`
      : "";
  // A WRITE THAT LANDED, whose answer the node could not read back yet: the
  // key is kept for the retry its words ask for, which is the only one that
  // hands back what it made.
  const unread =
    err.status === 503 && (err.body.outcome === "applied" || err.body.outcome === "pending");
  if (unread && sent) {
    const key = typeof err.body.op_id === "string" ? err.body.op_id : sent;
    return {
      kind: "unknown",
      key,
      text: `${openSentence(err.detail)}${changed}`,
    };
  }
  const unknown = err.unanswered || (err.status === 503 && err.body.outcome === "unknown");
  if (unknown) {
    const key = sent ? (typeof err.body.op_id === "string" ? err.body.op_id : sent) : "";
    // KEYED, A RETRY IS THE SAME OPERATION, which the engine applies once:
    // that promise is the whole of what the reader needs.
    if (key) {
      return {
        kind: "unknown",
        key,
        text: `${changed.trim()} Try again. It won’t happen twice.`.trim(),
      };
    }
    const said = err.unanswered
      ? "No answer came back, so nothing here knows whether this landed."
      : "The engine could not confirm whether this landed.";
    const next = openSentence(err.detail) || "Try again: this makes a new one.";
    return { kind: "unknown", key, text: `${said}${changed} ${next}` };
  }
  // A CONFIRMATION THE PERSON CANCELLED is no refusal of theirs: the engine's
  // sentence told them to confirm, which they had just declined, so the
  // dialog says what happened and keeps its button ready to try again.
  if (err.code === "step_up_required") {
    return {
      kind: "refused",
      text: `You didn’t confirm, so nothing changed.${changed}`,
      stale: false,
      error: err,
    };
  }
  // ONLY WHERE ACCESS IS THE WAY PAST does a refusal name the access the
  // caller lacks, and then in ONE sentence, as a refused change says it
  // everywhere ([accessToAsk]): the engine's own sentence lists every access
  // in general, and the two side by side said it twice. The rule decides,
  // never the grants alone: an artifact's role names the admin override
  // beside the share its owner makes, and reads the engine's own sentence.
  const asked = accessToAsk(err);
  const parts = asked === null ? [refusalText(err)] : [asked];
  return {
    kind: "refused",
    text: parts.join(" ") + changed,
    stale: err.status === 409 && err.code === "stale",
    error: err,
  };
}

/** One gesture's write: its key, its answer, and whether it is out. */
export interface KeyedGesture<R extends KeyedRequest = KeyedRequest> {
  busy: boolean;
  /** The last answer, or null before one. */
  answer: KeyedAnswer | null;
  /**
   * Send the write. `keyed: false` for a route that reads no key. The key is
   * the unknown answer's where the last one was unknown and this is the
   * request it was for (the retry it asked for), and a fresh one otherwise.
   */
  run: (req: Omit<R, "key">, keyed?: boolean) => Promise<KeyedAnswer | null>;
  /** Forget the last answer, for a dialog that opens again. */
  reset: () => void;
}

/** A gesture whose writes go through `write`: [keyedWrite], or a surface's own words over it. */
export function useKeyedGesture<R extends KeyedRequest = KeyedRequest>(
  write: (req: R) => Promise<KeyedAnswer> = keyedWrite,
): KeyedGesture<R> {
  const [busy, setBusy] = useState(false);
  const [answer, setAnswer] = useState<KeyedAnswer | null>(null);
  // THE REQUEST THE LAST ANSWER WAS FOR, which an unknown answer's key names.
  const [sent, setSent] = useState("");
  const run = useCallback(
    async (req: Omit<R, "key">, keyed = true) => {
      if (busy) return null;
      const request = JSON.stringify([
        req.method,
        req.path,
        req.query ?? null,
        req.body ?? null,
        req.headers ?? null,
      ]);
      const key = keyed
        ? answer?.kind === "unknown" && answer.key && sent === request
          ? answer.key
          : newActOpID()
        : undefined;
      setBusy(true);
      try {
        const next = await write({ ...req, ...(key ? { key } : {}) } as R);
        setSent(request);
        setAnswer(next);
        return next;
      } finally {
        setBusy(false);
      }
    },
    [answer, busy, sent, write],
  );
  const reset = useCallback(() => {
    setSent("");
    setAnswer(null);
  }, []);
  return { busy, answer, run, reset };
}
