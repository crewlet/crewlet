/**
 * What happened to the revision this tab just saved.
 *
 * A SAVE IS NOT AN APPLY. The configuration surface answers as soon as the
 * revision is stored and activated; each node then applies it on its own
 * reconcile tick, and a node can refuse it (a provider it cannot build, an
 * MCP server it cannot start) and go on serving the previous epoch. Until
 * then the seats, the routing and every read screen still run the revision
 * before this one, and a strip that said "Saved" and stopped would be the
 * dashboard claiming an outcome nobody has had yet.
 *
 * So the strip watches two things the dashboard already has: the health
 * push's `applied_epoch` and `config_attempt` for this node, and the `fleet`
 * query for the same of every node — the epoch it applied, the one it last
 * attempted with how that went — and the revision it runs, both judged by one
 * rule (`lib/nodeStanding.ts`). It resolves to Applied, to Applied on N of M
 * nodes, to the refusal with a link to Settings › Nodes, or — once a later
 * activation has passed the save before the nodes applied it — to the
 * revision activated since, and stops polling once it has.
 *
 * It also offers the two things an operator wants right after a save: the
 * diff of what they changed (the saved revision against its parent, never
 * against the active revision, which the save now is), and the company as
 * YAML, which is what keeps a `company.yaml` in a repository in step with what
 * the dashboard wrote. Both read the company document, so a lead who saved
 * their unit without Admin is offered neither — and is asked nothing of the
 * fleet, which is Admin's too, so the strip follows the save by this node's
 * own health push alone: what it applied, and what its last attempt made of
 * the revision — so a refusal here is said as one rather than as an apply
 * still running — and says that it cannot follow one whose answer was lost
 * until this node has attempted the revision it saved.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { href } from "~/app/router.tsx";
import { plural } from "~/lib/format.ts";
import { fleetNodeStanding, nodeStanding, type ActivatedRevision } from "~/lib/nodeStanding.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useLastHealth } from "~/lib/useLastHealth.ts";
import { useViewer } from "~/lib/viewer.ts";
import {
  rest,
  RestError,
  type FleetActivation,
  type FleetAnswer,
  type FleetNode,
} from "~/protocol/index.ts";
import type { EngineHealth } from "~/contract/health.ts";
import { useRecheck } from "~/routes/settings/recheck.ts";
import { revisionOfEtag } from "./model/transport.ts";
import { pinSavedEpoch, useSavedRevision, type SavedRevision } from "./savedRevision.ts";
import { screenPath } from "./dialogParts.tsx";
import {
  CheckGlyph,
  XGlyph,
  FileTextGlyph,
  CircleAlertGlyph,
  InfoGlyph,
  RotateCwGlyph,
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

/**
 * How often the fleet is read while the apply is still moving. This node's own
 * epoch is not read at all: it arrives on the health push every five seconds. The
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
  /**
   * `info` is an apply still moving, and only that: it is drawn with the
   * applying glyph. A save a later activation has passed, and one this reader
   * has no way to follow, are `neutral` — settled, and neither applied nor
   * refused.
   */
  readonly tone: "info" | "neutral" | "positive" | "critical";
  readonly message: string;
  /** True once nothing more is expected to change. */
  readonly resolved: boolean;
  /** Settings › Nodes answers what a refusal was about. */
  readonly showFleet: boolean;
}

/** The fleet's nodes by where each stands on the saved revision ([byStanding]). */
interface FleetStandings {
  readonly refused: readonly FleetNode[];
  readonly applied: readonly FleetNode[];
  /** The revision each node that passed the save attempted instead. */
  readonly passedBy: readonly string[];
}

/**
 * The fleet's nodes sorted by where each stands on the saved revision —
 * judged by `fleetNodeStanding`, which is [nodeStanding]'s rule over a node's
 * fleet row, so an Admin is told what a lead is told of the same node (but
 * for the one fact only a row carries, the revision a node runs:
 * `lib/nodeStanding.ts` says where that parts them).
 *
 * Judged by a rule of its own, the fleet path could not see the epoch a node
 * APPLIED, only the one it last attempted: counting a later epoch applied
 * whatever its status, a fleet in which every node had skipped the save and
 * refused the next one was told "Applied." while none ran either; counting
 * only a later epoch that went ok, a node that applied a later epoch and then
 * refused a still later one read as passed where its own push read applied,
 * and a reconnect that read the fleet again took an Admin's "Applied." back.
 *
 * A node not in any list has not reported on this save yet.
 */
function byStanding(fleet: FleetAnswer, write: ActivatedRevision): FleetStandings {
  const refused: FleetNode[] = [];
  const applied: FleetNode[] = [];
  const passedBy: string[] = [];
  for (const n of fleet.nodes) {
    const standing = fleetNodeStanding(n, write);
    if (standing.kind === "refused") refused.push(n);
    else if (standing.kind === "applied") applied.push(n);
    else if (standing.kind === "passed") passedBy.push(standing.by);
  }
  return { refused, applied, passedBy };
}

/** The first characters of a revision id, as Settings › Configuration shows one. */
export const shortRevision = (id: string) => id.slice(0, 10);

/**
 * A save a later activation has passed: nothing more happens to it, so the
 * strip settles. Said as activated SINCE, never as replacing the save: the
 * later revision is as often one built on it, carrying its changes — the
 * history walk finds a lost write beneath exactly such a revision — and
 * "replaced" read as a save that was lost.
 */
const activatedSince = (by: string): ApplyState => ({
  tone: "neutral",
  message: `Revision ${shortRevision(by)} has been activated since.`,
  resolved: true,
  showFleet: false,
});

/** A save no answer concludes yet: stored and activated, and nothing more known. */
const applying: ApplyState = {
  tone: "info",
  message: "The engine is applying it.",
  resolved: false,
  showFleet: false,
};

/** Which epoch the strip follows the saved revision at, as far as the answers say. */
type Followed =
  | { readonly kind: "epoch"; readonly epoch: number }
  /** Another revision has been activated since the save, passing it. */
  | { readonly kind: "passed"; readonly by: FleetActivation }
  /** Nothing says yet which epoch the save was activated at. */
  | { readonly kind: "unknown" }
  /** Nothing this reader may ask ever will. */
  | { readonly kind: "unfollowable" };

/**
 * The epoch the saved revision was activated at.
 *
 * A save's own answer carries it. A save whose answer was lost, and that was
 * found afterwards in the revision history, carries none, and the fleet's
 * activation supplies it ONLY WHEN IT NAMES THIS REVISION. Never from
 * `target_epoch`, which is the newest activation's number whatever revision
 * that is: the history walk that finds a lost write
 * (`settleUnknownWrite`) finds it from a later revision as readily as from
 * itself, and a later revision's epoch read as this save's had every node
 * that converged on that one count as neither applied nor refused, the strip
 * applying until some further activation. Nor is 0 an epoch: it is the
 * answer's word for no activation, or a pointer the engine could not read,
 * and compared against, every node that had reported anything counted as
 * past it and the strip said Applied with nothing behind it. So with no
 * activation the epoch stays unknown and the strip keeps asking.
 *
 * An activation naming ANOTHER revision was made since this one: the answer
 * was asked by a strip mounted for this save, so after it (see
 * [AfterSaveStrip]), and the pointer names the saved revision from the save
 * until something else is activated. Nothing more happens to this save, so
 * the strip settles; what the nodes make of the later revision is Settings ›
 * Nodes' question, not this strip's.
 *
 * A READER WHO MAY NOT READ THE FLEET has no activation to take the epoch
 * from: the fleet is Admin's. What a lead reads that names a revision beside
 * an epoch is this node's last apply attempt, on its health push, so an
 * attempt at the saved revision supplies the epoch. Until then the strip
 * says that it cannot follow the save, and settles: left saying the engine is
 * applying it, it said so for good, since no answer is asked for and this
 * node's applied epoch moving says nothing about a save whose own epoch is
 * unknown. The push keeps arriving, so the strip follows the save once this
 * node attempts it.
 *
 * AN EPOCH LEARNED IS KEPT ([learnedEpoch], `pinSavedEpoch`). Both answers
 * that supply one name the saved revision only until something else is
 * activated, so from then on this reads a save the answers once followed
 * from its first branch, at the epoch it was activated at.
 */
function followed(
  saved: SavedRevision,
  fleet: FleetAnswer | null,
  readsFleet: boolean,
  health: EngineHealth | null,
): Followed {
  if (saved.epoch !== null) return { kind: "epoch", epoch: saved.epoch };
  if (!readsFleet) {
    const attempt = health?.config_attempt;
    return attempt?.revision_id === saved.revisionId
      ? { kind: "epoch", epoch: attempt.epoch }
      : { kind: "unfollowable" };
  }
  const activation = fleet?.activation;
  if (!activation) return { kind: "unknown" };
  if (activation.revision_id === saved.revisionId) {
    return { kind: "epoch", epoch: activation.epoch };
  }
  return { kind: "passed", by: activation };
}

/**
 * The epoch an answer has just supplied for a save that carries none, to be
 * kept for it (`pinSavedEpoch`); null when the save has one, or nothing has
 * named it yet.
 *
 * Re-derived on every push instead, the epoch lasted only as long as the
 * answers went on naming the saved revision: a colleague's next activation
 * moved this node's attempt and the fleet's activation on to theirs, and an
 * applied save became one that "can't be followed from here", a refusal
 * became "activated since", and the chart's note — which follows a save
 * only by its epoch — never followed it at all.
 */
export function learnedEpoch(
  saved: SavedRevision,
  health: EngineHealth | null,
  fleet: FleetAnswer | null,
  readsFleet: boolean,
): number | null {
  if (saved.epoch !== null) return null;
  const at = followed(saved, fleet, readsFleet, health);
  return at.kind === "epoch" ? at.epoch : null;
}

/**
 * What the strip says about a saved revision, from the two answers: this
 * node's last health push (`useLastHealth`, null before the first) and the
 * fleet's.
 *
 * `readsFleet` is whether this reader is asked the fleet at all: `fleet` is
 * null both while its answer is out and for a reader it is never asked for,
 * and only the first is worth waiting on.
 */
export function applyState(
  saved: SavedRevision,
  health: EngineHealth | null,
  fleet: FleetAnswer | null,
  readsFleet: boolean,
): ApplyState {
  const at = followed(saved, fleet, readsFleet, health);
  if (at.kind === "unknown") return applying;
  if (at.kind === "unfollowable") {
    return {
      tone: "neutral",
      message:
        "Whether it has been applied can’t be followed from here. The org chart shows its changes once this node applies them.",
      resolved: true,
      showFleet: false,
    };
  }
  if (at.kind === "passed") return activatedSince(at.by.revision_id);
  const write = { revisionId: saved.revisionId, epoch: at.epoch };
  if (fleet && fleet.nodes.length > 0) {
    // A REFUSAL FIRST: one node running the revision before is what an
    // operator has to hear, however many others applied it.
    const { refused, applied, passedBy } = byStanding(fleet, write);
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
    // APPLIED ONLY ON EVIDENCE, and a node that went past the save without
    // running it settles the strip as passed rather than counting towards
    // Applied. The strip follows ONE revision, so a node still converging on
    // a newer one has nothing left to say about it; only a node yet to
    // report on this save keeps the strip asking.
    if (applied.length === fleet.nodes.length) {
      return { tone: "positive", message: "Applied.", resolved: true, showFleet: false };
    }
    if (applied.length + passedBy.length === fleet.nodes.length) {
      return activatedSince(passedBy[0] || fleet.activation?.revision_id || "");
    }
    if (applied.length > 0) {
      return {
        tone: "info",
        message: `Applied on ${applied.length} of ${fleet.nodes.length} nodes.`,
        resolved: false,
        showFleet: true,
      };
    }
    return applying;
  }
  // NOTHING HEARD FROM THIS NODE YET leaves the save where it was made:
  // applying. Never a standing read off a missing push (`useLastHealth`).
  if (health === null) return applying;
  const here = nodeStanding(health, write);
  if (here.kind === "applied") {
    return { tone: "positive", message: "Applied.", resolved: true, showFleet: false };
  }
  if (here.kind === "refused") {
    return {
      tone: "critical",
      message: "This node refused this revision and goes on running the one before.",
      resolved: true,
      showFleet: readsFleet,
    };
  }
  if (here.kind === "passed") return activatedSince(here.by);
  return applying;
}

/**
 * The strip for the revision this tab saved last.
 *
 * ONE MOUNT PER SAVED REVISION, so every fleet answer it reads was asked
 * after that save. Kept mounted across saves, the strip went on reading the
 * answer it held for the previous one until its next read came back, and a
 * save without an epoch of its own ([followed]) takes the activation in that
 * answer at its word: the previous save would be reported as activated since
 * this one, and the strip would stop polling on it.
 */
export function AfterSaveStrip(props: { saved: SavedRevision; onDismiss: () => void }) {
  return <SavedRevisionStrip key={props.saved.revisionId} {...props} />;
}

function SavedRevisionStrip({ saved, onDismiss }: { saved: SavedRevision; onDismiss: () => void }) {
  const [yaml, setYaml] = useState(false);
  const viewer = useViewer();
  const readsCompany = viewer.admin;
  // A READER NOT KNOWN YET is waited on as an answer still out is. Read as
  // one who may not ask the fleet, an Admin whose viewer answer had not
  // arrived was told a save without an epoch could not be followed, until it
  // did.
  const readsFleet = readsCompany || viewer.loading;
  const [pollMs, setPollMs] = useState<number | undefined>(APPLYING_POLL_MS);
  // THIS NODE'S LAST PUSH, kept through a dropped socket, which says nothing
  // about the save: read live, a lead's "Applied." went back to applying for
  // as long as the socket was down. Kept by the tab, not this mount, since
  // the strip mounts again on every return to the Builder, and its fleet
  // answer does not survive that: a strip mounted during a drop had neither,
  // and took the "Applied." back too.
  const health = useLastHealth();
  const fleet = useQuery("fleet", undefined, { pollMs, enabled: readsCompany });
  const state = useMemo(
    () => applyState(saved, health, fleet.data, readsFleet),
    [saved, health, fleet.data, readsFleet],
  );
  // AN EPOCH AN ANSWER SUPPLIED IS KEPT for the save ([learnedEpoch]), and
  // the store hands the strip the save with it on the next render.
  const learned = learnedEpoch(saved, health, fleet.data, readsFleet);
  useEffect(() => {
    if (learned !== null) pinSavedEpoch(saved.revisionId, learned);
  }, [saved.revisionId, learned]);

  // The fleet's answer, read through a ref so the recheck window is armed once
  // per saved revision rather than on every render. This node's epoch needs no
  // re-read: the health push brings it.
  const refetch = useRef<() => void>(() => {});
  refetch.current = () => fleet.refetch();
  const read = useCallback(() => refetch.current(), []);
  const { watching, watch } = useRecheck(read);
  useEffect(() => watch(), [saved.revisionId, watch]);
  useEffect(() => {
    setPollMs(state.resolved ? undefined : watching ? APPLYING_POLL_MS : RECONCILE_POLL_MS);
  }, [state.resolved, watching]);

  return (
    <>
      <Callout
        variant={
          state.tone === "critical" ? "danger" : state.tone === "neutral" ? "neutral" : "info"
        }
        icon={
          state.tone === "positive" ? (
            <CheckGlyph />
          ) : state.tone === "critical" ? (
            <CircleAlertGlyph />
          ) : state.tone === "neutral" ? (
            <InfoGlyph />
          ) : (
            <RotateCwGlyph />
          )
        }
        action={
          <span className="row gap-1 wrap">
            {!readsCompany ? null : saved.parentRevisionId ? (
              <ButtonLink
                size="small"
                variant="ghost"
                href={href(screenPath("config"), {
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
              <ButtonLink size="small" variant="ghost" href={href(screenPath("config"))}>
                View the configuration
              </ButtonLink>
            )}
            {readsCompany && (
              <Button size="small" variant="ghost" onClick={() => setYaml(true)}>
                Copy as YAML
              </Button>
            )}
            {state.showFleet && (
              <ButtonLink size="small" variant="ghost" href={href(screenPath("nodes"))}>
                Open the fleet
              </ButtonLink>
            )}
            <IconButton label="Dismiss" icon={<XGlyph />} size="sm" onClick={onDismiss} />
          </span>
        }
      >
        Saved revision <InlineCode>{shortRevision(saved.revisionId)}</InlineCode>.{" "}
        <span>{state.message}</span>
      </Callout>
      {yaml && <YamlDialog savedRevision={saved.revisionId} onClose={() => setYaml(false)} />}
    </>
  );
}

/**
 * What a read section says while this node is still applying the revision
 * this tab saved: the org projection it draws from is the previous one.
 *
 * On Agents' org chart and teams rather than in the Builder, because that is
 * where an operator goes to look at what they just changed, and the chart
 * that has not moved yet is exactly what would otherwise read as a save that
 * did nothing.
 */
export function PreviousRevisionNote() {
  const saved = useSavedRevision();
  // THE HEALTH PUSH, which carries this node's applied epoch and its last
  // apply attempt every five seconds to every tab — there is nothing to poll.
  // This note lives for as long as the node has not applied the revision,
  // which is for ever when the node refuses it, so a poller of its own would
  // be paid for the life of the tab — and the note says so then, rather than
  // that the node is still applying it. Its LAST push (`useLastHealth`): a
  // dropped socket is no news of the save, and read live it said "Still
  // applying" of a save this node had applied, or refused. The tab's, not
  // this mount's: the note mounts afresh on the chart, Teams and a unit's
  // page, and one mounted during a drop said the same.
  const health = useLastHealth();
  // A SAVE WITHOUT AN EPOCH learns it here as the strip does, from this
  // node's attempt at the saved revision — the note reads no fleet — so the
  // chart follows a save whose answer was lost although the Builder, and its
  // strip, are gone.
  const learned = saved ? learnedEpoch(saved, health, null, false) : null;
  useEffect(() => {
    if (saved && learned !== null) pinSavedEpoch(saved.revisionId, learned);
  }, [saved, learned]);
  if (!saved || saved.epoch === null) return null;
  // Nothing heard from this node yet: the save is where it was made.
  const here =
    health === null
      ? { kind: "applying" as const }
      : nodeStanding(health, { revisionId: saved.revisionId, epoch: saved.epoch });
  if (here.kind === "applied") return null;
  const revision = <InlineCode>{shortRevision(saved.revisionId)}</InlineCode>;
  if (here.kind === "applying") {
    return (
      <Callout variant="neutral" icon={<RotateCwGlyph />}>
        Still applying {revision}. Showing the previous revision.
      </Callout>
    );
  }
  return (
    <Callout variant="neutral" icon={<InfoGlyph />}>
      {here.kind === "refused" ? (
        <>This node refused {revision}.</>
      ) : (
        <>This node did not apply {revision}.</>
      )}{" "}
      Showing the revision it runs.
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
      icon={<FileTextGlyph />}
      size="lg"
      onClose={onClose}
      footer={
        <>
          {state.kind === "text" && (
            <CopyButton
              variant="secondary"
              text={state.text}
              label="Copy"
              title="the company as YAML"
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
              Showing active revision{" "}
              <InlineCode tone="inherit">{shortRevision(state.revision)}</InlineCode>, not the one
              you saved.
            </Callout>
          )}
          <p className="t-caption">Credentials are hidden.</p>
          {/* ITS OWN CEILING, the one block that states one: a dialog is
              portaled to the document, outside the shell's scrollers, so the
              view-derived ceiling they declare (`--record-ceiling`,
              frame.css) never reaches it. The root's cap does. */}
          <CodeBlock
            plain
            wrap
            selectable
            label="The company as YAML"
            code={state.text}
            maxHeight="var(--record-max)"
          />
        </>
      )}
    </Modal>
  );
}
