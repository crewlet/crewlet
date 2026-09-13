/**
 * A searchable multi-select: chosen values as removable chips, and a filtered
 * list to choose more from.
 *
 * WHY IT EXISTS, AND WHY ONLY FOR MANY. A single choice is a native `<select>`
 * (`Field kind="choice"`): the platform gives it keyboard navigation,
 * type-ahead and its own overlay, and a hand-built listbox would have to earn
 * all three back. Choosing several out of dozens (the seats and units a seat
 * manages) is what a select cannot do: a multiple select is a scrolling box
 * that loses its selection to a stray click. So this is the one hand-built
 * listbox, and its keys are the shared ones (`useListbox`), the same the
 * secret completion uses.
 *
 * THE RULES IT KEEPS.
 *
 * - FOCUS STAYS IN THE SEARCH BOX; the highlighted option is pointed at with
 *   `aria-activedescendant`, and every option says whether it is chosen with
 *   `aria-selected` in a listbox marked `aria-multiselectable`.
 * - ENTER OR A PRESS TOGGLES the highlighted option and leaves the list open
 *   with the search as it was, so several neighbours in one filtered list are
 *   chosen without retyping. Tab leaves the field, as every form control does.
 * - ESCAPE CLOSES THE LIST AND NOTHING ELSE; a second Escape reaches the
 *   drawer or dialog around it.
 * - EVERY ADDITION AND REMOVAL IS ANNOUNCED through a polite live region,
 *   because a chip appearing above the box is otherwise silent.
 * - BACKSPACE IN AN EMPTY SEARCH REMOVES THE LAST CHOICE, as in every token
 *   field, and says so.
 * - A CHOSEN VALUE THE OPTIONS DO NOT OFFER is shown as itself rather than
 *   dropped: a stored reference to something since renamed must stay visible
 *   until somebody removes it on purpose.
 * - ORDER IS THE ORDER CHOSEN. A new choice is appended; a removal keeps the
 *   rest where they were.
 */

import { useId, useState, type KeyboardEvent, type ReactNode } from "react";
import { Icon } from "./Icon.tsx";
import { Problems } from "./Problems.tsx";
import { cx } from "./primitives.tsx";
import { useListbox } from "./useListbox.ts";

export interface PickerOption {
  value: string;
  label: string;
  /** Options sharing a group are listed under its name, in first-seen order. */
  group?: string;
  /** A short second line of identity, such as a handle. */
  hint?: string;
}

export function MultiPicker({
  label,
  options,
  value,
  onChange,
  placeholder,
  help,
  error,
  disabled,
  required,
}: {
  label: string;
  options: readonly PickerOption[];
  value: readonly string[];
  onChange: (next: string[]) => void;
  placeholder?: string;
  help?: ReactNode;
  error?: string;
  disabled?: boolean;
  required?: boolean;
}) {
  const id = useId();
  const inputID = `${id}-input`;
  const helpID = `${id}-help`;
  const errorID = `${id}-error`;
  const [query, setQuery] = useState("");
  const [open, setOpen] = useState(false);
  const [said, setSaid] = useState({ text: "", n: 0 });

  const byValue = new Map(options.map((o) => [o.value, o]));
  const nameOf = (v: string) => byValue.get(v)?.label ?? v;
  const chosen = new Set(value);

  const needle = query.trim().toLocaleLowerCase();
  const offered = needle
    ? options.filter(
        (o) =>
          o.label.toLocaleLowerCase().includes(needle) ||
          o.value.toLocaleLowerCase().includes(needle),
      )
    : [...options];
  // Grouped for display, and the flat order of the groups is the order the
  // keys walk, so Down never jumps backwards across a heading.
  const groups: { name: string | undefined; items: PickerOption[] }[] = [];
  for (const option of offered) {
    const group = groups.find((g) => g.name === option.group);
    if (group) group.items.push(option);
    else groups.push({ name: option.group, items: [option] });
  }
  const flat = groups.flatMap((g) => g.items);
  const showing = open && !disabled;

  function say(text: string) {
    setSaid((was) => ({ text, n: was.n + 1 }));
  }

  function toggle(option: PickerOption) {
    if (disabled) return;
    if (chosen.has(option.value)) {
      onChange(value.filter((v) => v !== option.value));
      say(`Removed ${option.label}`);
    } else {
      onChange([...value, option.value]);
      say(`Added ${option.label}`);
    }
  }

  function remove(v: string) {
    if (disabled) return;
    onChange(value.filter((x) => x !== v));
    say(`Removed ${nameOf(v)}`);
  }

  const listbox = useListbox({
    id,
    open: showing,
    count: flat.length,
    onCommit: (index) => {
      const option = flat[index];
      if (option) toggle(option);
    },
    onClose: () => setOpen(false),
  });

  function onKeyDown(e: KeyboardEvent<HTMLInputElement>) {
    if (!showing && (e.key === "ArrowDown" || e.key === "ArrowUp")) {
      e.preventDefault();
      setOpen(true);
      return;
    }
    if (listbox.onKeyDown(e)) return;
    // Open with nothing to offer ("Nothing matches"): Escape still closes that
    // first, as it would a list, rather than the dialog around it.
    if (showing && e.key === "Escape") {
      e.preventDefault();
      e.stopPropagation();
      setOpen(false);
      return;
    }
    if (e.key === "Enter" && query.trim() !== "") {
      // A search with nothing highlighted is not a request to save the form.
      e.preventDefault();
      setOpen(true);
      return;
    }
    if (e.key === "Backspace" && query === "" && value.length > 0) {
      e.preventDefault();
      remove(value[value.length - 1]!);
    }
  }

  const describedBy = [help ? helpID : "", error ? errorID : ""].filter(Boolean).join(" ");
  const lists = showing && flat.length > 0;
  let index = -1;

  return (
    <div className="field multi-picker">
      <label htmlFor={inputID}>
        {label}
        {required === false && <span className="faint"> (optional)</span>}
      </label>
      {value.length > 0 && (
        <ul className="multi-picker-values" aria-label={`Chosen: ${label}`}>
          {value.map((v) => (
            <li key={v} className="multi-picker-value">
              <span>{nameOf(v)}</span>
              <button
                type="button"
                className="multi-picker-remove"
                aria-label={`Remove ${nameOf(v)}`}
                title={`Remove ${nameOf(v)}`}
                disabled={disabled}
                onClick={() => remove(v)}
              >
                <Icon name="x" size="xs" />
              </button>
            </li>
          ))}
        </ul>
      )}
      <div className="input-suggests">
        <input
          id={inputID}
          className="input"
          role="combobox"
          aria-autocomplete="list"
          aria-expanded={lists}
          aria-controls={lists ? listbox.listId : undefined}
          aria-activedescendant={lists ? listbox.optionId(listbox.active) : undefined}
          aria-describedby={describedBy || undefined}
          aria-invalid={error ? true : undefined}
          value={query}
          placeholder={placeholder}
          disabled={disabled}
          autoComplete="off"
          spellCheck={false}
          onChange={(e) => {
            setQuery(e.target.value);
            setOpen(true);
          }}
          onClick={() => setOpen(true)}
          onKeyDown={onKeyDown}
          onBlur={() => setOpen(false)}
        />
        {lists && (
          <div
            className="input-suggest-list multi-picker-list"
            id={listbox.listId}
            role="listbox"
            aria-multiselectable="true"
            aria-label={label}
          >
            {groups.map((group, g) => {
              const rows = group.items.map((option) => {
                index += 1;
                const i = index;
                const isChosen = chosen.has(option.value);
                return (
                  <div
                    key={option.value}
                    id={listbox.optionId(i)}
                    role="option"
                    aria-selected={isChosen}
                    className={cx("input-suggest", i === listbox.active && "is-active")}
                    {...listbox.optionHandlers(i)}
                  >
                    <span className="multi-picker-tick" aria-hidden="true">
                      {isChosen && <Icon name="check" size="xs" />}
                    </span>
                    <span className="truncate">{option.label}</span>
                    {option.hint && <span className="multi-picker-hint">{option.hint}</span>}
                  </div>
                );
              });
              if (group.name === undefined) return rows;
              // By position, not by name: an id is one token, and a group
              // named "Go to Market" would be three to `aria-labelledby`.
              const headingID = `${id}-group-${g}`;
              return (
                <div key={group.name} role="group" aria-labelledby={headingID}>
                  <div className="multi-picker-group" id={headingID} role="presentation">
                    {group.name}
                  </div>
                  {rows}
                </div>
              );
            })}
          </div>
        )}
        {showing && flat.length === 0 && needle !== "" && (
          <div className="input-suggest-list">
            <div className="multi-picker-empty">Nothing matches &ldquo;{query.trim()}&rdquo;</div>
          </div>
        )}
      </div>
      {help && (
        <span className="hint" id={helpID}>
          {help}
        </span>
      )}
      {error && (
        <span className="hint field-error" id={errorID} role="alert">
          <Problems detail={error} />
        </span>
      )}
      <span className="sr-only" role="status" aria-live="polite">
        <span key={said.n}>{said.text}</span>
      </span>
    </div>
  );
}
