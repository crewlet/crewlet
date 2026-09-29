/**
 * The third-party tools agents work on, and whether each one is working.
 *
 * Laid out the way the console's own Integrations page is: grouped by the
 * CAPABILITY a tool provides (messaging, tasks, code, observability), one
 * bordered list per group, one row per TOOL with its mark, its name and one
 * line on what it is for on the left and its state on the right. Every tool
 * this build serves is listed whether or not this company set it up, so the
 * reader sees the whole catalogue rather than only the part they configured.
 *
 * A row is the tool, not the engine's plumbing for it. Atlassian is one row
 * although the engine reaches it over three surfaces (Jira, Confluence and
 * the Forge relay), because that is how the company thinks of it; the row's
 * state is the ENGINE's roll-up of its surfaces (`integration.Rollup`, sent as
 * the answer's `tools`), and its status line names the surface that is not
 * ready. The counts, the inbound paths and the reconcile
 * findings are still here, folded under a per-row disclosure, so an operator
 * who needs to know why can open it without the screen leading with it.
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
  Callout,
  Card,
  EmptyState,
  EmptyValue,
  InlineCode,
  Skeleton,
  Tag,
  type Tone,
} from "@crewlethq/ui";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import {
  PlugGlyph,
  ChevronDownGlyph,
  KeyGlyph,
  LayersGlyph,
  LinkGlyph,
  InboxGlyph,
  PlusGlyph,
  RotateCwGlyph,
  SettingsGlyph,
  TriangleAlertGlyph,
} from "@crewlethq/icons/glyphs";
import { QueryState } from "~/components/common.tsx";
import { DataGrid, type GridColumn } from "~/app/frame/DataGrid.tsx";
import {
  DateCell,
  DurationCell,
  KeyCell,
  NumberCell,
  SeatCell,
  StatusCell,
  TextCell,
} from "~/app/frame/cells.tsx";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { href, useNavigator } from "~/app/router.tsx";
import { usePageLabels } from "~/app/Shell.tsx";
import { useNow } from "~/lib/clock.ts";
import { fmtDate, fmtDateTime, plural, relTime, tsKey } from "~/lib/format.ts";
import { useRecheck } from "./recheck.ts";
import { VendorMark, type Vendor } from "~/ui/VendorMark.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg, seatLookup } from "~/lib/seats.ts";
import { SetupDialog } from "./SetupDialog.tsx";
import { DisconnectDialog } from "./DisconnectDialog.tsx";
import { requestToken, rest, RestError } from "~/protocol/index.ts";
import { useRest } from "~/lib/useRest.ts";
import type { EventRecord, SetupRun } from "~/protocol/types.ts";
import {
  INTEGRATION_TOOLS,
  type IntegrationRow,
  type IntegrationTool,
  type IntegrationToolState,
  type ReconcileFinding,
  type ReconcileStatus,
} from "~/contract/integrations.ts";
import type { SetupListing, SetupSeatState, SetupToolState } from "~/protocol/types.ts";
import { Segmented } from "~/ui/primitives.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";

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
   * This tool's page on docs.crewlet.ai, under `integrations/`, for the one
   * tile that has nothing a person can do from here: Learn more.
   */
  doc: string;
}

/** Where the product documentation is published. See `docs/index.md`. */
const DOCS_BASE = "https://docs.crewlet.ai/";

/** The published address of an integration's own page. */
export function docHref(entry: Entry): string {
  return `${DOCS_BASE}integrations/${entry.doc}`;
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
    description: "Every agent posts as its own bot. Mentions and threads wake the right seat.",
    vendor: "slack",
    surfaces: surfacesOf("slack"),
    doc: "slack",
  },
  {
    key: "mattermost",
    name: "Mattermost",
    description: "Self-hosted chat. Every agent posts as its own bot over an outbound socket.",
    vendor: "mattermost",
    surfaces: surfacesOf("mattermost"),
    doc: "mattermost",
  },
  {
    key: "atlassian",
    name: "Atlassian",
    description: "Jira and Confluence. Each agent works issues and pages with its own account.",
    vendor: "atlassian",
    surfaces: surfacesOf("atlassian"),
    doc: "jira",
  },
  {
    key: "github",
    name: "GitHub",
    description: "Agents read issues and review pull requests, each through its own GitHub App.",
    vendor: "github",
    surfaces: surfacesOf("github"),
    doc: "github",
  },
  {
    key: "gitlab",
    name: "GitLab",
    description: "Agents branch, push and review merge requests with their own tokens.",
    vendor: "gitlab",
    surfaces: surfacesOf("gitlab"),
    doc: "gitlab",
  },
  {
    key: "datadog",
    name: "Datadog",
    description: "Monitors route to a seat by their tags; anything untagged goes to a fallback.",
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

/** Who has to act, phrased for the person reading it. */
function actorLabel(actor: string | undefined): string {
  switch (actor) {
    case "engine":
      return "the engine is working on it";
    case "provider":
      return "the third-party app is applying it";
    // THE ADMIN IS UNLABELLED TOO, on the same reasoning as the operator
    // below. "you, at the third-party app" named a place the finding's own
    // sentence already names — and names better, because it says WHICH app
    // and what to do there — so the clause was a second, vaguer copy of the
    // instruction sitting beside it.
    case "admin":
      return "";
    // THE OPERATOR IS UNLABELLED, and deliberately. "you, in the company
    // configuration" named a place this screen IS: the note sits inside the
    // card whose Settings control opens the very form the fix is made in, so
    // it sent a reader looking elsewhere for what was already in front of
    // them. The finding's own sentence says what to change; where is not a
    // second fact worth a clause.
    default:
      return "";
  }
}

/** A tool's rolled-up state: the tag on the right and the one line under the name. */
export interface EntryState {
  /**
   * The word in the badge: the engine's roll-up label — a phase's own label,
   * or Connecting, Paused, Not in use.
   *
   * EVERY TILE CARRIES ONE, an unconfigured tool included. The tiles are a
   * grid now, and a tile with no pill under its name is a hole in a row of
   * them that a reader has to explain to themself; the engine's "Not in use"
   * is drawn in the neutral outline, which is the one register on this
   * screen that reports nothing wrong. EMPTY only when the answer carries no
   * roll-up for an unconfigured tool, which is a node too old to send one.
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

type Present = { surface: Surface; row: IntegrationRow };

function presentSurfaces(entry: Entry, rows: Map<string, IntegrationRow>): Present[] {
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
 * takes, and that a tool with no configured surface gets no badge at all (the
 * Connect button beside it is the whole message, and a chip saying "not in
 * use" on every unconfigured row reads as a fault list).
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

/**
 * The tone each state is drawn in.
 *
 * CONNECTED IS ONLY EVER GREEN, and nothing else is: the engine never sends
 * `connected` over a surface nothing can reach. `not_connected` is AMBER for the
 * reason every in-progress phase is — an integration that does not work yet,
 * which is not the neutral this screen keeps for a state nobody needs to come
 * back to. `not_in_use` is that neutral.
 */
const TOOL_TONE: Record<IntegrationToolState, Tone> = {
  attention: "warning",
  not_connected: "warning",
  connected: "success",
  not_in_use: "neutral",
};

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
 * is the em dash [NumberCell] draws and the one this screen means by it.
 */
function total(
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
function latestOf(
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
function soonestOf(
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
        <NumberCell
          value={counted((r) => r.inbound)}
          title="deliveries this engine verified and stored"
        />
      ),
      note: covered,
    },
    {
      // NO NOTE ON THE OTHER TWO, although the same window covers them: a
      // fact line whose every value carries the same footnote is one nobody
      // reads, which is what `Fact.note` says about itself. The three counts
      // are read together and the first one states what they all cover.
      label: "Dropped",
      value: (
        <NumberCell
          value={counted((r) => r.skipped)}
          title="verified, and turned into work for nobody"
        />
      ),
    },
    {
      label: "Coalesced",
      value: (
        <NumberCell
          value={counted((r) => r.coalesced)}
          title="folded into a wake the seat already had"
        />
      ),
    },
    {
      // THE LOOP'S OWN CLOCK, which is the one thing a card never says: it
      // reports what the last pass CONCLUDED and never when that was, so a
      // surface checked a moment ago and one nothing has looked at in an hour
      // read identically.
      label: "Last pass",
      value: <DateCell at={latestOf(present, (r) => r.reconcile?.last_attempt_at)} now={now} />,
    },
    {
      label: "Next pass",
      value: <DateCell at={soonestOf(present, (r) => r.reconcile?.next_attempt_at)} now={now} />,
    },
  ];
}

/**
 * The findings the report did NOT summarise.
 *
 * The engine promotes the reported finding to index 0 (integration.Promote),
 * so dropping element zero would usually be right — and "usually" is the
 * problem: a row written by a peer on an older build carries the vendor's own
 * order, and a rolling upgrade puts exactly those rows here. Matching by
 * identity is correct for both, and it removes the positional assumption
 * rather than depending on it.
 *
 * The report's detail can carry a count suffix, because Classify appends
 * "(and 2 more)" when several findings share the winning kind, so that is
 * stripped before the comparison. Only the FIRST match is removed: several
 * findings can legitimately render the same sentence for different seats, and
 * the report stands for exactly one of them.
 */
/*
 * GENERIC, because it is a filter: it returns the findings it was handed and
 * has no business narrowing them. Typed to a literal shape it silently
 * DROPPED every field that shape did not list — so the caller got back
 * findings with no `remedy`, `subjects` or `action_url` on them, and the
 * renderer could not lay out what the engine had sent.
 */
export function withoutHeadline<T extends { kind: string; subject?: string; detail?: string }>(
  findings: T[],
  detail: string,
): T[] {
  const headline = detail.replace(/ \(and \d+ more\)$/, "").trim();
  if (headline === "") return findings;
  let dropped = false;
  return findings.filter((f) => {
    if (dropped || (f.detail ?? "").trim() !== headline) return true;
    dropped = true;
    return false;
  });
}

/**
 * What the reconcile loop last found for one surface.
 *
 * Renders NOTHING when the status is null, and the silence is the point: this
 * node may not have been able to read the fleet's rows, and the loop may not
 * have reached this surface yet, and neither of those is a claim that the
 * surface is healthy. A green tick here would be exactly the invented health
 * this screen has always refused to show.
 */
export function Reconcile({
  status,
  appName,
}: {
  status: ReconcileStatus | null | undefined;
  /**
   * The app this surface belongs to, for naming the link a finding carries.
   *
   * A LINK THAT SAYS WHERE IT GOES. The anchor here used to read "Open where
   * this is fixed", which named nothing and sat under a sentence that already
   * said where — and beside the agent row's own "Install on GitHub" button,
   * pointing at the same address. It was removed for that. What a finding
   * carries now is a place the engine DELIBERATELY will not act: an account
   * somebody else disabled, which it reports rather than reversing. Telling
   * somebody to do it at the app and handing them nothing is the other half
   * of that decision left undone.
   */
  appName?: string;
}) {
  // NOTHING TO SAY WITHOUT A REPORT. This once fell back to the row's own
  // `detail`, which the engine has never sent, so the fallback drew nothing on
  // every surface it was written for. What a surface with no loop CAN say —
  // paused, a secret that does not resolve, nothing routing — is on the
  // badges its row draws beside this.
  if (!status) return null;

  const actor = actorLabel(status.actor);
  // The findings the phase was NOT derived from. The report says what to do
  // next and the findings say what is actually wrong, so an operator who
  // fixes the first should not wait a full pass to learn there was a second.
  //
  // BY IDENTITY, NOT BY POSITION. This dropped element zero, on the
  // assumption that the reported finding is first — which the engine now
  // guarantees (integration.Promote) but a row written by a peer on an
  // older build does not, and a rolling upgrade puts exactly those rows on
  // this screen. Whenever the worst finding was not already first, a real
  // finding was hidden and the headline was re-printed as "1 more finding".
  //
  // Matched on the DETAIL, which is what the report carries and what this
  // list renders, so the comparison is between the two strings actually on
  // screen rather than between a rendered string and a reconstructed one.
  // A count suffix — Classify appends "(and 2 more)" when several findings
  // share the winning kind — is stripped from the report's side first.
  const others = withoutHeadline(status.findings ?? [], status.detail ?? "");
  // AND THE ONE THE HEADLINE IS, which is whichever finding `others` left
  // out: its sentence is already on screen, and what is not is its subject
  // list. Derived by difference rather than by a second match, so the two
  // cannot disagree about which finding the headline stands for.
  const reported = (status.findings ?? []).find((f) => !others.includes(f));

  // A WORKING SURFACE SAYS NOTHING.
  //
  // The band used to carry the loop's own clock — when it last settled and
  // when it next runs — under every surface, which is the engine narrating
  // its schedule to somebody who asked what state their integration is in.
  // On a healthy surface that timestamp was the ONLY thing in the band, so
  // the card grew a grey stripe per surface saying nothing had happened.
  // The tag says the state; the band is for what a person has to act on.
  // THE REPORTED FINDING'S OWN LINK, named. See `appName`: a bare "Open
  // where this is fixed" was removed, and an anchor that says which app it
  // opens is the opposite case — a finding whose whole content is "a person
  // has to do this at the app" needs somewhere to send them.
  const where = reported?.action_url ?? "";
  if (!status.detail && !status.last_error && others.length === 0 && !where) {
    return null;
  }

  return (
    <div className="int-row-note">
      <div className="int-row-note-text">
        {/* THE HEADLINE FINDING, laid out: what is wrong, which things,
            what to do, where to do it. Four slots at three weights, in the
            order somebody reads them — rather than one paragraph carrying
            all four, which is what this replaced. */}
        {status.detail && <p className="int-note-problem">{status.detail}</p>}
        {reported && <FindingSubjects of={reported} />}
        {(reported?.remedy || where) && (
          <p className="int-note-remedy">
            {reported?.remedy}
            {where && (
              <a className="prose-link" href={where} target="_blank" rel="noreferrer">
                Open {appName || "the app"}
              </a>
            )}
          </p>
        )}
        {actor && <span className="int-row-note-when">{actor}</span>}

        {status.last_error && (
          <span className="int-row-note-when" title="the last pass could not read this surface">
            Last pass failed: {status.last_error}
          </span>
        )}

        {/* THE REST, behind one disclosure and RULED OFF from the headline.
            Its summary used to butt straight against the sentence above it,
            so `…nothing else can do it.` and `1 more finding` read as one
            run-on line. */}
        {others.length > 0 && (
          <details className="int-more">
            <summary className="int-summary">
              {others.length} more finding{others.length === 1 ? "" : "s"}
            </summary>
            <ul className="col int-findings">
              {others.map((f, i) => (
                <li key={`${f.kind}:${f.subject ?? ""}:${i}`}>
                  <span className="int-note-problem">
                    {f.detail || `${f.kind.replace(/_/g, " ")}${f.subject ? `: ${f.subject}` : ""}`}
                  </span>
                  <FindingSubjects of={f} />
                  {f.remedy && <span className="int-note-remedy">{f.remedy}</span>}
                </li>
              ))}
            </ul>
          </details>
        )}
      </div>
    </div>
  );
}

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
 * Renders nothing for the ordinary finding about one thing, which sends no
 * list at all.
 */
function FindingSubjects({ of }: { of: ReconcileFinding }) {
  const all = of.subjects ?? [];
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
 * The order the catalogue is read in: what this company has, then what it
 * could have, each half alphabetical.
 *
 * CONFIGURED COVERS ATTEMPTED as well as working — a card with a block
 * behind it, whatever the loop says about it — because a broken integration
 * is one this company HAS, and is the row most worth reaching first. Sorting
 * on health instead would move a row out from under the cursor of somebody
 * watching an app recover, which is exactly when they are looking at it.
 */
export function byConfiguredThenName(
  rows: Map<string, IntegrationRow>,
): (a: Entry, b: Entry) => number {
  const rank = (entry: Entry) => (entry.surfaces.some((s) => rows.has(s.key)) ? 0 : 1);
  return (a, b) => rank(a) - rank(b) || a.name.localeCompare(b.name);
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
  // request that can 401 for want of an operator token or fail transiently.
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
          title="the config names a secret whose ${VAR} resolved to nothing, so every delivery is refused"
        >
          secret unresolved
        </Tag>
      )}
      {row.routes === false && (
        <Tag
          variant="warning"
          appearance="outline"
          title="deliveries are verified and stored, and no parser turns them into work for a seat"
        >
          routes nowhere
        </Tag>
      )}
      {row.endpoint_current === false && (
        <Tag
          variant="warning"
          appearance="outline"
          title="this surface is registered at an address that is no longer this deployment's, so its deliveries go nowhere"
        >
          address moved
        </Tag>
      )}
      {row.enabled === false && <Tag appearance="outline">paused</Tag>}
    </>
  );
}

/** Whether one surface has anything to report: [SurfaceRow] renders iff so. */
function surfaceFaulted(row: IntegrationRow): boolean {
  return (
    row.secret_usable === false ||
    row.routes === false ||
    row.enabled === false ||
    row.endpoint_current === false ||
    (typeof row.skipped === "number" && row.skipped > 0) ||
    Boolean(row.reconcile?.detail) ||
    Boolean(row.reconcile?.last_error) ||
    withoutHeadline(row.reconcile?.findings ?? [], row.reconcile?.detail ?? "").length > 0
  );
}

/**
 * What one surface has to report, or nothing.
 *
 * A CARD'S BODY IS ITS AGENTS. It used to open with a row per surface —
 * "Jira, /webhooks/jira, Connected" — under a header already saying
 * Connected, so the first thing a reader saw on opening a card was the
 * engine restating the tag above it and naming a route nobody has to paste
 * any more, now that the loop registers the hook itself.
 *
 * A surface with a PROBLEM still has to say so, and say which surface it is,
 * because the header rolls up to the least ready one and a tool with two
 * surfaces has two answers. So this renders when there is a fault and stays
 * out of the way when there is not.
 */
function SurfaceRow({
  surface,
  row,
  named,
  base,
}: {
  surface: Surface;
  row: IntegrationRow;
  /** Whether to say which surface this is. */
  named: boolean;
  /** The address this deployment is reachable at now, for the note below. */
  base?: string;
}) {
  const dropped = typeof row.skipped === "number" && row.skipped > 0;
  if (!surfaceFaulted(row)) return null;

  return (
    <li className="int-row">
      <div className="int-row-identity">
        {named && <span className="int-row-name">{surface.name}</span>}
      </div>

      <div className="int-row-badges">
        <SurfaceBadges row={row} />
      </div>
      {/* BOTH ADDRESSES, because the fix is to replace one with the other
          at the third-party app and a reader cannot do that from a badge.

          ONE SENTENCE IN ONE ELEMENT. The note's text is a flex column, so
          every text node and every code chip beside it became a row of its
          own: one sentence was drawn as five stacked fragments with the two
          addresses on lines by themselves. */}
      {row.endpoint_current === false && (
        <div className="int-row-note">
          <span className="int-row-note-text">
            <span>
              The event subscription was registered with <InlineCode>{row.endpoint}</InlineCode>,
              but this engine now listens on <InlineCode>{base ?? "no public address"}</InlineCode>.
              Update the address in {surface.name} to keep receiving events.
            </span>
          </span>
        </div>
      )}

      {dropped && (
        <div className="int-row-note">
          <span className="int-row-note-text">
            {/* ONE STRING, not a number beside its own noun. Split across
                nodes it reads as three fragments to a screen reader, and no
                test can match the sentence a person sees. */}
            <span>
              {row.skipped === 1
                ? "1 delivery was verified and dropped"
                : `${(row.skipped ?? 0).toLocaleString()} deliveries were verified and dropped`}
            </span>
            <span className="int-row-note-when">
              A drop is a delivery this surface accepted and no parser turned into work, so whatever
              sent it is reaching the engine and reaching nobody.
            </span>
          </span>
        </div>
      )}

      <Reconcile status={row.reconcile} appName={surface.name} />
    </li>
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
function credentialFindings(present: Present[]): ReconcileFinding[] {
  return present.flatMap((p) =>
    (p.row.reconcile?.findings ?? []).filter((f) => ROTATE_FINDINGS.has(f.kind)),
  );
}

/**
 * A tile's one action, from what the engine says about the tool.
 *
 * ONE, because a tile in a grid of eight is read in one glance, and a card
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
  // company block and seats together. `satisfied` folds the seats' OWN acts
  // in too — a GitHub seat whose app is created at GitHub, from its row on
  // the tool's page — so it is only the fallback for a node too old to send
  // the first: a Continue that opens a form with nothing in it to answer is a
  // smaller fault than a form nobody can reach.
  if (configured.some((t) => (t.form_complete ?? t.satisfied) === false)) return CONTINUE;
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
export function postManifest(answer: AppManifest): void {
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
}: {
  /** The tool in the reader's own words, from the catalogue. */
  app: string;
  /** The engine surface this roster belongs to: what the route is keyed on. */
  toolKey: string;
  seat: SetupSeatState;
}) {
  const [busy, setBusy] = useState(false);
  const [refused, setRefused] = useState("");

  async function create(): Promise<void> {
    setBusy(true);
    setRefused("");
    try {
      const answer = (await rest.post(`/setup/integrations/${toolKey}/app`, {
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
      <Button size="small" variant="primary" disabled={busy} onClick={() => void create()}>
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
function toolsOf(sections: { tool: SetupToolState }[] | undefined): SetupToolState[] {
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
function rosterOf(tools: SetupToolState[]): SetupToolState | undefined {
  const rosters = tools.filter((t) => (t.seats ?? []).length > 0);
  return (
    rosters.find((t) => t.seats_required) ?? rosters.find((t) => t.can_provision) ?? rosters[0]
  );
}

/**
 * Why an action that opens the settings form cannot be pressed, or undefined.
 *
 * A WRITE CONTROL IS NEVER HIDDEN (dashboard rule 4): it is disabled with the
 * one sentence that says why. `/setup` is guarded in full, so a reader with no
 * operator token has no form to open, and a listing that answered nothing for
 * this tool has none either — which is a different sentence, because the
 * token would not help.
 */
export function formBlocked(sections: unknown[], guarded: boolean): string | undefined {
  if (sections.length > 0) return undefined;
  return guarded
    ? "Setting an integration up needs an operator token."
    : "The engine did not say what this integration needs, so there is no form to open.";
}

/** Each form-opening action's accessible name, for one tool. */
const ACTION_NAMES: Record<"connect" | "continue" | "rotate", (name: string) => string> = {
  connect: (name) => `Connect ${name}`,
  continue: (name) => `Continue ${name} setup`,
  rotate: (name) => `Rotate token for ${name}`,
};

/**
 * A tile's one action, drawn.
 *
 * MANAGE AND LEARN MORE ARE LINKS, because they go somewhere: the tool's own
 * page, and its page on docs.crewlet.ai. The other three open the settings
 * form and are buttons. Rotate token is the one primary, because it is the
 * only one with a date on it; a list's other row actions are secondary, and
 * Learn more is the quietest because nothing is owed on a tile that carries
 * it.
 */
export function ActionButton({
  entry,
  action,
  blocked,
  onOpen,
}: {
  entry: Entry;
  action: TileAction;
  /** From [formBlocked]: why the form cannot open, for the three that open it. */
  blocked?: string;
  onOpen: () => void;
}) {
  switch (action.kind) {
    case "manage":
      return (
        <ButtonLink
          size="small"
          variant="secondary"
          href={href(["settings", "integrations", entry.key])}
          aria-label={`Manage ${entry.name}`}
        >
          Manage
        </ButtonLink>
      );
    case "learn":
      return (
        <ButtonLink
          size="small"
          variant="ghost"
          external
          href={docHref(entry)}
          aria-label={`Learn more about ${entry.name}`}
        >
          Learn more
        </ButtonLink>
      );
    default:
      return (
        <Button
          size="small"
          variant={action.kind === "rotate" ? "primary" : "secondary"}
          disabledReason={blocked}
          // NAMED FOR THE TOOL, with the visible label at its start (the
          // label is in the name, WCAG 2.5.3): a grid of eight tiles is eight
          // "Connect" buttons to a screen reader's list of controls.
          aria-label={ACTION_NAMES[action.kind](entry.name)}
          onClick={() => {
            if (!blocked) onOpen();
          }}
        >
          {action.label}
        </Button>
      );
  }
}

/**
 * The quiet line at a tile's foot: what the tool is made of, counted.
 *
 * FACTS THE ENGINE SENT, never an estimate: the agents on its roster, the
 * credentials it holds findings about, and the deliveries this node counted —
 * the last only when `traffic_known`, because a node that could not read its
 * event log sends zero for every surface, and "0 deliveries" there is the
 * alarming answer on precisely the node that did not look. Empty on a tool
 * nobody has connected, which has nothing yet to count.
 */
export function tileMeta(
  present: Present[],
  roster: SetupToolState | undefined,
  credentials: ReconcileFinding[],
  trafficKnown: boolean,
): string {
  if (present.length === 0) return "";
  const parts: string[] = [];
  const seats = roster?.seats?.length ?? 0;
  if (seats > 0) parts.push(plural(seats, "agent"));
  const expiring = credentials.filter((f) => f.kind === "credential_expiring").length;
  const rejected = credentials.filter((f) => f.kind === "credential_rejected").length;
  if (expiring > 0) parts.push(`${expiring} expiring`);
  if (rejected > 0) parts.push(`${rejected} refused`);
  const delivered = trafficKnown ? total(present, (r) => r.inbound) : null;
  if (delivered !== null) {
    parts.push(delivered === 0 ? "nothing delivered" : plural(delivered, "delivery", "deliveries"));
  }
  return parts.join(" · ");
}

/**
 * The roll-up's reason, as a sentence standing on its own.
 *
 * The engine writes its reasons to be EMBEDDED — "the third-party app refused
 * this integration's credential" reads on inside a longer line elsewhere — so
 * it starts them lower-case, and a tile or a dialog that leads with one has to
 * give it its capital. Only the first letter: the rest is the engine's.
 */
export function asSentence(reason: string): string {
  return reason.charAt(0).toUpperCase() + reason.slice(1);
}

/**
 * One integration in the catalogue: a tile with its mark, its name, the
 * engine's word for its state, one sentence and ONE action.
 *
 * THE SENTENCE IS WHAT IS OWED WHEN SOMETHING IS, and what the tool does
 * otherwise. The engine's roll-up carries a reason exactly when a person or
 * the engine has something left to do, and that is the sentence a reader
 * scanning eight tiles came for; on a working tool the reason is empty and the
 * tile says what the tool is for, which is what tells a reader the engine
 * serves it at all.
 */
export function IntegrationTile({
  entry,
  rows,
  rollups,
  sections,
  trafficKnown,
  guarded,
  onOpen,
}: {
  entry: Entry;
  rows: Map<string, IntegrationRow>;
  rollups?: IntegrationTool[];
  sections: { name: string; tool: SetupToolState; seat?: string }[];
  trafficKnown: boolean;
  guarded: boolean;
  /** Open the settings form for this tool, for the action the tile carries. */
  onOpen: (action: TileAction, reason: string) => void;
}) {
  const present = presentSurfaces(entry, rows);
  const tools = toolsOf(sections);
  const state = entryState(entry, rows, rollups);
  const credentials = credentialFindings(present);
  const action = actionFor(state, tools, present.length > 0, credentials, guarded);
  const meta = tileMeta(present, rosterOf(tools), credentials, trafficKnown);
  const nameID = `int-tile-${entry.key}`;
  const reason = asSentence(state.reason ?? "");
  return (
    <article
      className={present.length === 0 ? "int-tile is-absent" : "int-tile"}
      aria-labelledby={nameID}
    >
      <div className="int-tile-head">
        <span className="int-brand" aria-hidden>
          <VendorMark vendor={entry.vendor} />
        </span>
        <span className="int-tile-title">
          <h2 className="int-name" id={nameID}>
            {entry.name}
          </h2>
          {state.tag !== "" && (
            <Tag size="sm" variant={state.tone} appearance={state.outline ? "outline" : "soft"}>
              {state.tag}
            </Tag>
          )}
        </span>
      </div>
      <p className="int-tile-text">{reason || entry.description}</p>
      <div className="int-tile-foot">
        <span className="int-tile-meta">{meta}</span>
        <ActionButton
          entry={entry}
          action={action}
          blocked={formBlocked(sections, guarded)}
          onOpen={() => onOpen(action, reason)}
        />
      </div>
    </article>
  );
}

/**
 * One integration on its own page: the tool's agents, whatever is wrong with
 * a surface, and the controls a tile leaves here — the settings form, the
 * owed action, and Disconnect.
 *
 * THE DISCLOSURE STAYS, open. A page about one tool opens on the tool, and the
 * body is what the page is for; the chevron is there so a reader who came for
 * the passes and deliveries below can fold the roster out of the way.
 *
 * The body is where the engine's own plumbing lives: a row per faulted surface
 * with what the reconcile loop last found, and a row per agent with its own
 * step at the third-party app. None of that belongs in a catalogue tile.
 */
export function EntryRow({
  entry,
  rows,
  rollups,
  sections,
  publicBase,
  guarded = false,
  onOpen,
  onDisconnect,
}: {
  entry: Entry;
  rows: Map<string, IntegrationRow>;
  /** The answer's `tools`: the engine's roll-up of every tool. */
  rollups?: IntegrationTool[];
  /**
   * The address third-party apps reach this deployment on right now, for the
   * row that has to say what a moved registration should be changed to.
   */
  publicBase?: string;
  /** The engine's setup state per surface this tool is made of. */
  sections?: { name: string; tool: SetupToolState; seat?: string }[];
  /** Whether the setup listing was refused, for [formBlocked]. */
  guarded?: boolean;
  /**
   * Open the settings form — for the action named, or for the settings square
   * when none is. Absent where nothing may open it.
   */
  onOpen?: (action: TileAction | null, reason: string) => void;
  /** Take the tool away. Absent for a tool nothing has configured. */
  onDisconnect?: () => void;
}) {
  const [open, setOpen] = useState(true);
  const present = presentSurfaces(entry, rows);
  const absent = present.length === 0;
  const tools = toolsOf(sections);
  const state = entryState(entry, rows, rollups);
  const credentials = credentialFindings(present);
  const action = actionFor(state, tools, !absent, credentials, guarded);
  const blocked = formBlocked(sections ?? [], guarded);
  // WHAT THE LOOP IS SAYING ABOUT EACH AGENT, across every surface on this
  // card.
  //
  // The roster and the reconcile rows come from two endpoints and meet here,
  // which is why this is the only place the contradiction could be resolved.
  // The roster's `satisfied` means "a credential is sealed where this app
  // looks for it" — a real fact, and not the one a reader takes from a green
  // badge marked ready. Measured: the card said an agent had no Jira account
  // and that Atlassian was still setting it up, with the same agent's row
  // underneath badged ready, because the ${VAR} resolved.
  const seatNotes = seatFindings(present);
  const roster = rosterOf(tools);
  const seats = roster?.seats ?? [];
  const bodyID = `int-body-${entry.key}`;

  // THE PAGE CARRIES WHAT THE TILE WOULD HAVE, MINUS MANAGE — this IS where
  // Manage goes — plus the two controls a tile leaves here: the settings form
  // (the square) and Disconnect. An owed action (Continue, Rotate token) is
  // the card's one labelled button beside them, because on this page it is
  // still the thing a person came to do.
  const owed = action.kind === "manage" ? null : action;
  const actions = (
    <>
      {owed && onOpen && (
        <ActionButton
          entry={entry}
          action={owed}
          blocked={blocked}
          onOpen={() => onOpen(owed, asSentence(state.reason ?? ""))}
        />
      )}
      {!absent && onDisconnect && (
        <Button size="small" variant="ghost" onClick={onDisconnect}>
          Disconnect
        </Button>
      )}
    </>
  );

  // NOT CONNECTED IS A PLAIN CARD. There is nothing to disclose, so it gets
  // no disclosure: a chevron that opens an empty box is a control that lies.
  if (absent) {
    return (
      <div className="int-card int-card-absent">
        <span className="int-brand" aria-hidden>
          <VendorMark vendor={entry.vendor} />
        </span>
        <span className="int-heading">
          <span className="int-name">{entry.name}</span>
          <span className="int-desc">{entry.description}</span>
        </span>
        <div className="int-card-actions">{actions}</div>
      </div>
    );
  }

  return (
    <section className="int-card" aria-label={`${entry.name} agents and surfaces`}>
      <div className="int-card-head">
        {/* THE DISCLOSURE IS THE IDENTITY BLOCK, not the whole header, so the
            row's own buttons are not nested inside a button and a keyboard
            reader gets one predictable target. */}
        <button
          type="button"
          className="int-card-toggle"
          aria-expanded={open}
          aria-controls={bodyID}
          onClick={() => setOpen((was) => !was)}
        >
          <span className="int-brand" aria-hidden>
            <VendorMark vendor={entry.vendor} />
          </span>
          <span className="int-heading">
            <span className="int-name">Agents and surfaces</span>
            {/* WHAT THE TOOL IS, never how it is doing: the header above
                carries the state, and what is wrong is a note in the body. */}
            <span className="int-desc">{entry.description}</span>
          </span>
        </button>
        <div className="int-card-actions">
          {/* OURS, NOT `IconButton`. This square is a DISCLOSURE TRIGGER: it
              carries `aria-expanded` and `aria-controls` over the card body,
              and `.int-chevron` is what rotates the glyph when the card opens
              — a state uilet's icon button has no expression for, since its
              only stateful prop is `pressed`. The mark inside it is the
              design system's. */}
          <button
            type="button"
            className={open ? "int-chevron is-open" : "int-chevron"}
            aria-expanded={open}
            aria-controls={bodyID}
            aria-label={open ? `Hide ${entry.name} details` : `Show ${entry.name} details`}
            onClick={() => setOpen((was) => !was)}
          >
            <ChevronDownGlyph size="sm" />
          </button>
          {/* SETTINGS BESIDE THE DISCLOSURE, as a square the size of the
              chevron: chrome belonging to the card, next to the other control
              that is. The same form Connect opened, never a narrowed copy.
              Disabled with its reason rather than hidden, like every write. */}
          {onOpen && (
            // THE SAME SQUARE AS THE CHEVRON BESIDE IT, which is why it keeps
            // `.int-chevron` and is not an `IconButton`: uilet's sizes it from
            // its own scale, and a settings square a few pixels off its
            // neighbour is the one thing this pair must not be.
            <button
              type="button"
              className="int-chevron"
              aria-label={`${entry.name} settings`}
              aria-disabled={blocked ? true : undefined}
              title={blocked ?? `${entry.name} settings`}
              onClick={() => {
                if (!blocked) onOpen(null, "");
              }}
            >
              <SettingsGlyph size="sm" />
            </button>
          )}
          {actions}
        </div>
      </div>

      {open && (
        <div className="int-card-body" id={bodyID}>
          {/* ONE LIST, the agents and anything wrong with a surface. They are
              two kinds of the same thing to a reader (this tool, and whether
              it works), and two lists put an arbitrary seam down the middle
              of a card whose every row is an agent. A working surface adds
              nothing, so on a healthy tool this list IS the roster. */}
          <ul className="int-rows">
            {present.map((p) => (
              <SurfaceRow
                key={p.surface.key}
                surface={p.surface}
                row={p.row}
                // NAMED ON EVERY TOOL. A single-surface tool once left the
                // row's identity empty, so a faulted Mattermost drew a lone
                // "routes nowhere" badge at the end of a blank line.
                named
                base={publicBase}
              />
            ))}
            {/* A BODY WITH NOTHING IN IT SAYS SO, rather than opening onto an
                empty box under a chevron that promised something. */}
            {seats.length === 0 && !present.some((p) => surfaceFaulted(p.row)) && (
              <li className="int-row">
                <span className="int-row-detail">
                  Nothing to report: no surface of {entry.name} has a fault, and it keeps no roster
                  of agents.
                </span>
              </li>
            )}
            {seats.map((seat) => (
              <li key={seat.handle} className="int-row int-seat-row">
                {/* THE AGENT'S OWN MARK, the same one the org chart, the
                    people list and every seat chip render. An agent should
                    look like itself wherever it appears, which is also what
                    the console does on its roster: a row of bare names reads
                    as configuration, and a row with the agent's mark reads
                    as the person it stands for. */}
                <SeatAvatar name={seat.name || seat.handle} kind="agent" size="sm" decorative />
                <div className="int-row-identity">
                  <span className="int-row-name">
                    {seat.name || seat.handle}
                    {/* HOW MUCH THIS AGENT MAY DO, beside its name and drawn
                        quieter than a status tag: it is a standing fact about
                        the agent rather than something needing attention.
                        The console states it here for that reason, and this
                        roster is its counterpart.
                        It was a chip out on the right beside "ready", where
                        two chips on one row read as two verdicts and a
                        reader scanning for what is wrong stopped on "read
                        only" every time. */}
                    {seat.tier_label && (
                      <span
                        // THE WEIGHT CLASS ONLY WHERE THERE IS A TIER. An
                        // app can grade an agent in a vocabulary this engine
                        // does not have a scale for — a Datadog role an
                        // organization made is its own name and nothing else
                        // — and drawing it in the accent reserved for full
                        // access would be this screen claiming how much a
                        // role it has never seen grants.
                        className={
                          seat.tier ? `int-seat-tier int-seat-tier--${seat.tier}` : "int-seat-tier"
                        }
                        // WHAT THE TIER GRANTS, on hover. Three words on a
                        // pill cannot say what full access does and does not
                        // include, and the sentence that can is the engine's
                        // own, from the package that puts those permissions
                        // on the token.
                        title={seat.tier_hint}
                      >
                        {seat.tier_label}
                      </span>
                    )}
                  </span>
                  {/* WHERE THIS AGENT'S OWN CREDENTIAL IS, or what is
                      missing. It read "no inbound path yet" against every
                      agent of every app but Slack, because Slack is the only
                      one with a route per seat: true, and silent about the
                      thing the row exists to answer, which is whether this
                      agent can act as itself here. */}
                  {/* WHAT THIS AGENT IS AT THE APP, which is the question a
                      roster of agents raises. It showed the delivery ROUTE
                      instead, on the one app that has one per seat: true, and
                      the same shape for every agent bar the handle, so it
                      said nothing a reader could act on and nothing about
                      which of their apps this one is. The engine's sentence
                      wins; the path is the fallback for a node too old to
                      send one. */}
                  {/* THE ROW KEEPS ITS OWN SENTENCE, which is who this agent
                      IS at the app. The loop's reason for the badge beside it
                      is in the surface's own band above, once — printed here
                      as well it was the same sentence twice on one card, which
                      is the shape this whole screen is being cured of. */}
                  <span className="int-row-detail">
                    {seat.detail ? (
                      seat.detail
                    ) : seat.inbound_path ? (
                      <InlineCode>{seat.inbound_path}</InlineCode>
                    ) : (
                      "nothing set up for this agent"
                    )}
                  </span>
                </div>
                <div className="int-row-badges">
                  <SeatBadge satisfied={seat.satisfied} finding={seatNotes.get(seat.handle)} />
                </div>
                {/* THE STEP'S OWN CONTROL, outside the badges so a refusal can
                    take the full width of the row the way a finding does. */}
                {roster && <SeatStep app={entry.name} toolKey={roster.key} seat={seat} />}
              </li>
            ))}
          </ul>
        </div>
      )}
    </section>
  );
}

/**
 * The findings the loop has about individual agents, keyed by handle.
 *
 * ACROSS EVERY SURFACE ON THE CARD, because one agent's account is one thing
 * and the card's surfaces are three views of it: Atlassian creates the
 * account, Jira and Confluence are what it then works in, and any of the
 * three can be the one that knows this agent cannot work yet.
 *
 * THE FIRST ONE WINS and the order is the card's own surface order, which
 * puts the provisioning surface first — the one whose answer is about the
 * account rather than about a product refusing it.
 */
function seatFindings(present: Present[]): Map<string, ReconcileFinding> {
  const out = new Map<string, ReconcileFinding>();
  for (const p of present) {
    for (const f of p.row.reconcile?.findings ?? []) {
      if (f.subject && !advisory(f) && !out.has(f.subject)) out.set(f.subject, f);
    }
  }
  return out;
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
 * An ABSENT phase is not an advisory. A node older than the field sends none,
 * and "cannot say" must read as a fault so the badge stays honest.
 */
function advisory(f: ReconcileFinding): boolean {
  return f.phase === "ready";
}

/**
 * One agent's badge, from what is sealed AND what the loop found.
 *
 * THREE STATES, NOT TWO. `satisfied` answers "is a credential sealed where
 * this app looks for it", which is not the same question as "can this agent
 * work" — and rendering it as **ready** made the card contradict itself, with
 * a green agent under a surface reporting that the same agent had no account.
 *
 * A FINDING THAT NAMES THE AGENT WINS, whatever it is: the loop looked, and
 * the roster did not. Whether it is a wait or a fault is the finding's own
 * verdict, which the surface's own band above already renders — so this says
 * only that the agent is not there yet, in the tone the phase carries.
 */
function SeatBadge({ satisfied, finding }: { satisfied: boolean; finding?: ReconcileFinding }) {
  if (finding) {
    // AMBER FOR A WAIT, and amber for a fault too: the badge is a state, and
    // the band beside it is where the difference and the remedy are written.
    // A red agent under a surface saying "nothing has to be done" would be
    // the same contradiction in the other direction.
    return (
      <Tag variant="warning" appearance="outline">
        not ready
      </Tag>
    );
  }
  return (
    <Tag variant={satisfied ? "success" : "neutral"} appearance={satisfied ? "soft" : "outline"}>
      {satisfied ? "ready" : "not set up"}
    </Tag>
  );
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
  base: SetupListing["public_base_url"] | null;
  guarded: boolean;
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
  // things start one here — mount, a token change, the tab becoming visible
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
    base: listing?.public_base_url ?? null,
    guarded: setup.error?.unauthorized ?? false,
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
function stuckDisconnecting(entry: Entry, rows: Map<string, IntegrationRow>): string {
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
function SurfaceDeliveries({ surface, name }: { surface: string; name: string }) {
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
        loading={deliveries.loading}
        empty={
          deliveries.data && rows.length === 0
            ? {
                title: `Nothing has arrived on ${name}`,
                hint: "A verified delivery writes a row here. Nothing at all means either the provider is not sending, or it is being refused before it is recorded — the engine's own log is where a refusal appears.",
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
                header: "Arrived",
                shrink: true,
                sortValue: (e) => tsKey(e.timestamp),
                cell: (e) => <DateCell at={e.timestamp} now={now} />,
              },
              {
                key: "event",
                header: "Event",
                shrink: true,
                sortValue: (e) => e.type,
                // `webhook:` and `forge:` are the engine's own filing
                // prefixes, and the provider's event name is what an
                // operator is matching against their own console.
                cell: (e) => (
                  <code className="inline nowrap">{e.type.replace(/^(webhook|forge):/, "")}</code>
                ),
              },
              {
                key: "summary",
                header: "What it said",
                cell: (e) => <TextCell>{e.summary}</TextCell>,
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
                    <SeatCell handle={e.tags.recipient} {...who(e.tags.recipient)} />
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
                    <KeyCell value={e.tags.delivery_key} />
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
            The newest {DELIVERY_PAGE}. Older deliveries are in the{" "}
            <a className="prose-link" href={href(["live", "events"], { category: "webhook" })}>
              event log
            </a>
            , which pages.
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
 * so a reader with no operator token is REFUSED here rather than shown an
 * empty history, and `guarded` is what tells those two apart.
 */
function useSetupRuns(kinds: string[]): {
  runs: SetupRun[];
  scope: string;
  guarded: boolean;
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
          .map((one) => rest.get(`/setup/integrations/${encodeURIComponent(one)}/runs`, signal)),
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
): { run: SetupRun | null; missing: boolean; guarded: boolean; loading: boolean } {
  // FOLLOWED TO ITS END, and only while it is going: the row that opened this
  // may have been a pass that was running when the list answered.
  const [running, setRunning] = useState(false);
  const answer = useRest(
    kind === "" || id === "" ? null : `${kind}/${id}`,
    (signal) =>
      rest.get(
        `/setup/integrations/${encodeURIComponent(kind)}/runs/${encodeURIComponent(id)}`,
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
    loading: answer.loading,
  };
}

/**
 * A uilet variant, said in the tone names `app/frame/cells.tsx` still takes.
 *
 * The inverse of `app/frame/tone.ts`, and it exists for exactly as long as
 * that file does: one cell in this screen is a frame component this port may
 * not edit. `brand` and `info` have no status-cell spelling of their own and
 * take neutral, which is what the cell drew for them before.
 */
function cellTone(variant: Tone): "positive" | "caution" | "critical" | "info" | "neutral" {
  switch (variant) {
    case "success":
      return "positive";
    case "warning":
      return "caution";
    case "danger":
      return "critical";
    case "info":
      return "info";
    default:
      return "neutral";
  }
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
        title: "this pass is still going; the engine holds the surface's lease until it ends",
      };
    case "failed":
      return {
        glyph: "✕",
        label: "failed",
        tone: "danger",
        title:
          run.error ||
          "the third-party app refused this pass; nothing it had already done is undone",
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
        title: "a state this build does not know; a newer node wrote it",
      };
  }
}

/**
 * How long a pass took, or null while it is still going.
 *
 * NULL RATHER THAN "now minus started", which is the obvious alternative and
 * is a different measurement: a run whose node died mid-pass would report a
 * duration that grows for ever. [DurationCell] draws "not measured" for this,
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
function SetupPasses({ entry, kinds }: { entry: Entry; kinds: string[] }) {
  const now = useNow();
  const { runs, scope, guarded, loading } = useSetupRuns(kinds);
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
    // banner at the top of the screen is where the token is supplied.
    return (
      <Card>
        <Card.Header icon={<RotateCwGlyph size="sm" />}>Provisioning passes</Card.Header>
        <span className="t-caption">
          Reading what a pass found needs an operator token, so this is what the engine will not say
          without one.
        </span>
      </Card>
    );
  }

  const columns: GridColumn<SetupRun>[] = [
    {
      key: "ran",
      header: "Ran",
      shrink: true,
      sortValue: (run) => tsKey(run.started_at),
      cell: (run) => <DateCell at={run.started_at} now={now} />,
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
            cell: (run: SetupRun) => <TextCell>{named.get(run.key) ?? run.key}</TextCell>,
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
          <StatusCell
            glyph={state.glyph}
            label={state.label}
            // `app/frame/cells.tsx` still speaks OUR tone names, and it is
            // another port's file, so the spelling is turned back at this one
            // seam rather than this screen keeping a second vocabulary for
            // the one cell that needs it. It goes when that file takes
            // uilet's `Tone` — see `app/frame/tone.ts`, which is this same
            // translation in the other direction.
            tone={cellTone(state.tone)}
            title={state.title}
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
      cell: (run) => <DurationCell ms={passMs(run)} />,
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
      // [NumberCell].
      cell: (run) => (
        <NumberCell
          value={run.findings?.length ?? 0}
          title="what this pass observed that was not fine"
        />
      ),
    },
    {
      key: "detail",
      header: "What it concluded",
      cell: (run) => <TextCell>{concludedLine(run)}</TextCell>,
    },
  ];

  return (
    <Card padding="none">
      <Card.Header
        icon={<RotateCwGlyph size="sm" />}
        count={runs.length}
        subtitle={`what ${scope || "this node"} has run — a pass another node ran is remembered there`}
      >
        Provisioning passes
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
                hint: "The loop runs one per surface on its own cadence, and connecting an integration runs one at once. A pass another node ran is remembered on that node.",
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
  const { run, missing, guarded, loading } = useSetupRun(kind, id);

  if (loading && !run) return <Skeleton variant="text" rows={4} label="Loading" />;
  if (missing) {
    return (
      <div className="int-row-note">
        <span className="int-row-note-text">
          <span className="int-note-problem">This pass is no longer remembered.</span>
          <span className="int-row-note-when">
            A run is kept by the node that executed it, and only for its last few passes. What it
            concluded is folded into the surface&rsquo;s own state either way, which is what the
            card above reports.
          </span>
        </span>
      </div>
    );
  }
  if (guarded) {
    return (
      <div className="int-row-note">
        <span className="int-row-note-text">
          <span className="int-row-note-when">Reading one pass needs an operator token.</span>
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
                <FindingSubjects of={finding} />
                {finding.remedy && <span className="int-note-remedy">{finding.remedy}</span>}
              </li>
            ))}
          </ul>
        ) : (
          <span className="int-row-note-when">
            {run.state === "running"
              ? "Nothing outstanding so far — a pass records what it observes as it goes."
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
        <DateCell at={row.timestamp} now={now} />
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
  const { data, loading, error } = useQuery("integrations", undefined, {
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
        description="The link that opened this names a surface this engine does not have. Every integration it does serve is on the Integrations screen."
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
        <QueryState error={error} loading={loading}>
          {data && present.length === 0 && (
            <EmptyState
              size="compact"
              icon={<PlugGlyph size="xl" />}
              title={`${entry.name} is not connected`}
              description={`${entry.description}. Nothing in this company is configured for it, so no delivery reaches a seat and no pass runs against it.`}
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
                  Surfaces
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
                              {row.reconcile.phase_label || row.reconcile.phase.replace(/_/g, " ")}
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
                  Findings
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
                        <FindingSubjects of={finding} />
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
                      : "Nothing has reported on this integration yet, which is not the same as nothing being wrong."}
                  </span>
                )}
              </Card>

              {/* IS ANYTHING ARRIVING, per surface — the question the facts
                  above count and this one dates. `forge` is left out for the
                  reason the screen leaves it out of its delivery panels: it is
                  a relay, and its events are filed under the product they
                  belong to, so a row for it could only ever be empty. */}
              <Card>
                <Card.Header icon={<InboxGlyph size="sm" />}>Last delivery</Card.Header>
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

/** Which half of the catalogue is shown. */
type Show = "all" | "connected" | "available";

export function Integrations({ kind }: { kind?: string }) {
  // Traffic counters are not pushed, and they move slowly; a minute is the
  // right cadence for "is anything arriving at all".
  //
  // FASTER WHILE SOMETHING IS MOVING. A connect or a disconnect starts work
  // at the engine that takes seconds, and the row is what reports how it
  // went: at a minute's cadence an operator watched a card say "Waiting for
  // the provider" long after it had settled, and read the delay as the
  // failure. The quick cadence is bounded by its own condition, since a
  // surface that has settled leaves the set.
  const [settling, setSettling] = useState(false);
  // SOMETHING WAS JUST ASKED FOR, whether or not anything reflects it yet.
  //
  // The quick cadence below is derived from the ROWS, and a connect has no
  // row to derive it from: the write returns as soon as the revision is
  // activated, and the read that follows can land before the engine has
  // applied it. The screen then holds the pre-connect answer, sees nothing
  // in flight to watch, and waits out the slow poll: an operator pressed
  // Connect, the card said Connect, and only a manual refresh moved it.
  //
  // So a write starts its own window. It is what this screen knows and the
  // rows do not, and it is bounded, because a connect that never shows up is
  // a fault to read about rather than a reason to poll for ever.

  const {
    data,
    loading,
    error,
    refetch: reread,
  } = useQuery("integrations", undefined, {
    pollMs: settling ? 4_000 : 60_000,
    // AND WHENEVER THIS TAB COMES BACK. Setting an integration up means
    // leaving for the third-party app and returning, and returning is the
    // strongest signal there is that the answer moved — stronger than any
    // interval, and the only one that covers the half of the work done
    // somewhere this screen never sees. Measured: a GitHub App installed in
    // about eight seconds, then a card still asking for the install, reloaded
    // by hand to find out why.
    refetchOnFocus: true,
  });
  const setup = useSetup();
  // ONE CLOCK for every relative time on the screen, the header's facts
  // included — two components reading their own is how "4m ago" comes to sit
  // beside "3m ago" for one instant.
  const now = useNow();
  // READ AGAIN AFTER A WRITE, because the first read can land before the
  // engine has applied the revision it just stored. See [useRecheck].
  const { watching, watch } = useRecheck(
    useCallback(() => {
      // BOTH HALVES, every time. The rows say whether the company has a
      // surface and the listing says what its form should show, and a
      // re-read of one leaves the card drawn from two documents that
      // disagree: that is what put a Continue button beside Connected, and
      // later left a disconnected card with no control at all.
      reread();
      setup.reload();
    }, [reread, setup]),
  );
  const [dialog, setDialog] = useState<{
    title: string;
    sections: { name: string; tool: SetupToolState; seat?: string }[];
    /** Set when the form was opened to rotate a credential: the engine's reason. */
    rotate?: string;
  } | null>(null);
  const [dropping, setDropping] = useState<{
    name: string;
    kinds: string[];
    stuck: string;
    apps: { handle: string; name: string; url: string }[];
    appPath?: string;
  } | null>(null);
  // WHICH HALF OF THE CATALOGUE IS SHOWN. Per visit: it narrows eight tiles,
  // and remembering it would open the screen on a filter the reader has
  // forgotten setting, hiding the tile they came for.
  const [show, setShow] = useState<Show>("all");
  const rows = new Map((data?.integrations ?? []).map((r) => [r.key, r]));
  /** Open the settings form for a tool, for the action that asked. */
  const openForm = (entry: Entry, action: TileAction | null, reason: string) =>
    setDialog({
      title: entry.name,
      sections: sectionsFor(entry, setup.byKey),
      rotate: action?.kind === "rotate" ? reason : undefined,
    });
  // THE SEGMENT IS A DESTINATION, not decoration. `kind` was accepted and
  // never read, so `#/settings/integrations/github` rendered the whole catalogue
  // — every link into one integration landed on the list it came from. It
  // matches an entry by its own key OR by any surface it covers, because a
  // finding and a webhook route both name the SURFACE (`jira`), while the
  // card is the tool (`atlassian`).
  const focus = kind
    ? CATALOG.find((e) => e.key === kind || e.surfaces.some((s) => s.key === kind))
    : undefined;
  // THE HEADER'S OWN STATE, out of the same roll-up a card draws — a page and
  // a card disagreeing about a tool would be two answers to one question on
  // one screen.
  const focusState = focus ? entryState(focus, rows, data?.tools) : undefined;
  // THE CRUMB NAMES THE TOOL, not the segment: `#/settings/integrations/jira`
  // is the Atlassian page, and a trail reading "jira" over a header reading
  // Atlassian is two names for one object.
  usePageLabels(useMemo(() => (kind && focus ? { [kind]: focus.name } : {}), [kind, focus]));
  // WHICH SURFACES OF THIS TOOL A PASS CAN EVEN RUN AGAINST, which is what the
  // runs route is keyed on: `integration.Kinds`, the same set the setup
  // listing carries and the same set `DELETE /setup/integrations/{kind}`
  // accepts. `forge` is a surface of the Atlassian card and is not a kind, so
  // asking for its runs is a 404 — the same trap `disconnectOrder` fell into.
  //
  // EMPTY MEANS THE LISTING IS NOT HERE, not that nothing converges: every
  // kind is in it whether or not this company configured it, so the only way
  // to hold none of them is a refused or unfinished read — and a passes panel
  // drawn from that would report "no pass has run" about a question it never
  // asked.
  const focusKinds = focus
    ? focus.surfaces.filter((s) => setup.byKey.has(s.key)).map((s) => s.key)
    : [];
  // A TERMINAL PHASE IS ONE NOBODY IS WAITING ON. Everything else is the
  // engine mid-flight, and the screen's job while that is true is to keep
  // looking. Derived from what arrived rather than from what was clicked, so
  // a pass somebody else started is watched too.
  const moving = [...rows.values()].some((r) => IN_FLIGHT.has(r.reconcile?.phase ?? ""));
  useEffect(() => setSettling(moving || watching), [moving, watching]);
  const configured = CATALOG.filter((e) => e.surfaces.some((s) => rows.has(s.key)));
  const shown = [...CATALOG]
    .sort(byConfiguredThenName(rows))
    .filter((e) => show === "all" || (show === "connected") === configured.includes(e));

  return (
    <>
      {/* WHAT THE SCREEN IS, AND WHICH HALF OF IT. The lede and the filter
          share a row, as on every catalogue in this product: the counts are
          what a reader asks first ("how many do we have?"), and a figure in
          the page bar said it a second time. The counts are a READING, so
          the filter waits for one: "Connected 0" before the rows answer is a
          claim that the company was counted. */}
      {!focus && (
        <div className="int-bar">
          <PageNote>
            Connect the tools the company already uses. Each agent gets its own identity in every
            tool it is allowed to use, and acts there with its own credentials.
          </PageNote>
          {data && (
            <Segmented<Show>
              ariaLabel="Which integrations to show"
              size="sm"
              value={show}
              onChange={setShow}
              options={[
                { value: "all", label: "All" },
                { value: "connected", label: "Connected", count: configured.length },
                {
                  value: "available",
                  label: "Available",
                  count: CATALOG.length - configured.length,
                },
              ]}
            />
          )}
        </div>
      )}

      {/* THE ADDRESS EVERY INBOUND INTEGRATION IS BUILT ON, rendered once. It
          is one setting, and a screen that asked for it per integration would
          ask the operator to keep seven copies consistent. */}
      {setup.base && !setup.base.present && (
        <Callout variant="warning">
          <span className="col" style={{ gap: 4 }}>
            <span>No public address is set, so no third-party app can deliver to this engine.</span>
            <span className="t-caption">
              Set <InlineCode>{setup.base.config_path}</InlineCode> to the HTTPS address third-party
              apps reach this deployment on. Chat over an outbound socket, Mattermost, is
              unaffected.
            </span>
          </span>
        </Callout>
      )}
      {/* SET AND SET TO SOMETHING ARE DIFFERENT FACTS, which is why the
          engine answers `resolved` beside `present` as a three-valued field.
          The banner read `present` alone, so a `${VAR}` pointing at an
          environment variable nobody exported rendered as "Third-party apps
          reach this engine at" followed by an empty code span — the one
          screen that exists to say where deliveries land, saying nothing, on
          exactly the misconfiguration it should name. */}
      {setup.base?.present && setup.base.resolved !== false && (
        <Callout variant="neutral" icon={<LinkGlyph size="md" />}>
          <span>
            Third-party apps reach this engine at <InlineCode>{setup.base.value}</InlineCode>
          </span>
        </Callout>
      )}
      {setup.base?.present && setup.base.resolved === false && (
        <Callout variant="warning">
          <span className="col" style={{ gap: 4 }}>
            <span>
              The public address is configured as a reference that resolves to nothing, so no
              third-party app can deliver to this engine.
            </span>
            <span className="t-caption">
              <InlineCode>{setup.base.config_path}</InlineCode> is set to{" "}
              <InlineCode>
                {setup.base.reference ? `\${${setup.base.reference}}` : "a reference"}
              </InlineCode>
              , which resolves to nothing. Export it, or seal it as a secret, and re-activate the
              revision — a reference is resolved from the snapshot taken when the revision is
              applied.
            </span>
          </span>
        </Callout>
      )}
      {setup.guarded && (
        <Callout variant="neutral" icon={<KeyGlyph size="md" />}>
          <span>
            Setting an integration up needs an operator token. This screen is showing what it can
            read without one.
          </span>
          <span className="spacer" />
          {/* The same door QueryState opens, for the same reason: with
              anonymous reads allowed the socket is never refused, so a banner
              that only NAMES the missing credential leaves the reader with
              nothing on the page that can supply it. */}
          <Button size="small" leadingIcon={<KeyGlyph size="sm" />} onClick={requestToken}>
            Set token
          </Button>
        </Callout>
      )}

      {/* ONE OBJECT, ONE HEADER. `#/settings/integrations/{kind}` is a page about
          a single tool, and it opened with the catalogue's chrome and a card:
          the tool's name lived inside the card's disclosure BUTTON, which is a
          control rather than a title, so nothing on the screen named what it
          was about until the reader's eye reached the row.
          The card stays — it carries the actions, the surfaces and the roster
          — and this is the title and the facts re-homed above it. They are the
          peek's facts, from one function, so the two frames cannot drift; see
          [integrationFacts]. */}
      {focus && focusState && (
        <ObjectHeader
          kind="Integration"
          icon="plug"
          identifier={focus.key}
          title={focus.name}
          status={
            focusState.tag === "" ? undefined : (
              <Tag variant={focusState.tone} appearance={focusState.outline ? "outline" : "soft"}>
                {focusState.tag}
              </Tag>
            )
          }
          facts={
            presentSurfaces(focus, rows).length > 0
              ? integrationFacts(focus, rows, now, {
                  known: data?.traffic_known ?? false,
                  since: data?.traffic_since,
                })
              : undefined
          }
        />
      )}

      {dropping && (
        <DisconnectDialog
          name={dropping.name}
          kinds={dropping.kinds}
          stuck={dropping.stuck || undefined}
          apps={dropping.apps}
          appPath={dropping.appPath}
          onClose={() => setDropping(null)}
          // The row does not vanish here: the engine keeps the block until
          // the third-party app teardown succeeds, so what a re-read shows
          // is the surface moving to Disconnecting.
          onDone={() => {
            // BOTH HALVES, AND KEEP LOOKING AT BOTH. A disconnect leaves
            // the listing calling the surface configured while the rows
            // already say it is gone, which is the same race as a connect
            // in the other direction. See [useRecheck].
            watch();
          }}
        />
      )}

      {dialog && (
        <SetupDialog
          sections={dialog.sections}
          title={dialog.title}
          rotate={dialog.rotate}
          onClose={() => setDialog(null)}
          // BOTH HALVES. The requirements half says what the form should now
          // show; the status half is what reports whether the connect took,
          // and it is the one an operator is looking at when the dialog
          // closes.
          onDone={() => {
            // READ, AND KEEP LOOKING: the first read can land before the
            // engine has applied the revision it just stored. See [useRecheck].
            watch();
          }}
        />
      )}

      {/* BOTH HALVES, because a card drawn from one of them is a card with
          no buttons. See [useSetup]'s `loading`. */}
      {((loading && !data) || setup.loading) && (
        <Skeleton variant="text" rows={6} label="Loading" />
      )}
      <QueryState error={error} loading={loading} empty={undefined}>
        {/* NOTHING CONNECTED IS A FACT ABOUT THE LIST, SAID ABOVE IT. A page
            holding eight tiles is not empty, and what the sentence adds —
            that only a schedule can wake a seat until then — is worth reading
            before the list, not after it. */}
        {data && !kind && configured.length === 0 && (
          <Callout variant="neutral" icon={<PlugGlyph size="md" />}>
            No integration is connected yet. Until one is, the only thing that can wake a seat is a
            schedule — connect a chat surface, a tracker or a code host below.
          </Callout>
        )}
        {kind && !focus && (
          <EmptyState
            icon={<PlugGlyph size="xl" />}
            title={`This build serves no integration called “${kind}”`}
            description="The link that brought you here names a surface this engine does not have. Every integration it does serve is on the Integrations screen."
          />
        )}
        {/* THE CATALOGUE: WHAT THIS COMPANY HAS, THEN WHAT IT COULD HAVE, each
            half alphabetical, one tile per tool with one action each.

            Connected covers ATTEMPTED as well as working — a tile with a
            block behind it, whatever the loop says about it — because a
            broken integration is one this company has and is the tile most
            worth reaching first. Sorting on health instead would move a tile
            out from under the cursor every time an app recovered.

            NOT UNTIL BOTH HALVES HAVE ANSWERED. A tile drawn from the socket
            alone has no setup listing, so its action could not say whether a
            form exists behind it; the skeleton above says so honestly. A
            REFUSED setup read is not waiting: it answers, the banner says so,
            and the tiles render with their form actions disabled and saying
            why. */}
        {!setup.loading && !kind && (
          <div className="int-grid">
            {shown.map((entry) => (
              <IntegrationTile
                key={entry.key}
                entry={entry}
                rows={rows}
                rollups={data?.tools}
                sections={sectionsFor(entry, setup.byKey)}
                trafficKnown={data?.traffic_known ?? false}
                guarded={setup.guarded}
                onOpen={(action, reason) => openForm(entry, action, reason)}
              />
            ))}
            {/* EVERY OTHER TOOL IS AN MCP SERVER, and a catalogue that ended
                at the six vendors this build ships would read as the whole of
                what an agent can reach. A link rather than a tile with an
                action: the servers are configured, listed and checked on
                Tools & MCP. */}
            {show !== "connected" && (
              <a className="int-tile int-tile-more" href={href(["settings", "tools"])}>
                <span className="int-brand int-brand-more" aria-hidden>
                  <PlusGlyph size="sm" />
                </span>
                <span className="int-name">MCP servers</span>
                <span className="int-tile-text">
                  Give agents any tool that speaks MCP — per role, with per-seat credentials.
                </span>
              </a>
            )}
          </div>
        )}
        {/* ONE TOOL'S PAGE, which is where a tile's Manage goes: the agents,
            the surfaces that need something, the settings form and
            Disconnect. A KIND THAT NAMES NOTHING LISTS NOTHING — the empty
            state above already says the link is dead. */}
        {!setup.loading && focus && (
          <EntryRow
            key={focus.key}
            entry={focus}
            rows={rows}
            rollups={data?.tools}
            sections={sectionsFor(focus, setup.byKey)}
            publicBase={setup.base?.value}
            guarded={setup.guarded}
            onOpen={(action, reason) => openForm(focus, action, reason)}
            onDisconnect={() =>
              setDropping({
                name: focus.name,
                // EVERY SURFACE THE CARD COVERS, not the first one.
                //
                // Atlassian is an organization and two products, and
                // disconnecting took only the surface that happened to be
                // listed first: the account was deleted, its block removed,
                // and the card still read Connected because Jira and
                // Confluence were untouched. A person pressing Disconnect on a
                // card means the card.
                kinds: disconnectOrder(focus, rows, sectionsFor(focus, setup.byKey)),
                stuck: stuckDisconnecting(focus, rows),
                // WHAT THE ENGINE CANNOT DELETE ITSELF. A seat carries a
                // manage link only where what it holds has to be removed by
                // hand, so an integration with nothing to hand over renders no
                // list at all — each agent once, however many sections carry
                // the same tool.
                apps: toolsOf(sectionsFor(focus, setup.byKey))
                  .flatMap((tool) => tool.seats ?? [])
                  .filter((seat) => seat.manage_url)
                  .map((seat) => ({
                    handle: seat.handle,
                    // THE AGENT, not the handle: the dialog draws this roster
                    // the way every other roster in the product draws one.
                    name: seat.name || seat.handle,
                    url: seat.manage_url as string,
                  })),
                // WHAT IS LEFT TO CLICK once the link has opened, in the app's
                // own words. Stated once, because it is the same for every
                // agent.
                appPath: sectionsFor(focus, setup.byKey).find((s) => s.tool.manage_path)?.tool
                  .manage_path,
              })
            }
          />
        )}
        {/* WHAT HAS BEEN HAPPENING TO IT, above what has been arriving: every
            finding on this screen comes from a pass, and until this panel
            existed nothing said that one had ever run — `SetupRun` was
            declared on the wire and read by nobody. Only on the focused
            screen, because a history per tool over six cards is six tables
            nobody came for. */}
        {focus && focusKinds.length > 0 && (
          // KEYED ON THE TOOL, so walking from one integration's page to
          // another's does not carry the open pass across: the detail route is
          // keyed on (kind, id), and a pass opened on GitHub would be asked
          // for under Slack and answer 404.
          <SetupPasses key={focus.key} entry={focus} kinds={focusKinds} />
        )}

        {/* WHAT ACTUALLY ARRIVED, one panel per surface the tool covers.
            Per surface rather than per tool because the event rows carry the
            SURFACE as their source — Atlassian is an organization and two
            products, and a Jira delivery and a Confluence one are different
            answers to "is this working". A silent surface says so rather
            than being left out, which is the fact an operator came for. */}
        {focus &&
          focus.surfaces
            // `forge` is a relay rather than a source: it hands Jira and
            // Confluence events on in its own shape and the rows are filed
            // under the product they belong to, so a panel for it could
            // only ever be empty.
            .filter((surface) => surface.key !== "forge")
            .map((surface) => (
              <SurfaceDeliveries key={surface.key} surface={surface.key} name={surface.name} />
            ))}
      </QueryState>
    </>
  );
}
