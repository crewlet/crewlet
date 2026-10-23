/**
 * What a write's answer means: the one interpreter of "did it land", shared by
 * every surface that makes a change for a person.
 *
 * THE DASHBOARD'S BUTTONS (`act.ts`) AND THE ASSISTANT'S APPROVALS
 * (`assistant.ts`) READ ONE ANSWER. An approval executes the same request the
 * button beside it would have sent, so what its answer can be, and what each
 * tells a person, cannot be decided twice: a copy would come to call a lost
 * connection a refusal, or a refusal a success, on one surface and not the
 * other.
 *
 * # What an answer can be
 *
 *  - `applied`: the record landed, at a position;
 *  - `pending`: the engine accepted it and this node has not applied it yet;
 *  - `unknown`: nobody can say whether it landed (the connection dropped
 *    after the request left, a gateway gave up, or the engine said so). A
 *    retry is the person's decision, made under the same operation;
 *  - `refused`: the engine said no, with a code from `contract/errors.ts` and
 *    the sentence a person is shown for it.
 *
 * # Every sentence drawn is a person's (ADR-0060)
 *
 * The engine answers a refusal's `detail` in a person's words, or with none
 * where nobody has worded it yet, and then its `message` (the code's own
 * sentence) is what is drawn. An answer from an older node during a rolling
 * upgrade may lack `caveats` (read as none) or carry no detail (its message
 * stands); neither is an error.
 */

// RELATIVE, not through `~`: protocol/ is also built alone as protocol.js,
// where the alias does not exist.
import { ACT_ERRORS } from "../contract/errors.ts";
import { CAVEAT_ACTIONS } from "../contract/caveats.ts";
import { RestError } from "./rest.ts";
import { retryAfterMs } from "./retry.ts";
import type { Remedy } from "./remedy.ts";

/** A refusal code this build has a sentence for. */
export type ActErrorCode = keyof typeof ACT_ERRORS;

/** What a caveat asks of the person who reads it: `contract/caveats.ts`. */
export type CaveatAction = (typeof CAVEAT_ACTIONS)[number];

/**
 * One thing a landed write says beside its outcome, in a PERSON'S words: the
 * engine answers each caveat's person phrasing here and takes the model's
 * (`warnings`, which name tools and arguments) off the receipt, so nothing a
 * screen draws from an answer is advice written for a model (ADR-0060).
 */
export interface Caveat {
  /** The engine's stable code for the kind of caveat. */
  code: string;
  /** The sentence a person is shown. */
  message: string;
  /** Whether the person must act on it (`required`) or only know it (`none`). */
  action: CaveatAction;
}

/** What a write's 2xx answer came to. */
export type WriteLanded =
  | {
      kind: "applied" | "pending";
      position: string | null;
      receipt: unknown;
      /** What the write says beside its outcome, in a person's words. */
      caveats: Caveat[];
    }
  | { kind: "unknown"; reason: string };

/**
 * The caveats an answer carries, as this build reads them: each with a code
 * and a sentence, and an action this build knows. An action it does not know
 * (a newer engine's) is read as `none`, the engine's own rule for a caveat it
 * cannot explain: a toast nobody can say why it stays must not demand
 * dismissal. Anything that is not a caveat is dropped.
 */
export function caveatsOf(raw: unknown): Caveat[] {
  if (!Array.isArray(raw)) return [];
  const known = new Set<string>(CAVEAT_ACTIONS);
  const out: Caveat[] = [];
  for (const item of raw) {
    const c = (item ?? {}) as { code?: unknown; message?: unknown; action?: unknown };
    if (typeof c.code !== "string" || typeof c.message !== "string") continue;
    if (c.message.trim() === "") continue;
    const action =
      typeof c.action === "string" && known.has(c.action) ? (c.action as CaveatAction) : "none";
    out.push({ code: c.code, message: c.message, action });
  }
  return out;
}

/** What a write that did not land came to. */
export type WriteFailed =
  | { kind: "unknown"; reason: string }
  | {
      kind: "refused";
      /** The engine's code, or `""` for a refusal that carried none this build knows. */
      code: ActErrorCode | "";
      /** The sentence a person is shown. */
      sentence: string;
      /**
       * The engine's `detail` where it is written for the person (a code in
       * [WORDED_FOR_A_PERSON], ADR-0060), and `""` where it sent none or wrote
       * it for whoever built the request. Where there is one it is also the
       * sentence above ([sentenceOf]); a screen that shows the engine's words
       * for every refusal reads it here (`answer_knowledge`).
       */
      detail: string;
      /** The engine's own remedy, where it sent one. */
      hint: string;
      /**
       * On a refusal on AUTHORITY, the grants any ONE of which would have
       * admitted the caller, read from the answer. Empty everywhere else.
       */
      grants: string[];
      /**
       * On a refusal on AUTHORITY, the rule that decided it, read from the
       * answer; "" everywhere else. The grants alone cannot say whether the
       * access they name is a way past it (a stale proof names the access that
       * admitted the person), and the rule can (`lib/authorityRemedy.ts`).
       */
      reason: string;
      /** Whether sending it again may succeed: the engine said waiting can change it. */
      retryable: boolean;
      /** Where the limit that caused it is changed, for a refusal a limit caused (a spent budget, a model at its usage limit). */
      remedy: Remedy | null;
      /**
       * Whether the screen the press was made from is OUT OF DATE (see
       * [LOOK_AGAIN]), so every question the write would have moved is asked
       * again before the next press.
       */
      lookAgain: boolean;
    };

/**
 * What a person is told of a change nobody could confirm: the engine's own
 * `detail` on an unknown is the operation key's retry rule, written for a
 * client sending the header, and a person never sees the key (ADR-0060).
 */
export const UNCONFIRMED_SENTENCE =
  "Nobody could confirm it was saved. Look again to see whether it was.";

/** Whether a code off the wire is one this build knows. */
export function isActErrorCode(code: string): code is ActErrorCode {
  return Object.hasOwn(ACT_ERRORS, code);
}

/**
 * The 2xx answer `{outcome, position, receipt, caveats}` a write returns.
 *
 * AN OUTCOME THIS BUILD DOES NOT KNOW IS NOT A SUCCESS, the engine's own rule
 * for its own answer (`receiptOf`), for the same reason: it is the one reading
 * that cannot tell a person to stop looking at a change nobody vouched for.
 */
export function writeLanded(body: unknown): WriteLanded {
  const answer = (body ?? {}) as {
    outcome?: unknown;
    position?: unknown;
    receipt?: unknown;
    caveats?: unknown;
  };
  if (answer.outcome !== "applied" && answer.outcome !== "pending") {
    return {
      kind: "unknown",
      reason: "The engine accepted the change and could not confirm it landed.",
    };
  }
  return {
    kind: answer.outcome,
    position: typeof answer.position === "string" ? answer.position : null,
    receipt: answer.receipt,
    caveats: caveatsOf(answer.caveats),
  };
}

/**
 * The refusals that say the screen the press was made from is OUT OF DATE:
 * somebody else changed the object since it was drawn (`stale_version`, or a
 * `conflict`: a race, or a state the screen did not show yet, an archived
 * room or an item in the trash), removed it (`not_found`), made it already or
 * took the name it asked for (`exists`), answered it (`already_answered`), or
 * moved what the request named so its operation already landed on something
 * else (`invalid_input`). Each asks again every question the write would have
 * moved, without raising a floor, since nothing of this tab's landed, so the
 * page redraws what IS there and the next press is made against it.
 */
const LOOK_AGAIN = new Set<string>([
  "stale_version",
  "conflict",
  "not_found",
  "exists",
  "already_answered",
  "invalid_input",
]);

/** What a refusal, or a request that never got an answer, came to. */
export function writeFailed(err: RestError): WriteFailed {
  // THE ENGINE SAID IT DOES NOT KNOW: a call interrupted in flight, or a
  // write whose outcome nobody can establish, answers `outcome: unknown`
  // beside an `unavailable` that is otherwise what a tool that wrote nothing
  // answers.
  if (err.body.outcome === "unknown") {
    return { kind: "unknown", reason: UNCONFIRMED_SENTENCE };
  }
  // NEVER ANSWERED, OR ANSWERED BY SOMETHING THAT IS NOT THE ENGINE
  // ([RestError.unanswered]: status 0, a body that is not the engine's JSON,
  // an answer with no engine code), and a 5xx carrying a code this transport
  // never answers with: a gateway that gave up waiting says nothing about an
  // engine that may have written. All of those are "it may have landed",
  // never a refusal.
  const code = isActErrorCode(err.code) ? err.code : "";
  if (err.unanswered || (err.status >= 500 && code === "")) {
    return {
      kind: "unknown",
      reason: err.detail || `The answer was lost (status ${err.status}).`,
    };
  }
  // THE ENGINE SAYS WHETHER WAITING CHANGES IT: a `503` it wrote carries a
  // `Retry-After` where it does and none where it does not (a full log, a
  // record this node cannot decode), read through the one interpreter of
  // that hint, so a press offered "Try again" is one the engine said could
  // land.
  const retryable = err.retryHint !== null && retryAfterMs(err.retryHint) !== null;
  const detail = code !== "" && WORDED_FOR_A_PERSON.has(code) ? err.detail : "";
  return {
    kind: "refused",
    code,
    sentence: sentenceOf(code, err, detail, retryable),
    detail,
    hint: err.hint,
    grants: err.grants,
    reason: err.reason,
    retryable,
    remedy: err.remedy,
    lookAgain: LOOK_AGAIN.has(code),
  };
}

/**
 * The codes whose `detail` the engine writes for the person reading it and
 * for nobody else (ADR-0060): every refusal class a tool answers
 * (`mcp.Refusals`), which the act route answers through its one refusal
 * mapping (`operator.Fail`) with a `detail` only where that refusal was
 * worded for a person, and none otherwise; and `invalid_input`, an operation
 * key already used for another change, which the same mapping answers in the
 * one sentence every surface taking a key says it in (`opkey.Reused`).
 *
 * NOT THE REQUEST GUARD'S OR THE TRANSPORT'S OWN CODES. Where one of those
 * carries a `detail` (a node draining or still starting, a request from
 * another site, a reader the credential is not), it is written for whoever
 * built the request and names headers, routes and settings, so the
 * dashboard's sentence for the code stands. A code missing here keeps that
 * sentence: it says less, and never a developer's words.
 */
const WORDED_FOR_A_PERSON: ReadonlySet<ActErrorCode> = new Set<ActErrorCode>([
  "invalid",
  "not_found",
  "forbidden",
  "stale_version",
  "conflict",
  "exists",
  "already_answered",
  "reassignment_budget",
  "inbox_full",
  "quota_exceeded",
  "notes_full",
  "notes_spent",
  "not_running",
  "budget_exhausted",
  "unavailable",
  "internal_error",
  "invalid_input",
]);

/**
 * The refusals whose fixed sentence asks the person to wait, and which the
 * engine answers with no `Retry-After` where no wait clears them: a
 * condition that is stuck (a full log, an evicted node, a note the running
 * node could not verify) or a refusal only a person's change clears (a model
 * the company has not set up). The engine then answers its own sentence for
 * the code, the one that says waiting won't help (`internal/api/httpjson`'s
 * `waitless`), and the dashboard's "Try again in a moment" would say the
 * opposite. Every other code whose sentence asks for a wait is one the
 * engine always answers with a `Retry-After`: `draining`
 * (internal/api/drain.go), `starting` (internal/api/boot.go) and
 * `no_active_revision` (httpjson's `NoActiveRevision`).
 */
const WAITLESS_WHERE_STUCK: ReadonlySet<ActErrorCode> = new Set<ActErrorCode>([
  "unavailable",
  "identity_unavailable",
]);

/**
 * The sentence a person is shown for a refusal.
 *
 * THE DASHBOARD'S OWN, from `contract/errors.ts`, wherever it has one and the
 * engine's answer cannot contradict it. Three exceptions, and each is a case
 * where it can:
 *
 *  - THE ENGINE WROTE THE DETAIL FOR THE PERSON: `detail` is the engine's
 *    words for a code in [WORDED_FOR_A_PERSON], which it answers only where
 *    somebody worded that refusal for whoever pressed. They are about this
 *    refusal (who already answered the question, whose token budget is spent,
 *    that the change moved since it was first sent), where the fixed sentence
 *    is about every refusal of its code, so they are shown whatever the code
 *    and whatever the answer's `Retry-After`;
 *  - A FIXED SENTENCE ASKING FOR A WAIT the answer does not carry
 *    ([WAITLESS_WHERE_STUCK]) gives way to the engine's own sentence for the
 *    code, which says waiting won't change it;
 *  - A RULE'S REFUSAL is said by its rule: `unauthorized` is every refusal
 *    the authority table makes, and the engine's sentence for one follows
 *    the `reason` that decided it, where the fixed sentence for the code says
 *    the reader lacks access, to somebody whose role on an artifact is below
 *    the verb's or whose account is not active among them.
 *
 * A code this build does not know, or one whose sentence is the engine's
 * (`null`), is the engine's own words: its `detail`, or where it sent none,
 * its sentence for the code.
 */
function sentenceOf(
  code: ActErrorCode | "",
  err: RestError,
  detail: string,
  retryable: boolean,
): string {
  const own = code === "" ? null : ACT_ERRORS[code];
  if (detail !== "") return detail;
  if (code === "unauthorized" && err.reason !== "" && err.sentence !== "") return err.sentence;
  if (own !== null && code !== "" && !retryable && WAITLESS_WHERE_STUCK.has(code)) {
    return err.sentence || NO_WAIT_CLEARS;
  }
  return (
    own ?? (err.detail || err.sentence || `The engine refused the change (status ${err.status}).`)
  );
}

/** What a refusal no wait clears says where the engine's answer said nothing. */
const NO_WAIT_CLEARS = "This can't be done here, and waiting won't change that.";
