/**
 * Settings › File storage: where the company's files are kept, and what the
 * object store's collector last found. A page of its own in Settings' Engine
 * group; it was a card on Nodes, under the lease tables, where nobody looking
 * for the company's files would think to scroll.
 *
 * READ FROM THE FLEET'S RECORD, never this node: the collector is one duty
 * that moves between data nodes and records each pass in the coordination
 * store, so the card says the same thing whichever node served the screen,
 * and names the node that ran the pass.
 *
 * AND WHAT THE COMPANY HOLDS AGAINST ITS QUOTAS: the artifacts' files, their
 * count and the datastores' footprint against `artifacts.limits`, the plan's
 * quotas in each node's file. They were the foot of Artifacts › All, a
 * gallery, where they were the one block about the engine's storage rather
 * than about artifacts a reader opens; the gallery keeps a related row here.
 */

import type { ReactNode } from "react";
import {
  Callout,
  Card,
  EmptyState,
  EmptyValue,
  InlineCode,
  Link,
  Meter,
  SettingsTable,
  StatCard,
  StatGroup,
  TableDateCell,
  TableTextCell,
  PageNote,
} from "@crewlethq/ui";
import { href } from "~/app/router.tsx";
import { DatabaseGlyph, FolderGlyph, GaugeGlyph } from "@crewlethq/icons/glyphs";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { QueryState } from "~/components/common.tsx";
import { useNow } from "~/lib/clock.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { FLEET_POLL_MS } from "./Fleet.tsx";

import { fmtBytes, fmtCount, plural, tsKey } from "~/lib/format.ts";
import type {
  FleetObjects,
  ObjectAudit,
  ObjectCollect,
  ObjectFindings,
  ReportedObjects,
} from "~/protocol/index.ts";

/**
 * The store, in words: the fleet's own bucket, or the S3 bucket the identity
 * names.
 */
export function backendSummary(backend: string): string {
  // REPLICATED LIKE THE STREAM: the bucket is a stream of the fleet's broker,
  // kept in as many copies as every other one (`stream.replicas`). Named by
  // what it is; the key that sets it is drawn beside it.
  if (backend === "nats") return "The fleet's own NATS object bucket, replicated like the stream";
  if (backend.startsWith("s3:")) return `S3 bucket ${backend.slice("s3:".length)}`;
  // A store a newer engine names: shown as it is rather than guessed at.
  return backend;
}

/** What a collection did, in a line. */
export function collectSummary(c: ObjectCollect): string {
  if (c.error) return `Stopped: ${c.error}`;
  const deleted = `Deleted ${plural(c.deleted, "object")} that no file names`;
  const of = `of ${fmtCount(c.listed)} stored (${fmtCount(c.aged)} past the day's grace)`;
  // A PASS THAT STOPPED JUDGING KEEPS ITS COUNTS: what it deleted before it
  // met an estate it could not read whole is gone, and saying "deleted
  // nothing" would tell the operator otherwise.
  if (c.skipped) return `${deleted}, then stopped: ${c.skipped}`;
  const tidied = c.abandoned > 0 ? `abandoned ${plural(c.abandoned, "unfinished upload")}` : "";
  const done = c.completed ? `${deleted}, ${of}` : `${deleted} before it stopped, ${of}`;
  return tidied ? `${done}; ${tidied}` : done;
}

/** What the last audit to run to its end found, in a line. */
export function auditSummary(f: ObjectFindings): string {
  const asked = `${fmtCount(f.referenced)} named`;
  const lost =
    f.missing === 0 && f.damaged === 0 && f.unanswered === 0
      ? "none missing"
      : [
          f.missing > 0 ? `${fmtCount(f.missing)} missing` : "",
          f.damaged > 0 ? `${fmtCount(f.damaged)} damaged` : "",
          f.unanswered > 0 ? `${fmtCount(f.unanswered)} unanswered` : "",
        ]
          .filter(Boolean)
          .join(", ");
  // AN INCOMPLETE AUDIT'S COUNT IS A FLOOR, and says so: the estate it read
  // was missing a record that may name more.
  return f.completed ? `${asked}, ${lost}` : `${asked} so far, at least ${lost}`;
}

/**
 * The audit row: what the last whole audit found, and — beside it, never in
 * its place — an attempt that failed after it.
 */
export function auditRow(a: ObjectAudit): string {
  if (!a.found) return a.error ? `Stopped: ${a.error}` : "";
  const found = auditSummary(a.found);
  return a.error && a.at > a.found.at ? `${found} (a later audit stopped: ${a.error})` : found;
}

/** One row of the passes grid. */
interface PassRow {
  pass: string;
  at?: string;
  result: string;
  cadence: string;
}

function passRows(r: ReportedObjects): PassRow[] {
  return [
    {
      pass: "Collection",
      at: r.collect?.at,
      result: r.collect ? collectSummary(r.collect) : "",
      cadence: "hourly",
    },
    {
      pass: "Audit",
      at: r.audit?.found?.at ?? r.audit?.at,
      result: r.audit ? auditRow(r.audit) : "",
      cadence: "daily",
    },
  ];
}

/**
 * The object store: which store holds the company's files, and the
 * collector's last collection and audit.
 *
 * FILES THAT CANNOT BE READ ARE WHAT AN OPERATOR OPENS THIS FOR. Each is a
 * file whose bytes the store lost, holds wrong or will not answer for, after
 * acknowledging them, so the page names the first of them (the ones to
 * restore from a backup) from the last audit to run to its end, which an
 * audit that failed after it does not clear.
 *
 * TWO CARDS, EACH NAMED BY ITS SUBJECT: where the files are kept, and the
 * collector's passes. One card titled "File storage" under a page of that
 * name said the page's title twice and stacked the node that ran the passes
 * under the table as a loose line.
 *
 * Each state the engine names apart renders apart: a record the store would
 * not give up is not a fleet whose collector has not run.
 */
export function FileStoragePage() {
  const now = useNow();
  const { data, loading, error, refusal } = useQuery("fleet", undefined, {
    pollMs: FLEET_POLL_MS,
  });
  return (
    <>
      <PageNote>
        Where the company&rsquo;s files are kept, what the collector last found, and what the
        company holds against its storage quotas.
      </PageNote>
      <QueryState error={error} refusal={refusal} loading={loading && !data}>
        {data && <FileStorage objects={data.objects} now={now} />}
      </QueryState>
      <StorageQuotas />
    </>
  );
}

/**
 * The collector's passes, as a card headed by what it lists, and by the node
 * that ran the last of them once the fleet's record names one.
 */
function PassesCard({ node, children }: { node?: string; children: ReactNode }) {
  return (
    <Card padding="none">
      <Card.Header
        icon={<DatabaseGlyph size="sm" />}
        subtitle={
          node ? (
            <>
              {/* INLINE, as the subtitle is one line: the table's key cell is a
                  block and broke the node onto a line of its own. */}
              Last run by{" "}
              <Link href={href(["settings", "nodes", node])} className="mono">
                {node}
              </Link>
            </>
          ) : undefined
        }
      >
        <Card.Title>Collector passes</Card.Title>
      </Card.Header>
      {children}
    </Card>
  );
}

/** How often the quotas are read again: they move with uploads, slowly. */
const QUOTAS_POLL_MS = 60_000;

/**
 * The company's artifacts and datastores against the plan's quotas, each a
 * figure over its ceiling with a bar under it, and the key that sets it.
 *
 * THE COUNT IS THE COMPANY'S, not any listing's: it holds every artifact the
 * quota is charged for, the ones in anybody's trash until they are deleted for
 * good and the private ones no listing shows, so the card says so.
 *
 * NOTHING ON A READ THAT DID NOT ANSWER: a quota with no reading is not a
 * company holding nothing, and the page above already says what failed.
 */
function StorageQuotas() {
  const artifacts = useQuery("artifact_usage", undefined, { pollMs: QUOTAS_POLL_MS });
  const data = useQuery("artifact_data_usage", undefined, { pollMs: QUOTAS_POLL_MS });
  const a = artifacts.data;
  const d = data.data;
  if (artifacts.error || data.error || !a || !d) return null;
  const quotas = [
    {
      key: "artifacts.limits.company_bytes",
      label: "Artifact files",
      value: a.usage.bytes,
      max: a.limits.company_bytes,
      used: fmtBytes(a.usage.bytes),
      of: `of ${fmtBytes(a.limits.company_bytes)}`,
    },
    {
      key: "artifacts.limits.artifacts",
      label: "Artifacts",
      value: a.usage.artifacts,
      max: a.limits.artifacts,
      used: fmtCount(a.usage.artifacts),
      of: `of ${fmtCount(a.limits.artifacts)}`,
    },
    {
      key: "artifacts.limits.data_bytes",
      label: "Datastores",
      value: d.usage.footprint,
      max: d.data_bytes,
      used: fmtBytes(d.usage.footprint),
      of: `of ${fmtBytes(d.data_bytes)}`,
    },
  ];
  return (
    <Card padding="none">
      <Card.Header
        icon={<GaugeGlyph size="sm" />}
        subtitle="Every version of every artifact, private ones and the trash included"
      >
        <Card.Title>Storage quotas</Card.Title>
      </Card.Header>
      <StatGroup columns={3}>
        {quotas.map((quota) => (
          <StatCard
            key={quota.key}
            label={quota.label}
            value={quota.used}
            unit={quota.of}
            trendPlacement="below"
            trend={
              <Meter
                hideLabel
                size="compact"
                label={quota.label}
                value={quota.value}
                max={quota.max}
                valueText={`${quota.used} ${quota.of}`}
              />
            }
            sub={<code className="storage-quota-key">{quota.key}</code>}
          />
        ))}
      </StatGroup>
    </Card>
  );
}

export function FileStorage({ objects, now }: { objects?: FleetObjects; now: number }) {
  // NO RECORD IS NOT AN EMPTY STORE: the node that answered keeps no record
  // of the company's files (one running no company), and says so.
  if (!objects) {
    return (
      <Card>
        <EmptyState
          size="compact"
          icon={<DatabaseGlyph size="xl" />}
          title="No record of the company's files"
          description="This node keeps none, so there is nothing to show yet."
        />
      </Card>
    );
  }
  if (objects.state !== "reported") {
    return (
      <PassesCard>
        <Card.Body padding="md">
          {objects.state === "unavailable" ? (
            <Callout variant="warning" role="alert">
              <span>Couldn’t read the collector’s last report. Files are unaffected.</span>
            </Callout>
          ) : (
            <EmptyState
              size="compact"
              icon={<DatabaseGlyph size="xl" />}
              title="No collection yet"
              description="The first pass runs within a minute of a data node starting."
            />
          )}
        </Card.Body>
      </PassesCard>
    );
  }

  const reported = objects;
  const found = reported.audit?.found;
  const lost = (found?.missing ?? 0) + (found?.damaged ?? 0) + (found?.unanswered ?? 0);
  const shown = found?.missing_files ?? [];
  return (
    <div className="col gap-4">
      {/* THE STORE IS A SETTING, so it is drawn as Limits and Sign-in policy
          draw theirs: the kit's settings table, its key in the node's file.
          Which node ran the last pass is a fact about the passes, and heads
          their card. */}
      <Card padding="none">
        <Card.Header icon={<FolderGlyph size="sm" />}>
          <Card.Title id="file-storage-store">Where files are kept</Card.Title>
        </Card.Header>
        <SettingsTable aria-labelledby="file-storage-store">
          <SettingsTable.Row label="Store" settingKey="store.objects.backend" source="crewlet.yaml">
            {backendSummary(reported.backend)}
          </SettingsTable.Row>
        </SettingsTable>
      </Card>
      <PassesCard node={reported.node}>
        {lost > 0 && (
          <Card.Body padding="md">
            <Callout variant="danger" role="alert">
              <div className="col gap-2">
                <span>
                  {plural(lost, "file")} can’t be read: missing, damaged or unanswered by the store.
                  Check its health, then restore {lost === 1 ? "it" : "them"} from a backup or
                  upload {lost === 1 ? "it" : "them"} again.
                </span>
                <ul className="col gap-1">
                  {shown.map((m) => (
                    <li key={m.object}>
                      <InlineCode>{m.named_by}</InlineCode>{" "}
                      <span className="secondary">
                        {m.finding} · {m.object}
                      </span>
                    </li>
                  ))}
                </ul>
              </div>
            </Callout>
          </Card.Body>
        )}
        {reported.collect?.sweep_error && (
          <Card.Body padding="md">
            <Callout variant="warning" role="status">
              <span>
                Unfinished uploads couldn’t be cleared: {reported.collect.sweep_error}. On S3, grant{" "}
                <InlineCode>s3:ListBucketMultipartUploads</InlineCode> and{" "}
                <InlineCode>s3:AbortMultipartUpload</InlineCode>.
              </span>
            </Callout>
          </Card.Body>
        )}
        <DataGrid<PassRow>
          name="file-storage"
          rows={passRows(reported)}
          rowKey={(r) => r.pass}
          columns={[
            {
              key: "pass",
              header: "Pass",
              shrink: true,
              sortValue: (r) => r.pass,
              cell: (r) => <TableTextCell>{r.pass}</TableTextCell>,
            },
            {
              key: "at",
              align: "right",
              header: "Last ran",
              shrink: true,
              // A PASS THAT HAS NOT RUN has no instant, and sorts last.
              sortValue: (r) => (r.at ? tsKey(r.at) : null),
              cell: (r) => <TableDateCell value={r.at} now={now} />,
            },
            {
              key: "result",
              header: "Found",
              // THE SENTENCE WRAPS rather than being cut: it is the answer the
              // page is for, and at 1024 it read half of itself.
              cell: (r) =>
                r.result ? (
                  <span className="t-body">{r.result}</span>
                ) : (
                  <EmptyValue label="Not run yet" />
                ),
            },
            {
              key: "cadence",
              header: "Runs",
              shrink: true,
              cell: (r) => <TableTextCell>{r.cadence}</TableTextCell>,
            },
          ]}
        />
      </PassesCard>
    </div>
  );
}
