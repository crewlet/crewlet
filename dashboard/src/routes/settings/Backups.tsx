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
 * The fleet's register holds each owner's NEWEST point — the one the trim's
 * backup term reads, marked here by the policy the trim takes. It cannot say
 * that last night's copy failed or that three were taken this week, so the
 * history is the other record: the runtime audit every `POST /backup` leaves on
 * the node that took it, failures included, for as far back as the event log
 * reads — the span the engine reports as `event_history_seconds`, never a
 * number written here.
 * `backups` answers both; neither is derived from the other here.
 */

import { useMemo, useState } from "react";
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
} from "@crewlethq/ui";
import {
  ClockGlyph,
  DatabaseGlyph,
  RotateCcwGlyph,
  PlusGlyph,
  ShieldGlyph,
} from "@crewlethq/icons/glyphs";
import { QueryState } from "~/components/common.tsx";
import { CoverageNote } from "~/components/CoverageNote.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { DateCell, KeyCell, SeatCell, SeatLabel, StatusCell } from "~/app/frame/cells.tsx";
import { useEngineHealth, useOrg } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useNow } from "~/lib/clock.ts";
import {
  eventHistoryLabel,
  eventHistorySpan,
  fmtBytes,
  fmtDateTime,
  plural,
  relTime,
} from "~/lib/format.ts";
import { indexOrg, seatLookup } from "~/lib/seats.ts";
import {
  coverWords,
  OUTCOME_WORDS,
  OWNER_KIND_WORDS,
  POLICY_WORDS,
  suggestDir,
} from "~/lib/backups.ts";
import { apiToken } from "~/protocol/index.ts";
import type { BackupPointRow, BackupRunRow, BackupsAnswer } from "~/contract/backups.ts";
import { RetentionPanels } from "./Retention.tsx";
import { DomainScreen } from "./Domain.tsx";
import { TakeBackupDialog } from "./TakeBackupDialog.tsx";

/** A minute, the retention document's own cadence: a backup is a daily event,
 *  and a copy this page took re-reads the answer the moment it lands. */
const BACKUPS_POLL_MS = 60_000;

export function Backups({ domain }: { domain?: string }) {
  const health = useEngineHealth();
  const [taking, setTaking] = useState(false);
  // THE SECTION IS OPERATOR-SCOPED AND SAYS SO. Every answer below refuses a
  // reader with no token, and a page of its own that drew nothing would be a
  // blank screen under a heading that promises backups.
  const operator = apiToken() !== "";
  const backups = useQuery("backups", undefined, {
    pollMs: BACKUPS_POLL_MS,
    enabled: operator && domain === undefined,
  });
  if (domain !== undefined) return <DomainScreen key={domain} name={domain} />;
  const node = health?.node;
  return (
    <>
      <PageActions>
        <Button
          variant="primary"
          size="small"
          leadingIcon={<PlusGlyph />}
          disabledReason={operator ? undefined : "Taking a backup needs an operator token."}
          title={operator ? undefined : "Taking a backup needs an operator token."}
          onClick={() => setTaking(true)}
        >
          Take a backup
        </Button>
      </PageActions>
      <PageNote>
        What this fleet has backed up, and each state-log domain&rsquo;s trim — which waits for the
        slowest node and for the newest backup that covers it.
      </PageNote>
      <QueryState error={operator ? null : "unauthorized"} loading={false}>
        <QueryState error={backups.error} loading={backups.loading && !backups.data}>
          {backups.data ? (
            <BackupRecord answer={backups.data} />
          ) : (
            <Skeleton variant="text" rows={4} label="Loading the backups" />
          )}
        </QueryState>
        <RetentionPanels thisNode={node} />
      </QueryState>
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

/** The register's points and the history, with the three figures above them. */
export function BackupRecord({ answer }: { answer: BackupsAnswer }) {
  const now = useNow();
  const org = useOrg();
  // HOW FAR BACK THE HISTORY READS, as the engine reported it: the history is
  // an event-log read, floored where the log is (`store.EventHistory`).
  const historySeconds = useEngineHealth()?.event_history_seconds;
  const span = eventHistorySpan(historySeconds);
  const who = useMemo(() => seatLookup(indexOrg(org)), [org]);
  const newest = answer.points.find((p) => p.newest);
  const failed = answer.history.filter((r) => r.outcome === "failed").length;
  const requested = `${answer.history.length}${answer.more ? "+" : ""}`;

  return (
    <>
      <Card padding="none">
        <StatGroup columns={3}>
          <StatCard
            icon={<ClockGlyph size="xs" />}
            label="Newest counted backup"
            value={newest ? relTime(newest.taken_at, now) : EMPTY_VALUE}
            sub={
              newest
                ? `${newest.kind === "operator" ? "acknowledged by an operator" : newest.owner}${
                    newest.bytes != null ? ` · ${fmtBytes(newest.bytes)}` : ""
                  }`
                : "none — the trim waits for one"
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
        </StatGroup>
      </Card>

      <Card padding="none">
        <Card.Header
          icon={<DatabaseGlyph size="sm" />}
          count={answer.points.length}
          subtitle="Each owner's newest copy, as it announced it to the fleet."
        >
          Newest per owner
        </Card.Header>
        <DataGrid<BackupPointRow>
          name="points"
          rows={answer.points}
          rowKey={(p) => p.owner}
          defaultSort="-taken"
          empty={{
            title: "Nothing has been backed up",
            // A COMMAND IS CODE, as it is everywhere else on these screens;
            // in prose `crewlet backup -dir` read as four words of the hint.
            hint: (
              <>
                Until a backup covers them, no state-log domain can be trimmed. Take one from this
                page, or run <InlineCode>crewlet backup -dir</InlineCode>.
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
                  <KeyCell value={p.owner} path={["settings", "nodes", p.owner]} />
                ) : (
                  <span className="muted">{OWNER_KIND_WORDS.operator}</span>
                ),
            },
            {
              key: "taken",
              header: "Taken",
              shrink: true,
              sortValue: (p) => p.taken_at,
              cell: (p) => <DateCell at={p.taken_at} now={now} />,
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
          subtitle="Every backup a person asked a node for, newest first, from the runtime audit."
        >
          Backup history
        </Card.Header>
        <CoverageNote
          coverage={[answer.coverage]}
          what="this history"
          lost="the backups each of them took are not here"
        />
        <DataGrid<BackupRunRow>
          name="history"
          rows={answer.history}
          rowKey={(r) => r.id}
          defaultSort="-at"
          isFailed={(r) => r.outcome === "failed"}
          loadedNote={
            answer.more
              ? `The newest ${answer.history.length} — older backups are in the event log`
              : undefined
          }
          empty={{
            title: `No backup was asked for in ${span}`,
            hint: `This history reads the event log, and ${eventHistoryLabel(historySeconds)}; a backup older than that is still counted above if it is an owner's newest.`,
            icon: "rotate-ccw",
          }}
          columns={[
            {
              key: "at",
              header: "When",
              shrink: true,
              sortValue: (r) => r.at,
              cell: (r) => <DateCell at={r.at} now={now} />,
            },
            {
              key: "node",
              header: "Node",
              shrink: true,
              sortValue: (r) => r.node ?? "",
              cell: (r) =>
                r.node ? (
                  <KeyCell value={r.node} path={["settings", "nodes", r.node]} />
                ) : (
                  <EmptyValue label="The node was not recorded" />
                ),
            },
            {
              key: "who",
              header: "Who",
              sortValue: (r) => r.actor_seat || r.operator,
              cell: (r) => {
                if (!r.actor_seat) return <SeatLabel name={r.operator} kind="human" />;
                const seat = who(r.actor_seat);
                return (
                  <SeatCell
                    handle={r.actor_seat}
                    name={seat.name}
                    kind={seat.kind ?? "human"}
                    title={`${seat.name}, through the operator token ${r.operator}`}
                  />
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
                <StatusCell
                  glyph={r.outcome === "applied" ? "●" : "✕"}
                  label={OUTCOME_WORDS[r.outcome]}
                  tone={r.outcome === "applied" ? "positive" : "critical"}
                  title={
                    r.outcome === "applied"
                      ? `Manifest written ${fmtDateTime(r.at)}`
                      : "No manifest was written: the directory holds debris, not a backup"
                  }
                />
              ),
            },
          ]}
        />
      </Card>
    </>
  );
}

/** Whether the trim reads a point, in the words that say why not. */
function TrimCell({ point: p }: { point: BackupPointRow }) {
  if (p.newest) return <Tag variant="success">counted · newest</Tag>;
  if (!p.verified) {
    return (
      <Tag variant="warning" title="The taker never checked this copy, so no policy counts it">
        not verified
      </Tag>
    );
  }
  if (p.counted) {
    return (
      <Tag appearance="outline" title="A newer counted backup is the one the trim reads">
        counted
      </Tag>
    );
  }
  return (
    <Tag appearance="outline" title="The backup policy does not take this owner's word">
      not counted
    </Tag>
  );
}
