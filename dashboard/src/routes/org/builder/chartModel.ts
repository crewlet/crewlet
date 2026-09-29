/**
 * What the builder's canvas and outline draw, as data: the structure chart and
 * the reporting chart of the draft.
 *
 * ONE READING FOR BOTH VIEWS, AND FOR WHAT THEY OPEN. The canvas draws cards
 * and the outline draws rows, but "which unit is this seat drawn in", "who
 * leads this unit" and "who does this seat report to" must have one answer,
 * or the two views of one draft disagree. So both read this module, and so do
 * the editor and the dialogs for two facts a card also shows: the seat a
 * derived handle names, and the Datadog fallback. It is pure: no React, no
 * DOM, tested in a node environment.
 *
 * THE DRAFT GIVES THE SHAPE, THE ENGINE GIVES THE MEANING. Units, their seats
 * and their children are drawn as the draft holds them, because that is what
 * an operation edits — and the chart's rows state where every seat sits, so
 * there is nothing about placement to derive. What the engine derives from
 * the whole organization (a seat's primary manager, the reporting forest,
 * which follow from every `manages:` list with its unit entries expanded and
 * every lead managing its unit's members) comes from the org projection's
 * `derived` block, never from a second implementation of those rules.
 *
 * ONE RULE IS RESTATED, and it is the one a unit's card cannot do without:
 * a unit that declares no lead or channel takes the one its PARENT RESOLVED
 * to (`org.propagateDownward`, one `cmp.Or` per field). It is read off the
 * draft ([effectiveLeads]), because the lead a unit inherits is what its card
 * and the dialogs about it state after every edit, and nothing derives a
 * draft to say it.
 *
 * A DERIVATION DESCRIBES THE SAVED CHART, NOT THE DRAFT. The chart has no dry
 * run: the one derivation there is, is the org push's, of the chart the engine
 * holds, and the reducer keeps it only while it describes the chart the draft
 * was made on (`document.describes`). So it is placed on the BASE draft's
 * nodes, by the handles and keys the base holds (which a draft may have
 * changed since), and it is used only while the draft's chart IS the saved
 * one: a primary manager follows from the whole organization, so no one field
 * says it still holds. Past that, the fact is said not to be derived yet,
 * rather than shown stale.
 */

import type { CompanyDocument, Derived, DerivedSeat, DerivedUnit } from "~/protocol/index.ts";
import type { TreeInput } from "@crewlethq/ui";
import type { BuilderState } from "./model/reducer.ts";
import { COMPANY_KEY, type NodeKey } from "./model/keys.ts";
import {
  addressIndex,
  locate,
  sameChart,
  type Draft,
  type DraftSeat,
  type DraftUnit,
} from "./model/draft.ts";
import {
  DATADOG_ROUTE_TO,
  NO_DERIVATION,
  placeDerivation,
  type PlacedDerivation,
} from "./model/document.ts";
import { getPath } from "./model/json.ts";
import { kindOf, type SeatKind } from "./model/operations.ts";
import { referenceWarnings, type PlacedProblem } from "./model/problems.ts";
import { reportingForest, type ReportingNode } from "./model/reporting.ts";

/** What a chart is drawn from. */
export interface ChartInputs {
  readonly draft: Draft;
  /** The draft of the saved chart: which seats exist and run today, and what the derivation describes. */
  readonly baseDraft: Draft;
  /** The engine's derivation of the saved chart, as the org push carried it; `null` while there is none. */
  readonly derivation: Derived | null;
  /** That derivation, placed on the BASE draft's nodes. */
  readonly derived: PlacedDerivation;
  /** Whether the engine has described the saved chart at all. */
  readonly known: boolean;
  /** Whether the draft's chart is still the saved one, so every derived fact holds for it. */
  readonly current: boolean;
}

/** The chart inputs of a builder state. */
export function chartInputs(
  state: Pick<BuilderState, "draft" | "baseDraft" | "base">,
): ChartInputs {
  const derived = state.base.derived;
  return {
    draft: state.draft,
    baseDraft: state.baseDraft,
    derivation: derived,
    derived: derived ? placeDerivation(state.baseDraft, derived) : NO_DERIVATION,
    known: derived !== null,
    current: derived !== null && sameChart(state.draft, state.baseDraft),
  };
}

/** A unit's lead as the chart shows it. */
export interface LeadView {
  /** The seat's handle, as a lead is written. */
  readonly handle: string;
  /** The seat's name, as a person reads it: its handle when it has none. */
  readonly name: string;
  /** Inherited from an ancestor unit rather than declared by this one. */
  readonly inherited: boolean;
}

export interface CompanyView {
  readonly type: "company";
  readonly key: NodeKey;
  readonly name: string;
  readonly seats: readonly NodeKey[];
  readonly units: readonly NodeKey[];
}

export interface UnitView {
  readonly type: "unit";
  readonly key: NodeKey;
  /** The unit's key: the address every reference names it by. */
  readonly address: string;
  readonly name: string;
  /**
   * The type as written; else the engine's effective type, while the saved
   * unit writes none either; else "".
   */
  readonly unitType: string;
  /** Declared or inherited ([effectiveLeads]); `null` when it has none. */
  readonly lead: LeadView | null;
  /**
   * What the unit would have with no lead of its own, marked inherited: its
   * parent's lead when it declares one, the lead it inherits when it does
   * not. `null` when that is nothing.
   */
  readonly inheritable: LeadView | null;
  /** The lead the unit declares that names no seat of the draft. */
  readonly danglingLead: string | null;
  /** What the draft's own check says about `danglingLead`. */
  readonly danglingNote: string | undefined;
  readonly seats: readonly NodeKey[];
  readonly units: readonly NodeKey[];
  readonly parent: NodeKey;
}

export interface SeatView {
  readonly type: "seat";
  readonly key: NodeKey;
  readonly name: string;
  readonly kind: SeatKind;
  /** The handle it is addressed by in the draft. */
  readonly handle: string;
  /**
   * The seat as the saved chart holds it: the handle and name its own screen
   * and its live state are found by, which a rename in this draft has not
   * changed yet. `null` for a seat this draft created.
   */
  readonly saved: { readonly handle: string; readonly name: string } | null;
  /** A saved agent seat that is still an agent seat in the draft: it has a live state to show. */
  readonly running: boolean;
  /** Its `manages:` entries that name no seat and no unit of the draft, as the check words them. */
  readonly danglingNotes: readonly string[];
  /** The seat an alert that names nobody wakes (`integrations.datadog.route_to`, while enabled). */
  readonly datadogFallback: boolean;
  /**
   * Its primary manager's name; `null` for none; `undefined` while the draft's
   * chart is not the saved one, or the engine has not described it.
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
 * The key of the seat a derived handle names — a manager, a lead, an
 * automatic report — for reading it as a node of the draft. Derived handles
 * are the SAVED chart's, so they are found on the base and the node read in
 * the draft by its key, which follows a handle the draft has changed.
 * `undefined` when the saved chart holds no such seat or the draft removed it.
 */
export function keyOfHandle(
  state: Pick<BuilderState, "draft" | "baseDraft" | "base">,
  handle: string,
): NodeKey | undefined {
  const key = placeDerivation(state.baseDraft, state.base.derived).keyOfHandle.get(handle);
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

const text = (value: unknown): string =>
  typeof value === "string" && value.trim() !== "" ? value : "";

/** The engine's facts about one derived node of the saved chart. */
interface Engine {
  readonly seat: (key: NodeKey) => DerivedSeat | undefined;
  readonly unit: (key: NodeKey) => DerivedUnit | undefined;
  /** The draft's seat a derived handle names; see [keyOfHandle]. */
  readonly seatOfHandle: (handle: string) => DraftSeat | undefined;
}

function engineOf({ draft, derived }: ChartInputs): Engine {
  return {
    seat: (key) => derived.seatByKey.get(key),
    unit: (key) => derived.unitByKey.get(key),
    seatOfHandle: (handle) => {
      const key = derived.keyOfHandle.get(handle);
      const found = key === undefined ? undefined : locate(draft, key);
      return found?.kind === "seat" ? found.node : undefined;
    },
  };
}

/** A seat's name for a person: its handle when it has none. */
const seatName = (seat: DraftSeat) => seat.data.name || seat.data.handle;

/** A unit's resolved lead and channel, as the engine's cascade gives them. */
export interface Resolved {
  /** The lead's HANDLE, declared or inherited; "" for none. */
  readonly lead: string;
  readonly leadInherited: boolean;
  readonly channel: string;
  readonly channelInherited: boolean;
}

/**
 * Every unit's lead and channel as the engine RESOLVES them: what the unit
 * declares, else what its parent resolved to, down every chain.
 *
 * RESTATES `org.propagateDownward` — `u.Lead = cmp.Or(u.DeclaredLead,
 * parentLead)`, and the same for the channel — which is the whole of the
 * rule: a child inherits what its parent RESOLVED to, so a lead set on a
 * division reaches a team three levels down through units that named nothing.
 * A lead is a handle as written; whether it names a seat is the reader's to
 * ask ([addressIndex]), exactly as the engine resolves it after the cascade.
 */
export function effectiveLeads(draft: Draft): ReadonlyMap<NodeKey, Resolved> {
  const out = new Map<NodeKey, Resolved>();
  const walk = (units: readonly DraftUnit[], lead: string, channel: string) => {
    for (const unit of units) {
      const ownLead = text(unit.data.lead);
      const ownChannel = text(unit.data.channel);
      const resolved: Resolved = {
        lead: ownLead || lead,
        leadInherited: ownLead === "" && lead !== "",
        channel: ownChannel || channel,
        channelInherited: ownChannel === "" && channel !== "",
      };
      out.set(unit.key, resolved);
      walk(unit.children, resolved.lead, resolved.channel);
    }
  };
  walk(draft.units, "", "");
  return out;
}

/**
 * The lead a unit resolves to, as the chart shows it: `null` for none, and for
 * an INHERITED lead naming no seat, which the engine resolves to nobody (the
 * unit that declares it carries the mark).
 */
function leadView(
  resolved: Resolved | undefined,
  seats: ReadonlyMap<string, DraftSeat>,
): LeadView | null {
  if (!resolved || resolved.lead === "") return null;
  const seat = seats.get(resolved.lead);
  if (!seat && resolved.leadInherited) return null;
  return {
    handle: resolved.lead,
    name: seat ? seatName(seat) : resolved.lead,
    inherited: resolved.leadInherited,
  };
}

/** Builds the structure chart of a draft. */
export function structure(inputs: ChartInputs): Structure {
  const { draft, baseDraft } = inputs;
  const engine = engineOf(inputs);
  const nodes = new Map<NodeKey, NodeView>();
  const routeTo = datadogFallback(draft.company);
  const { seats: seatsByAddress } = addressIndex(draft);
  const resolved = effectiveLeads(draft);

  // THE DRAFT'S OWN CHECK OF ITS REFERENCES, taken here rather than read off
  // the last check: it is a pure reading of the draft, so a mark stands for
  // exactly as long as the reference it is about, and never lags an edit.
  const warnings = new Map<NodeKey, PlacedProblem[]>();
  for (const w of referenceWarnings(draft)) {
    if (w.node !== null) warnings.set(w.node, [...(warnings.get(w.node) ?? []), w]);
  }
  const refWarnings = (key: NodeKey, ref: "lead" | "manages") =>
    (warnings.get(key) ?? []).filter((w) => "ref" in w.source && w.source.ref === ref);

  // A PRIMARY MANAGER IS READ ONLY WHILE THE DRAFT'S CHART IS THE SAVED ONE.
  // Any edit (a manages list, a lead, a move, a new address a reference
  // follows) can change it, and deciding which ones do would be the engine's
  // rules written again.
  const managerName = (key: NodeKey): string | null | undefined => {
    const derived = inputs.current ? engine.seat(key) : undefined;
    if (!derived) return undefined;
    if (!derived.manager) return null;
    const manager = engine.seatOfHandle(derived.manager);
    return manager ? seatName(manager) : undefined;
  };

  const seatView = (seat: DraftSeat, parent: NodeKey): SeatView => {
    const kind = kindOf(seat.data);
    const base = locate(baseDraft, seat.key);
    const savedSeat =
      base?.kind === "seat" ? { handle: base.node.data.handle, name: base.node.data.name } : null;
    const running = base?.kind === "seat" && kindOf(base.node.data) === "agent" && kind === "agent";
    const view: SeatView = {
      type: "seat",
      key: seat.key,
      name: seat.data.name,
      kind,
      handle: seat.data.handle,
      saved: savedSeat,
      running,
      danglingNotes: refWarnings(seat.key, "manages").map((w) => w.message),
      datadogFallback: routeTo !== undefined && seat.data.handle === routeTo,
      manager: managerName(seat.key),
      parent,
    };
    nodes.set(seat.key, view);
    return view;
  };

  const visitUnits = (
    units: readonly DraftUnit[],
    parent: NodeKey,
    parentLead: LeadView | null,
  ): TreeInput[] =>
    units.map((unit) => {
      const declared = text(unit.data.lead);
      const saved = locate(baseDraft, unit.key);
      const savedUnit = saved?.kind === "unit" ? saved.node.data : undefined;
      const lead = leadView(resolved.get(unit.key), seatsByAddress);
      const danglingLead = declared !== "" && !seatsByAddress.has(declared) ? declared : null;
      // With a lead of its own, the unit would inherit its parent's; without
      // one, it already does.
      const source = declared !== "" ? parentLead : lead;
      const inheritable: LeadView | null = source ? { ...source, inherited: true } : null;
      nodes.set(unit.key, {
        type: "unit",
        key: unit.key,
        address: unit.data.key,
        name: unit.data.name,
        // The engine's type stands in for one the unit does not write only
        // while the saved unit writes none either: a type cleared since is
        // not the type the engine reported.
        unitType:
          text(unit.data.type) ||
          (savedUnit !== undefined && text(savedUnit.type) === ""
            ? (engine.unit(unit.key)?.type ?? "")
            : ""),
        lead,
        inheritable,
        danglingLead,
        danglingNote: danglingLead !== null ? refWarnings(unit.key, "lead")[0]?.message : undefined,
        seats: unit.roles.map((s) => s.key),
        units: unit.children.map((c) => c.key),
        parent,
      });
      const seatItems = unit.roles.map((seat): TreeInput => {
        const view = seatView(seat, unit.key);
        return { id: view.key, label: view.name };
      });
      return {
        id: unit.key,
        label: unit.data.name,
        children: [...seatItems, ...visitUnits(unit.children, unit.key, lead)],
      };
    });

  const companySeats = draft.roles.map((seat): TreeInput => {
    const view = seatView(seat, COMPANY_KEY);
    return { id: view.key, label: view.name };
  });
  const companyName = text(draft.company.name);
  nodes.set(COMPANY_KEY, {
    type: "company",
    key: COMPANY_KEY,
    name: companyName,
    seats: draft.roles.map((s) => s.key),
    units: draft.units.map((u) => u.key),
  });
  const tree: TreeInput[] = [
    {
      id: COMPANY_KEY,
      label: companyName || "Company",
      children: [...companySeats, ...visitUnits(draft.units, COMPANY_KEY, null)],
    },
  ];
  return { nodes, tree };
}

// ---------------------------------------------------------------------------
// The reporting chart
// ---------------------------------------------------------------------------

/** The id of the group holding every reporting cycle. */
export const CYCLE_GROUP = "reporting:cycles";

/** One seat in the reporting chart. */
export interface ReportingItem {
  /** The node key when the seat is in the draft, else a position id. */
  readonly id: string;
  /** The seat's node key; `null` when the draft no longer holds the seat. */
  readonly key: NodeKey | null;
  readonly name: string;
  readonly kind: SeatKind;
  /** The handle: the draft's for a seat it still holds, else the one the derivation carries. */
  readonly handle: string;
  /** No manager: a top of the forest. */
  readonly root: boolean;
  /** How many seats the cycle it belongs to holds; absent outside a cycle. */
  readonly cycleSize?: number;
  readonly reports: readonly ReportingItem[];
}

export interface Reporting {
  /** Whether the engine has described the saved chart's reporting lines at all. */
  readonly known: boolean;
  /**
   * Whether those lines are the draft's own: its chart is still the saved
   * one. When it is not, the lines drawn are the saved company's.
   */
  readonly current: boolean;
  readonly roots: readonly ReportingItem[];
  readonly cycles: readonly ReportingItem[];
  /** The roots, then the cycle group when there is one, as the tree model reads them. */
  readonly tree: readonly TreeInput[];
  readonly items: ReadonlyMap<string, ReportingItem>;
}

/**
 * Builds the reporting chart from the derivation of the saved chart, arranged
 * by the model's forest (primary managers, no-manager roots, the cycle
 * group). A seat's name and handle are its CURRENT ones in the draft, so a
 * rename since is not shown stale on a line that has not changed.
 */
export function reporting(inputs: ChartInputs): Reporting {
  const { draft, derived, derivation } = inputs;
  const items = new Map<string, ReportingItem>();
  if (!derivation) {
    return { known: false, current: false, roots: [], cycles: [], tree: [], items };
  }
  const forest = reportingForest(derivation);
  const convert = (node: ReportingNode, root: boolean): ReportingItem => {
    const key = derived.keyOfHandle.get(node.seat.handle);
    const found = key === undefined ? undefined : locate(draft, key);
    const current = found?.kind === "seat" ? found.node : undefined;
    const item: ReportingItem = {
      id: current ? current.key : `reporting:${node.index}`,
      key: current ? current.key : null,
      name: current?.data.name ?? node.seat.name,
      kind: current ? kindOf(current.data) : node.seat.kind === "human" ? "human" : "agent",
      handle: current?.data.handle ?? node.seat.handle,
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
  return { known: true, current: inputs.current, roots, cycles, tree, items };
}
