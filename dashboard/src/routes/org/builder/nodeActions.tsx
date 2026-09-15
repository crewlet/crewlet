/**
 * What an operator can do to a node of the org builder, and the keys that do
 * it: one list for the canvas cards and the outline rows.
 *
 * ONE LIST, TWO SURFACES. A card's menu, a row's menu, the keyboard's
 * ContextMenu key and the toolbar that mirrors the focused node all offer the
 * same actions in the same order under the same names, so an operator who
 * learned them on the canvas finds them in the outline. Every action goes
 * through the Builder's context: an `open*` function for a decision that needs
 * a dialog, `dispatch` for a choice that is already made (a unit's lead picked
 * in place), and the navigator for the seat's own screen. Nothing here calls
 * the transport.
 *
 * READ-ONLY DISABLES, IT DOES NOT HIDE. In a guarded or read-only posture, or
 * a conflict, every action that would change the draft stays in the menu and
 * says it is unavailable (`aria-disabled`), so an operator learns what the
 * builder does and why it will not do it now, instead of meeting a menu that
 * silently lost half its entries. Opening a seat's screen changes nothing and
 * stays available.
 */

import type { KeyboardEvent } from "react";
import { seatPath } from "~/lib/seats.ts";
import type { BuilderApi } from "./BuilderContext.tsx";
import type { NodeView, SeatView, Structure, UnitView } from "./chartModel.ts";
import { COMPANY_KEY, type NodeKey } from "./model/keys.ts";
import {
  AccountTreeGlyph,
  ArrowOutwardGlyph,
  CreateNewFolderGlyph,
  DeleteGlyph,
  EditGlyph,
  MoveItemGlyph,
  PersonAddGlyph,
  PersonGlyph,
  SmartToyGlyph,
} from "@crewlethq/icons/glyphs";
import { Kbd, isApplePlatform, type MenuEntry } from "@crewlethq/ui";

/** Where "Open seat" goes: the seat's own screen. */
export type OpenScreen = (path: string[]) => void;

/** What a node's keys asked for. */
export type NodeKeyAction = "edit" | "delete" | "menu";

/**
 * The action a key press on a focused node asks for, or `null`.
 *
 * Enter edits; Delete and Backspace both delete, because the key labelled
 * Delete on a Mac keyboard sends Backspace; the ContextMenu key and Shift+F10
 * open the node's menu, which is where every other action lives. A chord
 * with Ctrl, Command or Alt is an application shortcut (Undo is the Builder's)
 * and is never a node action.
 */
export function nodeKeyAction(e: KeyboardEvent): NodeKeyAction | null {
  if (e.key === "F10" && e.shiftKey && !e.ctrlKey && !e.metaKey && !e.altKey) return "menu";
  if (e.ctrlKey || e.metaKey || e.altKey) return null;
  if (e.key === "ContextMenu") return "menu";
  if (e.shiftKey) return null;
  if (e.key === "Enter") return "edit";
  if (e.key === "Delete" || e.key === "Backspace") return "delete";
  return null;
}

/** Whether a node can be deleted: every seat and unit, never the company. */
export function isDeletable(view: NodeView): boolean {
  return view.type !== "company";
}

const deleteKey = () => (isApplePlatform() ? "Backspace" : "Delete");

function addEntries(api: BuilderApi, parent: NodeKey): MenuEntry[] {
  const at = parent === COMPANY_KEY ? null : parent;
  return [
    {
      key: "add-unit",
      label: "Add unit",
      icon: <CreateNewFolderGlyph />,
      disabled: api.readOnly,
      onSelect: () => api.openAdd(at, "unit"),
    },
    {
      key: "add-agent",
      label: "Add agent seat",
      icon: <PersonAddGlyph />,
      disabled: api.readOnly,
      onSelect: () => api.openAdd(at, "agent"),
    },
    {
      key: "add-human",
      label: "Add human seat",
      icon: <PersonAddGlyph />,
      disabled: api.readOnly,
      onSelect: () => api.openAdd(at, "human"),
    },
  ];
}

/** The Add menu of the company or a unit. */
export function addMenu(api: BuilderApi, view: NodeView): MenuEntry[] {
  return view.type === "seat" ? [] : addEntries(api, view.key);
}

/** A seat's saved screen, when the saved company has the seat. */
function seatScreen(view: SeatView): string[] | null {
  return view.saved ? seatPath({ handle: view.saved.handle ?? "", name: view.saved.name }) : null;
}

/** Every action of a node, in the order every surface offers them. */
export function nodeMenu(api: BuilderApi, view: NodeView, open: OpenScreen): MenuEntry[] {
  const edit: MenuEntry = {
    key: "edit",
    label: "Edit",
    icon: <EditGlyph />,
    hint: <Kbd keys={["Enter"]} />,
    onSelect: () => api.openEditor(view.key),
  };
  const remove: MenuEntry = {
    key: "delete",
    label: "Delete",
    icon: <DeleteGlyph />,
    danger: true,
    disabled: api.readOnly,
    hint: <Kbd keys={[deleteKey()]} />,
    onSelect: () => api.openDelete(view.key),
  };
  const move: MenuEntry = {
    key: "move",
    label: "Move to",
    icon: <MoveItemGlyph />,
    disabled: api.readOnly,
    onSelect: () => api.openMove(view.key),
  };
  if (view.type === "company") {
    return [...addEntries(api, view.key), { kind: "separator", key: "sep" }, edit];
  }
  if (view.type === "unit") {
    return [
      ...addEntries(api, view.key),
      { kind: "separator", key: "sep" },
      edit,
      move,
      { kind: "separator", key: "sep-delete" },
      remove,
    ];
  }
  const screen = seatScreen(view);
  const entries: MenuEntry[] = [edit];
  if (screen) {
    entries.push({
      key: "open",
      label: "Open seat",
      icon: <ArrowOutwardGlyph />,
      onSelect: () => open(screen),
    });
  }
  return [
    ...entries,
    {
      key: "reports",
      label: "Edit reports",
      icon: <AccountTreeGlyph />,
      onSelect: () => api.openEditor(view.key, "reports"),
    },
    {
      key: "kind",
      label: view.kind === "human" ? "Change to agent seat" : "Change to human seat",
      icon: view.kind === "human" ? <SmartToyGlyph /> : <PersonGlyph />,
      disabled: api.readOnly,
      onSelect: () => api.openChangeKind(view.key),
    },
    move,
    { kind: "separator", key: "sep-delete" },
    remove,
  ];
}

/**
 * A seat's menu on the reporting chart, which is read-only: the reporting
 * lines are the engine's derivation of `manages`, `lead` and the unit tree,
 * so they are changed where those are written, in the seat's editor.
 */
export function reportingMenu(api: BuilderApi, view: SeatView, open: OpenScreen): MenuEntry[] {
  const screen = seatScreen(view);
  const entries: MenuEntry[] = [
    {
      key: "reports",
      label: "Edit reports",
      icon: <AccountTreeGlyph />,
      hint: <Kbd keys={["Enter"]} />,
      onSelect: () => api.openEditor(view.key, "reports"),
    },
  ];
  if (screen) {
    entries.push({
      key: "open",
      label: "Open seat",
      icon: <ArrowOutwardGlyph />,
      onSelect: () => open(screen),
    });
  }
  return entries;
}

/**
 * What a lead chip says: the lead, whether it is inherited, or that it is
 * known after the check. Never that a check is running: whether one is on its
 * way (or the engine cannot be reached at all) is the toolbar's to say.
 */
export function leadLabel(unit: UnitView): string {
  if (unit.lead === undefined) return "Lead after the check";
  if (unit.lead === null) return "No lead";
  return unit.lead.inherited ? `${unit.lead.name} (inherited)` : unit.lead.name;
}

/** What a unit's treeitem says about its lead to a screen reader: one sentence. */
export function leadSentence(unit: UnitView): string {
  if (unit.lead === undefined) return "Lead after the check.";
  if (unit.lead === null) return "No lead.";
  return `Lead: ${leadLabel(unit)}.`;
}

/**
 * The answers of a unit's lead choice, made in place: no lead (saying what
 * the unit then inherits), then each seat drawn in the unit, then the lead it
 * declares when that seat is elsewhere. A lead may name any seat, so the last
 * entry sends the operator to the editor to choose one outside the unit.
 *
 * CHOOSING THE CURRENT ANSWER CHANGES NOTHING AND SAYS NOTHING. The checked
 * answer only closes the menu, as a radio button already on does nothing:
 * recording it would be refused ("The lead is unchanged.") and the refusal
 * announced, for a choice that is exactly what the operator sees.
 */
export function leadMenu(api: BuilderApi, structure: Structure, unit: UnitView): MenuEntry[] {
  const declared = unit.lead && !unit.lead.inherited ? unit.lead.name : null;
  const answer = (key: string, label: string, lead: string | null): MenuEntry => {
    const checked = declared === lead;
    return {
      key,
      label,
      checked,
      disabled: api.readOnly,
      onSelect: checked
        ? () => {}
        : () =>
            api.dispatch({
              type: "record",
              intent: { type: "setLead", target: unit.key, lead: lead ?? undefined },
            }),
    };
  };
  const members = unit.seats
    .map((key) => structure.nodes.get(key))
    .filter((v): v is SeatView => v?.type === "seat");
  const names = [...new Set(members.map((m) => m.name))];
  const entries: MenuEntry[] = [
    answer(
      "none",
      unit.inheritable ? `No lead (inherits ${unit.inheritable.name})` : "No lead",
      null,
    ),
    ...names.map((name) => answer(`seat:${name}`, name, name)),
  ];
  // A declared lead drawn nowhere in the unit (a seat elsewhere, or a name
  // no seat holds) is still the current answer, and says so.
  if (declared !== null && !names.includes(declared)) {
    entries.push(answer("declared", declared, declared));
  }
  entries.push(
    { kind: "separator", key: "sep" },
    {
      key: "other",
      label: "Choose another seat",
      icon: <EditGlyph />,
      disabled: api.readOnly,
      onSelect: () => api.openEditor(unit.key, "leadership"),
    },
  );
  return entries;
}
