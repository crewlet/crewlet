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
  /** `401`: no key the engine accepts was presented — this surface needs
   *  one, reads included. The remedy is a key. */
  | { readonly kind: "guarded" }
  /**
   * `403 forbidden`: the key was ACCEPTED and is a member's, and the company
   * document is an admin's to read and change (ADR-0031). Nothing was
   * stored, and no other key typed into this browser is the remedy unless
   * it is an admin's — so it is never answered with the token dialog alone.
   */
  | { readonly kind: "forbidden" }
  /**
   * `403 config_managed`: the credential was ACCEPTED and is not one the
   * deployment lets change the company document — another system manages
   * it (ADR-0030). Nothing was stored, and no retry with this token changes
   * the answer; [managedSentence] is how a screen says so.
   */
  | { readonly kind: "managed"; readonly managedBy: readonly string[] }
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

/**
 * Why a managed company document cannot be changed from here, naming who
 * manages it (ADR-0030) — the ONE sentence every screen shows for it, whether
 * the viewer said so before a press or the engine refused one.
 *
 * MANAGED IS NOT A PERMISSION THIS PERSON LACKS, it is where the company is
 * written: another system renders the document and writes it with its own
 * token, and replaces any revision made here at its next reconcile. So the
 * sentence says where to change it, and what stays possible.
 */
export function managedSentence(managedBy: readonly string[]): string {
  const who = managedBy.length > 0 ? managedBy.join(", ") : "another system";
  return `This company's configuration is managed by ${who} — change it there. An edit made here would be overwritten; a credential it already names can still be rotated from Settings › Integrations.`;
}

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
  // A MANAGED DOCUMENT'S REFUSAL IS NOT ABOUT THE TOKEN, which the engine
  // accepted: it is about where the company is written (ADR-0030), so it is
  // its own answer rather than a credential to replace.
  if (answer.status === 403 && code === "config_managed") {
    const managedBy = Array.isArray(body.managed_by)
      ? body.managed_by.filter((id): id is string => typeof id === "string")
      : [];
    return { kind: "managed", managedBy };
  }
  if (answer.status === 401) return { kind: "guarded" };
  // A KEY THE ENGINE ACCEPTED AND THE SURFACE REFUSED: a member's, on the
  // admin's surface. The code and not the status, because a 403 carrying any
  // other code is a different refusal — `config_managed` above — and one that
  // names no reach is read below with whatever it said.
  if (answer.status === 403 && code === "forbidden") return { kind: "forbidden" };
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
