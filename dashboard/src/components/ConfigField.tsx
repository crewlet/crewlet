/**
 * One field of the company's configuration.
 *
 * The design system supplies the row (a label bound to its control, a line
 * saying where the value comes from, a line saying why the engine refused the
 * last one) and every control it draws. What is here is what the CONFIG means,
 * and only that: which kinds of value this document holds, which of them may
 * be a pointer into the sealed store, and what a field is allowed to do to a
 * value on its way through.
 *
 * A SECRET FIELD IS A PASSWORD FIELD, and that is not decoration: `type` is
 * what keeps the value out of an autofill store, out of a screenshot, and out
 * of the accessibility tree as text. The field also refuses to carry a
 * `defaultValue` for a secret, because a value the engine has is a value this
 * page must never have received.
 *
 * A REFERENCE IS NOT A CREDENTIAL, so it is not masked. A secret field whose
 * value is wholly a `${NAME}` holds the NAME of a sealed entry, and masking
 * it defeats the point of sending it: an operator has to be able to read
 * which entry this field points at. Typing over it masks again the moment the
 * value stops being a whole reference, so a credential is never on screen.
 *
 * TYPING `$` OFFERS THE COMPANY'S SEALED ENTRIES. A field takes either a
 * value or a `${NAME}` pointing at one, and the name has to be exact: one
 * character out resolves to nothing, which reads as configured on every
 * surface while the route it feeds refuses every delivery. Nothing on the
 * form knew the names, so getting one right meant opening the Secrets screen
 * in another tab and copying it across.
 *
 * A URL FIELD WEARS ITS SCHEME. Every config field of this kind is refused
 * without one, so a person typing their site the way they say it out loud
 * ("acme.atlassian.net") had a form that took the value, a Save that failed
 * validation, and nothing on screen to say which of the two was wrong. The
 * affix makes the requirement visible and satisfies it. See [schemeOf] for
 * what happens to a value that brings its own.
 *
 * A MULTILINE FIELD IS PROSE. A goal, a backstory or a mission is several
 * sentences a person writes and rereads, so `kind="multiline"` is a textarea
 * that keeps every space and newline it is given, grows by rows rather than
 * scrolling a single line sideways, and leaves the browser's spell check on:
 * the one field kind where a misspelling is a defect in what ships to a
 * model's prompt rather than an identifier the check would only mark wrong.
 * It offers no `${NAME}` completion, because a reference is a whole value and
 * prose is never one.
 *
 * A CHOICE IS THE DESIGN SYSTEM'S LISTBOX, never the platform's own dropdown.
 * A native list is drawn by the operating system: it takes none of the theme,
 * none of the density and none of the tokens, so a dark dialog opened a light
 * grey menu in the middle of itself. The listbox is also the only one that can
 * carry a second line under an option, which is where a choice's hint belongs.
 */

import { useId, useRef, useState, type ReactNode } from "react";

import { withProblems } from "~/ui/Problems.tsx";
import { complete, rank, referenceAt, type Typing } from "~/ui/secretref.ts";
import { Combobox, FormField, InputAffix, Select, Textarea } from "@crewlethq/ui";

export type FieldKind =
  "text" | "multiline" | "secret" | "url" | "id" | "choice" | "handle" | "email";

/** One option of a choice field. */
export interface FieldChoice {
  value: string;
  label: string;
  hint?: string;
}

export function ConfigField({
  label,
  kind = "text",
  value,
  onChange,
  help,
  error,
  placeholder,
  choices,
  disabled,
  required,
  markRequired,
  autoFocus,
  secrets,
  onSecretsNeeded,
  rows = 3,
}: {
  label: string;
  kind?: FieldKind;
  value: string;
  onChange: (v: string) => void;
  help?: ReactNode;
  error?: string;
  placeholder?: string;
  /** Required for `choice` and `handle`; ignored otherwise. */
  choices?: FieldChoice[];
  disabled?: boolean;
  required?: boolean;
  /**
   * Say "(required)" beside the label, for a field whose requiredness is
   * NEWS: one that appeared because of an answer above it. Required is the
   * unmarked default here; marking every required field is noise that
   * teaches a reader to skip the note.
   */
  markRequired?: boolean;
  autoFocus?: boolean;
  /**
   * The sealed entries this company holds, offered when somebody types `$`.
   *
   * NAMES ONLY, and that is the whole contract: a value never reaches this
   * screen, so a list that could not be assembled leaves the field exactly
   * as it was rather than degrading it.
   */
  secrets?: string[];
  /**
   * Called when the list is about to be shown, so the caller can make sure
   * it is current.
   *
   * A SECRET CREATED SINCE THIS FORM OPENED HAS TO BE OFFERABLE. There is no
   * push for the secret store, so a list read once at mount is a list that
   * goes stale the moment somebody adds an entry in another tab, which is
   * exactly what a person does when they find the name they wanted is not
   * there. Asking at the moment the answer matters is one request per time
   * somebody starts typing a reference, and the caller decides how often
   * that is worth spending.
   */
  onSecretsNeeded?: () => void;
  /** How many lines a `multiline` field shows before it scrolls; ignored otherwise. */
  rows?: number;
}) {
  const id = useId();
  const affixID = `${id}-affix`;
  const picker = kind === "choice" || kind === "handle";

  // THE AFFIX IS A DEFAULT, NOT A CAGE. It stands in for the scheme this
  // field would otherwise be refused without, and it steps aside for a value
  // that carries its own: a Data Center instance reachable only over http is
  // a thing the config accepts, and an affix that silently rewrote it to
  // https would point the engine at a port nothing answers on.
  // MASKED UNLESS IT IS A REFERENCE. See the note above.
  const reference = isReference(value);
  const masked = kind === "secret" && !reference;
  // A SINGLE TOKEN TAKES NO WHITESPACE, ever, including from a paste. An
  // address, an identifier and an email each name one thing, and a space
  // around one is invisible in the box and fatal at the vendor: it
  // authenticates as nobody, or resolves to no host. The engine refuses one
  // too; stripping it here is what stops a form being refused for a
  // character nobody can see.
  const tight = kind === "url" || kind === "id" || kind === "email";
  const own = kind === "url" ? schemeOf(value) : "";
  // AND IT STEPS ASIDE FOR A REFERENCE, for the same reason it steps aside for
  // http. `${VAR}` is a whole value this field accepts (the engine's own
  // `hasHTTPScheme` admits `envref.Has`), so the scheme the affix stands in
  // for is one the reference carries itself once it resolves. Prepending
  // produced `https://${VAR}`: a value no resolver reads, unenterable for
  // anyone setting one, and silently written over the first keystroke for
  // anyone who already had one.
  //
  // ANY LEADING `$`, not a whole reference. A person types one character at a
  // time and `${CREWLET_PUB` is not yet whole, so a rule that waited for a
  // complete reference would prepend on the very first keystroke and never
  // let go again.
  //
  // ONLY WHERE THE VALUE CARRIES NO SCHEME OF ITS OWN. Behind one, a `$` is
  // just the rest of the address (`https://${JIRA_HOST}` is an ordinary
  // value), and there the affix is CARRYING a scheme rather than offering
  // one, so it must stay.
  const refBase = own === "" && value.trimStart().startsWith("$");
  const affix = kind === "url" && own !== "http://" && !refBase ? "https://" : "";
  const shown = affix ? value.slice(own.length) : value;

  // --- completing a ${NAME} ------------------------------------------- //
  //
  // WIRED TO THE PLAIN FIELD ONLY. A url field wears an affix, so the box
  // holds a different string from the value and every caret offset would
  // have to be translated through it; every field a reference actually goes
  // in (a secret, an id, an email, a token) is a plain one.
  const box = useRef<HTMLInputElement>(null);
  const [typing, setTyping] = useState<Typing | null>(null);
  const offered = typing && secrets?.length ? rank(secrets, typing.query) : [];

  /** Re-reads what is under the caret after anything that can move it. */
  function reconsider(text: string, caretAt: number | null) {
    // A MASKED BOX REPORTS NO CARET. Browsers refuse selectionStart on a
    // password input, so the offer waits for the value to become a
    // reference, which unmasks it, or for the field to hold nothing but what
    // is being typed.
    const caret = masked || caretAt === null ? text.length : caretAt;
    const next = referenceAt(text, caret);
    // ON THE WAY IN ONLY. The list is about to be shown, which is the moment
    // its being current matters and the only moment worth a request.
    if (next && !typing) onSecretsNeeded?.();
    setTyping(next);
  }

  /** Re-reads the caret from the element, for every move that is not typing. */
  function reconsiderFrom(input: HTMLInputElement) {
    reconsider(input.value, masked ? null : (input.selectionStart ?? null));
  }

  /** Writes the chosen name in and puts the caret after it. */
  function choose(name: string) {
    const input = box.current;
    if (!input || !typing) return;
    const caret = masked ? input.value.length : (input.selectionStart ?? input.value.length);
    const next = complete(input.value, caret, typing, name);
    setTyping(null);
    onChange(next.value);
    // AFTER REACT HAS WRITTEN THE VALUE, or the caret is placed in the old
    // string and lands wherever the new one happens to put it.
    queueMicrotask(() => {
      input.setSelectionRange(next.caret, next.caret);
      input.focus();
    });
  }

  // What the box holds becomes the whole value again, with a scheme somebody
  // typed or pasted taken as said rather than doubled onto the affix.
  function changed(raw: string): string {
    const typed = tight ? raw.replace(/\s+/g, "") : raw;
    if (!affix) {
      onChange(typed);
      return typed;
    }
    const carried = schemeOf(typed);
    // The same two escapes the affix itself makes: a value that carries its
    // own scheme, and one that is becoming a `${VAR}`. `own` is empty here
    // exactly when the affix is OFFERING a scheme rather than carrying one,
    // which is the only state where a leading `$` means a reference.
    if (carried === "http://" || (own === "" && typed.trimStart().startsWith("$"))) {
      onChange(typed);
      return typed;
    }
    const next = affix + typed.slice(carried.length);
    onChange(next);
    return next;
  }

  // EB01. THE CHOSEN OPTION'S HINT IS PART OF THE HELP LINE. Every vendor's
  // setup declares one per choice (`internal/github/setup.go`,
  // `internal/atlassian/setup.go`) and the field accepted them and drew none
  // of them, so the sentence explaining what the selected mode DOES reached
  // nobody. It is also carried under each option in the list, which is what
  // the listbox can do and a native dropdown cannot: one says what you are
  // about to pick, the other what you have picked.
  const chosen = picker ? (choices ?? []).find((choice) => choice.value === value) : undefined;
  const helper: ReactNode =
    chosen?.hint && help ? (
      <>
        {help} {chosen.hint}
      </>
    ) : (
      (chosen?.hint ?? help)
    );

  return (
    <FormField
      htmlFor={id}
      label={label}
      // REQUIRED IS THE UNMARKED DEFAULT on this form, and the exception is
      // marked: most fields are required, so marking them all is noise that
      // teaches a reader to skip the note. A field that just APPEARED because
      // of an answer is the exception to that, and its caller says so. See
      // SetupDialog's `required_when`.
      optional={required === false}
      requiredNews={required === true && markRequired === true}
      helper={helper}
      error={withProblems(error)}
      describedBy={affix ? affixID : undefined}
    >
      {(field) =>
        picker ? (
          <Select
            id={field.id}
            value={value}
            onChange={(next) => onChange(String(next))}
            options={choiceOptions(choices, value)}
            // NO "CHOOSE ONE" OVER AN ANSWER THAT EXISTS. The placeholder is
            // drawn only where nothing is chosen, and empty counts as chosen
            // wherever the list offers it: a unit's lead offers "No lead
            // (inherits the parent's)" as the empty value, and a placeholder
            // over that option would be a second, unlabelled spelling of the
            // same answer, selected in its place.
            placeholder="Choose one"
            disabled={disabled}
            autoFocus={autoFocus}
            error={field.invalid}
            required={required === true}
            aria-describedby={field.describedBy}
          />
        ) : kind === "multiline" ? (
          <Textarea
            id={field.id}
            rows={rows}
            value={value}
            placeholder={placeholder}
            disabled={disabled}
            autoComplete="off"
            // ON, unlike every other kind: see the module doc.
            spellCheck
            autoFocus={autoFocus}
            aria-describedby={field.describedBy}
            aria-required={required === true || undefined}
            error={field.invalid}
            onChange={(event) => onChange(event.target.value)}
          />
        ) : (
          <Combobox
            id={field.id}
            ref={box}
            // A REFERENCE READS AS A NAME, not as the value it stands in for,
            // and the Secrets screen already sets what a stored entry's name
            // looks like. The same face here is what says the two are the same
            // kind of thing.
            appearance={reference ? "reference" : "default"}
            // A secret is a password field unless it holds a reference. See
            // the note above: the type is what keeps a CREDENTIAL out of
            // autofill and out of a screenshot, and a name is neither.
            // NEVER type="email", even on the email field. Any field here may
            // hold a `${VAR}` naming a sealed entry instead of a value, and the
            // browser's own validation refuses one for having no "@" in it:
            // the form could not be saved at all, over a value the engine
            // resolves correctly. inputMode still offers the right keyboard
            // without asserting a shape.
            type={masked ? "password" : "text"}
            inputMode={kind === "email" ? "email" : kind === "url" ? "url" : undefined}
            value={shown}
            placeholder={placeholder}
            disabled={disabled}
            autoFocus={autoFocus}
            error={field.invalid}
            aria-describedby={field.describedBy}
            aria-required={required === true || undefined}
            leading={
              affix ? (
                // The affix is decoration to a sighted reader and part of the
                // value to everybody else, so it is said once rather than read
                // out as punctuation before the box.
                <InputAffix text={affix} id={affixID} />
              ) : undefined
            }
            label="Secrets"
            mono
            // TAB TAKES THE HIGHLIGHTED NAME rather than leaving the field,
            // which is what it means in every other completion list.
            tabCommits
            options={offered.map((name) => ({ value: name }))}
            onCommit={(option) => choose(option.value)}
            open={offered.length > 0}
            onOpenChange={(open) => {
              // Escape means "not this", and the field keeps what was typed.
              if (!open) setTyping(null);
            }}
            onValueChange={(raw) => {
              const next = changed(raw);
              reconsider(next, box.current?.selectionStart ?? null);
            }}
            // EVERY WAY THE CARET MOVES, not only typing: clicking into an
            // existing `${NAME}` or arrowing back over one is how somebody
            // edits the reference they already have.
            onKeyUp={(event) => reconsiderFrom(event.currentTarget)}
            onClick={(event) => reconsiderFrom(event.currentTarget)}
            // CLOSED ON THE WAY OUT, and after the press that chose a name:
            // blur fires before the list's own mousedown, so the choice is
            // taken on mousedown rather than on click.
            onBlur={() => setTyping(null)}
          />
        )
      }
    </FormField>
  );
}

/**
 * The options a choice field offers, and the two rules that are not obvious.
 *
 * A STORED ANSWER THIS LIST DOES NOT OFFER keeps its own row, rather than
 * vanishing into a selection it is not. A form may narrow its choices
 * (GitHub's coverage question now offers two of the three modes its config
 * accepts), and a company already holding the dropped one must not open the
 * dialog to find a different answer selected, then save it. The design
 * system's listbox keeps an unoffered value on the TRIGGER by itself; what is
 * added here is the row, so the reader can see what they have and leave it.
 *
 * A CHOICE'S HINT IS ITS OPTION'S SECOND LINE, which is what it was written
 * to be: a sentence saying what picking this one does.
 */
function choiceOptions(choices: FieldChoice[] | undefined, value: string) {
  const all = choices ?? [];
  const rows = all.map((choice) => ({
    value: choice.value,
    label: choice.label,
    description: choice.hint,
  }));
  if (value !== "" && !all.some((choice) => choice.value === value)) {
    return [{ value, label: value }, ...rows];
  }
  return rows;
}

/**
 * The scheme a value already carries, or "".
 *
 * Only the two the config accepts, and matched case-insensitively because a
 * pasted address is whatever the address bar had.
 */
function schemeOf(value: string): string {
  const lower = value.toLowerCase();
  if (lower.startsWith("https://")) return "https://";
  if (lower.startsWith("http://")) return "http://";
  return "";
}

/**
 * Whether a value is wholly a `${NAME}` reference.
 *
 * The same grammar as the engine's `internal/envref`, and PRESENTATION ONLY:
 * the engine decides what a reference is, refuses what is not one, and only
 * ever sends a secret's value when it is a whole reference already. This
 * decides one thing, whether to mask, and it fails in the safe direction. Too
 * strict masks a reference, which is what this field did before; too loose
 * would need a credential that is literally spelled `${WORD}`.
 */
function isReference(value: string): boolean {
  return /^\$\{[A-Za-z_][A-Za-z0-9_]*\}$/.test(value.trim());
}
