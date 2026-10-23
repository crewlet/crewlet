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
 */

import {
  Callout,
  Card,
  EmptyState,
  EmptyValue,
  InlineCode,
  TableKeyCell,
  TableDateCell,
  TableTextCell,
} from "@crewlethq/ui";
import { href } from "~/app/router.tsx";
import { DatabaseGlyph } from "@crewlethq/icons/glyphs";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { QueryState } from "~/components/common.tsx";
import { useNow } from "~/lib/clock.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { FLEET_POLL_MS } from "./Fleet.tsx";

import { fmtCount, plural } from "~/lib/format.ts";
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
  if (backend === "nats") return "the fleet's own NATS bucket, replicated at stream.replicas";
  if (backend.startsWith("s3:")) return `S3 bucket ${backend.slice("s3:".length)}`;
  // A store a newer engine names: shown as it is rather than guessed at.
  return backend;
}

/** What a collection did, in a line. */
export function collectSummary(c: ObjectCollect): string {
  if (c.error) return `stopped: ${c.error}`;
  const deleted = `deleted ${plural(c.deleted, "object")} no file names`;
  const of = `of ${fmtCount(c.listed)} stored, ${fmtCount(c.aged)} past the day's grace`;
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
    f.missing === 0 && f.damaged === 0
      ? "none missing"
      : [
          f.missing > 0 ? `${fmtCount(f.missing)} missing` : "",
          f.damaged > 0 ? `${fmtCount(f.damaged)} damaged` : "",
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
  if (!a.found) return a.error ? `stopped: ${a.error}` : "";
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
 * file whose bytes the store lost, or holds wrong, after acknowledging them,
 * so the card names the first of them — the ones to restore from a backup —
 * from the last audit to run to its end, which an audit that failed after it
 * does not clear.
 *
 * Each state the engine names apart renders apart: a record the store would
 * not give up is not a fleet whose collector has not run.
 */
/**
 * The page: the fleet's record of its object store, read from the same answer
 * Nodes reads, on the same cadence.
 */
export function FileStoragePage() {
  const now = useNow();
  const { data, loading, error, refusal } = useQuery("fleet", undefined, {
    pollMs: FLEET_POLL_MS,
  });
  return (
    <QueryState error={error} refusal={refusal} loading={loading && !data}>
      {data && <FileStorage objects={data.objects} now={now} />}
    </QueryState>
  );
}

export function FileStorage({ objects, now }: { objects?: FleetObjects; now: number }) {
  const reported = objects?.state === "reported" ? objects : undefined;
  const header = (
    <Card.Header
      icon={<DatabaseGlyph size="sm" />}
      subtitle={reported ? backendSummary(reported.backend) : undefined}
    >
      File storage
    </Card.Header>
  );
  // NO RECORD IS NOT AN EMPTY STORE: the node that answered keeps no record
  // of the company's files (one running no company), and says so.
  if (!objects) {
    return (
      <Card>
        {header}
        <EmptyState
          size="compact"
          icon={<DatabaseGlyph size="xl" />}
          title="No record of the company's files"
          description="This node keeps none, so there is nothing to show yet."
        />
      </Card>
    );
  }
  if (!reported) {
    return (
      <Card>
        {header}
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
      </Card>
    );
  }

  const found = reported.audit?.found;
  const lost = (found?.missing ?? 0) + (found?.damaged ?? 0);
  const shown = found?.missing_files ?? [];
  return (
    <Card padding="none">
      {header}
      {lost > 0 && (
        <Card.Body padding="md">
          <Callout variant="danger" role="alert">
            <div className="col gap-2">
              <span>
                {plural(lost, "file")} {lost === 1 ? "is" : "are"} missing or damaged. Restore{" "}
                {lost === 1 ? "it" : "them"} from a backup or upload {lost === 1 ? "it" : "them"}{" "}
                again.
              </span>
              <ul className="col gap-1">
                {shown.map((m) => (
                  <li key={m.object}>
                    <InlineCode>{m.named_by}</InlineCode>{" "}
                    <span className="secondary">
                      {m.damaged ? "damaged" : "missing"} · {m.object}
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
            cell: (r) => <TableTextCell>{r.pass}</TableTextCell>,
          },
          {
            key: "at",
            header: "Last ran",
            shrink: true,
            cell: (r) => <TableDateCell value={r.at} now={now} />,
          },
          {
            key: "result",
            header: "Found",
            cell: (r) =>
              r.result ? (
                <TableTextCell>{r.result}</TableTextCell>
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
      <Card.Body padding="md">
        <span className="secondary">
          Run by{" "}
          <TableKeyCell value={reported.node} href={href(["settings", "nodes", reported.node])} />
        </span>
      </Card.Body>
    </Card>
  );
}
