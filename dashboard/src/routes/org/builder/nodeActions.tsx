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

import type { ReactNode } from "react";
import { seatPath } from "~/lib/seats.ts";
import type { AddKind, BuilderApi } from "./BuilderContext.tsx";
import type { NodeView, SeatView, Structure, UnitView } from "./chartModel.ts";
import { COMPANY_KEY, type NodeKey } from "./model/keys.ts";
import { CrewletIcon } from "@crewlethq/icons";
import {
  NetworkGlyph,
  ArrowUpRightGlyph,
  TrashGlyph,
  PencilGlyph,
  FolderInputGlyph,
  UserGlyph,
  BotGlyph,
} from "@crewlethq/icons/glyphs";
import { Kbd, isApplePlatform, type AddPillSection, type MenuEntry } from "@crewlethq/ui";

/** Where "Open seat" goes: the seat's own screen. */
export type OpenScreen = (path: string[]) => void;

/*
 * WHAT A NODE'S KEYS ASK FOR is the design system's `treeItemAction`, not this
 * module's. It was written out here as well, character for character, when the
 * chart and the outline each had their own copy of the tree pattern; the chart
 * is `TreeCanvas` now and the outline is a table, so the one reading of Enter,
 * Delete, Backspace, ContextMenu and Shift+F10 is the package's.
 */

/** Whether a node can be deleted: every seat and unit, never the company. */
export function isDeletable(view: NodeView): boolean {
  return view.type !== "company";
}

const deleteKey = () => (isApplePlatform() ? "Backspace" : "Delete");

/**
 * WHAT CAN GO UNDER A PARENT, once, for the three surfaces that ask.
 *
 * The node's menu, the toolbar and the pill on the branch below a node are
 * three drawings of ONE list, so the kind, the word for it and the mark it
 * wears are decided here rather than three times. The marks are the ones the
 * chart draws on the nodes themselves, so what a reader presses to add an
 * agent seat carries the mark every agent seat in the chart wears.
 */
const ADD_CHOICES: readonly { kind: AddKind; label: string; icon: ReactNode }[] = [
  { kind: "unit", label: "Add unit", icon: <NetworkGlyph /> },
  { kind: "agent", label: "Add agent seat", icon: <CrewletIcon /> },
  { kind: "human", label: "Add human seat", icon: <UserGlyph /> },
];

/** Where an add under `parent` lands: the company root is `null`. */
const addTarget = (parent: NodeKey) => (parent === COMPANY_KEY ? null : parent);

function addEntries(api: BuilderApi, parent: NodeKey): MenuEntry[] {
  const at = addTarget(parent);
  return ADD_CHOICES.map((choice) => ({
    key: `add-${choice.kind}`,
    label: choice.label,
    icon: choice.icon,
    disabled: api.readOnly,
    onSelect: () => api.openAdd(at, choice.kind),
  }));
}

/** The Add menu of the company or a unit. */
export function addMenu(api: BuilderApi, view: NodeView): MenuEntry[] {
  return view.type === "seat" ? [] : addEntries(api, view.key);
}

/**
 * The same list as the sections of the pill on a node's branch.
 *
 * NAMED AFTER THE NODE, because a chart draws one of these per branch: nine
 * sections called "Add unit" are nine controls a reader tells apart by looking
 * at where they are, and the name is the tooltip as well.
 */
export function addSections(api: BuilderApi, view: NodeView): AddPillSection[] {
  if (view.type === "seat") return [];
  const at = addTarget(view.key);
  const name = view.name || "the company";
  return ADD_CHOICES.map((choice) => ({
    key: choice.kind,
    label: `${choice.label} to ${name}`,
    icon: choice.icon,
    disabled: api.readOnly,
    onSelect: () => api.openAdd(at, choice.kind),
  }));
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
    icon: <PencilGlyph />,
    hint: <Kbd keys={["Enter"]} />,
    onSelect: () => api.openEditor(view.key),
  };
  const remove: MenuEntry = {
    key: "delete",
    label: "Delete",
    icon: <TrashGlyph />,
    danger: true,
    disabled: api.readOnly,
    hint: <Kbd keys={[deleteKey()]} />,
    onSelect: () => api.openDelete(view.key),
  };
  const move: MenuEntry = {
    key: "move",
    label: "Move to",
    icon: <FolderInputGlyph />,
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
      icon: <ArrowUpRightGlyph />,
      onSelect: () => open(screen),
    });
  }
  return [
    ...entries,
    {
      key: "reports",
      label: "Edit reports",
      icon: <NetworkGlyph />,
      onSelect: () => api.openEditor(view.key, "reports"),
    },
    {
      key: "kind",
      label: view.kind === "human" ? "Change to agent seat" : "Change to human seat",
      icon: view.kind === "human" ? <BotGlyph /> : <UserGlyph />,
      disabled: api.readOnly,
      onSelect: () => api.openChangeKind(view.key),
    },
    move,
    { kind: "separator", key: "sep-delete" },
    remove,
  ];
}

/**
 * WHAT A SURFACE ALREADY OFFERS WITHOUT ITS MENU, by the keys the entries
 * above carry. A key rather than a label, because a label is what a menu SAYS
 * and two of these say different things on different nodes ("Change to human
 * seat"), and because a renamed label would silently stop matching and put the
 * entry back on the surface twice.
 *
 * THE TWO LISTS DIFFER BY THE THREE ADDS, and by exactly one thing: whether
 * the reader reading the menu can reach the control that draws them.
 *
 * - The chart is a `tree`, and a tree item may hold no tab stop of its own, so
 *   every control on a card is pointer-only. Edit and Delete are still reached
 *   by a key (Enter, Delete), so a menu carrying them offers a second way to
 *   do what the reader already has one for. The add pill has no key of its
 *   own, so its three kinds stay in the menu: taken out they would be pointer-
 *   only on that surface, and a tree's menu is where the pattern puts a per-
 *   item control anyway.
 * - The table is a `treegrid`, whose cell-entry model reaches every control in
 *   a row, the add pill included (`grid.opened`). So all five come out.
 *
 * Both lists take Edit and Delete out, which is the duplication the console
 * has none of: a card's menu opened with an Edit drawn two inches to its left
 * and ended with a Delete beside it.
 */
const REACHED_ON_A_CARD = new Set(["edit", "delete"]);
const REACHED_ON_A_ROW = new Set(["add-unit", "add-agent", "add-human", "edit", "delete"]);

/** The chart's own: a card draws the pencil and the trash, and keys reach both. */
export function cardMenu(api: BuilderApi, view: NodeView, open: OpenScreen): MenuEntry[] {
  return surfaceMenu(api, view, open, REACHED_ON_A_CARD);
}

/** The table's own: a row draws the add pill as well, and the grid reaches it. */
export function rowMenu(api: BuilderApi, view: NodeView, open: OpenScreen): MenuEntry[] {
  return surfaceMenu(api, view, open, REACHED_ON_A_ROW);
}

/**
 * The menu of a node on a surface that draws some of its actions itself: every
 * action the surface does not already offer.
 *
 * ONE SUBTRACTION FOR BOTH, because the question is the same one and the two
 * answers drifted when they were written out separately: the card's menu kept
 * an Edit and a Delete it had drawn itself, so one node read two ways on two
 * views of one draft. The toolbar draws no control of its own and therefore
 * keeps `nodeMenu` whole.
 *
 * THERE IS NO MOVE UP OR MOVE DOWN. The chart keeps no order among siblings —
 * a unit's seats and child units are served by address and stored in no other
 * order (`model/draft.Placement`) — so a move among them would be an
 * arrangement the save silently drops. Where a node sits is its parent, and
 * Move to is how that changes.
 *
 * SEPARATORS ARE PART OF THE SUBTRACTION. `nodeMenu` groups its entries with
 * rules, and taking the adds and Delete out of a unit's menu left a rule at
 * the top, a rule at the bottom and two in a row in the middle: a menu of
 * three actions drawn as five. One survives only between two actions.
 */
function surfaceMenu(
  api: BuilderApi,
  view: NodeView,
  open: OpenScreen,
  reached: ReadonlySet<string>,
): MenuEntry[] {
  const kept = nodeMenu(api, view, open).filter(
    (entry) => entry.kind === "separator" || !reached.has(entry.key),
  );
  const menu: MenuEntry[] = [];
  for (const entry of kept) {
    if (entry.kind !== "separator") {
      menu.push(entry);
      continue;
    }
    if (menu.length > 0 && menu[menu.length - 1]!.kind !== "separator") menu.push(entry);
  }
  if (menu.length > 0 && menu[menu.length - 1]!.kind === "separator") menu.pop();
  return menu;
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
      icon: <NetworkGlyph />,
      hint: <Kbd keys={["Enter"]} />,
      onSelect: () => api.openEditor(view.key, "reports"),
    },
  ];
  if (screen) {
    entries.push({
      key: "open",
      label: "Open seat",
      icon: <ArrowUpRightGlyph />,
      onSelect: () => open(screen),
    });
  }
  return entries;
}

/** What a lead chip says: the lead, and whether it is inherited. */
export function leadLabel(unit: UnitView): string {
  if (unit.lead === null) return "No lead";
  return unit.lead.inherited ? `${unit.lead.name} (inherited)` : unit.lead.name;
}

/**
 * The same, as the CHART's pill writes it: the word and the name together.
 *
 * TWO LABELS, BECAUSE TWO SURFACES ASK DIFFERENT QUESTIONS. A table cell sits
 * under a column already headed "Lead", so the name alone is the answer and a
 * prefix would write the word twice on one row. A chart node's pill stands on
 * its own along the bottom edge of a card, with nothing above it saying what
 * the name IS: the console chart this is drawn from writes "Lead: Ada" there
 * for exactly that reason, and "Lead" in a pill with nothing in it.
 */
export function leadChipLabel(unit: UnitView): string {
  if (unit.lead === null) return "Lead";
  return `Lead: ${leadLabel(unit)}`;
}

/** What a unit's treeitem says about its lead to a screen reader: one sentence. */
export function leadSentence(unit: UnitView): string {
  if (unit.lead === null) return "No lead.";
  return `Lead: ${leadLabel(unit)}.`;
}

/**
 * The answers of a unit's lead choice, made in place: no lead (saying what
 * the unit then inherits), then each seat drawn in the unit, then the lead it
 * declares when that seat is elsewhere. A lead may name any seat, so the last
 * entry sends the operator to the editor to choose one outside the unit.
 *
 * BY HANDLE, as the chart stores a lead: a name is prose two seats may share,
 * so two members called the same are told apart by their handles rather than
 * folded into one answer that could only ever pick the first.
 *
 * CHOOSING THE CURRENT ANSWER CHANGES NOTHING AND SAYS NOTHING. The checked
 * answer only closes the menu, as a radio button already on does nothing:
 * recording it would be refused ("The lead is unchanged.") and the refusal
 * announced, for a choice that is exactly what the operator sees.
 */
export function leadMenu(api: BuilderApi, structure: Structure, unit: UnitView): MenuEntry[] {
  const declared = unit.lead && !unit.lead.inherited ? unit.lead.handle : null;
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
  const nameCount = new Map<string, number>();
  for (const m of members) nameCount.set(m.name, (nameCount.get(m.name) ?? 0) + 1);
  const labelOf = (m: SeatView) =>
    m.name === ""
      ? `@${m.handle}`
      : nameCount.get(m.name)! > 1
        ? `${m.name} (@${m.handle})`
        : m.name;
  const entries: MenuEntry[] = [
    answer(
      "none",
      unit.inheritable ? `No lead (inherits ${unit.inheritable.name})` : "No lead",
      null,
    ),
    ...members.map((m) => answer(`seat:${m.handle}`, labelOf(m), m.handle)),
  ];
  // A declared lead drawn nowhere in the unit (a seat elsewhere, or a handle
  // no seat holds) is still the current answer, and says so.
  if (declared !== null && !members.some((m) => m.handle === declared)) {
    entries.push(answer("declared", unit.lead!.name, declared));
  }
  entries.push(
    { kind: "separator", key: "sep" },
    {
      key: "other",
      label: "Choose another seat",
      icon: <PencilGlyph />,
      disabled: api.readOnly,
      onSelect: () => api.openEditor(unit.key, "leadership"),
    },
  );
  return entries;
}
