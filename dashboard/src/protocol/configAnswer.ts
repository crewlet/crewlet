/**
 * What an answer from the configuration surface MEANS — the pure half of
 * `protocol/configWrite.ts`.
 *
 * ONE READING OF A `/config` REFUSAL, for every screen that writes the
 * company document: the builder's check and save, a budget raised in place,
 * an MCP server added, a model chosen. Each of those used to be one more place
 * a 409 could be read as "invalid" or a 503 as "refused", and the reading that
 * matters most — whether a write that got no answer may have landed — is the
 * one that is easiest to get wrong and hardest to notice wrong.
 *
 * PURE, AND IMPORTING NOTHING BUT TYPES, so the builder's model (which may not
 * reach the network, a clock or a global — `model/boundary.test.ts`) can
 * classify through it rather than keeping a second copy of the rule. The
 * network half is `configWrite.ts`.
 */

import type { SeatHeldHolder } from "../contract/config.ts";
import type { ConfigProblem, ConfigWarning, Derived } from "./types.ts";

/** What the engine answered: its status, its parsed body and its entity tag.
 *  Status 0 is a request that was never fully answered. */
export interface ConfigAnswer {
  readonly status: number;
  readonly body: unknown;
  readonly etag?: string | null;
}

/** Why the engine holds a revision the write was not built on. */
export type ConfigConflictReason =
  /** `409 revision_advanced`: another write activated after the write's base. */
  | "revision_advanced"
  /** `412 already_configured`: a company exists, and the write was creating one. */
  | "already_configured"
  /** `409` or `412 no_active_revision`: the write edits a company the engine no longer holds. */
  | "no_active_revision"
  /** `412 entity_exists`: a create-only entity write named an id the revision already carries. */
  | "entity_exists";

/** What a refusal from the configuration surface means. */
export type ConfigRefusal =
  /**
   * The engine refused the caller on AUTHORITY: this surface is guarded in
   * full. `code` is the refusal's own — `unauthorized` for a grant the caller
   * does not hold, `step_up_required` for a confirmation they declined,
   * `csrf_origin`, `invalid_token` — because each sends a person somewhere
   * different, and only the first is a grant to ask an administrator for.
   */
  | { readonly kind: "guarded"; readonly code: string }
  | {
      readonly kind: "conflict";
      readonly reason: ConfigConflictReason;
      /** The revision the engine holds instead, where it named one. */
      readonly currentRevisionId: string | null;
    }
  /**
   * `503 draining`: the drain gate refused it BEFORE the handler ran, so
   * nothing was stored. The one 5xx that is certain.
   */
  | { readonly kind: "draining"; readonly detail: string }
  /**
   * Never answered, or answered with a 5xx that is not the drain gate's: a
   * gateway that gave up, an engine that failed after storing the revision.
   * For a WRITE this is "it may have landed" — settle it before believing
   * either way.
   */
  | { readonly kind: "unreachable"; readonly detail: string }
  /** The document or the request was refused: every other status. */
  | {
      readonly kind: "problems";
      /** Never empty: a refusal carrying none is given one at document level from its detail. */
      readonly problems: readonly ConfigProblem[];
      readonly derived: Derived | null;
      readonly code: string;
      readonly hint: string;
    };

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

const text = (value: unknown): string => (typeof value === "string" ? value : "");

/**
 * What a non-2xx answer from `/config` means.
 *
 * A 2xx is the caller's to read, because what a success says differs by what
 * was asked (a dry run's base, a save's revision); every refusal means the
 * same thing whichever write it answered.
 */
export function classifyConfigRefusal(answer: ConfigAnswer): ConfigRefusal {
  const body = isRecord(answer.body) ? answer.body : {};
  const code = text(body.error);
  // A WRITE REFUSED PART BY PART. Without `config:write` the engine admits a
  // write only where everything it changes is inside a unit the caller leads,
  // and its refusal names each part that is not (`refused`,
  // `configapi.RefusedChange`): the caller holds what they need for the rest,
  // so it is a problem with the draft, placed on each seat and unit it names,
  // and not a refusal of the person. A caller who leads no unit is refused
  // every part the same way, and a writer that knows its draft is the whole
  // company reads that as the grant they lack (the builder's `classifyCheck`).
  const refused = Array.isArray(body.refused) ? body.refused.filter(isRecord) : [];
  if (answer.status === 403 && refused.length > 0) {
    return {
      kind: "problems",
      problems: refused.map(refusedProblem),
      derived: null,
      code,
      hint: text(body.hint),
    };
  }
  if (answer.status === 401 || answer.status === 403) return { kind: "guarded", code };
  // A 409 THAT IS NOT A RACE. `seat_held` refuses a write taking a human seat
  // out of the company while something holds it — a person, a service
  // account, an open invitation: nothing about it moves with a newer
  // revision, so reading it as one offered to update a draft onto the very
  // revision it was built on, for ever. It is a problem with the draft, one
  // per held seat, which freeing the seat clears.
  if (answer.status === 409 && code === "seat_held") {
    return {
      kind: "problems",
      problems: heldSeatProblems(body.held, text(body.detail) || code),
      derived: null,
      code,
      hint: text(body.hint),
    };
  }
  if (answer.status === 409 || answer.status === 412) {
    const reason: ConfigConflictReason =
      code === "no_active_revision"
        ? "no_active_revision"
        : code === "already_configured"
          ? "already_configured"
          : code === "entity_exists"
            ? "entity_exists"
            : "revision_advanced";
    return {
      kind: "conflict",
      reason,
      currentRevisionId: text(body.current_revision_id) || null,
    };
  }
  const detail = text(body.detail) || code;
  if (answer.status === 503 && code === "draining") return { kind: "draining", detail };
  if (answer.status === 0 || answer.status >= 500) return { kind: "unreachable", detail };
  // Every other refusal is about the document or the request that carried it:
  // a validation error, a patch the engine could not apply, a body too large.
  const problems = Array.isArray(body.problems) ? (body.problems as ConfigProblem[]) : [];
  const message = detail || `The engine refused the change with status ${answer.status}.`;
  return {
    kind: "problems",
    problems:
      problems.length > 0
        ? problems
        : [{ path: "", segments: null, kind: "invalid", message } as ConfigProblem],
    derived: isRecord(body.derived) ? (body.derived as unknown as Derived) : null,
    code,
    hint: text(body.hint),
  };
}

/**
 * One part of a write the caller may not make, as a problem about the seat or
 * the unit it is (`seat` by handle, `unit` by KEY — where a validation
 * problem's `unit` is the unit's name, its kind `refused` says which) or
 * about the company.
 */
function refusedProblem(entry: Record<string, unknown>): ConfigProblem {
  const kind = text(entry.kind);
  const id = text(entry.id);
  return {
    path: "",
    segments: null,
    kind: "refused",
    ...(kind === "seat" ? { seat: id } : {}),
    ...(kind === "unit" ? { unit: id } : {}),
    message: refusedSentence(entry),
  };
}

/** What a refused part says, by WHY the engine refused it. */
export function refusedSentence(entry: Record<string, unknown>): string {
  const kind = text(entry.kind);
  const id = text(entry.id);
  const value = text(entry.value);
  const place = text(entry.place);
  const op = text(entry.op) || "changed";
  const grant = "takes the config:write grant";
  const subject = kind === "seat" ? `@${id}` : `Unit ${id}`;
  const where = place ? `in ${place}` : "at the company's top level";
  if (kind === "document") {
    return `This write changes nothing, and storing the company as it is ${grant}.`;
  }
  if (kind === "setting") return `The company's ${id} setting ${grant}.`;
  switch (text(entry.why)) {
    case "credential":
      return `${subject}: ${value} is a credential, and setting, changing or clearing one ${grant}.`;
    case "key":
      return `${subject}: ${value} is how another system finds it, and changing it ${grant}.`;
    case "self":
      return `${subject}'s own lead and place decide who may change it, so changing them ${grant}.`;
    // A REFERENCE AT THE ROOT names a root seat or nothing at all: the engine
    // places both there, so neither is said to be a seat.
    case "lead":
      return place
        ? `${subject}'s lead would be ${value}, in ${place}, outside the units you lead.`
        : `${subject}'s lead would be ${value}, which names no seat inside the units you lead.`;
    case "manages":
      return place
        ? `${subject} would manage ${value}, in ${place}, outside the units you lead.`
        : `${subject} would manage ${value}, which names nothing inside the units you lead.`;
    // The ADDED object, and where an entry already naming its id sits.
    case "named":
      return `${subject}: a lead or manages entry ${where} already names ${value}, outside the units you lead, so adding it ${grant}.`;
    case "duplicate":
      return `${value} answers to two seats or units, and only the config:write grant can change them.`;
    default:
      return text(entry.side) === "after"
        ? `${subject} would sit ${where}, outside the units you lead.`
        : `${subject} sits ${where}, outside the units you lead, so it cannot be ${op} here.`;
  }
}

/**
 * One problem per seat a `409 seat_held` names, each about that seat (by its
 * handle) and naming the ONE thing holding it (`SeatHeldHolder`) with the
 * command that frees it. A body that names none is one problem at document
 * level carrying the refusal's own sentence.
 *
 * THE REMEDY IS THE HOLDER'S KIND. A person holds a human seat for as long as
 * they exist, so they are MOVED to another human seat or removed — never
 * unbound, which the engine refuses for a person; a service account is
 * unbound; an open invitation is cancelled. One remedy for all of them told
 * an administrator to unbind a person, which could never clear the refusal.
 */
function heldSeatProblems(held: unknown, detail: string): ConfigProblem[] {
  const seats = isRecord(held) ? Object.entries(held).sort(([a], [b]) => a.localeCompare(b)) : [];
  const problems = seats.map(([seat, claim]): ConfigProblem => ({
    path: "",
    segments: null,
    kind: "seat_held",
    seat,
    message: `@${seat} ${heldBy(isRecord(claim) ? (claim as SeatHeldHolder) : {})}, then save again. The seat's page in the org chart says what holds it.`,
  }));
  return problems.length > 0
    ? problems
    : [{ path: "", segments: null, kind: "seat_held", message: detail }];
}

/** What holds one seat and how it is freed, as the middle of a sentence. */
function heldBy(claim: SeatHeldHolder): string {
  const id = text(claim.person);
  const who = text(claim.login) || id;
  if (id && text(claim.kind) === "machine") {
    return `is held by the service account ${who}: unbind it first (crewlet iam unbind ${id})`;
  }
  if (id) {
    return `is held by ${who}: move them to another human seat (crewlet iam bind ${id} SEAT) or remove them (crewlet iam remove ${id}) first`;
  }
  const invitation = text(claim.invitation);
  if (invitation) {
    return `is held by the open invitation ${invitation}: cancel it first (crewlet iam cancel-invite ${invitation})`;
  }
  return "is held: free it first";
}

/**
 * The warnings a change INTRODUCES: the check's, less the ones the company
 * already had.
 *
 * A check answers every warning the whole document raises, and a company
 * already carrying one — a dangling `manages:` entry three seats away — would
 * otherwise stop every ceiling change on the same unrelated sentence until
 * somebody fixed it. What a person making a change has to see is what THEIR
 * change does: a seat ceiling now above the company's, a week now below its
 * own day, a server nobody is granted.
 */
export function introducedWarnings(
  before: readonly ConfigWarning[],
  after: readonly ConfigWarning[],
): ConfigWarning[] {
  const had = new Set(before.map((w) => `${w.path}\u0000${w.message}`));
  return after.filter((w) => !had.has(`${w.path}\u0000${w.message}`));
}
