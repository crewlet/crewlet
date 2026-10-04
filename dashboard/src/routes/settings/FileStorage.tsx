/**
 * Where the company's files are kept, and what the object store's collector
 * last found — the Nodes screen's card beneath the lease tables.
 *
 * READ FROM THE FLEET'S RECORD, never this node: the collector is one duty
 * that moves between data nodes and records each pass in the coordination
 * store, so the card says the same thing whichever node served the screen,
 * and names the node that ran the pass.
 */

import { Callout, Card, EmptyState, EmptyValue, InlineCode } from "@crewlethq/ui";
import { DatabaseGlyph } from "@crewlethq/icons/glyphs";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { DateCell, KeyCell, TextCell } from "~/app/frame/cells.tsx";
import { fmtCount, plural } from "~/lib/format.ts";
import type {
  FleetObjects,
  ObjectAudit,
  ObjectCollect,
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
  if (c.skipped) return `deleted nothing: ${c.skipped}`;
  const deleted = `deleted ${plural(c.deleted, "chunk")} no file names`;
  const of = `of ${fmtCount(c.listed)} stored, ${fmtCount(c.aged)} past the day's grace`;
  return c.completed ? `${deleted}, ${of}` : `${deleted} before it stopped, ${of}`;
}

/** What an audit found, in a line. */
export function auditSummary(a: ObjectAudit): string {
  if (a.error && a.missing === 0) return `stopped: ${a.error}`;
  const asked = `${fmtCount(a.referenced)} named`;
  const missing = a.missing === 0 ? "none missing" : `${fmtCount(a.missing)} missing`;
  // AN INCOMPLETE AUDIT'S COUNT IS A FLOOR, and says so: the estate it read
  // was missing a record that may name more.
  return a.completed ? `${asked}, ${missing}` : `${asked} so far, at least ${missing}`;
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
      at: r.audit?.at,
      result: r.audit ? auditSummary(r.audit) : "",
      cadence: "daily",
    },
  ];
}

/**
 * The object store: which store holds the company's files, and the
 * collector's last collection and audit.
 *
 * MISSING CHUNKS ARE WHAT AN OPERATOR OPENS THIS FOR. Each is part of a file
 * nobody can download, and the store lost it after acknowledging it, so the
 * card names the first of them — the ones to restore from a backup.
 *
 * Each state the engine names apart renders apart: a record the store would
 * not give up is not a fleet whose collector has not run.
 */
export function FileStorage({ objects, now }: { objects?: FleetObjects; now: number }) {
  if (!objects) return null;
  const reported = objects.state === "reported" ? objects : undefined;
  const header = (
    <Card.Header
      icon={<DatabaseGlyph size="sm" />}
      subtitle={reported ? backendSummary(reported.backend) : undefined}
    >
      File storage
    </Card.Header>
  );
  if (!reported) {
    return (
      <Card>
        {header}
        {objects.state === "unavailable" ? (
          <Callout variant="warning" role="alert">
            <span>
              The collector's record could not be read from the coordination store. Files are where
              they were; this screen cannot say what the collector last found.
            </span>
          </Callout>
        ) : (
          <EmptyState
            size="compact"
            icon={<DatabaseGlyph size="xl" />}
            title="No collection yet"
            description="One data node collects the store hourly and audits it daily. Its first pass runs within a minute of a data node starting."
          />
        )}
      </Card>
    );
  }

  const missing = reported.audit?.missing ?? 0;
  const shown = reported.audit?.missing_chunks ?? [];
  return (
    <Card padding="none">
      {header}
      {missing > 0 && (
        <Card.Body padding="md">
          <Callout variant="danger" role="alert">
            <div className="col gap-2">
              <span>
                {plural(missing, "chunk")} the company's files are made of{" "}
                {missing === 1 ? "is" : "are"} not in the store, so the files naming{" "}
                {missing === 1 ? "it" : "them"} cannot be downloaded. The store lost bytes it had
                acknowledged: check its health, and restore{" "}
                {shown.length < missing ? `these first ${shown.length}` : "these"} from a backup.
              </span>
              <span className="row wrap gap-1">
                {shown.map((h) => (
                  <InlineCode key={h}>{h}</InlineCode>
                ))}
              </span>
            </div>
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
            cell: (r) => <TextCell>{r.pass}</TextCell>,
          },
          {
            key: "at",
            header: "Last ran",
            shrink: true,
            cell: (r) => <DateCell at={r.at} now={now} />,
          },
          {
            key: "result",
            header: "Found",
            cell: (r) =>
              r.result ? <TextCell>{r.result}</TextCell> : <EmptyValue label="Not run yet" />,
          },
          {
            key: "cadence",
            header: "Runs",
            shrink: true,
            cell: (r) => <TextCell>{r.cadence}</TextCell>,
          },
        ]}
      />
      <Card.Body padding="md">
        <span className="secondary">
          Run by <KeyCell value={reported.node} path={["settings", "nodes", reported.node]} />
        </span>
      </Card.Body>
    </Card>
  );
}
