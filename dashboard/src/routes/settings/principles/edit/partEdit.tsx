/**
 * One card's share of the editing: its Edit, its Add, its editor, and what it
 * shows meanwhile, as the page's card takes them (`../PrincipleCard.tsx`).
 *
 * THE CARD STAYS THE VIEW'S. It draws its header, its words and its empty
 * state as it always does, and places what this returns where it goes: the
 * Edit in its header's actions, the Add in its empty state, and the editor in
 * place of its body while the part is open. Every control is null for a
 * reader who may not write the charter, so the card needs no question of its
 * own about who is reading it.
 */

import { Button } from "@crewlethq/ui";
import { PencilGlyph, PlusGlyph } from "@crewlethq/icons/glyphs";
import type { PrincipleCardEdit } from "../PrincipleCard.tsx";
import {
  charterOf,
  isEmptyPart,
  partName,
  partOf,
  type CharterPart,
  type PartValue,
} from "./charter.ts";
import { PartEditor } from "./PartEditor.tsx";
import { landedView } from "./state.ts";
import type { Editing } from "./useCharterEditing.tsx";

/** The words that write an unwritten part, the same wherever the part is drawn. */
export const ADD_LABELS: Readonly<Record<CharterPart, string>> = {
  mission: "Add a mission",
  vision: "Add a vision",
  policies: "Add a policy",
};

/** The part's share of the editing, for the card that shows it. */
export function partEdit(editing: Editing, part: "policies"): PrincipleCardEdit<readonly string[]>;
export function partEdit(editing: Editing, part: "mission" | "vision"): PrincipleCardEdit<string>;
export function partEdit(editing: Editing, part: CharterPart): PrincipleCardEdit<PartValue> {
  const session = editing.state.sessions[part];
  const projected = partOf(charterOf(editing.org), part);
  const landed = editing.state.landed[part];
  // A SAVE IS SHOWN AS SAVED until the page's chart has it, and once this
  // node has refused its revision the card draws what this node still runs
  // and says so (`state.ts`, [landedView]).
  const view = landed === undefined ? "settled" : landedView(landed, projected, editing.health);
  const shown = landed !== undefined && view === "applying" ? landed.value : projected;
  const open = session !== undefined;
  const blocked = editing.access.can ? undefined : editing.access.reason;
  const empty = isEmptyPart(shown);

  return {
    shown,
    // THE WORDS ALONE: the card sets its note as the caption under its body
    // (`../PrincipleCard.tsx`), and a paragraph of its own here was a
    // paragraph inside that one.
    note:
      view === "applying"
        ? "Saved. This page shows it as soon as this node applies the new revision."
        : view === "refused"
          ? "Saved, but this node refused the new revision and goes on running the one before. Settings › Nodes says why."
          : null,
    actions:
      editing.mayEdit && !open && !empty ? (
        <Button
          ref={editing.triggerRef(part)}
          size="small"
          variant="ghost"
          leadingIcon={<PencilGlyph size="sm" />}
          aria-label={`Edit ${partName(part)}`}
          disabledReason={blocked}
          title={blocked}
          onClick={() => editing.open(part, "edit")}
        >
          Edit
        </Button>
      ) : null,
    add:
      editing.mayEdit && !open ? (
        <Button
          ref={empty ? editing.triggerRef(part) : undefined}
          size="small"
          variant="secondary"
          leadingIcon={<PlusGlyph size="sm" />}
          disabledReason={blocked}
          title={blocked}
          onClick={() => editing.open(part, "add")}
        >
          {ADD_LABELS[part]}
        </Button>
      ) : null,
    // NON-NULL IS EDITING: the card draws it in its body's place, its own
    // Save and Cancel included (`PartEditor.tsx`).
    editor: open ? <PartEditor part={part} editing={editing} /> : null,
  };
}
