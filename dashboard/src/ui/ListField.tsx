/**
 * An ordered list of strings, edited in place.
 *
 * WHY IT EXISTS. A seat's responsibilities, a unit's goals and a company's
 * policies are ordered lists of sentences, and the order is meaning: the
 * first policy is read first. A comma-separated text box loses commas inside
 * a sentence, and one row per item with no way to reorder turns "move this
 * up" into deleting and retyping it.
 *
 * THE RULES IT KEEPS.
 *
 * - ENTER ADDS. In the new-item box, Enter appends what was typed (trimmed at
 *   the ends) and leaves focus in the box for the next one. In an item, Enter
 *   moves on to the next item, or to the new-item box after the last. Enter is
 *   never allowed to submit the form the list sits in: finishing a sentence is
 *   not saving the editor.
 * - SHIFT+ENTER IS A NEWLINE in a multiline list, where an item is a paragraph.
 * - A KEY AN INPUT METHOD IS COMPOSING WITH IS ITS OWN (`ui/keys.ts`): the
 *   Enter that accepts a word neither adds the item nor moves on, and Alt+Arrow
 *   does not reorder under a half-built word.
 * - EVERY ITEM IS EDITED WHERE IT IS, as its own labelled control ("Goal 2 of
 *   3"), with Move up, Move down and Remove beside it.
 * - ALT+UP AND ALT+DOWN MOVE THE FOCUSED ITEM, and focus moves with it. After a
 *   Move button, focus stays on that item's button, or on its other one when
 *   the item reached an end, so pressing it again keeps working.
 * - REMOVING an item moves focus to the item that took its place, else the one
 *   before, else the new-item box, so focus never falls to the page.
 * - EVERY CHANGE IS ANNOUNCED through one polite live region, because a move
 *   or a removal is otherwise silent to anyone not watching the rows.
 * - AN ITEM KEEPS ITS ELEMENT ACROSS A MOVE, so a caret and an input method
 *   composition survive reordering. Two identical sentences are still two
 *   items.
 */

import {
  useId,
  useLayoutEffect,
  useRef,
  useState,
  type KeyboardEvent,
  type ReactNode,
} from "react";
import { Button } from "./primitives.tsx";
import { Problems } from "./Problems.tsx";
import {
  AddGlyph,
  ArrowDownwardGlyph,
  ArrowUpwardGlyph,
  DeleteGlyph,
} from "@crewlethq/icons/glyphs";
import { isComposing } from "@crewlethq/ui";

let minted = 0;
const mint = () => `item-${++minted}`;

type Target = "input" | "up" | "down" | "add";

function capitalized(text: string): string {
  return text.charAt(0).toLocaleUpperCase() + text.slice(1);
}

export function ListField({
  label,
  itemName,
  value,
  onChange,
  multiline = false,
  help,
  error,
  placeholder,
  disabled,
  required,
}: {
  /** The list's name, such as "Responsibilities". */
  label: string;
  /** One item's name in lower case, such as "responsibility": used in every control's name. */
  itemName: string;
  value: readonly string[];
  onChange: (next: string[]) => void;
  /** Items are paragraphs: Shift+Enter makes a newline. */
  multiline?: boolean;
  help?: ReactNode;
  error?: string;
  /** Shown in the new-item box. */
  placeholder?: string;
  disabled?: boolean;
  required?: boolean;
}) {
  const id = useId();
  const helpID = `${id}-help`;
  const errorID = `${id}-error`;
  const [draft, setDraft] = useState("");
  const [said, setSaid] = useState({ text: "", n: 0 });

  // IDENTITY PER ITEM, kept beside the strings. See the module doc.
  const ids = useRef<string[]>([]);
  const emitted = useRef<readonly string[] | null>(null);
  if (value !== emitted.current && value.length !== ids.current.length) {
    ids.current = value.map(mint);
  }

  const inputs = useRef(new Map<string, HTMLInputElement | HTMLTextAreaElement>());
  const ups = useRef(new Map<string, HTMLButtonElement>());
  const downs = useRef(new Map<string, HTMLButtonElement>());
  const addBox = useRef<HTMLInputElement | HTMLTextAreaElement | null>(null);
  const focusNext = useRef<{ id: string; target: Target } | null>(null);

  useLayoutEffect(() => {
    const want = focusNext.current;
    if (!want) return;
    focusNext.current = null;
    if (want.target === "add") return addBox.current?.focus();
    const up = ups.current.get(want.id);
    const down = downs.current.get(want.id);
    const input = inputs.current.get(want.id);
    const order =
      want.target === "up"
        ? [up, down, input]
        : want.target === "down"
          ? [down, up, input]
          : [input];
    order.find((el) => el && !(el as HTMLButtonElement).disabled)?.focus();
  });

  function emit(next: string[], nextIds: string[], announce?: string) {
    ids.current = nextIds;
    emitted.current = next;
    onChange(next);
    if (announce) setSaid((was) => ({ text: announce, n: was.n + 1 }));
  }

  const name = capitalized(itemName);
  const count = value.length;

  function add() {
    const text = draft.trim();
    if (!text || disabled) return;
    emit(
      [...value, text],
      [...ids.current, mint()],
      `Added ${itemName} ${count + 1} of ${count + 1}`,
    );
    setDraft("");
    focusNext.current = { id: "", target: "add" };
  }

  function move(index: number, by: -1 | 1, target: Target) {
    const to = index + by;
    if (disabled || to < 0 || to >= count) return;
    const next = [...value];
    const nextIds = [...ids.current];
    [next[index], next[to]] = [next[to]!, next[index]!];
    [nextIds[index], nextIds[to]] = [nextIds[to]!, nextIds[index]!];
    focusNext.current = { id: nextIds[to]!, target };
    emit(next, nextIds, `Moved ${itemName} to position ${to + 1} of ${count}`);
  }

  function remove(index: number) {
    if (disabled) return;
    const next = value.filter((_, i) => i !== index);
    const nextIds = ids.current.filter((_, i) => i !== index);
    const neighbour = nextIds[index] ?? nextIds[index - 1];
    focusNext.current = neighbour ? { id: neighbour, target: "input" } : { id: "", target: "add" };
    emit(next, nextIds, `Removed ${itemName} ${index + 1} of ${count}`);
  }

  /** Enter that should act rather than insert: always in one line, without Shift in many. */
  const acts = (e: KeyboardEvent) =>
    e.key === "Enter" && !(multiline && e.shiftKey) && !isComposing(e);

  function itemKeys(e: KeyboardEvent, index: number) {
    if (isComposing(e)) return;
    if (e.altKey && (e.key === "ArrowUp" || e.key === "ArrowDown")) {
      e.preventDefault();
      move(index, e.key === "ArrowUp" ? -1 : 1, "input");
      return;
    }
    if (acts(e)) {
      e.preventDefault();
      const next = ids.current[index + 1];
      if (next) inputs.current.get(next)?.focus();
      else addBox.current?.focus();
    }
  }

  const describedBy = [help ? helpID : "", error ? errorID : ""].filter(Boolean).join(" ");
  const control = (props: {
    label: string;
    value: string;
    onChange: (v: string) => void;
    onKeyDown: (e: KeyboardEvent) => void;
    ref: (el: HTMLInputElement | HTMLTextAreaElement | null) => void;
    placeholder?: string;
  }) =>
    multiline ? (
      <textarea
        className="textarea"
        rows={2}
        aria-label={props.label}
        value={props.value}
        placeholder={props.placeholder}
        disabled={disabled}
        spellCheck
        onChange={(e) => props.onChange(e.target.value)}
        onKeyDown={props.onKeyDown}
        ref={props.ref}
      />
    ) : (
      <input
        className="input"
        aria-label={props.label}
        value={props.value}
        placeholder={props.placeholder}
        disabled={disabled}
        autoComplete="off"
        spellCheck
        onChange={(e) => props.onChange(e.target.value)}
        onKeyDown={props.onKeyDown}
        ref={props.ref}
      />
    );

  return (
    <fieldset className="field list-field" aria-describedby={describedBy || undefined}>
      <legend>
        {label}
        {required === false && <span className="faint"> (optional)</span>}
      </legend>
      {count > 0 && (
        <ol className="list-field-items">
          {value.map((item, index) => {
            const key = ids.current[index]!;
            const position = `${index + 1} of ${count}`;
            return (
              <li key={key} className="list-field-item">
                {control({
                  label: `${name} ${position}`,
                  value: item,
                  onChange: (v) => {
                    const next = [...value];
                    next[index] = v;
                    emit(next, ids.current);
                  },
                  onKeyDown: (e) => itemKeys(e, index),
                  ref: (el) => {
                    if (el) inputs.current.set(key, el);
                    else inputs.current.delete(key);
                  },
                })}
                <span className="list-field-actions">
                  <Button
                    variant="ghost"
                    size="sm"
                    icon={ArrowUpwardGlyph}
                    aria-label={`Move ${itemName} ${position} up`}
                    title="Move up"
                    disabled={disabled || index === 0}
                    onClick={() => move(index, -1, "up")}
                    ref={(el) => {
                      if (el) ups.current.set(key, el);
                      else ups.current.delete(key);
                    }}
                  />
                  <Button
                    variant="ghost"
                    size="sm"
                    icon={ArrowDownwardGlyph}
                    aria-label={`Move ${itemName} ${position} down`}
                    title="Move down"
                    disabled={disabled || index === count - 1}
                    onClick={() => move(index, 1, "down")}
                    ref={(el) => {
                      if (el) downs.current.set(key, el);
                      else downs.current.delete(key);
                    }}
                  />
                  <Button
                    variant="ghost"
                    size="sm"
                    icon={DeleteGlyph}
                    aria-label={`Remove ${itemName} ${position}`}
                    title="Remove"
                    disabled={disabled}
                    onClick={() => remove(index)}
                  />
                </span>
              </li>
            );
          })}
        </ol>
      )}
      <div className="list-field-add">
        {control({
          label: `New ${itemName}`,
          value: draft,
          placeholder,
          onChange: setDraft,
          onKeyDown: (e) => {
            if (!acts(e)) return;
            e.preventDefault();
            add();
          },
          ref: (el) => {
            addBox.current = el;
          },
        })}
        <Button size="sm" icon={AddGlyph} onClick={add} disabled={disabled || draft.trim() === ""}>
          Add
        </Button>
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
      {/* Keyed by a counter, so the same sentence twice in a row (two
          removals) is announced twice rather than read as no change. */}
      <span className="sr-only" role="status" aria-live="polite">
        <span key={said.n}>{said.text}</span>
      </span>
    </fieldset>
  );
}
