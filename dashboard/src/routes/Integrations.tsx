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
import { Badge, Button, Empty, Panel, Skeleton } from "~/ui/primitives.tsx";
import { Icon, type IconName } from "~/ui/Icon.tsx";
import { VendorMark, type Vendor } from "~/ui/VendorMark.tsx";
import { useToast } from "~/ui/Toast.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtDateTime } from "~/lib/format.ts";
import { SetupDialog } from "./SetupDialog.tsx";
import { PassDialog } from "./PassDialog.tsx";
import { onTokenChanged, requestToken, rest, RestError } from "~/protocol/index.ts";
import type { IntegrationRow, ReconcileStatus } from "~/protocol/types.ts";
import type { SetupListing, SetupRun, SetupToolState } from "~/protocol/types.ts";

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
    case "awaiting_admin":
      return "info";
    case "provisioning":
    case "activating":
      return "neutral";
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
      return "the vendor is applying it";
    case "admin":
      return "you, at the vendor";
    case "operator":
      return "you, in the company configuration";
    default:
      return "";
  }
}

/** A tool's rolled-up state: the tag on the right and the one line under the name. */
export interface EntryState {
  /**
   * The word in the badge: the engine's phase label, or Not checked / Paused.
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
      // "Nothing the engine or a vendor is doing needs a person told about
      // it"). Marking `provisioning` and `activating` amber told an operator
      // to act while the engine was still working, beside a tag drawn neutral
      // for the same phase.
      attention: actor === "admin" || actor === "operator" || (!phaseLine && ingress !== undefined),
    };
  }
  if (present.every((p) => p.row.enabled === false)) {
    return { tag: "Paused", tone: "neutral", outline: true, attention: false };
  }
  // No measured claim at all. The config's own word, and the ingress fault if
  // there is one, which is the only thing that can be said without a pass.
  return {
    // NOT "connected". The block exists and no pass has reported on it, so
    // there is no measured claim to make: saying connected here would be the
    // invented health this screen refuses to show.
    tag: "Not checked",
    tone: "neutral",
    outline: true,
    status: ingress,
    attention: ingress !== undefined,
  };
}

function Count({ value, label }: { value: number | null | undefined; label: string }) {
  if (value == null) {
    return (
      <span
        className="t-caption faint"
        title="this process cannot answer; it is not serving ingress"
      >
        {label}: unknown
      </span>
    );
  }
  return (
    <span className="t-caption t-num">
      {label}: {value.toLocaleString()}
    </span>
  );
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

        <span className="int-row-note-when">
          {status.settled_at ? `settled ${fmtDateTime(status.settled_at)}` : "not settled yet"}
          {status.next_attempt_at ? ` · next check ${fmtDateTime(status.next_attempt_at)}` : ""}
        </span>
      </span>
    </div>
  );
}

/**
 * One surface inside a tool's details: its own phase, what arrived and what
 * became of it, where it listens, and what the loop found.
 */
function SurfaceRow({
  surface,
  row,
  onPass,
  running,
}: {
  surface: Surface;
  row: IntegrationRow;
  /** Run this surface's provisioning pass, or a read-only check of it. */
  onPass?: (readOnly: boolean) => void;
  running?: boolean;
}) {
  return (
    <li className="int-row">
      <div className="int-row-identity">
        <span className="int-row-name">{surface.name}</span>
        <span className="int-row-detail int-row-facts">
          {/* The three counts GET /integrations answers with. Read together:
              "128 arrived" alone cannot tell a working surface from one whose
              every delivery reaches nobody. */}
          <Count value={row.inbound} label="in" />
          <Count value={row.skipped} label="dropped" />
          <Count value={row.coalesced} label="merged" />
          {typeof row.inbound_path === "string" && row.inbound_path && (
            <code className="inline">{String(row.inbound_path)}</code>
          )}
        </span>
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
        {row.reconcile ? (
          <Badge tone={phaseTone(row.reconcile.phase)}>
            {row.reconcile.phase_label || row.reconcile.phase.replace(/_/g, " ")}
          </Badge>
        ) : row.enabled === false ? (
          <Badge outline>paused</Badge>
        ) : null}
      </div>

      {onPass && (
        <div className="int-row-actions">
          <Button size="sm" onClick={() => onPass(false)} disabled={running}>
            {running ? "Running" : "Run setup"}
          </Button>
          <Button size="sm" variant="ghost" onClick={() => onPass(true)} disabled={running}>
            Recheck
          </Button>
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
 * no action: nothing a person does moves a surface the engine or the vendor
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
  // A per-seat vendor with no seat set up yet is not connected, whatever its
  // company block says: a Slack company with no agent holding an app is a
  // company where nothing can post.
  if (configured.some((t) => t.seats && t.seats.length > 0 && !t.seats.some((s) => s.satisfied))) {
    return { label: "Continue" };
  }
  if (state.attention) {
    // Narrowed to the fields that clear what the loop actually found, so a
    // Fix opens the inputs that matter rather than the whole form.
    return { label: "Fix", blocks: "credential_missing" };
  }
  return { label: "Manage" };
}

/**
 * The sections of one tool's dialog: the surfaces the engine reaches it over,
 * and for a vendor whose credentials live on the SEAT, one per agent.
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
    if (tool.seats && tool.seats.length > 0) {
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
 * per-seat vendor a row per agent. None of that belongs in the header, which
 * is what the previous layout got wrong: an operator scanning six
 * integrations wants six names and six states, not six paragraphs.
 */
export function EntryRow({
  entry,
  rows,
  sections,
  onConnect,
  onPass,
  running,
}: {
  entry: Entry;
  rows: Map<string, IntegrationRow>;
  /** The engine's setup state per surface this tool is made of. */
  sections?: { name: string; tool: SetupToolState }[];
  onConnect?: (blocks?: string) => void;
  /** Run a surface's provisioning pass, or a read-only check of it. */
  onPass?: (tool: SetupToolState, readOnly: boolean) => void;
  /** A pass this row started and is waiting on. */
  running?: boolean;
}) {
  const [open, setOpen] = useState(false);
  const state = rollUp(entry, rows);
  const present = presentSurfaces(entry, rows);
  const absent = present.length === 0;
  const tools = (sections ?? []).map((s) => s.tool);
  const action = actionFor(state, tools);
  const seats = tools.flatMap((t) => t.seats ?? []);
  const bodyID = `int-body-${entry.key}`;

  // A FRAGMENT, not a wrapper: the header already has one actions row, and
  // nesting a second inside it would put a flex container in a flex
  // container for nothing.
  //
  // THE HEADER CARRIES ONE ACTION, whatever the tool is made of. The
  // per-surface passes used to sit here too, which made Atlassian the only
  // card in the list with four controls in its header ("Manage", "Set up
  // Jira", "Set up Confluence", "Recheck") purely because it is the only tool
  // with more than one provisionable surface. A reader cannot tell that from
  // looking, so the row simply read as inconsistent. They belong beside the
  // surface they act on, in the body, which is where the console puts a
  // per-item control as well.
  const actions = (
    <>
      {/* No tag at all for a tool nobody has connected: the Connect button
          beside it already says everything true about it, and a grey chip on
          every unconfigured row turned a catalogue into a list of
          complaints. */}
      {state.tag !== "" && (
        <Badge tone={state.tone} outline={state.outline}>
          {state.tag}
        </Badge>
      )}
      {action && onConnect && (
        <Button
          size="sm"
          variant={action.label === "Manage" ? "ghost" : "primary"}
          onClick={() => onConnect(action.blocks)}
          disabled={running}
        >
          {action.label}
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
          {actions}
        </div>
      </div>

      {open && (
        <div className="int-card-body" id={bodyID}>
          {/* ONE LIST, surfaces and seats together. They are two kinds of the
              same thing to a reader (a part of this tool, and whether it
              works), and two lists put an arbitrary seam down the middle of a
              Slack card whose every row is a seat. */}
          <ul className="int-rows">
            {present.map((p) => {
              const tool = tools.find((t) => t.key === p.surface.key);
              return (
                <SurfaceRow
                  key={p.surface.key}
                  surface={p.surface}
                  row={p.row}
                  // Only where this build has a pass behind the button.
                  // Offering it everywhere would give an operator a control
                  // that discovers on a press that there is nothing to do.
                  onPass={
                    onPass && tool && tool.can_provision && tool.satisfied
                      ? (readOnly) => onPass(tool, readOnly)
                      : undefined
                  }
                  running={running}
                />
              );
            })}
            {seats.map((seat) => (
              <li key={seat.handle} className="int-row">
                <div className="int-row-identity">
                  <span className="int-row-name">{seat.name || seat.handle}</span>
                  <span className="int-row-detail">
                    {seat.inbound_path ? (
                      <code className="inline">{seat.inbound_path}</code>
                    ) : (
                      "no inbound path yet"
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
 * Whether this company has set a tool up at all.
 *
 * The sort key, and it reads the SAME rows the card does rather than the
 * setup listing: the listing is guarded, so a reader with no operator token
 * would otherwise see the whole catalogue reorder itself the moment they
 * signed in.
 */
export function isSetUp(entry: Entry, rows: Map<string, IntegrationRow>): boolean {
  return entry.surfaces.some((s) => rows.has(s.key));
}

/** Which catalogue row a surface belongs to, so a running pass disables it. */
function entryOwning(surfaceKey: string): string {
  return CATALOG.find((e) => e.surfaces.some((s) => s.key === surfaceKey))?.key ?? surfaceKey;
}

export function Integrations() {
  // Traffic counters are not pushed, and they move slowly; a minute is the
  // right cadence for "is anything arriving at all".
  const { data, loading, error } = useQuery("integrations", undefined, { pollMs: 60_000 });
  const setup = useSetup();
  const toast = useToast();
  const [dialog, setDialog] = useState<{
    title: string;
    sections: { name: string; tool: SetupToolState }[];
    blocks?: string;
  } | null>(null);
  const [running, setRunning] = useState("");
  const [passing, setPassing] = useState<{ tool: SetupToolState; title: string } | null>(null);
  const [lastRun, setLastRun] = useState<SetupRun | null>(null);

  /**
   * Run a vendor's pass, or check it.
   *
   * The outcome is a toast plus the run's own findings, and the row's state
   * tag updates on its own: the engine wrote the same fleet status the
   * reconcile loop writes, so the next poll of `integrations` carries it.
   */
  async function runPass(
    tool: SetupToolState,
    readOnly: boolean,
    operatorCredential = "",
  ): Promise<void> {
    setRunning(entryOwning(tool.key));
    setLastRun(null);
    try {
      const run = (await rest.post(
        `/setup/integrations/${tool.key}/${readOnly ? "check" : "provision"}`,
        // The credential is sent and never kept: this component drops it
        // with the dialog, and the engine drops it when the pass returns.
        readOnly || !operatorCredential ? {} : { operator_credential: operatorCredential },
      )) as SetupRun;
      setLastRun(run);
      toast.ok(readOnly ? "Checked" : "Setup pass finished");
      setup.reload();
    } catch (err) {
      if (err instanceof RestError) {
        const run = err.body.run as SetupRun | undefined;
        if (run) setLastRun(run);
        toast.failed(err.detail || err.hint || err.code || "The pass was refused.");
      } else {
        toast.failed(String(err));
      }
    } finally {
      setRunning("");
    }
  }

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
          one setting, and a screen that asked for it per vendor would ask the
          operator to keep seven copies consistent. */}
      {setup.base && !setup.base.present && (
        <div className="banner caution">
          <Icon name="alert" size="sm" />
          <span className="col" style={{ gap: 4 }}>
            <span>No public address is set, so no vendor can deliver to this engine.</span>
            <span className="t-caption">
              Set <code className="inline">{setup.base.config_path}</code> to the HTTPS address
              vendors reach this deployment on. Chat over an outbound socket, Mattermost, is
              unaffected.
            </span>
          </span>
        </div>
      )}
      {setup.base?.present && (
        <div className="banner neutral">
          <Icon name="link" size="sm" />
          <span>
            Vendors reach this engine at <code className="inline">{setup.base.value}</code>
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

      {passing && (
        <PassDialog
          tool={passing.tool}
          title={passing.title}
          onClose={() => setPassing(null)}
          onRun={(credential) => {
            const tool = passing.tool;
            setPassing(null);
            void runPass(tool, false, credential);
          }}
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

      {/* WHAT THE LAST PASS ACTUALLY DID. The row's tag says what the
          integration is now; this says what the run reported, which is the
          half an operator needs while fixing something. */}
      {lastRun && (
        <Panel
          title={lastRun.state === "failed" ? "The last pass failed" : "The last pass"}
          icon="activity"
          actions={
            <Button size="sm" variant="ghost" onClick={() => setLastRun(null)}>
              Dismiss
            </Button>
          }
        >
          <div className="col gap-2">
            {lastRun.error && <span className="t-caption">{lastRun.error}</span>}
            {lastRun.report?.detail && <span className="t-caption">{lastRun.report.detail}</span>}
            {(lastRun.findings ?? []).map((f, i) => (
              <span key={`${f.kind}:${f.subject ?? ""}:${i}`} className="t-caption">
                {f.detail || `${f.kind.replace(/_/g, " ")}${f.subject ? `: ${f.subject}` : ""}`}
              </span>
            ))}
            {!lastRun.error && (lastRun.findings ?? []).length === 0 && (
              <span className="t-caption faint">
                The pass found nothing to report, which is what a working integration looks like.
              </span>
            )}
          </div>
        </Panel>
      )}

      {loading && !data && <Skeleton rows={6} />}
      <QueryState error={error} loading={loading} empty={undefined}>
        {/* CONNECTED FIRST, then the rest, in one flat list rather than
            grouped into panels. This is the shape the console's own
            Integrations page has, and the reason is the reading order: an
            operator comes here to finish something, so what is already
            working belongs above the catalogue of what is not, and a
            capability heading over a group of one is chrome around a single
            row. The catalogue's order still runs messaging, tasks, code,
            observability, so related tools stay adjacent without the panel
            around them. */}
        <div className="int-list">
          {[...CATALOG]
            .sort((a, b) => Number(isSetUp(b, rows)) - Number(isSetUp(a, rows)))
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
                onPass={(tool, readOnly) => {
                  // A pass that writes at the vendor is confirmed first, and
                  // a pass that needs an administrator credential collects
                  // it there. A check writes nothing, so it runs on the
                  // press.
                  if (readOnly) {
                    void runPass(tool, true);
                    return;
                  }
                  setPassing({ tool, title: entry.name });
                }}
                running={running === entry.key}
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
