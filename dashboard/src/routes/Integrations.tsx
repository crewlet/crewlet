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
import { useQuery } from "~/lib/useQuery.ts";
import { fmtDateTime } from "~/lib/format.ts";
import { SetupDialog } from "./SetupDialog.tsx";
import { rest, RestError } from "~/protocol/index.ts";
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
  /** The word in the tag: a reconcile phase, or configured / paused / not configured. */
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
    return { tag: "not configured", tone: "neutral", outline: true, attention: false };
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
      tag: phase.replace(/_/g, " "),
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
    return { tag: "paused", tone: "neutral", outline: true, attention: false };
  }
  // No measured claim at all. The config's own word, and the ingress fault if
  // there is one, which is the only thing that can be said without a pass.
  return {
    tag: "configured",
    tone: "neutral",
    outline: false,
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
export function Reconcile({ status }: { status: ReconcileStatus | null | undefined }) {
  if (!status) return null;

  const actor = actorLabel(status.actor);
  // The findings the phase was NOT derived from. The report says what to do
  // next and the findings say what is actually wrong, so an operator who
  // fixes the first should not wait a full pass to learn there was a second.
  const rest = (status.findings ?? []).slice(1);

  return (
    <div className="col gap-1">
      {actor && <span className="t-caption faint">{actor}</span>}

      {status.detail && <span className="t-caption">{status.detail}</span>}

      {status.action_url && (
        <a className="t-caption" href={status.action_url} target="_blank" rel="noreferrer">
          Open where this is fixed
        </a>
      )}

      {status.last_error && (
        <span className="t-caption faint" title="the last pass could not read this surface">
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
              <li key={`${f.kind}:${f.subject ?? ""}:${i}`} className="t-caption">
                {f.detail || `${f.kind.replace(/_/g, " ")}${f.subject ? `: ${f.subject}` : ""}`}
              </li>
            ))}
          </ul>
        </details>
      )}

      <span className="t-caption faint">
        {status.settled_at ? `settled ${fmtDateTime(status.settled_at)}` : "not settled yet"}
        {status.next_attempt_at ? ` · next check ${fmtDateTime(status.next_attempt_at)}` : ""}
      </span>
    </div>
  );
}

/**
 * One surface inside a tool's details: its own phase, what arrived and what
 * became of it, where it listens, and what the loop found.
 */
function SurfaceDetail({ surface, row }: { surface: Surface; row: IntegrationRow }) {
  return (
    <div className="int-surface">
      <div className="row wrap gap-2 int-surface-head">
        <span className="int-surface-name">{surface.name}</span>
        {row.reconcile ? (
          <Badge tone={phaseTone(row.reconcile.phase)} dot>
            {row.reconcile.phase.replace(/_/g, " ")}
          </Badge>
        ) : row.enabled === false ? (
          <Badge outline>paused</Badge>
        ) : null}
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
      </div>
      {row.detail && <span className="t-caption faint">{row.detail}</span>}
      <div className="row wrap gap-3">
        {/* The three counts GET /integrations answers with. Read together:
            "128 arrived" alone cannot tell a working surface from one whose
            every delivery reaches nobody. */}
        <Count value={row.inbound} label="in" />
        <Count value={row.skipped} label="dropped" />
        <Count value={row.coalesced} label="merged" />
        {typeof row.inbound_path === "string" && row.inbound_path && (
          <code className="inline t-caption faint">{String(row.inbound_path)}</code>
        )}
      </div>
      <Reconcile status={row.reconcile} />
    </div>
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
  setup: SetupToolState | undefined,
): { label: string; blocks?: string } | null {
  if (!setup) return null;
  if (!setup.configured) return { label: "Connect" };
  if (!setup.satisfied) return { label: "Continue" };
  if (state.attention) {
    // Narrowed to the fields that clear what the loop actually found, so a
    // Fix opens the two inputs that matter rather than the whole form.
    return { label: "Fix", blocks: "credential_missing" };
  }
  return { label: "Manage" };
}

export function EntryRow({
  entry,
  rows,
  setup,
  revision,
  onConnect,
}: {
  entry: Entry;
  rows: Map<string, IntegrationRow>;
  setup?: SetupToolState;
  revision?: string;
  onConnect?: (tool: SetupToolState, blocks?: string) => void;
}) {
  const state = rollUp(entry, rows);
  const present = presentSurfaces(entry, rows);
  const absent = present.length === 0;
  const action = actionFor(state, setup);

  return (
    <div className={absent ? "list-row int-row int-row-absent" : "list-row int-row"}>
      <span className="int-row-icon" aria-hidden>
        <VendorMark vendor={entry.vendor} />
      </span>
      <div className="int-row-text">
        <span className="int-row-name">{entry.name}</span>
        <span className="int-row-desc">{entry.description}</span>
        {state.status && (
          <span className={state.attention ? "int-row-status attention" : "int-row-status"}>
            {state.attention && <Icon name="alert" size="sm" />}
            {state.status}
          </span>
        )}
        {!absent && (
          <details className="int-details">
            <summary className="int-summary">Details</summary>
            <div className="col gap-3 int-details-body">
              {present.map((p) => (
                <SurfaceDetail key={p.surface.key} surface={p.surface} row={p.row} />
              ))}
            </div>
          </details>
        )}
      </div>
      <div className="int-row-state">
        <Badge tone={state.tone} outline={state.outline} dot={!state.outline}>
          {state.tag}
        </Badge>
        {action && setup && onConnect && (
          <Button
            size="sm"
            variant={action.label === "Manage" ? "ghost" : "primary"}
            onClick={() => onConnect(setup, action.blocks)}
          >
            {action.label}
          </Button>
        )}
      </div>
    </div>
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

  return {
    byKey: new Map((listing?.tools ?? []).map((t) => [t.key, t])),
    base: listing?.public_base_url ?? null,
    guarded,
    reload,
  };
}

export function Integrations() {
  // Traffic counters are not pushed, and they move slowly; a minute is the
  // right cadence for "is anything arriving at all".
  const { data, loading, error } = useQuery("integrations", undefined, { pollMs: 60_000 });
  const setup = useSetup();
  const [dialog, setDialog] = useState<{ tool: SetupToolState; blocks?: string } | null>(null);

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
        </div>
      )}

      {dialog && (
        <SetupDialog
          tool={dialog.tool}
          title={CATALOG.find((e) => e.key === dialog.tool.key)?.name ?? dialog.tool.key}
          blocks={dialog.blocks}
          onClose={() => setDialog(null)}
          onDone={setup.reload}
        />
      )}

      {loading && !data && <Skeleton rows={6} />}
      <QueryState error={error} loading={loading} empty={undefined}>
        {CAPABILITIES.map((cap) => (
          <Panel
            key={cap.id}
            title={cap.title}
            subtitle={cap.description}
            icon={cap.icon}
            padding="none"
          >
            <div className="list">
              {CATALOG.filter((e) => e.capability === cap.id).map((entry) => (
                <EntryRow
                  key={entry.key}
                  entry={entry}
                  rows={rows}
                  setup={setup.byKey.get(entry.key)}
                  onConnect={(tool, blocks) => setDialog({ tool, blocks })}
                />
              ))}
            </div>
          </Panel>
        ))}
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
