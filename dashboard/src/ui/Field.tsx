/**
 * A labelled input, with the help line and the error line beside it.
 *
 * The dashboard had `SearchInput` and `Select` and nothing else: a filter box
 * and a dropdown, both for narrowing a read. A form that collects a credential
 * needs a label bound to its control, a line saying where the value comes
 * from, and a line saying why the engine refused the last one, and every one
 * of those is a thing that gets forgotten when each screen writes its own
 * `<label>` next to its own `<input>`.
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
  autoFocus,
  secrets,
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
  autoFocus?: boolean;
  /**
   * The sealed entries this company holds, offered when somebody types `$`.
   *
   * NAMES ONLY, and that is the whole contract: a value never reaches this
   * screen, so a list that could not be assembled leaves the field exactly
   * as it was rather than degrading it.
   */
  secrets?: string[];
}) {
  const id = useId();
  const helpID = `${id}-help`;
  const errorID = `${id}-error`;
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
  const affix = kind === "url" && own !== "http://" ? "https://" : "";
  const shown = affix ? value.slice(own.length) : value;
  const describedBy = [affix ? affixID : "", help ? helpID : "", error ? errorID : ""]
    .filter(Boolean)
    .join(" ");

  // --- completing a ${NAME} ------------------------------------------- //
  //
  // WIRED TO THE PLAIN INPUT ONLY. A url field wears an affix, so the box
  // holds a different string from the value and every caret offset would
  // have to be translated through it; every field a reference actually goes
  // in — a secret, an id, an email, a token — is a plain one.
  const box = useRef<HTMLInputElement>(null);
  const [typing, setTyping] = useState<Typing | null>(null);
  const [at, setAt] = useState(0);
  const listID = `${id}-secrets`;
  const offered = typing && secrets?.length ? rank(secrets, typing.query) : [];
  const open = offered.length > 0;
  // CLAMPED RATHER THAN RESET, so a list that shrinks under somebody's
  // finger leaves the highlight on a row that exists instead of jumping
  // back to the top as they type.
  const active = Math.min(at, offered.length - 1);

  /** Re-reads what is under the caret after anything that can move it. */
  function reconsider(target: HTMLInputElement) {
    if (masked) {
      // A MASKED BOX REPORTS NO CARET. Browsers refuse selectionStart on a
      // password input, so the offer waits for the value to become a
      // reference, which unmasks it, or for the field to hold nothing but
      // what is being typed.
      setTyping(referenceAt(target.value, target.value.length));
      return;
    }
    setTyping(referenceAt(target.value, target.selectionStart ?? target.value.length));
  }

  /** Writes the chosen name in and puts the caret after it. */
  function choose(name: string) {
    const input = box.current;
    if (!input || !typing) return;
    const caret = masked ? input.value.length : (input.selectionStart ?? input.value.length);
    const next = complete(input.value, caret, typing, name);
    setTyping(null);
    setAt(0);
    onChange(next.value);
    // AFTER REACT HAS WRITTEN THE VALUE, or the caret is placed in the old
    // string and lands wherever the new one happens to put it.
    queueMicrotask(() => {
      input.setSelectionRange(next.caret, next.caret);
      input.focus();
    });
  }

  function navigate(e: React.KeyboardEvent<HTMLInputElement>) {
    if (!open) return;
    switch (e.key) {
      case "ArrowDown":
        e.preventDefault();
        setAt((was) => (was + 1) % offered.length);
        return;
      case "ArrowUp":
        e.preventDefault();
        setAt((was) => (was - 1 + offered.length) % offered.length);
        return;
      case "Enter":
      case "Tab":
        // TAB TAKES THE HIGHLIGHTED NAME rather than leaving the field,
        // which is what it means in every other completion list. Enter is
        // prevented too: this form submits on Enter, and choosing a name
        // is not asking to save.
        e.preventDefault();
        choose(offered[active] ?? "");
        return;
      case "Escape":
        // THE LIST CLOSES, AND THE DIALOG DOES NOT. Escape here means "not
        // this", and the field keeps what was typed.
        e.preventDefault();
        e.stopPropagation();
        setTyping(null);
        return;
      default:
        return;
    }
  }

  // What the box holds becomes the whole value again, with a scheme somebody
  // typed or pasted taken as said rather than doubled onto the affix.
  function changed(raw: string) {
    const typed = tight ? raw.replace(/\s+/g, "") : raw;
    if (!affix) return onChange(typed);
    const carried = schemeOf(typed);
    onChange(carried === "http://" ? typed : affix + typed.slice(carried.length));
  }

  return (
    <div className="field">
      <label htmlFor={id}>
        {label}
        {required === false && <span className="faint"> (optional)</span>}
      </label>
      {picker ? (
        <select
          id={id}
          className="input"
          value={value}
          disabled={disabled}
          aria-describedby={describedBy || undefined}
          aria-invalid={error ? true : undefined}
          onChange={(e) => onChange(e.target.value)}
        >
          <option value="">Choose one</option>
          {(choices ?? []).map((c) => (
            <option key={c.value} value={c.value}>
              {c.label}
            </option>
          ))}
        </select>
      ) : affix ? (
        <div className="input-affixed">
          <span className="input-affix" id={affixID} aria-hidden="true">
            {affix}
          </span>
          <input
            id={id}
            className={reference ? "input is-reference" : "input"}
            inputMode="url"
            value={shown}
            placeholder={placeholder}
            disabled={disabled}
            autoComplete="off"
            spellCheck={false}
            autoFocus={autoFocus}
            aria-describedby={describedBy || undefined}
            aria-invalid={error ? true : undefined}
            onChange={(e) => changed(e.target.value)}
          />
        </div>
      ) : (
        <div className="input-suggests">
          <input
            id={id}
            ref={box}
            // A REFERENCE READS AS A NAME, not as the value it stands in for,
            // and the Secrets screen already sets what a stored entry's name
            // looks like. The same face here is what says the two are the same
            // kind of thing.
            className={reference ? "input is-reference" : "input"}
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
            value={value}
            placeholder={placeholder}
            disabled={disabled}
            // Off for every field here, not only the secrets: a browser
            // offering a saved password for a webhook token is offering the
            // wrong credential to the wrong vendor.
            autoComplete="off"
            spellCheck={false}
            autoFocus={autoFocus}
            aria-describedby={describedBy || undefined}
            aria-invalid={error ? true : undefined}
            role={open ? "combobox" : undefined}
            aria-expanded={open || undefined}
            aria-controls={open ? listID : undefined}
            aria-activedescendant={open ? `${listID}-${active}` : undefined}
            onChange={(e) => {
              changed(e.target.value);
              reconsider(e.target);
            }}
            // EVERY WAY THE CARET MOVES, not only typing: clicking into an
            // existing `${NAME}` or arrowing back over one is how somebody
            // edits the reference they already have.
            onKeyUp={(e) => reconsider(e.currentTarget)}
            onClick={(e) => reconsider(e.currentTarget)}
            onKeyDown={navigate}
            // CLOSED ON THE WAY OUT, and after the click that chose a name:
            // blur fires before the list's own mousedown, so the choice is
            // taken on mousedown below rather than on click.
            onBlur={() => setTyping(null)}
          />
          {open && (
            <ul className="input-suggest-list" id={listID} role="listbox" aria-label="Secrets">
              {offered.map((name, i) => (
                <li
                  key={name}
                  id={`${listID}-${i}`}
                  role="option"
                  aria-selected={i === active}
                  className={i === active ? "input-suggest is-active" : "input-suggest"}
                  // MOUSEDOWN, NOT CLICK. The field blurs on mousedown, and a
                  // blur that closed the list would take the row out from
                  // under the click that was choosing it.
                  onMouseDown={(e) => {
                    e.preventDefault();
                    choose(name);
                  }}
                  onMouseEnter={() => setAt(i)}
                >
                  {name}
                </li>
              ))}
            </ul>
          )}
        </div>
      )}
      {affix && (
        // The affix is decoration to a sighted reader and part of the value
        // to everybody else, so it is said once rather than read out as
        // punctuation before the box.
        <span className="sr-only" id={affixID}>
          Begins with {affix}
        </span>
      )}
      {help && (
        <span className="hint" id={helpID}>
          {help}
        </span>
      )}
      {error && (
        <span className="hint field-error" id={errorID} role="alert">
          {/* THE SAME FACES the banner gives a refusal: a field's own error
              is one problem out of the same set, and a config path or a
              `${VAR}` in it is the same kind of thing here. */}
          <Problems detail={error} />
        </span>
      )}
    </div>
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
