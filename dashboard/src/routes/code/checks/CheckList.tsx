/**
 * Attempts of checks, listed and opened — what Code › Checks, a change's
 * Checks tab and a repository's draw, each over its own question.
 *
 * # A listing is the engine's, a page at a time
 *
 * `code_checks` answers newest ended first (a repository's and the window's)
 * or by the checks of one commit (a change's), and a narrowing — a state, a
 * check's name — is tested on the rows a page READ, so a page may be short or
 * even empty with a `next`: Load more follows it. The pager is the one every
 * paged list shares (`lib/usePaged.ts`), and holds its first page still from
 * the press of Load more, because the cursor was minted from its last row.
 *
 * # One attempt opens in a pane
 *
 * A listing carries every column but the wide ones; the pane reads the one
 * attempt whole (`code_check`): its definition, the failures its ending named
 * — each path:line a link into the diff it was checked on, the comparison of
 * the attempt's base and its commit on that file — the tail of its output,
 * and the log a failed run kept, read a page at a time on demand
 * (`code_check_log`, by its object, which never changes under the reader).
 *
 * COLOUR IS STATE: passed is success, failed is danger, and an attempt that
 * measured nothing wears no verdict's tone (`lib/checks.ts`). What a check
 * measured is a duration and an exit code; it runs no model, so nothing here
 * is tokens.
 */

import { useState, type ReactNode } from "react";
import {
  Button,
  Callout,
  Card,
  CodeBlock,
  EmptyValue,
  Skeleton,
  TableDateCell,
  TableNumberCell,
  TableTextCell,
  Tag,
} from "@crewlethq/ui";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { PageFailed, QueryState } from "~/components/common.tsx";
import { useNow } from "~/lib/clock.ts";
import { refName, shortOID } from "~/lib/code.ts";
import { checkPurposeWord, checkStateTone, checkStateWord } from "~/lib/checks.ts";
import { fmtBytes, fmtDuration, plural } from "~/lib/format.ts";
import { codeRest, useCodeQuery, type CodeParams } from "~/lib/useCodeQuery.ts";
import { usePageHold, usePaged } from "~/lib/usePaged.ts";
import type {
  CodeCheckDetail,
  CodeCheckFailure,
  CodeCheckRow,
  CodeChecksAnswer,
} from "~/protocol/index.ts";
import { commitHref, compareHref } from "../paths.ts";

/**
 * How often a listing's newest page is read again while nothing older is
 * held: HALF A MINUTE. A check ends minutes after it starts, and the code log
 * pushes nothing to the dashboard, so this read is how a reader watching a
 * run sees it end; the pushes history's minute is for a log people read after
 * the fact.
 */
export const CHECKS_POLL_MS = 30_000;

/** An attempt's key, as one string: what a grid row and the pane are keyed by. */
export function attemptKey(r: CodeCheckRow): string {
  return `${r.repo_id}/${r.oid}/${r.target}/${r.name}/${r.attempt}`;
}

/** An attempt's state, in its word and its state's tone. */
export function CheckStateTag({ state }: { state: string }) {
  const tone = checkStateTone(state);
  const word = checkStateWord(state);
  return tone === "neutral" ? (
    <Tag appearance="outline">{word}</Tag>
  ) : (
    <Tag variant={tone}>{word}</Tag>
  );
}

/**
 * A page of attempts: what each check is, its state, the commit and the
 * branch it was checked for, what it was checking, how long it ran and when
 * it ended. Pressing one opens it.
 */
export function CheckGrid({
  rows,
  label,
  showRepo,
  open,
  onOpen,
}: {
  rows: readonly CodeCheckRow[];
  label: string;
  /** The repository column, for a listing that spans repositories. */
  showRepo: boolean;
  open: string;
  onOpen: (row: CodeCheckRow) => void;
}) {
  return (
    <DataGrid<CodeCheckRow>
      rows={[...rows]}
      rowKey={attemptKey}
      // THE ENGINE'S ORDER: newest ended first, or a commit's checks by
      // name. A head that sorted the pages loaded answers another question.
      serverSorted
      aria-label={label}
      isSelected={(r) => attemptKey(r) === open}
      onRowActivate={(r) => onOpen(r)}
      columns={[
        {
          key: "name",
          header: "Check",
          phoneLead: true,
          cell: (r) => (
            <span className="row gap-1 nowrap">
              <span className="mono">{r.name}</span>
              {r.attempt > 1 && <span className="t-caption">{`attempt ${r.attempt}`}</span>}
              {r.required && <span className="t-caption">required</span>}
            </span>
          ),
        },
        {
          key: "state",
          header: "State",
          shrink: true,
          cell: (r) => <CheckStateTag state={r.state} />,
        },
        ...(showRepo
          ? [
              {
                key: "repo",
                header: "Repository",
                shrink: true,
                cell: (r: CodeCheckRow) => <TableTextCell>{r.repo ?? r.repo_id}</TableTextCell>,
              },
            ]
          : []),
        {
          key: "commit",
          header: "Commit",
          shrink: true,
          cell: (r) => (
            <span className="row gap-1 nowrap" title={r.target}>
              {r.repo ? (
                <a className="t-link mono" href={commitHref(r.repo, r.oid)}>
                  {shortOID(r.oid)}
                </a>
              ) : (
                <span className="mono">{shortOID(r.oid)}</span>
              )}
              <span className="muted">{`for ${refName(r.target).name}`}</span>
            </span>
          ),
        },
        {
          key: "purpose",
          header: "Checking",
          shrink: true,
          drop: 2,
          cell: (r) => <TableTextCell>{checkPurposeWord(r.purpose)}</TableTextCell>,
        },
        {
          key: "duration",
          header: "Ran",
          shrink: true,
          align: "right",
          cell: (r) => (
            <TableNumberCell
              value={r.duration_ms ?? null}
              format={fmtDuration}
              emptyLabel="Not ended"
            />
          ),
        },
        {
          key: "ended",
          header: "Ended",
          shrink: true,
          drop: 1,
          cell: (r) => <EndedAt at={r.finished_at} />,
        },
      ]}
    />
  );
}

/** When an attempt ended, against the one clock; read HERE so a tick re-renders this cell alone. */
function EndedAt({ at }: { at: string | undefined }) {
  const now = useNow();
  if (!at) return <EmptyValue label="Not ended" />;
  return <TableDateCell value={at} now={now} />;
}

/**
 * One listing of attempts, paged, with the pane of the one opened beneath
 * it: the question `params` asks of `code_checks`, every row of which is
 * drawn — a narrowing, the Checks section's window's end among them, is the
 * engine's.
 */
export function CheckListing({
  params,
  label,
  showRepo,
  empty,
  header,
}: {
  params: CodeParams;
  label: string;
  showRepo: boolean;
  empty: { title: string; hint: string };
  /** Drawn above the grid with every row the pager holds: a section's chart and facets. */
  header?: (rows: readonly CodeCheckRow[], more: boolean) => ReactNode;
}) {
  const hold = usePageHold<CodeChecksAnswer>(params);
  const first = useCodeQuery("code_checks", params, { pollMs: CHECKS_POLL_MS, hold: hold.holding });
  const paged = usePaged(
    hold,
    first,
    (cursor) => codeRest.query("code_checks", { ...params, cursor }),
    (a: CodeChecksAnswer) => a.checks ?? [],
    attemptKey,
    (a: CodeChecksAnswer) => a.next ?? "",
  );
  const rows = paged.items;
  const more = paged.more;
  const [open, setOpen] = useState<CodeCheckRow | null>(null);
  const openKey = open ? attemptKey(open) : "";

  return (
    <>
      {header?.(rows, more)}
      {first.loading && rows.length === 0 && (
        <Skeleton variant="text" rows={5} label={`Loading ${label}`} />
      )}
      <QueryState
        error={first.error}
        refusal={first.refusal}
        detail={first.detail}
        loading={first.loading}
        empty={rows.length > 0 || more ? undefined : empty}
      >
        <CheckGrid
          rows={rows}
          label={label}
          showRepo={showRepo}
          open={openKey}
          onOpen={(r) => setOpen(attemptKey(r) === openKey ? null : r)}
        />
      </QueryState>
      {(rows.length > 0 || more || paged.pageError) && (
        <div className="row gap-2 wrap">
          {more ? (
            <Button
              size="small"
              variant="secondary"
              onClick={paged.older}
              disabled={paged.paging}
              loading={paged.paging}
            >
              Load more
            </Button>
          ) : (
            <span className="t-caption">That is every attempt this listing reaches.</span>
          )}
          {paged.pageError && <PageFailed error={paged.pageError} />}
          {rows.length > 0 && (
            <span className="t-caption">{plural(rows.length, "attempt")} listed</span>
          )}
          {paged.held && <span className="t-caption">Paused while you page on.</span>}
        </div>
      )}
      {open && open.repo && (
        <CheckPane key={openKey} row={open} repo={open.repo} onClose={() => setOpen(null)} />
      )}
    </>
  );
}

/**
 * One attempt read whole: what it ran, what it said, where it failed — each
 * failure a link into the diff it was checked on — the tail of its output,
 * and the log a failed run kept, a page at a time.
 */
export function CheckPane({
  row,
  repo,
  onClose,
}: {
  row: CodeCheckRow;
  repo: string;
  onClose: () => void;
}) {
  const read = useCodeQuery("code_check", {
    name: repo,
    oid: row.oid,
    check: row.name,
    attempt: row.attempt,
    target: row.target,
  });
  const d: CodeCheckDetail | undefined = read.data?.check;
  return (
    <section aria-label={`Check ${row.name}, attempt ${row.attempt}`}>
      <Card>
        <Card.Header>
          <Card.Title>
            <span className="row gap-2 wrap">
              <span className="mono">{row.name}</span>
              <span className="t-caption">{`attempt ${row.attempt}`}</span>
              <CheckStateTag state={d?.state ?? row.state} />
            </span>
          </Card.Title>
          <Button size="small" variant="ghost" onClick={onClose}>
            Close
          </Button>
        </Card.Header>
        <QueryState
          error={read.error}
          refusal={read.refusal}
          detail={read.detail}
          loading={read.loading}
        >
          {d && <CheckWhole repo={repo} d={d} />}
        </QueryState>
      </Card>
    </section>
  );
}

function CheckWhole({ repo, d }: { repo: string; d: CodeCheckDetail }) {
  const facts: { label: string; value: ReactNode }[] = [
    {
      label: "Checking",
      value: checkPurposeWord(d.purpose) || <EmptyValue label="Not recorded" />,
    },
    {
      label: "Commit",
      value: (
        <a className="t-link mono" href={commitHref(repo, d.oid)}>
          {shortOID(d.oid)}
        </a>
      ),
    },
    { label: "For", value: <span className="mono">{refName(d.target).name}</span> },
    { label: "Ran", value: fmtDuration(d.duration_ms ?? null) },
  ];
  if (d.exit_code !== undefined) facts.push({ label: "Exit code", value: String(d.exit_code) });
  if (d.reason) facts.push({ label: "Why it ended", value: d.reason.replaceAll("_", " ") });
  if (d.placement || d.runner) {
    facts.push({ label: "Ran on", value: [d.placement, d.runner].filter(Boolean).join(" · ") });
  }
  if (d.def?.run) facts.push({ label: "Runs", value: <span className="mono">{d.def.run}</span> });
  return (
    <div className="col gap-3">
      {d.summary && <p>{d.summary}</p>}
      <dl className="code-facts" aria-label="The attempt">
        {facts.map((f) => (
          <div key={f.label}>
            <dt>{f.label}</dt>
            <dd>{f.value}</dd>
          </div>
        ))}
      </dl>
      {d.url && (
        <a className="t-link" href={d.url} target="_blank" rel="noreferrer">
          Its run on the CI that reported it →
        </a>
      )}
      {(d.failures?.length ?? 0) > 0 && (
        <Failures repo={repo} d={d} failures={d.failures ?? []} omitted={d.failures_omitted ?? 0} />
      )}
      {d.tail && (
        <CodeBlock
          plain
          selectable
          focusWhenScrollable
          label="The end of its output"
          code={d.tail}
        />
      )}
      {d.log && <LogPages repo={repo} d={d} object={d.log.object} size={d.log.size} />}
    </div>
  );
}

/**
 * Where a failure is in the diff the attempt was checked on: the comparison
 * of its base and its commit, opened on the file — or the commit's own page,
 * where the attempt records no base.
 */
export function failureHref(repo: string, d: CodeCheckRow, f: CodeCheckFailure): string {
  if (!f.path) return commitHref(repo, d.oid);
  if (!d.base) return commitHref(repo, d.oid);
  return compareHref(repo, { base: d.base, head: d.oid, file: f.path });
}

function Failures({
  repo,
  d,
  failures,
  omitted,
}: {
  repo: string;
  d: CodeCheckDetail;
  failures: readonly CodeCheckFailure[];
  omitted: number;
}) {
  return (
    <div className="col gap-1">
      <strong>{plural(failures.length + omitted, "failure")}</strong>
      <ul className="col gap-1" aria-label="Failures">
        {failures.map((f, i) => (
          <li key={i} className="col">
            <span className="row gap-1 wrap">
              {f.path && (
                <a className="t-link mono" href={failureHref(repo, d, f)}>
                  {f.line ? `${f.path}:${f.line}` : f.path}
                </a>
              )}
              {f.test_id && <span className="mono">{f.test_id}</span>}
            </span>
            <span className="t-caption">{f.message}</span>
          </li>
        ))}
      </ul>
      {omitted > 0 && (
        <span className="t-caption">{`and ${plural(omitted, "more failure")} the run counted`}</span>
      )}
    </div>
  );
}

/** Where the log read so far stands. */
interface LogRead {
  text: string;
  next: number | null;
  reading: boolean;
  error: string;
}

/**
 * The log a failed run kept, a page at a time, on demand: never asked until
 * the reader asks, since a log is up to its whole size and the tail above
 * already holds where a failure is printed. Asked BY ITS OBJECT, which the
 * engine answers immutably: an object's key is minted once and never reused.
 */
function LogPages({
  repo,
  d,
  object,
  size,
}: {
  repo: string;
  d: CodeCheckDetail;
  object: string;
  size: number;
}) {
  const [log, setLog] = useState<LogRead>({ text: "", next: 0, reading: false, error: "" });
  const started = log.text !== "" || log.next !== 0;
  const read = async () => {
    if (log.next === null) return;
    setLog((l) => ({ ...l, reading: true, error: "" }));
    try {
      const page = await codeRest.query("code_check_log", {
        name: repo,
        oid: d.oid,
        check: d.name,
        attempt: d.attempt,
        target: d.target,
        object,
        offset: log.next,
      });
      setLog((l) => ({
        text: l.text + page.text,
        next: page.next ?? null,
        reading: false,
        error: "",
      }));
    } catch (err) {
      setLog((l) => ({
        ...l,
        reading: false,
        error: err instanceof Error ? err.message : "query_failed",
      }));
    }
  };
  return (
    <div className="col gap-1">
      {started && (
        <CodeBlock plain selectable focusWhenScrollable label="Its log" code={log.text} />
      )}
      <span className="row gap-2 wrap">
        {log.next !== null && (
          <Button
            size="small"
            variant="secondary"
            onClick={() => void read()}
            loading={log.reading}
          >
            {started ? "Read more of the log" : `Read its log (${fmtBytes(size)})`}
          </Button>
        )}
        {log.next === null && <span className="t-caption">That is the whole log.</span>}
        {log.error && (
          <Callout variant="danger">{`Could not read the log (${log.error}). Try again.`}</Callout>
        )}
      </span>
    </div>
  );
}
