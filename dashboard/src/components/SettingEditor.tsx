/**
 * One setting of the company document, drawn and changed where it is read:
 * its name and key, the value in force with the pencil that changes it, and
 * where the value comes from.
 *
 * THE ONE EDITOR of such a value, whatever it is. A number (`LimitSetting`),
 * a line of text, a zone, a space or a switch (`TextSetting`) each says only
 * how its value is typed, drawn and planned; the pencil, the panel, the check
 * and the save (`lib/useSettingWrite.ts`), what a refusal or a warning says,
 * and what is drawn while the saved value applies are this, once.
 *
 * WHAT IT SHOWS AFTER A SAVE is the value SAVED, marked as applying, until
 * this node has applied it and the answer the page draws carries it: going
 * back to the served value at once drew the setting being replaced for a beat
 * after the save, which reads as a save that did not take. A node that
 * refused the revision goes on running the value before, so that is what is
 * drawn, marked as refused here.
 */

import { Button, FormField, IconButton, Popover, returnFocus, SettingsTable } from "@crewlethq/ui";
import { PencilGlyph } from "@crewlethq/icons/glyphs";
import { useEffect, useId, useRef, useState, type FormEvent, type ReactNode } from "react";
import { SettingWriteStatus, standingWords } from "~/components/SettingWriteStatus.tsx";
import type { ConfigProblem } from "~/protocol/types.ts";
import { useSettingWrite, type SettingPlan } from "~/lib/useSettingWrite.ts";
import { useConfigWriteAccess } from "~/lib/useWriteAccess.ts";

/** What a field holds, read: a value to save, or why it is not one. */
export type Typed<V> =
  { readonly ok: true; readonly value: V } | { readonly ok: false; readonly error: string };

/** What the row draws for a value: the value, and where it comes from. */
export interface Drawn {
  readonly value: ReactNode;
  readonly source: string;
  readonly changed: boolean;
}

/** How one kind of setting is typed, drawn and saved. */
export interface SettingEdit<S, V> {
  /** The field's state as the pencil opens it, from the value in force. */
  readonly open: () => S;
  /** What the state holds: a value to save, or the error said under the field. */
  readonly read: (state: S) => Typed<V>;
  /** Whether a value is the one in force already, so Save is held. */
  readonly same: (value: V) => boolean;
  /** Why Save is held on a value in force already: "This is the limit already." */
  readonly sameWords: string;
  /** What "nothing to save" reads as once the engine says so: "Already the limit." */
  readonly unchangedWords: string;
  /** The change a value makes, worked out against the document at the save. */
  readonly plan: (value: V) => SettingPlan;
  /** Whether the answer the page draws already says the value saved. */
  readonly served: (value: V) => boolean;
  /** The line under the field. */
  readonly helper: ReactNode;
  /** The control, inside the field the editor labels. */
  readonly field: (props: {
    readonly id: string;
    readonly state: S;
    readonly set: (next: S) => void;
    readonly error: boolean;
    readonly busy: boolean;
  }) => ReactNode;
}

export function SettingEditor<S, V>({
  label,
  settingKey,
  answer,
  settable,
  draw,
  edit,
  reread,
  onApplied,
  problemWords,
}: {
  /** What the setting is called on its page; its key is drawn beside it. */
  label: string;
  settingKey: string;
  /**
   * The answer the row is drawn from: a new object once the page reads it
   * again, which is how the editor knows the read after an apply arrived.
   */
  answer: object;
  /** Whether the document carries the key, so the pencil is offered. */
  settable: boolean;
  /** The row's value and source: the served one, or the saved one while it applies. */
  draw: (saved: { readonly value: V } | null) => Drawn;
  edit: SettingEdit<S, V>;
  /** How a conflict ends, in the page's words: "Reload to see the limits as they are now." */
  reread: string;
  /** Called once this node applies the saved revision, to read the page's answer again. */
  onApplied: () => void;
  /** The engine's refusal in the page's words; each problem's own person's words otherwise. */
  problemWords?: (problems: readonly ConfigProblem[]) => string;
}) {
  const access = useConfigWriteAccess();
  const write = useSettingWrite({
    reread,
    onApplied,
    ...(problemWords ? { problemWords } : {}),
  });
  const [editing, setEditing] = useState(false);
  const [state, setState] = useState<S | null>(null);
  // WHAT IS TYPED, READ AS IT IS TYPED: a value the setting cannot take is
  // said under the field before Save, not sent for the engine to refuse.
  const read = state === null ? null : edit.read(state);
  const error = read && !read.ok ? read.error : undefined;
  // SAVE IS HELD while there is nothing it could save: a value the setting
  // cannot take, or the one in force already.
  const held =
    write.state.kind === "warned"
      ? undefined
      : !read
        ? undefined
        : !read.ok
          ? read.error
          : edit.same(read.value)
            ? edit.sameWords
            : undefined;
  // The value the last save wrote, drawn while it applies.
  const [expected, setExpected] = useState<{ readonly value: V } | null>(null);
  const pencil = useRef<HTMLButtonElement>(null);
  const fieldId = `${useId()}-setting`;

  const saved = write.state.kind === "saved" ? write.state : null;
  const standing = saved?.standing;
  const { reset } = write;
  const { served } = edit;
  // SETTLED once the answer re-read after the apply has arrived (the answer
  // is a new object, or already says the saved value), and at once where a
  // later activation passed the save: the served value is the truth again.
  const seenAtApply = useRef<object | null>(null);
  useEffect(() => {
    if (standing === "passed") {
      seenAtApply.current = null;
      reset();
      setExpected(null);
      return;
    }
    if (standing !== "applied") {
      seenAtApply.current = null;
      return;
    }
    if (seenAtApply.current === null) seenAtApply.current = answer;
    if ((expected && served(expected.value)) || answer !== seenAtApply.current) {
      seenAtApply.current = null;
      reset();
      setExpected(null);
    }
  }, [standing, answer, expected, reset, served]);

  // A SAVE CLOSES THE FIELD: the value beside the pencil says the new one and
  // that it is applying, which is the answer the person was waiting for.
  useEffect(() => {
    if (!editing || !saved) return;
    setEditing(false);
    returnFocus(pencil.current);
  }, [editing, saved]);

  function open() {
    write.reset();
    setState(edit.open());
    setEditing(true);
  }

  function close() {
    write.reset();
    setEditing(false);
    // FOCUS GOES BACK TO WHAT OPENED IT, handed back rather than focused, so
    // a pencil a pointer opened is not ringed for leaving the field.
    queueMicrotask(() => returnFocus(pencil.current));
  }

  function submit(event?: FormEvent) {
    event?.preventDefault();
    if (write.busy) return;
    // ENTER ON A WARNING IS THE SECOND PRESS the warning asked for.
    if (write.state.kind === "warned") {
      write.confirm();
      return;
    }
    // ENTER SUBMITS THE FORM WITHOUT THE BUTTON, so the hold is checked here
    // too.
    if (held !== undefined || !read || !read.ok) return;
    setExpected({ value: read.value });
    write.submit(edit.plan(read.value));
  }

  // THE SAVED VALUE while this node applies it; the served one otherwise.
  const drawsSaved =
    saved && (saved.standing === "applying" || saved.standing === "applied") && expected !== null;
  const shown = draw(drawsSaved ? expected : null);
  const mark = saved ? standingWords(saved.standing) : "";
  return (
    <SettingsTable.Row
      label={label}
      settingKey={settingKey}
      source={shown.source}
      changed={shown.changed}
    >
      <span className="ceiling">
        <span>{shown.value}</span>
        {mark && (
          <span className="ceiling-applying t-caption" role="status">
            {mark}
          </span>
        )}
        {settable && (
          // A PANEL ANCHORED TO THE PENCIL, not a field in the cell: the
          // engine's refusal of a value is a sentence, and the value column
          // is a quarter of the card wide.
          <Popover
            label={label}
            align="start"
            className="ceiling-pop"
            open={editing}
            onOpenChange={(next) => (next ? open() : close())}
            trigger={(isOpen, toggle) => (
              <IconButton
                ref={pencil}
                size="sm"
                variant="ghost"
                icon={<PencilGlyph />}
                label={`Change ${label}`}
                aria-expanded={isOpen}
                disabledReason={access.can ? undefined : access.reason}
                title={access.can ? undefined : access.reason}
                onClick={toggle}
              />
            )}
          >
            <form className="ceiling-edit" onSubmit={submit} noValidate>
              <FormField
                htmlFor={fieldId}
                label={label}
                optional
                error={error}
                helper={edit.helper}
              >
                {state !== null &&
                  edit.field({
                    id: fieldId,
                    state,
                    set: (next) => {
                      setState(next);
                      if (write.state.kind !== "idle" && !write.busy) write.reset();
                    },
                    error: Boolean(error),
                    busy: write.busy,
                  })}
              </FormField>
              <SettingWriteStatus
                write={write}
                unchanged={edit.unchangedWords}
                onReload={() => {
                  onApplied();
                  close();
                }}
              />
              <div className="ceiling-actions">
                <Button type="button" size="small" variant="ghost" onClick={close}>
                  Cancel
                </Button>
                <Button
                  type={write.state.kind === "warned" ? "button" : "submit"}
                  size="small"
                  variant="primary"
                  loading={write.busy}
                  disabledReason={held}
                  title={held}
                  onClick={write.state.kind === "warned" ? write.confirm : undefined}
                >
                  {write.state.kind === "warned" ? "Save anyway" : "Save"}
                </Button>
              </div>
            </form>
          </Popover>
        )}
      </span>
    </SettingsTable.Row>
  );
}
