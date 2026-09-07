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
 * state is the least ready of its surfaces and its status line names the
 * surface that is not. The counts, the inbound paths and the reconcile
 * findings are still here, folded under a per-row disclosure, so an operator
 * who needs to know why can open it without the screen leading with it.
 *
 * Every count is THREE-VALUED (a number, zero, or `null` meaning "this
 * process cannot say"), and so is the reconcile block: `null` is a process
 * with no loop to ask or a surface the loop has not reached, and neither is a
 * claim that the tool is fine.
 */

import { useCallback, useEffect, useState } from "react";
import { ScreenHead } from "~/app/Shell.tsx";
import { QueryState } from "~/components/common.tsx";
import { Avatar, Badge, Button, Empty, Skeleton } from "~/ui/primitives.tsx";
import { Icon, type IconName } from "~/ui/Icon.tsx";
import { VendorMark, type Vendor } from "~/ui/VendorMark.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { SetupDialog } from "./SetupDialog.tsx";
import { DisconnectDialog } from "./DisconnectDialog.tsx";
import { onTokenChanged, requestToken, rest, RestError } from "~/protocol/index.ts";
import type { IntegrationRow, ReconcileStatus } from "~/protocol/types.ts";
import type { SetupListing, SetupSeatState, SetupToolState } from "~/protocol/types.ts";

type Tone = "positive" | "caution" | "critical" | "info" | "neutral";

/**
 * What agents need in order to work, in the order the console asks about
 * them. Each tool answers exactly one capability.
 */
const CAPABILITIES: { id: string; title: string; icon: IconName; description: string }[] = [
  {
    id: "messaging",
    title: "Messaging",
    icon: "message",
    description: "Where agents talk with you and with each other.",
  },
  {
    id: "tasks",
    title: "Task management",
    icon: "target",
    description: "Where work is planned, assigned and tracked.",
  },
  {
    id: "code",
    title: "Code",
    icon: "gitBranch",
    description: "Where agents commit, review and ship.",
  },
  {
    id: "observability",
    title: "Observability",
    icon: "activity",
    description: "Where agents watch production and respond.",
  },
];

/** One engine surface behind a tool: the key the API row carries, named. */
export interface Surface {
  key: string;
  name: string;
}

export interface Entry {
  key: string;
  capability: string;
  name: string;
  description: string;
  vendor: Vendor;
  /** The API rows that together are this tool. Most tools have one. */
  surfaces: Surface[];
}

/**
 * The catalogue: every tool this build serves, in the console's own words,
 * with the engine surfaces that make it up. The surface keys match the
 * `integrations` answer's rows, so the two join by name.
 */
export const CATALOG: Entry[] = [
  {
    key: "slack",
    capability: "messaging",
    name: "Slack",
    description: "Team communication",
    vendor: "slack",
    surfaces: [{ key: "slack", name: "Slack" }],
  },
  {
    key: "mattermost",
    capability: "messaging",
    name: "Mattermost",
    description: "Self-hosted team chat",
    vendor: "mattermost",
    surfaces: [{ key: "mattermost", name: "Mattermost" }],
  },
  {
    key: "atlassian",
    capability: "tasks",
    name: "Atlassian",
    description: "Issue tracking and documentation",
    vendor: "atlassian",
    surfaces: [
      // THE ORGANIZATION FIRST, because it is where an agent's account is
      // created and the two products are what that account then works in.
      // It is not a third product: Jira and Confluence are sites this
      // engine reads and writes AS an account, and this is the only place
      // an account can be made at all.
      { key: "atlassian", name: "Organization" },
      // THEN ALPHABETICALLY. The two products are peers, so any other order
      // is a claim about which matters more, and this order is the one the
      // rest of the screen already sorts by.
      { key: "confluence", name: "Confluence" },
      { key: "jira", name: "Jira" },
      { key: "forge", name: "Forge relay" },
    ],
  },
  {
    key: "github",
    capability: "code",
    name: "GitHub",
    description: "Code and pull requests",
    vendor: "github",
    surfaces: [{ key: "github", name: "GitHub" }],
  },
  {
    key: "gitlab",
    capability: "code",
    name: "GitLab",
    description: "Code and merge requests",
    vendor: "gitlab",
    surfaces: [{ key: "gitlab", name: "GitLab" }],
  },
  {
    key: "datadog",
    capability: "observability",
    name: "Datadog",
    description: "Monitoring and observability",
    vendor: "datadog",
    surfaces: [{ key: "datadog", name: "Datadog" }],
  },
];

/**
 * COLOUR CARRIES STATE, NEVER IDENTITY (see reference/dashboard-design.md), so
 * the tone follows the phase rather than the vendor.
 *
 * `ready` is the only positive one, and `degraded` is caution rather than
 * critical on purpose: agents are still working, which is exactly what
 * separates it from a surface that cannot authenticate at all.
 */
/**
 * The phases that mean the engine is still working.
 *
 * The screen polls faster while any surface is in one, because each is a
 * state that RESOLVES ON ITS OWN within seconds and the row is the only place
 * that says how it resolved.
 */
export const IN_FLIGHT = new Set(["awaiting_admin", "provisioning", "activating", "disconnecting"]);

export function phaseTone(phase: string): Tone {
  switch (phase) {
    case "ready":
      return "positive";
    case "degraded":
      return "caution";
    case "unconfigured":
      return "critical";
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
      return "caution";
    default:
      // A phase a newer node wrote. Rendered as-is in a neutral tone
      // rather than guessed at: claiming a surface is fine on the
      // strength of a word this build cannot interpret is the one answer
      // that is certainly wrong.
      return "neutral";
  }
}

/**
 * The engine's own phase order, "furthest-from-working to working": the
 * `Phases` slice in internal/integration/report.go. Duplicated here because a
 * TypeScript screen cannot import a Go slice, and kept in that file's order so
 * the two cannot say different things about which of two surfaces is worse.
 * `degraded` sits ABOVE `activating` deliberately: a degraded integration is
 * still working, and one still coming up is not.
 */
const PHASE_ORDER = [
  // A teardown wins, the same precedence the console gives it: showing a
  // tool as connected while one of its surfaces is being removed invites a
  // reader to act on something that is going away.
  "disconnecting",
  "unconfigured",
  "awaiting_admin",
  "provisioning",
  "activating",
  "degraded",
  "ready",
];

/**
 * How far from working a phase is, lowest first, so the least ready surface
 * behind a tool is the one its row reports.
 *
 * Doubled so a phase this build does not know can sit BETWEEN the worst phase
 * it knows and ready: such a phase is never presented as the ready one, and
 * never allowed to mask a surface this build knows is broken.
 */
function distance(phase: string): number {
  const at = PHASE_ORDER.indexOf(phase);
  return at >= 0 ? at * 2 : PHASE_ORDER.length * 2 - 3;
}

/** Who has to act, phrased for the person reading it. */
function actorLabel(actor: string | undefined): string {
  switch (actor) {
    case "engine":
      return "the engine is working on it";
    case "provider":
      return "the third-party app is applying it";
    case "admin":
      return "you, at the third-party app";
    case "operator":
      return "you, in the company configuration";
    default:
      return "";
  }
}

/** A tool's rolled-up state: the tag on the right and the one line under the name. */
export interface EntryState {
  /**
   * The word in the badge: the engine's phase label, or Connecting / Paused.
   *
   * EMPTY MEANS NO BADGE. A tool nobody has connected has no status to
   * report — the Connect button beside it already says everything true about
   * it — and a grey "not connected" chip on six of them turned a catalogue
   * into a list of complaints.
   */
  tag: string;
  tone: Tone;
  /** Drawn outlined when the tool is absent or paused, filled when it is live. */
  outline: boolean;
}

type Present = { surface: Surface; row: IntegrationRow };

function presentSurfaces(entry: Entry, rows: Map<string, IntegrationRow>): Present[] {
  return entry.surfaces
    .map((s) => ({ surface: s, row: rows.get(s.key) }))
    .filter((p): p is Present => p.row !== undefined);
}

/**
 * The state of one tool from the rows of its surfaces.
 *
 * The reconcile phase wins when the loop has one, because it is a measured
 * claim, and the LEAST READY surface is the one reported: a tool whose Jira is
 * degraded and whose Confluence is fine is degraded, and the status names
 * Jira so the reader knows which. Without a phase the tag says only what the
 * config says. "Connected" is deliberately not a word used here; a configured
 * tool whose every delivery is refused is not connected, and the status line
 * is what says so.
 */
export function rollUp(entry: Entry, rows: Map<string, IntegrationRow>): EntryState {
  const present = presentSurfaces(entry, rows);
  if (present.length === 0) {
    // NO BADGE. The Connect button is the whole message.
    return { tag: "", tone: "neutral", outline: true };
  }

  // THE TWO INGRESS FAULTS ARE READ WHATEVER THE PHASE SAYS, because the
  // reconcile loop does not look at ingress at all: the Jira and GitHub
  // passes run with no webhook base and report nothing about deliveries
  // (internal/engine/integrations.go), while `secret_usable` is computed
  // separately from what this process actually resolved. So `ready` and "every
  // delivery is refused" are not contradictory answers, they are answers to
  // different questions, and a card that stopped at the phase was drawn green
  // over a surface nothing could reach.
  //
  // IT COLOURS THE TAG RATHER THAN WRITING A SENTENCE. The sentence used to
  // replace the tool's name in the header, which put a complaint where an
  // identity belongs; what is wrong is a note in the body, where the surface
  // that has the fault says which one it is and names it. What the collapsed
  // card owes a reader is that something is off, and the tone is that.
  const ingress = present.some((p) => p.row.secret_usable === false || p.row.routes === false);

  // A TEARDOWN OUTRANKS EVERY OTHER ANSWER. The engine reports the phase as
  // disconnecting the moment one is asked for, but a build that does not know
  // the word would fall through to whatever the last reconcile concluded and
  // show the tool connected — so the intent is read directly too.
  const going = present.find((p) => p.row.reconcile?.disconnecting);
  if (going) {
    return {
      tag: going.row.reconcile?.phase_label || "Disconnecting",
      tone: "neutral",
      outline: false,
    };
  }

  const worst = present
    .filter((p) => p.row.reconcile)
    .sort((a, b) => distance(a.row.reconcile!.phase) - distance(b.row.reconcile!.phase))[0];
  if (worst?.row.reconcile) {
    const { phase } = worst.row.reconcile;
    return {
      // The ENGINE's word for the phase, not this screen's. `phase_label`
      // is derived once, in Go, from a vocabulary the client does not have
      // to know; the raw phase is the fallback for a node too old to send
      // one, and opening its underscores is all this build can honestly do
      // with a value it may not recognise.
      tag: worst.row.reconcile.phase_label || phase.replace(/_/g, " "),
      // THE PHASE'S OWN TONE, unless nothing can reach the surface: a ready
      // phase drawn green over a route refusing every delivery is the one
      // combination this screen must never show.
      tone: ingress ? "caution" : phaseTone(phase),
      outline: false,
    };
  }
  if (present.every((p) => p.row.enabled === false)) {
    return { tag: "Paused", tone: "neutral", outline: true };
  }
  // CONFIGURED, AND THE LOOP HAS NOT REPORTED YET. That is a window of one
  // reconcile interval after connecting, not a resting state, so the word is
  // the one the console uses for it.
  //
  // It read "Not checked", which was true of an engine that would not run a
  // pass until somebody pressed Run setup: nothing was going to check it, and
  // the tag was telling the operator so. The loop provisions now, so the same
  // tag would be reporting an absence that resolves itself in seconds, next
  // to a Connect button that had already gone.
  return {
    tag: "Connecting",
    // Amber for the same reason every in-progress phase is: this is the
    // window before the loop's first report, and it is not yet working.
    tone: "caution",
    outline: true,
  };
}

/**
 * What the reconcile loop last found for one surface.
 *
 * Renders NOTHING when the status is null, and the silence is the point: a
 * standalone API has no loop to ask and the loop may not have reached this
 * surface yet, and neither of those is a claim that the surface is healthy.
 * A green tick here would be exactly the invented health this screen has
 * always refused to show.
 */
export function Reconcile({
  status,
  detail,
}: {
  status: ReconcileStatus | null | undefined;
  /** The surface's own one-line summary, when the loop has nothing to add. */
  detail?: string | null;
}) {
  if (!status) {
    // A surface with no loop still has a sentence worth showing, and dropping
    // it here is what left a paused integration explaining nothing at all.
    return detail ? (
      <div className="int-row-note">
        <span className="int-row-note-text">{detail}</span>
      </div>
    ) : null;
  }

  const actor = actorLabel(status.actor);
  // The findings the phase was NOT derived from. The report says what to do
  // next and the findings say what is actually wrong, so an operator who
  // fixes the first should not wait a full pass to learn there was a second.
  const rest = (status.findings ?? []).slice(1);

  // A WORKING SURFACE SAYS NOTHING.
  //
  // The band used to carry the loop's own clock — when it last settled and
  // when it next runs — under every surface, which is the engine narrating
  // its schedule to somebody who asked what state their integration is in.
  // On a healthy surface that timestamp was the ONLY thing in the band, so
  // the card grew a grey stripe per surface saying nothing had happened.
  // The tag says the state; the band is for what a person has to act on.
  if (!status.detail && !status.action_url && !status.last_error && rest.length === 0) {
    return null;
  }

  return (
    <div className="int-row-note">
      <span className="int-row-note-text">
        {status.detail && <span>{status.detail}</span>}
        {actor && <span className="int-row-note-when">{actor}</span>}

        {status.action_url && (
          <a href={status.action_url} target="_blank" rel="noreferrer">
            Open where this is fixed
          </a>
        )}

        {status.last_error && (
          <span className="int-row-note-when" title="the last pass could not read this surface">
            Last pass failed: {status.last_error}
          </span>
        )}

        {rest.length > 0 && (
          <details>
            <summary className="int-summary">
              {rest.length} more finding{rest.length === 1 ? "" : "s"}
            </summary>
            <ul className="col gap-1 int-findings">
              {rest.map((f, i) => (
                <li key={`${f.kind}:${f.subject ?? ""}:${i}`}>
                  {f.detail || `${f.kind.replace(/_/g, " ")}${f.subject ? `: ${f.subject}` : ""}`}
                </li>
              ))}
            </ul>
          </details>
        )}
      </span>
    </div>
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
 * THE PROVISIONING SURFACE LAST. On Atlassian the two products are reached
 * with their own credentials and the ORGANIZATION's is what removes the
 * accounts, so taking the organization first would strand every account the
 * products' teardown still has to account for, and leave nothing able to
 * remove them.
 *
 * Only surfaces this company actually has: a card lists what a tool can be
 * made of, and a delete against a surface nobody configured is a request with
 * nothing behind it.
 *
 * CONFIGURED, NOT TRAFFICKED, and that distinction is the whole of this
 * function's history. It asked the traffic rows, which answer "does this
 * surface have an inbound route" — and Atlassian's organization has none: it
 * receives no deliveries, it is where accounts are made. So the one surface
 * whose teardown deletes the service accounts was filtered out of every
 * disconnect, and "remove the accounts Crewlet created" removed nothing while
 * reporting success. The sections know what is configured; the rows only know
 * what has been delivered to.
 */
export function disconnectOrder(
  entry: Entry,
  rows: Map<string, IntegrationRow>,
  sections: { name: string; tool: SetupToolState }[],
): string[] {
  const provisions = new Set(sections.filter((s) => s.tool.can_provision).map((s) => s.tool.key));
  const configured = new Set(sections.filter((s) => s.tool.configured).map((s) => s.tool.key));
  const present = entry.surfaces
    .map((s) => s.key)
    .filter((key) => configured.has(key) || rows.has(key));
  return [
    ...present.filter((key) => !provisions.has(key)),
    ...present.filter((key) => provisions.has(key)),
  ];
}

/**
 * What one surface has to report, or nothing./**
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
}: {
  surface: Surface;
  row: IntegrationRow;
  /** Whether to say which surface this is: only where the tool has more than one. */
  named: boolean;
}) {
  const dropped = typeof row.skipped === "number" && row.skipped > 0;
  const faulted =
    row.secret_usable === false ||
    row.routes === false ||
    row.enabled === false ||
    dropped ||
    Boolean(row.reconcile?.detail) ||
    Boolean(row.reconcile?.last_error) ||
    (row.reconcile?.findings ?? []).length > 1;
  if (!faulted) return null;

  return (
    <li className="int-row">
      <div className="int-row-identity">
        {named && <span className="int-row-name">{surface.name}</span>}
      </div>

      <div className="int-row-badges">
        {row.secret_usable === false && (
          <Badge
            tone="caution"
            outline
            title="the config names a secret whose ${VAR} resolved to nothing, so every delivery is refused"
          >
            secret unresolved
          </Badge>
        )}
        {row.routes === false && (
          <Badge
            tone="caution"
            outline
            title="deliveries are verified and stored, and no parser turns them into work for a seat"
          >
            routes nowhere
          </Badge>
        )}
        {row.enabled === false && <Badge outline>paused</Badge>}
      </div>

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

      <Reconcile status={row.reconcile} detail={row.detail} />
    </li>
  );
}

/**
 * What the row's button does, from what the engine says about the tool.
 *
 * A PURE FUNCTION of (satisfied, phase, actor), so the action and the state
 * tag beside it can never tell an operator two different things. Empty means
 * no action: nothing a person does moves a surface the engine or the
 * third-party app is still working on.
 */
export function actionFor(
  state: EntryState,
  tools: SetupToolState[],
  present = false,
): { label: string; blocks?: string } | null {
  if (tools.length === 0) return null;
  // THE TWO HALVES OF THIS SCREEN ARRIVE SEPARATELY, and the socket's rows
  // are the quicker one. Straight after a connect the rows already say the
  // block exists while this listing is still the pre-connect one, and a
  // card then drew "Connect" beside a tag reading Connected: two controls
  // describing the same tool, disagreeing.
  //
  // The rows are the fresher answer to "does this company have this", so
  // where they say a surface is there, this half is behind rather than
  // reporting an unconnected tool. Nothing is offered until it catches up,
  // which is a moment, and the tag carries the truth throughout.
  const stale = present && tools.every((t) => !t.configured);
  if (stale) return null;
  // A TOOL IS CONFIGURED WHEN ANY OF ITS SURFACES IS, and complete only when
  // every configured one is. Atlassian with Jira set up and Confluence not is
  // neither "connect" nor "done": it is a tool with something left to do.
  const configured = tools.filter((t) => t.configured);
  if (configured.length === 0) return { label: "Connect" };
  // A BOX LEFT TO FILL IS WHAT THIS BUTTON FIXES, and `satisfied` is not
  // that question: it folds in the seats, and a GitHub seat's requirements
  // are empty because both acts that produce an agent's app happen at GitHub,
  // from that agent's own row. Read as "unsatisfied means offer the form", a
  // card whose only outstanding work was two clicks in a browser drew a
  // Continue button beside "Action needed" that opened a dialog with nothing
  // in it to answer.
  //
  // `form_complete` is the engine's own count of unanswered requirements,
  // company block and seats together. Absent from a node too old to send it,
  // which falls back to the folded answer rather than to silence: a button
  // that should not be there is a smaller fault than a form nobody can reach.
  if (configured.some((t) => (t.form_complete ?? t.satisfied) === false)) {
    return { label: "Continue" };
  }
  // A CARD WITH A SURFACE LEFT TO CONNECT SAYS SO, and says "Continue".
  //
  // Atlassian is an organization and two products, and connecting the
  // organization alone left the card with no button at all: the one
  // configured surface was satisfied, the unconfigured ones did not make the
  // tool unfinished, and there was nothing on screen that would add them.
  // Being partly connected is a state to act on, and the action is the same
  // dialog that started it.
  //
  // NOT "Connect", which is what it said and which contradicts the tag
  // beside it: a card cannot be Connected and offer to connect. Continue is
  // the word this screen already uses for a tool with something left to do,
  // and it is true of a half-connected card whichever half is missing.
  if (tools.some((t) => !t.configured)) {
    return { label: "Continue" };
  }
  // A FAULT IS NOT AN ACTION. A card that needs attention says so in its tag,
  // and what is wrong and where to fix it are the note in its body and the
  // settings the gear opens: the same settings, not a narrowed copy. A second button
  // beside Disconnect, appearing and disappearing as an integration breaks
  // and recovers, was a control whose whole content the line above it
  // already carried.
  // NOTHING FOR A WORKING TOOL. It returned "Manage", which is the one label
  // here that named a place rather than a thing to do: every other value is
  // the engine saying a person is needed, and Manage was the engine saying
  // nobody is. Sitting beside Disconnect it read as the primary action of a
  // card whose primary action was to leave it alone, so changing a setting
  // is the Settings control in the body and this returns nothing.
  return null;
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
      <a className="btn sm" href={seat.action_url} target="_blank" rel="noreferrer">
        Install on {app}
      </a>
    );
  }
  if (seat.step !== "create_app") return null;

  return (
    <>
      <Button size="sm" variant="primary" disabled={busy} onClick={() => void create()}>
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

/**
 * One integration, as a card.
 *
 * THE SHAPE THE CONSOLE USES, and it is two states of one object rather than
 * two components. A tool nobody has connected is a bordered card: mark, name,
 * what it is for, and the one action that starts it. A connected one is the
 * same card with a header that discloses a body, so it reads as the thing it
 * already was, just taller.
 *
 * The body is where the engine's own plumbing lives: a row per surface with
 * its counts, its path and what the reconcile loop last found, and for a
 * per-seat third-party app a row per agent. None of that belongs in the
 * header, which is what the previous layout got wrong: an operator scanning
 * six integrations wants six names and six states, not six paragraphs.
 */
export function EntryRow({
  entry,
  rows,
  sections,
  onConnect,
  onDisconnect,
}: {
  entry: Entry;
  rows: Map<string, IntegrationRow>;
  /** The engine's setup state per surface this tool is made of. */
  sections?: { name: string; tool: SetupToolState }[];
  /** Open the settings form: the connect form, and the same one afterwards. */
  onConnect?: (blocks?: string) => void;
  /** Take the tool away. Absent for a tool nothing has configured. */
  onDisconnect?: () => void;
}) {
  const [open, setOpen] = useState(false);
  const state = rollUp(entry, rows);
  const present = presentSurfaces(entry, rows);
  const absent = present.length === 0;
  // BY KEY, because a per-seat app contributes one section per agent and
  // they all carry the same tool. Counting it once per section made a
  // roster of one agent render four rows on the Atlassian card.
  const tools = [...new Map((sections ?? []).map((s) => [s.tool.key, s.tool])).values()];
  const action = actionFor(state, tools, !absent);
  // ONE ROW PER AGENT, whatever the card is made of.
  //
  // A company with one agent saw THREE rows on Atlassian — Organization,
  // Jira, Confluence — because every surface reports its own roster and the
  // card listed them all. They are one person's one account: Atlassian is
  // where it is created, and the two products are what it then works in.
  //
  // The surface that PROVISIONS wins, because it is the one whose answer is
  // about the account rather than about a credential slot. Where no surface
  // provisions, the first roster with anything in it is as good as any: they
  // are reading the same seat's mcp_env.
  const rosters = tools.filter((t) => (t.seats ?? []).length > 0);
  const roster =
    rosters.find((t) => t.seats_required) ?? rosters.find((t) => t.can_provision) ?? rosters[0];
  const seats = roster?.seats ?? [];
  const bodyID = `int-body-${entry.key}`;

  // A FRAGMENT, not a wrapper: the header already has one actions row, and
  // nesting a second inside it would put a flex container in a flex
  // container for nothing.
  //
  // ONE BUTTON, which is what the console's own card has.
  //
  // It carried four — Manage, Disconnect, and Run setup and Recheck per
  // surface — and three of them existed only because this loop used to
  // refuse to provision: somebody had to press something to grant a
  // permission they had already granted by connecting. The loop does that
  // work now, so the buttons have nothing left to ask for, and the card is
  // a state and a way out.
  //
  // Settings live under the disclosure, beside the surfaces they configure,
  // rather than behind a header button competing with Disconnect.
  const actions = (
    <>
      {state.tag !== "" && (
        <Badge tone={state.tone} outline={state.outline}>
          {state.tag}
        </Badge>
      )}
      {action && onConnect && (
        <Button size="sm" variant="primary" onClick={() => onConnect(action.blocks)}>
          {action.label}
        </Button>
      )}
      {!absent && onDisconnect && (
        <Button size="sm" variant="ghost" onClick={onDisconnect}>
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
    <section className="int-card">
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
            <span className="int-name">{entry.name}</span>
            {/* WHAT THE TOOL IS, never how it is doing. This line carried the
                roll-up's status sentence in amber, so a card's identity was
                replaced by its latest complaint and every reader scanning the
                list read six warnings where six names belong.
                The state is the tag; what is wrong is a note in the body,
                which is where the surface's badges, the loop's own sentence,
                the link that fixes it and the rest of its findings already
                are. Opening the card is what asks the question this answers. */}
            <span className="int-desc">{entry.description}</span>
          </span>
        </button>
        <div className="int-card-actions">
          <button
            type="button"
            className={open ? "int-chevron is-open" : "int-chevron"}
            aria-expanded={open}
            aria-controls={bodyID}
            aria-label={open ? `Hide ${entry.name} details` : `Show ${entry.name} details`}
            onClick={() => setOpen((was) => !was)}
          >
            <Icon name="chevronDown" size="sm" />
          </button>
          {/* SETTINGS BESIDE THE DISCLOSURE, as a square the size of the
              chevron. It sat at the foot of the open card, which put an
              always-available control behind a disclosure and one scroll
              away; as a labelled button in the header it had competed with
              Disconnect for the reader's eye. An icon square does neither:
              it reads as chrome belonging to the row, next to the other
              control that does. */}
          {onConnect && !absent && (
            <button
              type="button"
              className="int-chevron"
              aria-label={`${entry.name} settings`}
              title={`${entry.name} settings`}
              onClick={() => onConnect()}
            >
              <Icon name="gear" size="sm" />
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
                named={present.length > 1}
              />
            ))}
            {seats.map((seat) => (
              <li key={seat.handle} className="int-row int-seat-row">
                {/* THE AGENT'S OWN MARK, the same one the org chart, the
                    people list and every seat chip render. An agent should
                    look like itself wherever it appears, which is also what
                    the console does on its roster: a row of bare names reads
                    as configuration, and a row with the agent's mark reads
                    as the person it stands for. */}
                <Avatar name={seat.name || seat.handle} size="sm" />
                <div className="int-row-identity">
                  <span className="int-row-name">{seat.name || seat.handle}</span>
                  {/* WHERE THIS AGENT'S OWN CREDENTIAL IS, or what is
                      missing. It read "no inbound path yet" against every
                      agent of every app but Slack, because Slack is the only
                      one with a route per seat: true, and silent about the
                      thing the row exists to answer, which is whether this
                      agent can act as itself here. */}
                  <span className="int-row-detail">
                    {seat.inbound_path ? (
                      <code className="inline">{seat.inbound_path}</code>
                    ) : seat.detail ? (
                      seat.detail
                    ) : (
                      "nothing set up for this agent"
                    )}
                  </span>
                </div>
                <div className="int-row-badges">
                  {/* HOW MUCH THIS AGENT MAY DO, where the app has tiers. It
                      is the one thing about a per-agent app that a roster
                      cannot be read without: two agents on the same card can
                      hold apps with different permissions, and "ready" says
                      the same word over both. Rendered in the engine's own
                      value with its underscores opened, since the tiers are a
                      closed set this build must not be caught renaming. */}
                  {seat.tier && (
                    <Badge outline title="The access tier this agent's app is created with">
                      {seat.tier.replace(/_/g, " ")}
                    </Badge>
                  )}
                  <Badge tone={seat.satisfied ? "positive" : "neutral"} outline={!seat.satisfied}>
                    {seat.satisfied ? "ready" : "not set up"}
                  </Badge>
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
  const [listing, setListing] = useState<SetupListing | null>(null);
  const [guarded, setGuarded] = useState(false);
  const [loading, setLoading] = useState(true);

  // `quiet` re-reads without the skeleton, for a refresh nobody asked for.
  // Every re-read a person triggers keeps it, because the two halves of this
  // screen disagree for a moment either side of a connect and a card drawn
  // from one of them is wrong; a background refresh has no such moment, and
  // blanking six cards because somebody came back to the tab would be the
  // screen reporting an absence that is not there.
  const reload = useCallback((quiet = false) => {
    if (!quiet) setLoading(true);
    void (async () => {
      try {
        setListing((await rest.get("/setup/integrations")) as SetupListing);
        setGuarded(false);
      } catch (err) {
        // A refusal is not an empty answer. The screen keeps every read it
        // already has and simply offers no writes.
        setListing(null);
        setGuarded(err instanceof RestError && err.unauthorized);
      } finally {
        // ANSWERED, not answered WELL. A refusal is a state the screen can
        // render honestly, with the banner and no buttons; waiting is not.
        setLoading(false);
      }
    })();
  }, []);

  useEffect(reload, [reload]);
  // A refusal here is the one the banner asks the reader to fix, so the fix
  // has to land on this screen without a reload.
  useEffect(() => onTokenChanged(reload), [reload]);
  // AN AGENT'S APP IS SET UP AT THE CODE HOST, IN ANOTHER TAB, and this
  // listing is the only thing that carries the roster: nothing pushes it, and
  // no answer this screen holds says when a person finished creating an app.
  // So it is re-read when the tab comes back, which is exactly the moment a
  // seat that offered Create needs to be offering Install instead.
  useEffect(() => {
    const onVisible = () => {
      if (document.visibilityState === "visible") reload(true);
    };
    document.addEventListener("visibilitychange", onVisible);
    return () => document.removeEventListener("visibilitychange", onVisible);
  }, [reload]);

  return {
    byKey: new Map((listing?.tools ?? []).map((t) => [t.key, t])),
    base: listing?.public_base_url ?? null,
    guarded,
    loading,
    reload,
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

export function Integrations() {
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
  const {
    data,
    loading,
    error,
    refetch: reread,
  } = useQuery("integrations", undefined, { pollMs: settling ? 4_000 : 60_000 });
  const setup = useSetup();
  const [dialog, setDialog] = useState<{
    title: string;
    sections: { name: string; tool: SetupToolState }[];
    blocks?: string;
  } | null>(null);
  const [dropping, setDropping] = useState<{
    name: string;
    kinds: string[];
    stuck: string;
    apps: { handle: string; url: string }[];
  } | null>(null);
  const rows = new Map((data?.integrations ?? []).map((r) => [r.key, r]));
  // A TERMINAL PHASE IS ONE NOBODY IS WAITING ON. Everything else is the
  // engine mid-flight, and the screen's job while that is true is to keep
  // looking. Derived from what arrived rather than from what was clicked, so
  // a pass somebody else started is watched too.
  const moving = [...rows.values()].some((r) => IN_FLIGHT.has(r.reconcile?.phase ?? ""));
  useEffect(() => setSettling(moving), [moving]);
  const configured = CATALOG.filter((e) => e.surfaces.some((s) => rows.has(s.key)));

  return (
    <>
      <ScreenHead
        title="Integrations"
        sub="The tools the company works in. Each agent acts as itself on these, with its own credentials."
        badges={
          <Badge outline>
            {configured.length} of {CATALOG.length} configured
          </Badge>
        }
      />

      {/* THE ADDRESS EVERY INBOUND INTEGRATION IS BUILT ON, rendered once. It
          is one setting, and a screen that asked for it per integration would
          ask the operator to keep seven copies consistent. */}
      {setup.base && !setup.base.present && (
        <div className="banner caution">
          <Icon name="alert" size="sm" />
          <span className="col" style={{ gap: 4 }}>
            <span>No public address is set, so no third-party app can deliver to this engine.</span>
            <span className="t-caption">
              Set <code className="inline">{setup.base.config_path}</code> to the HTTPS address
              third-party apps reach this deployment on. Chat over an outbound socket, Mattermost,
              is unaffected.
            </span>
          </span>
        </div>
      )}
      {setup.base?.present && (
        <div className="banner neutral">
          <Icon name="link" size="sm" />
          <span>
            Third-party apps reach this engine at <code className="inline">{setup.base.value}</code>
          </span>
        </div>
      )}
      {setup.guarded && (
        <div className="banner neutral">
          <Icon name="key" size="sm" />
          <span>
            Setting an integration up needs an operator token. This screen is showing what it can
            read without one.
          </span>
          <span className="spacer" />
          {/* The same door QueryState opens, for the same reason: with
              anonymous reads allowed the socket is never refused, so a banner
              that only NAMES the missing credential leaves the reader with
              nothing on the page that can supply it. */}
          <Button size="sm" icon="key" onClick={requestToken}>
            Set token
          </Button>
        </div>
      )}

      {dropping && (
        <DisconnectDialog
          name={dropping.name}
          kinds={dropping.kinds}
          stuck={dropping.stuck || undefined}
          apps={dropping.apps}
          onClose={() => setDropping(null)}
          // The row does not vanish here: the engine keeps the block until
          // the third-party app teardown succeeds, so what a re-read shows
          // is the surface moving to Disconnecting.
          onDone={() => {
            setup.reload();
            reread();
          }}
        />
      )}

      {dialog && (
        <SetupDialog
          sections={dialog.sections}
          title={dialog.title}
          blocks={dialog.blocks}
          onClose={() => setDialog(null)}
          // BOTH HALVES. The requirements half says what the form should now
          // show; the status half is what reports whether the connect took,
          // and it is the one an operator is looking at when the dialog
          // closes.
          onDone={() => {
            setup.reload();
            reread();
          }}
        />
      )}

      {/* BOTH HALVES, because a card drawn from one of them is a card with
          no buttons. See [useSetup]'s `loading`. */}
      {((loading && !data) || setup.loading) && <Skeleton rows={6} />}
      <QueryState error={error} loading={loading} empty={undefined}>
        {/* WHAT THIS COMPANY HAS, THEN WHAT IT COULD HAVE, each half
            alphabetical. One flat list rather than panels: a capability
            heading over a group of one is chrome around a single row.

            Connected covers ATTEMPTED as well as working — a card with a
            block behind it, whatever the loop says about it — because a
            broken integration is one this company has and is the row most
            worth reaching first. Sorting on health instead would move a row
            out from under the cursor every time an app recovered. */}
        {/* NOT UNTIL BOTH HALVES HAVE ANSWERED. A card drawn from the socket
            alone has no tools, and a card with no tools offers no buttons and
            no state: the row appeared bare and stayed that way until
            something made the page re-render, which is why it looked like a
            refresh fixed it. The skeleton above says the same thing honestly.
            A REFUSED setup read is not waiting: it answers, the banner says
            so, and the cards render without their writes. */}
        {!setup.loading && (
          <div className="int-list">
            {[...CATALOG].sort(byConfiguredThenName(rows)).map((entry) => (
              <EntryRow
                key={entry.key}
                entry={entry}
                rows={rows}
                sections={sectionsFor(entry, setup.byKey)}
                onConnect={(blocks) =>
                  setDialog({
                    title: entry.name,
                    sections: sectionsFor(entry, setup.byKey),
                    blocks,
                  })
                }
                onDisconnect={() =>
                  setDropping({
                    name: entry.name,
                    // EVERY SURFACE THE CARD COVERS, not the first one.
                    //
                    // Atlassian is an organization and two products, and
                    // disconnecting took only the surface that happened to
                    // be listed first: the account was deleted, its block
                    // removed, and the card still read Connected because
                    // Jira and Confluence were untouched. A person pressing
                    // Disconnect on a card means the card.
                    kinds: disconnectOrder(entry, rows, sectionsFor(entry, setup.byKey)),
                    stuck: stuckDisconnecting(entry, rows),
                    // WHAT THE ENGINE CANNOT DELETE ITSELF. A seat carries a
                    // manage link only where what it holds has to be removed
                    // by hand, so an integration with nothing to hand over
                    // renders no list at all.
                    apps: sectionsFor(entry, setup.byKey)
                      .flatMap((section) => section.tool.seats ?? [])
                      .filter((seat) => seat.manage_url)
                      .map((seat) => ({ handle: seat.handle, url: seat.manage_url as string })),
                  })
                }
              />
            ))}
          </div>
        )}
        {data && configured.length === 0 && (
          <Empty
            icon="plug"
            title="No integration is connected yet"
            hint="Until one is, the only thing that can wake a seat is a schedule. Connect a chat surface, a tracker or a code host in the company configuration."
          />
        )}
      </QueryState>
    </>
  );
}
