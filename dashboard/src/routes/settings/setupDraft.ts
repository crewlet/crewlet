/**
 * An integration's settings form as the assistant reads and changes it
 * (`useAssistantDraft`, from `SetupForm.tsx`).
 *
 * EVERY FIELD THE FORM SHOWS, by the key the form holds it under, and a
 * credential never: a `secret` requirement is sent with no value and is
 * never set, whatever the assistant is asked, so a key typed into the form
 * stays in the form. A choice or a toggle takes only one of its own values;
 * anything else is text the form validates on submit, as it does a typed
 * one. Submitting stays the person's.
 */

import { refuse, type DraftFieldSpec } from "~/app/assistant/draft.ts";
import type { SetupRequirement } from "~/protocol/index.ts";

/** What the assistant is told about an integration's settings form. */
export const SETUP_ASSISTANT_HELP =
  "These are the integration's settings as the form holds them, unsaved. You can propose " +
  "the text and choice fields; credentials are never shown to you and you cannot set them, " +
  "so the person types or references those. Saving or connecting stays the person's.";

/** One field the form shows: the key its value is held under, and its requirement. */
export interface SetupDraftEntry {
  key: string;
  r: SetupRequirement;
}

/**
 * A form key as a draft path: lowercase, dotted where the key separates a
 * surface from its field (`jira:url` reads `jira.url`), and in the page
 * context's grammar.
 */
export function setupPath(key: string): string {
  const path = key
    .toLowerCase()
    .replace(/[:/]/g, ".")
    .replace(/[^a-z0-9_.]/g, "_");
  return (/^[a-z]/.test(path) ? path : `f_${path}`).slice(0, 64);
}

/** The values a choice or a toggle takes, or null for a free field. */
function choicesOf(r: SetupRequirement): readonly string[] | null {
  if (r.kind === "toggle") return ["true", "false"];
  return r.choices?.map((c) => c.value) ?? null;
}

/** The form's fields as the assistant sees them, over the form's own value map. */
export function setupDraftFields(
  entries: readonly SetupDraftEntry[],
): DraftFieldSpec<Readonly<Record<string, string>>>[] {
  return entries.map(({ key, r }) => {
    const choices = choicesOf(r);
    const field: DraftFieldSpec<Readonly<Record<string, string>>> = {
      path: setupPath(key),
      label: r.label,
      read: (values) => values[key] ?? "",
      ...(choices ? { hint: `One of ${choices.join(", ")}` } : {}),
    };
    if (r.kind === "secret") {
      field.secret = true;
      return field;
    }
    field.write = (values, value) => {
      if (typeof value !== "string") return refuse(`${r.label} is one value.`);
      if (choices && !choices.includes(value)) {
        return refuse(`${r.label} is one of ${choices.join(", ")}.`);
      }
      return { ...values, [key]: value };
    };
    return field;
  });
}
