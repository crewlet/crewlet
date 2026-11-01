/**
 * One field of the company's configuration: a logic-only adapter over uilet's
 * ValueField, drawing nothing of its own.
 *
 * The design system draws the field: uilet's [ValueField] holds the control
 * each kind of value needs and every rule about a value on its way through
 * (a secret masked unless it is a whole reference, `$` completing a sealed
 * entry's name, a url's scheme affix, whitespace dropped from a single token,
 * a choice's hint on the help line). What stays here is only what the ENGINE
 * means, and has no peer in a design system:
 *
 * - WHAT A REFERENCE IS. The engine's grammar (`lib/references.ts`, held to
 *   `internal/envref`) decides which values read as a sealed entry's name, so
 *   a screen never asks a private copy of it.
 * - WHAT A REFUSAL LOOKS LIKE. The engine refuses with several problems
 *   joined by newlines, each opening with a config path; [withProblems] draws
 *   them as the list they are, with each path picked out.
 * - HOW DENSE THE FORM IS. A dense form (the org builder's node editor)
 *   provides [HelperOnFocus], so it reads as labels and values at rest.
 */

import { createContext, useContext, type ReactNode } from "react";
import { ValueField, type ValueFieldChoice, type ValueFieldKind } from "@crewlethq/ui";

import { isWholeReference } from "~/lib/references.ts";
import { withProblems } from "~/ui/Problems.tsx";

/**
 * Whether a field's help line is drawn only while it has focus. A dense form
 * provides `true`; every other form draws its help as it always has.
 */
export const HelperOnFocus = createContext(false);

export type FieldKind = ValueFieldKind;

/** One option of a choice field. */
export type FieldChoice = ValueFieldChoice;

export function ConfigField({
  error,
  secrets,
  onSecretsNeeded,
  ...field
}: {
  label: string;
  /** A mark drawn before the label, such as a third-party app's own mark. */
  mark?: ReactNode;
  kind?: FieldKind;
  value: string;
  onChange: (v: string) => void;
  help?: ReactNode;
  /** The engine's refusal of the last value, drawn as its problems. */
  error?: string;
  placeholder?: string;
  /** Required for `choice` and `handle`; ignored otherwise. */
  choices?: FieldChoice[];
  disabled?: boolean;
  required?: boolean;
  /** Say "(required)" beside the label, where requiredness is news. */
  markRequired?: boolean;
  autoFocus?: boolean;
  /**
   * The sealed entries this company holds, offered when somebody types `$`.
   * NAMES ONLY: a value never reaches this screen.
   */
  secrets?: string[];
  /**
   * Called as the list is about to be shown, so the caller can make sure it
   * is current: there is no push for the secret store, and a secret created
   * in another tab since the form opened has to be offerable.
   */
  onSecretsNeeded?: () => void;
  /** How many lines a `multiline` field shows before it scrolls. */
  rows?: number;
  /** A class for a form's own composition of the field, such as its label's case. */
  className?: string;
}) {
  const helperOnFocus = useContext(HelperOnFocus);
  return (
    <ValueField
      {...field}
      error={withProblems(error)}
      references={secrets}
      onReferencesNeeded={onSecretsNeeded}
      isReference={isWholeReference}
      helperOnFocus={helperOnFocus}
    />
  );
}
