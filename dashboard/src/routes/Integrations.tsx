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
import type { SetupListing, SetupToolState } from "~/protocol/types.ts";

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
      { key: "jira", name: "Jira" },
      { key: "confluence", name: "Confluence" },
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
  /** The one sentence worth reading without opening the details, if any. */
  status?: string;
  /** Whether that sentence is a problem rather than a note. */
  attention: boolean;
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
    return { tag: "", tone: "neutral", outline: true, attention: false };
  }
  // Prefix a surface's line with its name only when the tool has more than
  // one, so "Jira: the org credential was refused" reads on Atlassian and
  // "Slack: ..." does not on Slack.
  const named = (surface: Surface, text: string) =>
    entry.surfaces.length > 1 ? `${surface.name}: ${text}` : text;

  // THE TWO INGRESS FAULTS ARE READ WHATEVER THE PHASE SAYS, because the
  // reconcile loop does not look at ingress at all: the Jira and GitHub
  // passes run with no webhook base and report nothing about deliveries
  // (internal/engine/integrations.go), while `secret_usable` is computed
  // separately from what this process actually resolved. So `ready` and "every
  // delivery is refused" are not contradictory answers, they are answers to
  // different questions, and a row that stopped at the phase showed a green
  // tag over a surface nothing could reach.
  const unresolved = present.find((p) => p.row.secret_usable === false);
  const unrouted = present.find((p) => p.row.routes === false);
  const ingress = unresolved
    ? named(unresolved.surface, "the webhook secret did not resolve, so every delivery is refused")
    : unrouted
      ? named(
          unrouted.surface,
          "deliveries are verified and stored, and nothing routes them to a seat",
        )
      : undefined;

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
      status: named(going.surface, "this integration is being removed"),
      attention: false,
    };
  }

  const worst = present
    .filter((p) => p.row.reconcile)
    .sort((a, b) => distance(a.row.reconcile!.phase) - distance(b.row.reconcile!.phase))[0];
  if (worst?.row.reconcile) {
    const { phase, actor, detail } = worst.row.reconcile;
    const phaseLine = phase !== "ready" && detail ? named(worst.surface, detail) : undefined;
    return {
      // The ENGINE's word for the phase, not this screen's. `phase_label`
      // is derived once, in Go, from a vocabulary the client does not have
      // to know; the raw phase is the fallback for a node too old to send
      // one, and opening its underscores is all this build can honestly do
      // with a value it may not recognise.
      tag: worst.row.reconcile.phase_label || phase.replace(/_/g, " "),
      tone: phaseTone(phase),
      outline: false,
      // The phase's own sentence when it has one, and the ingress fault
      // otherwise: a ready phase has nothing to say and must not silence it.
      status: phaseLine ?? ingress,
      // ATTENTION MEANS A PERSON IS NEEDED, which is the actor's own
      // question and not "is the phase ready" (internal/integration/report.go:
      // "Nothing the engine or a third-party app is doing needs a person told about
      // it"). Marking `provisioning` and `activating` amber told an operator
      // to act while the engine was still working, beside a tag drawn neutral
      // for the same phase.
      attention: actor === "admin" || actor === "operator" || (!phaseLine && ingress !== undefined),
    };
  }
  if (present.every((p) => p.row.enabled === false)) {
    return { tag: "Paused", tone: "neutral", outline: true, attention: false };
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
    status: ingress,
    attention: ingress !== undefined,
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
 * The surfaces a card's Disconnect takes away, in the order it takes them.
 *
 * THE PROVISIONING SURFACE LAST. On Atlassian the two products are reached
 * with their own credentials and the ORGANIZATION's is what removes the
 * accounts, so taking the organization first would strand every account the
 * products' teardown still has to account for — and leave nothing able to
 * remove them.
 *
 * Only surfaces this company actually has: a card lists what a tool can be
 * made of, and a delete against a surface nobody configured is a request
 * with nothing behind it.
 */
export function disconnectOrder(
  entry: Entry,
  rows: Map<string, IntegrationRow>,
  sections: { name: string; tool: SetupToolState }[],
): string[] {
  const provisions = new Set(sections.filter((s) => s.tool.can_provision).map((s) => s.tool.key));
  const present = entry.surfaces.map((s) => s.key).filter((key) => rows.has(key));
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
 * no action: nothing a person does moves a surface the engine or the third-party app
 * is still working on.
 */
export function actionFor(
  state: EntryState,
  tools: SetupToolState[],
): { label: string; blocks?: string } | null {
  if (tools.length === 0) return null;
  // A TOOL IS CONFIGURED WHEN ANY OF ITS SURFACES IS, and complete only when
  // every configured one is. Atlassian with Jira set up and Confluence not is
  // neither "connect" nor "done": it is a tool with something left to do.
  const configured = tools.filter((t) => t.configured);
  if (configured.length === 0) return { label: "Connect" };
  if (configured.some((t) => !t.satisfied)) return { label: "Continue" };
  // A per-seat third-party app with no seat set up yet is not connected, whatever its
  // company block says: a Slack company with no agent holding an app is a
  // company where nothing can post.
  //
  // ONLY WHERE THE SEATS ARE LOAD-BEARING, which the engine says with
  // seats_required. Every card carries a roster now, and most of those seat
  // credentials are an upgrade on an app that already works — so reading any
  // roster this way put Continue on every connected card, next to Connected.
  if (
    configured.some(
      (t) => t.seats_required && t.seats && t.seats.length > 0 && !t.seats.some((s) => s.satisfied),
    )
  ) {
    return { label: "Continue" };
  }
  if (state.attention) {
    // Narrowed to the fields that clear what the loop actually found, so a
    // Fix opens the inputs that matter rather than the whole form.
    return { label: "Fix", blocks: "credential_missing" };
  }
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
 * per-seat third-party app a row per agent. None of that belongs in the header, which
 * is what the previous layout got wrong: an operator scanning six
 * integrations wants six names and six states, not six paragraphs.
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
  const action = actionFor(state, tools);
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
  const seats = (roster?.seats ?? []).map((seat) => ({ seat, surface: "" }));
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
            <span className={state.attention ? "int-desc attention" : "int-desc"}>
              {state.attention && <Icon name="alert" size="sm" />}
              {state.status ?? entry.description}
            </span>
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
            {seats.map(({ seat, surface }) => (
              <li key={`${surface}:${seat.handle}`} className="int-row int-seat-row">
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
                  <Badge tone={seat.satisfied ? "positive" : "neutral"} outline={!seat.satisfied}>
                    {seat.satisfied ? "ready" : "not set up"}
                  </Badge>
                </div>
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
function useSetup(): {
  byKey: Map<string, SetupToolState>;
  base: SetupListing["public_base_url"] | null;
  guarded: boolean;
  reload: () => void;
} {
  const [listing, setListing] = useState<SetupListing | null>(null);
  const [guarded, setGuarded] = useState(false);

  const reload = useCallback(() => {
    void (async () => {
      try {
        setListing((await rest.get("/setup/integrations")) as SetupListing);
        setGuarded(false);
      } catch (err) {
        // A refusal is not an empty answer. The screen keeps every read it
        // already has and simply offers no writes.
        setListing(null);
        setGuarded(err instanceof RestError && err.unauthorized);
      }
    })();
  }, []);

  useEffect(reload, [reload]);
  // A refusal here is the one the banner asks the reader to fix, so the fix
  // has to land on this screen without a reload.
  useEffect(() => onTokenChanged(reload), [reload]);

  return {
    byKey: new Map((listing?.tools ?? []).map((t) => [t.key, t])),
    base: listing?.public_base_url ?? null,
    guarded,
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
  const { data, loading, error } = useQuery("integrations", undefined, { pollMs: 60_000 });
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
  } | null>(null);
  const rows = new Map((data?.integrations ?? []).map((r) => [r.key, r]));
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

      {/* THE ADDRESS EVERY INBOUND VENDOR IS BUILT ON, rendered once. It is
          one setting, and a screen that asked for it per app would ask the
          operator to keep seven copies consistent. */}
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
          onClose={() => setDropping(null)}
          // The row does not vanish here: the engine keeps the block until
          // the third-party app teardown succeeds, so what a re-read shows is the
          // surface moving to Disconnecting.
          onDone={setup.reload}
        />
      )}

      {dialog && (
        <SetupDialog
          sections={dialog.sections}
          title={dialog.title}
          blocks={dialog.blocks}
          onClose={() => setDialog(null)}
          onDone={setup.reload}
        />
      )}

      {loading && !data && <Skeleton rows={6} />}
      <QueryState error={error} loading={loading} empty={undefined}>
        {/* ALPHABETICAL, in one flat list rather than grouped into panels.
            A capability heading over a group of one is chrome around a
            single row, and the list is short enough that a reader looking
            for a particular app finds it by name.

            It replaces a connected-first sort. That put what was already
            working at the top, which reads well the first time and badly
            afterwards: a list whose order changes as an app connects or
            fails moves a row out from under the cursor of somebody who
            came back to the same screen expecting to find it where it
            was. A name does not move. */}
        <div className="int-list">
          {[...CATALOG]
            .sort((a, b) => a.name.localeCompare(b.name))
            .map((entry) => (
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
                  })
                }
              />
            ))}
        </div>
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
