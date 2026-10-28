/**
 * Where a person changes the limit that refused them, and whether they may.
 *
 * A refusal a configurable limit caused carries a remedy naming the setting
 * (`protocol/remedy.ts`, ADR-0060). This module is the dashboard's half of
 * that contract: the LINK TABLE from a setting to the screen and the section
 * that edit it, and the GATE deciding whether the reader is offered the way
 * there or told who can change it.
 *
 * # Every token ceiling is raised on Company › Budgets
 *
 * The company's own ceilings, each seat's and the one every person's assistant
 * spend is held to are edited in place there, each with the same check, save
 * and revision summary (`components/budgetWrite.tsx`). So every remedy opens
 * that screen at its own section, named by `section=` so the address alone
 * says what to reveal: a link opened in a new tab lands where the drawer's
 * press does.
 *
 * # The link is offered by the editor's own gate
 *
 * The section a remedy opens draws its editor behind [orgWriteAccess], so the
 * remedy is drawn behind it too: the two cannot disagree about who may change
 * the ceiling. A reader the gate refuses for WHO THEY ARE (no Admin, a lead
 * outside the seat's unit, a document another system manages, nobody signed
 * in) is told who can change it instead, and never handed a link to a screen
 * whose controls are all locked for them. A reader held for a MOMENT (offline,
 * a step-up, a hold the screen sets) is still offered the link: the screen says
 * what holds it, and the moment passes.
 */

import type { Remedy } from "~/protocol/remedy.ts";
import type { CeilingScope } from "./ceilings.ts";
import type { ConfigWriteAccess, OrgTarget } from "./useWriteAccess.ts";

/** The query key a section of a screen is named by. */
export const SECTION_PARAM = "section";

/** Settings › Models, where the company's models are given a fallback or
 *  another plan. */
export const MODELS_PATH: readonly string[] = ["settings", "models"];

/** Company › Budgets, where every token ceiling is raised. */
export const BUDGETS_PATH: readonly string[] = ["company", "budgets"];

/** The Budgets section of the company's own ceilings. */
export const COMPANY_SECTION = "company";

/** The Budgets section of the ceiling every person's assistant is held to. */
export const ASSISTANT_SECTION = "assistant";

/** The Budgets section of one seat's own ceilings: its row in the seats table. */
export function seatSection(handle: string): string {
  return `seat:${handle}`;
}

/** Where a remedy opens, and what the press is called. */
export interface RemedyDestination {
  readonly path: readonly string[];
  readonly query: Readonly<Record<string, string>>;
  /** The press, in the reader's words: what it opens. */
  readonly label: string;
  /** What a change there is about, for the write gate. */
  readonly target: OrgTarget;
}

/** The link table: each setting's screen and section. */
export function remedyDestination(remedy: Remedy): RemedyDestination {
  switch (remedy.setting) {
    case "company_token_budget":
      return {
        path: BUDGETS_PATH,
        query: { [SECTION_PARAM]: COMPANY_SECTION },
        label: "Open company budget",
        target: "company",
      };
    case "assistant_token_budget":
      return {
        path: BUDGETS_PATH,
        query: { [SECTION_PARAM]: ASSISTANT_SECTION },
        label: "Open assistant budget",
        target: "company",
      };
    case "seat_token_budget": {
      const seat = remedy.seat ?? "";
      return {
        path: BUDGETS_PATH,
        query: { [SECTION_PARAM]: seatSection(seat) },
        label: "Open seat budget",
        target: { seat },
      };
    }
    case "model_providers":
      // THE COMPANY'S MODELS, which an admin alone edits: a model at its
      // usage limit is answered there by a fallback or another plan.
      return { path: MODELS_PATH, query: {}, label: "Open models", target: "company" };
  }
}

/** The remedy naming a scope's own ceilings, for a screen that knows which scope refused. */
export function scopeRemedy(scope: CeilingScope): Remedy {
  switch (scope.kind) {
    case "company":
      return { setting: "company_token_budget" };
    case "assistant":
      return { setting: "assistant_token_budget" };
    case "seat":
      return { setting: "seat_token_budget", seat: scope.handle };
  }
}

/** What a remedy offers the reader. */
export type RemedyOffer =
  /** The way to the section, for a reader who may change the ceiling there. */
  | { readonly kind: "open"; readonly destination: RemedyDestination }
  /** Who can change it, for a reader who may not. */
  | { readonly kind: "who"; readonly sentence: string }
  /** Nothing yet: who the reader is has not been answered. */
  | { readonly kind: "pending" };

/** What a reader who holds no Admin is told of a ceiling only an admin raises. */
export const ADMIN_RAISES = "An admin can raise this ceiling.";

/** What a reader who holds no Admin is told of the company's models. */
export const ADMIN_CHANGES_MODELS = "An admin can change the company's models.";

/**
 * What a remedy offers a reader the write gate answered `access` for: the
 * decision over values, which the component reads and a test pins.
 */
export function remedyOffer(remedy: Remedy, access: ConfigWriteAccess): RemedyOffer {
  const destination = remedyDestination(remedy);
  if (access.can) return { kind: "open", destination };
  switch (access.block) {
    case "loading":
      return { kind: "pending" };
    case "no_grant":
      return {
        kind: "who",
        sentence: remedy.setting === "model_providers" ? ADMIN_CHANGES_MODELS : ADMIN_RAISES,
      };
    case "anonymous":
    case "outside_scope":
    case "managed":
      // THE GATE'S OWN SENTENCE: who manages the document, the units a lead
      // leads, or signing in, each the reason the editor there is locked.
      return { kind: "who", sentence: access.reason };
    case "offline":
    case "refused":
    case "held":
      return { kind: "open", destination };
  }
}
