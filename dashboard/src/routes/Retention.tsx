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
 * than showing the last reading as if it were now. That is why the failure is
 * a banner above the panels rather than [QueryState], which replaces what it
 * reports on: the last reading has to stay on screen underneath it.
 *
 * # What is load-bearing here, and why each one is
 *
 * Every rendering below exists because the alternative reads as a DIFFERENT
 * fact rather than as a missing one, which is the failure mode a dashboard has
 * that a log does not:
 *
 *   - `counted, no position yet` rather than a zero. A node that has never
 *     reported and a node at sequence zero block the trim for very different
 *     lengths of time.
 *   - a MARKED ABSENCE rather than 0 wherever a node published nothing, which
 *     is the "absent is NOT zero" rule `internal/api/queries/fleet.go` already
 *     states. It is the design system's `EmptyValue`, which carries its own
 *     reading, rather than the punctuation mark this screen used to draw and a
 *     screen reader says "dash" to.
 *   - `applied_through` BESIDE `seq`, because a node applying nothing while its
 *     position advances looks identical to a caught-up one from either number
 *     alone: a deferral rendered as lag is the state nobody diagnoses.
 *   - `n/a` rather than `0` for a term a domain does not have.
 *   - `evicted` carrying its `effective_at` while the fence window is open,
 *     because an operator who cannot see that the exclusion is PENDING runs
 *     the gesture twice.
 *   - the snapshot block's OWN blocked reason: a stalled snapshot tier and a
 *     stalled trim are different problems with different remedies, and the
 *     first is silent until a node tries to join.
 */

import { useState } from "react";
import {
  Callout,
  Card,
  Count,
  DescriptionList,
  EmptyState,
  EmptyValue,
  InlineCode,
  Link,
  RelativeTime,
  Section,
  Skeleton,
  Stack,
  StatCard,
  StatGroup,
  Table,
  Tag,
  useNow,
} from "@crewlethq/ui";
import {
  BlockGlyph,
  DatabaseGlyph,
  DnsGlyph,
  ErrorGlyph,
  Package2Glyph,
  ScheduleGlyph,
  UndoGlyph,
  WarningGlyph,
} from "@crewlethq/icons/glyphs";
import { RecordTable } from "~/components/common.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtBytes, fmtDateTime, fmtDuration } from "~/lib/format.ts";
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

export function RetentionPanels({ thisNode }: { thisNode?: string | undefined }) {
  const now = useNow();
  // OPERATOR-GATED, so the panel renders only when this browser holds a
  // token, the same condition every other operator surface uses. Asking
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
        <Callout variant="danger" icon={<ErrorGlyph size="sm" />}>
          <span>
            This retention poll failed ({error}). What is below is the last reading that succeeded,
            and on this screen, which is read while nodes are dying, that difference matters.
          </span>
        </Callout>
      )}
      {loading && !data && (
        <Skeleton label="Loading the retention report" variant="text" rows={4} />
      )}

      {/* THE BANNER NAMES THE TERM IN THE OPERATOR'S OWN WORDS, and never the
          term name alone: `backup_max_age` is a field, and "the newest
          complete backup is 3 days old" is the fact somebody acts on. The
          server composes that sentence, because the CLI renders the same one. */}
      {blocked.map((d) => (
        <Callout key={d.domain} variant="warning" icon={<WarningGlyph size="sm" />}>
          <span>
            Nothing is being trimmed on <InlineCode tone="inherit">{d.domain}</InlineCode>:{" "}
            {d.prose || `the ${d.blocked_by} term is holding it`}
            {d.blocked_since && (
              <>
                , since <RelativeTime value={d.blocked_since} now={now} />
              </>
            )}
            .{" "}
            <Link href="https://docs.crewlet.ai/guides/retention" external>
              What the six terms mean
            </Link>
          </span>
        </Callout>
      ))}

      {data?.maintenance && <MaintenanceBanner op={data.maintenance} now={now} />}

      {data?.register_readable === false && (
        <Callout variant="warning" icon={<WarningGlyph size="sm" />}>
          <span>
            The node register could not be listed, so the rows below are what could be READ of the
            fleet rather than the fleet. An empty list here is not an empty company.
          </span>
        </Callout>
      )}

      <ServedLevelBanner level={data?.read_level} />

      {(data?.alarms?.length ?? 0) > 0 && (
        <Card as="section">
          <Card.Header icon={<WarningGlyph size="sm" />} count={data?.alarms.length}>
            <Card.Title>Alarms</Card.Title>
          </Card.Header>
          <Stack gap={2}>
            {data?.alarms.map((a) => (
              <Callout key={a.kind} variant="warning" icon={<WarningGlyph size="sm" />}>
                <Stack gap={1}>
                  <span>
                    <InlineCode tone="inherit">{a.kind}</InlineCode>: {a.detail}
                  </span>
                  <span className="t-caption">{a.remedy}</span>
                </Stack>
              </Callout>
            ))}
          </Stack>
        </Card>
      )}

      <Card as="section" padding="none">
        <Card.Header divided icon={<DnsGlyph size="sm" />} count={nodes.length}>
          <Card.Title>Replication</Card.Title>
        </Card.Header>
        <RecordTable<RetentionNode>
          screen="fleet"
          table="retention"
          rows={nodes}
          getRowKey={(n) => n.node_id}
          defaultSort={{ key: "node", direction: "asc" }}
          loading={loading && !data}
          emptyMessage={
            <EmptyState
              size="compact"
              icon={<DnsGlyph />}
              title="No node is in the register"
              description="The trim waits on every node the register counts, so an engine that has not registered one has nothing holding the floor."
            />
          }
          // THE GESTURE IS A ROW ACTION rather than a button in a column of
          // its own, which is what every other destructive gesture in this
          // dashboard is: it names the node it acts on, so a misread row
          // cannot be pressed by accident, and the eviction is drawn in the
          // critical ink while the readmission is not.
          rowActions={(n) => [
            n.evicted
              ? {
                  label: `Readmit ${n.node_id}`,
                  icon: <UndoGlyph />,
                  onClick: () => setGate({ node: n.node_id, evict: false }),
                }
              : {
                  label: `Evict ${n.node_id}`,
                  icon: <BlockGlyph />,
                  danger: true,
                  onClick: () => setGate({ node: n.node_id, evict: true }),
                },
          ]}
          columns={[
            {
              key: "node",
              header: "Node",
              sortable: true,
              // WHICH NODE cannot be hidden: every other cell here is a fact
              // about a node, and a row of them belonging to nobody is a row
              // nobody can act on.
              hideable: false,
              sortValue: (n) => n.node_id,
              render: (n) => (
                <span className="row wrap gap-1">
                  <InlineCode>{n.node_id}</InlineCode>
                  {n.node_id === thisNode && <Tag variant="brand">this one</Tag>}
                  {n.counted && !n.live && <Tag variant="warning">counted, not live</Tag>}
                  {/* THE FENCE WINDOW IS THE POINT. An eviction is not
                      immediate, the node stays counted until effective_at so
                      a live one is certain to have noticed, and an operator
                      who cannot see that runs the gesture twice. */}
                  {n.evicted && !n.evicted.effective && (
                    <Tag
                      variant="warning"
                      title={`evicted by ${n.evicted.by}, takes effect ${fmtDateTime(
                        n.evicted.effective_at,
                      )}`}
                    >
                      evicted <RelativeTime value={n.evicted.effective_at} now={now} />
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
              sortable: true,
              sortValue: (n) => firstDomain(n)?.seq ?? -1,
              render: (n) => <NodePositions node={n} />,
            },
            {
              key: "counted",
              header: "Counted",
              shrink: true,
              sortable: true,
              sortValue: (n) => (n.counted ? 1 : 0),
              render: (n) =>
                n.counted ? <Tag variant="success">yes</Tag> : <Tag variant="neutral">no</Tag>,
            },
            {
              key: "reported",
              header: "Reported",
              shrink: true,
              sortable: true,
              sortValue: (n) => n.at ?? "",
              render: (n) =>
                n.at ? (
                  <RelativeTime className="t-caption" value={n.at} now={now} />
                ) : (
                  // NOT A ZERO AND NOT A MARKED ABSENCE. A live node that has
                  // never published a position is COUNTED, the trim waits for
                  // it, and rendering that as "never" or as position 0 are two
                  // different wrong answers.
                  <span className="muted">counted, no position yet</span>
                ),
            },
          ]}
        />
      </Card>

      {/* ONE BLOCK PER REGISTERED DOMAIN, from the document itself rather
          than as hand-written sections, so a domain added to the register
          appears here with no dashboard change at all.

          A BAND RATHER THAN A PANEL holding panels: each domain is a record
          with a head, a set of figures and a table of its own, and a card
          inside a card draws the same boundary twice. */}
      {data && (
        <Section
          title="Log retention"
          description="what each registered domain holds, and which of the six terms is deciding its floor"
          // HOW MANY DOMAINS, beside the heading, which is where the two
          // panels around this one carry theirs. A band has no header chip of
          // its own, so the count is the heading row's own trailing slot.
          actions={<Count value={domains.length} />}
        >
          {domains.length > 0 ? (
            <Stack gap={4}>
              {domains.map((d) => (
                <DomainBlock key={d.domain} domain={d} />
              ))}
            </Stack>
          ) : (
            <EmptyState
              size="compact"
              icon={<DatabaseGlyph />}
              title="No domain is registered"
              description="A domain registers itself with the trim when its stream is created, so a node that has applied no company has none to report."
            />
          )}
        </Section>
      )}

      <Card as="section" padding="none">
        <Card.Header
          divided
          icon={<Package2Glyph size="sm" />}
          // NO COUNT UNTIL THERE IS AN ANSWER. A zero here before the first
          // poll returns is the one claim this screen may never make: no
          // donor and no reading yet are what the whole panel is about.
          count={data?.snapshots?.length}
          subtitle={`${donorsCounted(data?.snapshots ?? [])} of ${SNAPSHOT_DONORS_REQUIRED} donors, which is the trim's sixth term`}
        >
          <Card.Title>Snapshots</Card.Title>
        </Card.Header>
        <RecordTable<RetentionSnapshot>
          screen="fleet"
          table="snapshots"
          rows={data?.snapshots ?? []}
          getRowKey={(s) => s.node_id}
          defaultSort={{ key: "node", direction: "asc" }}
          loading={loading && !data}
          emptyMessage={
            <EmptyState
              size="compact"
              icon={<Package2Glyph />}
              title="No node holds a snapshot"
              description="A joining peer is hydrated from a snapshot, so until one is taken the trim's sixth term holds the floor at the first record."
            />
          }
          columns={[
            {
              key: "node",
              header: "Node",
              sortable: true,
              hideable: false,
              sortValue: (s) => s.node_id,
              render: (s) => <InlineCode>{s.node_id}</InlineCode>,
            },
            {
              key: "at",
              header: "Taken",
              sortable: true,
              sortValue: (s) => s.at ?? "",
              render: (s) =>
                s.at ? (
                  <RelativeTime className="t-caption" value={s.at} now={now} />
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
              firstDirection: "desc",
              sortable: true,
              sortValue: (s) => s.bytes ?? 0,
              // A RIGHT-ALIGNED COLUMN IS ALREADY A COLUMN OF NUMBERS: the
              // table sets the tabular figures and stops it wrapping, so a
              // cell that restated either would be the screen owning a value
              // the design system owns.
              render: (s) => (s.bytes ? fmtBytes(s.bytes) : <EmptyValue />),
            },
            {
              key: "positions",
              header: "Covers",
              sortable: true,
              sortValue: (s) => Object.keys(s.domains ?? {}).length,
              render: (s) =>
                s.domains ? (
                  <span className="row wrap gap-1">
                    {Object.entries(s.domains).map(([domain, seq]) => (
                      <Tag key={domain} appearance="outline" monospace>
                        {domain} @{seq}
                      </Tag>
                    ))}
                  </span>
                ) : (
                  <EmptyValue label="Holds no snapshot" />
                ),
            },
          ]}
        />
      </Card>

      {/* A BAND OF TILES, not a panel around them. The group draws one
          surface and a hairline between the two figures; a card around it
          would be a box holding a box holding two numbers. */}
      {data?.replica && (
        <Section
          title="This node as a replica"
          description="what a peer joining this fleet has to receive, and whether it could receive it in time"
        >
          <StatGroup columns={2}>
            <StatCard
              icon={<DatabaseGlyph />}
              label="Replicated store"
              value={fmtBytes(data.replica.store_bytes)}
              sub="what a joining peer has to receive"
            />
            <ProjectedJoin replica={data.replica} />
          </StatGroup>
        </Section>
      )}

      {gate && <GateDialog node={gate.node} evict={gate.evict} onClose={() => setGate(null)} />}
    </>
  );
}

/**
 * MaintenanceBanner is the one thing on this screen that describes an outage
 * in progress rather than a property of the fleet.
 *
 * A capacity operation stops every publisher on every node (no seats, no
 * duties, no scheduler, no write routes) and it was visible on NO screen: an
 * operator watching a company go completely quiet had nothing to look at that
 * said why, and the alarm that names it only fires after an hour of it.
 *
 * So it is a banner rather than a panel, above everything, and it carries the
 * three things somebody finding it at an inconvenient hour needs first: how
 * long it has been open, who opened it, and WHO IS STILL OUTSTANDING, because
 * an operation with nobody outstanding is one waiting on its operator, and
 * that is the state that otherwise looks identical to one waiting on a node.
 */
export function MaintenanceBanner({ op, now }: { op: RetentionMaintenance; now: number }) {
  const missing = op.participants_missing ?? [];
  return (
    <Callout variant="danger" role="alert" icon={<ErrorGlyph size="sm" />}>
      <Stack gap={2}>
        <span>
          <strong>Maintenance is open on {op.stream}.</strong> No publisher is running anywhere in
          this fleet. Phase <InlineCode tone="inherit">{op.phase}</InlineCode>, attempt {op.attempt}
          , open for{" "}
          {/* HOW LONG IT HAS BEEN GOING, which is the elapsed reading rather
              than the relative one: "1h ago" beside something still happening
              claims it is over. */}
          <RelativeTime mode="elapsed" value={op.since} now={now} />
          {op.by && <> ({op.by})</>}. Resizing from {fmtBytes(op.original_max_bytes)} to{" "}
          {fmtBytes(op.target_max_bytes)}.
        </span>
        {op.blocked && (
          <span>
            <strong>Blocked:</strong> {op.blocked}. This needs a person, not time.
          </span>
        )}
        <span className="t-caption">
          {missing.length > 0 ? (
            <>Waiting on {missing.join(", ")}.</>
          ) : (
            // NOBODY OUTSTANDING IS NOT PROGRESS. It is the operation
            // waiting on whoever ran the verb, and rendering it as an
            // empty list would read as "nearly done".
            <>No acknowledgement is outstanding, so this operation is waiting on its operator.</>
          )}
        </span>
      </Stack>
    </Callout>
  );
}

/**
 * WHAT THIS DOCUMENT MAY CLAIM ABOUT ITS OWN AGE.
 *
 * A replication answer never takes a barrier: the barrier is the instrument
 * and its own health is the subject, so `linearizable` here is not a stronger
 * answer costing more, it is one that cannot be served, refusing in exactly
 * the incident somebody opened this page for.
 *
 * But `stale` is still a claim about AGE, and a node that could not measure
 * its distance from the log cannot make one, which is the ordinary signature
 * of the broker or coordination being unreachable. Then the level weakens to
 * `consistent_prefix` and this says so, because a document that cannot claim
 * an age otherwise renders IDENTICALLY to one that can: the same figures, read
 * as fresh, during the outage that made them unmeasurable.
 *
 * Nothing renders on the ordinary path. A tag that always drew the level would
 * put a word nobody reads beside every healthy answer, and the one case that
 * matters would arrive as a changed word rather than as a banner.
 */
export function ServedLevelBanner({ level }: { level?: string | undefined }) {
  if (!level || level === "stale") return null;
  return (
    <Callout variant="warning" icon={<WarningGlyph size="sm" />}>
      <span>
        This node could not measure its own distance from the log, so the figures below are a
        coherent point in its order with no statement about age (read level{" "}
        <InlineCode tone="inherit">{level}</InlineCode>).
      </span>
    </Callout>
  );
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
 * How long a joining peer would take, against the window it has to do it in.
 *
 * THE TILE CARRIES THE VERDICT rather than only the figure. A projection
 * longer than the rejoin window is an outcome, which is the one case the
 * design system colours a number for: a node that fell out could not catch up,
 * and the sentence underneath it used to be the only thing that said so.
 */
function ProjectedJoin({
  replica,
}: {
  replica: { projected_join_seconds: number; rejoin_window_seconds: number };
}) {
  const rejoin = replica.rejoin_window_seconds;
  const overruns = rejoin > 0 && replica.projected_join_seconds > rejoin;
  return (
    <StatCard
      icon={<ScheduleGlyph />}
      label="Projected join"
      value={fmtDuration(replica.projected_join_seconds * 1000)}
      {...(overruns ? { tone: "warning" as const } : {})}
      sub={
        overruns
          ? `longer than the ${fmtDuration(rejoin * 1000)} window, so a node that fell out could not catch up`
          : `inside the ${fmtDuration(rejoin * 1000)} window`
      }
    />
  );
}

/**
 * NodePositions renders one node's per-domain progress.
 *
 * `applied_through` is shown BESIDE `seq` rather than instead of it, and a
 * node whose two differ carries a deferred tag: a position that advances
 * while nothing is applied is exactly what a retained record produces, and
 * from the position alone it is indistinguishable from being caught up.
 */
export function NodePositions({ node }: { node: RetentionNode }) {
  const domains = Object.entries(node.domains ?? {});
  if (domains.length === 0) {
    // ABSENT IS NOT ZERO. This node has published nothing for any domain,
    // which the "Reported" column explains; a 0 here would read as a node
    // that has applied nothing, which is a different claim.
    return <EmptyValue label="No position published yet" />;
  }
  return (
    <Stack gap={1}>
      {domains.map(([name, d]) => (
        <span key={name} className="row gap-1 t-caption">
          <InlineCode>{name}</InlineCode>
          <span className="t-num">
            {d.seq}
            {d.applied_through !== d.seq && (
              <span className="muted"> · applied {d.applied_through}</span>
            )}
          </span>
          {d.lag != null ? (
            d.lag > 0 && <span className="muted">{d.lag} behind</span>
          ) : (
            <span
              className="row gap-1 muted"
              title="the stream could not be read, so the lag is unknown"
            >
              lag <EmptyValue label="not measured" />
            </span>
          )}
          {(d.deferred ?? 0) > 0 && (
            <Tag
              variant="danger"
              title="this node holds records it cannot decode, so its rows are missing their effects"
            >
              {d.deferred} deferred
            </Tag>
          )}
        </span>
      ))}
    </Stack>
  );
}

/** DomainBlock is one registered domain: its window, its floor, and its six terms. */
function DomainBlock({ domain: d }: { domain: RetentionDomain }) {
  return (
    <Card as="article">
      <Card.Header
        icon={<DatabaseGlyph size="sm" />}
        subtitle={`${d.replay} replay, generation ${d.generation}`}
        actions={
          d.blocked_by ? (
            <Tag variant="warning">{d.blocked_by}</Tag>
          ) : (
            <Tag variant="success">advancing</Tag>
          )
        }
      >
        <Card.Title>{d.domain}</Card.Title>
      </Card.Header>
      <Stack gap={3}>
        <DescriptionList
          dense
          items={[
            [
              "Records",
              <span className="t-num">
                {d.first_seq}…{d.last_seq}
              </span>,
            ],
            [
              "Trim floor",
              <span className="t-num">
                {d.trim_floor}
                {d.trim_to !== d.trim_floor && (
                  <span className="muted"> · this tick concluded {d.trim_to}</span>
                )}
              </span>,
            ],
            ["Size", <span className="t-num">{fmtBytes(d.bytes)}</span>],
            [
              "Headroom",
              // A FRACTION OF AN UNKNOWN CEILING IS NOT ZERO HEADROOM, which
              // is why the server sends it absent. Rendering it as 0% would
              // fire the one alarm nobody may ignore, so an unread ceiling is
              // a marked absence with its own reading instead.
              d.headroom_fraction != null ? (
                `${Math.round(d.headroom_fraction * 100)}% free`
              ) : (
                <EmptyValue label="The ceiling could not be read" />
              ),
            ],
          ]}
        />
        <Terms terms={d.terms} snapshotBlocked={d.snapshot_blocked_by} />
      </Stack>
    </Card>
  );
}

/** Terms renders the six, each with its own sentence and its own remedy. */
export function Terms({
  terms,
  snapshotBlocked,
}: {
  terms: RetentionTerm[];
  snapshotBlocked?: string | undefined;
}) {
  return (
    <Stack gap={2}>
      {snapshotBlocked && (
        <Callout variant="warning" icon={<WarningGlyph size="sm" />}>
          <span>
            This node is taking no snapshots:{" "}
            <InlineCode tone="inherit">{snapshotBlocked}</InlineCode>. A stalled snapshot tier and a
            stalled trim are different problems, and this one is silent until a node tries to join.
          </span>
        </Callout>
      )}
      <Table
        caption="What the trim is waiting on, term by term"
        captionHidden
        headers={["Term", "Position", "What it holds", "Remedy"]}
        data={terms.map((t) => [
          <InlineCode>{t.name}</InlineCode>,
          // `n/a` RATHER THAN `0` for a term this domain does not have, and
          // `unknown` rather than a number for one that could not be read: a
          // term permitting zero and a term nobody could evaluate are
          // different things to do.
          t.state === "ok" ? (
            <span className="t-num">{t.seq ?? 0}</span>
          ) : (
            <Tag variant={t.state === "unknown" ? "warning" : "neutral"}>{t.state}</Tag>
          ),
          <span className="t-caption">{t.detail}</span>,
          <span className="t-caption">{t.remedy}</span>,
        ])}
      />
    </Stack>
  );
}
