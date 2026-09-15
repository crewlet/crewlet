/**
 * What happened to the revision this tab just saved.
 *
 * A SAVE IS NOT AN APPLY. The configuration surface answers as soon as the
 * revision is stored and activated; each node then applies it on its own
 * reconcile tick, and a node can refuse it (a provider it cannot build, an
 * MCP server it cannot start) and go on serving the previous epoch. Until
 * then the seats, the routing and every read lens still run the revision
 * before this one, and a strip that said "Saved" and stopped would be the
 * dashboard claiming an outcome nobody has had yet.
 *
 * So the strip watches two answers the dashboard already has: the `stream`
 * query's `applied_epoch` for this node, and the `fleet` query for every
 * node's `config_epoch` and `config_status`. It resolves to Applied, to
 * Applied on N of M nodes, or to the refusal with a link to the Fleet screen,
 * and stops polling once it has.
 *
 * It also offers the two things an operator wants right after a save: the
 * diff of what they changed (the saved revision against its parent, never
 * against the active revision, which the save now is), and the company as
 * YAML, which is what keeps a `company.yaml` in a repository in step with what
 * the dashboard wrote.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { href } from "~/app/router.tsx";
import { plural } from "~/lib/format.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { rest, RestError, type EngineHealth, type FleetAnswer } from "~/protocol/index.ts";
import { Button, ButtonLink, Code, CopyButton } from "~/ui/primitives.tsx";
import { useRecheck } from "~/routes/recheck.ts";
import { revisionOfEtag } from "./model/transport.ts";
import { useSavedRevision, type SavedRevision } from "./savedRevision.ts";
import {
  CheckGlyph,
  CloseGlyph,
  DescriptionGlyph,
  ErrorGlyph,
  RefreshGlyph,
} from "@crewlethq/icons/glyphs";
import { Callout, Modal, Skeleton } from "@crewlethq/ui";

/**
 * How often the two answers are read while the apply is still moving. The
 * Integrations screen settles on the same cadence after its own writes: fast
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

/** What the strip says about a saved revision, from the two live answers. */
export function applyState(
  saved: SavedRevision,
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

/** The first characters of a revision id, as the Configuration screen shows one. */
export const shortRevision = (id: string) => id.slice(0, 10);

export function AfterSaveStrip({
  saved,
  onDismiss,
}: {
  saved: SavedRevision;
  onDismiss: () => void;
}) {
  const [yaml, setYaml] = useState(false);
  const [pollMs, setPollMs] = useState<number | undefined>(APPLYING_POLL_MS);
  const stream = useQuery("stream", undefined, { pollMs });
  const fleet = useQuery("fleet", undefined, { pollMs });
  const state = useMemo(
    () => applyState(saved, stream.data, fleet.data),
    [saved, stream.data, fleet.data],
  );

  // Both answers, read through a ref so the recheck window is armed once per
  // saved revision rather than on every render.
  const both = useRef<() => void>(() => {});
  both.current = () => {
    stream.refetch();
    fleet.refetch();
  };
  const read = useCallback(() => both.current(), []);
  const { watching, watch } = useRecheck(read);
  useEffect(() => watch(), [saved.revisionId, watch]);
  useEffect(() => {
    setPollMs(state.resolved ? undefined : watching ? APPLYING_POLL_MS : RECONCILE_POLL_MS);
  }, [state.resolved, watching]);

  return (
    <>
      <Callout
        variant={state.tone === "critical" ? "danger" : "info"}
        icon={
          state.tone === "positive" ? (
            <CheckGlyph />
          ) : state.tone === "critical" ? (
            <ErrorGlyph />
          ) : (
            <RefreshGlyph />
          )
        }
        action={
          <span className="row gap-1 wrap">
            {saved.parentRevisionId ? (
              <ButtonLink
                size="sm"
                variant="ghost"
                href={href(["config"], {
                  lens: "diff",
                  revision: saved.revisionId,
                  against: saved.parentRevisionId,
                })}
              >
                View changes
              </ButtonLink>
            ) : (
              // The company's first revision has no parent to differ from:
              // all of it is what the save wrote.
              <ButtonLink size="sm" variant="ghost" href={href(["config"])}>
                View the configuration
              </ButtonLink>
            )}
            <Button size="sm" variant="ghost" onClick={() => setYaml(true)}>
              Copy as YAML
            </Button>
            {state.showFleet && (
              <ButtonLink size="sm" variant="ghost" href={href(["fleet"])}>
                Open the fleet
              </ButtonLink>
            )}
            <Button
              size="sm"
              variant="ghost"
              icon={CloseGlyph}
              title="Dismiss"
              onClick={onDismiss}
            />
          </span>
        }
      >
        Saved revision <code className="inline">{shortRevision(saved.revisionId)}</code>.{" "}
        <span>{state.message}</span>
      </Callout>
      {yaml && <YamlDialog savedRevision={saved.revisionId} onClose={() => setYaml(false)} />}
    </>
  );
}

/**
 * What a read lens says while this node is still applying the revision this
 * tab saved: the org projection it draws from is the previous one.
 *
 * On the Org screen's read lenses rather than in the Builder, because that is
 * where an operator goes to look at what they just changed, and the chart
 * that has not moved yet is exactly what would otherwise read as a save that
 * did nothing.
 */
export function PreviousRevisionNote() {
  const saved = useSavedRevision();
  const waiting = saved !== null && saved.epoch !== null;
  // ON THE RECONCILE CADENCE, not the strip's quick one. This note lives for
  // as long as the node has not applied the revision, which can be for ever
  // when the node refuses it, and a node's applied epoch cannot move faster
  // than `configplane.ReconcileInterval` anyway. The Shell already reads
  // `stream` on the same interval; a second, faster poller of the same
  // answer would be paid for the life of the tab.
  const stream = useQuery("stream", undefined, {
    enabled: waiting,
    pollMs: waiting ? RECONCILE_POLL_MS : undefined,
  });
  if (!saved || saved.epoch === null) return null;
  if ((stream.data?.applied_epoch ?? 0) >= saved.epoch) return null;
  return (
    <Callout variant="neutral" icon={<RefreshGlyph />}>
      This node is still applying revision{" "}
      <code className="inline">{shortRevision(saved.revisionId)}</code>, so what is drawn below is
      the revision before it.
    </Callout>
  );
}

/**
 * The active company as YAML, to copy into a `company.yaml` kept in a
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
      title="The company as YAML"
      icon={<DescriptionGlyph />}
      size="lg"
      onClose={onClose}
      footer={
        <>
          {state.kind === "text" && (
            <CopyButton text={state.text} label="Copy" title="the company as YAML" />
          )}
          <span className="spacer" />
          <Button onClick={onClose}>Close</Button>
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
              This is revision <code className="inline">{shortRevision(state.revision)}</code>,
              which is active now, rather than the one you saved.
            </Callout>
          )}
          <p className="t-caption">
            Credentials are redacted, as every configuration read is: a value the engine holds reads
            as <code className="inline">__redacted__</code>, and a reference keeps its{" "}
            <code className="inline">${"{NAME}"}</code> form.
          </p>
          <Code selectable label="The company as YAML">
            {state.text}
          </Code>
        </>
      )}
    </Modal>
  );
}
