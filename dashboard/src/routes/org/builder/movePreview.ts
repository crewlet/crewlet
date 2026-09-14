/**
 * What a move changes that the engine derives, previewed from the last check
 * of the draft as it stands.
 *
 * THE LAST CHECK IS WHAT IS KNOWN. The engine derives a seat's manager, a
 * unit's effective lead and channel and a seat's onboarding chain, and the
 * builder does not compute them again (see `nodeFacts.ts`). What the Move
 * dialog can say before the move exists is therefore what the last check
 * reported about BOTH ends: who the seat reports to now, the lead and channel
 * the destination resolved to, the onboarding chain a seat has now against
 * the unit names above the destination, and the tool credential servers the
 * seat's home unit gives it against the ones the destination's gives. Each
 * of those follows from one documented engine rule applied to reported
 * values: a unit that declares no lead or channel takes the one its parent
 * resolved to, an agent seat onboards again when the unit names above it
 * change, and a unit's `mcp_env` reaches its direct agent members. The check
 * that follows the move reports the result, and the review shows it.
 */

import { COMPANY_KEY, type NodeKey } from "./model/keys.ts";
import {
  allSeats,
  allUnits,
  locate,
  subtreeKeys,
  type Draft,
  type DraftUnit,
} from "./model/draft.ts";
import { jsonEqual } from "./model/json.ts";
import { kindOf } from "./model/operations.ts";
import type { BuilderState } from "./model/reducer.ts";
import { derivedSeatOf, derivedUnitOf, nameOfHandle, toolCredentialNames } from "./nodeFacts.ts";

/** A lead or a channel a moved unit resolves to, before and after. */
export interface InheritedChange {
  readonly unit: string;
  readonly before: string;
  readonly after: string;
}

export interface MovePreview {
  /** Whether the last check described the draft, so the rest is known. */
  readonly known: boolean;
  /** The moved seat's primary manager now, by name; `null` for none. Seats only. */
  readonly reportsTo: string | null;
  /**
   * The destination unit's effective lead, by name, when a seat moves into a
   * unit it does not lead: that lead manages the unit's direct members unless
   * another member manages the seat. `null` for none or at the root.
   */
  readonly destinationLead: string | null;
  /** Units of a moved subtree whose inherited lead changes. */
  readonly leads: readonly InheritedChange[];
  /** Units of a moved subtree whose inherited channel changes. */
  readonly channels: readonly InheritedChange[];
  /** Agent seats whose onboarding chain changes, by name. */
  readonly onboarding: readonly string[];
  /** Tool credential servers a moved agent seat gains and loses through its home unit. */
  readonly credentials: { readonly gained: readonly string[]; readonly lost: readonly string[] };
}

const NOTHING: MovePreview = {
  known: false,
  reportsTo: null,
  destinationLead: null,
  leads: [],
  channels: [],
  onboarding: [],
  credentials: { gained: [], lost: [] },
};

/** The names of a unit and every unit above it, outermost first; empty at the root. */
function chainNames(draft: Draft, unit: NodeKey): string[] {
  if (unit === COMPANY_KEY) return [];
  const parents = new Map([...allUnits(draft)].map(({ unit: u, parent }) => [u.key, parent]));
  const out: string[] = [];
  for (let at: NodeKey | undefined = unit; at !== undefined && at !== COMPANY_KEY;) {
    const found = locate(draft, at);
    if (found?.kind !== "unit") break;
    out.unshift(found.node.data.name);
    at = parents.get(at);
  }
  return out;
}

/** The key of the unit an authored unit path names in the last check's document. */
function unitKeyAt(state: BuilderState, path: string | undefined): NodeKey | undefined {
  if (path === undefined) return undefined;
  if (path === "") return COMPANY_KEY;
  return state.check.sent?.index.byPath.get(path);
}

/** What moving `target` to the end of `destination` changes, from the last check. */
export function movePreview(
  state: BuilderState,
  target: NodeKey,
  destination: NodeKey,
): MovePreview {
  const draft = state.draft;
  const found = locate(draft, target);
  if (!found || state.check.derived === null || state.check.sent === null) return NOTHING;

  const destUnit = destination === COMPANY_KEY ? undefined : derivedUnitOf(state, destination);
  const destLeadName = destUnit?.lead ? nameOfHandle(state, destUnit.lead) : null;
  const destChannel = destUnit?.channel ?? "";
  const destChain = chainNames(draft, destination);

  if (found.kind === "seat") {
    const seat = derivedSeatOf(state, target);
    if (!seat) return NOTHING;
    const agent = kindOf(found.node.data) === "agent";
    const leadsDestination = destUnit?.lead !== undefined && destUnit.lead === seat.handle;
    const home = unitKeyAt(state, seat.unit_path);
    const homeData = home === undefined || home === COMPANY_KEY ? undefined : locate(draft, home);
    const destData = destination === COMPANY_KEY ? undefined : locate(draft, destination);
    const servers = (unit: ReturnType<typeof locate>) =>
      new Set([
        ...toolCredentialNames(found.node.data).map((s) => s.server),
        ...(unit?.kind === "unit" ? toolCredentialNames(unit.node.data).map((s) => s.server) : []),
      ]);
    const had = agent ? servers(homeData) : new Set<string>();
    const has = agent ? servers(destData) : new Set<string>();
    return {
      known: true,
      reportsTo: seat.manager ? nameOfHandle(state, seat.manager) : null,
      destinationLead: leadsDestination ? null : destLeadName,
      leads: [],
      channels: [],
      onboarding:
        agent && !jsonEqual(seat.onboarding_chain ?? [], destChain) ? [found.node.data.name] : [],
      credentials: {
        gained: [...has].filter((s) => !had.has(s)).sort(),
        lost: [...had].filter((s) => !has.has(s)).sort(),
      },
    };
  }

  // A unit: its members stay its members, so what changes is what it and the
  // units below it inherit, and every agent seat's chain of unit names.
  const unit = found.node;
  const leads: InheritedChange[] = [];
  const channels: InheritedChange[] = [];
  const walk = (u: DraftUnit, leadFromAbove: boolean, channelFromAbove: boolean) => {
    const declaresLead = typeof u.data.lead === "string" && u.data.lead !== "";
    const declaresChannel = typeof u.data.channel === "string" && u.data.channel !== "";
    const reported = derivedUnitOf(state, u.key);
    const inheritsLead = leadFromAbove && !declaresLead;
    const inheritsChannel = channelFromAbove && !declaresChannel;
    if (reported && inheritsLead) {
      const before = reported.lead ? nameOfHandle(state, reported.lead) : "";
      const after = destLeadName ?? "";
      if (before !== after) leads.push({ unit: u.data.name, before, after });
    }
    if (reported && inheritsChannel && reported.channel !== destChannel) {
      channels.push({ unit: u.data.name, before: reported.channel, after: destChannel });
    }
    for (const child of u.children) walk(child, inheritsLead, inheritsChannel);
  };
  walk(unit, true, true);

  const nowAbove = chainNames(draft, found.parent);
  const inside = new Set(subtreeKeys(unit));
  const onboarding = jsonEqual(nowAbove, destChain)
    ? []
    : [...allSeats(draft)]
        .filter(({ seat }) => kindOf(seat.data) === "agent")
        .filter(({ seat }) => {
          const home = unitKeyAt(state, derivedSeatOf(state, seat.key)?.unit_path);
          return home !== undefined && inside.has(home);
        })
        .map(({ seat }) => seat.data.name);

  return {
    known: true,
    reportsTo: null,
    destinationLead: null,
    leads,
    channels,
    onboarding,
    credentials: { gained: [], lost: [] },
  };
}
