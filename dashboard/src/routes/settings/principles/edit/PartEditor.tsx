/**
 * One part of the charter, open in its card: the kit's `InlineEditor` around
 * the part's field, with what is wrong with the draft, the question a
 * colleague's change raises, and why a save did not land.
 *
 * THE EDITOR'S OWN DECISIONS ARE THE KIT'S: the keys (the command key with
 * Enter saves from any field, Escape cancels; `charter.save` and
 * `charter.cancel` in `app/keymap.ts`), a Save that says why it does nothing
 * rather than going dead, the status at the start of the bar and the focus
 * in the field as the part opens. This file decides what the part's field is
 * and what each of those slots says.
 *
 * # One line, or paragraphs
 *
 * The mission and each policy are ONE LINE, as the engine requires (each is
 * inlined in every seat's prompt, a policy as one item of a list), and one
 * line WRAPS here so a long one can be read whole while it is written: the
 * mission is the kit's `singleLine` textarea, and each policy a one-line
 * ListInput row, which is the same field. Neither takes a line break. The
 * vision is paragraphs and keeps its line breaks.
 *
 * # Problems are said where they are
 *
 * A statement or a policy that runs long is said by its own count, beside it;
 * a repeated policy under that policy; a list too long under the list. Save
 * with a problem standing sends nothing and puts the focus on the first one.
 * Only what the person CHANGED is held to the rules, as the engine holds it
 * (`charter.ts`, [problemsOf]); a fault the stored charter already had is a
 * warning above the bar instead.
 */

import { Callout, FormField, InlineEditor, Textarea, VisuallyHidden } from "@crewlethq/ui";
import {
  PART_HELP,
  PART_TITLES,
  lengthOf,
  limitOf,
  partName,
  problemsOf,
  warningsOf,
  type CharterPart,
  type PartProblems,
} from "./charter.ts";
import { ConflictPanel } from "./ConflictPanel.tsx";
import { PolicyListField } from "./PolicyListField.tsx";
import { effective, isDirty, type Session } from "./state.ts";
import type { Editing } from "./useCharterEditing.tsx";

export function PartEditor({ part, editing: ctx }: { part: CharterPart; editing: Editing }) {
  const session = ctx.state.sessions[part];
  if (!session) return null;
  const saving = session.phase.kind === "saving";
  const problems = visibleProblems(part, session);
  const conflict = session.phase.kind === "conflict" ? session.phase : null;
  const warnings = warningsOf(part, effective(session), session.base);
  return (
    // THE WRAPPER IS WHERE THE PAGE PUTS THE FOCUS after a step the editor
    // does not take itself: back into the field, or onto the first problem.
    <div ref={ctx.editorRef(part)}>
      <InlineEditor
        label={`Edit ${partName(part)}`}
        // A CONFLICT IS ANSWERED BEFORE ANYTHING IS SAVED: Save is held while
        // the question stands, and says how to answer it.
        dirty={isDirty(session) && conflict === null}
        saving={saving}
        invalid={hasShownProblems(problems) || !ctx.access.can}
        focusOnMount={session.focusOnOpen}
        onSave={() => ctx.save(part)}
        onCancel={() => ctx.cancel(part)}
        notice={
          conflict ? (
            <ConflictPanel
              part={part}
              base={session.base}
              theirs={conflict.theirs}
              onKeepMine={() => ctx.keepMine(part)}
              onTakeTheirs={() => ctx.takeTheirs(part)}
            />
          ) : warnings.length > 0 ? (
            // A FAULT THE SAVE DOES NOT TOUCH, written before the rules: said
            // so it can be fixed while the person is here, never holding Save,
            // since the engine admits a write that leaves it as it was.
            <Callout variant="warning" title="Saved before the current rules">
              {warnings.join(" ")}
            </Callout>
          ) : undefined
        }
        error={session.phase.kind === "failed" ? session.phase.message : undefined}
        labels={{
          unchanged: conflict ? "Keep your draft or use theirs first." : "Change something first.",
          // A SAVE THIS BROWSER CANNOT MAKE NOW (offline, still being
          // identified) is held with the reason, where Save says why it waits.
          ...(ctx.access.can ? {} : { invalid: ctx.access.reason }),
        }}
      >
        {part === "policies" ? (
          <PolicyListField
            value={typeof session.draft === "string" ? [] : session.draft}
            draft={session.pending}
            problems={problems}
            disabled={saving}
            onChange={(items) => ctx.change(part, items)}
            onDraftChange={(text) => ctx.pending(part, text)}
          />
        ) : (
          <StatementField
            part={part}
            value={typeof session.draft === "string" ? session.draft : ""}
            error={problems.field}
            disabled={saving}
            onChange={(text) => ctx.change(part, text)}
          />
        )}
      </InlineEditor>
    </div>
  );
}

/** A statement: the mission on one wrapping line, or the vision in paragraphs. */
function StatementField({
  part,
  value,
  error,
  disabled,
  onChange,
}: {
  part: Exclude<CharterPart, "policies">;
  value: string;
  error: string | undefined;
  disabled: boolean;
  onChange: (text: string) => void;
}) {
  return (
    <FormField
      // THE CARD'S TITLE IS THE VISIBLE LABEL, right above the field; the
      // field still carries its own name for a reader that lists fields.
      label={<VisuallyHidden>{PART_TITLES[part]}</VisuallyHidden>}
      helper={PART_HELP[part]}
      error={error}
      limit={{ length: lengthOf(value), max: limitOf(part) }}
    >
      {(field) => (
        <Textarea
          id={field.id}
          aria-describedby={field.describedBy}
          aria-invalid={field.invalid || undefined}
          error={field.invalid}
          autoResize
          // THE MISSION IS ONE LINE THAT WRAPS: read whole while it is
          // written, with no line break typed, pasted or dropped in (the
          // kit folds one into a space). The vision is paragraphs.
          singleLine={part === "mission"}
          rows={part === "mission" ? 1 : 3}
          value={value}
          readOnly={disabled}
          onChange={(e) => onChange(e.target.value)}
        />
      )}
    </FormField>
  );
}

/**
 * The problems a draft shows now: every rule it breaks but a length, which
 * the field's own count says, beside what the engine refused the last save
 * over. A problem with the policy still in the new-item box is said under the
 * list, since that box is no row of its own.
 */
export function visibleProblems(part: CharterPart, session: Session): PartProblems {
  const own = problemsOf(part, effective(session), { lengths: false, was: session.base });
  const rows = typeof session.draft === "string" ? 0 : session.draft.length;
  const items = own.items?.slice(0, rows);
  const typed = own.items?.[rows];
  const refused = session.refused;
  const merged = mergeItems(items, refused?.items);
  const field =
    [own.field, typed && `The new policy: ${typed}`, refused?.field].filter(Boolean).join(" ") ||
    undefined;
  return {
    ...(field ? { field } : {}),
    ...(merged ? { items: merged } : {}),
  };
}

/** Whether anything is shown as wrong: Save waits for it. */
function hasShownProblems(problems: PartProblems): boolean {
  return problems.field !== undefined || (problems.items?.some(Boolean) ?? false);
}

function mergeItems(
  a: readonly (string | undefined)[] | undefined,
  b: readonly (string | undefined)[] | undefined,
): (string | undefined)[] | undefined {
  const length = Math.max(a?.length ?? 0, b?.length ?? 0);
  if (length === 0) return undefined;
  const out = Array.from(
    { length },
    (_, i) => [a?.[i], b?.[i]].filter(Boolean).join(" ") || undefined,
  );
  return out.some(Boolean) ? out : undefined;
}
