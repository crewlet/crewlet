/**
 * What happened to what this tab just saved: a settings revision, the org
 * chart's writes, or both.
 *
 * A SAVE IS NOT AN APPLY. The settings surface answers as soon as the revision
 * is stored and activated; each node then applies it on its own reconcile
 * tick, and a node can refuse it (a provider it cannot build, an MCP server it
 * cannot start) and go on serving the previous epoch. The chart answers once
 * its records are durable, and each node applies them from the log in its own
 * time. Until then the seats, the routing and every read lens still run the
 * company before this save, and a strip that said "Saved" and stopped would be
 * the dashboard claiming an outcome nobody has had yet.
 *
 * So the strip follows each half on the answers that say where it stands. The
 * SETTINGS on the `stream` query's `applied_epoch` for this node and the
 * `fleet` query's `config_epoch` and `config_status` for every node, resolving
 * to Applied, to Applied on N of M nodes, or to the refusal with a link to the
 * Fleet screen. The CHART on the `retention` report's per-node applied
 * position for the chart's log, which only a reader who may operate the fleet
 * is shown — for anybody else it says what this node's own answer said.
 *
 * It also offers what an operator wants right after a save: the diff of the
 * settings they changed (the saved revision against its parent, never against
 * the active revision, which the save now is), the settings as YAML, and the
 * chart as the document `crewlet chart export` writes — which, together, are
 * what keeps a company file in a repository in step with what the dashboard
 * wrote.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { href } from "~/app/router.tsx";
import { plural } from "~/lib/format.ts";
import { useEngineHealth } from "~/lib/engineHealth.ts";
import { useQuery } from "~/lib/useQuery.ts";
import {
  rest,
  RestError,
  type EngineHealth,
  type FleetAnswer,
  type RetentionReport,
} from "~/protocol/index.ts";
import { useRecheck } from "~/routes/admin/recheck.ts";
import { parsePosition } from "./model/save.ts";
import { revisionOfEtag } from "./model/transport.ts";
import {
  useSavedChanges,
  type SavedChanges,
  type SavedChart,
  type SavedSettings,
} from "./savedChanges.ts";
import { screenPath } from "./dialogParts.tsx";
import {
  CheckGlyph,
  CloseGlyph,
  DescriptionGlyph,
  ErrorGlyph,
  RefreshGlyph,
} from "@crewlethq/icons/glyphs";
import {
  Button,
  ButtonLink,
  Callout,
  CodeBlock,
  CopyButton,
  IconButton,
  InlineCode,
  Modal,
  Skeleton,
} from "@crewlethq/ui";
import { RECORD_MAX_HEIGHT } from "~/components/common.tsx";

/**
 * How often the fleet and retention answers are read while the apply is still
 * moving — the engine's own health is the shared read, at its own five
 * seconds (`lib/engineHealth.ts`). The Integrations screen settles on the
 * same cadence after its own writes: fast
 * enough that a node applying in a second or two is seen at once, slow enough
 * not to poll an engine that is busy applying.
 */
const APPLYING_POLL_MS = 4_000;

/**
 * The cadence once the quick window is over and the apply has still not
 * finished. `configplane.ReconcileInterval` is fifteen seconds, so nothing
 * can change faster than that from here on.
 */
const RECONCILE_POLL_MS = 15_000;

export interface ApplyState {
  readonly tone: "info" | "positive" | "critical";
  readonly message: string;
  /** True once nothing more is expected to change. */
  readonly resolved: boolean;
  /** The Fleet screen answers what a refusal was about. */
  readonly showFleet: boolean;
}

/**
 * A node's own outcome counts only for the epoch it reported it for.
 *
 * A node records the epoch it ATTEMPTED beside the outcome
 * (`engine.Reconciler.record`), so a failure names the revision it failed on.
 * Without the epoch match, a node that refused an earlier revision and has
 * since stopped reporting would be read as refusing this one.
 */
function refusedBy(fleet: FleetAnswer, epoch: number) {
  return fleet.nodes.filter(
    (n) =>
      (n.config_epoch ?? 0) === epoch &&
      (n.config_status === "error" || n.config_status === "degraded"),
  );
}

/** What the strip says about a saved settings revision, from the two live answers. */
export function applyState(
  saved: SavedSettings,
  stream: EngineHealth | null,
  fleet: FleetAnswer | null,
): ApplyState {
  // A save whose answer was lost carries no epoch; the fleet's target is the
  // epoch of the newest activation, which is that write's own.
  const epoch = saved.epoch ?? fleet?.target_epoch ?? null;
  if (epoch === null) {
    return {
      tone: "info",
      message: "The engine is applying it.",
      resolved: false,
      showFleet: false,
    };
  }
  if (fleet && fleet.nodes.length > 0) {
    const refused = refusedBy(fleet, epoch);
    if (refused.length > 0) {
      const first = refused[0]!;
      const others =
        refused.length > 1 ? ` ${plural(refused.length - 1, "other node")} refused it too.` : "";
      return {
        tone: "critical",
        message: `Node ${first.id} refused this revision: ${first.config_error || "no reason reported"}.${others}`,
        resolved: true,
        showFleet: true,
      };
    }
    // A NODE THAT HAS REPORTED A LATER EPOCH HAS PASSED THIS ONE, whatever it
    // made of that later revision: this strip follows one revision, and a
    // node still converging on a newer one has nothing left to say about it.
    // A refusal OF THIS EPOCH is the branch above, so nothing is counted
    // applied here that refused the revision the operator saved.
    const applied = fleet.nodes.filter((n) => (n.config_epoch ?? 0) >= epoch);
    if (applied.length === fleet.nodes.length) {
      return { tone: "positive", message: "Applied.", resolved: true, showFleet: false };
    }
    if (applied.length > 0) {
      return {
        tone: "info",
        message: `Applied on ${applied.length} of ${fleet.nodes.length} nodes.`,
        resolved: false,
        showFleet: true,
      };
    }
    return {
      tone: "info",
      message: "The engine is applying it.",
      resolved: false,
      showFleet: false,
    };
  }
  if (stream && (stream.applied_epoch ?? 0) >= epoch) {
    return { tone: "positive", message: "Applied.", resolved: true, showFleet: false };
  }
  return { tone: "info", message: "The engine is applying it.", resolved: false, showFleet: false };
}

/**
 * What the strip says about the chart's writes, from the retention report's
 * per-node applied positions — or, where this reader is not shown it, from
 * what this node's own answer said.
 *
 * A NODE HAS APPLIED THE WRITES once it has applied the log through their
 * position: the same generation of the log at or past its sequence, or a
 * later generation (a log re-created after them carries everything before).
 */
export function chartApplyState(saved: SavedChart, retention: RetentionReport | null): ApplyState {
  const at = parsePosition(saved.position);
  const nodes = (retention?.nodes ?? []).filter((n) => n.live || n.counted);
  if (retention && at && nodes.length > 0) {
    const applied = nodes.filter((n) => {
      const chart = n.domains?.chart;
      return (
        chart !== undefined &&
        (chart.generation > at.generation ||
          (chart.generation === at.generation && chart.applied_through >= at.seq))
      );
    });
    if (applied.length === nodes.length) {
      return { tone: "positive", message: "Applied.", resolved: true, showFleet: false };
    }
    return {
      tone: "info",
      message:
        applied.length > 0
          ? `Applied on ${applied.length} of ${nodes.length} nodes.`
          : "The nodes are applying it.",
      resolved: false,
      showFleet: false,
    };
  }
  // WHERE THIS READER IS NOT SHOWN THE FLEET, this node's own answer is all
  // there is to say, and it is said as that rather than as the fleet's.
  return saved.appliedHere
    ? { tone: "positive", message: "Applied on this node.", resolved: true, showFleet: false }
    : { tone: "info", message: "This node is applying it.", resolved: false, showFleet: false };
}

/** The first characters of a revision id, as the Configuration screen shows one. */
export const shortRevision = (id: string) => id.slice(0, 10);

/** The tone of the strip: the worst of its halves. */
function toneOf(states: readonly (ApplyState | null)[]): ApplyState["tone"] {
  if (states.some((s) => s?.tone === "critical")) return "critical";
  if (states.some((s) => s?.tone === "info")) return "info";
  return "positive";
}

export function AfterSaveStrip({
  saved,
  onDismiss,
}: {
  saved: SavedChanges;
  onDismiss: () => void;
}) {
  const [yaml, setYaml] = useState(false);
  const [exporting, setExporting] = useState(false);
  const [pollMs, setPollMs] = useState<number | undefined>(APPLYING_POLL_MS);
  const settings = saved.settings;
  const chart = saved.chart;
  // THE SHARED HEALTH READ (`lib/engineHealth.ts`), already at five seconds —
  // the rail reads it too, and a second poller of the same answer on this
  // strip's cadence made the two disagree about the applied epoch for the
  // length of the difference. Its `refetch` brings the shared poll forward.
  const stream = useEngineHealth();
  const fleet = useQuery("fleet", undefined, { pollMs, enabled: settings !== null });
  const retention = useQuery("retention", undefined, { pollMs, enabled: chart !== null });
  const settingsState = useMemo(
    () => (settings ? applyState(settings, stream.data, fleet.data) : null),
    [settings, stream.data, fleet.data],
  );
  const chartState = useMemo(
    () => (chart ? chartApplyState(chart, retention.data) : null),
    [chart, retention.data],
  );
  const resolved = (settingsState?.resolved ?? true) && (chartState?.resolved ?? true);

  // Every answer, read through a ref so the recheck window is armed once per
  // save rather than on every render.
  const all = useRef<() => void>(() => {});
  all.current = () => {
    if (settings) {
      stream.refetch();
      fleet.refetch();
    }
    if (chart) retention.refetch();
  };
  const read = useCallback(() => all.current(), []);
  const { watching, watch } = useRecheck(read);
  useEffect(() => watch(), [settings?.revisionId, chart?.position, watch]);
  useEffect(() => {
    setPollMs(resolved ? undefined : watching ? APPLYING_POLL_MS : RECONCILE_POLL_MS);
  }, [resolved, watching]);

  const tone = toneOf([settingsState, chartState]);
  const showFleet = settingsState?.showFleet ?? false;

  return (
    <>
      <Callout
        variant={tone === "critical" ? "danger" : "info"}
        icon={
          tone === "positive" ? (
            <CheckGlyph />
          ) : tone === "critical" ? (
            <ErrorGlyph />
          ) : (
            <RefreshGlyph />
          )
        }
        action={
          <span className="row gap-1 wrap">
            {settings &&
              (settings.parentRevisionId ? (
                <ButtonLink
                  size="small"
                  variant="tertiary"
                  href={href(screenPath("config"), {
                    lens: "diff",
                    revision: settings.revisionId,
                    against: settings.parentRevisionId,
                  })}
                >
                  View changes
                </ButtonLink>
              ) : (
                // The company's first revision has no parent to differ from:
                // all of it is what the save wrote.
                <ButtonLink size="small" variant="tertiary" href={href(screenPath("config"))}>
                  View the configuration
                </ButtonLink>
              ))}
            {settings && (
              <Button size="small" variant="tertiary" onClick={() => setYaml(true)}>
                Copy settings as YAML
              </Button>
            )}
            {chart && (
              <Button size="small" variant="tertiary" onClick={() => setExporting(true)}>
                Copy the chart
              </Button>
            )}
            {showFleet && (
              <ButtonLink size="small" variant="tertiary" href={href(screenPath("fleet"))}>
                Open the fleet
              </ButtonLink>
            )}
            <IconButton label="Dismiss" icon={<CloseGlyph />} size="sm" onClick={onDismiss} />
          </span>
        }
      >
        <span className="col gap-1">
          {settings && settingsState && (
            <span>
              Saved settings revision <InlineCode>{shortRevision(settings.revisionId)}</InlineCode>.{" "}
              {settingsState.message}
            </span>
          )}
          {chart && chartState && (
            <span>
              Saved the org chart at <InlineCode>{chart.position}</InlineCode>. {chartState.message}
            </span>
          )}
        </span>
      </Callout>
      {yaml && settings && (
        <YamlDialog savedRevision={settings.revisionId} onClose={() => setYaml(false)} />
      )}
      {exporting && <ChartExportDialog onClose={() => setExporting(false)} />}
    </>
  );
}

/**
 * What a read lens says while this node has not applied what this tab saved:
 * the org projection it draws from is the company before the save.
 *
 * On the Org screen's read lenses rather than in the Builder, because that is
 * where an operator goes to look at what they just changed, and the chart
 * that has not moved yet is exactly what would otherwise read as a save that
 * did nothing.
 */
export function PreviousRevisionNote() {
  const saved = useSavedChanges();
  const settings = saved?.settings ?? null;
  // THE SHARED HEALTH READ, which the frame is already polling: this note
  // lives for as long as the node has not applied the revision — for ever,
  // when the node refuses it — and a poller of its own, however slow, was a
  // second read of the one answer paid for the life of the tab.
  const stream = useEngineHealth();
  const settingsBehind =
    settings !== null &&
    settings.epoch !== null &&
    (stream.data?.applied_epoch ?? 0) < settings.epoch;
  const chartBehind = saved?.chart ? !saved.chart.appliedHere : false;
  if (!settingsBehind && !chartBehind) return null;
  return (
    <Callout variant="neutral" icon={<RefreshGlyph />}>
      {settingsBehind && settings ? (
        <>
          This node is still applying settings revision{" "}
          <InlineCode>{shortRevision(settings.revisionId)}</InlineCode>
          {chartBehind ? " and the org chart's changes" : ""}, so what is drawn below is the company
          before them.
        </>
      ) : (
        <>
          This node is still applying the org chart's changes, so what is drawn below is the chart
          before them.
        </>
      )}
    </Callout>
  );
}

/**
 * The org chart as the document `crewlet chart export` writes, to copy into a
 * company file kept in a repository. Read on open, like the settings' YAML,
 * for the same reason: a clipboard write that no longer belongs to a gesture
 * is refused, and a copy nobody can see fail is a copy nobody can trust.
 */
function ChartExportDialog({ onClose }: { onClose: () => void }) {
  const [state, setState] = useState<
    { kind: "loading" } | { kind: "text"; text: string } | { kind: "failed"; detail: string }
  >({ kind: "loading" });

  useEffect(() => {
    const controller = new AbortController();
    void rest
      .request("GET", "/company/export", { signal: controller.signal })
      .then((answer) => {
        if (controller.signal.aborted) return;
        setState({ kind: "text", text: `${JSON.stringify(answer.body, null, 2)}\n` });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setState({
          kind: "failed",
          detail:
            err instanceof RestError
              ? err.detail || err.code || `The engine answered with status ${err.status}.`
              : "The engine could not be reached.",
        });
      });
    return () => controller.abort();
  }, []);

  return (
    <Modal
      open
      stackBody
      title="The org chart"
      icon={<DescriptionGlyph />}
      size="lg"
      onClose={onClose}
      footer={
        <>
          {state.kind === "text" && (
            <CopyButton variant="secondary" text={state.text} label="Copy" title="the org chart" />
          )}
          <span className="spacer" />
          <Button variant="secondary" onClick={onClose}>
            Close
          </Button>
        </>
      }
    >
      {state.kind === "loading" && <Skeleton label="Loading the chart" variant="text" rows={4} />}
      {state.kind === "failed" && (
        <Callout variant="danger" role="alert">
          {state.detail}
        </Callout>
      )}
      {state.kind === "text" && (
        <>
          <p className="t-caption">
            Every unit and seat with its runtime half, and whom each seat manages — the document{" "}
            <InlineCode>crewlet chart export</InlineCode> writes. It carries the names of every
            credential the company holds, never their values.
          </p>
          <CodeBlock
            plain
            wrap
            selectable
            label="The org chart"
            code={state.text}
            maxHeight={RECORD_MAX_HEIGHT}
          />
        </>
      )}
    </Modal>
  );
}

/**
 * The active settings as YAML, to copy into a company file kept in a
 * repository. Read on open rather than copied straight to the clipboard: a
 * browser refuses a clipboard write that no longer belongs to a gesture, and
 * a copy nobody can see fail is a copy nobody can trust.
 */
function YamlDialog({ savedRevision, onClose }: { savedRevision: string; onClose: () => void }) {
  const [state, setState] = useState<
    | { kind: "loading" }
    | { kind: "text"; text: string; revision: string | null }
    | { kind: "failed"; detail: string }
  >({ kind: "loading" });

  useEffect(() => {
    const controller = new AbortController();
    void rest
      .request("GET", "/config", {
        query: { format: "yaml" },
        read: "text",
        signal: controller.signal,
      })
      .then((answer) => {
        if (controller.signal.aborted) return;
        setState({
          kind: "text",
          text: typeof answer.body === "string" ? answer.body : "",
          revision: revisionOfEtag(answer.etag),
        });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setState({
          kind: "failed",
          detail:
            err instanceof RestError
              ? err.detail || err.code || `The engine answered with status ${err.status}.`
              : "The engine could not be reached.",
        });
      });
    return () => controller.abort();
  }, []);

  return (
    <Modal
      open
      stackBody
      title="The settings as YAML"
      icon={<DescriptionGlyph />}
      size="lg"
      onClose={onClose}
      footer={
        <>
          {state.kind === "text" && (
            <CopyButton
              variant="secondary"
              text={state.text}
              label="Copy"
              title="the settings as YAML"
            />
          )}
          <span className="spacer" />
          <Button variant="secondary" onClick={onClose}>
            Close
          </Button>
        </>
      }
    >
      {state.kind === "loading" && (
        <Skeleton label="Loading the saved revision" variant="text" rows={4} />
      )}
      {/* It replaces the placeholder AFTER the dialog was read out, so a
          reader who has already heard "Loading the saved revision" hears
          nothing more unless this says so. */}
      {state.kind === "failed" && (
        <Callout variant="danger" role="alert">
          {state.detail}
        </Callout>
      )}
      {state.kind === "text" && (
        <>
          {state.revision !== null && state.revision !== savedRevision && (
            <Callout variant="warning">
              This is revision{" "}
              <InlineCode tone="inherit">{shortRevision(state.revision)}</InlineCode>, which is
              active now, rather than the one you saved.
            </Callout>
          )}
          <p className="t-caption">
            Credentials are redacted, as every configuration read is: a value the engine holds reads
            as <InlineCode>__redacted__</InlineCode>, and a reference keeps its{" "}
            <InlineCode>${"{NAME}"}</InlineCode> form.
          </p>
          <CodeBlock
            plain
            wrap
            selectable
            label="The settings as YAML"
            code={state.text}
            maxHeight={RECORD_MAX_HEIGHT}
          />
        </>
      )}
    </Modal>
  );
}
