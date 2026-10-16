/**
 * Settings › Backups & retention: what this fleet has backed up, a backup you
 * can take, the state log's domains, what is holding each one's trim, and every
 * node's place in it.
 *
 * `#/settings/backups` is the panels, and `#/settings/backups/{domain}` one
 * domain's page. Domains live ONLY under this segment, one level down, which
 * is what lets a domain be called anything: the fleet page used to hold nodes
 * and domains under one segment and tell them apart by the tail's length.
 *
 * # Two records, because they answer two questions
 *
 * The fleet's register holds each node's NEWEST point and the operator's
 * newest acknowledgement of each log — each log's trim reads the newest of them
 * that covers it, marked here by the policy the trim takes. It cannot say
 * that last night's copy failed or that three were taken this week, so the
 * history is the other record: the runtime audit every `POST /backup` leaves on
 * the node that took it, failures included, for as far back as the event log
 * reads — the span the engine reports as `event_history_seconds`, never a
 * number written here.
 * `backups` answers both; neither is derived from the other here.
 */

import { useState } from "react";
import { uiletTone } from "~/ui/primitives.tsx";
import { href } from "~/app/router.tsx";
import {
  Button,
  Card,
  EMPTY_VALUE,
  EmptyValue,
  InlineCode,
  Skeleton,
  StatCard,
  StatGroup,
  Tag,
  TableKeyCell,
  TableDateCell,
  TableStatusCell,
} from "@crewlethq/ui";
import {
  ClockGlyph,
  DatabaseGlyph,
  RotateCcwGlyph,
  PlusGlyph,
  ShieldGlyph,
  UserGlyph,
} from "@crewlethq/icons/glyphs";
import { QueryState } from "~/components/common.tsx";
import { CoverageNote, missingFrom } from "~/components/CoverageNote.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";

import { useEngineHealth } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useNow } from "~/lib/clock.ts";
import { eventHistorySpan, fmtBytes, fmtDateTime, plural, relTime } from "~/lib/format.ts";
import { useViewer } from "~/lib/viewer.ts";
import { ADMIN, needsWords } from "~/lib/access.ts";
import { throughOf } from "~/lib/attribution.ts";
import { useSeatOf, writerOf, WriterCell } from "./Audit.tsx";
import {
  coverWords,
  oldestRead,
  OUTCOME_WORDS,
  OWNER_KIND_WORDS,
  POLICY_WORDS,
  suggestDir,
} from "~/lib/backups.ts";

import type { BackupPointRow, BackupRunRow, BackupsAnswer } from "~/contract/backups.ts";
import { RetentionPanels, RetentionStatus, useRetentionReport } from "./Retention.tsx";
import { DomainScreen } from "./Domain.tsx";
import { TakeBackupDialog } from "./TakeBackupDialog.tsx";

/** A minute, the retention document's own cadence: a backup is a daily event,
 *  and a copy this page took re-reads the answer the moment it lands. */
const BACKUPS_POLL_MS = 60_000;

export function Backups({ domain }: { domain?: string }) {
  const health = useEngineHealth();
  const [taking, setTaking] = useState(false);
  const viewer = useViewer();
  // THE ENGINE DECIDES WHO READS THIS, and the refusal it answers is drawn
  // with the grant that would have admitted the reader: a page that guessed
  // from a credential's shape decided for a person the engine had not asked
  // about. The button alone reads the viewer, because a press the engine will
  // refuse is one to say no to before it is made.
  const page = domain === undefined;
  const backups = useQuery("backups", undefined, { pollMs: BACKUPS_POLL_MS, enabled: page });
  // THE RETENTION REPORT IS ASKED ONCE FOR THE PAGE, and only of an admin,
  // whom the engine decides it on: the alarms leading the page and the panels
  // under them read this one answer, and asking for anybody else would refuse
  // on every poll and paint the screen red for a reader who simply does not
  // operate the fleet, under the backups' own refusal that already names the
  // access.
  const retention = useRetentionReport(page && viewer.admin);
  const refused =
    !viewer.loading && !viewer.admin ? needsWords("Taking a backup", [ADMIN]) : undefined;
  if (domain !== undefined) return <DomainScreen key={domain} name={domain} />;
  const node = health?.node;
  return (
    <>
      <PageActions>
        <Button
          variant="primary"
          size="small"
          leadingIcon={<PlusGlyph />}
          disabledReason={refused}
          title={refused}
          onClick={() => setTaking(true)}
        >
          Take a backup
        </Button>
      </PageActions>
      {/* WHAT IS WRONG LEADS: the alarms, each once with the logs it stands
          on, and the backup they ask for one press away in the bar. */}
      <RetentionStatus retention={retention} />
      <QueryState
        error={backups.error}
        refusal={backups.refusal}
        detail={backups.detail ?? undefined}
        loading={backups.loading && !backups.data}
      >
        {backups.data ? (
          <BackupRecord
            answer={backups.data}
            owner={retention.data ? (retention.data.backup_owner ?? "") : undefined}
          />
        ) : (
          <Skeleton variant="text" rows={4} label="Loading the backups" />
        )}
      </QueryState>
      <RetentionPanels thisNode={node} retention={retention} />
      {taking && (
        <TakeBackupDialog
          node={node}
          suggested={suggestDir(
            backups.data?.points ?? [],
            node,
            new Date(),
            (backups.data?.history ?? []).filter((r) => r.node === node).map((r) => r.dir),
          )}
          onClose={() => setTaking(false)}
          onTaken={() => backups.refetch()}
        />
      )}
    </>
  );
}

/**
 * The register's points and the history, with the figures above them.
 *
 * NOTHING BACKED UP IS SAID ONCE, by the figure. The two lists under it each
 * drew an empty state of their own when there was nothing at all, three ways
 * of saying "no backup" beside the alarm that already says it; they are drawn
 * once there is something to list, or when a node's history could not be
 * read, which is a gap the empty figure cannot speak for.
 */
export function BackupRecord({
  answer,
  owner,
}: {
  answer: BackupsAnswer;
  /**
   * Whose job the backups are (`retention.backup_owner`), empty where nobody
   * is named, and undefined until the retention report has answered.
   */
  owner?: string;
}) {
  const now = useNow();
  const seatOf = useSeatOf();
  // HOW FAR BACK THE HISTORY READS, as the engine reported it: the history is
  // an event-log read, floored where the log is (`store.EventHistory`).
  const historySeconds = useEngineHealth()?.event_history_seconds;
  const span = eventHistorySpan(historySeconds);

  const read = oldestRead(answer.points);
  const failed = answer.history.filter((r) => r.outcome === "failed").length;
  const requested = `${answer.history.length}${answer.more ? "+" : ""}`;
  const unread = missingFrom([answer.coverage]);
  const nothingYet =
    answer.points.length === 0 &&
    answer.history.length === 0 &&
    unread.nodes.length === 0 &&
    !unread.incomplete;

  return (
    <>
      <Card padding="none">
        <StatGroup columns={owner === undefined ? 3 : 4}>
          <StatCard
            icon={<ClockGlyph size="xs" />}
            // THE OLDEST POINT ANY LOG'S TRIM READS, not the newest: each log
            // is trimmed against the newest counted backup that covers it, so
            // there is one such point per log under `operator`, and the one
            // that matters is the stalest — it is the log the backup-age alarm
            // names first. The freshest hid a log days past its
            // acknowledgement behind another acknowledged a minute ago.
            label="Oldest backup a trim reads"
            value={read ? relTime(read.point.taken_at, now) : EMPTY_VALUE}
            sub={
              read
                ? `${read.point.kind === "operator" ? "acknowledged by an operator" : read.point.owner}${
                    read.point.bytes != null ? ` · ${fmtBytes(read.point.bytes)}` : ""
                  }${read.of > 1 ? ` · oldest of ${read.of}` : ""}`
                : "none yet"
            }
          />
          <StatCard
            icon={<ShieldGlyph size="xs" />}
            // THE CONFIG FIELD'S OWN NAME AND VALUE (`backup_floor`), which is
            // what an operator greps for to change it; the sentence says what
            // it means.
            label="Backup floor"
            value={answer.policy}
            sub={POLICY_WORDS[answer.policy]}
          />
          <StatCard
            icon={<RotateCcwGlyph size="xs" />}
            label="Requested"
            value={requested}
            // THE SPAN THE COUNT ACTUALLY COVERS. A history that filled its
            // page stops short of the event window, so its failures are the
            // newest page's rather than the whole window's.
            sub={`${failed ? `${failed} failed` : "none failed"} · ${
              answer.more ? `in the newest ${answer.history.length}` : `in ${span}`
            }`}
          />
          {owner !== undefined && (
            <StatCard
              icon={<UserGlyph size="xs" />}
              label="Backup owner"
              value={owner || "Not set"}
              // THE FIELD THAT NAMES THEM, which is what an operator types to
              // change it, and what the backup term's remedy points at.
              sub={owner ? "retention.backup_owner" : "set retention.backup_owner"}
            />
          )}
        </StatGroup>
      </Card>

      {!nothingYet && (
        <>
          <Card padding="none">
            <Card.Header icon={<DatabaseGlyph size="sm" />} count={answer.points.length}>
              <Card.Title>Newest per node and log</Card.Title>
            </Card.Header>
            <DataGrid<BackupPointRow>
              name="points"
              rows={answer.points}
              // OWNER AND REACH, because the owner alone is not a row's
              // identity: the operator holds one acknowledgement per log, all
              // under one owner, and a node's single row is told apart by its
              // owner anyway.
              rowKey={(p) => `${p.owner}/${p.covers.map((c) => c.stream).join(",")}`}
              defaultSort="-taken"
              empty={{
                title: "Nothing has been backed up",
                // A COMMAND IS CODE, as it is everywhere else on these screens;
                // in prose `crewlet backup -dir` read as four words of the hint.
                hint: (
                  <>
                    Take one from this page, or run <InlineCode>crewlet backup -dir</InlineCode>.
                  </>
                ),
                icon: "database",
              }}
              columns={[
                {
                  key: "owner",
                  header: "Owner",
                  phoneLead: true,
                  sortValue: (p) => p.owner,
                  cell: (p) =>
                    p.kind === "node" ? (
                      <TableKeyCell value={p.owner} href={href(["settings", "nodes", p.owner])} />
                    ) : (
                      <span className="muted">{OWNER_KIND_WORDS.operator}</span>
                    ),
                },
                {
                  key: "taken",
                  align: "right",
                  header: "Taken",
                  shrink: true,
                  sortValue: (p) => p.taken_at,
                  cell: (p) => <TableDateCell value={p.taken_at} now={now} />,
                },
                {
                  key: "dir",
                  header: "Directory",
                  sortValue: (p) => p.dir ?? "",
                  cell: (p) =>
                    p.dir ? (
                      <span className="mono truncate" title={p.dir}>
                        {p.dir}
                      </span>
                    ) : (
                      <EmptyValue label="An acknowledgement names no directory" />
                    ),
                },
                {
                  key: "size",
                  header: "Size",
                  align: "right",
                  shrink: true,
                  // ABSENT RATHER THAN ZERO: an acknowledgement asserts a copy the
                  // engine never saw, and a 0 would read as an empty one.
                  sortValue: (p) => p.bytes ?? null,
                  cell: (p) =>
                    p.bytes == null ? (
                      <EmptyValue label="The engine never saw this copy" />
                    ) : (
                      <span className="t-num t-caption">{fmtBytes(p.bytes)}</span>
                    ),
                },
                {
                  key: "covers",
                  header: "Covers",
                  optional: true,
                  sortValue: (p) => p.covers.length,
                  cell: (p) => (
                    <span className="t-caption" title={p.covers.map(coverWords).join("\n")}>
                      {plural(p.covers.length, "log")}
                    </span>
                  ),
                },
                {
                  key: "trim",
                  header: "Trim",
                  shrink: true,
                  sortValue: (p) => (p.newest ? 2 : p.counted ? 1 : 0),
                  cell: (p) => <TrimCell point={p} />,
                },
              ]}
            />
          </Card>

          <Card padding="none">
            <Card.Header
              icon={<RotateCcwGlyph size="sm" />}
              count={answer.history.length}
              subtitle="Newest first"
            >
              <Card.Title>Backup history</Card.Title>
            </Card.Header>
            <CoverageNote
              coverage={[answer.coverage]}
              what="this history"
              lost={{
                one: "The backups it took are not here",
                many: "The backups they took are not here",
              }}
            />
            <DataGrid<BackupRunRow>
              name="history"
              rows={answer.history}
              rowKey={(r) => r.id}
              defaultSort="-at"
              isFailed={(r) => r.outcome === "failed"}
              loadedNote={
                answer.more
                  ? `Newest ${answer.history.length}. Older ones are in the event log.`
                  : undefined
              }
              empty={{
                title: `No backup was asked for in ${span}`,
                hint: "Older backups still count above.",
                icon: "rotate-ccw",
              }}
              columns={[
                {
                  key: "at",
                  align: "right",
                  header: "When",
                  shrink: true,
                  sortValue: (r) => r.at,
                  cell: (r) => <TableDateCell value={r.at} now={now} />,
                },
                {
                  key: "node",
                  header: "Node",
                  shrink: true,
                  sortValue: (r) => r.node ?? "",
                  cell: (r) =>
                    r.node ? (
                      <TableKeyCell value={r.node} href={href(["settings", "nodes", r.node])} />
                    ) : (
                      <EmptyValue label="The node was not recorded" />
                    ),
                },
                {
                  key: "who",
                  header: "Who",
                  // WHO AND WITH WHAT, apart (`iam.ActorFor`): the person or
                  // seat that asked, drawn as every audit trail draws a
                  // writer, and the credential they asked through where it
                  // names something else.
                  sortValue: (r) => r.actor,
                  cell: (r) => (
                    <WriterCell
                      writer={writerOf({ actor: r.actor, actorKind: r.actor_kind ?? "" }, seatOf)}
                    />
                  ),
                },
                {
                  key: "through",
                  header: "Through",
                  shrink: true,
                  drop: 1,
                  sortValue: (r) => throughOf(r.actor, r.operator_id),
                  cell: (r) => {
                    const through = throughOf(r.actor, r.operator_id);
                    return through ? (
                      <TableKeyCell value={through} />
                    ) : (
                      <EmptyValue label="Their own" />
                    );
                  },
                },
                {
                  key: "dir",
                  header: "Directory",
                  sortValue: (r) => r.dir,
                  cell: (r) => (
                    <span className="mono truncate" title={r.dir}>
                      {r.dir}
                    </span>
                  ),
                },
                {
                  key: "outcome",
                  header: "Outcome",
                  shrink: true,
                  phoneLead: true,
                  sortValue: (r) => r.outcome,
                  cell: (r) => (
                    <TableStatusCell
                      glyph={r.outcome === "applied" ? "●" : "✕"}
                      label={OUTCOME_WORDS[r.outcome]}
                      title={
                        r.outcome === "applied"
                          ? `Manifest written ${fmtDateTime(r.at)}`
                          : "Incomplete, not a usable backup"
                      }
                      tone={uiletTone(r.outcome === "applied" ? "positive" : "critical")}
                    />
                  ),
                },
              ]}
            />
          </Card>
        </>
      )}
    </>
  );
}

/** Whether the trim reads a point, in the words that say why not. */
function TrimCell({ point: p }: { point: BackupPointRow }) {
  if (p.newest) {
    return (
      <Tag
        variant="success"
        title="The newest counted backup of at least one log it covers, which that log’s trim reads"
      >
        counted · newest
      </Tag>
    );
  }
  if (!p.verified) {
    return (
      <Tag variant="warning" title="Unverified, so it doesn’t count">
        not verified
      </Tag>
    );
  }
  if (p.counted) {
    return (
      <Tag appearance="outline" title="Every log it covers is read from a newer counted backup">
        counted
      </Tag>
    );
  }
  return (
    <Tag appearance="outline" title="Doesn’t count under the backup policy">
      not counted
    </Tag>
  );
}
