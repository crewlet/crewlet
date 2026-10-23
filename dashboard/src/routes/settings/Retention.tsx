/**
 * The state log's retention: Settings › Backups & retention.
 *
 * # Why it is a section of its own
 *
 * It was drawn under the node list, on the argument that *why is nothing
 * being trimmed* and *which node is behind* are one investigation. They are,
 * and the Nodes section says where this is in one line; but drawn there it put
 * every state-log domain, the evictions and the snapshot tier under a screen
 * called Nodes, where an operator looking for the backups had no way to know
 * to look. Each domain's own page is one segment under this section
 * (`#/settings/backups/{domain}`), and a node's row here links to the node.
 *
 * It keeps the Nodes section's rule: it says when its own poll failed rather
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
 *   - the snapshot block's OWN blocked reason, in the Snapshots card: a
 *     stalled snapshot tier and a stalled trim are different problems with
 *     different remedies, and the first is silent until a node tries to join.
 *
 * # Each problem is said once
 *
 * The alarms lead the page ([RetentionStatus]), each kind once with the logs it
 * stands on, and every panel under them states facts. A log names what holds
 * its trim in a word and keeps its six checks behind a disclosure; a single
 * node's missing snapshots are one sentence. The same missing backup used to
 * be said about twenty times on this page.
 */

import { Fragment, useEffect, useRef, useState } from "react";
import { uiletTone } from "~/ui/primitives.tsx";
import {
  Button,
  Callout,
  Card,
  DescriptionList,
  Disclosure,
  EmptyValue,
  InlineCode,
  Skeleton,
  StatCard,
  StatGroup,
  Tag,
  type DescriptionListEntry,
  TableKeyCell,
  TableDateCell,
  TableStatusCell,
  type Tone,
} from "@crewlethq/ui";
import {
  DatabaseGlyph,
  CopyGlyph,
  PackageGlyph,
  ClockGlyph,
  TriangleAlertGlyph,
} from "@crewlethq/icons/glyphs";
import { DataGrid } from "~/app/frame/DataGrid.tsx";

import { href, useRoute } from "~/app/router.tsx";
import { ALARMS_AT, alarmKindTitle, alarmLogLines, type AlarmLogLine } from "~/lib/alarms.ts";
import { bindingTerm, TERM_WORDS } from "~/lib/retention.ts";
import { reveal } from "~/lib/scroller.ts";
import { useQuery, type QueryResult } from "~/lib/useQuery.ts";
import { asSentence, fmtBytes, fmtDateTime, fmtDuration, fmtExact, relTime } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import { marked } from "~/ui/Problems.tsx";
import type {
  RetentionAlarm,
  RetentionDomain,
  RetentionEviction,
  RetentionMaintenance,
  RetentionNode,
  RetentionNodeDomain,
  RetentionReplica,
  RetentionReport,
  RetentionSnapshot,
  RetentionTerm,
} from "~/protocol/index.ts";
import { finishable, GateDialog } from "./GateDialog.tsx";
import type { GateGesture } from "./GateDialog.tsx";

/** Sixty seconds: this document is assembled from coordination and three
 *  loops, and none of them moves faster than a tick. */
export const RETENTION_POLL_MS = 60_000;

/**
 * The retention report, asked the one way every reader of it asks: this
 * section's page, a log's own page and the tests that stand either up.
 *
 * ONE ANSWER PER PAGE, held by the page and handed to each panel, so the
 * alarms leading the page and the panels under them can never describe two
 * different reports, and a poll asks once rather than once per panel.
 */
export function useRetentionReport(enabled: boolean): QueryResult<RetentionReport> {
  return useQuery("retention", undefined, { pollMs: RETENTION_POLL_MS, enabled });
}

/** The trim's sixth term (D72), and the number an operator has to fix when the
 *  banner names `snapshot_floor`. */
const SNAPSHOT_DONORS_REQUIRED = 2;

/** The alarm list's element, which `at=alarms` brings into view. */
const ALARMS_ID = "retention-alarms";

/**
 * What is wrong, drawn first: the report's own health, an open maintenance
 * operation, and the standing alarms.
 *
 * # Each problem once
 *
 * This page used to say a missing backup about twenty times: in three empty
 * panels, a banner per log, an alarm per log, and the same sentence and remedy
 * in every log's table of terms. The engine now raises each alarm ONCE, with
 * the logs it stands on (`statelog.Alarm.Domains`), so the alarm list is the
 * page's diagnosis and leads it; every panel below states facts and points back
 * up to it rather than saying it again.
 */
export function RetentionStatus({ retention }: { retention: QueryResult<RetentionReport> }) {
  const now = useNow();
  const { data, error } = retention;
  // ARRIVING FOR THE ALARMS (`at=alarms`, the engine panel's "Open alarms")
  // brings their list into view once the report has drawn it, ONCE per
  // arrival, so a poll that moved the count does not scroll a reader who has
  // moved on.
  const route = useRoute();
  const forAlarms = route.query.get("at") === ALARMS_AT;
  const standing = data?.alarms?.length ?? 0;
  const revealed = useRef<string | null>(null);
  useEffect(() => {
    if (!forAlarms || standing === 0 || revealed.current === route.hash) return;
    revealed.current = route.hash;
    reveal(document.getElementById(ALARMS_ID));
  }, [forAlarms, standing, route.hash]);

  return (
    <>
      {error && (
        <Callout variant="danger" role="alert">
          Refresh failed ({error}). Showing the last good reading.
        </Callout>
      )}
      <MaintenanceBanners windows={data?.maintenance} now={now} />
      <ServedLevelBanner level={data?.read_level} />
      {data && standing > 0 && <AlarmList alarms={data.alarms} now={now} />}
    </>
  );
}

/**
 * The standing alarms, each drawn once with the logs it stands on.
 *
 * ROWS OF THE CARD, NOT CALLOUTS IN IT: a bordered warning callout inside the
 * bordered card was a frame in a frame. Each alarm is a row on the card's own
 * inset, ruled from the one above, its warning glyph in the warning ink. And
 * no alarm carries a Take a backup of its own: the page bar's is one press
 * away on the same screen, and a second identical button beside the alarm
 * read as a different gesture.
 */
export function AlarmList({ alarms, now }: { alarms: RetentionAlarm[]; now: number }) {
  return (
    <Card id={ALARMS_ID} padding="none">
      <Card.Header icon={<TriangleAlertGlyph size="sm" />} count={alarms.length}>
        <Card.Title>Alarms</Card.Title>
      </Card.Header>
      <ul className="alarm-rows">
        {alarms.map((alarm) => (
          // THE KIND IS THE IDENTITY: the engine raises each one once.
          <AlarmItem key={alarm.kind} alarm={alarm} now={now} />
        ))}
      </ul>
    </Card>
  );
}

/**
 * One alarm: its name, what was measured, the logs it stands on, and what to
 * do about it.
 *
 * THE NAME IS THE KIND IN WORDS (`lib/alarms.ts`) and the measurement is the
 * engine's own sentence, so this reads the same as the log line and
 * `crewlet retention status`. A single measurement that IS the alarm's is
 * folded into it with the logs it was taken on beside it, as the CLI folds it:
 * drawn as both a headline and a line, the one sentence read as two facts — a
 * young fleet's missing backup, measured alike on every log, is one such
 * sentence naming them all. Every other measurement the alarm stands on is
 * said under it with its logs, as the CLI prints one, so a log a backup holds
 * says how many records it keeps past its window and the second log of a
 * blocked trim is not read as the first one's.
 */
export function AlarmItem({ alarm, now }: { alarm: RetentionAlarm; now: number }) {
  const lines = alarmLogLines(alarm);
  const only = lines.length === 1 ? lines[0] : undefined;
  const folded = only && !only.term && only.detail === alarm.detail ? only : undefined;
  // ONE HELD LOG WHOSE MEASUREMENT IS THE HEADLINE keeps its term's line and
  // drops the sentence under it, which would repeat the headline word for word.
  const headlined =
    only?.term !== undefined && only.logs?.length === 1 && only.logs[0]?.detail === alarm.detail;
  return (
    <li className="alarm-row">
      <span className="alarm-row-glyph" aria-hidden>
        <TriangleAlertGlyph size="sm" />
      </span>
      <div className="alarm-body">
        <p className="alarm-title">{alarmKindTitle(alarm.kind)}</p>
        <p>
          {folded && (
            <>
              <LogNames domains={folded.domains} />:{" "}
            </>
          )}
          {marked(asSentence(alarm.detail), "inherit")}
        </p>
        {!folded && lines.length > 0 && (
          <ul className="alarm-logs">
            {lines.map((line) => (
              <li key={`${line.term ?? ""}:${line.domains.join(",")}`}>
                <AlarmLogText line={line} now={now} measured={!headlined} />
              </li>
            ))}
          </ul>
        )}
        <p className="t-caption muted">{marked(alarm.remedy)}</p>
      </div>
    </li>
  );
}

/**
 * One line of the logs an alarm stands on, in words: a held trim's term with
 * each log's own measurement under it (unless `measured` is false, where the
 * one log's sentence is the headline already).
 */
function AlarmLogText({
  line,
  now,
  measured,
}: {
  line: AlarmLogLine;
  now: number;
  measured: boolean;
}) {
  if (!line.term) {
    return (
      <>
        <LogNames domains={line.domains} />: {marked(line.detail ?? "", "inherit")}
      </>
    );
  }
  const held = line.since ? fmtDuration(now - Date.parse(line.since)) : undefined;
  const logs = measured ? (line.logs ?? []).filter((l) => l.detail !== "") : [];
  return (
    <>
      Trim held by {TERM_WORDS[line.term].holds} on <LogNames domains={line.domains} />
      {held ? <>, for {held}</> : null}.
      {logs.length > 0 && (
        <ul className="alarm-logs">
          {logs.map((l) => (
            <li key={l.domain}>
              <LogLink domain={l.domain} />: {marked(l.detail, "inherit")}
            </li>
          ))}
        </ul>
      )}
    </>
  );
}

/** Logs in a sentence: "tracker, vectors and pages", each one a link to its page. */
function LogNames({ domains }: { domains: string[] }) {
  return (
    <>
      {domains.map((domain, i) => (
        <Fragment key={domain}>
          {i === 0 ? null : i === domains.length - 1 ? " and " : ", "}
          <LogLink domain={domain} />
        </Fragment>
      ))}
    </>
  );
}

/**
 * A log's name in a sentence, as a link to the log's own page. INLINE, unlike
 * a table's key cell, which is a box of its own and stacked a sentence's four
 * names one per line.
 */
function LogLink({ domain }: { domain: string }) {
  return (
    <a className="mono t-link" href={href(["settings", "backups", domain])}>
      {domain}
    </a>
  );
}

/**
 * The fleet's own state under the alarms: every node's place in each log, the
 * logs and their trims, and what seeds a new node.
 */
export function RetentionPanels({
  thisNode,
  retention,
}: {
  thisNode?: string;
  retention: QueryResult<RetentionReport>;
}) {
  const now = useNow();
  const { data, loading, refetch } = retention;
  const [gate, setGate] = useState<{ node: string; evict: boolean } | null>(null);
  // THE GESTURES STILL TO BE FINISHED, keyed by node and sign, held HERE
  // rather than in the dialog: the dialog is unmounted when it closes, and a
  // partial eviction reopened as a fresh one is a second gesture, which writes
  // every log the first one reached again and re-dates its eviction.
  //
  // A COMPLETE ONE IS HELD TOO, until the report shows it. The report is
  // polled, and until its next answer the row still read "Evict…" for a node
  // just evicted; reopened, that minted a fresh id and wrote a second record
  // on every log, re-dating the eviction straight after the dialog said not
  // to retry. So an answer asks the report again at once, and the gesture
  // stays on the row until the report agrees with it, or until a report that
  // already includes it disagrees because something later moved the node
  // ([heldGestures]).
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

  if (!data) {
    return loading ? <Skeleton variant="text" rows={3} label="Loading the logs" /> : null;
  }
  const domains = data.domains ?? [];
  const nodes = data.nodes ?? [];

  return (
    <>
      {data.register_readable === false && (
        <Callout variant="warning">Couldn’t list nodes, so some may be missing below.</Callout>
      )}

      {domains
        .filter((d) => d.evictions_unreadable)
        .map((d) => (
          <Callout key={`evictions:${d.domain}`} variant="warning">
            Couldn’t read <InlineCode>{d.domain}</InlineCode>’s evictions, so evicted nodes may not
            show.
          </Callout>
        ))}

      <Card padding="none">
        <Card.Header icon={<CopyGlyph size="sm" />} count={nodes.length}>
          <Card.Title>Replication</Card.Title>
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
                  <TableKeyCell value={n.node_id} href={href(["settings", "nodes", n.node_id])} />
                  {n.node_id === thisNode && <Tag variant="brand">this one</Tag>}
                  {n.counted && !n.live && <Tag variant="warning">counted · not live</Tag>}
                  {/* A LIVE NODE'S EVICTION IS REFUSED unless forced, and the
                      button alone read as though it would simply work. Said
                      with the node's other states, under the Node head: in
                      the action column it was a fact under no header. */}
                  {gateAction(n, gestures).evict && n.live && (
                    <Tag variant="neutral" title="Still running. Eviction needs force.">
                      live
                    </Tag>
                  )}
                  {n.evicted && <Tombstone evicted={n.evicted} now={now} />}
                </span>
              ),
            },
            {
              key: "position",
              header: "Position",
              // ITS LINES WHOLE: a floor wide enough for the longest log's
              // line, and the counted and reported facts give way first.
              floor: "15rem",
              sortValue: (n) => firstDomain(n)?.seq ?? -1,
              cell: (n) => <NodePositions node={n} />,
            },
            {
              key: "counted",
              header: "Counted",
              shrink: true,
              drop: 2,
              sortValue: (n) => (n.counted ? 1 : 0),
              // ONE STATE IN ONE SPELLING. A badge on the yes branch and
              // faint prose on the no branch read as two different kinds of
              // fact, and this is one: whether the trim waits for this node.
              cell: (n) => (
                <TableStatusCell
                  glyph={n.counted ? "●" : "○"}
                  label={n.counted ? "yes" : "no"}
                  title={
                    n.counted
                      ? "the trim waits for this node's position"
                      : "the trim no longer waits for this node"
                  }
                  tone={uiletTone(n.counted ? "positive" : "neutral")}
                />
              ),
            },
            {
              key: "reported",
              align: "right",
              header: "Reported",
              shrink: true,
              drop: 1,
              sortValue: (n) => n.at ?? "",
              cell: (n) =>
                n.at ? (
                  <TableDateCell value={n.at} now={now} />
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
              action: true,
              shrink: true,
              cell: (n) => {
                const action = gateAction(n, gestures);
                return (
                  <Button
                    variant="ghost"
                    size="small"
                    onClick={() => setGate({ node: n.node_id, evict: action.evict })}
                  >
                    {action.label}
                  </Button>
                );
              },
            },
          ]}
        />
      </Card>

      <LogRetention domains={domains} />

      <SnapshotsCard snapshots={data.snapshots ?? []} replica={data.replica} now={now} />

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

/** The skip reason a single-node fleet's snapshot loop gives, by design. */
const SOLE_NODE = "sole_node";

/**
 * Snapshots and joining: which nodes can seed a new one, and what a join costs.
 *
 * A SINGLE NODE TAKES NO SNAPSHOTS BY DESIGN: its recovery artefact is a
 * backup, which the backup term already gates. It was drawn as a table of one
 * row reading `sole_node`, a donor count of 0 of 2 and a warning under every
 * log, four faults on a fleet working as configured. One sentence says it. Any
 * other reason a node holds no snapshot is a real gap and keeps its row:
 * `lagging`, `unhydrated`, `insufficient_space`, `deferred` and `recent` are
 * five different things to do about it.
 */
export function SnapshotsCard({
  snapshots,
  replica,
  now,
}: {
  snapshots: RetentionSnapshot[];
  replica?: RetentionReplica;
  now: number;
}) {
  const solo = snapshots.length > 0 && snapshots.every((s) => !s.at && s.skip === SOLE_NODE);
  return (
    <Card padding="none">
      <Card.Header
        icon={<PackageGlyph size="sm" />}
        count={solo ? undefined : snapshots.length}
        subtitle={
          solo ? undefined : `${donorsCounted(snapshots)} of ${SNAPSHOT_DONORS_REQUIRED} donors`
        }
      >
        <Card.Title>Snapshots and joining</Card.Title>
      </Card.Header>
      {solo ? (
        <Card.Body padding="md">
          <p className="muted">
            Snapshots start when a second node joins. Until then, a backup is this company’s
            recovery copy.
          </p>
        </Card.Body>
      ) : (
        <DataGrid<RetentionSnapshot>
          name="snapshots"
          rows={snapshots}
          rowKey={(s) => s.node_id}
          defaultSort="node"
          columns={[
            {
              key: "node",
              header: "Node",
              sortValue: (s) => s.node_id,
              cell: (s) => (
                <TableKeyCell value={s.node_id} href={href(["settings", "nodes", s.node_id])} />
              ),
            },
            {
              key: "at",
              align: "right",
              header: "Taken",
              sortValue: (s) => s.at ?? "",
              cell: (s) =>
                s.at ? (
                  <TableDateCell value={s.at} now={now} />
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
                      {domain} through {fmtExact(seq)}
                    </Tag>
                  ))}
                  {!s.domains && <EmptyValue label="This node holds no snapshot" />}
                </span>
              ),
            },
          ]}
        />
      )}
      {replica && (
        <Card.Body padding="md">
          <StatGroup>
            <StatCard
              icon={<DatabaseGlyph size="xs" />}
              label="Replicated store"
              value={fmtBytes(replica.store_bytes)}
              sub="what a joining peer has to receive"
            />
            <StatCard
              icon={<ClockGlyph size="xs" />}
              label="Projected join"
              value={fmtDuration(replica.projected_join_seconds * 1000)}
              sub={
                replica.rejoin_window_seconds > 0 &&
                replica.projected_join_seconds > replica.rejoin_window_seconds
                  ? `Longer than the ${fmtDuration(
                      replica.rejoin_window_seconds * 1000,
                    )} window, so a lagging node can’t catch up`
                  : `inside the ${fmtDuration(replica.rejoin_window_seconds * 1000)} window`
              }
            />
          </StatGroup>
        </Card.Body>
      )}
    </Card>
  );
}

/**
 * A node's tombstone on its row.
 *
 * THE FENCE WINDOW IS THE POINT. An eviction is not immediate — the node stays
 * counted until `effective_at` so a live one is certain to have noticed — and
 * an operator who cannot see that runs the gesture twice.
 */
export function Tombstone({ evicted, now }: { evicted: RetentionEviction; now: number }) {
  const who = `evicted by ${evicted.by}`;
  if (!evicted.effective) {
    return (
      <Tag variant="warning" title={`${who}; takes effect ${fmtDateTime(evicted.effective_at)}`}>
        evicted in {fmtDuration(Date.parse(evicted.effective_at) - now)}
      </Tag>
    );
  }
  return (
    <Tag variant="danger" title={who}>
      evicted
    </Tag>
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
    <Callout variant="warning">How current these figures are can’t be measured right now.</Callout>
  );
}

/**
 * MaintenanceBanners draws a banner per open capacity window, the oldest first
 * as the engine orders them. A window is per log and two can be open at once,
 * each waiting on its own nodes, so one banner for the oldest hid what the other
 * was waiting on until the first had closed.
 */
export function MaintenanceBanners({
  windows,
  now,
}: {
  windows?: RetentionMaintenance[];
  now: number;
}) {
  return (
    <>
      {(windows ?? []).map((op) => (
        <MaintenanceBanner key={op.stream} op={op} now={now} />
      ))}
    </>
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
  // AN ADMISSION FIRST. It holds a window at `opened` whoever has restarted,
  // and no participant owes it, so a banner that named only who it waits on
  // read as every node owing something while one node's claim held the window.
  const held = op.admissions_blocking ?? [];
  return (
    <Callout variant="danger" role="alert">
      <span className="col" style={{ gap: 6 }}>
        <span>
          <strong>Maintenance on {op.stream}</strong>: publishing is paused. Phase{" "}
          <InlineCode>{op.phase}</InlineCode>, attempt {op.attempt}, since {relTime(op.since, now)}
          {op.by && <> ({op.by})</>}. Resizing {fmtBytes(op.original_max_bytes)} to{" "}
          {fmtBytes(op.target_max_bytes)}.
        </span>
        {op.blocked && (
          <span>
            <strong>Blocked:</strong> {op.blocked}. Needs a person.
          </span>
        )}
        <span className="t-caption">
          {held.length > 0 && (
            <>Held by an admission from {held.join(", ")}: nothing is sent while it stands. </>
          )}
          {missing.length > 0 ? (
            <>Waiting on {missing.join(", ")}.</>
          ) : held.length > 0 ? null : (
            // NOBODY OUTSTANDING IS NOT PROGRESS. It is the operation
            // waiting on whoever ran the verb, and rendering it as an
            // empty list would read as "nearly done".
            <>Waiting on its operator.</>
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

/**
 * donorsCounted is how many nodes hold a snapshot at all: one row per node,
 * since every data node keeps the one estate and an artefact is a copy of all
 * of it.
 */
export function donorsCounted(snapshots: RetentionSnapshot[]): number {
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
        // ONE LINE PER LOG: a log's name broke letter by letter ("i / a / m")
        // in a narrow column, and a row grew to 270px.
        <span key={name} className="row gap-1 t-caption nowrap">
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
              title="This node’s history differs from the log. An operator must choose which to keep."
            >
              log diverged
            </Tag>
          )}
          {(d.deferred ?? 0) > 0 && (
            <Tag variant="danger" title="Records this node can’t read yet">
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
        title={
          d.generation_state === "ahead"
            ? "From a newer generation of the log"
            : "From an older generation of the log"
        }
      >
        {d.generation_state} gen {d.generation}
      </Tag>
    );
  }
  if (d.lag == null) {
    return (
      <span className="muted" title="Lag unknown">
        lag unknown
      </span>
    );
  }
  return d.lag > 0 ? <span className="muted">{d.lag} behind</span> : null;
}

/**
 * Every registered log, one row each: what it holds, and whether its trim is
 * moving.
 *
 * ONE ROW PER DOMAIN, from the document rather than written by hand, so a
 * domain added to the register appears here with no dashboard change at all.
 *
 * A ROW SAYS WHAT HOLDS ITS TRIM AND NOTHING MORE. Why, and what to do about
 * it, is the alarm's, drawn once above; the six checks are behind the row for
 * a reader who wants the arithmetic. They were a table of six under every log,
 * each with its remedy, which printed the same backup paragraph once per log
 * and told a healthy single node to evict itself.
 */
export function LogRetention({ domains }: { domains: RetentionDomain[] }) {
  // THE SHARED STATE IS SAID ONCE, in the head, and only a log that differs
  // carries a tag: "held by the backup" on all seven rows was one cause said
  // seven times, and at a phone's width it was the tag that kept its width
  // while the log's own name was cut to "track...".
  const shared = sharedTrimStatus(domains);
  return (
    <Card padding="none">
      <Card.Header
        icon={<ClockGlyph size="sm" />}
        count={domains.length}
        subtitle={
          shared
            ? `${asSentence(shared.label).replace(/\.$/, "")} on ${shared.count === domains.length ? "every log" : `${shared.count} of ${domains.length} logs`}`
            : "A log trims old history once all six checks allow it"
        }
      >
        <Card.Title>Log retention</Card.Title>
      </Card.Header>
      <div>
        {domains.map((d) => (
          <LogRow key={d.domain} domain={d} quiet={shared?.key === trimStatusOf(d).key} />
        ))}
      </div>
    </Card>
  );
}

/**
 * The trim state more than half of the logs share, with how many share it,
 * or null where there is no such state (one log, or no majority): then every
 * row says its own.
 */
export function sharedTrimStatus(
  domains: RetentionDomain[],
): { key: string; label: string; count: number } | null {
  if (domains.length < 2) return null;
  const counts = new Map<string, { label: string; count: number }>();
  for (const d of domains) {
    const { key, label } = trimStatusOf(d);
    const seen = counts.get(key);
    counts.set(key, { label, count: (seen?.count ?? 0) + 1 });
  }
  for (const [key, { label, count }] of counts) {
    if (count * 2 > domains.length) return { key, label, count };
  }
  return null;
}

/**
 * One log: a refusal first, above everything else about it, then its facts and
 * what holds its trim, with its six checks behind a disclosure.
 *
 * EXPORTED so the log's own page and this card draw the same facts, the same
 * status and the same checks: two renderings of a log let the card and the
 * page disagree about whether it is advancing.
 */
export function LogRow({
  domain: d,
  quiet = false,
}: {
  domain: RetentionDomain;
  /** The state is the one its card's head already says for most logs: no tag. */
  quiet?: boolean;
}) {
  return (
    <div className="log-row">
      <DomainRefusals domain={d} />
      <Disclosure
        title={d.domain}
        mono
        size="compact"
        meta={<LogFacts domain={d} />}
        actions={quiet ? undefined : <TrimStatus domain={d} />}
      >
        <div className="col gap-2">
          <Checks domain={d} />
          <a className="t-link" href={href(["settings", "backups", d.domain])}>
            Open the {d.domain} log
          </a>
        </div>
      </Disclosure>
    </div>
  );
}

/**
 * A log's facts in one line: how it replays, its generation, its range, its
 * floor and its size, each a phrase of its own so each reads as itself.
 */
function LogFacts({ domain: d }: { domain: RetentionDomain }) {
  const facts = [
    <span key="replay">{d.replay}</span>,
    <span key="generation">generation {d.generation}</span>,
    <span key="range" className="t-num">
      {d.first_seq}…{d.last_seq}
    </span>,
    <span key="floor">
      <FloorFact domain={d} />
    </span>,
    <span key="size" className="t-num">
      <DomainSize domain={d} />
    </span>,
  ];
  return (
    <span className="t-caption">
      {facts.map((fact, i) => (
        <Fragment key={fact.key}>
          {i > 0 && " · "}
          {fact}
        </Fragment>
      ))}
    </span>
  );
}

/**
 * The trim's status as one tag, ONE rendering for the card and the page.
 *
 * "ADVANCING" ONLY WHERE THE TRIM HAS CONCLUDED SOMETHING. A domain with no
 * published floor at its own generation has no blocking term because it has
 * no terms at all, and both screens drew that as a trim nothing is holding,
 * right after every reanchor, which is when they are read.
 *
 * A HELD TRIM SAYS WHAT HOLDS IT, in words, with the term's own name on hover:
 * the name is what the CLI prints and the guide is indexed by.
 */
export function TrimStatus({ domain: d }: { domain: RetentionDomain }) {
  const { label, tone, title } = trimStatusOf(d);
  return (
    <Tag variant={tone} title={title}>
      {label}
    </Tag>
  );
}

/** The trim's status in words, one source for the tag and the card's shared line. */
function trimStatusOf(d: RetentionDomain): {
  key: string;
  label: string;
  tone: Tone;
  title?: string;
} {
  switch (d.trim_floor_state) {
    case "published":
      return d.blocked_by
        ? {
            key: `held:${d.blocked_by}`,
            label: `held by ${TERM_WORDS[d.blocked_by].holds}`,
            tone: "warning",
            title: `Held by the ${d.blocked_by} check`,
          }
        : { key: "advancing", label: "advancing", tone: "success" };
    case "unreadable":
      return { key: "unreadable", label: "floor unreadable", tone: "warning" };
    default:
      return { key: "none", label: "no conclusion yet", tone: "neutral" };
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
export function noConclusion(d: RetentionDomain): string | undefined {
  switch (d.trim_floor_state) {
    case "published":
      return undefined;
    case "unreadable":
      return `Couldn’t read the trim floor for generation ${d.generation}.`;
    default:
      return `No trim result for generation ${d.generation} yet.`;
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
            )}: {d.not_ready.detail}
          </span>
        </Callout>
      )}
      {d.writes_refused && (
        <Callout variant="warning" role="alert">
          <span>
            <strong>Writes refused on this node:</strong>{" "}
            <InlineCode>{d.writes_refused.code}</InlineCode>: {d.writes_refused.detail}
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

/**
 * A domain's six checks, the one that decides where its trim stops marked.
 *
 * EACH IS ITS WORDS BESIDE ITS NAME (`lib/retention.ts`), its value and the
 * engine's sentence for it. The remedy is printed for the deciding check only:
 * every term carries one, and six side by side told a reader to evict a node
 * that was healthy and to read `min_age` as the problem on a log the backup
 * was holding.
 *
 * `n/a` RATHER THAN `0` for a term this domain does not have, and `unknown`
 * rather than a number for one that could not be read: a term permitting zero
 * and a term nobody could evaluate are different things to do. And
 * `unbounded` for one that binds nothing, whose sequence inside the engine is
 * 2^64-1: printed, it sat beside the term's own sentence saying nothing was
 * pinning the log. Every state but `ok` is a word, because in each of them
 * the number is not an answer.
 */
export function Checks({ domain: d }: { domain: RetentionDomain }) {
  if (d.terms.length === 0) {
    const empty = noConclusion(d);
    return empty ? <p className="t-caption muted">{empty}</p> : null;
  }
  const deciding = bindingTerm(d);
  const holding = Boolean(d.blocked_by);
  return (
    <DescriptionList
      dense
      items={d.terms.map((t): DescriptionListEntry => [
        <span className="check-name">
          <span>{TERM_WORDS[t.name].name}</span>
          <span className="mono t-caption muted">{t.name}</span>
        </span>,
        <CheckValue term={t} deciding={t.name === deciding} holding={holding} />,
      ])}
    />
  );
}

/** One check's value, its sentence, and the remedy where it decides the trim. */
function CheckValue({
  term: t,
  deciding,
  holding,
}: {
  term: RetentionTerm;
  deciding: boolean;
  holding: boolean;
}) {
  return (
    <span className="col gap-1">
      <span className="row wrap gap-1 baseline">
        {t.state === "ok" ? (
          <span className="t-num">{t.seq ?? 0}</span>
        ) : (
          <Tag variant={t.state === "unknown" ? "warning" : "neutral"}>{t.state}</Tag>
        )}
        {deciding &&
          (holding ? (
            <Tag variant="warning">holding the trim</Tag>
          ) : (
            <Tag appearance="outline">sets the floor</Tag>
          ))}
      </span>
      {t.detail && <span className="t-caption muted">{marked(t.detail)}</span>}
      {deciding && t.remedy && <span className="t-caption">{marked(t.remedy)}</span>}
    </span>
  );
}
