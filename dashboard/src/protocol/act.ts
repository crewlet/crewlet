/**
 * The dashboard's one write: a change, made as the principal this browser's
 * session resolves to (ADR-0024).
 *
 * EVERY BUTTON THAT CHANGES THE COMPANY COMES THROUGH HERE, and here goes to
 * exactly one route — `POST /operator/act/{tool}` — where the engine runs the
 * same operator tool a person's own assistant would, decided by the one
 * authority table and attributed the way every surface attributes a write: a
 * person the identity directory binds to a seat writes as that seat, and a
 * credential nobody is bound through writes under its own login. There is no
 * second write path to keep in step with it: `app/source.test.ts` holds every
 * `act(` to a literal tool, `protocol/transport.test.ts` holds every network
 * read to `src/protocol/`, and `contract/actions.ts` is the whole vocabulary,
 * held against the engine's catalogue by a Go gate.
 *
 * THE CREDENTIAL IS THE SESSION COOKIE, as for every request `rest.ts` makes:
 * this module holds nothing a script could read. A `401` is noted there as a
 * session that needs a sign-in, and a `403 step_up_required` is confirmed and
 * the SAME request replayed there — so a press refused for a stale proof is
 * made once the person has confirmed who they are, under the same operation.
 *
 * # What an answer can be, and what each tells a person
 *
 *  - `applied` — the record landed at `position`, and this tab's read floor
 *    for its domain rose to it (`protocol/session.ts`), so every read after
 *    this one includes it;
 *  - `pending` — the engine accepted it and this node has not applied it
 *    yet: the floor rises, and the reads wait for it;
 *  - `unknown` — nobody can say whether it landed: the connection dropped
 *    after the request left, a gateway gave up, or the engine itself said so.
 *    NEVER RETRIED HERE. A retry is the person's decision, and when they make
 *    it the same `opId` goes again, which the engine derives the same
 *    operations from — so a retry is the first attempt's write rather than a
 *    second one;
 *  - `refused` — the engine said no, with a code from `contract/errors.ts`
 *    and the sentence a person is shown for it.
 *
 * AND IT NEVER THROWS for an answer, a refusal or a lost connection — those
 * are all values a screen renders. It rejects only for the caller's own
 * abort, which is not an answer at all.
 */

// RELATIVE, not through `~`: protocol/ is also built alone as protocol.js,
// where the alias does not exist.
import { ACTIONS } from "../contract/actions.ts";
import { ACT_ERRORS } from "../contract/errors.ts";
import { session, type SessionDomain, type SessionFloors } from "./session.ts";
import { layoutOpID } from "./gate.ts";
import { isAbort, rest, RestError } from "./rest.ts";
import { retryAfterMs } from "./retry.ts";

/** A change the dashboard may make: a key of `contract/actions.ts`. */
export type ActionTool = keyof typeof ACTIONS;

/** The arguments a screen may pass for one change — only those its row names. */
export type ActionArgs<T extends ActionTool> = {
  [K in (typeof ACTIONS)[T]["args"][number]]?: unknown;
};

/** A refusal the act transport can answer with. */
export type ActErrorCode = keyof typeof ACT_ERRORS;

// EVERY ROW'S DOMAIN IS A LOG A READ CAN WAIT ON, checked by the compiler: a
// domain spelled wrong in the table would raise a floor no question reads.
const ROWS: {
  readonly [T in ActionTool]: {
    readonly domain: SessionDomain | null;
    readonly refreshes: readonly string[];
  };
} = ACTIONS;

/** What a change came to. */
export type ActResult =
  | {
      kind: "applied" | "pending";
      tool: ActionTool;
      /** The operation the change was made under — what a retry sends again. */
      opId: string;
      /** Where the record landed; null for a write that appended nothing. */
      position: string | null;
      domain: SessionDomain | null;
      /** The tool's own answer, verbatim. */
      receipt: unknown;
    }
  | {
      kind: "unknown";
      tool: ActionTool;
      opId: string;
      /** What is known about why nobody knows. */
      reason: string;
    }
  | {
      kind: "refused";
      tool: ActionTool;
      opId: string;
      /** The engine's code, or `""` for a refusal that carried none this build knows. */
      code: ActErrorCode | "";
      /** The sentence a person is shown. */
      sentence: string;
      /** The engine's own remedy, where it sent one. */
      hint: string;
      /**
       * On a refusal on AUTHORITY, the grants any ONE of which would have
       * admitted the caller — read from the answer, never named by a screen.
       * Empty everywhere else.
       */
      grants: string[];
      /**
       * Whether sending it again may succeed: the engine refused for the
       * NODE rather than the request, and said waiting can change that. A
       * retry sends the same operation.
       */
      retryable: boolean;
    };

export interface ActOptions {
  /**
   * The gesture's operation key. Minted once per press ([newActOpID]) and
   * sent again, unchanged, on a retry of that press — never minted per
   * attempt, or a retry would be a second write.
   */
  opId?: string;
  signal?: AbortSignal;
  /** The floors a write raises. The tab's own unless a suite brings one. */
  floors?: SessionFloors;
}

/** Whether a code off the wire is one this build knows. */
function isActErrorCode(code: string): code is ActErrorCode {
  return Object.hasOwn(ACT_ERRORS, code);
}

/**
 * A fresh operation key: a UUIDv7, stamped with the instant the gesture began.
 *
 * VERSION 7 BECAUSE THE ENGINE READS THE INSTANT. The key travels in the
 * `Idempotency-Key` header, which the act route REQUIRES and holds to the
 * engine's operation-id grammar (`statelog.CheckCallerOpID`); the operation's
 * mint instant is the key's own, and it is what the engine uses to decide
 * whether its operation ledger can still vouch for a retry. A key carrying no
 * instant is refused `op_id_invalid`. The layout is the engine's operation-id
 * grammar with no name ([layoutOpID]), so there is one encoder rather than
 * two. The engine then scopes the key by the principal that sent it, so a key
 * somebody copied names an operation of theirs, never this person's.
 *
 * `crypto.getRandomValues`, never `crypto.randomUUID`: the second exists only
 * in a secure context, and a node's dashboard reached at
 * `http://10.0.0.4:8000` is not one — every write there would throw.
 */
export function newActOpID(now: number = Date.now()): string {
  return layoutOpID(now, crypto.getRandomValues(new Uint8Array(10)), "");
}

/**
 * Make one change, as the signed-in principal.
 *
 * The body is `{args}` as JSON — the engine refuses any other content type,
 * which is what keeps a cross-site form from reaching a write — and the
 * operation rides the `Idempotency-Key` header, where every surface that
 * takes one reads it.
 */
export async function act<T extends ActionTool>(
  tool: T,
  args: ActionArgs<T>,
  options: ActOptions = {},
): Promise<ActResult> {
  const opId = options.opId ?? newActOpID();
  const floors = options.floors ?? session;
  const unknown = (reason: string): ActResult => ({ kind: "unknown", tool, opId, reason });

  let body: unknown;
  try {
    ({ body } = await rest.request("POST", `/operator/act/${encodeURIComponent(tool)}`, {
      body: { args },
      contentType: "application/json",
      headers: { "Idempotency-Key": opId },
      signal: options.signal,
    }));
  } catch (err) {
    if (isAbort(err)) throw err;
    if (!(err instanceof RestError)) return unknown(String(err));
    return refusalOf(tool, opId, err, floors);
  }

  const answer = (body ?? {}) as { outcome?: unknown; position?: unknown; receipt?: unknown };
  const outcome = answer.outcome;
  if (outcome !== "applied" && outcome !== "pending") {
    // AN OUTCOME THIS BUILD DOES NOT KNOW IS NOT A SUCCESS — the engine's own
    // rule for its own answer (`receiptOf`), for the same reason: it is the
    // one reading that cannot tell a person to stop looking at a change
    // nobody vouched for.
    return unknown("The engine accepted the change and could not confirm it landed.");
  }
  const position = typeof answer.position === "string" ? answer.position : null;
  const { domain, refreshes } = ROWS[tool];
  floors.written(domain, position, refreshes);
  return { kind: outcome, tool, opId, position, domain, receipt: answer.receipt };
}

/**
 * The refusals that say the screen the press was made from is OUT OF DATE:
 * somebody else changed the object since it was drawn (`stale_version`,
 * `conflict`), removed it (`not_found`), made it already (`exists`), answered
 * it (`already_answered`), or moved what the request named so its operation
 * already landed on something else (`invalid_input`). Each asks again every
 * question the write would have moved — without raising a floor, since
 * nothing of this tab's landed — so the page redraws what IS there and the
 * next press is made against it. Without that a task page kept sending the
 * version it was drawn at and was refused `stale_version` on every press
 * until its own poll happened to come round.
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
function refusalOf(
  tool: ActionTool,
  opId: string,
  err: RestError,
  floors: SessionFloors,
): ActResult {
  // THE ENGINE SAID IT DOES NOT KNOW: a call interrupted in flight, or a
  // write whose outcome nobody can establish, answers `outcome: unknown`
  // beside an `unavailable` that is otherwise what a tool that wrote nothing
  // answers.
  if (err.body.outcome === "unknown") {
    return { kind: "unknown", tool, opId, reason: err.detail || err.code };
  }
  // NEVER ANSWERED, OR ANSWERED BY SOMETHING THAT IS NOT THE ENGINE
  // ([RestError.unanswered]: status 0, a body that is not the engine's JSON,
  // an answer with no engine code), and a 5xx carrying a code the act
  // transport never answers with — a gateway that gave up waiting says
  // nothing about an engine that may have written. All of those are "it may
  // have landed", never a refusal.
  const code = isActErrorCode(err.code) ? err.code : "";
  if (err.unanswered || (err.status >= 500 && code === "")) {
    return {
      kind: "unknown",
      tool,
      opId,
      reason: err.detail || `The answer was lost (status ${err.status}).`,
    };
  }
  if (LOOK_AGAIN.has(code)) {
    const { domain, refreshes } = ROWS[tool];
    floors.written(domain, null, refreshes);
  }
  const own = code === "" ? null : ACT_ERRORS[code];
  const sentence =
    own ?? (err.detail || err.sentence || `The engine refused the change (status ${err.status}).`);
  return {
    kind: "refused",
    tool,
    opId,
    code,
    sentence,
    hint: err.hint,
    grants: err.grants,
    // THE ENGINE SAYS WHETHER WAITING CHANGES IT: a `503` it wrote carries a
    // `Retry-After` where it does and none where it does not (a full log, a
    // record this node cannot decode) — read through the one interpreter of
    // that hint, so a press offered "Try again" is one the engine said could
    // land.
    retryable: err.retryHint !== null && retryAfterMs(err.retryHint) !== null,
  };
}
