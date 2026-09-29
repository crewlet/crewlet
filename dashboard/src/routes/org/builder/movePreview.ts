/**
 * What a move changes beyond where a node is drawn, previewed before it is
 * recorded.
 *
 * TWO KINDS OF CONSEQUENCE, read two ways. What follows from what the chart's
 * rows STATE is read off the draft, because it follows from one documented
 * rule applied to values the draft holds: the lead and channel a moved unit
 * inherits (a unit that declares none takes the one its parent resolved to,
 * `chartModel.effectiveLeads`), the agent seats that onboard again (their
 * onboarding marker hashes the IDENTITIES of the units above them, so other
 * units above them re-fire it, `learning.ChainHash`), and the tool credential
 * servers a seat's home unit hands it (a unit's `mcp_env` reaches its direct
 * agent members, `org.inheritMCPEnv`). What follows from the whole
 * organization — who a seat reports to — is the ENGINE's derivation, and the
 * only one there is describes the SAVED chart (`nodeFacts.savedDerivation`),
 * so it is said as "today" and only while the draft's chart is still the
 * saved one. The review lists the change before the save.
 */

import { COMPANY_KEY, type NodeKey } from "./model/keys.ts";
import { allSeats, locate, subtreeKeys, type DraftUnit } from "./model/draft.ts";
import { kindOf } from "./model/operations.ts";
import type { BuilderState } from "./model/reducer.ts";
import { chartInputs, effectiveLeads, structure } from "./chartModel.ts";
import { homeUnitOf, nameOfHandle, savedDerivation, toolServersOf } from "./nodeFacts.ts";

/** A lead or a channel a moved unit resolves to, before and after. */
export interface InheritedChange {
  readonly unit: string;
  readonly before: string;
  readonly after: string;
}

export interface MovePreview {
  /**
   * The moved seat's primary manager today, by name; `null` for none;
   * `undefined` while the engine has not derived it for this draft (the
   * draft's chart is not the saved one). Seats only.
   */
  readonly reportsTo: string | null | undefined;
  /**
   * The unit whose lead that manager is, when it manages the seat only
   * automatically, as the lead of the seat's home unit, and the move takes
   * the seat out of that unit: the engine's auto-management reaches a unit's
   * direct members, so the relation ends with the move. `null` otherwise.
   */
  readonly endsAsLeadOf: string | null;
  /**
   * The destination unit's lead, by name, when a seat moves into a unit it
   * does not lead: that lead manages the unit's direct members unless another
   * member manages the seat. `null` for none, or at the top level.
   */
  readonly destinationLead: string | null;
  /** Units of a moved subtree whose inherited lead changes. */
  readonly leads: readonly InheritedChange[];
  /** Units of a moved subtree whose inherited channel changes. */
  readonly channels: readonly InheritedChange[];
  /** Agent seats that onboard again, by name. */
  readonly onboarding: readonly string[];
  /** Tool credential servers a moved agent seat gains and loses through its home unit. */
  readonly credentials: { readonly gained: readonly string[]; readonly lost: readonly string[] };
}

/** What moving `target` under `destination` changes; `null` when the draft holds no such node. */
export function movePreview(
  state: BuilderState,
  target: NodeKey,
  destination: NodeKey,
): MovePreview | null {
  const draft = state.draft;
  const found = locate(draft, target);
  if (!found) return null;
  const chart = structure(chartInputs(state));
  const resolved = effectiveLeads(draft);
  const destView = destination === COMPANY_KEY ? undefined : chart.nodes.get(destination);
  const destLead = destView?.type === "unit" ? destView.lead : null;
  // What a node placed under the destination inherits: what it resolves to.
  const destChannel = resolved.get(destination)?.channel ?? "";

  if (found.kind === "seat") {
    const seat = found.node;
    const agent = kindOf(seat.data) === "agent";
    const home = homeUnitOf(draft, target);
    const dest = destination === COMPANY_KEY ? undefined : locate(draft, destination);
    const had = agent ? toolServersOf(seat.data, home) : new Set<string>();
    const has = agent
      ? toolServersOf(seat.data, dest?.kind === "unit" ? dest.node : undefined)
      : new Set<string>();
    const saved = savedDerivation(state);
    const derived = saved?.seatByKey.get(target);
    // Automatic, as the engine derived it: the manager's own automatic
    // reports name this seat.
    const manager = derived?.manager
      ? saved?.derived.seats?.find((s) => s.handle === derived.manager)
      : undefined;
    const automatic = derived ? manager?.auto_reports?.includes(derived.handle) === true : false;
    return {
      reportsTo: derived
        ? derived.manager
          ? nameOfHandle(state, derived.manager)
          : null
        : undefined,
      endsAsLeadOf: automatic && home && home.key !== destination ? home.data.name : null,
      destinationLead:
        destLead === null || destLead.handle === seat.data.handle ? null : destLead.name,
      leads: [],
      channels: [],
      // THE CHAIN OF UNITS ABOVE IT CHANGES with any move, and that chain is
      // what its onboarding marker is keyed on.
      onboarding: agent && destination !== found.parent ? [seat.data.name || seat.data.handle] : [],
      credentials: {
        gained: [...has].filter((s) => !had.has(s)).sort(),
        lost: [...had].filter((s) => !has.has(s)).sort(),
      },
    };
  }

  // A unit: its members stay its members, so what changes is what it and the
  // units below it inherit, and every agent seat's chain of units above it.
  const leads: InheritedChange[] = [];
  const channels: InheritedChange[] = [];
  const walk = (u: DraftUnit, leadFromAbove: boolean, channelFromAbove: boolean) => {
    const declaresLead = typeof u.data.lead === "string" && u.data.lead.trim() !== "";
    const declaresChannel = typeof u.data.channel === "string" && u.data.channel.trim() !== "";
    const inheritsLead = leadFromAbove && !declaresLead;
    const inheritsChannel = channelFromAbove && !declaresChannel;
    const view = chart.nodes.get(u.key);
    if (view?.type === "unit" && inheritsLead) {
      const before = view.lead?.name ?? "";
      const after = destLead?.name ?? "";
      if (before !== after) leads.push({ unit: u.data.name, before, after });
    }
    if (inheritsChannel) {
      const before = resolved.get(u.key)?.channel ?? "";
      if (before !== destChannel) channels.push({ unit: u.data.name, before, after: destChannel });
    }
    for (const child of u.children) walk(child, inheritsLead, inheritsChannel);
  };
  walk(found.node, true, true);

  const inside = new Set(subtreeKeys(found.node));
  const onboarding =
    destination === found.parent
      ? []
      : [...allSeats(draft)]
          .filter(({ seat, parent }) => kindOf(seat.data) === "agent" && inside.has(parent))
          .map(({ seat }) => seat.data.name || seat.data.handle);

  return {
    reportsTo: null,
    endsAsLeadOf: null,
    destinationLead: null,
    leads,
    channels,
    onboarding,
    credentials: { gained: [], lost: [] },
  };
}
