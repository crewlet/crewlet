/**
 * One setting of the company document, drawn and changed where it is read:
 * its name and key, the value in force, where it comes from, and at the
 * row's end the pencil that changes it and, while it differs from its
 * default, the reset that puts the default back.
 *
 * THE ONE EDITOR of such a value, whatever it is. A number (`LimitSetting`),
 * a line of text, a zone, a space or a switch (`TextSetting`) each says only
 * how its value is typed, drawn and planned; the pencil, the panel (the kit's
 * `SettingForm`), the reset, the check and the save
 * (`lib/useSettingWrite.ts`), what a refusal or a warning says, and what is
 * drawn while the saved value applies are this, once.
 *
 * A RESET IS A SAVE OF THE DEFAULT on the same path: checked, and where the
 * engine warns or refuses, the panel opens on the default to say so.
 *
 * WHAT IT SHOWS AFTER A SAVE is the value SAVED until this node has applied
 * it and the answer the page draws carries it; the save's toast says it is
 * applying. A node that refused the revision goes on running the value
 * before, marked so beside it.
 *
 * The pencil is always drawn, held with its reason for a reader who may not
 * change it ([SettingPencil]), also where the engine's row is not served ([UnreadSetting]).
 */

import {
  IconButton,
  Popover,
  returnFocus,
  SettingForm,
  SettingsTable,
  type IconButtonProps,
} from "@crewlethq/ui";
import { PencilGlyph } from "@crewlethq/icons/glyphs";
import { forwardRef, useEffect, useId, useRef, useState, type ReactNode } from "react";
import { REFUSED_HERE, SettingWriteStatus } from "~/components/SettingWriteStatus.tsx";
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
  /** The field's state that is the default: what a reset saves. */
  readonly defaultState: S;
  /** The default in the value column's words, for the reset's question. */
  readonly defaultWords: string;
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
  /** What the value may be, short, under the field: "1 to 100". */
  readonly hint?: string | undefined;
  /** The control, inside the field the panel labels. */
  readonly field: (props: {
    readonly id: string;
    readonly describedBy: string | undefined;
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
  // A reset's save, which opens the panel where the engine has more to say.
  const resetting = useRef(false);

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

  // A SAVE CLOSES THE FIELD: the value beside the pencil says the new one, and
  // the save's toast that it is applying.
  useEffect(() => {
    if (!editing || !saved) return;
    setEditing(false);
    returnFocus(pencil.current);
  }, [editing, saved]);

  // A RESET THE ENGINE WARNS OF OR REFUSES opens the panel on the default, so
  // the warning's Save anyway or the refusal is read where every save's is.
  const kind = write.state.kind;
  useEffect(() => {
    if (!resetting.current) return;
    if (kind === "warned" || kind === "refused" || kind === "unchanged") {
      resetting.current = false;
      setEditing(true);
    } else if (kind === "saved" || kind === "idle") {
      resetting.current = false;
    }
  }, [kind]);

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

  function submit() {
    if (write.busy) return;
    // ENTER ON A WARNING IS THE SECOND PRESS the warning asked for.
    if (write.state.kind === "warned") {
      write.confirm();
      return;
    }
    if (held !== undefined || !read || !read.ok) return;
    setExpected({ value: read.value });
    write.submit(edit.plan(read.value));
  }

  function resetToDefault() {
    if (write.busy) return;
    const back = edit.read(edit.defaultState);
    if (!back.ok) return;
    write.reset();
    setState(edit.defaultState);
    setExpected({ value: back.value });
    resetting.current = true;
    write.submit(edit.plan(back.value));
  }

  // THE SAVED VALUE while this node applies it; the served one otherwise.
  const drawsSaved =
    saved && (saved.standing === "applying" || saved.standing === "applied") && expected !== null;
  const shown = draw(drawsSaved ? expected : null);
  const mark = saved?.standing === "refused" ? REFUSED_HERE : "";
  return (
    <SettingsTable.Row
      label={label}
      settingKey={settingKey}
      source={shown.source}
      changed={shown.changed}
      defaultLabel={edit.defaultWords}
      onReset={settable && !write.busy && !drawsSaved ? resetToDefault : undefined}
      resetHeld={access.can ? undefined : access.reason}
      actions={
        settable && (
          // A PANEL ANCHORED TO THE PENCIL, not a field in the cell: the
          // engine's refusal of a value is a sentence, and the value column
          // is a quarter of the card wide.
          <Popover
            label={label}
            align="end"
            height="layer"
            open={editing}
            onOpenChange={(next) => (next ? open() : close())}
            trigger={(isOpen, toggle) => (
              <SettingPencil
                ref={pencil}
                label={label}
                held={access.can ? undefined : access.reason}
                aria-expanded={isOpen}
                onClick={toggle}
              />
            )}
          >
            <SettingForm
              label={label}
              htmlFor={fieldId}
              hint={edit.hint}
              error={error}
              status={
                <SettingWriteStatus
                  write={write}
                  unchanged={edit.unchangedWords}
                  onReload={() => {
                    onApplied();
                    close();
                  }}
                />
              }
              saving={write.busy}
              saveHeld={held}
              saveLabel={write.state.kind === "warned" ? "Save anyway" : undefined}
              onSave={submit}
              onCancel={close}
            >
              {(field) =>
                state !== null &&
                edit.field({
                  id: fieldId,
                  describedBy: field.describedBy,
                  state,
                  set: (next) => {
                    setState(next);
                    if (write.state.kind !== "idle" && !write.busy) write.reset();
                  },
                  error: Boolean(error),
                  busy: write.busy,
                })
              }
            </SettingForm>
          </Popover>
        )
      }
    >
      <span className="setting-value">
        <span>{shown.value}</span>
        {mark && (
          <span className="setting-value-refused t-caption" role="status">
            {mark}
          </span>
        )}
      </span>
    </SettingsTable.Row>
  );
}

/**
 * The pencil that opens a setting's editor, held with the reason where this
 * reader may not change it. The kit tooltip, not a native title, so hover,
 * keyboard focus and a press all show the reason.
 */
export const SettingPencil = forwardRef<
  HTMLButtonElement,
  Omit<IconButtonProps, "label" | "icon" | "tooltip" | "disabledReason"> & {
    /** What the setting is called; the pencil is "Change" it. */
    label: string;
    /** Why this reader may not change it, or undefined where they may. */
    held: string | undefined;
  }
  // The rest is the button's: a popover hands its trigger aria state.
>(function SettingPencil({ label, held, ...rest }, ref) {
  return (
    <IconButton
      {...rest}
      ref={ref}
      size="sm"
      variant="ghost"
      icon={<PencilGlyph />}
      label={`Change ${label}`}
      tooltip
      disabledReason={held}
    />
  );
});

/**
 * A setting whose engine row this reader is not served: its value where the
 * page has it, and the pencil held with the reason (a write control is never
 * hidden). A reader who may change it sees no pencil until the row arrives.
 */
export function UnreadSetting({
  label,
  settingKey,
  children,
}: {
  label: string;
  settingKey: string;
  /** The value, where the page has it; what it says where it does not. */
  children: ReactNode;
}) {
  const access = useConfigWriteAccess();
  return (
    <SettingsTable.Row
      label={label}
      settingKey={settingKey}
      actions={!access.can && <SettingPencil label={label} held={access.reason} />}
    >
      {children}
    </SettingsTable.Row>
  );
}
