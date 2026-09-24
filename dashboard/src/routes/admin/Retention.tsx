/**
 * The state log's retention, on the Fleet screen.
 *
 * # Why it lives here rather than on a screen of its own
 *
 * Fleet's own doc comment says it "is read when nodes are dying, which is
 * exactly when the API answering it is least reliable". *Why is nothing being
 * trimmed* and *which node is behind* are the same investigation, and a
 * separate screen would make an operator hold two polls in their head to
 * answer one question.
 *
 * It obeys that screen's rule too: it says when its own poll failed rather
 * than showing the last reading as if it were now.
 *
 * # What is load-bearing here, and why each one is
 *
 * Every rendering below exists because the alternative reads as a DIFFERENT
 * fact rather than as a missing one, which is the failure mode a dashboard has
 * that a log does not:
 *
 *   - `counted · no position yet` rather than a zero. A node that has never
 *     reported and a node at sequence zero block the trim for very different
 *     lengths of time.
 *   - an em-dash rather than 0 wherever a node published nothing, which is the
 *     "absent is NOT zero" rule `internal/api/queries/fleet.go` already states.
 *   - `applied_through` BESIDE `seq`, because a node applying nothing while its
 *     position advances looks identical to a caught-up one from either number
 *     alone — a deferral rendered as lag is the state nobody diagnoses.
 *   - `n/a` rather than `0` for a term a domain does not have.
 *   - `evicted` carrying its `effective_at` while the fence window is open,
 *     because an operator who cannot see that the exclusion is PENDING runs
 *     the gesture twice.
 *   - the snapshot block's OWN blocked reason: a stalled snapshot tier and a
 *     stalled trim are different problems with different remedies, and the
 *     first is silent until a node tries to join.
 */

import { useEffect, useState } from "react";
import {
  Button,
  Callout,
  Card,
  EmptyValue,
  InlineCode,
  Skeleton,
  StatCard,
  StatGroup,
  Tag,
} from "@crewlethq/ui";
import {
  ContentCopyGlyph,
  DatabaseGlyph,
  DnsGlyph,
  Package2Glyph,
  ScheduleGlyph,
  WarningGlyph,
} from "@crewlethq/icons/glyphs";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { DateCell, KeyCell, StatusCell } from "~/app/frame/cells.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtBytes, fmtDateTime, fmtDuration, relTime } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import { apiToken } from "~/protocol/authToken.ts";
import type {
  RetentionDomain,
  RetentionMaintenance,
  RetentionNode,
  RetentionNodeDomain,
  RetentionReport,
  RetentionSnapshot,
  RetentionTerm,
} from "~/protocol/index.ts";
import { finishable, GateDialog } from "./GateDialog.tsx";
import type { GateGesture } from "./GateDialog.tsx";

/** Sixty seconds: this document is assembled from coordination and three
 *  loops, and none of them moves faster than a tick. */
const POLL_MS = 60_000;

/** The trim's sixth term (D72), and the number an operator has to fix when the
 *  banner names `snapshot_floor`. */
const SNAPSHOT_DONORS_REQUIRED = 2;

export function RetentionPanels({ thisNode }: { thisNode?: string }) {
  const now = useNow();
  // OPERATOR-GATED, so the panel renders only when this browser holds a
  // token — the same condition every other operator surface uses. Asking
  // without one would refuse on every poll and paint the screen red for a
  // reader who simply is not an operator.
  const operator = apiToken() !== "";
  const { data, loading, error, refetch } = useQuery("retention", undefined, {
    pollMs: POLL_MS,
    enabled: operator,
  });
  const [gate, setGate] = useState<{ node: string; evict: boolean } | null>(null);
  // THE GESTURES STILL TO BE FINISHED, keyed by node and sign, held HERE
  // rather than in the dialog: the dialog is unmounted when it closes, and a
  // partial eviction reopened as a fresh one is a second gesture — every log
  // the first one reached is written again and its eviction re-dated.
  //
  // A COMPLETE ONE IS HELD TOO, until the report shows it. The report is
  // polled, and until its next answer the row still read "Evict…" for a node
  // just evicted — reopened, that minted a fresh id and wrote a second record
  // on every log, re-dating the eviction straight after the dialog said not
  // to retry. So an answer asks the report again at once, and the gesture
  // stays on the row until the report agrees with it — or until a report
  // that already includes it disagrees, because something later moved the
  // node ([heldGestures]).
  const [gestures, setGestures] = useState<Record<string, GateGesture>>({});
  const holdGesture = (node: string, evict: boolean, g: GateGesture | null) => {
    setGestures((all) => {
      const next = { ...all };
      if (g) next[gestureKey(node, evict)] = g;
      else delete next[gestureKey(node, evict)];
      return next;
    });
    if (g?.answer) refetch();
  };
  // WHAT THE REPORT NOW SHOWS IS LET GO OF, so a later gesture on the same
  // node and sign is a new one rather than this one reopened.
  const nodesSeen = data?.nodes;
  const domainsSeen = data?.domains;
  const servedBy = data?.node_id;
  useEffect(() => {
    if (!nodesSeen) return;
    setGestures((all) => {
      const kept = heldGestures(all, {
        nodes: nodesSeen,
        domains: domainsSeen ?? [],
        node_id: servedBy ?? "",
      });
      return Object.keys(kept).length === Object.keys(all).length ? all : kept;
    });
  }, [nodesSeen, domainsSeen, servedBy]);

  if (!operator) return null;

  const domains = data?.domains ?? [];
  const nodes = data?.nodes ?? [];
  const blocked = domains.filter((d) => d.blocked_by);

  return (
    <>
      {error && (
        <Callout variant="danger" role="alert">
          This retention poll failed ({error}). What is below is the last reading that succeeded —
          on this screen, which is read while nodes are dying, that difference matters.
        </Callout>
      )}
      {loading && !data && <Skeleton variant="text" rows={3} label="Loading" />}

      {/* THE BANNER NAMES THE TERM IN THE OPERATOR'S OWN WORDS, and never the
          term name alone: `backup_max_age` is a field, and "the newest
          complete backup is 3 days old" is the fact somebody acts on. The
          server composes that sentence, because the CLI renders the same one. */}
      {blocked.map((d) => (
        <Callout key={d.domain} variant="warning">
          <span>
            Nothing is being trimmed on <InlineCode>{d.domain}</InlineCode>:{" "}
            {d.prose || `the ${d.blocked_by} term is holding it`}
            {d.blocked_since && <> — since {relTime(d.blocked_since, now)}</>}.{" "}
            <a
              className="prose-link"
              href="https://docs.crewlet.ai/guides/retention"
              target="_blank"
              rel="noreferrer"
            >
              What the six terms mean
            </a>
          </span>
        </Callout>
      ))}

      {data?.maintenance && <MaintenanceBanner op={data.maintenance} now={now} />}

      {data?.register_readable === false && (
        <Callout variant="warning">
          The node register could not be listed, so the rows below are what could be READ of the
          fleet rather than the fleet. An empty list here is not an empty company.
        </Callout>
      )}

      {domains
        .filter((d) => d.evictions_unreadable)
        .map((d) => (
          <Callout key={`evictions:${d.domain}`} variant="warning">
            This node could not read <InlineCode>{d.domain}</InlineCode>'s evictions, so no node
            below shows as evicted there — an eviction may be hidden rather than absent. The next
            report reads them again.
          </Callout>
        ))}

      <ServedLevelBanner level={data?.read_level} />

      {(data?.alarms?.length ?? 0) > 0 && (
        <Card>
          <Card.Header icon={<WarningGlyph size="sm" />} count={data?.alarms.length}>
            Alarms
          </Card.Header>
          <div className="col gap-2">
            {data?.alarms.map((a) => (
              // THE KIND IS NOT THE IDENTITY. `trim_blocked` is raised PER
              // DOMAIN — the tracker's log, the vectors' and the pages' — and
              // `statelog.Alarm` carries only `{kind, detail, remedy}`, so
              // three genuinely different alarms arrive under one kind and
              // React kept ONE of them. Caught by running a node with no
              // backup: three blocked trims, one banner. The pair is what
              // identifies the alarm, and the detail is where the domain is.
              <Callout key={`${a.kind}:${a.detail}`} variant="warning">
                <span>
                  <InlineCode>{a.kind}</InlineCode> — {a.detail}{" "}
                  <span className="muted">{a.remedy}</span>
                </span>
              </Callout>
            ))}
          </div>
        </Card>
      )}

      <Card padding="none">
        <Card.Header icon={<DnsGlyph size="sm" />} count={nodes.length}>
          Replication
        </Card.Header>
        <DataGrid<RetentionNode>
          rows={nodes}
          rowKey={(n) => n.node_id}
          defaultSort="node"
          columns={[
            {
              key: "node",
              header: "Node",
              sortValue: (n) => n.node_id,
              cell: (n) => (
                <span className="row wrap gap-1">
                  {/* A LINK NOW, because the id finally has somewhere to go:
                      a node's page says what it is running, what it holds and
                      which revision it applied, and every one of those is the
                      next question after "this one is behind". The row itself
                      is deliberately NOT a peek row — it carries the evict
                      gesture, and a button inside a link is markup no browser
                      agrees about. */}
                  <KeyCell value={n.node_id} path={["admin", "fleet", n.node_id]} />
                  {n.node_id === thisNode && <Tag variant="brand">this one</Tag>}
                  {n.counted && !n.live && <Tag variant="warning">counted · not live</Tag>}
                  {/* THE FENCE WINDOW IS THE POINT. An eviction is not
                      immediate — the node stays counted until effective_at so
                      a live one is certain to have noticed — and an operator
                      who cannot see that runs the gesture twice. */}
                  {n.evicted && !n.evicted.effective && (
                    <Tag
                      variant="warning"
                      title={`evicted by ${n.evicted.by}; takes effect ${fmtDateTime(
                        n.evicted.effective_at,
                      )}`}
                    >
                      evicted in {fmtDuration(Date.parse(n.evicted.effective_at) - now)}
                    </Tag>
                  )}
                  {n.evicted?.effective && (
                    <Tag variant="danger" title={`evicted by ${n.evicted.by}`}>
                      evicted
                    </Tag>
                  )}
                </span>
              ),
            },
            {
              key: "position",
              header: "Position",
              sortValue: (n) => firstDomain(n)?.seq ?? -1,
              cell: (n) => <NodePositions node={n} />,
            },
            {
              key: "counted",
              header: "Counted",
              shrink: true,
              sortValue: (n) => (n.counted ? 1 : 0),
              // ONE STATE IN ONE SPELLING. A badge on the yes branch and
              // faint prose on the no branch read as two different kinds of
              // fact, and this is one: whether the trim waits for this node.
              cell: (n) => (
                <StatusCell
                  glyph={n.counted ? "●" : "○"}
                  label={n.counted ? "yes" : "no"}
                  tone={n.counted ? "positive" : "neutral"}
                  title={
                    n.counted
                      ? "the trim waits for this node's position"
                      : "the trim no longer waits for this node"
                  }
                />
              ),
            },
            {
              key: "reported",
              header: "Reported",
              shrink: true,
              sortValue: (n) => n.at ?? "",
              cell: (n) =>
                n.at ? (
                  <DateCell at={n.at} now={now} />
                ) : (
                  // NOT A ZERO AND NOT AN EM-DASH. A live node that has
                  // never published a position is COUNTED — the trim waits
                  // for it — and rendering that as "never" or as position 0
                  // are two different wrong answers.
                  <span className="muted">counted · no position yet</span>
                ),
            },
            {
              key: "gate",
              header: "",
              label: "Eviction",
              shrink: true,
              cell: (n) => {
                const action = gateAction(n, gestures);
                return (
                  <span className="row gap-1">
                    {/* A LIVE NODE'S EVICTION IS REFUSED unless forced, and
                        the button alone read as though it would simply
                        work. */}
                    {action.evict && n.live && (
                      <Tag
                        variant="neutral"
                        title="it holds a presence lease, so an eviction is refused unless you force it"
                      >
                        live
                      </Tag>
                    )}
                    <Button
                      variant="tertiary"
                      size="small"
                      onClick={() => setGate({ node: n.node_id, evict: action.evict })}
                    >
                      {action.label}
                    </Button>
                  </span>
                );
              },
            },
          ]}
        />
      </Card>

      <Card>
        <Card.Header icon={<DatabaseGlyph size="sm" />} count={domains.length}>
          Log retention
        </Card.Header>
        {/* ONE BLOCK PER REGISTERED DOMAIN, from the document itself rather
            than as hand-written sections — so a domain added to the register
            appears here with no dashboard change at all. */}
        <div className="col gap-3">
          {domains.map((d) => (
            <DomainBlock key={d.domain} domain={d} />
          ))}
        </div>
      </Card>

      <Card padding="none">
        <Card.Header
          icon={<Package2Glyph size="sm" />}
          count={data?.snapshots?.length}
          subtitle={`${donorsCounted(data?.snapshots ?? [])} of ${SNAPSHOT_DONORS_REQUIRED} donors — the trim's sixth term`}
        >
          Snapshots
        </Card.Header>
        <DataGrid<RetentionSnapshot>
          name="snapshots"
          rows={data?.snapshots ?? []}
          rowKey={(s) => s.node_id}
          defaultSort="node"
          columns={[
            {
              key: "node",
              header: "Node",
              sortValue: (s) => s.node_id,
              cell: (s) => <KeyCell value={s.node_id} path={["admin", "fleet", s.node_id]} />,
            },
            {
              key: "at",
              header: "Taken",
              sortValue: (s) => s.at ?? "",
              cell: (s) =>
                s.at ? (
                  <DateCell at={s.at} now={now} />
                ) : (
                  // THE LOOP'S OWN REASON, never an empty cell: "node-4
                  // none" is not an answer, and `lagging`, `unhydrated`,
                  // `sole_node`, `insufficient_space`, `deferred` and
                  // `recent` are six different things to do about it.
                  <Tag variant="warning">{s.skip || "no snapshot"}</Tag>
                ),
            },
            {
              key: "size",
              header: "Size",
              align: "right",
              // ABSENT RATHER THAN ZERO, on the sort as well as in the cell:
              // a node holding no snapshot is not a node holding an empty one,
              // and `s.bytes ?` would have rendered a real zero — the shape of
              // a snapshot that failed mid-write — as "none at all".
              sortValue: (s) => s.bytes ?? null,
              cell: (s) =>
                s.bytes == null ? (
                  <EmptyValue label="This node holds no snapshot" />
                ) : (
                  <span className="t-num t-caption">{fmtBytes(s.bytes)}</span>
                ),
            },
            {
              key: "positions",
              header: "Covers",
              sortValue: (s) => Object.keys(s.domains ?? {}).length,
              cell: (s) => (
                <span className="row wrap gap-1">
                  {Object.entries(s.domains ?? {}).map(([domain, seq]) => (
                    <Tag key={domain} appearance="outline">
                      {domain} @{seq}
                    </Tag>
                  ))}
                  {!s.domains && <EmptyValue label="This node holds no snapshot" />}
                </span>
              ),
            },
          ]}
        />
      </Card>

      {data?.replica && (
        <Card>
          <Card.Header icon={<ContentCopyGlyph size="sm" />}>This node as a replica</Card.Header>
          <StatGroup>
            <StatCard
              icon={<DatabaseGlyph size="xs" />}
              label="Replicated store"
              value={fmtBytes(data.replica.store_bytes)}
              sub="what a joining peer has to receive"
            />
            <StatCard
              icon={<ScheduleGlyph size="xs" />}
              label="Projected join"
              value={fmtDuration(data.replica.projected_join_seconds * 1000)}
              sub={
                data.replica.rejoin_window_seconds > 0 &&
                data.replica.projected_join_seconds > data.replica.rejoin_window_seconds
                  ? `LONGER than the ${fmtDuration(
                      data.replica.rejoin_window_seconds * 1000,
                    )} window — a node that fell out could not catch up`
                  : `inside the ${fmtDuration(data.replica.rejoin_window_seconds * 1000)} window`
              }
            />
          </StatGroup>
        </Card>
      )}

      {gate && (
        <GateDialog
          node={gate.node}
          evict={gate.evict}
          held={gestures[gestureKey(gate.node, gate.evict)]}
          onHeld={(g) => holdGesture(gate.node, gate.evict, g)}
          onClose={() => setGate(null)}
        />
      )}
    </>
  );
}

/**
 * WHAT THIS DOCUMENT MAY CLAIM ABOUT ITS OWN AGE.
 *
 * A replication answer never takes a barrier: the barrier is the instrument
 * and its own health is the subject, so `linearizable` here is not a stronger
 * answer costing more — it is one that cannot be served, refusing in exactly
 * the incident somebody opened this page for.
 *
 * But `stale` is still a claim about AGE, and a node that could not measure
 * its distance from the log cannot make one — which is the ordinary signature
 * of the broker or coordination being unreachable. Then the level weakens to
 * `consistent_prefix` and this says so, because a document that cannot claim
 * an age otherwise renders IDENTICALLY to one that can: the same figures, read
 * as fresh, during the outage that made them unmeasurable.
 *
 * Nothing renders on the ordinary path. A badge that always drew the level
 * would put a word nobody reads beside every healthy answer, and the one case
 * that matters would arrive as a changed word rather than as a banner.
 */
export function ServedLevelBanner({ level }: { level?: string }) {
  if (!level || level === "stale") return null;
  return (
    <Callout variant="warning">
      This node could not measure its own distance from the log, so the figures below are a coherent
      point in its order with no statement about age (read level <InlineCode>{level}</InlineCode>).
    </Callout>
  );
}

/**
 * MaintenanceBanner is the one thing on this screen that describes an outage
 * in progress rather than a property of the fleet.
 *
 * A capacity operation stops every publisher on every node — no seats, no
 * duties, no scheduler, no write routes — and it was visible on NO screen: an
 * operator watching a company go completely quiet had nothing to look at that
 * said why, and the alarm that names it only fires after an hour of it.
 *
 * So it is a banner rather than a panel, above everything, and it carries the
 * three things somebody finding it at an inconvenient hour needs first: how
 * long it has been open, who opened it, and WHO IS STILL OUTSTANDING — because
 * an operation with nobody outstanding is one waiting on its operator, and
 * that is the state that otherwise looks identical to one waiting on a node.
 */
export function MaintenanceBanner({ op, now }: { op: RetentionMaintenance; now: number }) {
  const missing = op.participants_missing ?? [];
  return (
    <Callout variant="danger" role="alert">
      <span className="col" style={{ gap: 6 }}>
        <span>
          <strong>Maintenance is open on {op.stream}</strong> — no publisher is running anywhere in
          this fleet. Phase <InlineCode>{op.phase}</InlineCode>, attempt {op.attempt}, since{" "}
          {relTime(op.since, now)}
          {op.by && <> ({op.by})</>}. Resizing from {fmtBytes(op.original_max_bytes)} to{" "}
          {fmtBytes(op.target_max_bytes)}.
        </span>
        {op.blocked && (
          <span>
            <strong>Blocked:</strong> {op.blocked} — this needs a person, not time.
          </span>
        )}
        <span className="t-caption">
          {missing.length > 0 ? (
            <>Waiting on {missing.join(", ")}.</>
          ) : (
            // NOBODY OUTSTANDING IS NOT PROGRESS. It is the operation
            // waiting on whoever ran the verb, and rendering it as an
            // empty list would read as "nearly done".
            <>No acknowledgement is outstanding — this operation is waiting on its operator.</>
          )}
        </span>
      </span>
    </Callout>
  );
}

/** The key a held gesture is filed under: one node, one sign. */
function gestureKey(node: string, evict: boolean): string {
  return `${node}:${evict ? "evict" : "readmit"}`;
}

/**
 * Whether the report already shows what a complete gesture did: an eviction
 * once the node reads evicted, a readmission once it no longer does. A node
 * the report no longer lists has no row to mislabel, so its gesture is shown
 * too.
 *
 * A READMISSION IS SHOWN ONLY BY A REPORT THAT READ EVERY LOG'S EVICTIONS the
 * gesture wrote. The report marks a node evicted only where every log holds
 * its tombstone, so one log it could not read hides the eviction whatever
 * happened, and "not evicted" from that report is no evidence the readmission
 * applied: releasing on it offered "Evict…" for a node still evicted, and the
 * next report that did read the logs offered "Readmit…" again — a second
 * record on every log. An eviction needs no such check, because an unread log
 * can only ever hide a tombstone, never show one.
 */
function reflected(
  g: GateGesture,
  key: string,
  nodes: RetentionNode[],
  domains: RetentionDomain[],
): boolean {
  const answer = g.answer;
  if (!answer?.complete) return false;
  const [node, sign] = key.split(":") as [string, string];
  const row = nodes.find((n) => n.node_id === node);
  if (!row) return true;
  if (sign === "evict") return Boolean(row.evicted);
  const read = answer.domains.every((d) => {
    const log = domains.find((r) => r.domain === d.domain);
    return log !== undefined && !log.evictions_unreadable;
  });
  return read && !row.evicted;
}

/**
 * Whether the node that served this report has applied every record a complete
 * gesture wrote, so whatever the report says about the gesture's node already
 * includes it — and a report that still disagrees means something LATER moved
 * the node: a readmission from the command line straight after an eviction
 * here, say. The gesture is spent then. Held, it left the row reading
 * "Eviction sent…" for the rest of the page's life, reopening an answer with
 * nothing to press, so the node could not be evicted again from this screen.
 *
 * STRICT, never a guess: a log whose position the answer lacks, a serving node
 * the report does not list or whose own row carries no position there, and a
 * generation that differs all keep the gesture held. A report that has not
 * caught up with a `pending` record disagrees for exactly that reason, and
 * letting go of it then offered the fresh gesture the hold exists to prevent.
 *
 * AND ONLY WHERE THE SERVING NODE READ ITS EVICTIONS on every log the gesture
 * wrote. One it could not read contributes no tombstone, so the node reads as
 * not evicted there whatever happened — during an adoption's rename, say — and
 * that absence released an eviction the operator had just made, offering it
 * again: a second record on every log, re-dating the first.
 */
function overtaken(
  g: GateGesture,
  nodes: RetentionNode[],
  domains: RetentionDomain[],
  servedBy?: string,
): boolean {
  const answer = g.answer;
  if (!answer?.complete || !servedBy) return false;
  const here = nodes.find((n) => n.node_id === servedBy)?.domains;
  if (!here) return false;
  return answer.domains.every((d) => {
    const at = here[d.domain];
    const row = domains.find((r) => r.domain === d.domain);
    return (
      d.position !== undefined &&
      at !== undefined &&
      at.generation === d.position.generation &&
      at.applied_through >= d.position.seq &&
      row !== undefined &&
      !row.evictions_unreadable
    );
  });
}

/**
 * The gestures still worth holding against this report: every one a request
 * can still finish, and every complete one the report does not show yet —
 * unless the node that served it has applied every record the gesture wrote
 * ([overtaken]), when its disagreement is a later change rather than a lag.
 */
export function heldGestures(
  gestures: Record<string, GateGesture>,
  report: Pick<RetentionReport, "nodes" | "domains" | "node_id">,
): Record<string, GateGesture> {
  return Object.fromEntries(
    Object.entries(gestures).filter(
      ([key, g]) =>
        !reflected(g, key, report.nodes, report.domains) &&
        !overtaken(g, report.nodes, report.domains, report.node_id),
    ),
  );
}

/**
 * What a node row's gate button does.
 *
 * A GESTURE STILL TO BE FINISHED COMES FIRST. The report marks a node evicted
 * only once EVERY log holds its tombstone, so after a partial eviction the row
 * would offer "Evict…" again — a fresh gesture over the logs the first one
 * reached — and after a partial readmission it would offer the eviction. The
 * held gesture is what says which sign is in flight.
 */
export function gateAction(
  n: RetentionNode,
  gestures: Record<string, GateGesture>,
): { evict: boolean; label: string } {
  const readmit = gestures[gestureKey(n.node_id, false)];
  if (readmit && finishable(readmit)) return { evict: false, label: "Finish readmission…" };
  const evict = gestures[gestureKey(n.node_id, true)];
  if (evict && finishable(evict)) return { evict: true, label: "Finish eviction…" };
  // A COMPLETE GESTURE THE REPORT DOES NOT SHOW YET reopens as itself — its
  // answer, and nothing to press — rather than as a fresh gesture with a new
  // id over the logs it already holds.
  if (evict?.answer?.complete && !n.evicted) return { evict: true, label: "Eviction sent…" };
  if (readmit?.answer?.complete && n.evicted) return { evict: false, label: "Readmission sent…" };
  return n.evicted ? { evict: false, label: "Readmit…" } : { evict: true, label: "Evict…" };
}

/** firstDomain is any one of a node's domains, for a sort key. */
function firstDomain(n: RetentionNode) {
  const domains = Object.values(n.domains ?? {});
  return domains.length ? domains[0] : undefined;
}

/** donorsCounted is how many nodes hold a snapshot at all. */
function donorsCounted(snapshots: RetentionSnapshot[]): number {
  return snapshots.filter((s) => s.at && s.domains).length;
}

/**
 * NodePositions renders one node's per-domain progress.
 *
 * `applied_through` is shown BESIDE `seq` rather than instead of it, and a
 * node whose two differ carries a deferred badge: a position that advances
 * while nothing is applied is exactly what a retained record produces, and
 * from the position alone it is indistinguishable from being caught up.
 */
export function NodePositions({ node }: { node: RetentionNode }) {
  const domains = Object.entries(node.domains ?? {});
  if (domains.length === 0) {
    // ABSENT IS NOT ZERO. This node has published nothing for any domain,
    // which the "Reported" column explains; a 0 here would read as a node
    // that has applied nothing, which is a different claim.
    return <EmptyValue label="This node has published no position for any domain" />;
  }
  return (
    <span className="col gap-1">
      {domains.map(([name, d]) => (
        <span key={name} className="row gap-1 t-caption">
          <InlineCode>{name}</InlineCode>
          {/* THE GENERATION BESIDE THE SEQUENCE, because a sequence is a
              number in ONE generation's space: the readmission refusal tells
              an operator to compare a node's position "at the log's current
              generation", and this screen showed no generation at all. */}
          <span className="muted">gen {d.generation}</span>
          <span className="t-num">
            {d.seq}
            {d.applied_through !== d.seq && (
              <span className="muted"> · applied {d.applied_through}</span>
            )}
          </span>
          <PositionStanding d={d} />
          {d.log_diverged && (
            <Tag
              variant="danger"
              title="the log holds another record at this node's checkpoint than the one it consumed there; every other node refuses its writes of this log until an operator decides which history the fleet keeps"
            >
              log diverged
            </Tag>
          )}
          {(d.deferred ?? 0) > 0 && (
            <Tag
              variant="danger"
              title="this node holds records it cannot decode; its rows are missing their effects"
            >
              {d.deferred} deferred
            </Tag>
          )}
        </span>
      ))}
    </span>
  );
}

/**
 * How a node's position stands against the log: how far behind within one
 * generation, or which generation it is on where that is not the log's.
 *
 * A POSITION FROM ANOTHER GENERATION IS NOT A DISTANCE. Its sequence is in a
 * space the log has left, and the difference read 0 whenever the old number
 * was the larger — a node on a dead number space reported caught up.
 */
function PositionStanding({ d }: { d: RetentionNodeDomain }) {
  if (d.generation_state === "left" || d.generation_state === "ahead") {
    return (
      <Tag
        variant="warning"
        title="this position is from another generation of the log, so its sequence compares with nothing the log holds now"
      >
        {d.generation_state} gen {d.generation}
      </Tag>
    );
  }
  if (d.lag == null) {
    return (
      <span className="muted" title="the stream could not be read, so the lag is unknown">
        lag —
      </span>
    );
  }
  return d.lag > 0 ? <span className="muted">{d.lag} behind</span> : null;
}

/**
 * One domain's whole state, drawn once.
 *
 * EXPORTED so the domain's own page and the fleet card draw the same thing: a
 * domain is recognised by its floor, its six terms and what is blocking it,
 * and two renderings of that would let the card and the page disagree about
 * whether a log is advancing.
 */
export function DomainBlock({ domain: d }: { domain: RetentionDomain }) {
  return (
    <div className="col gap-2">
      <DomainRefusals domain={d} />
      <div className="row wrap gap-2 baseline">
        <InlineCode>{d.domain}</InlineCode>
        <Tag appearance="outline">{d.replay}</Tag>
        <span className="t-caption">generation {d.generation}</span>
        <span className="t-caption t-num">
          {d.first_seq}…{d.last_seq}
        </span>
        <span className="t-caption">
          <FloorFact domain={d} />
        </span>
        <span className="t-caption t-num">
          <DomainSize domain={d} />
        </span>
        <TrimStatus domain={d} />
      </div>
      <Terms terms={d.terms} snapshotBlocked={d.snapshot_blocked_by} empty={noConclusion(d)} />
    </div>
  );
}

/**
 * The trim's status as one tag — ONE rendering, for the card and the page.
 *
 * "ADVANCING" ONLY WHERE THE TRIM HAS CONCLUDED SOMETHING. A domain with no
 * published floor at its own generation has no blocking term because it has
 * no terms at all, and both screens drew that as a trim nothing is holding —
 * right after every reanchor, which is when they are read.
 */
export function TrimStatus({ domain: d }: { domain: RetentionDomain }) {
  switch (d.trim_floor_state) {
    case "published":
      return d.blocked_by ? (
        <Tag variant="warning">{d.blocked_by}</Tag>
      ) : (
        <Tag variant="success">advancing</Tag>
      );
    case "unreadable":
      return <Tag variant="warning">floor unreadable</Tag>;
    default:
      return <Tag variant="neutral">no conclusion yet</Tag>;
  }
}

/**
 * The floor and what this tick concluded, as one phrase.
 *
 * THE FLOOR IS THE FLEET'S PUBLISHED BOUND — everything below it may already be
 * gone, and it never moves down within a generation — and THE CONCLUSION IS
 * WHAT THIS TICK'S SIX TERMS PERMITTED, which is zero while the trim is blocked.
 * They are equal on a healthy domain; a blocked one reads "blocked" rather than
 * "concluded 0", which is the wire value and not a position. Neither is a
 * number at all where nothing is published.
 */
export function FloorFact({ domain: d }: { domain: RetentionDomain }) {
  switch (d.trim_floor_state) {
    case "published":
      return (
        <>
          floor <span className="t-num">{d.trim_floor}</span>
          {d.blocked_by ? (
            <span className="muted"> · blocked</span>
          ) : (
            d.trim_to !== d.trim_floor && (
              <span className="muted">
                {" "}
                · this tick concluded <span className="t-num">{d.trim_to}</span>
              </span>
            )
          )}
        </>
      );
    case "unreadable":
      return <>floor unreadable</>;
    default:
      return <>no floor yet</>;
  }
}

/** The sentence a domain with no conclusion shows in place of its terms. */
function noConclusion(d: RetentionDomain): string | undefined {
  switch (d.trim_floor_state) {
    case "published":
      return undefined;
    case "unreadable":
      return `The trim floor register could not be read, so nothing is known here about the trim of generation ${d.generation}.`;
    default:
      return `The trim has concluded nothing about generation ${d.generation} yet — its first tick on this stream is pending.`;
  }
}

/**
 * The answering node's own refusals of a domain, above everything else about
 * it: every figure beside them describes rows no read — or no write — of this
 * domain is served from, and the guides send an operator here after a restore
 * to find exactly this.
 */
export function DomainRefusals({ domain: d }: { domain: RetentionDomain }) {
  return (
    <>
      {d.not_ready && (
        <Callout variant="danger" role="alert">
          <span>
            <strong>Not ready on this node:</strong> <InlineCode>{d.not_ready.code}</InlineCode>
            {(d.not_ready.causes?.length ?? 0) > 0 && (
              <> ({d.not_ready.causes!.join(", ")})</>
            )} — {d.not_ready.detail}
          </span>
        </Callout>
      )}
      {d.writes_refused && (
        <Callout variant="warning" role="alert">
          <span>
            <strong>Writes refused on this node:</strong>{" "}
            <InlineCode>{d.writes_refused.code}</InlineCode> — {d.writes_refused.detail}
          </span>
        </Callout>
      )}
    </>
  );
}

/**
 * A domain's size: what the log holds, how much of the ceiling its ordinary
 * writes are held to is left, and the gate reserve kept above that ceiling.
 *
 * ONE RENDERING for the fleet card and the domain's page, for [DomainBlock]'s
 * reason — the page drew its own copy, which is how two screens come to
 * disagree about one number.
 *
 * THE RESERVE IS NAMED BESIDE THE HEADROOM, because on a log that keeps one
 * "0% free" is not a log nothing can be written to: an eviction still lands
 * there, and it is the gesture that unpins a log a gone node has filled.
 */
export function DomainSize({ domain: d }: { domain: RetentionDomain }) {
  return (
    <>
      {fmtBytes(d.bytes)}
      {/* A FRACTION OF AN UNKNOWN CEILING IS NOT ZERO HEADROOM, which is
          why the server sends it absent. Rendering it as 0% would fire
          the one alarm nobody may ignore. */}
      {d.headroom_fraction != null && (
        <span className="muted"> · {Math.round(d.headroom_fraction * 100)}% free</span>
      )}
      {d.reserve_bytes != null && (
        <span className="muted"> · {fmtBytes(d.reserve_bytes)} kept for evictions</span>
      )}
    </>
  );
}

/** Terms renders the six, each with its own sentence and its own remedy. */
export function Terms({
  terms,
  snapshotBlocked,
  empty,
}: {
  terms: RetentionTerm[];
  snapshotBlocked?: string;
  /** What stands in the table's place where there are no terms to draw. */
  empty?: string;
}) {
  return (
    <div className="col gap-2">
      {snapshotBlocked && (
        <Callout variant="warning">
          <span>
            This node is taking no snapshots: <InlineCode>{snapshotBlocked}</InlineCode>. A stalled
            snapshot tier and a stalled trim are different problems — this one is silent until a
            node tries to join.
          </span>
        </Callout>
      )}
      {terms.length === 0 && empty && <p className="t-caption muted">{empty}</p>}
      <table className="table">
        <tbody>
          {terms.map((t) => (
            <tr key={t.name}>
              <td>
                <InlineCode>{t.name}</InlineCode>
              </td>
              <td>
                {/* `n/a` RATHER THAN `0` for a term this domain does not
                    have, and `unknown` rather than a number for one that
                    could not be read — a term permitting zero and a term
                    nobody could evaluate are different things to do. And
                    `unbounded` for one that binds nothing: its sequence
                    inside the engine is 2^64-1, the identity for the
                    minimum the trim takes, which this cell printed as
                    `18446744073709552000` next to the term's own prose
                    saying nothing was pinning the log. Every state but
                    `ok` is a word, because in every one of them the
                    number is not an answer. */}
                {t.state === "ok" ? (
                  <span className="t-num t-caption">{t.seq ?? 0}</span>
                ) : (
                  <Tag variant={t.state === "unknown" ? "warning" : "neutral"}>{t.state}</Tag>
                )}
              </td>
              <td className="t-caption">{t.detail}</td>
              <td className="t-caption">{t.remedy}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
