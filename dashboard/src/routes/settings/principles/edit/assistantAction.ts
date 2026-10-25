/**
 * What the Principles editor takes from the assistant: a new draft of a part
 * the person has OPEN (`principles.draft.set`).
 *
 * INTO THE DRAFT, NEVER THE COMPANY. The value replaces the open part's
 * draft through the editor's own `change`, exactly as typing does: the rules
 * the engine admits a charter change by are shown beside the field as they
 * are for anything typed, Save is held while they are broken, and saving
 * stays the person's (with its If-Match and its step-up). A part that is not
 * open is refused rather than opened, since opening one is the person's
 * gesture, and so is one being saved or holding a conflict.
 */

import type { PageActionParse } from "~/app/assistant/context.ts";
import {
  CHARTER_PARTS,
  PART_TITLES,
  partName,
  type CharterPart,
  type PartValue,
} from "./charter.ts";
import type { EditingState } from "./state.ts";

/** A part's new draft. */
export interface CharterDraft {
  readonly part: CharterPart;
  readonly value: PartValue;
}

/** The most text one draft carries: well past every part's own limit, which the editor states. */
const MAX_TEXT = 16 * 1024;
/** The most policies one draft lists. */
const MAX_POLICIES = 100;

const capital = (words: string) => words.charAt(0).toUpperCase() + words.slice(1);

const plain = (value: unknown): value is string =>
  typeof value === "string" && !/[\u0000-\u0008\u000e-\u001f\u007f]/.test(value);

/** Parse a part's new draft, or refuse it whole. */
export function parseCharterDraft(
  raw: unknown,
  state: EditingState,
): PageActionParse<CharterDraft> {
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) {
    return { ok: false, reason: "That is not a draft of a part of the principles." };
  }
  const { part, value, ...rest } = raw as Record<string, unknown>;
  if (Object.keys(rest).length > 0) return { ok: false, reason: "A draft is a part and its text." };
  if (!(CHARTER_PARTS as readonly unknown[]).includes(part)) {
    return { ok: false, reason: "The principles have a mission, a vision and standing policies." };
  }
  const which = part as CharterPart;
  const session = state.sessions[which];
  if (!session) return { ok: false, reason: `Open ${partName(which)} to write it first.` };
  if (session.phase.kind !== "editing") {
    return { ok: false, reason: `${capital(partName(which))} is not being edited right now.` };
  }
  if (which === "policies") {
    if (
      !Array.isArray(value) ||
      value.length > MAX_POLICIES ||
      !value.every(plain) ||
      value.join("").length > MAX_TEXT
    ) {
      return { ok: false, reason: `${PART_TITLES.policies} are a list of short sentences.` };
    }
    return { ok: true, value: { part: which, value } };
  }
  if (!plain(value) || value.length > MAX_TEXT) {
    return { ok: false, reason: `${capital(partName(which))} is a statement.` };
  }
  return { ok: true, value: { part: which, value } };
}

/** A part's value as one line of a card. */
export function draftWords(value: PartValue): string {
  return typeof value === "string" ? value : value.map((p, i) => `${i + 1}. ${p}`).join(" ");
}
