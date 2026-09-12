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

import { useState } from "react";
import { Badge, Banner, Panel, Skeleton, Stat, StatRow } from "~/ui/primitives.tsx";
import { DataTable } from "~/ui/DataTable.tsx";
import { Icon } from "~/ui/Icon.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtBytes, fmtDateTime, fmtDuration, relTime } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import { apiToken } from "~/protocol/authToken.ts";
import type {
  RetentionDomain,
  RetentionMaintenance,
  RetentionNode,
  RetentionSnapshot,
  RetentionTerm,
} from "~/protocol/index.ts";
import { GateDialog } from "./GateDialog.tsx";

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
  const { data, loading, error } = useQuery("retention", undefined, {
    pollMs: POLL_MS,
    enabled: operator,
  });
  const [gate, setGate] = useState<{ node: string; evict: boolean } | null>(null);

  if (!operator) return null;

  const domains = data?.domains ?? [];
  const nodes = data?.nodes ?? [];
  const blocked = domains.filter((d) => d.blocked_by);

  return (
    <>
      {error && (
        <Banner tone="critical" icon="alert">
          This retention poll failed ({error}). What is below is the last reading that succeeded —
          on this screen, which is read while nodes are dying, that difference matters.
        </Banner>
      )}
      {loading && !data && <Skeleton rows={3} />}

      {/* THE BANNER NAMES THE TERM IN THE OPERATOR'S OWN WORDS, and never the
          term name alone: `backup_max_age` is a field, and "the newest
          complete backup is 3 days old" is the fact somebody acts on. The
          server composes that sentence, because the CLI renders the same one. */}
      {blocked.map((d) => (
        <Banner key={d.domain} tone="caution" icon="alert">
          <span>
            Nothing is being trimmed on <code className="inline">{d.domain}</code>:{" "}
            {d.prose || `the ${d.blocked_by} term is holding it`}
            {d.blocked_since && <> — since {relTime(d.blocked_since, now)}</>}.{" "}
            <a href="https://docs.crewlet.ai/guides/retention" target="_blank" rel="noreferrer">
              What the six terms mean
            </a>
          </span>
        </Banner>
      ))}

      {data?.maintenance && <MaintenanceBanner op={data.maintenance} now={now} />}

      {data?.register_readable === false && (
        <Banner tone="caution" icon="alert">
          The node register could not be listed, so the rows below are what could be READ of the
          fleet rather than the fleet. An empty list here is not an empty company.
        </Banner>
      )}

      <ServedLevelBanner level={data?.read_level} />

      {(data?.alarms?.length ?? 0) > 0 && (
        <Panel title="Alarms" icon="alert" count={data?.alarms.length}>
          <div className="col gap-2">
            {data?.alarms.map((a) => (
              <div key={a.kind} className="banner caution">
                <Icon name="alert" size="sm" />
                <span>
                  <code className="inline">{a.kind}</code> — {a.detail}{" "}
                  <span className="faint">{a.remedy}</span>
                </span>
              </div>
            ))}
          </div>
        </Panel>
      )}

      <Panel title="Replication" icon="server" count={nodes.length} padding="none">
        <DataTable<RetentionNode>
          rows={nodes}
          rowKey={(n) => n.node_id}
          defaultSort={{ key: "node", dir: "asc" }}
          columns={[
            {
              key: "node",
              header: "Node",
              sortValue: (n) => n.node_id,
              cell: (n) => (
                <span className="row wrap gap-1">
                  <code className="inline">{n.node_id}</code>
                  {n.node_id === thisNode && <Badge tone="accent">this one</Badge>}
                  {n.counted && !n.live && <Badge tone="caution">counted · not live</Badge>}
                  {/* THE FENCE WINDOW IS THE POINT. An eviction is not
                      immediate — the node stays counted until effective_at so
                      a live one is certain to have noticed — and an operator
                      who cannot see that runs the gesture twice. */}
                  {n.evicted && !n.evicted.effective && (
                    <Badge
                      tone="caution"
                      title={`evicted by ${n.evicted.by}; takes effect ${fmtDateTime(
                        n.evicted.effective_at,
                      )}`}
                    >
                      evicted in {fmtDuration(Date.parse(n.evicted.effective_at) - now)}
                    </Badge>
                  )}
                  {n.evicted?.effective && (
                    <Badge tone="critical" title={`evicted by ${n.evicted.by}`}>
                      evicted
                    </Badge>
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
              cell: (n) =>
                n.counted ? (
                  <Badge tone="positive">yes</Badge>
                ) : (
                  <span className="faint">no</span>
                ),
            },
            {
              key: "reported",
              header: "Reported",
              shrink: true,
              sortValue: (n) => n.at ?? "",
              cell: (n) =>
                n.at ? (
                  <span className="t-caption" title={fmtDateTime(n.at)}>
                    {relTime(n.at, now)}
                  </span>
                ) : (
                  // NOT A ZERO AND NOT AN EM-DASH. A live node that has
                  // never published a position is COUNTED — the trim waits
                  // for it — and rendering that as "never" or as position 0
                  // are two different wrong answers.
                  <span className="faint">counted · no position yet</span>
                ),
            },
            {
              key: "gate",
              header: "",
              shrink: true,
              cell: (n) => (
                <button
                  type="button"
                  className="btn subtle sm"
                  onClick={() => setGate({ node: n.node_id, evict: !n.evicted })}
                >
                  {n.evicted ? "Readmit…" : "Evict…"}
                </button>
              ),
            },
          ]}
        />
      </Panel>

      <Panel title="Log retention" icon="database" count={domains.length}>
        {/* ONE BLOCK PER REGISTERED DOMAIN, from the document itself rather
            than as hand-written sections — so a domain added to the register
            appears here with no dashboard change at all. */}
        <div className="col gap-3">
          {domains.map((d) => (
            <DomainBlock key={d.domain} domain={d} />
          ))}
        </div>
      </Panel>

      <Panel
        title="Snapshots"
        icon="box"
        count={data?.snapshots?.length}
        subtitle={`${donorsCounted(data?.snapshots ?? [])} of ${SNAPSHOT_DONORS_REQUIRED} donors — the trim's sixth term`}
        padding="none"
      >
        <DataTable<RetentionSnapshot>
          rows={data?.snapshots ?? []}
          rowKey={(s) => s.node_id}
          defaultSort={{ key: "node", dir: "asc" }}
          columns={[
            {
              key: "node",
              header: "Node",
              sortValue: (s) => s.node_id,
              cell: (s) => <code className="inline">{s.node_id}</code>,
            },
            {
              key: "at",
              header: "Taken",
              sortValue: (s) => s.at ?? "",
              cell: (s) =>
                s.at ? (
                  <span className="t-caption" title={fmtDateTime(s.at)}>
                    {relTime(s.at, now)}
                  </span>
                ) : (
                  // THE LOOP'S OWN REASON, never an empty cell: "node-4
                  // none" is not an answer, and `lagging`, `unhydrated`,
                  // `sole_node`, `insufficient_space`, `deferred` and
                  // `recent` are six different things to do about it.
                  <Badge tone="caution">{s.skip || "no snapshot"}</Badge>
                ),
            },
            {
              key: "size",
              header: "Size",
              align: "right",
              sortValue: (s) => s.bytes ?? 0,
              cell: (s) =>
                s.bytes ? (
                  <span className="t-num t-caption">{fmtBytes(s.bytes)}</span>
                ) : (
                  <span className="faint">—</span>
                ),
            },
            {
              key: "positions",
              header: "Covers",
              sortValue: (s) => Object.keys(s.domains ?? {}).length,
              cell: (s) => (
                <span className="row wrap gap-1">
                  {Object.entries(s.domains ?? {}).map(([domain, seq]) => (
                    <Badge key={domain} outline>
                      {domain} @{seq}
                    </Badge>
                  ))}
                  {!s.domains && <span className="faint">—</span>}
                </span>
              ),
            },
          ]}
        />
      </Panel>

      {data?.replica && (
        <Panel title="This node as a replica" icon="copy">
          <StatRow>
            <Stat
              icon="database"
              label="Replicated store"
              value={fmtBytes(data.replica.store_bytes)}
              sub="what a joining peer has to receive"
            />
            <Stat
              icon="clock"
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
          </StatRow>
        </Panel>
      )}

      {gate && (
        <GateDialog
          node={gate.node}
          evict={gate.evict}
          onClose={() => setGate(null)}
        />
      )}
    </>
  );
}

/** firstDomain is any one of a node's domains, for a sort key. */
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
    <Banner tone="caution" icon="alert">
      This node could not measure its own distance from the log, so the figures below are a
      coherent point in its order with no statement about age (read level{" "}
      <code className="inline">{level}</code>).
    </Banner>
  );
}

export function MaintenanceBanner({ op, now }: { op: RetentionMaintenance; now: number }) {
  const missing = op.participants_missing ?? [];
  return (
    <Banner tone="critical" icon="alert">
      <span className="col" style={{ gap: 6 }}>
        <span>
          <strong>Maintenance is open on {op.stream}</strong> — no publisher is running anywhere in
          this fleet. Phase <code className="inline">{op.phase}</code>, attempt {op.attempt}, since{" "}
          {relTime(op.since, now)}
          {op.by && <> ({op.by})</>}. Resizing from {fmtBytes(op.original_max_bytes)} to{" "}
          {fmtBytes(op.target_max_bytes)}.
        </span>
        {op.blocked && (
          <span>
            <strong>Blocked:</strong> {op.blocked} — this needs a person, not time.
          </span>
        )}
        <span className="t-caption faint">
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
    </Banner>
  );
}

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
    return <span className="faint">—</span>;
  }
  return (
    <span className="col gap-1">
      {domains.map(([name, d]) => (
        <span key={name} className="row gap-1 t-caption">
          <code className="inline">{name}</code>
          <span className="t-num">
            {d.seq}
            {d.applied_through !== d.seq && (
              <span className="faint"> · applied {d.applied_through}</span>
            )}
          </span>
          {d.lag != null ? (
            d.lag > 0 && <span className="faint">{d.lag} behind</span>
          ) : (
            <span className="faint" title="the stream could not be read, so the lag is unknown">
              lag —
            </span>
          )}
          {(d.deferred ?? 0) > 0 && (
            <Badge
              tone="critical"
              title="this node holds records it cannot decode; its rows are missing their effects"
            >
              {d.deferred} deferred
            </Badge>
          )}
        </span>
      ))}
    </span>
  );
}

/** DomainBlock is one registered domain: its window, its floor, and its six terms. */
function DomainBlock({ domain: d }: { domain: RetentionDomain }) {
  return (
    <div className="col gap-2">
      <div className="row wrap gap-2 baseline">
        <code className="inline">{d.domain}</code>
        <Badge outline>{d.replay}</Badge>
        <span className="t-caption faint">generation {d.generation}</span>
        <span className="t-caption t-num">
          {d.first_seq}…{d.last_seq}
        </span>
        <span className="t-caption faint">
          floor {d.trim_floor}
          {d.trim_to !== d.trim_floor && <> · this tick concluded {d.trim_to}</>}
        </span>
        <span className="t-caption t-num">
          {fmtBytes(d.bytes)}
          {/* A FRACTION OF AN UNKNOWN CEILING IS NOT ZERO HEADROOM, which is
              why the server sends it absent. Rendering it as 0% would fire
              the one alarm nobody may ignore. */}
          {d.headroom_fraction != null && (
            <span className="faint"> · {Math.round(d.headroom_fraction * 100)}% free</span>
          )}
        </span>
        {d.blocked_by ? (
          <Badge tone="caution">{d.blocked_by}</Badge>
        ) : (
          <Badge tone="positive">advancing</Badge>
        )}
      </div>
      <Terms terms={d.terms} snapshotBlocked={d.snapshot_blocked_by} />
    </div>
  );
}

/** Terms renders the six, each with its own sentence and its own remedy. */
export function Terms({ terms, snapshotBlocked }: { terms: RetentionTerm[]; snapshotBlocked?: string }) {
  return (
    <div className="col gap-2">
      {snapshotBlocked && (
        <div className="banner caution">
          <Icon name="alert" size="sm" />
          <span>
            This node is taking no snapshots: <code className="inline">{snapshotBlocked}</code>. A
            stalled snapshot tier and a stalled trim are different problems — this one is silent
            until a node tries to join.
          </span>
        </div>
      )}
      <table className="sub">
        <tbody>
          {terms.map((t) => (
            <tr key={t.name}>
              <td>
                <code className="inline">{t.name}</code>
              </td>
              <td>
                {/* `n/a` RATHER THAN `0` for a term this domain does not
                    have, and `unknown` rather than a number for one that
                    could not be read — a term permitting zero and a term
                    nobody could evaluate are different things to do. */}
                {t.state === "ok" ? (
                  <span className="t-num t-caption">{t.seq ?? 0}</span>
                ) : (
                  <Badge tone={t.state === "unknown" ? "caution" : "neutral"}>{t.state}</Badge>
                )}
              </td>
              <td className="t-caption faint">{t.detail}</td>
              <td className="t-caption faint">{t.remedy}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
