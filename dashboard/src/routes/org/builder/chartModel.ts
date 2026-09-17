/**
 * What the builder's canvas and outline draw, as data: the structure chart and
 * the reporting chart of the draft.
 *
 * ONE READING FOR BOTH VIEWS, AND FOR WHAT THEY OPEN. The canvas draws cards
 * and the outline draws rows, but "which unit is this seat drawn in", "who
 * leads this unit" and "who does this seat report to" must have one answer,
 * or the two views of one draft disagree. So both read this module, and so do
 * the editor and the dialogs for two facts a card also shows: the seat a
 * reported handle names, and the Datadog fallback. The third, a seat's
 * handle, is the model's (`document.knownHandles`), because the reducer
 * records operations by it; this module reads it there like every other
 * surface. It is pure: no React, no DOM, tested in a node environment.
 *
 * THE DOCUMENT GIVES THE SHAPE, THE ENGINE GIVES THE MEANING. Units, their
 * seats and their children are drawn as the draft holds them, because that is
 * what an operation edits. Everything the engine derives (a root seat placed
 * in a unit by its `unit:` reference, the lead a unit inherits, a seat's
 * primary manager, the reporting forest) comes from the last check's
 * `derived` block, never from a second implementation of the engine's rules.
 *
 * A DERIVATION IS READ THROUGH THE DOCUMENT IT DESCRIBES. Its paths are paths
 * in the document that check was sent, so they are turned into node keys with
 * that document's own path index (`state.check.sent`), never the draft's: the
 * operator may have moved a node since, and `units[1]` may name a different
 * unit now. And because the draft may have changed since that check, a
 * derived fact is used only while the fields it was derived from still hold
 * the values the check saw. A root seat is drawn in the unit its reference
 * resolved to only while it still names that unit by its current name; an
 * inherited lead is shown only while the unit still declares none and every
 * unit above it still sits and declares as it did, because the engine hands
 * a unit its parent's lead, and a unit moved under another parent inherits
 * another one. A seat's primary manager follows from the whole organization
 * (every `manages` list with its unit references expanded, every lead and
 * the order the engine walks seats in), so no field says it still holds: it
 * is shown only from a check of the draft as it stands. When the draft has
 * moved past what the check saw, the fact is left out until the next check
 * answers, rather than shown stale.
 *
 * A CHECK THAT IS OUT MOVES NO CARD. The canvas lays cards out by their
 * measured size, and every edit sends a check. So a mark on a card that
 * comes from the engine (a reference that names nothing) is held to the same
 * rule as a placement: it stands while the node still writes what the last
 * check warned about, whatever generation that check was, rather than
 * vanishing with every edit and coming back a moment later.
 */

import type { CompanyDocument, ConfigWarning, DerivedSeat, DerivedUnit } from "~/protocol/index.ts";
import type { TreeInput } from "@crewlethq/ui";
import { checkedDocument, type BuilderState } from "./model/reducer.ts";
import { COMPANY_KEY, handleOfKey, type NodeKey } from "./model/keys.ts";
import {
  allUnits,
  locate,
  seatNames as draftSeatNames,
  type Draft,
  type DraftSeat,
} from "./model/draft.ts";
import {
  DATADOG_ROUTE_TO,
  knownHandles,
  NO_DERIVATION,
  nodeDataIn,
  pathOfSegments,
  placeDerivation,
  type CheckedDocument,
} from "./model/document.ts";
import { getPath, isRecord } from "./model/json.ts";
import { kindOf, type SeatKind } from "./model/operations.ts";
import { reportingForest, type ReportingNode } from "./model/reporting.ts";

/** What a chart is drawn from. */
export interface ChartInputs {
  readonly draft: Draft;
  /** The draft of the saved company: which seats exist and run today. */
  readonly baseDraft: Draft;
  /** The document the last check was sent and its derivation, when it answered with one. */
  readonly checked: CheckedDocument | null;
  /** Whether that derivation describes the draft as it stands: no change since that check. */
  readonly current: boolean;
  /** The warnings the last check placed on each node, whatever generation it answered. */
  readonly warnings: ReadonlyMap<NodeKey, readonly ConfigWarning[]>;
}

/**
 * The chart inputs of a builder state. The check's own document and
 * derivation travel together, because the one is only readable through the
 * other (see the module doc).
 */
export function chartInputs(
  state: Pick<BuilderState, "draft" | "baseDraft" | "check" | "generation">,
): ChartInputs {
  const checked = checkedDocument(state.check);
  const warnings = new Map<NodeKey, ConfigWarning[]>();
  for (const [key, placed] of state.check.problems.byNode) {
    const found = placed.filter((p) => p.severity === "warning");
    if (found.length > 0)
      warnings.set(
        key,
        found.map((p) => p.source as ConfigWarning),
      );
  }
  return {
    draft: state.draft,
    baseDraft: state.baseDraft,
    checked,
    current: checked !== null && state.check.generation === state.generation,
    warnings,
  };
}

/** A unit's lead as the chart shows it. */
export interface LeadView {
  /** The seat name, as a lead is written. */
  readonly name: string;
  /** Inherited from an ancestor unit rather than declared by this one. */
  readonly inherited: boolean;
}

export interface CompanyView {
  readonly type: "company";
  readonly key: NodeKey;
  readonly name: string;
  /** Root seats drawn at the root: every one not placed in a unit by reference. */
  readonly seats: readonly NodeKey[];
  readonly units: readonly NodeKey[];
}

export interface UnitView {
  readonly type: "unit";
  readonly key: NodeKey;
  readonly name: string;
  /**
   * The type as written; else the engine's effective type, while the check
   * saw the unit write none either; else "".
   */
  readonly unitType: string;
  /** Declared or inherited; `null` when it has none, `undefined` until a check says what it inherits. */
  readonly lead: LeadView | null | undefined;
  /**
   * What the unit would have with no lead of its own, marked inherited: its
   * parent's lead when it declares one, the lead it inherits when it does
   * not. `null` when that is nothing, or not known.
   */
  readonly inheritable: LeadView | null;
  /**
   * The lead the unit declares that the last check found no seat for, while
   * the unit still declares it and no seat of that name has been added since.
   */
  readonly danglingLead: string | null;
  /** The engine's sentence about `danglingLead`, when that check gave one. */
  readonly danglingNote: string | undefined;
  /** Seats drawn inside the unit: its own, then the root seats placed in it. */
  readonly seats: readonly NodeKey[];
  readonly units: readonly NodeKey[];
  readonly parent: NodeKey;
}

export interface SeatView {
  readonly type: "seat";
  readonly key: NodeKey;
  readonly name: string;
  readonly kind: SeatKind;
  /**
   * The handle the engine runs it under: declared, carried by its key, or
   * reported by the last check while the seat is still called what that
   * check saw. `undefined` while no check has said.
   */
  readonly handle: string | undefined;
  /**
   * The seat as the saved company holds it: the handle and name its own
   * screen and its live state are found by, which a rename or a handle chosen
   * in this draft has not changed yet. `null` for a seat this draft created.
   */
  readonly saved: { readonly handle: string | undefined; readonly name: string } | null;
  /** A saved agent seat that is still an agent seat in the draft: it has a live state to show. */
  readonly running: boolean;
  /** A root seat the engine placed in the unit it is drawn in, by its `unit:` reference. */
  readonly placedByRef: boolean;
  /** The `unit:` a root seat writes that the engine resolved to no unit. */
  readonly danglingUnitRef: string | null;
  /** The engine's sentence about `danglingUnitRef`, when the last check gave one. */
  readonly danglingNote: string | undefined;
  /** The seat an alert that names nobody wakes (`integrations.datadog.route_to`, while enabled). */
  readonly datadogFallback: boolean;
  /**
   * Its primary manager's name; `null` for none; `undefined` until a check
   * of the draft as it stands says (see the module doc).
   */
  readonly manager: string | null | undefined;
  /** The node it is drawn under: the company or a unit. */
  readonly parent: NodeKey;
}

export type NodeView = CompanyView | UnitView | SeatView;

export interface Structure {
  readonly nodes: ReadonlyMap<NodeKey, NodeView>;
  /** The company, its seats and units, as the tree model reads a forest. */
  readonly tree: readonly TreeInput[];
}

// ---------------------------------------------------------------------------
// What every surface reads about a node
// ---------------------------------------------------------------------------

/**
 * The key of the seat of the draft the last check gave `handle`, for reading
 * a handle the engine reported (a manager, a lead, an automatic report) as a
 * node. `undefined` when that check named no such seat or the seat is gone.
 * Whether the fact the handle came from still holds is the caller's to judge:
 * a manager only from a check of the draft as it stands, a lead while the
 * chain above it is as checked.
 */
export function keyOfHandle(
  state: Pick<BuilderState, "draft" | "check">,
  handle: string,
): NodeKey | undefined {
  const checked = checkedDocument(state.check);
  if (!checked) return undefined;
  const key = placeDerivation(checked.sent.index, checked.derived).keyOfHandle.get(handle);
  return key !== undefined && locate(state.draft, key)?.kind === "seat" ? key : undefined;
}

/**
 * The handle of the seat an alert that names nobody wakes, or `undefined`.
 *
 * ONLY WHILE DATADOG IS ENABLED: the engine builds no Datadog parser
 * otherwise and refuses every alert at the route, and it validates
 * `route_to` only for an enabled block, so a `route_to` left in a disabled
 * block names a seat nothing will ever wake and nothing requires. The engine
 * trims what it reads (`config.Company.Validate`), and so does this, or a
 * value written with a space would name no seat here while it names one to
 * the engine.
 */
export function datadogFallback(company: CompanyDocument): string | undefined {
  if (!datadogEnabled(company)) return undefined;
  const routeTo = getPath(company, DATADOG_ROUTE_TO);
  const handle = typeof routeTo === "string" ? routeTo.trim() : "";
  return handle === "" ? undefined : handle;
}

/**
 * Whether the company's Datadog block is switched on, which is the only time
 * the engine builds its parser and routes an alert to anybody (`enabled` is a
 * plain boolean whose absence is off, `config.Datadog.Enabled`).
 */
export function datadogEnabled(company: CompanyDocument): boolean {
  return getPath(company, [...DATADOG_ROUTE_TO.slice(0, -1), "enabled"]) === true;
}

/** The engine's facts about the last checked document, by node key. */
interface Engine {
  readonly seatByKey: ReadonlyMap<NodeKey, DerivedSeat>;
  readonly unitByKey: ReadonlyMap<NodeKey, DerivedUnit>;
  readonly keyOfHandle: ReadonlyMap<string, NodeKey>;
  /** The handle each seat of the draft runs under, where known (`document.knownHandles`). */
  readonly handles: ReadonlyMap<NodeKey, string>;
  /** The node's JSON in the document that was checked. */
  readonly sentData: (key: NodeKey) => Record<string, unknown> | undefined;
  /** The node it sat under in the document that was checked: a unit, the company, or unknown. */
  readonly sentParent: (key: NodeKey) => NodeKey | undefined;
  readonly checked: CheckedDocument | null;
}

function engineOf({ draft, checked }: ChartInputs): Engine {
  const placed = checked ? placeDerivation(checked.sent.index, checked.derived) : NO_DERIVATION;
  return {
    ...placed,
    handles: knownHandles(draft, checked),
    sentData: (key) => (checked ? nodeDataIn(checked.sent, key) : undefined),
    sentParent: (key) => {
      // `units[i]` and `roles[i]` sit under the company; anything deeper sits
      // under the unit two segments up (`...children[i]`, `...roles[i]`).
      const segments = checked?.sent.index.segmentsOf.get(key);
      if (!checked || !segments || segments.length < 2) return undefined;
      if (segments.length === 2) return COMPANY_KEY;
      return checked.sent.index.byPath.get(pathOfSegments(segments.slice(0, -2)));
    },
    checked,
  };
}

const text = (value: unknown): string =>
  typeof value === "string" && value.trim() !== "" ? value : "";

/** Builds the structure chart of a draft. */
export function structure(inputs: ChartInputs): Structure {
  const { draft, baseDraft } = inputs;
  const engine = engineOf(inputs);
  const nodes = new Map<NodeKey, NodeView>();
  const routeTo = datadogFallback(draft.company);

  const unitName = new Map<NodeKey, string>();
  for (const { unit } of allUnits(draft)) unitName.set(unit.key, unit.data.name);
  const unitNames = new Set(unitName.values());
  const seatNames = new Set(draftSeatNames(draft));
  /** The last check's warning about one of a node's references, by the reference it names. */
  const warningOn = (key: NodeKey, ref: ConfigWarning["ref"]) =>
    inputs.warnings.get(key)?.find((w) => w.kind === "dangling_reference" && w.ref === ref);

  // WHERE A ROOT SEAT IS DRAWN. The engine says whether its reference placed
  // it, and in which unit; the draft says whether that is still true. A
  // reference the check found no unit for is dangling only while the draft
  // still holds none of that name: a unit added or renamed to it since is
  // one the engine will place the seat in, which the next check says.
  const placedIn = new Map<NodeKey, NodeKey[]>();
  const rootSeats: NodeKey[] = [];
  const placement = new Map<NodeKey, { unit: NodeKey | null; dangling: string | null }>();
  for (const seat of draft.roles) {
    const ref = text(seat.data.unit);
    const derived = engine.seatByKey.get(seat.key);
    const checked = engine.sentData(seat.key);
    let unit: NodeKey | null = null;
    let dangling: string | null = null;
    if (ref !== "" && derived) {
      if (derived.placed_by_ref && derived.unit_path) {
        const target = engine.checked?.sent.index.byPath.get(derived.unit_path);
        if (target !== undefined && unitName.get(target) === ref) unit = target;
      } else if (
        !derived.placed_by_ref &&
        checked &&
        text(checked.unit) === ref &&
        !unitNames.has(ref)
      ) {
        dangling = ref;
      }
    }
    placement.set(seat.key, { unit, dangling });
    if (unit === null) rootSeats.push(seat.key);
    else placedIn.set(unit, [...(placedIn.get(unit) ?? []), seat.key]);
  }

  // A PRIMARY MANAGER IS READ ONLY FROM A CHECK OF THIS VERY DRAFT. Any edit
  // (a manages list, a lead, a move, a reorder, a rename a reference follows)
  // can change it, and deciding which ones do would be the engine's rules
  // written again, so until the next check answers it is unknown.
  const managerName = (key: NodeKey): string | null | undefined => {
    const derived = inputs.current ? engine.seatByKey.get(key) : undefined;
    if (!derived) return undefined;
    if (!derived.manager) return null;
    const managerKey = engine.keyOfHandle.get(derived.manager);
    const found = managerKey === undefined ? undefined : locate(draft, managerKey);
    return found?.kind === "seat" ? found.node.data.name : undefined;
  };

  const seatView = (seat: DraftSeat, parent: NodeKey): SeatView => {
    const kind = kindOf(seat.data);
    const handle = engine.handles.get(seat.key);
    const base = locate(baseDraft, seat.key);
    const savedSeat =
      base?.kind === "seat"
        ? {
            handle: text(base.node.data.handle) || handleOfKey(seat.key),
            name: base.node.data.name,
          }
        : null;
    const running = base?.kind === "seat" && kindOf(base.node.data) === "agent" && kind === "agent";
    const placed = placement.get(seat.key);
    return {
      type: "seat",
      key: seat.key,
      name: seat.data.name,
      kind,
      handle,
      saved: savedSeat,
      running,
      placedByRef: (placed?.unit ?? null) !== null,
      danglingUnitRef: placed?.dangling ?? null,
      danglingNote: placed?.dangling ? warningOn(seat.key, "unit")?.message : undefined,
      datadogFallback: routeTo !== undefined && handle === routeTo,
      manager: managerName(seat.key),
      parent,
    };
  };

  /**
   * The lead a unit that declares none has, as the engine derived it: `null`
   * for none, `undefined` while unknown.
   *
   * A LEAD IS INHERITED DOWN A CHAIN, so the derivation of it holds only while
   * every unit on that chain still sits where the check saw it and declares
   * the lead the check saw. Change an ancestor's lead, or move a unit under
   * another parent, and every unit below inherits something the check never
   * saw, which the next check reports.
   */
  const inheritedLead = (key: NodeKey, chainAsChecked: boolean): LeadView | null | undefined => {
    const derived = engine.unitByKey.get(key);
    if (!derived || !chainAsChecked) return undefined;
    if (!derived.lead || !derived.lead_inherited) return null;
    const leadKey = engine.keyOfHandle.get(derived.lead);
    const found = leadKey === undefined ? undefined : locate(draft, leadKey);
    // The inherited lead removed since the check: what the unit inherits now
    // is the next check's to say.
    return found?.kind === "seat" ? { name: found.node.data.name, inherited: true } : undefined;
  };

  const tree: TreeInput[] = [];
  const visitUnits = (
    units: Draft["units"],
    parent: NodeKey,
    parentLead: LeadView | null | undefined,
    parentAsChecked: boolean,
  ): TreeInput[] =>
    units.map((unit) => {
      const declared = text(unit.data.lead);
      const checked = engine.sentData(unit.key);
      const asChecked =
        parentAsChecked &&
        checked !== undefined &&
        engine.sentParent(unit.key) === parent &&
        text(checked.lead) === declared;
      const lead: LeadView | null | undefined =
        declared !== "" ? { name: declared, inherited: false } : inheritedLead(unit.key, asChecked);
      // A LEAD THAT NAMES NO SEAT is marked while the unit still declares the
      // lead the check warned about and no seat of that name has come since,
      // which the next check would resolve.
      const leadWarning = warningOn(unit.key, "lead");
      const danglingLead =
        leadWarning &&
        declared !== "" &&
        checked !== undefined &&
        text(checked.lead) === declared &&
        !seatNames.has(declared)
          ? declared
          : null;
      // With a lead of its own, the unit would inherit its parent's; without
      // one, it already does.
      const source = declared !== "" ? parentLead : lead;
      const inheritable: LeadView | null = source ? { name: source.name, inherited: true } : null;
      const seats = [...unit.roles.map((s) => s.key), ...(placedIn.get(unit.key) ?? [])];
      const own = unit.roles.map((s) => seatView(s, unit.key));
      const placed = (placedIn.get(unit.key) ?? []).map((k) =>
        seatView(
          draft.roles.find((s) => s.key === k)!,
          unit.key,
        ),
      );
      nodes.set(unit.key, {
        type: "unit",
        key: unit.key,
        name: unit.data.name,
        // The engine's type stands in for one the unit does not write only
        // while the check saw none written either: a type cleared since is
        // not the type that check reported.
        unitType:
          text(unit.data.type) ||
          (checked !== undefined && text(checked.type) === ""
            ? (engine.unitByKey.get(unit.key)?.type ?? "")
            : ""),
        lead,
        inheritable,
        danglingLead,
        danglingNote: danglingLead !== null ? leadWarning?.message : undefined,
        seats,
        units: unit.children.map((c) => c.key),
        parent,
      });
      const seatItems = [...own, ...placed].map((view): TreeInput => {
        nodes.set(view.key, view);
        return { id: view.key, label: view.name };
      });
      return {
        id: unit.key,
        label: unit.data.name,
        children: [...seatItems, ...visitUnits(unit.children, unit.key, lead, asChecked)],
      };
    });

  const companySeats = rootSeats.map((key): TreeInput => {
    const view = seatView(
      draft.roles.find((s) => s.key === key)!,
      COMPANY_KEY,
    );
    nodes.set(key, view);
    return { id: key, label: view.name };
  });
  const companyName = text(draft.company.name);
  nodes.set(COMPANY_KEY, {
    type: "company",
    key: COMPANY_KEY,
    name: companyName,
    seats: rootSeats,
    units: draft.units.map((u) => u.key),
  });
  tree.push({
    id: COMPANY_KEY,
    label: companyName || "Company",
    children: [...companySeats, ...visitUnits(draft.units, COMPANY_KEY, null, true)],
  });
  return { nodes, tree };
}

// ---------------------------------------------------------------------------
// The reporting chart
// ---------------------------------------------------------------------------

/** The id of the group holding every reporting cycle. */
export const CYCLE_GROUP = "reporting:cycles";

/** One seat in the reporting chart. */
export interface ReportingItem {
  /** The node key when the seat is in the draft that was checked, else a position id. */
  readonly id: string;
  /** The seat's node key; `null` when the derivation names a seat no key reaches. */
  readonly key: NodeKey | null;
  readonly name: string;
  readonly kind: SeatKind;
  /**
   * The handle, read as the structure chart reads it for a seat still in the
   * draft (`undefined` while no check has reported it), else the one the
   * derivation carries.
   */
  readonly handle: string | undefined;
  /** No manager: a top of the forest. */
  readonly root: boolean;
  /** How many seats the cycle it belongs to holds; absent outside a cycle. */
  readonly cycleSize?: number;
  readonly reports: readonly ReportingItem[];
}

export interface Reporting {
  /** Whether the engine has described this draft's reporting lines at all. */
  readonly known: boolean;
  readonly roots: readonly ReportingItem[];
  readonly cycles: readonly ReportingItem[];
  /** The roots, then the cycle group when there is one, as the tree model reads them. */
  readonly tree: readonly TreeInput[];
  readonly items: ReadonlyMap<string, ReportingItem>;
}

/**
 * Builds the reporting chart from the last check's derivation, arranged by
 * the model's forest (primary managers, no-manager roots, the cycle group).
 * A seat's name and handle are its CURRENT ones in the draft, so a rename
 * since the check is not shown stale on a line that has not changed.
 */
export function reporting(inputs: ChartInputs): Reporting {
  const { checked, draft } = inputs;
  const engine = engineOf(inputs);
  const items = new Map<string, ReportingItem>();
  if (!checked) {
    return { known: false, roots: [], cycles: [], tree: [], items };
  }
  const { sent, derived } = checked;
  const forest = reportingForest(derived);
  const convert = (node: ReportingNode, root: boolean): ReportingItem => {
    const key = node.seat.path === undefined ? undefined : sent.index.byPath.get(node.seat.path);
    const found = key === undefined ? undefined : locate(draft, key);
    const current = found?.kind === "seat" ? found.node : undefined;
    const item: ReportingItem = {
      id: key ?? `reporting:${node.index}`,
      key: current ? key! : null,
      name: current?.data.name ?? node.seat.name,
      kind: current ? kindOf(current.data) : node.seat.kind === "human" ? "human" : "agent",
      handle: current ? engine.handles.get(current.key) : node.seat.handle,
      root,
      ...(node.cycle ? { cycleSize: node.cycle.length } : {}),
      reports: node.reports.map((r) => convert(r, false)),
    };
    items.set(item.id, item);
    return item;
  };
  const roots = forest.roots.map((n) => convert(n, true));
  const cycles = forest.cycles.map((n) => convert(n, false));
  const input = (item: ReportingItem): TreeInput => ({
    id: item.id,
    label: item.name,
    children: item.reports.map(input),
  });
  const tree: TreeInput[] = roots.map(input);
  if (cycles.length > 0) {
    tree.push({ id: CYCLE_GROUP, label: "Reporting cycle", children: cycles.map(input) });
  }
  return { known: true, roots, cycles, tree, items };
}
