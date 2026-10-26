/**
 * The Principles editor's draft as the assistant reads and changes it
 * (`useAssistantDraft`): each part's value, and a new value for a part the
 * person has OPEN.
 *
 * INTO THE DRAFT, NEVER THE COMPANY. A value replaces the open part's draft
 * through the editor's own `change`, exactly as typing does: the rules the
 * engine admits a charter change by are shown beside the field as they are
 * for anything typed, Save is held while they are broken, and saving stays
 * the person's (with its If-Match and its step-up). A part that is not open
 * is read and never set, since opening one is the person's gesture, and so
 * is one being saved or holding a conflict.
 */

import { refuse, type AssistantDraftSpec, type DraftFieldSpec } from "~/app/assistant/draft.ts";
import {
  CHARTER_PARTS,
  PART_TITLES,
  partName,
  partOf,
  type Charter,
  type CharterPart,
  type PartValue,
} from "./charter.ts";
import { reducer, type EditingState } from "./state.ts";

/** The most text one part carries: well past every part's own limit, which the editor states. */
const MAX_TEXT = 16 * 1024;
/** The most policies one draft lists. */
const MAX_POLICIES = 100;

const capital = (words: string) => words.charAt(0).toUpperCase() + words.slice(1);

/** A part's value as one line of a card. */
export function draftWords(value: PartValue): string {
  return typeof value === "string" ? value : value.map((p, i) => `${i + 1}. ${p}`).join(" ");
}

/** What the assistant is told about this editor, beside its fields. */
const HELP =
  "Each part is saved on its own by the person. A part can be changed only while it is " +
  "open for editing; ask the person to open a part with Edit first. Policies are a list, " +
  "one short sentence per item.";

/** The fields of the charter as the editor holds it: the open draft where a part is open, the saved part otherwise. */
export function charterFields(
  state: EditingState,
  charter: Charter,
): DraftFieldSpec<EditingState>[] {
  return CHARTER_PARTS.map((part) => {
    const session = state.sessions[part];
    const field: DraftFieldSpec<EditingState> = {
      path: part,
      label: PART_TITLES[part],
      read: (s) => s.sessions[part]?.draft ?? partOf(charter, part),
      ...(part === "policies" ? { hint: "One short sentence per policy" } : {}),
      summary: (value) => draftWords(value),
    };
    // ONLY AN OPEN PART IS SET, through the reducer's own `change`.
    if (session?.phase.kind === "editing") field.write = (s, value) => writePart(s, part, value);
    return field;
  });
}

/** The editor's state with a part's draft replaced, or why it is not. */
function writePart(state: EditingState, part: CharterPart, value: PartValue) {
  const session = state.sessions[part];
  if (!session) return refuse(`Open ${partName(part)} to write it first.`);
  if (session.phase.kind !== "editing") {
    return refuse(`${capital(partName(part))} is not being edited right now.`);
  }
  if (typeof value === "string") {
    if (part === "policies")
      return refuse(`${PART_TITLES.policies} are a list of short sentences.`);
    if (value.length > MAX_TEXT) return refuse(`${capital(partName(part))} is a statement.`);
  } else if (
    part !== "policies" ||
    value.length > MAX_POLICIES ||
    value.join("").length > MAX_TEXT
  ) {
    return part === "policies"
      ? refuse(`${PART_TITLES.policies} are a list of short sentences.`)
      : refuse(`${capital(partName(part))} is a statement.`);
  }
  return reducer(state, { type: "change", part, draft: value });
}

/** The editor's draft as the page declares it, around the setters the page already has. */
export function charterDraft(args: {
  state: EditingState;
  charter: Charter;
  change: (part: CharterPart, value: PartValue) => void;
  blocked: () => string | null;
}): AssistantDraftSpec<EditingState> {
  const { state, charter, change, blocked } = args;
  const open = CHARTER_PARTS.find((part) => state.sessions[part] !== undefined);
  return {
    kind: "client",
    step: open ?? null,
    scope: "The parts of the principles you are writing (not saved)",
    help: HELP,
    value: state,
    fields: charterFields(state, charter),
    apply: (next) => {
      for (const part of CHARTER_PARTS) {
        const draft = next.sessions[part]?.draft;
        if (draft !== undefined && draft !== state.sessions[part]?.draft) change(part, draft);
      }
    },
    blocked,
  };
}
