/**
 * A labelled input, with the help line and the error line beside it.
 *
 * ON uilet's [FormField], which is what the hand-pairing here was. A label,
 * a control, a line saying where the value comes from and a line saying why
 * the engine refused the last one: the component hands the control an `id`,
 * an `aria-describedby` and an `invalid` flag already joined, so the one
 * thing every form gets wrong — a label pointing at one element and a
 * description pointing at another — cannot be got wrong here.
 *
 * THE REFUSAL NOW READS AS A REFUSAL, which is the change worth naming. Our
 * own stylesheet drew a field's error in `.field > .hint`'s muted grey,
 * because `.field-error` only ever restyled the code chips inside it: the one
 * line saying the value cannot be saved was set in exactly the ink of the
 * line saying what the value is for. uilet's error line takes the measured
 * danger step. (The bug uilet's own doc warns about — the help line being
 * REPLACED by the error — this field never had: both have always rendered,
 * and both still do.)
 *
 * What stays ours is everything below, because none of it has a peer.
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
 */

import { useId, useRef, useState, type ReactNode } from "react";
import { FormField, Input, InputAffix, Select, useListbox } from "@crewlethq/ui";

import { Problems } from "./Problems.tsx";
import { complete, rank, referenceAt, type Typing } from "./secretref.ts";

export type FieldKind = "text" | "secret" | "url" | "id" | "choice" | "handle" | "email";

/** One option of a choice field. */
export interface FieldChoice {
  value: string;
  label: string;
  hint?: string;
}

export function Field({
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
   * NEWS — one that appeared because of an answer above it. Required is the
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
  // http. `${VAR}` is a whole value this field accepts — the engine's own
  // `hasHTTPScheme` admits `envref.Has` — so the scheme the affix stands in
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
  // just the rest of the address — `https://${JIRA_HOST}` is an ordinary
  // value — and there the affix is CARRYING a scheme rather than offering
  // one, so it must stay.
  const refBase = own === "" && value.trimStart().startsWith("$");
  const affix = kind === "url" && own !== "http://" && !refBase ? "https://" : "";
  const shown = affix ? value.slice(own.length) : value;

  // --- completing a ${NAME} ------------------------------------------- //
  const box = useRef<HTMLInputElement>(null);
  const [typing, setTyping] = useState<Typing | null>(null);
  const offered = typing && secrets?.length ? rank(secrets, typing.query) : [];
  const open = offered.length > 0;
  // THE KEYBOARD IS uilet's, WHICH IS WHERE THIS ONE CAME FROM: `useListbox`'s
  // own doc says it was ported out of this dashboard. It keeps every rule the
  // hand-written one kept — the highlight wraps, it is CLAMPED rather than
  // reset as a list shrinks under somebody's typing, Enter and Tab take the
  // highlighted name and are prevented (this form submits on Enter, and
  // choosing a name is not asking to save), Escape closes the list and STOPS
  // there so the dialog around it stays open, and a press on a row is taken on
  // `mousedown` because the field blurs first and a list that closed on blur
  // would take the row out from under the click choosing it.
  //
  // What it adds is the layer stack: the list is a popup above the dialog, so
  // a press on the dialog's veil closes the list and leaves the dialog.
  const listbox = useListbox({
    id,
    open,
    count: offered.length,
    onCommit: (index) => choose(offered[index] ?? ""),
    onClose: () => setTyping(null),
    tabCommits: true,
  });

  /** Re-reads what is under the caret after anything that can move it. */
  function reconsider(target: HTMLInputElement) {
    // WIRED TO THE PLAIN INPUT ONLY. A url field wears an affix, so the box
    // holds a different string from the value and every caret offset would
    // have to be translated through it; every field a reference actually goes
    // in — a secret, an id, an email, a token — is a plain one.
    if (affix) return;
    // A MASKED BOX REPORTS NO CARET. Browsers refuse selectionStart on a
    // password input, so the offer waits for the value to become a
    // reference, which unmasks it, or for the field to hold nothing but what
    // is being typed.
    const caret = masked ? target.value.length : (target.selectionStart ?? target.value.length);
    const next = referenceAt(target.value, caret);
    // ON THE WAY IN ONLY. The list is about to be shown, which is the moment
    // its being current matters and the only moment worth a request.
    if (next && !typing) onSecretsNeeded?.();
    setTyping(next);
  }

  /** Writes the chosen name in and puts the caret after it. */
  function choose(name: string) {
    const input = box.current;
    if (!input || !typing) return;
    const caret = masked ? input.value.length : (input.selectionStart ?? input.value.length);
    const next = complete(input.value, caret, typing, name);
    setTyping(null);
    // BACK TO THE TOP for the next list. The highlight is the package's own
    // state and survives a close, so a second `$` would otherwise open on
    // whichever row the last one ended on.
    listbox.setActive(0);
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
  function changed(raw: string) {
    const typed = tight ? raw.replace(/\s+/g, "") : raw;
    if (!affix) return onChange(typed);
    const carried = schemeOf(typed);
    // The same two escapes the affix itself makes: a value that carries its
    // own scheme, and one that is becoming a `${VAR}`. `own` is empty here
    // exactly when the affix is OFFERING a scheme rather than carrying one,
    // which is the only state where a leading `$` means a reference.
    if (carried === "http://" || (own === "" && typed.trimStart().startsWith("$"))) {
      return onChange(typed);
    }
    onChange(affix + typed.slice(carried.length));
  }

  return (
    <FormField
      label={label}
      htmlFor={id}
      // OUR OWN CLASS ON THE PACKAGE'S COMPONENT, and the one rule that keys
      // on it is the reason: `.int-form .field > label` puts the setup form's
      // questions in sentence case, where the product's field label is an
      // uppercase micro-label. That is a rule about OUR composition of
      // [FormField], and a class we write is a better anchor for it than one
      // the package owns and could rename.
      className="field"
      // NOT `required`, WHICH WOULD DRAW AN ASTERISK. uilet's Label welds the
      // red `*` to the same flag that carries `aria-required`, and the
      // convention on this form is that required is the unmarked default and
      // the exception is marked — most fields are required, so marking them
      // all is noise that teaches a reader to skip the note.
      optional={required === false}
      // AND REQUIRED IS MARKED ONLY WHERE IT IS NEWS. A field that just
      // APPEARED because of an answer is the exception: its requiredness was
      // not on screen a moment ago and is now the one thing standing between
      // the answer above it and its working. See SetupDialog's `required_when`.
      requiredNews={required === true && markRequired === true}
      helper={help}
      error={
        error ? (
          // THE SAME FACES the banner gives a refusal: a field's own error is
          // one problem out of the same set, and a config path or a `${VAR}`
          // in it is the same kind of thing here.
          <Problems detail={error} />
        ) : undefined
      }
      // The affix is part of the value, so the reader has to hear it; it is
      // joined AHEAD of the help and the error, in reading order.
      describedBy={affix ? affixID : undefined}
    >
      {(field) =>
        picker ? (
          <Select
            // THE LISTBOX, WHICH IS THE HOUSE STYLE AND ALSO THE ONLY MODE
            // THAT CAN DRAW A CHOICE'S SECOND LINE. A native `<option>` holds
            // one line, so `mode="native"` accepted `choices[].hint` and put
            // it nowhere — and the engine populates it with exactly the
            // sentence that tells two choices apart: `internal/atlassian`
            // offers "Cloud" and "Data Center" and distinguishes them only as
            // "Crewlet reads your sites and creates each agent's account"
            // against "Self-hosted. Give each product's address and its own
            // token." An operator reading the labels alone is choosing blind
            // between two deployments of the same vendor.
            id={field.id}
            value={value}
            // NO "CHOOSE ONE" OVER AN ANSWER THAT EXISTS, and A STORED ANSWER
            // THIS LIST DOES NOT OFFER KEEPS ITS OWN OPTION. Both were rules
            // this file spelled out and both are the package's now: it draws
            // the placeholder only while nothing is chosen, and an unknown
            // value as itself — so a form may narrow its choices without a
            // company already holding the dropped one opening the dialog to
            // find a different answer selected, then saving it.
            placeholder="Choose one"
            options={(choices ?? []).map((c) => ({
              value: c.value,
              label: c.label,
              // Carried, and drawn only in the listbox mode: a native
              // `<option>` holds one line. See the report.
              description: c.hint,
            }))}
            disabled={disabled}
            error={field.invalid}
            aria-describedby={field.describedBy}
            // A native single-choice select never answers with a list.
            onChange={(next) => onChange(String(next))}
          />
        ) : (
          <div className="input-suggests" ref={listbox.anchorRef}>
            <Input
              id={field.id}
              ref={box}
              // A REFERENCE READS AS A NAME, not as the value it stands in
              // for, and the Secrets screen already sets what a stored entry's
              // name looks like. The same face here is what says the two are
              // the same kind of thing.
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
              // Off for every field here, not only the secrets: a browser
              // offering a saved password for a webhook token is offering the
              // wrong credential to the wrong vendor.
              autoComplete="off"
              spellCheck={false}
              autoFocus={autoFocus}
              error={field.invalid}
              aria-describedby={field.describedBy}
              // The affix sits INSIDE the control's boundary, so the box reads
              // as one value: a chip in front of the field reads as two, and
              // the second of them looks optional. It is a picture for a
              // sighted reader and part of the value for everybody else, which
              // is why it says itself once as a sentence rather than being
              // read out as punctuation before the box.
              leading={affix ? <InputAffix text={affix} id={affixID} /> : undefined}
              role={open ? "combobox" : undefined}
              aria-expanded={open || undefined}
              aria-controls={open ? listbox.listId : undefined}
              aria-activedescendant={open ? listbox.optionId(listbox.active) : undefined}
              onChange={(e) => {
                changed(e.target.value);
                reconsider(e.target);
              }}
              // EVERY WAY THE CARET MOVES, not only typing: clicking into an
              // existing `${NAME}` or arrowing back over one is how somebody
              // edits the reference they already have.
              onKeyUp={(e) => reconsider(e.currentTarget)}
              onClick={(e) => reconsider(e.currentTarget)}
              onKeyDown={listbox.onKeyDown}
              // CLOSED ON THE WAY OUT, and after the click that chose a name:
              // blur fires before the list's own mousedown, so the choice is
              // taken on mousedown rather than on click.
              onBlur={() => setTyping(null)}
            />
            {open && (
              // OURS, AND DELIBERATELY. uilet's listbox register is a
              // `--size-row-sm` row set in the page's own face, which is right
              // for a picker read one candidate at a time; these are SEALED
              // ENTRY NAMES, and they wear the monospace face a reference
              // wears in the box above and on the Secrets screen. Only the
              // keyboard is the package's. See the report.
              <ul
                className="input-suggest-list"
                id={listbox.listId}
                ref={listbox.listRef}
                role="listbox"
                aria-label="Secrets"
              >
                {offered.map((name, i) => (
                  <li
                    key={name}
                    id={listbox.optionId(i)}
                    role="option"
                    aria-selected={i === listbox.active}
                    className={i === listbox.active ? "input-suggest is-active" : "input-suggest"}
                    {...listbox.optionHandlers(i)}
                  >
                    {name}
                  </li>
                ))}
              </ul>
            )}
          </div>
        )
      }
    </FormField>
  );
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
