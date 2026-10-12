/**
 * The drawing of an organization's nodes, shared by the two charts of one
 * company: the live org chart (Agents › Org chart) and the org builder
 * (Agents › Edit org).
 *
 * ONE DRAWING, TWO SOURCES. The builder draws a draft somebody is editing and
 * the live chart draws the company this node has applied, and their models
 * never meet (`lib/orgchart.ts` holds that boundary). What a node LOOKS like
 * is not a fact about either source: the mark a seat leads with, the hue an
 * agent is toned in, the words under a unit's name, the switch between the
 * two arrangements and the control that opens what hangs under a node are one
 * decision, drawn here, so the chart a reader edits and the chart they watch
 * are one drawing.
 */

import type { ReactNode } from "react";
import {
  IconButton,
  OrgNodeDisclosure,
  SegmentedControl,
  type TreeCardContext,
  type TreeCardTone,
} from "@crewlethq/ui";
import { CrewletCharacter } from "@crewlethq/icons/characters";
import { DEFAULT_AVATAR, type AgentAvatar, type AvatarCharacter } from "~/lib/avatar.ts";
import {
  BuildingComplexGlyph,
  ChevronDownGlyph,
  ChevronRightGlyph,
  NetworkGlyph,
  UserGlyph,
  cssLength,
  type GlyphSize,
} from "@crewlethq/icons/glyphs";

/**
 * The two arrangements of one organization: its STRUCTURE (the company, its
 * units and the seats in each) and its REPORTING lines (who each seat reports
 * to, as the engine derived it).
 */
export type ChartKind = "structure" | "reporting";

/** The chart a `chart` section param names, the structure for anything else. */
export function chartKindOf(param: string): ChartKind {
  return param === "reporting" ? "reporting" : "structure";
}

const CHART_OPTIONS: { value: ChartKind; label: string }[] = [
  { value: "structure", label: "Structure" },
  { value: "reporting", label: "Reporting" },
];

/**
 * The switch between the two arrangements, as a bar under a canvas's zoom bar,
 * where the console chart keeps it.
 */
export function ChartSwitch({
  value,
  onValueChange,
  panelId,
}: {
  value: ChartKind;
  onValueChange: (value: ChartKind) => void;
  /** The element the two charts are drawn in, which the switch controls. */
  panelId: string;
}) {
  return (
    <SegmentedControl
      label="Chart"
      semantics="tabs"
      panelId={panelId}
      value={value}
      onValueChange={(next) => onValueChange(chartKindOf(next))}
      size="sm"
      options={CHART_OPTIONS}
    />
  );
}

/** "Agent seat" or "Human seat". */
export function seatKindLabel(view: { readonly kind?: string | undefined }): string {
  return view.kind === "human" ? "Human seat" : "Agent seat";
}

/**
 * A unit's own type, capitalised: the word a chart node and a table row write
 * under the unit's name.
 *
 * CAPITALISED, and in ONE place. The type is the founder's own word from the
 * document, so the first letter is the only thing touched; written out on both
 * surfaces it drifted, and one draft read "Team" on the card and "team" on the
 * row beside it.
 */
export function unitTypeLabel(view: { readonly unitType: string }): string {
  const type = view.unitType.trim();
  if (type === "") return "Unit";
  return type.charAt(0).toUpperCase() + type.slice(1);
}

/** A unit's lead as both charts name it: the seat, and whether it is inherited. */
export interface LeadName {
  readonly name: string;
  /** Handed down from a unit above rather than declared by this one. */
  readonly inherited: boolean;
}

/** A lead's name, marked when the unit inherits it from a unit above. */
export function leadName(lead: LeadName): string {
  return lead.inherited ? `${lead.name} (inherited)` : lead.name;
}

/**
 * What a unit's lead pill writes along the bottom edge of its node, as the
 * console chart writes it: "Lead: Ada" set, and "Lead" in a pill with nothing
 * in it. The pill stands on its own, with nothing above it saying what the
 * name IS, so the word travels with the name.
 */
export function leadPill(lead: LeadName | null): string {
  return lead ? `Lead: ${leadName(lead)}` : "Lead";
}

/** What a unit's node says about its lead to a screen reader: one sentence. */
export function leadSentenceOf(lead: LeadName | null): string {
  return lead ? `Lead: ${leadName(lead)}.` : "No lead.";
}

/** Which of the four things a node is, for the mark that stands for it. */
export type NodeGlyphKind = "company" | "unit" | "agent" | "human";

/**
 * A node's mark: a building for the company, a tree for a unit, a person for a
 * human seat and the Crewlet figure for an agent seat.
 *
 * ONE MAPPING FOR EVERY SURFACE, because a node marked three ways is a node a
 * reader has to learn three times. Both charts' nodes, the table's rows and
 * the head of the editor over either of them all draw this: the editor's own
 * head used to draw a pencil on all four, which said the panel edits rather
 * than what it is editing, and the table and the chart each held a copy of
 * the mapping beside it.
 *
 * SIZE IS THE CALLER'S, and its absence means the mark takes the font size of
 * the zone it is in, which is how a chart gives an agent seat three quarters
 * of its icon zone and a unit half of it. Every drawing here answers `1em` by
 * default, the Crewlet figure included.
 *
 * A HUMAN'S FIGURE IS THE EXCEPTION, because it is the one mark drawn INSIDE
 * something: the dashed boundary that says this seat is a person outside the
 * system, held at the 24px target floor. At the zone's own step the figure
 * measured 20px inside that 24px ring and touched it on every side; the small
 * step leaves the air the boundary needs to read as a boundary. A caller that
 * says a size still gets it.
 */
export function NodeGlyph({
  kind,
  size,
  character,
}: {
  kind: NodeGlyphKind;
  size?: GlyphSize;
  /** The Crewlet character an agent is drawn as; the original where it names none. */
  character?: AvatarCharacter;
}) {
  if (kind === "company") return <BuildingComplexGlyph size={size} />;
  if (kind === "unit") return <NetworkGlyph size={size} />;
  if (kind === "human") return <UserGlyph size={size ?? "sm"} />;
  const side = cssLength(size);
  // COMPACT, because a node's mark is drawn at a glyph's size, where the
  // keyline and the visor gap would be hairlines a screen blurs.
  return (
    <CrewletCharacter
      character={character ?? DEFAULT_AVATAR.character}
      detail="compact"
      width={side}
      height={side}
    />
  );
}

/**
 * The hue an agent seat is drawn in, on a chart's node and a table's row: the
 * colour its author chose for it ([AgentAvatar]), purple where it names none.
 * It is the same colour as the seat's avatar everywhere else, so an agent is
 * one colour wherever it appears; a person, a unit and the company stay on
 * the chart's neutral surface.
 */
export function seatTone(
  kind: string | undefined,
  avatar: AgentAvatar | null | undefined,
): TreeCardTone | undefined {
  return kind === "human" ? undefined : (avatar ?? DEFAULT_AVATAR).color;
}

/**
 * The mark a seat leads with, in a chart's node or a table's row: an agent's
 * own Crewlet character, at the size a node gives the thing the chart is
 * about, and a person inside a dashed ring for a human. A row draws the
 * character without its ring, which the design system decides.
 */
export function seatMark(
  kind: string | undefined,
  avatar: AgentAvatar | null | undefined,
): {
  icon: ReactNode;
  iconSize: "md" | "lg";
  iconRing: boolean;
} {
  const human = kind === "human";
  return {
    icon: <NodeGlyph kind={human ? "human" : "agent"} character={avatar?.character} />,
    iconSize: human ? "md" : "lg",
    iconRing: human,
  };
}

/**
 * The control that opens and closes what hangs under a node, on its leading
 * edge.
 *
 * BESIDE THE TREEITEM, NEVER INSIDE IT, which is what `OrgNodeDisclosure` is
 * for: a tree's items hold nothing focusable, so the design system draws this
 * as a sibling of the node's own item, exactly where the actions strip goes,
 * and places it on the card's boundary from there. It used to hang on the
 * BRANCH beside the Add, where the add pill splitting open into its three
 * kinds covered it, which is the one gesture a reader makes right next to it.
 *
 * A NODE WITH NOTHING UNDER IT DRAWS NONE, so a leaf of the chart is a card
 * with no disclosure rather than one with an empty slot.
 *
 * AND THE PRESS LANDS ON THE NODE, never in the control, which is why
 * `card.press` is spread on the region rather than on the button inside it:
 * it is the same rule the actions strip gets from `card.actions`, and the
 * design system asks a caller for it here because only the chart knows which
 * node this belongs to. Focus in a subtree hidden from assistive technology is
 * focus nowhere, so a press that left it on the expander would leave the chart
 * with no announced position at all.
 */
export function NodeToggle({
  card,
  id,
  name,
}: {
  card: TreeCardContext;
  id: string;
  name: string;
}) {
  if (!card.expandable(id)) return null;
  const expanded = card.expanded(id);
  return (
    <OrgNodeDisclosure {...card.press(id)}>
      <IconButton
        label={expanded ? `Collapse ${name}` : `Expand ${name}`}
        icon={expanded ? <ChevronDownGlyph /> : <ChevronRightGlyph />}
        size="sm"
        variant="ghost"
        tabIndex={-1}
        onClick={(e) => {
          e.stopPropagation();
          card.toggle(id);
        }}
      />
    </OrgNodeDisclosure>
  );
}
