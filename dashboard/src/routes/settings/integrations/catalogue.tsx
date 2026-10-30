/**
 * The third-party apps this build serves, and the readings every surface that
 * names one shares: the catalogue (`CATALOG`), a tool's state off the engine's
 * roll-up (`entryState`), its counts, the action it offers, its setup passes,
 * its deliveries, and the peek a link to it opens (`IntegrationPeek`).
 *
 * The page is `Directory.tsx` and its two panes; the Home widget and the peek
 * read the same functions, so no surface claims something about a tool
 * another does not.
 *
 * An app is the tool, not the engine's plumbing for it. Atlassian is one entry
 * although the engine reaches it over three surfaces (Jira, Confluence and the
 * Forge relay), because that is how the company thinks of it; its state is the
 * ENGINE's roll-up of its surfaces (`integration.Rollup`, sent as the answer's
 * `tools`).
 *
 * Every count is THREE-VALUED (a number, zero, or `null` meaning "this
 * process cannot say"), and so is the reconcile block: `null` is a process
 * with no loop to ask or a surface the loop has not reached, and neither is a
 * claim that the tool is fine.
 */

import { useCallback, useEffect, useMemo, useState } from "react";
import {
  Button,
  ButtonLink,
  Card,
  EmptyState,
  EmptyValue,
  InlineCode,
  Skeleton,
  TableDateCell,
  TableEntityCell,
  TableKeyCell,
  TableNumberCell,
  TableStatusCell,
  TableTextCell,
  Tag,
  type Tone,
} from "@crewlethq/ui";
import {
  PlugGlyph,
  LayersGlyph,
  InboxGlyph,
  RotateCwGlyph,
  TriangleAlertGlyph,
} from "@crewlethq/icons/glyphs";
import { VendorMark, type Vendor } from "@crewlethq/icons";
import { TOOL_TONE } from "~/lib/toolState.ts";
import { QueryState } from "~/components/common.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { type DataGridColumn } from "@crewlethq/ui";
import { linkedSeat } from "~/app/frame/cells.tsx";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { href, useNavigator } from "~/app/router.tsx";
import { useNow } from "~/lib/clock.ts";
import {
  fmtDate,
  fmtDateTime,
  plural,
  relTime,
  tsKey,
  fmtCount,
  fmtDuration,
} from "~/lib/format.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg, seatLookup } from "~/lib/seats.ts";
import { pathSegment, rest, RestError } from "~/protocol/index.ts";
import { ADMIN, needsWords } from "~/lib/access.ts";
import { authorityWords } from "~/lib/refusal.ts";
import { useRest } from "~/lib/useRest.ts";
import type { EventRecord, SetupRun } from "~/protocol/types.ts";
import {
  INTEGRATION_TOOLS,
  type IntegrationRow,
  type IntegrationTool,
  type ReconcileFinding,
} from "~/contract/integrations.ts";
import type { SetupListing, SetupSeatState, SetupToolState } from "~/protocol/types.ts";

// THE TONE VOCABULARY IS uilet's NOW. This screen used to declare its own
// five-value union beside the one the design system publishes, which is the
// drift `app/frame/tone.ts` was written to stop — so `Tone` is imported rather
// than re-spelled, and `positive`/`caution`/`critical` are `success`/`warning`/
// `danger` throughout. `brand` is in the type and is deliberately unused here:
// a vendor is an identity, and identity never takes colour.

/** One engine surface behind a tool: the key the API row carries, named. */
export interface Surface {
  key: string;
  name: string;
}

export interface Entry {
  key: string;
  name: string;
  /**
   * What the tool DOES for the company, in one sentence — the line a tile
   * carries when nothing is owed on it.
   *
   * A SENTENCE RATHER THAN A CATEGORY. It read "Team communication" and
   * "Code and pull requests", which named a shelf in a shop: every reader
   * already knows what Slack is, and what they did not know is what the
   * engine does with it — that each agent posts as its own bot, that a
   * monitor routes by its tags.
   */
  description: string;
  vendor: Vendor;
  /** The API rows that together are this tool. Most tools have one. */
  surfaces: Surface[];
  /**
   * This tool's page on docs.crewlet.ai, `docs/integrations/<doc>.md` in the
   * engine's tree, for the one tile that has nothing a person can do from
   * here: Learn more.
   */
  doc: string;
}

/**
 * Where the engine's documentation is published: the newest release's copy
 * (`/engine/latest/`; main is `/engine/next/`, an older line `/engine/vX.Y/`).
 * See `docs/index.md`.
 */
const DOCS_BASE = "https://docs.crewlet.ai/engine/latest/";

/**
 * The published address of an integration's own page.
 *
 * WITH ITS TRAILING SLASH, because every page on the site is a directory: the
 * slashless spelling answers a redirect to this one, a round trip on every
 * click before the page loads.
 */
export function docHref(entry: Entry): string {
  return `${DOCS_BASE}integrations/${entry.doc}/`;
}

/**
 * How a sentence on this screen names each surface.
 *
 * WHICH surfaces make up a tool, and in what order, is the engine's
 * (`INTEGRATION_TOOLS`, held against `integration.Tools`); only the words are
 * this screen's. Atlassian's organization is first because it is where an
 * agent's account is created, and the two products are what that account
 * then works in.
 */
const SURFACE_NAMES: Record<string, string> = {
  slack: "Slack",
  mattermost: "Mattermost",
  atlassian: "Organization",
  confluence: "Confluence",
  jira: "Jira",
  forge: "Forge relay",
  github: "GitHub",
  gitlab: "GitLab",
  datadog: "Datadog",
};

/** One tool's surfaces, in the engine's grouping, named for this screen. */
function surfacesOf(tool: keyof typeof INTEGRATION_TOOLS): Surface[] {
  return INTEGRATION_TOOLS[tool].map((key) => ({ key, name: SURFACE_NAMES[key] ?? key }));
}

/**
 * The catalogue: every tool this build serves, in the console's own words.
 * The surface keys match the `integrations` answer's rows and the tool keys
 * its `tools`, so the three join by name.
 */
export const CATALOG: Entry[] = [
  {
    key: "slack",
    name: "Slack",
    description: "Each agent posts as its own bot.",
    vendor: "slack",
    surfaces: surfacesOf("slack"),
    doc: "slack",
  },
  {
    key: "mattermost",
    name: "Mattermost",
    description: "Self-hosted chat. Each agent posts as its own bot.",
    vendor: "mattermost",
    surfaces: surfacesOf("mattermost"),
    doc: "mattermost",
  },
  {
    key: "atlassian",
    name: "Atlassian",
    description: "Jira and Confluence, an account per agent.",
    vendor: "atlassian",
    surfaces: surfacesOf("atlassian"),
    doc: "jira",
  },
  {
    key: "github",
    name: "GitHub",
    description: "Issues and pull requests, an app per agent.",
    vendor: "github",
    surfaces: surfacesOf("github"),
    doc: "github",
  },
  {
    key: "gitlab",
    name: "GitLab",
    description: "Merge requests and code, a token per agent.",
    vendor: "gitlab",
    surfaces: surfacesOf("gitlab"),
    doc: "gitlab",
  },
  {
    key: "datadog",
    name: "Datadog",
    description: "Alerts reach seats by tag.",
    vendor: "datadog",
    surfaces: surfacesOf("datadog"),
    doc: "datadog",
  },
];

/**
 * The phases that mean the engine is still working.
 *
 * The screen polls faster while any surface is in one, because each is a
 * state that RESOLVES ON ITS OWN within seconds and the row is the only place
 * that says how it resolved.
 */
export const IN_FLIGHT = new Set(["awaiting_admin", "provisioning", "activating", "disconnecting"]);

/**
 * COLOUR CARRIES STATE, NEVER IDENTITY (see reference/dashboard-design.md), so
 * the tone follows the phase rather than the vendor.
 *
 * `ready` is the only positive one, and `degraded` is caution rather than
 * critical on purpose: agents are still working, which is exactly what
 * separates it from a surface that cannot authenticate at all.
 */
export function phaseTone(phase: string): Tone {
  switch (phase) {
    case "ready":
      return "success";
    case "degraded":
      return "warning";
    case "unconfigured":
      return "danger";
    // IN PROGRESS IS NOT NEUTRAL. Every one of these is an integration
    // that does not work YET — setting up agents, waiting for the provider,
    // waiting for a person, being taken away — and neutral is the tone this
    // screen uses for a state nobody needs to come back to. Amber is the
    // word for incomplete: it says look again, without saying broken, which
    // is what critical and caution's red neighbour would say.
    case "awaiting_admin":
    case "provisioning":
    case "activating":
    case "disconnecting":
      return "warning";
    default:
      // A phase a newer node wrote. Rendered as-is in a neutral tone
      // rather than guessed at: claiming a surface is fine on the
      // strength of a word this build cannot interpret is the one answer
      // that is certainly wrong.
      return "neutral";
  }
}

/** A tool's rolled-up state: the pill under its name, and what is owed when something is. */
export interface EntryState {
  /**
   * The word in the badge: the engine's roll-up label — a phase's own label,
   * or Connecting, Paused, Not in use.
   *
   * A TOOL'S OWN PAGE CARRIES IT WHATEVER IT SAYS, Not in use included: it is
   * the page's one statement about the tool. The CATALOGUE draws it only for
   * a tool the company set up ([IntegrationTile]): a "Not in use" pill on
   * every unconnected tile was a column a reader learned to skip, and the
   * filter's counts and the tile's quieter ground already say it. EMPTY only
   * while no roll-up for an unconfigured tool is in hand, before the
   * integrations answer arrives.
   */
  tag: string;
  tone: Tone;
  /** Drawn outlined when the tool is absent or paused, filled when it is live. */
  outline: boolean;
  /**
   * Whether the engine or the third-party app is mid-flight on this card.
   *
   * A CARD IN MOTION OFFERS NOTHING, which is what this exists to say. It is
   * the window between a connect and the loop's first report, and the one
   * between asking for a disconnect and its finishing: a person has nothing
   * to do in either, and a button beside "Connecting" invites them to act on
   * a card whose state is about to change under them.
   */
  busy?: boolean;
  /** The engine's one sentence on why, about `surface`. */
  reason?: string;
  /** The surface the state was taken from. */
  surface?: string;
}

export type Present = { surface: Surface; row: IntegrationRow };

export function presentSurfaces(entry: Entry, rows: Map<string, IntegrationRow>): Present[] {
  return entry.surfaces
    .map((s) => ({ surface: s, row: rows.get(s.key) }))
    .filter((p): p is Present => p.row !== undefined);
}

/**
 * A tool's card state, from the ENGINE's roll-up of it.
 *
 * NO RULE LIVES HERE. Which surface's word wins, when a ready phase still
 * needs a person and what a surface no pass converges reads as are decided
 * once, in `integration.Rollup`, and arrive as `tools` on the answer. This
 * screen used to decide them itself — its own copy of the phase order, its own
 * ingress override, its own reading of the setup listing — and the copy had
 * drifted: a card being disconnected drew neutral while the surface it named
 * drew amber. What is left is presentation: which tone and outline a state
 * takes, and that a tool with no configured surface takes the roll-up's word
 * only when it is "Not in use" — in the neutral outline, see [EntryState.tag] —
 * and never a state a surface the rows no longer carry would draw.
 */
export function entryState(
  entry: Entry,
  rows: Map<string, IntegrationRow>,
  tools: IntegrationTool[] | undefined,
): EntryState {
  const tool = tools?.find((t) => t.key === entry.key);
  if (presentSurfaces(entry, rows).length === 0) {
    // NOTHING CONFIGURED IS NOT IN USE WHATEVER THE ROLL-UP SAYS. The rows
    // are the fresher half (see [actionFor]), and a roll-up still describing
    // a surface the rows no longer carry would draw a state over a tile
    // whose only action is Connect.
    return { tag: tool?.state === "not_in_use" ? tool.label : "", tone: "neutral", outline: true };
  }
  if (!tool) {
    // AN ANSWER WITH NO ROLL-UP FOR A TOOL IT HAS ROWS FOR cannot be judged
    // here, and inventing a state is the one thing this screen must not do.
    return { tag: "Status unavailable", tone: "neutral", outline: true, busy: true };
  }
  return {
    tag: tool.label,
    tone: TOOL_TONE[tool.state] ?? "neutral",
    outline: tool.state === "not_in_use",
    // NOT CONNECTED IS THE ENGINE MID-FLIGHT, or a node that cannot say:
    // nothing a person does moves the card, so it offers nothing.
    busy: tool.state === "not_connected",
    reason: tool.reason,
    surface: tool.surface,
  };
}

// ---------------------------------------------------------------------------
// What an integration IS, in facts
// ---------------------------------------------------------------------------

/**
 * A counter summed over the surfaces one tool is made of, still three-valued.
 *
 * A SURFACE THAT CANNOT SAY CONTRIBUTES NOTHING RATHER THAN ZERO, so the total
 * is a FLOOR: `null` is this process reporting that it could not read its own
 * event log, and folding that in as 0 would turn "nobody looked" into "nothing
 * arrived there". Where no surface can say at all the answer stays null, which
 * is the absence [TableNumberCell] draws and the one this screen means by it.
 */
export function total(
  present: Present[],
  pick: (row: IntegrationRow) => number | null | undefined,
): number | null {
  let sum: number | null = null;
  for (const p of present) {
    const one = pick(p.row);
    if (one == null) continue;
    sum = (sum ?? 0) + one;
  }
  return sum;
}

/** The most recent of an instant across a tool's surfaces, or null. */
export function latestOf(
  present: Present[],
  pick: (row: IntegrationRow) => string | null | undefined,
): string | null {
  let best = "";
  for (const p of present) {
    const at = pick(p.row) ?? "";
    if (at !== "" && tsKey(at) > tsKey(best)) best = at;
  }
  return best === "" ? null : best;
}

/**
 * The SOONEST of an instant across a tool's surfaces, or null.
 *
 * The other direction from [latestOf], and not a flag on it: "when did
 * anything last look" and "when does anything look next" are two questions,
 * and taking the latest of the second would report the slowest surface's tick
 * as the whole tool's — on a card whose every other roll-up reports the LEAST
 * ready surface, that is the wrong end of the list every time.
 */
export function soonestOf(
  present: Present[],
  pick: (row: IntegrationRow) => string | null | undefined,
): string | null {
  let best = "";
  for (const p of present) {
    const at = pick(p.row) ?? "";
    if (at !== "" && (best === "" || tsKey(at) < tsKey(best))) best = at;
  }
  return best === "" ? null : best;
}

/**
 * The facts one integration answers, in one order.
 *
 * ONE FUNCTION, because the page and the peek must show the SAME facts in the
 * SAME order — see `app/frame/ObjectHeader.tsx`. Two lists drift the moment
 * either one gains a value, and a reader who has to re-learn an object every
 * time it changes frame is what that component exists to prevent.
 *
 * THE COUNTS ARE GATED ON `traffic_known`, which is not a detail. `inbound`
 * arrives as a plain number, and a node that could not read its event log
 * sends ZERO for every surface — the engine says so at the field
 * (internal/api/queries/company.go) and carries `traffic_known` beside the
 * rows for exactly this reason. Rendered straight, the one screen that answers
 * "is anything arriving at all" would report the alarming answer on precisely
 * the node that had not looked. So a measurement nobody made is null here,
 * which draws the dash, and the note says which of the two a reader has.
 *
 * `skipped` and `coalesced` are already three-valued on the wire; they are
 * gated too, because a rule applied to one count and not its neighbours is one
 * somebody has to re-derive per column.
 */
export function integrationFacts(
  entry: Entry,
  rows: Map<string, IntegrationRow>,
  now: number,
  traffic: { known: boolean; since?: string | null },
): Fact[] {
  const present = presentSurfaces(entry, rows);
  const covered = !traffic.known
    ? "this node could not read its event log"
    : traffic.since
      ? `since ${fmtDate(traffic.since)}`
      : undefined;
  const counted = (pick: (row: IntegrationRow) => number | null | undefined): number | null =>
    traffic.known ? total(present, pick) : null;
  return [
    {
      label: "Delivered",
      value: (
        <TableNumberCell
          value={counted((r) => r.inbound)}
          title="Deliveries that reached a seat"
          format={fmtCount}
        />
      ),
      note: covered,
    },
    {
      // NO NOTE ON THE OTHER TWO, although the same window covers them — the
      // engine counts the drops and merges over exactly the window
      // `traffic_since` names, never over a page of its own: a fact line
      // whose every value carries the same footnote is one nobody reads,
      // which is what `Fact.note` says about itself. The three counts are
      // read together and the first one states what they all cover.
      label: "Dropped",
      value: (
        <TableNumberCell
          value={counted((r) => r.skipped)}
          title="verified, and turned into work for nobody"
          format={fmtCount}
        />
      ),
    },
    {
      label: "Coalesced",
      value: (
        <TableNumberCell
          value={counted((r) => r.coalesced)}
          title="Merged into a pending wake"
          format={fmtCount}
        />
      ),
    },
    {
      // THE LOOP'S OWN CLOCK, which is the one thing a card never says: it
      // reports what the last pass CONCLUDED and never when that was, so a
      // surface checked a moment ago and one nothing has looked at in an hour
      // read identically.
      label: "Last pass",
      value: (
        <TableDateCell value={latestOf(present, (r) => r.reconcile?.last_attempt_at)} now={now} />
      ),
    },
    {
      label: "Next pass",
      value: (
        <TableDateCell value={soonestOf(present, (r) => r.reconcile?.next_attempt_at)} now={now} />
      ),
    },
  ];
}

/**
 * The findings the report did NOT summarise.
 *
 * BY IDENTITY, NOT BY POSITION. A pass's findings are stored with the reported
 * one first (integration.Promote), but the report a row carries is not always
 * the one they were classified from: while a disconnect is pending,
 * State.Reported substitutes "this integration is being removed", and the
 * findings beside it are the last pass's, none of them the headline. Dropping
 * element zero would then hide a real finding; matching the detail the
 * headline carries drops exactly the one it stands for, or none.
 *
 * The report's detail can carry a count suffix, because Classify appends
 * "(and 2 more)" when several findings share the winning kind, so that is
 * stripped before the comparison. Only the FIRST match is removed: several
 * findings can legitimately render the same sentence for different seats, and
 * the report stands for exactly one of them.
 */
/** How many subjects a finding shows before the rest go behind a count. */
const SUBJECT_CHIPS = 6;

/**
 * Everything a finding is about — as things, not as prose.
 *
 * The engine caps a finding's `detail` because that string is the card's
 * status line and an oversized status row is refused by its store outright,
 * so a finding about thirty-six things arrived as a wall cut off mid-item. It
 * now sends the count in the sentence and the list here, and this is where
 * the list is laid out.
 *
 * CHIPS RATHER THAN A DISCLOSURE. What was here was a `<details>` whose
 * summary read `all {n} {subject}s` — which produced "all 1 agents without an
 * app", ungrammatical twice, sitting under a sentence that had ALREADY named
 * the one agent, and needing a click to reveal what it had just repeated. A
 * handle or an address is a thing a reader scans for, not a sentence they
 * read: laid out, six of them are legible at a glance and the seventh is a
 * count.
 *
 * The count EXPANDS rather than links away, because the reason a list is
 * folded here is width, not secrecy — and the measured case, thirty-six
 * service accounts, is a list somebody is working through.
 *
 * Renders nothing for a finding that names no list, the ordinary finding
 * about one thing.
 */
export function FindingSubjects({ subjects: all }: { subjects: readonly string[] }) {
  const [open, setOpen] = useState(false);
  if (all.length === 0) return null;
  const shown = open ? all : all.slice(0, SUBJECT_CHIPS);
  const hidden = all.length - shown.length;
  return (
    <ul className="int-subjects" aria-label={`what this is about, ${all.length}`}>
      {shown.map((one) => (
        <li key={one} className="int-subject">
          {one}
        </li>
      ))}
      {hidden > 0 && (
        <li>
          <button
            type="button"
            className="int-subject int-subject-more"
            onClick={() => setOpen(true)}
          >
            +{hidden} more
          </button>
        </li>
      )}
    </ul>
  );
}

/**
 * The surfaces a card's Disconnect takes away, in the order it takes them.
 *
 * A TEARDOWN IS A CONVERGE RUN BACKWARDS. A card's `surfaces` are declared in
 * DEPENDENCY order — Atlassian lists the Organization first because that is
 * where an agent's account is created, and the two products are what that
 * account then works in. Removing is the other direction: the products stop
 * using the accounts, and only then does the surface whose credential can
 * delete them go. Taking the organization first strands every account the
 * products still hold and leaves nothing able to remove them.
 *
 * So the order is the declared one reversed, and there is exactly ONE list to
 * keep right. The engine states the same dependency for the same reason and
 * in the same direction — see `integration.ConvergeOrder`.
 *
 * IT USED TO PARTITION ON `can_provision`, meaning to express "the
 * provisioning surface last". That field says whether a surface has a
 * reconcile PASS, which was never the question: the engine registers one for
 * Atlassian, Jira AND Confluence, so every Atlassian surface answers true, both
 * halves of the partition hold the same set, and the declared order shipped
 * unchanged — organization first, which is precisely the order the doc forbade.
 * The tests agreed only because their fixtures said `can_provision: false` for
 * the two products, which production never does.
 *
 * ONLY WHAT CAN ACTUALLY BE DISCONNECTED, which is what has a tool state. Those
 * come from `integration.Kinds`, the same set `DELETE /setup/integrations/{kind}`
 * accepts. This also asked the TRAFFIC rows — and the Forge relay has a traffic
 * row and is not a kind, so every Atlassian Cloud company put `forge` first in
 * the list and the dialog aborted on a 404 before touching anything at all.
 *
 * Only surfaces this company actually has: a card lists what a tool can be
 * made of, and a delete against a surface nobody configured is a request with
 * nothing behind it.
 */
export function disconnectOrder(
  entry: Entry,
  rows: Map<string, IntegrationRow>,
  sections: { name: string; tool: SetupToolState }[],
): string[] {
  // THE ROWS ARE THE FALLBACK, and dropping them made Disconnect a silent
  // no-op.
  //
  // The button is rendered from the ROWS (`!absent && onDisconnect`), and
  // `sections` comes from `GET /setup/integrations`, which is a separate
  // request that can be refused for want of a grant or fail transiently.
  // With the key set taken from `sections` alone, that window rendered a
  // Disconnect button whose dialog computed an EMPTY list, issued no DELETE
  // at all, and then ran onDone() and closed exactly as it does after a real
  // teardown — so an operator was told the integration was being removed
  // while nothing had been asked of anything.
  //
  // A row means "this surface is configured" on the authority of the engine's
  // own company document, which is the same claim `tool.configured` makes
  // from the other endpoint, so the union is not a guess: it is the two
  // readings of one fact, and either one alone can be missing.
  const configured = new Set(sections.filter((s) => s.tool.configured).map((s) => s.tool.key));
  return entry.surfaces
    .map((s) => s.key)
    .filter((key) => configured.has(key) || rows.has(key))
    .reverse();
}

/**
 * What is wrong with one surface, as tags.
 *
 * TWO READERS, ONE COPY. The card's surface row draws these under a tool the
 * operator has opened, and the peek draws them beside EVERY surface whether or
 * not it is faulted — the peek answers "is this the one I meant", and a
 * surface that renders nothing when it is healthy cannot answer it. Written
 * twice, the two would disagree about what an unresolved secret means the
 * first time either one gained a badge.
 *
 * EVERY ONE OF THEM IS A FALSE, never a missing value: each of these fields is
 * three-valued and `null` is "nothing here can say", which is not a fault to
 * badge a surface with.
 */
function SurfaceBadges({ row }: { row: IntegrationRow }) {
  return (
    <>
      {row.secret_usable === false && (
        <Tag
          variant="warning"
          appearance="outline"
          title="A secret it needs isn’t set, so deliveries are refused"
        >
          secret unresolved
        </Tag>
      )}
      {row.routes === false && (
        <Tag variant="warning" appearance="outline" title="Deliveries arrive but reach no seat">
          routes nowhere
        </Tag>
      )}
      {row.endpoint_current === false && (
        <Tag
          variant="warning"
          appearance="outline"
          title="Registered at an old address, so deliveries are lost"
        >
          address moved
        </Tag>
      )}
      {row.enabled === false && <Tag appearance="outline">paused</Tag>}
    </>
  );
}

/**
 * The one thing a tile offers.
 *
 * FIVE KINDS, and every tile has exactly one of them. Three open the tool's
 * settings form (`connect`, `continue`, `rotate`), one goes to the tool's own
 * page (`manage`) and one to its documentation (`learn`). The label is the
 * kind's own word and never varies, so a test and a reader can both hold a
 * tile to it.
 */
export type TileAction =
  | { kind: "connect"; label: "Connect" }
  | { kind: "continue"; label: "Continue" }
  | { kind: "rotate"; label: "Rotate token" }
  | { kind: "manage"; label: "Manage" }
  | { kind: "learn"; label: "Learn more" };

const CONNECT: TileAction = { kind: "connect", label: "Connect" };
const CONTINUE: TileAction = { kind: "continue", label: "Continue" };
const ROTATE: TileAction = { kind: "rotate", label: "Rotate token" };
const MANAGE: TileAction = { kind: "manage", label: "Manage" };
const LEARN: TileAction = { kind: "learn", label: "Learn more" };

/**
 * The finding kinds a new credential fixes, and nothing else does.
 *
 * `credential_rejected` is a credential that resolved and that the vendor
 * refused; `credential_expiring` is one that works today and stops on a
 * published date (`integration.ExpiryWarning` ahead of it). Both are answered
 * by the same gesture — a new value typed over the held one, sealed, and the
 * revision re-activated — which is what Rotate token opens.
 *
 * NOT `credential_missing`: nothing is held to rotate, and the form's own
 * empty field is what Continue already opens.
 */
export const ROTATE_FINDINGS: ReadonlySet<string> = new Set([
  "credential_expiring",
  "credential_rejected",
]);

/** The credential findings the loop holds on a tool, across its surfaces. */
export function credentialFindings(present: Present[]): ReconcileFinding[] {
  return present.flatMap((p) =>
    (p.row.reconcile?.findings ?? []).filter((f) => ROTATE_FINDINGS.has(f.kind)),
  );
}

/**
 * A tile's one action, from what the engine says about the tool.
 *
 * ONE, because a tile in a grid is read in one glance, and a card
 * that carried a state tag, a Connect, a Disconnect, a settings square and a
 * chevron asked that glance to parse five controls to learn that nothing was
 * owed. Everything else a tool offers — its settings, its agents' own apps,
 * its passes, its deliveries, Disconnect — is on the tool's own page, which is
 * where Manage goes.
 *
 * A PURE FUNCTION of the same inputs the tag beside it is derived from, so the
 * two can never tell a reader different things. In order:
 *
 *  1. NOTHING CONFIGURED is Connect — or, where this build offers no form for
 *     the tool and nothing refused the read, Learn more: a Connect that
 *     discovers on a press that there is no form behind it is worse than
 *     sending somebody to the page that says how.
 *  2. A CARD IN MOTION is Manage. Between a connect and the loop's first
 *     report, and between asking for a disconnect and its finishing, nothing a
 *     person WRITES moves it — but where it is going is exactly what they want
 *     to watch, and Manage writes nothing.
 *  3. A CREDENTIAL THE VENDOR REFUSED OR IS ABOUT TO is Rotate token, ahead of
 *     everything else a configured tool can owe, because it is the one that
 *     lapses on a date whether or not anybody looks.
 *  4. A FORM LEFT UNFINISHED is Continue: a requirement the engine counts as
 *     unanswered, or a surface of a partly-connected tool nobody configured
 *     (Atlassian with the organization and not the products).
 *  5. Everything else is Manage.
 *
 * THE ROWS ARE THE FRESHER HALF, IN BOTH DIRECTIONS. The `integrations` rows
 * and the setup listing arrive separately, and the rows answer "does this
 * company have it" first — so rows present over a listing still calling every
 * surface unconfigured is a connect the listing has not caught up with
 * (Manage, not Continue), and rows gone over a listing still calling it
 * configured is a disconnect it has not caught up with (Connect).
 */
export function actionFor(
  state: EntryState,
  tools: SetupToolState[],
  /** Whether the ROWS have this tool. No default: absent is a real state. */
  present: boolean,
  /** The credential findings on the tool's configured surfaces. */
  credentials: ReconcileFinding[],
  /** Whether the setup listing was REFUSED, as opposed to answering nothing. */
  guarded: boolean,
): TileAction {
  if (!present) return tools.length > 0 || guarded ? CONNECT : LEARN;
  if (state.busy) return MANAGE;
  if (credentials.length > 0) return ROTATE;
  const configured = tools.filter((t) => t.configured);
  if (configured.length === 0) return MANAGE;
  // `form_complete` is the engine's own count of unanswered requirements,
  // company block and seats together, and it decides — never `satisfied`,
  // which folds the seats' OWN acts in too (a GitHub seat whose app is
  // created at GitHub, from its row on the tool's page): a Continue that
  // opened a form with nothing in it to answer would offer work there is no
  // box for.
  if (configured.some((t) => !t.form_complete)) return CONTINUE;
  if (tools.some((t) => !t.configured)) return CONTINUE;
  return MANAGE;
}

/**
 * The sections of one tool's dialog: the surfaces the engine reaches it over,
 * and for a third-party app whose credentials live on the SEAT, one per agent.
 *
 * Slack is the per-seat one, because each agent has its own app. Every agent
 * gets a section whether or not it has one yet: a seat with no app is exactly
 * the seat somebody opened this dialog to give one to.
 */
export function sectionsFor(
  entry: Entry,
  byKey: Map<string, SetupToolState>,
): { name: string; tool: SetupToolState; seat?: string }[] {
  const out: { name: string; tool: SetupToolState; seat?: string }[] = [];
  for (const surface of entry.surfaces) {
    const tool = byKey.get(surface.key);
    if (!tool) continue;
    // A SECTION PER SEAT IS SLACK'S SHAPE, and seats_required is what says
    // so: each agent has its own Slack app with its own credentials, so each
    // one is a form. Everywhere else a seat holds a credential written in
    // its mcp_env, which this dialog does not edit, and the roster on the
    // card is where it is reported.
    //
    // Without the flag every app with a roster split its dialog into one
    // section per agent, each repeating the company's own fields.
    if (tool.seats_required && tool.seats && tool.seats.length > 0) {
      // The company-wide block first, when it has anything to set, then a
      // section per seat.
      if (tool.requirements.length > 0) out.push({ name: surface.name, tool });
      for (const seat of tool.seats) {
        out.push({ name: seat.name || seat.handle, tool, seat: seat.handle });
      }
      continue;
    }
    out.push({ name: surface.name, tool });
  }
  return out;
}

/**
 * What the begin route answers: everything a browser needs to create one
 * agent's app, and nothing it could work out for itself.
 *
 * Declared here rather than in the protocol types, for the reason the setup
 * dialog declares its own answer shape there: it is the answer to one write
 * made on one screen, read once and merged into nothing.
 */
interface AppManifest {
  /** The account the app is created under: a person's, or an organization's. */
  action_url: string;
  /** The app's own declaration, sent as one JSON string. */
  manifest: Record<string, unknown>;
  /** The signed token that ties the code host's callback back to this seat. */
  state: string;
}

/**
 * Send the operator to the code host with the manifest, as a real form POST.
 *
 * A FORM RATHER THAN A FETCH, and nothing else can do this job. The request
 * carries the operator's OWN session at the code host, which is what decides
 * whether they may create an app on that organization at all, and the answer
 * is a page the host renders for them to confirm what is being created. A
 * fetch has neither half: it would arrive as this dashboard rather than as
 * the person, and the confirmation page would come back as a string with
 * nowhere to display it.
 */
function postManifest(answer: AppManifest): void {
  const form = document.createElement("form");
  form.method = "post";
  form.action = answer.action_url;
  // A NEW TAB, like the install link that replaces this button on the next
  // pass. The flow ends on the engine's own landing page, so submitting in
  // place would take the dashboard away and leave the operator with the
  // browser's history as the only route back to the card they started from.
  form.target = "_blank";
  form.rel = "noreferrer";
  const fields: [string, string][] = [
    // STRINGIFIED HERE. The engine answers the manifest as the JSON document
    // it is, and the form field the host reads is that document as text.
    ["manifest", JSON.stringify(answer.manifest)],
    ["state", answer.state],
  ];
  for (const [name, value] of fields) {
    const input = document.createElement("input");
    input.type = "hidden";
    input.name = name;
    input.value = value;
    form.append(input);
  }
  // IN THE DOCUMENT, because a form outside one does not submit at all. It
  // comes out again immediately: the submission is under way by then, and a
  // hidden form left in the page is one the next thing to go looking for a
  // form would find.
  document.body.append(form);
  form.submit();
  form.remove();
}

/**
 * The one act an agent's own app is waiting on, as the control that does it.
 *
 * TWO STEPS, TWO CONTROLS, because they are two acts by a person. One app is
 * one bot identity, so each agent has its own, and creating it and installing
 * it can be a day apart: an operator who has just created an app must be
 * shown what is left rather than the button they have already pressed.
 *
 * The tool is named rather than assumed, so the control says where it sends
 * somebody. This screen carries no vendor branch anywhere else and needs none
 * here: the step is the engine's own vocabulary and the route that begins one
 * is the surface's own.
 *
 * A STEP THIS BUILD DOES NOT KNOW DRAWS NOTHING. A newer node may name one,
 * and a control that guesses what it means is worse than no control at all.
 */
export function SeatStep({
  app,
  toolKey,
  seat,
  managed = null,
}: {
  /** The tool in the reader's own words, from the catalogue. */
  app: string;
  /** The engine surface this roster belongs to: what the route is keyed on. */
  toolKey: string;
  seat: SetupSeatState;
  /**
   * Why the company document cannot be changed from here — another system
   * manages it (ADR-0030) — or null. An app is recorded on the seat, so a
   * managed document holds its creation BEFORE the host makes one.
   */
  managed?: string | null;
}) {
  const [busy, setBusy] = useState(false);
  const [refused, setRefused] = useState("");

  async function create(): Promise<void> {
    setBusy(true);
    setRefused("");
    try {
      const answer = (await rest.post(`/setup/integrations/${pathSegment(toolKey)}/app`, {
        seat: seat.handle,
      })) as AppManifest;
      postManifest(answer);
    } catch (err) {
      setRefused(
        err instanceof RestError
          ? err.detail || err.hint || err.code || "The engine refused that."
          : String(err),
      );
    } finally {
      setBusy(false);
    }
  }

  if (seat.step === "install_app") {
    // NO ADDRESS, NO LINK. The install page is built from the slug the host
    // returned rather than from the name that was asked for, so a seat whose
    // app the engine cannot name has nowhere to send anybody, and an anchor
    // to nothing is a control that lies.
    if (!seat.action_url) return null;
    return (
      // PRIMARY, like the Create button it follows. These are the two acts
      // that build a seat's app and they are the same kind of thing — the one
      // control on the row a person is meant to press — so drawing the second
      // as an ordinary button made the finished half look optional.
      // A REAL ANCHOR WEARING THE BUTTON, which is what `ButtonLink` is for:
      // this goes somewhere rather than doing something, so it has to be
      // openable in a tab and copyable. `external` is what withholds the
      // referrer and the opener and draws the mark that says the press leaves
      // this application — the hand-written anchor carried the first half and
      // never the second.
      <ButtonLink variant="primary" size="small" external href={seat.action_url}>
        Install on {app}
      </ButtonLink>
    );
  }
  if (seat.step !== "create_app") return null;

  return (
    <>
      <Button
        size="small"
        variant="primary"
        disabled={busy}
        disabledReason={managed ?? undefined}
        onClick={() => {
          if (!managed) void create();
        }}
      >
        {busy ? `Opening ${app}` : `Create app on ${app}`}
      </Button>
      {refused && (
        <div className="int-row-note">
          <span className="int-row-note-text">{refused}</span>
        </div>
      )}
    </>
  );
}

/** The setup listing's tools behind a card's sections, once each. */
export function toolsOf(sections: { tool: SetupToolState }[] | undefined): SetupToolState[] {
  // BY KEY, because a per-seat app contributes one section per agent and
  // they all carry the same tool. Counting it once per section made a roster
  // of one agent render four rows on the Atlassian card.
  return [...new Map((sections ?? []).map((s) => [s.tool.key, s.tool])).values()];
}

/**
 * The roster a tool reports, once: one row per agent whatever the tool is
 * made of.
 *
 * A company with one agent saw THREE rows on Atlassian — Organization, Jira,
 * Confluence — because every surface reports its own roster and the card
 * listed them all. They are one person's one account. The surface whose seats
 * carry their own FORM wins (Slack's app per agent), then the one that
 * PROVISIONS, because its answer is about the account rather than about a
 * credential slot; where neither, the first roster with anything in it is as
 * good as any, since they read the same seat's mcp_env.
 */
export function rosterOf(tools: SetupToolState[]): SetupToolState | undefined {
  const rosters = tools.filter((t) => (t.seats ?? []).length > 0);
  return (
    rosters.find((t) => t.seats_required) ?? rosters.find((t) => t.can_provision) ?? rosters[0]
  );
}

/**
 * Why an action that opens the settings form cannot be pressed, or undefined.
 *
 * A WRITE CONTROL IS NEVER HIDDEN (dashboard rule 4): it is disabled with the
 * one sentence that says why. `/setup` is guarded in full, so a reader the
 * engine refused has no form to open, and a listing that answered nothing for
 * this tool has none either — which is a different sentence, because no
 * access would help.
 *
 * THE ACCESS, NAMED: reading what a tool needs and connecting it are both
 * Admin's.
 */
export function formBlocked(sections: unknown[], guarded: boolean): string | undefined {
  if (sections.length > 0) return undefined;
  return guarded
    ? needsWords("Setting up integrations", [ADMIN])
    : "The engine did not say what this integration needs, so there is no form to open.";
}

/**
 * A finding that describes something WORKING.
 *
 * Two of the engine's kinds have a verdict of `ready` — a permission wider
 * than the role asked for, and a registration the engine no longer manages
 * but which is still delivering correctly. Both are notes on a healthy
 * integration, and `Classify` ranks them beneath every real problem for
 * exactly that reason.
 *
 * Read off the finding's OWN phase, which the engine sends from its per-kind
 * verdict table, rather than from a list of advisory kinds kept here: a
 * second copy of a closed set is a copy that stops matching, and the failure
 * direction is the bad one — a kind this build had not heard of would be
 * treated as an advisory and could then hide a broken agent.
 *
 * An ABSENT phase is not an advisory: "cannot say" must read as a fault so the
 * badge stays honest.
 */
export function advisory(f: ReconcileFinding): boolean {
  return f.phase === "ready";
}

/**
 * What each tool still needs, from /setup.
 *
 * A SECOND READ, and it has to be: /integrations is an ordinary read served
 * to anybody the anonymous-read posture allows, while this one is guarded in
 * full because it names the credentials a company holds. A deployment where
 * the operator has no token still gets the whole screen, minus the buttons.
 */
export function useSetup(): {
  byKey: Map<string, SetupToolState>;
  base: SetupListing["external_url"] | null;
  guarded: boolean;
  /** The refusal, when [guarded] — said through `lib/refusal.ts`'s `authorityWords`. */
  refused: RestError | null;
  /**
   * True until this read has answered ONCE, however it answered.
   *
   * The two halves of this screen arrive separately, and the socket's is
   * quicker: a card rendered from it alone has no tools, and a card with no
   * tools offers no buttons. So the row appeared with no status and no
   * Connect or Disconnect beside it, and stayed that way until something
   * else made the page re-render, which is why it looked like a refresh
   * fixed it. Nothing was wrong; the answer had not arrived.
   *
   * TRUE AGAIN WHILE A RE-READ IS IN FLIGHT, because the same disagreement
   * happens in both directions. After a connect the rows say the block
   * exists while this half still says it does not, and the card offered
   * Connect beside Connected. After a disconnect the rows are gone while
   * this half still says configured and satisfied, and the card had no tag
   * and no button at all. A card rendered from one half is wrong either way,
   * and a moment of the skeleton already on this screen is honest about
   * which of the two it is: nothing is known yet.
   */
  loading: boolean;
  reload: () => void;
} {
  // THE ONE REST LOADER, which supersedes a read that is still in flight: four
  // things start one here — mount, a sign-in, the tab becoming visible
  // and useRecheck, which fires the same read twice 700 ms apart — and before
  // it every answer was written into state unconditionally, so whichever
  // landed last was what the screen held.
  //
  // AN AGENT'S APP IS SET UP AT THE CODE HOST, IN ANOTHER TAB, and this
  // listing is the only thing that carries the roster: nothing pushes it, and
  // no answer this screen holds says when a person finished creating an app.
  // So it is re-read when the tab comes back, quietly — a background refresh
  // has no moment where the two halves disagree, and blanking six cards
  // because somebody came back to the tab would report an absence that is not
  // there. Every re-read a PERSON triggers is loud, for the reason `loading`
  // gives.
  const setup = useRest(
    "/setup/integrations",
    (signal) => rest.get("/setup/integrations", signal) as Promise<SetupListing>,
    { refetchOnFocus: true },
  );
  // A REFUSAL IS NOT AN EMPTY ANSWER. The loader drops the listing on one, so
  // the screen keeps every read the socket gave it and simply offers no
  // writes; `guarded` is what tells a refused read from a missing one.
  const listing = setup.data;
  const { reload } = setup;
  return {
    byKey: new Map((listing?.tools ?? []).map((t) => [t.key, t])),
    base: listing?.external_url ?? null,
    guarded: setup.error?.unauthorized ?? false,
    refused: setup.error?.unauthorized ? setup.error : null,
    // ANSWERED, not answered WELL. A refusal is a state the screen can render
    // honestly, with the banner and no buttons; waiting is not.
    loading: setup.loading,
    reload: useCallback(() => void reload(), [reload]),
  };
}

/**
 * Why a disconnect this tool has already been asked for has not finished.
 *
 * Empty when it is not disconnecting, or when it is and nothing has failed
 * yet — a teardown in flight is not a teardown stuck, and offering the way
 * out of one that is simply running invites somebody to abandon it a second
 * after asking.
 */
export function stuckDisconnecting(entry: Entry, rows: Map<string, IntegrationRow>): string {
  for (const surface of entry.surfaces) {
    const reconcile = rows.get(surface.key)?.reconcile;
    if (reconcile?.disconnecting && reconcile.last_error) {
      return reconcile.last_error;
    }
  }
  return "";
}

/**
 * What actually arrived on one surface, newest first.
 *
 * FROM THE LISTING, never one payload per row. The event list returns a row's
 * summary, source, type and tags and deliberately NOT its payload — so a page
 * of deliveries is one request, and the raw body an operator wants for exactly
 * one of them is a click to the event's own page. The `#/model` screen fetched
 * a payload per row and paid sixty-one round trips for one screen.
 */
/**
 * A delivery row's event, as the provider named it: the row's type without the
 * engine's own filing prefix, which says which edge it came in on — a webhook
 * route, the Forge relay, or a chat socket.
 */
export function deliveryEvent(type: string): string {
  return type.replace(/^(webhook|forge|socket):/, "");
}

export function SurfaceDeliveries({ surface, name }: { surface: string; name: string }) {
  const nav = useNavigator();
  const now = useNow();
  const org = useOrg();
  // WHO A DELIVERY REACHED. The tag carries a HANDLE, which is an address
  // rather than a label: passed straight through as the name it put
  // `agent-ai-systems-engineer` where the seat's name belongs, built the
  // badge's monogram out of it, and drew every recipient as an agent.
  const who = useMemo(() => seatLookup(indexOrg(org)), [org]);
  // Not pushed — a webhook row reaches the live stream, but this is a page of
  // history and a delivery arrives on the provider's schedule rather than
  // this screen's. A minute is the cadence "is anything arriving at all" is
  // asked at, which is the same one the traffic counters use.
  const deliveries = useQuery(
    "events",
    { category: "webhook", source: surface, limit: DELIVERY_PAGE },
    { pollMs: 60_000, refetchOnFocus: true },
  );
  const rows = deliveries.data?.events ?? [];

  return (
    <Card padding="none">
      <Card.Header
        icon={<InboxGlyph size="sm" />}
        count={rows.length}
        subtitle="what the provider actually sent, newest first"
      >
        {`${name} deliveries`}
      </Card.Header>
      <QueryState
        error={deliveries.error}
        refusal={deliveries.refusal}
        loading={deliveries.loading}
        empty={
          deliveries.data && rows.length === 0
            ? {
                title: `Nothing has arrived on ${name}`,
                hint: `Check that ${name} is sending. Refusals are in the engine log.`,
              }
            : undefined
        }
      >
        {rows.length > 0 && (
          <DataGrid<EventRecord>
            rows={rows}
            rowKey={(e) => e.id}
            defaultSort="-at"
            // THE ROW IS A REAL LINK to the delivery's own event, so the
            // middle button, ⌘-click and the status bar all behave — which
            // they did not, because the row carried a click handler and no
            // address at all.
            //
            // AND A PLAIN CLICK STILL NAVIGATES, which is the opposite of the
            // rule every list of a PEEKABLE kind follows. `event` does have a
            // peek (`app/frame/peeks.tsx` registers one), so this is a choice
            // rather than a fallback: the RAW DELIVERY is the whole reason to
            // open a webhook row — what the provider actually sent, which is
            // what an operator is here to read against what the engine did
            // with it — and that is a page rather than a panel.
            rowHref={(e) => href(["live", "events", e.id])}
            onRowActivate={(e, event) => {
              // THE ANCHOR DOES THE MOUSE. What it never sees is the grid's
              // own `enter` chord, which carries no button because it is not
              // a mouse event at all.
              if (!("button" in event)) nav.to(["live", "events", e.id]);
            }}
            empty={{ title: "No delivery matches" }}
            columns={[
              {
                key: "at",
                align: "right",
                header: "Arrived",
                shrink: true,
                sortValue: (e) => tsKey(e.timestamp),
                cell: (e) => <TableDateCell value={e.timestamp} now={now} />,
              },
              {
                key: "event",
                header: "Event",
                shrink: true,
                sortValue: (e) => e.type,
                // `webhook:`, `forge:` and `socket:` are the engine's own
                // filing prefixes — which edge a delivery came in on — and
                // the provider's event name is what an operator is matching
                // against their own console.
                cell: (e) => <InlineCode whole>{deliveryEvent(e.type)}</InlineCode>,
              },
              {
                key: "summary",
                header: "What it said",
                cell: (e) => <TableTextCell>{e.summary}</TableTextCell>,
              },
              {
                key: "for",
                header: "Addressed to",
                shrink: true,
                sortValue: (e) => e.tags?.recipient ?? "",
                cell: (e) =>
                  e.tags?.recipient ? (
                    // THE SEAT AS IT LOOKS EVERYWHERE ELSE — the mark and the
                    // name, linking to the seat. A delivery is addressed to a
                    // colleague, and a colleague should not be a chip on this
                    // grid and an avatar on the next.
                    <TableEntityCell
                      {...linkedSeat({ handle: e.tags.recipient, ...who(e.tags.recipient) })}
                    />
                  ) : (
                    // NOT THE CELL'S OWN "nobody" DASH: a company-wide
                    // delivery is routed by the notification spine rather than
                    // addressed in the URL, and calling that unaddressed would
                    // read as a delivery that reached no one.
                    <span className="t-caption">the company</span>
                  ),
              },
              {
                key: "key",
                header: "Provider id",
                shrink: true,
                sortValue: (e) => e.tags?.delivery_key ?? "",
                cell: (e) =>
                  e.tags?.delivery_key ? (
                    <TableKeyCell value={e.tags.delivery_key} />
                  ) : (
                    // A DASH THAT SAYS WHICH ABSENCE THIS IS. Not every
                    // provider sends an id of its own, and a blank cell and an
                    // id nobody sent are different facts.
                    <EmptyValue label="This provider sent no delivery id" />
                  ),
              },
            ]}
          />
        )}
      </QueryState>
      {rows.length >= DELIVERY_PAGE && (
        // `Card.Footer` RATHER THAN OUR `.panel-foot`: this band is the
        // design system's meta footer, which is the same thing the class drew
        // — a rule, a quiet register and the card's own horizontal padding.
        <Card.Footer variant="meta">
          <span className="t-caption">
            Newest {DELIVERY_PAGE}. Older ones are in the{" "}
            <a className="prose-link" href={href(["live", "events"], { category: "webhook" })}>
              event log
            </a>
            .
          </span>
        </Card.Footer>
      )}
    </Card>
  );
}

/**
 * One page of deliveries.
 *
 * Sized to a screen rather than to the log: this is "what has been arriving",
 * and the event log is where a reader goes to page through history.
 */
const DELIVERY_PAGE = 50;

// ---------------------------------------------------------------------------
// The provisioning passes
// ---------------------------------------------------------------------------

/**
 * What `GET /setup/integrations/{kind}/runs` answers.
 *
 * Declared here rather than in the protocol types, for the reason the setup
 * dialog gives for its own answer shape: it is the ENVELOPE of one read made
 * on one screen. The record inside it is `SetupRun`, which is shared — the
 * same run comes back from a pass this dashboard starts, and from
 * `runs/{id}`.
 */
interface RunListing {
  runs: SetupRun[];
  /**
   * Whose history this is, in the engine's own words ("this node").
   *
   * A pass is executed by whichever node held the surface's lease and is
   * remembered in THAT node's process, so an empty list on a fleet is an
   * honest answer to a question the reader did not mean to ask. The engine
   * sends the scope for that reason and the panel prints it.
   */
  scope?: string;
}

/**
 * How often a pass that is still running is asked about.
 *
 * FOUR SECONDS — the cadence this screen already uses while something is
 * settling, and it is bounded by the pass itself rather than picked: a run is
 * stopped at `setup.PassDeadline`, four minutes, so a pass still `running`
 * past that is a node that stopped existing mid-pass rather than work in
 * progress. A slower poll would leave an operator watching "running" for most
 * of a minute after the third-party app had already answered, which is the
 * delay they read as the failure.
 */
const PASS_POLL_MS = 4_000;

/**
 * How often the history is re-read when nothing is running.
 *
 * A MINUTE, the cadence this screen already reads its traffic counters at, and
 * it is not idleness that earns it: a pass is started by things this panel
 * cannot see — the loop's own tick, a connect in the dialog above it, another
 * operator pressing recheck on another node — so a panel that only polled
 * while it already knew a pass was running would report "no pass has run"
 * across the whole of the first one.
 */
const PASS_IDLE_POLL_MS = 60_000;

/**
 * The passes this node has run for a tool, newest first.
 *
 * ONE READ PER KIND, because the route is keyed on one and a card is several:
 * Atlassian's organization, Jira and Confluence each converge separately and
 * each has a history of its own. They are merged on the clock rather than
 * drawn as three tables, because what an operator is reading is a history of
 * one TOOL and which surface it was is a column of it.
 *
 * ONLY WHAT IS A KIND. `forge` is a surface of the Atlassian card and is not
 * in `integration.Kinds`, so the route answers 404 for it — the caller passes
 * the keys the setup listing carries, which is that same set.
 *
 * REST, LIKE [useSetup], AND FOR THE SAME REASONS: no query in the socket's
 * registry answers anything about a pass, and `/setup` is guarded in full —
 * so a reader without Admin is REFUSED here rather than shown an
 * empty history, and `guarded` is what tells those two apart.
 */
function useSetupRuns(kinds: string[]): {
  runs: SetupRun[];
  scope: string;
  guarded: boolean;
  refused: RestError | null;
  loading: boolean;
} {
  // THE KEY IS THE DEPENDENCY, not the array. A caller derives its kinds from
  // the catalogue on every render, so a loader keyed on the array itself
  // would re-read this several times a second. NOTHING TO ASK IS NOT A
  // LOADING STATE: with no kind there is no route, and a panel that spun for
  // ever would be reporting a wait on a request nobody made.
  const key = kinds.join(",");
  const [running, setRunning] = useState(false);
  // TWO CADENCES, ONE LOOP. A pass in flight ends on its own inside
  // `setup.PassDeadline` and this list is the only place that says how it
  // ended, so it is watched at [PASS_POLL_MS]; the rest of the time the
  // history still moves without this panel touching anything, which is what
  // [PASS_IDLE_POLL_MS] is for. QUIET, both of them — the loader's polls are —
  // since a re-read that blanked the table into its skeleton once a minute
  // would take the row an operator was reading out from under them to say
  // nothing new.
  //
  // AND WHENEVER THIS TAB COMES BACK. Connecting an integration means leaving
  // for the third-party app and returning, and the pass that ran while the
  // reader was away is the one they came back to read — the same argument
  // [useSetup] makes for re-reading its listing, on the same event.
  const answer = useRest(
    key === "" ? null : key,
    async (signal) => {
      const answers = (await Promise.all(
        key
          .split(",")
          .map((one) => rest.get(`/setup/integrations/${pathSegment(one)}/runs`, signal)),
      )) as RunListing[];
      return {
        runs: answers
          .flatMap((one) => one.runs ?? [])
          .sort((a, b) => tsKey(b.started_at) - tsKey(a.started_at)),
        scope: answers.find((one) => one.scope)?.scope ?? "",
      };
    },
    { pollMs: running ? PASS_POLL_MS : PASS_IDLE_POLL_MS, refetchOnFocus: true },
  );
  const runs = answer.data?.runs ?? EMPTY_RUNS;
  const nowRunning = runs.some((run) => run.state === "running");
  useEffect(() => setRunning(nowRunning), [nowRunning]);

  return {
    runs,
    scope: answer.data?.scope ?? "",
    guarded: answer.error?.unauthorized ?? false,
    refused: answer.error?.unauthorized ? answer.error : null,
    loading: answer.loading,
  };
}

/** One shared empty history, so a panel with none keeps one identity. */
const EMPTY_RUNS: SetupRun[] = [];

/**
 * ONE pass, read fresh.
 *
 * WHY NOT THE ROW THAT WAS CLICKED: the listing is a snapshot of the moment it
 * answered, and it is capped at ten (`setupapi.MaxRunsListed`) against the
 * runner's own memory of thirty-two. This route answers a single pass —
 * including one the listing has already dropped, and including one that is
 * still going, which is what makes a running pass readable WHILE it runs
 * rather than by fetching every other pass's findings again beside it.
 *
 * `missing` is the engine's own 404 and is NOT an error to draw in red: a run
 * is remembered by the node that executed it and only for its last few passes,
 * so ageing out is the ordinary end of one's life. What it concluded is folded
 * into the surface's state either way.
 */
function useSetupRun(
  kind: string,
  id: string,
): {
  run: SetupRun | null;
  missing: boolean;
  guarded: boolean;
  refused: RestError | null;
  loading: boolean;
} {
  // FOLLOWED TO ITS END, and only while it is going: the row that opened this
  // may have been a pass that was running when the list answered.
  const [running, setRunning] = useState(false);
  const answer = useRest(
    kind === "" || id === "" ? null : `${kind}/${id}`,
    (signal) =>
      rest.get(
        `/setup/integrations/${pathSegment(kind)}/runs/${pathSegment(id)}`,
        signal,
      ) as Promise<SetupRun>,
    { pollMs: running ? PASS_POLL_MS : undefined },
  );
  const run = answer.data;
  const nowRunning = run?.state === "running";
  useEffect(() => setRunning(nowRunning), [nowRunning]);

  return {
    run,
    missing: answer.error?.status === 404,
    guarded: answer.error?.unauthorized ?? false,
    refused: answer.error?.unauthorized ? answer.error : null,
    loading: answer.loading,
  };
}

/**
 * How a pass ended, in the vocabulary the rest of this screen already uses.
 *
 * THE RUN'S REPORT IS WHERE THE TONE COMES FROM, not its findings. A run
 * carries its findings RAW — `setup.Run` marshals `integration.Finding` itself
 * — so unlike the socket's reconcile rows they arrive with no verdict on them
 * (the query surface is what adds `phase`, from `FindingKind.Verdict`), and
 * colouring one here would be this build inventing a severity. What the run
 * does carry is `report`, which IS the engine's classification of exactly
 * those findings, so the pass wears the phase its own findings classify to.
 *
 * A STATE THIS BUILD DOES NOT KNOW IS DRAWN AS ITSELF, in neutral — the same
 * rule [phaseTone] follows, and for the same reason.
 */
function passState(run: SetupRun): { glyph: string; label: string; tone: Tone; title: string } {
  switch (run.state) {
    case "running":
      return {
        glyph: "◐",
        label: "running",
        tone: "info",
        title: "Still running",
      };
    case "failed":
      return {
        glyph: "✕",
        label: "failed",
        tone: "danger",
        title: run.error || "The app refused this pass",
      };
    case "done": {
      const phase = run.report?.phase ?? "";
      return {
        glyph: "●",
        // THE ENGINE'S OWN WORD, with its underscores opened — which is all
        // this build can honestly do with it. A run's report carries the
        // phase and NOT the `phase_label` the socket's rows do: that label is
        // derived in Go on the query surface, and a marshalled
        // `integration.Report` never passes through it.
        label: phase === "" ? "done" : phase.replace(/_/g, " "),
        tone: phaseTone(phase),
        title: run.report?.detail || "the pass ran and the third-party app answered",
      };
    }
    default:
      return {
        glyph: "○",
        label: run.state || "unknown",
        tone: "neutral",
        title: "Unknown state from a newer version",
      };
  }
}

/**
 * How long a pass took, or null while it is still going.
 *
 * NULL RATHER THAN "now minus started", which is the obvious alternative and
 * is a different measurement: a run whose node died mid-pass would report a
 * duration that grows for ever. [TableNumberCell] draws "not measured" for this,
 * which is exactly what it is until the run ends.
 */
function passMs(run: SetupRun): number | null {
  if (!run.ended_at) return null;
  const ms = tsKey(run.ended_at) - tsKey(run.started_at);
  return ms >= 0 ? ms : null;
}

/** The one line a pass's outcome reduces to, for the column that has one. */
function concludedLine(run: SetupRun): string {
  // THE FAULT FIRST. A pass that could not run has no report to classify, and
  // the engine's sentence is the whole of what happened.
  if (run.error) return run.error;
  if (run.report?.detail) return run.report.detail;
  if (run.state === "running") return "still running";
  const found = run.findings?.length ?? 0;
  return found === 0 ? "nothing outstanding" : plural(found, "finding");
}

/**
 * What the reconcile passes actually did.
 *
 * THE GAP THIS CLOSES. Every finding on this screen comes from a pass, and
 * nothing anywhere said that one had ever run: the card reports what the LAST
 * pass concluded and the loop's cadence, which answers "what is wrong" and
 * never "what has been happening". An operator who fixed something at the
 * third-party app could not tell a surface that had just been checked from one
 * nothing had looked at in an hour — and a pass running right now, which is
 * the state most worth seeing, was invisible until it ended.
 *
 * THE FINDINGS ARE THE POINT OF A ROW, so opening one reads the pass itself
 * rather than expanding the row's own copy — see [useSetupRun].
 */
export function SetupPasses({ entry, kinds }: { entry: Entry; kinds: string[] }) {
  const now = useNow();
  const { runs, scope, guarded, refused, loading } = useSetupRuns(kinds);
  // WHICH PASS IS OPEN, as the surface AND the id rather than the id alone:
  // the detail route is keyed on both, and a tool has several kinds — an id on
  // its own could not say which surface's pass it was once the listing that
  // carried it had moved on.
  const [open, setOpen] = useState<{ kind: string; id: string } | null>(null);
  const named = useMemo(
    () => new Map(entry.surfaces.map((surface) => [surface.key, surface.name])),
    [entry],
  );

  if (guarded) {
    // NOT AN EMPTY HISTORY. `/setup` is guarded in full, reads included, so
    // this reader is being refused rather than told nothing has run — and the
    // refusal names the grant that would have admitted them.
    return (
      <Card>
        <Card.Header icon={<RotateCwGlyph size="sm" />}>
          <Card.Title>Provisioning passes</Card.Title>
        </Card.Header>
        <span className="t-caption">{authorityWords("Seeing passes", refused)}</span>
      </Card>
    );
  }

  const columns: DataGridColumn<SetupRun>[] = [
    {
      key: "ran",
      align: "right",
      header: "Ran",
      shrink: true,
      sortValue: (run) => tsKey(run.started_at),
      cell: (run) => <TableDateCell value={run.started_at} now={now} />,
    },
    // THE SURFACE ONLY WHERE THERE IS MORE THAN ONE. A column whose every
    // value is the tool's own name is a column that says nothing.
    ...(kinds.length > 1
      ? [
          {
            key: "surface",
            header: "Surface",
            shrink: true,
            sortValue: (run: SetupRun) => named.get(run.key) ?? run.key,
            cell: (run: SetupRun) => <TableTextCell>{named.get(run.key) ?? run.key}</TableTextCell>,
          },
        ]
      : []),
    {
      key: "state",
      header: "How it ended",
      shrink: true,
      sortValue: (run) => run.state,
      cell: (run) => {
        const state = passState(run);
        return (
          <TableStatusCell
            glyph={state.glyph}
            label={state.label}
            title={state.title}
            tone={state.tone}
          />
        );
      },
    },
    {
      key: "took",
      header: "Took",
      shrink: true,
      align: "right",
      // A RUNNING PASS SORTS LAST rather than as zero: it has no duration
      // yet, and a column sorted by "shortest" that put every live pass at
      // the top would be ordering on a measurement nobody made.
      sortValue: (run) => passMs(run) ?? -1,
      cell: (run) => (
        <TableNumberCell value={passMs(run)} format={fmtDuration} emptyLabel="Not measured" />
      ),
    },
    {
      key: "findings",
      header: "Findings",
      shrink: true,
      align: "right",
      sortValue: (run) => run.findings?.length ?? 0,
      // ZERO IS THE GOOD ANSWER HERE, and it has to look like one: a
      // converged third-party app reports an EMPTY slice rather than saying it
      // is fine (`integration.Classify`), so "found nothing" and "nothing
      // recorded" must not draw the same mark. That is the whole of
      // [TableNumberCell].
      cell: (run) => (
        <TableNumberCell
          value={run.findings?.length ?? 0}
          title="what this pass observed that was not fine"
          format={fmtCount}
        />
      ),
    },
    {
      key: "detail",
      header: "What it concluded",
      cell: (run) => <TableTextCell>{concludedLine(run)}</TableTextCell>,
    },
  ];

  return (
    <Card padding="none">
      <Card.Header
        icon={<RotateCwGlyph size="sm" />}
        count={runs.length}
        subtitle={`on ${scope || "this node"}`}
      >
        <Card.Title>Provisioning passes</Card.Title>
      </Card.Header>
      <DataGrid<SetupRun>
        rows={runs}
        rowKey={(run) => run.run_id}
        defaultSort="-ran"
        isSelected={(run) => run.run_id === open?.id}
        // A ROW OPENS THE PASS, in place. There is no page for a run and no
        // peek either — a pass is not one of the kinds `app/frame/objects.ts`
        // addresses, because it is one node's memory of a few minutes' work
        // rather than an object with a life of its own — so what a row
        // discloses is its findings, under the list it came from.
        onRowActivate={(run) =>
          setOpen((was) => (was?.id === run.run_id ? null : { kind: run.key, id: run.run_id }))
        }
        // WHICH ABSENCE THIS IS. A read still in flight and a node that has
        // run nothing are the same empty array, and only one of them is a
        // fact about the integration — a table that announced "no pass has
        // run" for the half second before its answer landed would be the
        // alarming one, said first.
        empty={
          loading
            ? { title: "Reading this node's passes", icon: "rotate-cw" }
            : {
                title: "No pass has run on this node",
                icon: "rotate-cw",
                hint: "Passes run on a schedule and when you connect.",
              }
        }
        columns={columns}
      />
      {open && (
        // AT THE FOOT OF THE LIST IT CAME FROM, ruled off and inset the way
        // every other trailing note on a panel is — the grid itself is flush
        // to the panel's edges, as every grid in this product is.
        <Card.Footer variant="meta">
          <PassDetail
            kind={open.kind}
            id={open.id}
            name={named.get(open.kind) ?? open.kind}
            now={now}
          />
        </Card.Footer>
      )}
    </Card>
  );
}

/**
 * One pass, opened: what it found, and what it could not do about it.
 *
 * THE FINDINGS ARE LAID OUT THE WAY THE CARD'S ARE — what is wrong, which
 * things, what to do — because they are the same findings, arriving by another
 * route. What is deliberately NOT here is a tone per finding: see [passState],
 * a run's findings carry no verdict, and the pass's own phase is the honest
 * place to put one.
 */
function PassDetail({
  kind,
  id,
  name,
  now,
}: {
  kind: string;
  id: string;
  /** The surface in the reader's words, from the catalogue. */
  name: string;
  now: number;
}) {
  const { run, missing, guarded, refused, loading } = useSetupRun(kind, id);

  if (loading && !run) return <Skeleton variant="text" rows={4} label="Loading" />;
  if (missing) {
    return (
      <div className="int-row-note">
        <span className="int-row-note-text">
          <span className="int-note-problem">This pass is no longer kept.</span>
          <span className="int-row-note-when">Its result is in the card above.</span>
        </span>
      </div>
    );
  }
  if (guarded) {
    return (
      <div className="int-row-note">
        <span className="int-row-note-text">
          <span className="int-row-note-when">{authorityWords("Reading one pass", refused)}</span>
        </span>
      </div>
    );
  }
  if (!run) return null;

  const state = passState(run);
  const findings = run.findings ?? [];
  return (
    <div className="int-row-note">
      <div className="int-row-note-text">
        <p className="int-note-problem">
          {name} · {state.label}
        </p>
        <span className="int-row-note-when">
          started {fmtDateTime(run.started_at)} ({relTime(run.started_at, now)})
          {run.ended_at ? `, ended ${fmtDateTime(run.ended_at)}` : ""}
        </span>
        {/* THE FAULT, WHERE THERE IS ONE. A pass that could not run has no
            findings to show and the engine's sentence is the whole answer. */}
        {run.error && <p className="int-note-problem">{run.error}</p>}
        {findings.length > 0 ? (
          <ul className="col int-findings">
            {findings.map((finding, at) => (
              <li key={`${finding.kind}:${finding.subject ?? ""}:${at}`} className="col gap-1">
                <span className="int-note-problem">
                  {finding.detail ||
                    `${finding.kind.replace(/_/g, " ")}${finding.subject ? `: ${finding.subject}` : ""}`}
                </span>
                <FindingSubjects subjects={finding.subjects ?? []} />
                {finding.remedy && <span className="int-note-remedy">{finding.remedy}</span>}
              </li>
            ))}
          </ul>
        ) : (
          <span className="int-row-note-when">
            {run.state === "running"
              ? "Nothing outstanding so far."
              : "This pass found nothing outstanding."}
          </span>
        )}
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// The peek
// ---------------------------------------------------------------------------

/**
 * Every finding the loop holds about a tool, worst first.
 *
 * ACROSS SURFACES, because a tool is one thing to a reader and three answers
 * to the engine. The order is the finding's OWN verdict: the advisory ones —
 * `phase: "ready"`, something the engine did not do on an integration that
 * works — sink below everything outstanding rather than being dropped, because
 * an operator reading what is wrong is entitled to the notes as well.
 *
 * The surface travels with each one, since the peek rolls three answers into
 * one list and "which of them said this" is the first thing a reader asks of
 * it.
 */
function openFindings(present: Present[]): { surface: string; finding: ReconcileFinding }[] {
  return present
    .flatMap((p) =>
      (p.row.reconcile?.findings ?? []).map((finding) => ({ surface: p.surface.name, finding })),
    )
    .sort((a, b) => Number(advisory(a.finding)) - Number(advisory(b.finding)));
}

/**
 * The newest delivery on one surface, or the honest absence of one.
 *
 * ONE ROW, ONE READ, and the read is keyed on the SURFACE because that is what
 * an event row carries as its source. A single read over the whole `webhook`
 * category could not answer this: a busier integration's page would push this
 * tool's last delivery off the end, and a short page would read as silence —
 * which on the panel that exists to say whether anything is arriving is the
 * one wrong answer.
 */
function LastDelivery({ surface, name, now }: { surface: string; name: string; now: number }) {
  // A minute, the cadence "is anything arriving at all" is asked at — the same
  // one the deliveries panel and the traffic counters use. A delivery arrives
  // on the provider's schedule rather than this panel's.
  const deliveries = useQuery(
    "events",
    { category: "webhook", source: surface, limit: 1 },
    { pollMs: 60_000, refetchOnFocus: true },
  );
  const row = deliveries.data?.events?.[0];
  return (
    <li className="int-row">
      <div className="int-row-identity">
        <span className="int-row-name">{name}</span>
        <span className="int-row-detail">
          {/* A FAILED READ IS NOT SILENCE. This is the only thing on the peek
              that can see the event log, so nothing else contradicts it, and
              "nothing has arrived" over a read that never answered is the
              alarming answer given on no evidence. */}
          {deliveries.error
            ? "this node could not read its event log"
            : deliveries.loading && !deliveries.data
              ? "reading"
              : row
                ? row.summary
                : "nothing has arrived on this surface"}
        </span>
      </div>
      {row ? (
        <TableDateCell value={row.timestamp} now={now} />
      ) : (
        <EmptyValue label="No delivery is recorded for this surface" />
      )}
    </li>
  );
}

/**
 * One integration, beside whatever brought the reader to it.
 *
 * THE TOOL, NOT THE SURFACE, and by the SAME lookup the screen behind it uses:
 * a finding and a webhook route both name the surface (`jira`) while the card
 * is the tool (`atlassian`), so both addresses resolve here to the object the
 * page would have shown. A peek that resolved them differently would be a
 * second answer to "what is this", which is the one thing the frame's header
 * exists to stop.
 *
 * IT ASKS FOR ITS OWN ROWS. The catalogue is static and needs no read, but
 * what an integration is DOING is the socket's answer, and this is opened from
 * a pasted URL as often as from a row — see `app/frame/peeks.tsx`.
 *
 * THE CHROME IS THE FRAME'S. No rail, no way out, no close: those are the same
 * on every kind, and this renders the integration's own content and nothing
 * else.
 */
export function IntegrationPeek({ kind }: { kind: string }) {
  const now = useNow();
  const { data, loading, error, refusal } = useQuery("integrations", undefined, {
    enabled: kind !== "",
    // The same slow cadence the screen uses at rest: traffic counters move
    // slowly, and a peek is read for seconds rather than watched.
    pollMs: 60_000,
    refetchOnFocus: true,
  });
  const rows = useMemo(
    () => new Map((data?.integrations ?? []).map((row) => [row.key, row])),
    [data],
  );
  const entry = CATALOG.find((e) => e.key === kind || e.surfaces.some((s) => s.key === kind));

  if (!entry) {
    return (
      <EmptyState
        size="compact"
        icon={<PlugGlyph size="xl" />}
        title={`This build serves no integration called “${kind}”`}
        description="See Company › Integrations for the ones available."
      />
    );
  }

  const present = presentSurfaces(entry, rows);
  const state = entryState(entry, rows, data?.tools);
  const findings = openFindings(present);

  return (
    <>
      <ObjectHeader
        size="peek"
        kind="Integration"
        icon="plug"
        identifier={entry.key}
        mark={<VendorMark vendor={entry.vendor} size={28} />}
        title={entry.name}
        status={
          state.tag === "" ? undefined : (
            <Tag variant={state.tone} appearance={state.outline ? "outline" : "soft"}>
              {state.tag}
            </Tag>
          )
        }
        // NO FACT LINE OVER A TOOL NOBODY HAS CONNECTED: every one of these is
        // a measurement of traffic that cannot exist yet, and five em dashes
        // under a name is a card reporting an absence rather than the absence
        // itself. The panel below says the one true thing instead.
        facts={
          present.length > 0
            ? integrationFacts(entry, rows, now, {
                known: data?.traffic_known ?? false,
                since: data?.traffic_since,
              })
            : undefined
        }
      />
      <div className="col gap-3">
        {loading && !data && <Skeleton variant="text" rows={6} label="Loading" />}
        <QueryState error={error} refusal={refusal} loading={loading}>
          {data && present.length === 0 && (
            <EmptyState
              size="compact"
              icon={<PlugGlyph size="xl" />}
              title={`${entry.name} is not connected`}
              description={`${entry.description} Connect it to start.`}
            />
          )}
          {present.length > 0 && (
            <>
              {/* WHAT THIS TOOL IS MADE OF, all of it. The card below shows a
                  surface only when something is wrong with it, which is right
                  for a reader scanning six integrations and wrong for one
                  asking whether this is the integration they meant: a tool
                  whose Jira is fine and whose Confluence is not is a different
                  object from one with no Confluence at all. */}
              <Card>
                <Card.Header icon={<LayersGlyph size="sm" />} count={entry.surfaces.length}>
                  <Card.Title>Surfaces</Card.Title>
                </Card.Header>
                <ul className="int-rows">
                  {entry.surfaces.map((surface) => {
                    const row = rows.get(surface.key);
                    return (
                      <li key={surface.key} className="int-row">
                        <div className="int-row-identity">
                          <span className="int-row-name">{surface.name}</span>
                          <span className="int-row-detail">
                            {/* A ROW WITH NO RECONCILE IS NOT A HEALTHY ROW.
                                Null is a process with no loop to ask or a
                                surface it has not reached, and "nothing
                                outstanding" there would be this screen
                                inventing the health it has always refused to
                                show. */}
                            {row
                              ? row.reconcile?.detail ||
                                (row.reconcile
                                  ? "nothing outstanding"
                                  : "no pass has reported on this surface")
                              : "not configured"}
                          </span>
                        </div>
                        <div className="int-row-badges">
                          {row?.reconcile?.phase && (
                            <Tag variant={phaseTone(row.reconcile.phase)} appearance="outline">
                              {row.reconcile.phase_label}
                            </Tag>
                          )}
                          {row && <SurfaceBadges row={row} />}
                        </div>
                      </li>
                    );
                  })}
                </ul>
              </Card>

              <Card>
                <Card.Header icon={<TriangleAlertGlyph size="sm" />} count={findings.length}>
                  <Card.Title>Findings</Card.Title>
                </Card.Header>
                {findings.length > 0 ? (
                  <ul className="col int-findings">
                    {findings.map(({ surface, finding }, at) => (
                      <li
                        key={`${finding.kind}:${finding.subject ?? ""}:${at}`}
                        className="col gap-1"
                      >
                        <span className="row wrap gap-2">
                          {/* THE VERDICT THE ENGINE PUT ON THE KIND, never one
                              derived here: `phase` travels with every finding
                              for exactly this, and an ABSENT phase is "cannot
                              say" rather than fine — which is what
                              [phaseTone]'s neutral default draws. */}
                          <Tag variant={phaseTone(finding.phase ?? "")} appearance="outline">
                            {finding.kind.replace(/_/g, " ")}
                          </Tag>
                          {present.length > 1 && <span className="t-caption">{surface}</span>}
                        </span>
                        <span className="int-note-problem">
                          {finding.detail ||
                            `${finding.kind.replace(/_/g, " ")}${finding.subject ? `: ${finding.subject}` : ""}`}
                        </span>
                        <FindingSubjects subjects={finding.subjects ?? []} />
                        {finding.remedy && (
                          <span className="int-note-remedy">{finding.remedy}</span>
                        )}
                      </li>
                    ))}
                  </ul>
                ) : (
                  <span className="t-caption">
                    {/* WHICH SILENCE THIS IS. A pass that found nothing and a
                        surface no pass has reached are both an empty list, and
                        only one of them is good news. */}
                    {present.some((p) => p.row.reconcile)
                      ? "The last pass found nothing outstanding."
                      : "No status reported yet."}
                  </span>
                )}
              </Card>

              {/* IS ANYTHING ARRIVING, per surface — the question the facts
                  above count and this one dates. `forge` is left out for the
                  reason the screen leaves it out of its delivery panels: it is
                  a relay, and its events are filed under the product they
                  belong to, so a row for it could only ever be empty. */}
              <Card>
                <Card.Header icon={<InboxGlyph size="sm" />}>
                  <Card.Title>Last delivery</Card.Title>
                </Card.Header>
                <ul className="int-rows">
                  {entry.surfaces
                    .filter((surface) => surface.key !== "forge")
                    .map((surface) => (
                      <LastDelivery
                        key={surface.key}
                        surface={surface.key}
                        name={surface.name}
                        now={now}
                      />
                    ))}
                </ul>
              </Card>
            </>
          )}
        </QueryState>
      </div>
    </>
  );
}
